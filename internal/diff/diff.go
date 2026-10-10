// Package diff implements the comparison of two snapshot trees.
//
// A comparison can either run in a single pass or be split into an arbitrary
// number of finite segments. Every segment consumes a [ResumeToken] that
// records the recursive walk position together with the accumulated blob
// de-duplication sets and counters. The token returned by a segment points
// exactly at the position from which another process can continue. Changes are
// reported exactly once, so concatenating the changes of all segments yields
// the same change list and the same final tree/data statistics as a single
// full comparison.
//
// The token is bound to the repository, the two fully resolved snapshots, the
// compared subdirectories and the comparison options. It must be validated
// against these values before a segment is continued.
package diff

import (
	"context"
	"encoding/json"
	"iter"
	"path"
	"reflect"
	"sort"

	"github.com/restic/restic/internal/data"
	"github.com/restic/restic/internal/errors"
	"github.com/restic/restic/internal/restic"
)

// tokenKind and tokenVersion identify the serialized token format.
const (
	tokenKind    = "restic-snapshot-diff"
	tokenVersion = 1
)

// Options controls how two snapshots are compared.
type Options struct {
	// ShowMetadata reports nodes whose metadata (access mode, timestamps,
	// ...) changed, even if the content is identical.
	ShowMetadata bool
}

// Change describes a single difference between the two compared snapshots.
type Change struct {
	Path     string
	Modifier string
}

// Stat collects statistics for one side (added or removed) of a comparison.
type Stat struct {
	Files     int    `json:"files"`
	Dirs      int    `json:"dirs"`
	Others    int    `json:"others"`
	DataBlobs int    `json:"data_blobs"`
	TreeBlobs int    `json:"tree_blobs"`
	Bytes     uint64 `json:"bytes"`
}

// Add counts node in the item statistics (blob statistics are added during
// finalization).
func (s *Stat) Add(node *data.Node) {
	if node == nil {
		return
	}

	switch node.Type {
	case data.NodeTypeFile:
		s.Files++
	case data.NodeTypeDir:
		s.Dirs++
	default:
		s.Others++
	}
}

// Stats is the result of a completed comparison.
type Stats struct {
	Added        Stat
	Removed      Stat
	ChangedFiles int
}

// Repository is the subset of the repository interface required to compare
// snapshots.
type Repository interface {
	restic.Loader
	NewAssociatedBlobSet() restic.AssociatedBlobSet
}

// frameKind describes how a frame on the walk stack is processed.
type frameKind string

const (
	// frameCompare diffs the two frame trees against each other.
	frameCompare frameKind = "compare"
	// frameCollect records all blobs of an identical shared subtree in the
	// common blob set.
	frameCollect frameKind = "collect-common"
	// frameAdded lists a subtree that only exists in the target snapshot.
	frameAdded frameKind = "added"
	// frameRemoved lists a subtree that only exists in the source snapshot.
	frameRemoved frameKind = "removed"
)

// frame is one level of the recursive walk. After is the name of the last
// child node at this level whose processing, including all of its
// descendants, is complete. Resuming reloads the frame trees and skips every
// node whose name is not greater than After.
type frame struct {
	Kind   frameKind `json:"kind"`
	Tree1  restic.ID `json:"tree1,omitempty"`
	Tree2  restic.ID `json:"tree2,omitempty"`
	Tree   restic.ID `json:"tree,omitempty"`
	Prefix string    `json:"prefix"`
	After  string    `json:"after,omitempty"`
}

// SkippedSubtree records a subtree that could not be processed because a tree
// could not be read. The walk continues at the next sibling, exactly like a
// full comparison. Recording the boundary in the token lets later processes
// judge which parts of the tree were not compared.
type SkippedSubtree struct {
	Kind   string    `json:"kind"`
	Prefix string    `json:"prefix"`
	Tree   restic.ID `json:"tree,omitempty"`
	Tree1  restic.ID `json:"tree1,omitempty"`
	Tree2  restic.ID `json:"tree2,omitempty"`
	Error  string    `json:"error"`
}

