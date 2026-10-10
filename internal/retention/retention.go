// Package retention implements a read-only lookup that explains why a pack
// file or blob is still present in the repository.
//
// It never modifies the repository: only listing, index lookups and blob
// loads are performed.
package retention

import (
	"context"
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/restic/restic/internal/data"
	"github.com/restic/restic/internal/debug"
	"github.com/restic/restic/internal/restic"
)

// Status describes why an object is still retained.
type Status string

const (
	// StatusReferenced means that the object (or at least one blob of a pack)
	// is reachable from one or more snapshots.
	StatusReferenced Status = "referenced"
	// StatusIndexOnly means that the object is listed in the index and present
	// in the repository, but no snapshot references it. Prune would repack or
	// remove it.
	StatusIndexOnly Status = "index-only"
	// StatusUnreferenced means that a pack file physically exists but is not
	// listed in the index. It is not retained by anything and prune would
	// delete it immediately.
	StatusUnreferenced Status = "unreferenced"
	// StatusUntraceable means that the index references the object, but the
	// corresponding pack file is missing from the repository, so the
	// reference chain cannot be verified.
	StatusUntraceable Status = "untraceable"
	// StatusNotFound means that the ID is neither present in the index nor in
	// the repository.
	StatusNotFound Status = "not-found"
)

// ObjectKind describes whether a query targets a pack or a blob.
type ObjectKind string

const (
	// KindPack selects pack files.
	KindPack ObjectKind = "pack"
	// KindBlob selects data or tree blobs.
	KindBlob ObjectKind = "blob"
)

// Query is one retention lookup request. An empty Kind selects the object
// automatically.
type Query struct {
	Kind ObjectKind
	ID   string
}

// Reference describes a single direct reference of a blob from within a
// snapshot tree.
type Reference struct {
	Snapshot      restic.ID       `json:"snapshot"`
	Time          time.Time       `json:"time"`
	Hostname      string          `json:"hostname,omitempty"`
	SnapshotPaths []string        `json:"snapshot_paths,omitempty"`
	BlobID        restic.ID       `json:"blob_id"`
	BlobType      restic.BlobType `json:"blob_type"`
	// Path of the node referencing the blob. It is "/" for a snapshot root
	// tree and otherwise the slash-separated path within the snapshot.
	Path string `json:"path"`
	// ParentTree is the tree blob that contains the referencing node.
	ParentTree restic.ID `json:"parent_tree"`
	// Chain lists the tree blobs from the snapshot root down to ParentTree,
	// both inclusive. It describes the necessary intermediate objects.
	Chain restic.IDs `json:"chain"`
}

// BlobLocation describes one index entry of a blob.
type BlobLocation struct {
	BlobID      restic.ID       `json:"blob_id"`
	BlobType    restic.BlobType `json:"blob_type"`
	PackID      restic.ID       `json:"pack_id"`
	PackPresent bool            `json:"pack_present"`
}

// Result is the retention analysis for one queried ID.
type Result struct {
	Kind ObjectKind `json:"object_type"`
	ID   restic.ID  `json:"id"`
	// Status is one of the Status* constants.
	Status Status `json:"status"`
	// Reason contains a short human-readable explanation.
	Reason string `json:"reason,omitempty"`

	// Pack results: whether the pack exists in the backend and the index.
	InBackend    bool  `json:"in_backend"`
	InIndex      bool  `json:"in_index"`
	PhysicalSize int64 `json:"physical_size,omitempty"`
	// PackBlobs are the blob handles listed for this pack in the index, or,
	// for unreferenced packs, the blobs found in the pack file header.
	PackBlobs []restic.BlobHandle `json:"pack_blobs,omitempty"`

	// Blob results: all index entries for the blob.
	Locations []BlobLocation `json:"locations,omitempty"`

	// References found while walking the snapshots.
	References []Reference `json:"references,omitempty"`

	// Warnings encountered while tracing reference chains.
	Warnings []string `json:"warnings,omitempty"`
}