// ResumeToken is the credential of a resumable snapshot comparison. It is
// both the input and the output of [Comparer.RunSegment] and is safe to
// serialize between processes.
type ResumeToken struct {
	Kind    string `json:"kind"`
	Version int    `json:"version"`

	// Identity the comparison is bound to.
	RepositoryID    string           `json:"repository_id"`
	SourceSnapshot  restic.ID        `json:"source_snapshot"`
	TargetSnapshot  restic.ID        `json:"target_snapshot"`
	SourceSubfolder string           `json:"source_subfolder"`
	TargetSubfolder string           `json:"target_subfolder"`
	Options         Options          `json:"options"`
	Root1           restic.ID        `json:"root1"`
	Root2           restic.ID        `json:"root2"`
	Skipped         []SkippedSubtree `json:"skipped_subtrees,omitempty"`

	// Accumulated state.
	ChangedFiles int                 `json:"changed_files"`
	Added        Stat                `json:"added"`
	Removed      Stat                `json:"removed"`
	BlobsBefore  []restic.BlobHandle `json:"blobs_before,omitempty"`
	BlobsAfter   []restic.BlobHandle `json:"blobs_after,omitempty"`
	BlobsCommon  []restic.BlobHandle `json:"blobs_common,omitempty"`

	// Walk frontier. An empty stack means that the comparison is complete.
	Stack []frame `json:"stack,omitempty"`
	Done  bool    `json:"done"`

	// Live de-duplication sets backing the serialized handle lists. They are
	// materialized once per process and are not part of the serialized form.
	before restic.AssociatedBlobSet `json:"-"`
	after  restic.AssociatedBlobSet `json:"-"`
	common restic.AssociatedBlobSet `json:"-"`
}

// FramePosition describes one level of the continuation position.
type FramePosition struct {
	Kind   string    `json:"kind"`
	Prefix string    `json:"prefix"`
	After  string    `json:"after"`
	Tree   restic.ID `json:"tree,omitempty"`
	Tree1  restic.ID `json:"tree1,omitempty"`
	Tree2  restic.ID `json:"tree2,omitempty"`
}

// Position is the continuation position of a segment. Done is true once the
// whole comparison has finished.
type Position struct {
	Done  bool            `json:"done"`
	Stack []FramePosition `json:"stack"`
}

// Position returns the explicit continuation position of the token. The last
// entry of Stack is the level at which the next segment resumes.
func (t *ResumeToken) Position() Position {
	p := Position{Done: t.Done}
	for _, f := range t.Stack {
		p.Stack = append(p.Stack, FramePosition{
			Kind:   string(f.Kind),
			Prefix: f.Prefix,
			After:  f.After,
			Tree:   f.Tree,
			Tree1:  f.Tree1,
			Tree2:  f.Tree2,
		})
	}
	return p
}

// Status reports how a segment ended.
type Status int

const (
	// StatusComplete means that the whole comparison is finished and the
	// final statistics are available.
	StatusComplete Status = iota
	// StatusIncomplete means that the segment budget was exhausted and the
	// returned token must be used to continue.
	StatusIncomplete
	// StatusCanceled means that the context was canceled. The returned token
	// is resumable from a clean completion boundary.
	StatusCanceled
	// StatusFailed means that the root comparison level could not be read.
	// The token is left at the failing boundary and stays resumable; running
	// another segment retries the read. Errors in descendant subtrees do not
	// fail the segment, they are reported and skipped.
	StatusFailed
)

// Budget limits the amount of work done by one segment. The zero value runs
// the whole comparison in a single segment.
type Budget struct {
	// MaxNodes limits the number of tree nodes processed by the segment. A
	// value of zero means no limit.
	MaxNodes int
}

// SegmentResult is the result of [Comparer.RunSegment].
type SegmentResult struct {
	// Status describes how the segment ended.
	Status Status
	// Token is the continuation credential. It is always set and must be
	// persisted for incomplete and canceled segments.
	Token *ResumeToken
	// Position is the continuation position as reported by Token.
	Position Position
	// Stats contains the final statistics once the comparison is complete.
	Stats *Stats
	// Nodes is the number of nodes and unreadable subtree boundaries handled
	// by this segment.
	Nodes int
	// ReadErrors contains the tree read errors encountered by this segment.
	// The affected subtrees are skipped and also recorded in the token.
	ReadErrors []error
}

// NewResumeToken creates the initial token for a comparison of the two
// snapshot subtree roots.
//
// repoID identifies the repository (the repository configuration's ID),
// sourceID and targetID are the FULL snapshot IDs (prefix resolution must
// have happened before creating the token), sub1 and sub2 are the
// subdirectories that were resolved to root1 and root2, and opts are the
// comparison options. All of these values are part of the credential and are
// checked by [ResumeToken.Validate] before a token can be continued.
func NewResumeToken(repoID string, sourceID, targetID restic.ID, sub1, sub2 string, root1, root2 restic.ID, opts Options) *ResumeToken {
	t := &ResumeToken{
		Kind:            tokenKind,
		Version:         tokenVersion,
		RepositoryID:    repoID,
		SourceSnapshot:  sourceID,
		TargetSnapshot:  targetID,
		SourceSubfolder: sub1,
		TargetSubfolder: sub2,
		Options:         opts,
		Root1:           root1,
		Root2:           root2,
		BlobsBefore: []restic.BlobHandle{
			{Type: restic.TreeBlob, ID: root1},
		},
		BlobsAfter: []restic.BlobHandle{
			{Type: restic.TreeBlob, ID: root2},
		},
		Stack: []frame{{
			Kind:   frameCompare,
			Tree1:  root1,
			Tree2:  root2,
			Prefix: "/",
		}},
	}
	return t
}

// Validate checks that the token belongs to the given repository and that the
// two fully resolved snapshots, their subdirectories, the resolved subtree
// roots and the comparison options match the ones the token was created with.
// A token that fails validation must never be continued.
func (t *ResumeToken) Validate(repoID string, sourceID, targetID restic.ID, sub1, sub2 string, root1, root2 restic.ID, opts Options) error {
	if err := t.check(); err != nil {
		return err
	}

	switch {
	case t.RepositoryID != repoID:
		return errors.Errorf("resume token belongs to repository %q, got %q", t.RepositoryID, repoID)
	case !t.SourceSnapshot.Equal(sourceID):
		return errors.Errorf("resume token source snapshot is %v, got %v", t.SourceSnapshot, sourceID)
	case !t.TargetSnapshot.Equal(targetID):
		return errors.Errorf("resume token target snapshot is %v, got %v", t.TargetSnapshot, targetID)
	case t.SourceSubfolder != sub1:
		return errors.Errorf("resume token source subdirectory is %q, got %q", t.SourceSubfolder, sub1)
	case t.TargetSubfolder != sub2:
		return errors.Errorf("resume token target subdirectory is %q, got %q", t.TargetSubfolder, sub2)
	case !t.Root1.Equal(root1):
		return errors.Errorf("resume token source subtree root is %v, got %v", t.Root1, root1)
	case !t.Root2.Equal(root2):
		return errors.Errorf("resume token target subtree root is %v, got %v", t.Root2, root2)
	case t.Options != opts:
		return errors.Errorf("resume token was created with different comparison options: got %+v, want %+v", t.Options, opts)
	}
	return nil
}

func (t *ResumeToken) check() error {
	if t.Kind != tokenKind {
		return errors.Errorf("%w: unexpected token kind %q", ErrInvalidToken, t.Kind)
	}
	if t.Version != tokenVersion {
		return errors.Errorf("%w: unsupported token version %d", ErrInvalidToken, t.Version)
	}
	for i := range t.Stack {
		f := &t.Stack[i]
		switch f.Kind {
		case frameCompare:
			if f.Tree1.IsNull() || f.Tree2.IsNull() || f.Prefix == "" {
				return errors.Errorf("%w: malformed compare frame at stack position %d", ErrInvalidToken, i)
			}
		case frameCollect, frameAdded, frameRemoved:
			if f.Tree.IsNull() || f.Prefix == "" {
				return errors.Errorf("%w: malformed %q frame at stack position %d", ErrInvalidToken, f.Kind, i)
			}
		default:
			return errors.Errorf("%w: unknown frame kind %q at stack position %d", ErrInvalidToken, f.Kind, i)
		}
	}
	return nil
}

// ErrInvalidToken is returned when a resume token cannot be interpreted.
var ErrInvalidToken = errors.New("invalid diff resume token")