// Repository is the read-only subset of the repository interface required by
// the retention analysis. restic.Repository satisfies it.
type Repository interface {
	Connections() uint
	List(ctx context.Context, t restic.FileType, fn func(restic.ID, int64) error) error
	ListBlobs(ctx context.Context, fn func(restic.PackBlob)) error
	LoadBlob(ctx context.Context, bh restic.BlobHandle, buf []byte) ([]byte, error)
	LoadUnpacked(ctx context.Context, t restic.FileType, id restic.ID) ([]byte, error)
	LookupBlob(bh restic.BlobHandle) []restic.PackBlob
	ListPackHandles(ctx context.Context, id restic.ID, packSize int64) ([]restic.BlobHandle, error)
}

// fullIDLength is the hexadecimal length of a full restic ID (sha256 hash).
const fullIDLength = 64

// ErrAmbiguousPrefix is returned if an ID prefix matches more than one object.
type ErrAmbiguousPrefix struct {
	Prefix  string
	Matches []string
}

func (e *ErrAmbiguousPrefix) Error() string {
	sort.Strings(e.Matches)
	return fmt.Sprintf("short ID %q is ambiguous, matches:\n  %s", e.Prefix, strings.Join(e.Matches, "\n  "))
}

type resolvedTarget struct {
	id    restic.ID
	kinds map[ObjectKind]struct{}
	blobs []restic.BlobHandle
}

type snapshotInfo struct {
	id restic.ID
	sn *data.Snapshot
}

// Analyze answers the retention queries. It never modifies the repository.
// snapshotLister is used to enumerate snapshots and must be created before the
// repository index is loaded (see restic.MemorizeList). The returned results
// are ordered like the queries.
func Analyze(ctx context.Context, repo Repository, snapshotLister restic.Lister, queries []Query) ([]*Result, error) {
	inv, err := collectInventory(ctx, repo)
	if err != nil {
		return nil, err
	}

	targets := make([]*resolvedTarget, 0, len(queries))
	want := make(map[restic.BlobHandle]struct{})
	for _, q := range queries {
		target, err := inv.resolve(q)
		if err != nil {
			return nil, err
		}
		targets = append(targets, target)
		for _, h := range target.blobs {
			want[h] = struct{}{}
		}
	}

	a := &analyzer{
		repo:           repo,
		snapshotLister: snapshotLister,
		want:           want,
		refs:           make(map[restic.BlobHandle][]Reference),
		reaches:        make(map[restic.ID]bool),
		trees:          make(map[restic.ID][]*data.Node),
	}
	if len(want) > 0 {
		if err := a.findReferences(ctx); err != nil {
			return nil, err
		}
	}

	results := make([]*Result, 0, len(targets))
	for _, target := range targets {
		results = append(results, a.classify(ctx, target, inv))
	}
	return results, nil
}

type inventory struct {
	backendPacks map[restic.ID]int64
	indexPacks   map[restic.ID][]restic.BlobHandle
	blobHandles  []restic.BlobHandle
}

func collectInventory(ctx context.Context, repo Repository) (*inventory, error) {
	inv := &inventory{
		backendPacks: make(map[restic.ID]int64),
		indexPacks:   make(map[restic.ID][]restic.BlobHandle),
	}

	if err := repo.List(ctx, restic.PackFile, func(id restic.ID, size int64) error {
		inv.backendPacks[id] = size
		return nil
	}); err != nil {
		return nil, err
	}

	seenBlobs := make(map[restic.BlobHandle]struct{})
	if err := repo.ListBlobs(ctx, func(pb restic.PackBlob) {
		h := pb.Handle()
		inv.indexPacks[pb.PackID()] = append(inv.indexPacks[pb.PackID()], h)
		if _, ok := seenBlobs[h]; !ok {
			seenBlobs[h] = struct{}{}
			inv.blobHandles = append(inv.blobHandles, h)
		}
	}); err != nil {
		return nil, err
	}
	sort.Sort(restic.BlobHandles(inv.blobHandles))
	return inv, nil
}