// BlobSizeNotFoundError reports that a blob's size is not known to the
// repository index during statistics finalization.
type BlobSizeNotFoundError struct {
	Handle restic.BlobHandle
}

func (e *BlobSizeNotFoundError) Error() string {
	return "unable to find blob size for " + e.Handle.String()
}

// Marshal serializes the token.
func (t *ResumeToken) Marshal() ([]byte, error) {
	if err := t.check(); err != nil {
		return nil, err
	}
	t.flushSets()
	return json.Marshal(t)
}

// ParseResumeToken parses a serialized token.
func ParseResumeToken(buf []byte) (*ResumeToken, error) {
	var t ResumeToken
	if err := json.Unmarshal(buf, &t); err != nil {
		return nil, errors.Errorf("%w: %v", ErrInvalidToken, err)
	}
	if err := t.check(); err != nil {
		return nil, err
	}
	return &t, nil
}

// materializeSets restores the live de-duplication sets from the serialized
// handle lists. Sets are reused if the token is continued within the same
// process.
func (t *ResumeToken) materializeSets(repo Repository) {
	if t.before != nil {
		return
	}
	t.before = repo.NewAssociatedBlobSet()
	for _, h := range t.BlobsBefore {
		t.before.Insert(h)
	}
	t.after = repo.NewAssociatedBlobSet()
	for _, h := range t.BlobsAfter {
		t.after.Insert(h)
	}
	t.common = repo.NewAssociatedBlobSet()
	for _, h := range t.BlobsCommon {
		t.common.Insert(h)
	}
}

func sortedHandles(s restic.AssociatedBlobSet) []restic.BlobHandle {
	handles := make([]restic.BlobHandle, 0, s.Len())
	for h := range s.Keys() {
		handles = append(handles, h)
	}
	sort.Sort(restic.BlobHandles(handles))
	return handles
}

// flushSets writes the live sets back into the serialized handle lists.
func (t *ResumeToken) flushSets() {
	if t.before == nil {
		return
	}
	t.BlobsBefore = sortedHandles(t.before)
	t.BlobsAfter = sortedHandles(t.after)
	t.BlobsCommon = sortedHandles(t.common)
}

// ResolveRoots resolves the compared subdirectories within the two snapshot
// tree roots. An empty subdirectory selects the snapshot root itself.
func ResolveRoots(ctx context.Context, repo restic.BlobLoader, root1, root2 *restic.ID, sub1, sub2 string) (restic.ID, restic.ID, error) {
	r1, err := data.FindTreeDirectory(ctx, repo, root1, sub1)
	if err != nil {
		return restic.ID{}, restic.ID{}, err
	}
	r2, err := data.FindTreeDirectory(ctx, repo, root2, sub2)
	if err != nil {
		return restic.ID{}, restic.ID{}, err
	}
	return *r1, *r2, nil
}

// Comparer compares two snapshot trees.
type Comparer struct {
	repo Repository
	opts Options
}

// NewComparer returns a Comparer using the given repository and options.
func NewComparer(repo Repository, opts Options) *Comparer {
	return &Comparer{repo: repo, opts: opts}
}

// RunSegment processes one segment of the comparison bound to token. Changes
// discovered in this segment are reported via onChange and unreadable
// subtrees via onError. Both callbacks may be nil.
//
// The passed token is updated in place and also returned in the result. On
// cancellation, both a resumable result and the context error are returned.
func (c *Comparer) RunSegment(ctx context.Context, token *ResumeToken, budget Budget, onChange func(*Change), onError func(error)) (*SegmentResult, error) {
	if onChange == nil {
		onChange = func(*Change) {}
	}
	if onError == nil {
		onError = func(error) {}
	}

	res := &SegmentResult{Token: token, Position: token.Position()}

	if err := token.check(); err != nil {
		return nil, err
	}
	if token.Options != c.opts {
		return nil, errors.Errorf("resume token was created with different comparison options: got %+v, want %+v", token.Options, c.opts)
	}

	// A completed token is terminal: re-running it returns the same result
	// without touching the repository.
	if token.Done {
		res.Status = StatusComplete
		res.Stats = token.stats()
		res.Position = token.Position()
		return res, nil
	}

	token.materializeSets(c.repo)

	r := &segment{
		c:         c,
		t:         token,
		ctx:       ctx,
		onChange:  onChange,
		onError:   onError,
		finite:    budget.MaxNodes > 0,
		remaining: budget.MaxNodes,
	}

	status := r.run()

	res.Status = status
	res.Nodes = r.processed
	res.ReadErrors = r.readErrors
	res.Position = token.Position()

	switch status {
	case StatusComplete:
		token.Done = true
		token.flushSets()
		res.Stats = token.stats()
		return res, nil
	case StatusCanceled:
		return res, ctx.Err()
	case StatusFailed:
		// the token remains at the failing boundary and can be retried
		return res, r.fatal
	default:
		return res, nil
	}
}

// segment holds the per-segment walk state.
type segment struct {
	c        *Comparer
	t        *ResumeToken
	ctx      context.Context
	onChange func(*Change)
	onError  func(error)

	finite    bool
	remaining int
	processed int

	readErrors []error
	fatal      error
	// active is aligned 1:1 with t.Stack. An entry is nil until that frame
	// is entered for the first time in this segment; frames pushed during
	// the segment start with a nil entry. This keeps a resumed parent frame
	// from being mistaken for an already entered child frame.
	active []*activeFrame
}

func (r *segment) exhausted() bool {
	return r.finite && r.remaining <= 0
}

func (r *segment) tick() {
	r.processed++
	if r.finite {
		r.remaining--
	}
}

// activeFrame is the live iterator state of one frame for the duration of a
// segment.
type activeFrame struct {
	stop func()
	next func() (walkStep, bool)
}

type walkStep struct {
	dual   bool
	n1, n2 *data.Node
	node   *data.Node
	err    error
}

func (w walkStep) name() string {
	if w.dual {
		if w.n1 != nil {
			return w.n1.Name
		}
		return w.n2.Name
	}
	return w.node.Name
}

// run walks the frame stack until the budget is exhausted, the context is
// canceled or the whole tree has been processed.
func (r *segment) run() Status {
	// active mirrors t.Stack; pushed frames get a nil active entry until they
	// are entered.
	r.active = make([]*activeFrame, len(r.t.Stack))
	defer func() {
		// stop is idempotent, so it is safe to also stop entries already
		// stopped by popFrame.
		for _, af := range r.active {
			if af != nil {
				af.stop()
			}
		}
	}()

	for len(r.t.Stack) > 0 {
		if r.ctx.Err() != nil {
			return StatusCanceled
		}
		// The boundary is only considered reached once the previous node has
		// been committed completely.
		if r.exhausted() {
			return StatusIncomplete
		}

		top := len(r.t.Stack) - 1
		f := &r.t.Stack[top]

		if r.active[top] == nil {
			af, err := r.enter(f)
			if err != nil {
				if r.ctx.Err() != nil {
					return StatusCanceled
				}
				if top == 0 {
					// a root-level read failure is fatal; keep the boundary
					// so that another segment can retry the read
					r.fatal = err
					return StatusFailed
				}
				// An unreadable subtree is a completed skip boundary: the
				// parent node is already committed, so drop the frame and
				// continue with the siblings.
				r.popFrame(top)
				r.skipFrame(f, err)
				continue
			}
			r.active[top] = af
		}

		af := r.active[top]
		step, ok := af.next()
		if !ok {
			r.popFrame(top)
			r.t.Stack = r.t.Stack[:top]
			continue
		}
		if step.err != nil {
			if r.ctx.Err() != nil {
				return StatusCanceled
			}
			if top == 0 {
				r.fatal = step.err
				return StatusFailed
			}
			r.popFrame(top)
			r.skipFrame(&r.t.Stack[top], step.err)
			continue
		}

		// The own-node effects and the frame position are committed
		// together. A descendant frame is pushed before the position is
		// advanced, so an interrupted subtree walk never emits its parent's
		// change twice.
		var child *frame
		if step.dual {
			child = r.processCompare(f, step.n1, step.n2)
		} else {
			child = r.processSingle(f, step.node)
		}
		f.After = step.name()
		r.tick()

		if child != nil {
			r.t.Stack = append(r.t.Stack, *child)
			r.active = append(r.active, nil)
		}
	}

	r.finalize()
	return StatusComplete
}