func isHexPrefix(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

func (inv *inventory) resolve(q Query) (*resolvedTarget, error) {
	idStr := strings.ToLower(q.ID)
	if !isHexPrefix(idStr) {
		return nil, fmt.Errorf("invalid ID %q: not a hexadecimal ID", q.ID)
	}

	exact := len(idStr) == fullIDLength
	var exactID restic.ID
	if exact {
		id, err := restic.ParseID(idStr)
		if err != nil {
			return nil, err
		}
		exactID = id
	}

	matchesPrefix := func(id restic.ID) bool {
		if exact {
			return id.Equal(exactID)
		}
		return strings.HasPrefix(id.String(), idStr)
	}

	t := &resolvedTarget{kinds: make(map[ObjectKind]struct{})}
	var packMatches []restic.ID
	var blobMatches []restic.BlobHandle

	if q.Kind != KindBlob {
		for id := range inv.indexPacks {
			if matchesPrefix(id) {
				packMatches = append(packMatches, id)
			}
		}
		for id := range inv.backendPacks {
			if matchesPrefix(id) && !slices.Contains(packMatches, id) {
				packMatches = append(packMatches, id)
			}
		}
		if len(packMatches) > 1 {
			matches := make([]string, 0, len(packMatches))
			for _, id := range packMatches {
				matches = append(matches, "pack "+id.String())
			}
			return nil, &ErrAmbiguousPrefix{Prefix: q.ID, Matches: matches}
		}
		if len(packMatches) == 1 {
			t.id = packMatches[0]
			t.kinds[KindPack] = struct{}{}
			t.blobs = inv.indexPacks[t.id]
		}
	}

	if q.Kind != KindPack {
		for _, h := range inv.blobHandles {
			if matchesPrefix(h.ID) {
				blobMatches = append(blobMatches, h)
			}
		}
		if len(blobMatches) > 1 {
			ids := make(map[restic.ID]struct{})
			for _, h := range blobMatches {
				ids[h.ID] = struct{}{}
			}
			if len(ids) > 1 {
				matches := make([]string, 0, len(blobMatches))
				for _, h := range blobMatches {
					matches = append(matches, fmt.Sprintf("%s blob %s", h.Type, h.ID))
				}
				return nil, &ErrAmbiguousPrefix{Prefix: q.ID, Matches: matches}
			}
		}
		if len(blobMatches) > 0 {
			// The same ID can theoretically exist as a data and a tree blob.
			t.id = blobMatches[0].ID
			t.kinds[KindBlob] = struct{}{}
			t.blobs = blobMatches
		}
	}

	_, isPackMatch := t.kinds[KindPack]
	_, isBlobMatch := t.kinds[KindBlob]
	if isPackMatch && isBlobMatch {
		// the same ID matched a pack and a blob; ask the user to disambiguate
		matches := []string{"pack " + packMatches[0].String()}
		for _, h := range blobMatches {
			matches = append(matches, fmt.Sprintf("%s blob %s", h.Type, h.ID))
		}
		return nil, &ErrAmbiguousPrefix{Prefix: q.ID, Matches: matches}
	}

	if len(t.kinds) == 0 {
		// keep the query as stated so that not-found results are reported
		if !exact {
			return nil, fmt.Errorf("no object found matching ID prefix %q", q.ID)
		}
		t.id = exactID
		if q.Kind == KindPack || q.Kind == KindBlob {
			t.kinds[ObjectKind(q.Kind)] = struct{}{}
		}
	}
	return t, nil
}

type analyzer struct {
	repo           Repository
	snapshotLister restic.Lister
	want           map[restic.BlobHandle]struct{}

	// refs collects all references per blob handle.
	refs map[restic.BlobHandle][]Reference
	// reaches caches whether a tree transitively references a wanted blob.
	reaches map[restic.ID]bool
	// trees caches the parsed nodes of trees that reach a wanted blob.
	trees map[restic.ID][]*data.Node

	warnings []string
}

func (a *analyzer) findReferences(ctx context.Context) error {
	var snapshots []snapshotInfo
	err := data.ForAllSnapshots(ctx, a.snapshotLister, a.repo, nil, func(id restic.ID, sn *data.Snapshot, err error) error {
		if err != nil {
			return err
		}
		if sn.Tree == nil {
			a.warnings = append(a.warnings, fmt.Sprintf("snapshot %s has no tree, skipped", id.Str()))
			return nil
		}
		snapshots = append(snapshots, snapshotInfo{id: id, sn: sn})
		return nil
	})
	if err != nil {
		return fmt.Errorf("loading snapshots failed: %w", err)
	}

	// youngest first, like the default order of `restic find`
	sort.Slice(snapshots, func(i, j int) bool {
		if snapshots[i].sn.Time.Equal(snapshots[j].sn.Time) {
			return snapshots[i].id.String() < snapshots[j].id.String()
		}
		return snapshots[i].sn.Time.After(snapshots[j].sn.Time)
	})

	for _, s := range snapshots {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		root := *s.sn.Tree
		rootHandle := restic.BlobHandle{ID: root, Type: restic.TreeBlob}
		if _, ok := a.want[rootHandle]; ok {
			a.record(s, rootHandle, "/", root, restic.IDs{root})
		}
		a.walk(ctx, s, root, "/", restic.IDs{root})
	}
	return nil
}

// walk traverses the tree treeID. chain contains the tree IDs from the
// snapshot root up to and including treeID. It returns true if the tree
// transitively references a wanted blob.
func (a *analyzer) walk(ctx context.Context, s snapshotInfo, treeID restic.ID, prefix string, chain restic.IDs) bool {
	if reaches, ok := a.reaches[treeID]; ok {
		if reaches {
			// the cached subtree reaches a wanted blob, re-traverse it to
			// record the references for this snapshot and path
			a.traverseCached(s, treeID, prefix, chain)
		}
		return reaches
	}

	for _, ancestor := range chain[:len(chain)-1] {
		if ancestor.Equal(treeID) {
			a.warnings = append(a.warnings, fmt.Sprintf("tree %s references itself, stopping traversal at %q", treeID.Str(), prefix))
			return false
		}
	}

	nodes, err := a.loadNodes(ctx, treeID)
	if err != nil {
		debug.Log("failed to load tree %v: %v", treeID, err)
		a.warnings = append(a.warnings, fmt.Sprintf("unable to load tree %s (%v); references below %q may be incomplete", treeID.Str(), err, prefix))
		return true
	}

	hit := false
	for _, node := range nodes {
		nodePath := path.Join(prefix, node.Name)
		switch node.Type {
		case data.NodeTypeFile:
			for _, contentID := range node.Content {
				h := restic.BlobHandle{ID: contentID, Type: restic.DataBlob}
				if _, ok := a.want[h]; ok {
					a.record(s, h, nodePath, treeID, chain)
					hit = true
				}
			}
		case data.NodeTypeDir:
			if node.Subtree == nil {
				continue
			}
			subHandle := restic.BlobHandle{ID: *node.Subtree, Type: restic.TreeBlob}
			if _, ok := a.want[subHandle]; ok {
				a.record(s, subHandle, nodePath, treeID, chain)
				hit = true
			}
			childChain := append(slices.Clone(chain), *node.Subtree)
			if a.walk(ctx, s, *node.Subtree, nodePath, childChain) {
				hit = true
			}
		}
	}

	a.reaches[treeID] = hit
	if hit {
		a.trees[treeID] = nodes
	}
	return hit
}

// traverseCached re-walks a positively cached subtree to record references
// under a different snapshot or path.
func (a *analyzer) traverseCached(s snapshotInfo, treeID restic.ID, prefix string, chain restic.IDs) {
	nodes := a.trees[treeID]
	for _, node := range nodes {
		nodePath := path.Join(prefix, node.Name)
		switch node.Type {
		case data.NodeTypeFile:
			for _, contentID := range node.Content {
				h := restic.BlobHandle{ID: contentID, Type: restic.DataBlob}
				if _, ok := a.want[h]; ok {
					a.record(s, h, nodePath, treeID, chain)
				}
			}
		case data.NodeTypeDir:
			if node.Subtree == nil {
				continue
			}
			subHandle := restic.BlobHandle{ID: *node.Subtree, Type: restic.TreeBlob}
			if _, ok := a.want[subHandle]; ok {
				a.record(s, subHandle, nodePath, treeID, chain)
			}
			if reaches, ok := a.reaches[*node.Subtree]; ok && reaches {
				childChain := append(slices.Clone(chain), *node.Subtree)
				a.traverseCached(s, *node.Subtree, nodePath, childChain)
			}
		}
	}
}

func (a *analyzer) record(s snapshotInfo, h restic.BlobHandle, nodePath string, parentTree restic.ID, chain restic.IDs) {
	a.refs[h] = append(a.refs[h], Reference{
		Snapshot:      s.id,
		Time:          s.sn.Time,
		Hostname:      s.sn.Hostname,
		SnapshotPaths: s.sn.Paths,
		BlobID:        h.ID,
		BlobType:      h.Type,
		Path:          nodePath,
		ParentTree:    parentTree,
		Chain:         slices.Clone(chain),
	})
}

func (a *analyzer) loadNodes(ctx context.Context, treeID restic.ID) ([]*data.Node, error) {
	if nodes, ok := a.trees[treeID]; ok {
		return nodes, nil
	}
	iter, err := data.LoadTree(ctx, a.repo, treeID)
	if err != nil {
		return nil, err
	}
	var nodes []*data.Node
	for item := range iter {
		if item.Error != nil {
			return nil, item.Error
		}
		nodes = append(nodes, item.Node)
	}
	return nodes, nil
}

func (a *analyzer) classify(ctx context.Context, t *resolvedTarget, inv *inventory) *Result {
	_, isPack := t.kinds[KindPack]
	_, isBlob := t.kinds[KindBlob]
	if isPack {
		return a.classifyPack(ctx, t.id, inv)
	}
	if isBlob {
		return a.classifyBlob(t.id, t.blobs, inv)
	}
	return &Result{Kind: "object", ID: t.id, Status: StatusNotFound, Reason: "no pack or blob with this ID exists in the repository"}
}

func (a *analyzer) classifyPack(ctx context.Context, id restic.ID, inv *inventory) *Result {
	res := &Result{Kind: KindPack, ID: id}
	res.InIndex = len(inv.indexPacks[id]) > 0
	res.InBackend = false
	if size, ok := inv.backendPacks[id]; ok {
		res.InBackend = true
		res.PhysicalSize = size
	}
	res.PackBlobs = slices.Clone(inv.indexPacks[id])

	switch {
	case !res.InBackend && !res.InIndex:
		res.Status = StatusNotFound
		res.Reason = "no pack file with this ID exists in the repository or the index"
		return res
	case res.InBackend && !res.InIndex:
		res.Status = StatusUnreferenced
		res.Reason = "pack file exists in the repository but is not listed in the index; prune would delete it"
		handles, err := a.repo.ListPackHandles(ctx, id, res.PhysicalSize)
		if err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("unable to read pack file header: %v", err))
		} else {
			res.PackBlobs = handles
		}
		return res
	case !res.InBackend && res.InIndex:
		res.Status = StatusUntraceable

		referenced := a.collectReferences(res, res.PackBlobs)
		res.Reason = "the index lists this pack, but the pack file is missing from the repository; run 'restic repair index'"
		if referenced > 0 {
			res.Reason = fmt.Sprintf("%s (%d indexed blobs are still referenced by snapshots)", res.Reason, referenced)
		}
		if len(a.warnings) > 0 {
			res.Warnings = append(res.Warnings, a.warnings...)
		}
		return res
	}

	referenced := a.collectReferences(res, res.PackBlobs)
	unreferenced := len(res.PackBlobs) - referenced
	if referenced > 0 {
		res.Status = StatusReferenced
		res.Reason = fmt.Sprintf("pack is kept: %d of its blobs are referenced by snapshots (%d unreferenced blobs)", referenced, unreferenced)
	} else {
		res.Status = StatusIndexOnly
		res.Reason = "pack is listed in the index but none of its blobs is referenced by a snapshot; prune would remove it"
	}
	if len(a.warnings) > 0 {
		res.Warnings = append(res.Warnings, a.warnings...)
	}
	return res
}