// popFrame stops and drops the active entry of the frame at top together with
// any descendant active entries.
func (r *segment) popFrame(top int) {
	for i := top; i < len(r.active); i++ {
		if r.active[i] != nil {
			r.active[i].stop()
		}
	}
	r.active = r.active[:top]
}

// skipFrame records an unreadable subtree, reports it and removes the frame
// so that processing continues with its siblings.
func (r *segment) skipFrame(f *frame, err error) {
	r.onError(err)
	r.readErrors = append(r.readErrors, err)
	r.t.Skipped = append(r.t.Skipped, SkippedSubtree{
		Kind:   string(f.Kind),
		Prefix: f.Prefix,
		Tree:   f.Tree,
		Tree1:  f.Tree1,
		Tree2:  f.Tree2,
		Error:  err.Error(),
	})
	r.t.Stack = r.t.Stack[:len(r.t.Stack)-1]
	r.tick()
}

// skipNodes drops all nodes of an iterator whose name is not greater than
// after. Iterator errors are always passed through.
func skipNodes(it data.TreeNodeIterator, after string) data.TreeNodeIterator {
	if after == "" {
		return it
	}
	return func(yield func(data.NodeOrError) bool) {
		for item := range it {
			if item.Node != nil && item.Node.Name <= after {
				continue
			}
			if !yield(item) {
				return
			}
		}
	}
}

// enter builds the live iterator state for a frame, loading the required
// trees. A load failure leaves the token unchanged.
func (r *segment) enter(f *frame) (*activeFrame, error) {
	switch f.Kind {
	case frameCompare:
		t1, err := data.LoadTree(r.ctx, r.c.repo, f.Tree1)
		if err != nil {
			return nil, err
		}
		t2, err := data.LoadTree(r.ctx, r.c.repo, f.Tree2)
		if err != nil {
			return nil, err
		}
		next, stop := iter.Pull(data.DualTreeIterator(skipNodes(t1, f.After), skipNodes(t2, f.After)))
		return &activeFrame{
			stop: stop,
			next: func() (walkStep, bool) {
				dt, ok := next()
				if !ok {
					return walkStep{}, false
				}
				return walkStep{dual: true, n1: dt.Tree1, n2: dt.Tree2, err: dt.Error}, true
			},
		}, nil
	case frameCollect, frameAdded, frameRemoved:
		tree, err := data.LoadTree(r.ctx, r.c.repo, f.Tree)
		if err != nil {
			return nil, err
		}
		next, stop := iter.Pull(skipNodes(tree, f.After))
		return &activeFrame{
			stop: stop,
			next: func() (walkStep, bool) {
				item, ok := next()
				if !ok {
					return walkStep{}, false
				}
				return walkStep{node: item.Node, err: item.Error}, true
			},
		}, nil
	default:
		return nil, errors.Errorf("%w: unknown frame kind %q", ErrInvalidToken, f.Kind)
	}
}

// addBlobs records the blobs owned by node in bs: the content blobs of files
// and the subtree tree blob of directories.
func addBlobs(bs restic.AssociatedBlobSet, node *data.Node) {
	if node == nil {
		return
	}

	switch node.Type {
	case data.NodeTypeFile:
		for _, blob := range node.Content {
			bs.Insert(restic.BlobHandle{ID: blob, Type: restic.DataBlob})
		}
	case data.NodeTypeDir:
		if node.Subtree != nil {
			bs.Insert(restic.BlobHandle{ID: *node.Subtree, Type: restic.TreeBlob})
		}
	}
}

func joinName(prefix, name string, isDir bool) string {
	full := path.Join(prefix, name)
	if isDir {
		full += "/"
	}
	return full
}