// collectReferences appends the snapshot references of the given blob handles
// to the result and returns the number of distinct blobs that are referenced.
func (a *analyzer) collectReferences(res *Result, handles []restic.BlobHandle) int {
	referenced := 0
	for _, h := range handles {
		if refs := a.refs[h]; len(refs) > 0 {
			referenced++
			res.References = append(res.References, refs...)
		}
	}
	sortReferences(res.References)
	return referenced
}

func sortReferences(refs []Reference) {
	sort.Slice(refs, func(i, j int) bool {
		if !refs[i].Snapshot.Equal(refs[j].Snapshot) {
			return refs[i].Snapshot.String() < refs[j].Snapshot.String()
		}
		if refs[i].BlobID != refs[j].BlobID {
			return refs[i].BlobID.String() < refs[j].BlobID.String()
		}
		return refs[i].Path < refs[j].Path
	})
}

func (a *analyzer) classifyBlob(id restic.ID, handles []restic.BlobHandle, inv *inventory) *Result {
	res := &Result{Kind: KindBlob, ID: id}

	if len(handles) == 0 {
		handles = []restic.BlobHandle{
			{ID: id, Type: restic.DataBlob},
			{ID: id, Type: restic.TreeBlob},
		}
	}

	allMissing := true
	for _, h := range handles {
		for _, pb := range a.repo.LookupBlob(h) {
			_, present := inv.backendPacks[pb.PackID()]
			if present {
				allMissing = false
			}
			res.Locations = append(res.Locations, BlobLocation{
				BlobID:      h.ID,
				BlobType:    h.Type,
				PackID:      pb.PackID(),
				PackPresent: present,
			})
			if refs := a.refs[h]; len(refs) > 0 {
				res.References = append(res.References, refs...)
			}
		}
	}
	sortReferences(res.References)
	res.InBackend = len(res.Locations) > 0 && !allMissing
	res.InIndex = len(res.Locations) > 0

	switch {
	case len(res.Locations) == 0:
		res.Status = StatusNotFound
		res.Reason = "blob is not listed in the index and not referenced by a snapshot; it occupies no retained space"
	case len(res.References) > 0:
		res.Status = StatusReferenced
		res.Reason = fmt.Sprintf("blob is referenced by %d snapshot location(s)", len(res.References))
		if allMissing {
			res.Reason += ", but all pack files containing it are missing; run 'restic repair index'"
		}
	case allMissing:
		res.Status = StatusUntraceable
		res.Reason = "the index lists this blob, but all pack files containing it are missing; run 'restic repair index'"
	default:
		res.Status = StatusIndexOnly
		res.Reason = "blob is listed in the index but no snapshot references it; prune would repack or remove its pack file"
	}
	if len(a.warnings) > 0 {
		res.Warnings = append(res.Warnings, a.warnings...)
	}
	return res
}