// processCompare handles one node pair of a compare frame. It returns the
// frame that must be descended into, or nil.
func (r *segment) processCompare(f *frame, node1, node2 *data.Node) *frame {
	addBlobs(r.t.before, node1)
	addBlobs(r.t.after, node2)

	switch {
	case node1 != nil && node2 != nil:
		name := joinName(f.Prefix, node1.Name, node2.Type == data.NodeTypeDir)
		mod := ""

		if node1.Type != node2.Type {
			mod += "T"
		}

		if node1.Type == data.NodeTypeFile &&
			node2.Type == data.NodeTypeFile &&
			!reflect.DeepEqual(node1.Content, node2.Content) {
			mod += "M"
			r.t.ChangedFiles++

			node1NilContent := *node1
			node2NilContent := *node2
			node1NilContent.Content = nil
			node2NilContent.Content = nil
			// the bitrot detection may not work if `backup --ignore-inode` or `--ignore-ctime` were used
			if node1NilContent.Equals(node2NilContent) {
				// probable bitrot detected
				mod += "?"
			}
		} else if r.c.opts.ShowMetadata && !node1.Equals(*node2) {
			mod += "U"
		}

		if mod != "" {
			r.onChange(&Change{Path: name, Modifier: mod})
		}

		if node1.Type == data.NodeTypeDir && node2.Type == data.NodeTypeDir {
			if (*node1.Subtree).Equal(*node2.Subtree) {
				return &frame{Kind: frameCollect, Tree: *node1.Subtree, Prefix: name}
			}
			return &frame{Kind: frameCompare, Tree1: *node1.Subtree, Tree2: *node2.Subtree, Prefix: name}
		}
	case node1 != nil && node2 == nil:
		name := joinName(f.Prefix, node1.Name, node1.Type == data.NodeTypeDir)
		r.onChange(&Change{Path: name, Modifier: "-"})
		r.t.Removed.Add(node1)
		if node1.Type == data.NodeTypeDir {
			return &frame{Kind: frameRemoved, Tree: *node1.Subtree, Prefix: name}
		}
	case node1 == nil && node2 != nil:
		name := joinName(f.Prefix, node2.Name, node2.Type == data.NodeTypeDir)
		r.onChange(&Change{Path: name, Modifier: "+"})
		r.t.Added.Add(node2)
		if node2.Type == data.NodeTypeDir {
			return &frame{Kind: frameAdded, Tree: *node2.Subtree, Prefix: name}
		}
	}
	return nil
}

// processSingle handles one node of a collect/add/remove frame.
func (r *segment) processSingle(f *frame, node *data.Node) *frame {
	switch f.Kind {
	case frameCollect:
		addBlobs(r.t.common, node)
		if node.Type == data.NodeTypeDir {
			return &frame{Kind: frameCollect, Tree: *node.Subtree, Prefix: joinName(f.Prefix, node.Name, true)}
		}
	case frameAdded:
		name := joinName(f.Prefix, node.Name, node.Type == data.NodeTypeDir)
		r.onChange(&Change{Path: name, Modifier: "+"})
		r.t.Added.Add(node)
		addBlobs(r.t.after, node)
		if node.Type == data.NodeTypeDir {
			return &frame{Kind: frameAdded, Tree: *node.Subtree, Prefix: name}
		}
	case frameRemoved:
		name := joinName(f.Prefix, node.Name, node.Type == data.NodeTypeDir)
		r.onChange(&Change{Path: name, Modifier: "-"})
		r.t.Removed.Add(node)
		addBlobs(r.t.before, node)
		if node.Type == data.NodeTypeDir {
			return &frame{Kind: frameRemoved, Tree: *node.Subtree, Prefix: name}
		}
	}
	return nil
}

// finalize computes the added/removed blob statistics from the accumulated
// de-duplication sets.
func (r *segment) finalize() {
	both := r.t.before.Intersect(r.t.after)
	r.updateBlobs(r.t.before.Sub(both).Sub(r.t.common), &r.t.Removed)
	r.updateBlobs(r.t.after.Sub(both).Sub(r.t.common), &r.t.Added)
}

func (r *segment) updateBlobs(blobs restic.AssociatedBlobSet, stats *Stat) {
	for h := range blobs.Keys() {
		switch h.Type {
		case restic.DataBlob:
			stats.DataBlobs++
		case restic.TreeBlob:
			stats.TreeBlobs++
		}

		size, found := r.c.repo.LookupBlobSize(h)
		if !found {
			err := &BlobSizeNotFoundError{Handle: h}
			r.onError(err)
			r.readErrors = append(r.readErrors, err)
			continue
		}

		stats.Bytes += uint64(size)
	}
}

func (t *ResumeToken) stats() *Stats {
	return &Stats{
		Added:        t.Added,
		Removed:      t.Removed,
		ChangedFiles: t.ChangedFiles,
	}
}
