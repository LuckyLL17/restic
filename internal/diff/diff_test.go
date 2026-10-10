package diff

import (
	"context"
	"encoding/json"
	"iter"
	"os"
	"testing"
	"time"

	"github.com/restic/restic/internal/data"
	"github.com/restic/restic/internal/restic"
	rtest "github.com/restic/restic/internal/test"
)

// mockBlobSet is a simple AssociatedBlobSet implementation used by testRepo.
type mockBlobSet map[restic.BlobHandle]struct{}

func newMockBlobSet() restic.AssociatedBlobSet {
	return mockBlobSet{}
}

func (s mockBlobSet) Has(h restic.BlobHandle) bool {
	_, ok := s[h]
	return ok
}

func (s mockBlobSet) Insert(h restic.BlobHandle) {
	s[h] = struct{}{}
}

func (s mockBlobSet) Delete(h restic.BlobHandle) {
	delete(s, h)
}

func (s mockBlobSet) Len() int {
	return len(s)
}

func (s mockBlobSet) Keys() iter.Seq[restic.BlobHandle] {
	return func(yield func(restic.BlobHandle) bool) {
		for h := range s {
			if !yield(h) {
				return
			}
		}
	}
}

func (s mockBlobSet) Intersect(other restic.AssociatedBlobSet) restic.AssociatedBlobSet {
	o := other.(mockBlobSet)
	res := mockBlobSet{}
	for h := range s {
		if _, ok := o[h]; ok {
			res[h] = struct{}{}
		}
	}
	return res
}

func (s mockBlobSet) Sub(other restic.AssociatedBlobSet) restic.AssociatedBlobSet {
	o := other.(mockBlobSet)
	res := mockBlobSet{}
	for h := range s {
		if _, ok := o[h]; !ok {
			res[h] = struct{}{}
		}
	}
	return res
}

// testRepo stores tree blobs in memory and answers blob size lookups.
type testRepo struct {
	data.TestWritableTreeMap
	sizes      map[restic.BlobHandle]uint
	failLoad   map[restic.ID]bool
	blocked    map[restic.ID]bool
	blockCalls int
}

func newTestRepo() *testRepo {
	return &testRepo{
		TestWritableTreeMap: data.TestWritableTreeMap{
			TestTreeMap: data.TestTreeMap{},
		},
		sizes:    map[restic.BlobHandle]uint{},
		failLoad: map[restic.ID]bool{},
		blocked:  map[restic.ID]bool{},
	}
}

func (r *testRepo) LookupBlobSize(h restic.BlobHandle) (uint, bool) {
	size, ok := r.sizes[h]
	return size, ok
}

func (r *testRepo) NewAssociatedBlobSet() restic.AssociatedBlobSet {
	return newMockBlobSet()
}

func (r *testRepo) LoadBlob(ctx context.Context, bh restic.BlobHandle, buf []byte) ([]byte, error) {
	if bh.Type == restic.TreeBlob {
		if r.blocked[bh.ID] {
			r.blockCalls++
			return nil, &blockedTreeError{id: bh.ID}
		}
		if r.failLoad[bh.ID] {
			return nil, &missingTreeError{id: bh.ID}
		}
	}
	return r.TestTreeMap.LoadBlob(ctx, bh, buf)
}

func (r *testRepo) SaveBlob(ctx context.Context, tpe restic.BlobType, buf []byte, id restic.ID, storeDuplicate bool) (restic.ID, bool, int, error) {
	newID, known, size, err := r.TestWritableTreeMap.SaveBlob(ctx, tpe, buf, id, storeDuplicate)
	if err == nil && size > 0 {
		h := restic.BlobHandle{Type: tpe, ID: newID}
		if _, ok := r.sizes[h]; !ok {
			r.sizes[h] = uint(size)
		}
	}
	return newID, known, size, err
}

type missingTreeError struct{ id restic.ID }

func (e *missingTreeError) Error() string { return "tree not found: " + e.id.String() }

type blockedTreeError struct{ id restic.ID }

func (e *blockedTreeError) Error() string {
	return "completed subtree must not be reloaded: " + e.id.String()
}

var testTime = time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)

type fixture struct {
	repo                       *testRepo
	root1, root2               restic.ID
	commonTree, commonDeepTree restic.ID
	goneTree                   restic.ID
}

// saveTree stores nodes as a tree blob and returns its ID.
func saveTree(t testing.TB, repo *testRepo, nodes ...*data.Node) restic.ID {
	return data.TestSaveNodes(t, context.Background(), repo, nodes)
}

func fileNode(t testing.TB, repo *testRepo, name string, mode os.FileMode, parts ...string) *data.Node {
	node := &data.Node{
		Name:    name,
		Type:    data.NodeTypeFile,
		Mode:    mode,
		ModTime: testTime,
	}
	var size uint64
	for _, part := range parts {
		id := restic.Hash([]byte(part))
		node.Content = append(node.Content, id)
		size += uint64(len(part))
		repo.sizes[restic.BlobHandle{Type: restic.DataBlob, ID: id}] = uint(len(part))
	}
	node.Size = size
	return node
}

func dirNode(name string, subtree restic.ID) *data.Node {
	return &data.Node{Name: name, Type: data.NodeTypeDir, Mode: 0755, ModTime: testTime, Subtree: &subtree}
}

func symlinkNode(name, target string) *data.Node {
	return &data.Node{Name: name, Type: data.NodeTypeSymlink, Mode: 0777 | os.ModeSymlink, ModTime: testTime, LinkTarget: target}
}

// buildFixture constructs two snapshot roots covering all comparison cases.
func buildFixture(t testing.TB) *fixture {
	t.Helper()
	repo := newTestRepo()
	ctx := context.Background()
	_ = ctx

	// shared subtree (identical tree ID in both snapshots)
	deep := saveTree(t, repo, fileNode(t, repo, "x", 0644, "shared-deep-content"))
	common := saveTree(t, repo,
		dirNode("deep", deep),
		fileNode(t, repo, "unchanged.txt", 0644, "shared-content"),
	)

	// changed directory
	changed1 := saveTree(t, repo,
		fileNode(t, repo, "f", 0644, "old-content--long"),
		fileNode(t, repo, "same", 0644, "identical-content"),
	)
	changed2 := saveTree(t, repo,
		fileNode(t, repo, "f", 0644, "new"),
		fileNode(t, repo, "same", 0644, "identical-content"),
	)

	// removed directory
	goneSub := saveTree(t, repo, fileNode(t, repo, "nested", 0644, "nested-gone"))
	gone := saveTree(t, repo,
		dirNode("olddir", goneSub),
		fileNode(t, repo, "oldfile", 0644, "gone-content"),
	)

	// added directory
	addedSub := saveTree(t, repo, fileNode(t, repo, "nested2", 0644, "nested-new"))
	added := saveTree(t, repo,
		dirNode("newdir", addedSub),
		fileNode(t, repo, "newfile", 0644, "new-content"),
	)

	// bitrot: identical metadata (including size), different content blobs
	bitrot1 := fileNode(t, repo, "bitrot", 0644, "same-length-A")
	bitrot2 := fileNode(t, repo, "bitrot", 0644, "same-length-B")
	// ensure both contents have the same length so that node metadata matches
	rtest.Equals(t, bitrot1.Size, bitrot2.Size)
	rtest.Assert(t, !bitrot1.Content[0].Equal(bitrot2.Content[0]), "expected different content ids")

	// metadata-only change
	modmeta1 := fileNode(t, repo, "modmeta", 0644, "metadata-content")
	modmeta2 := fileNode(t, repo, "modmeta", 0600, "metadata-content")

	root1 := saveTree(t, repo,
		bitrot1,
		dirNode("changed", changed1),
		dirNode("common", common),
		dirNode("gone", gone),
		modmeta1,
		fileNode(t, repo, "only1", 0644, "only-one"),
		fileNode(t, repo, "typed", 0644, "typed-as-file"),
	)
	root2 := saveTree(t, repo,
		dirNode("added", added),
		bitrot2,
		dirNode("changed", changed2),
		dirNode("common", common),
		modmeta2,
		fileNode(t, repo, "only2", 0644, "only-two"),
		symlinkNode("typed", "somewhere"),
	)

	return &fixture{
		repo:           repo,
		root1:          root1,
		root2:          root2,
		commonTree:     common,
		commonDeepTree: deep,
		goneTree:       gone,
	}
}

func expectedChanges(withMetadata bool) []Change {
	changes := []Change{
		{Path: "/added/", Modifier: "+"},
		{Path: "/added/newdir/", Modifier: "+"},
		{Path: "/added/newdir/nested2", Modifier: "+"},
		{Path: "/added/newfile", Modifier: "+"},
		{Path: "/bitrot", Modifier: "M?"},
	}
	if withMetadata {
		// the directory entry itself changes because the subtree ID is part
		// of the compared node metadata
		changes = append(changes, Change{Path: "/changed/", Modifier: "U"})
	}
	changes = append(changes,
		Change{Path: "/changed/f", Modifier: "M"},
		Change{Path: "/gone/", Modifier: "-"},
		Change{Path: "/gone/olddir/", Modifier: "-"},
		Change{Path: "/gone/olddir/nested", Modifier: "-"},
		Change{Path: "/gone/oldfile", Modifier: "-"},
	)
	if withMetadata {
		changes = append(changes, Change{Path: "/modmeta", Modifier: "U"})
	}
	changes = append(changes,
		Change{Path: "/only1", Modifier: "-"},
		Change{Path: "/only2", Modifier: "+"},
	)
	if withMetadata {
		// type and metadata (mode, link target, size) both differ
		changes = append(changes, Change{Path: "/typed", Modifier: "TU"})
	} else {
		changes = append(changes, Change{Path: "/typed", Modifier: "T"})
	}
	return changes
}

type runIdentity struct {
	repoID string
	sn1    restic.ID
	sn2    restic.ID
	sub1   string
	sub2   string
	opts   Options
}

func idFor(f *fixture, opts Options) runIdentity {
	return runIdentity{
		repoID: "repo-id",
		sn1:    restic.Hash([]byte("snapshot-one")),
		sn2:    restic.Hash([]byte("snapshot-two")),
		opts:   opts,
	}
}

func tokenFor(f *fixture, opts Options) *ResumeToken {
	id := idFor(f, opts)
	return NewResumeToken(id.repoID, id.sn1, id.sn2, id.sub1, id.sub2, f.root1, f.root2, opts)
}

func (id runIdentity) validate(t testing.TB, token *ResumeToken, f *fixture) {
	t.Helper()
	roots := func() (restic.ID, restic.ID) {
		r1, r2, err := ResolveRoots(context.Background(), f.repo, &f.root1, &f.root2, id.sub1, id.sub2)
		rtest.OK(t, err)
		return r1, r2
	}
	r1, r2 := roots()
	rtest.OK(t, token.Validate(id.repoID, id.sn1, id.sn2, id.sub1, id.sub2, r1, r2, id.opts))
}

// runReference performs the whole comparison in a single segment.
func runReference(t testing.TB, f *fixture, opts Options) ([]Change, *Stats, *ResumeToken) {
	t.Helper()
	token := tokenFor(f, opts)
	c := NewComparer(f.repo, opts)
	var changes []Change
	res, err := c.RunSegment(context.Background(), token, Budget{}, func(ch *Change) {
		changes = append(changes, *ch)
	}, nil)
	rtest.OK(t, err)
	rtest.Equals(t, StatusComplete, res.Status)
	rtest.Assert(t, res.Token.Done, "comparison must be marked done")
	return changes, res.Stats, token
}

func TestCompareChanges(t *testing.T) {
	f := buildFixture(t)
	opts := Options{ShowMetadata: true}
	changes, stats, _ := runReference(t, f, opts)

	rtest.Equals(t, expectedChanges(true), changes)

	rtest.Equals(t, 3, stats.Added.Files)
	rtest.Equals(t, 2, stats.Added.Dirs)
	rtest.Equals(t, 3, stats.Removed.Files)
	rtest.Equals(t, 2, stats.Removed.Dirs)
	rtest.Equals(t, 2, stats.ChangedFiles)
	rtest.Assert(t, stats.Added.DataBlobs > 0, "expected added data blobs")
	rtest.Assert(t, stats.Removed.DataBlobs > 0, "expected removed data blobs")
	rtest.Assert(t, stats.Added.TreeBlobs > 0, "expected added tree blobs")
	rtest.Assert(t, stats.Removed.TreeBlobs > 0, "expected removed tree blobs")
	rtest.Assert(t, stats.Added.Bytes > 0 && stats.Removed.Bytes > 0, "expected non-zero byte stats")

	// without --metadata the metadata-only change is not reported
	changesNoMeta, statsNoMeta, _ := runReference(t, f, Options{})
	rtest.Equals(t, expectedChanges(false), changesNoMeta)
	rtest.Equals(t, stats.ChangedFiles, statsNoMeta.ChangedFiles)
}

// TestSegmentedEquivalence checks that arbitrary segmentations produce the
// same change list and final statistics as a full comparison. Tokens are
// serialized and parsed between segments to simulate another process.
func TestSegmentedEquivalence(t *testing.T) {
	for _, showMetadata := range []bool{false, true} {
		opts := Options{ShowMetadata: showMetadata}
		refChanges, refStats, _ := runReference(t, buildFixture(t), opts)

		for _, maxNodes := range []int{1, 2, 3, 5, 7, 1000} {
			f := buildFixture(t)
			id := idFor(f, opts)

			token := tokenFor(f, opts)
			var changes []Change
			segments := 0
			for !token.Done {
				buf, err := token.Marshal()
				rtest.OK(t, err)
				token, err = ParseResumeToken(buf)
				rtest.OK(t, err)

				c := NewComparer(f.repo, opts)
				res, err := c.RunSegment(context.Background(), token, Budget{MaxNodes: maxNodes}, func(ch *Change) {
					changes = append(changes, *ch)
				}, nil)
				rtest.Assert(t, err == nil || res.Status == StatusCanceled, "unexpected error %v", err)
				rtest.Assert(t, res.Status != StatusComplete || token.Done, "complete status requires done token")
				if !token.Done {
					rtest.Equals(t, StatusIncomplete, res.Status)
					rtest.Assert(t, len(res.Position.Stack) > 0, "incomplete token must expose a continuation position")
					id.validate(t, token, f)
				}
				segments++
				rtest.Assert(t, segments < 1000, "comparison did not finish")
			}

			rtest.Equals(t, refChanges, changes)
			rtest.Equals(t, refStats, token.stats())
			rtest.Assert(t, segments > 1 || maxNodes >= 1000, "small budgets must require multiple segments")
		}
	}
}

// TestNoDuplicateChangesWithInProcessContinuation keeps the live token
// across segments without serialization.
func TestNoDuplicateChangesWithInProcessContinuation(t *testing.T) {
	f := buildFixture(t)
	opts := Options{ShowMetadata: true}
	token := tokenFor(f, opts)
	c := NewComparer(f.repo, opts)
	var changes []Change
	for !token.Done {
		res, err := c.RunSegment(context.Background(), token, Budget{MaxNodes: 2}, func(ch *Change) {
			changes = append(changes, *ch)
		}, nil)
		rtest.OK(t, err)
		_ = res
	}
	rtest.Equals(t, expectedChanges(true), changes)
}

// TestCompletedSubtreeNotReloaded ensures that once a subtree has been fully
// processed, continuing from a saved token never requests it again.
func TestCompletedSubtreeNotReloaded(t *testing.T) {
	f := buildFixture(t)
	opts := Options{ShowMetadata: true}
	refChanges, refStats, _ := runReference(t, buildFixture(t), opts)

	token := tokenFor(f, opts)
	commonStarted := false
	var changes []Change

	// Drive segments of one node. Once the shared subtree frame has both
	// started and finished, block loading its tree blobs to prove they are
	// never requested again.
	for !token.Done {
		inCommon := false
		for _, fr := range token.Stack {
			if fr.Kind == frameCollect && (fr.Prefix == "/common/" || fr.Prefix == "/common/deep/") {
				inCommon = true
			}
		}
		if inCommon {
			commonStarted = true
		}
		if commonStarted && !inCommon {
			// common subtree completed in a previous segment
			f.repo.blocked[f.commonTree] = true
			f.repo.blocked[f.commonDeepTree] = true
		}

		buf, err := token.Marshal()
		rtest.OK(t, err)
		token, err = ParseResumeToken(buf)
		rtest.OK(t, err)

		c := NewComparer(f.repo, opts)
		res, err := c.RunSegment(context.Background(), token, Budget{MaxNodes: 1}, func(ch *Change) {
			changes = append(changes, *ch)
		}, nil)
		rtest.OK(t, err)
		_ = res
	}

	rtest.Equals(t, 0, f.repo.blockCalls)
	rtest.Equals(t, 0, len(token.Skipped))
	rtest.Equals(t, refChanges, changes)
	rtest.Equals(t, refStats, token.stats())
}

// TestCancelAndResume cancels the context after an increasing number of
// emitted changes and then completes the comparison in another run.
func TestCancelAndResume(t *testing.T) {
	refChanges, refStats, _ := runReference(t, buildFixture(t), Options{ShowMetadata: true})

	for cancelAfter := 0; cancelAfter <= len(refChanges); cancelAfter++ {
		f := buildFixture(t)
		opts := Options{ShowMetadata: true}
		token := tokenFor(f, opts)
		var changes []Change

		if cancelAfter < len(refChanges) {
			ctx, cancel := context.WithCancel(context.Background())
			emitted := 0
			c := NewComparer(f.repo, opts)
			res, err := c.RunSegment(ctx, token, Budget{}, func(ch *Change) {
				if emitted >= cancelAfter {
					cancel()
				}
				emitted++
				changes = append(changes, *ch)
			}, nil)
			if cancelAfter == 0 {
				rtest.Equals(t, StatusCanceled, res.Status)
				rtest.Assert(t, err != nil, "expected context error")
			}
			cancel()
			rtest.Assert(t, !token.Done, "comparison must remain resumable after cancellation")

			// continue from the serialized boundary with a fresh context
			buf, mErr := token.Marshal()
			rtest.OK(t, mErr)
			token, mErr = ParseResumeToken(buf)
			rtest.OK(t, mErr)
		}

		c := NewComparer(f.repo, opts)
		res, err := c.RunSegment(context.Background(), token, Budget{MaxNodes: 3}, func(ch *Change) {
			changes = append(changes, *ch)
		}, nil)
		rtest.OK(t, err)
		for !token.Done {
			res, err = c.RunSegment(context.Background(), token, Budget{MaxNodes: 3}, func(ch *Change) {
				changes = append(changes, *ch)
			}, nil)
			rtest.OK(t, err)
		}
		rtest.Equals(t, StatusComplete, res.Status)
		rtest.Equals(t, refChanges, changes)
		rtest.Equals(t, refStats, token.stats())
	}
}

// TestReadErrorBoundary makes an unreadable subtree a definite completion
// boundary: the parent change is still reported, descendants are skipped,
// the error is recorded in the token and siblings are compared normally.
func TestReadErrorBoundary(t *testing.T) {
	f := buildFixture(t)
	opts := Options{}
	// fail the removed subtree and the shared subtree
	f.repo.failLoad[f.goneTree] = true
	f.repo.failLoad[f.commonTree] = true

	var changes []Change
	var errs []error
	token := tokenFor(f, opts)
	c := NewComparer(f.repo, opts)
	res, err := c.RunSegment(context.Background(), token, Budget{}, func(ch *Change) {
		changes = append(changes, *ch)
	}, func(e error) {
		errs = append(errs, e)
	})
	rtest.OK(t, err)
	rtest.Equals(t, StatusComplete, res.Status)
	rtest.Equals(t, 2, len(errs))
	rtest.Equals(t, 2, len(token.Skipped))

	// the removed directory itself was reported before its frame failed
	rtest.Assert(t, containsChange(changes, Change{Path: "/gone/", Modifier: "-"}), "expected removed directory change")
	// its descendants were not
	rtest.Assert(t, !containsChange(changes, Change{Path: "/gone/oldfile", Modifier: "-"}), "skipped subtree must not be reported")
	// siblings before and after the failed subtrees were compared
	rtest.Assert(t, containsChange(changes, Change{Path: "/bitrot", Modifier: "M?"}), "expected bitrot change")
	rtest.Assert(t, containsChange(changes, Change{Path: "/only1", Modifier: "-"}), "expected later sibling change")

	// a completed token with skip records is terminal and survives another process
	buf, err := token.Marshal()
	rtest.OK(t, err)
	parsed, err := ParseResumeToken(buf)
	rtest.OK(t, err)
	rtest.Equals(t, 2, len(parsed.Skipped))
	c2 := NewComparer(nil, opts)
	res2, err := c2.RunSegment(context.Background(), parsed, Budget{}, nil, nil)
	rtest.OK(t, err)
	rtest.Equals(t, StatusComplete, res2.Status)
	rtest.Equals(t, token.stats(), res2.Stats)
}

// TestMissingBlobSize reports the lookup error but still finishes and returns
// statistics.
func TestMissingBlobSize(t *testing.T) {
	f := buildFixture(t)
	opts := Options{}
	// the "only-one" data blob exists only in the source snapshot, so it is
	// always looked up during statistics finalization
	victim := restic.BlobHandle{Type: restic.DataBlob, ID: restic.Hash([]byte("only-one"))}
	rtest.Assert(t, f.repo.sizes[victim] > 0, "expected victim blob to be known")
	delete(f.repo.sizes, victim)

	token := tokenFor(f, opts)
	var errs []error
	c := NewComparer(f.repo, opts)
	res, err := c.RunSegment(context.Background(), token, Budget{}, nil, func(e error) {
		errs = append(errs, e)
	})
	rtest.OK(t, err)
	rtest.Equals(t, StatusComplete, res.Status)
	rtest.Equals(t, 1, len(errs))
	rtest.Equals(t, 1, len(res.ReadErrors))
}

// TestRootReadFailureIsFatalButRetriable verifies that an unreadable root
// tree aborts the segment (like a full comparison would), leaves the token at
// a resumable boundary and succeeds after a retry.
func TestRootReadFailureIsFatalButRetriable(t *testing.T) {
	f := buildFixture(t)
	refChanges, refStats, _ := runReference(t, buildFixture(t), Options{})

	f.repo.failLoad[f.root1] = true
	token := tokenFor(f, Options{})
	c := NewComparer(f.repo, Options{})
	res, err := c.RunSegment(context.Background(), token, Budget{}, nil, nil)
	rtest.Assert(t, err != nil, "expected root read error")
	rtest.Equals(t, StatusFailed, res.Status)
	rtest.Assert(t, !token.Done, "failed comparison must stay resumable")
	rtest.Equals(t, 1, len(token.Stack))
	rtest.Equals(t, "", token.Stack[0].After)

	delete(f.repo.failLoad, f.root1)
	var changes []Change
	for !token.Done {
		res, err = c.RunSegment(context.Background(), token, Budget{MaxNodes: 3}, func(ch *Change) {
			changes = append(changes, *ch)
		}, nil)
		rtest.OK(t, err)
	}
	rtest.Equals(t, StatusComplete, res.Status)
	rtest.Equals(t, refChanges, changes)
	rtest.Equals(t, refStats, token.stats())
}

func TestCredentialValidation(t *testing.T) {
	f := buildFixture(t)
	opts := Options{ShowMetadata: true}
	token := tokenFor(f, opts)
	id := idFor(f, opts)

	root := func(sub1, sub2 string) (restic.ID, restic.ID) {
		r1, r2, err := ResolveRoots(context.Background(), f.repo, &f.root1, &f.root2, sub1, sub2)
		rtest.OK(t, err)
		return r1, r2
	}
	r1, r2 := root("", "")

	// happy path
	rtest.OK(t, token.Validate(id.repoID, id.sn1, id.sn2, "", "", r1, r2, opts))

	other := restic.Hash([]byte("other"))
	rtest.Assert(t, token.Validate("other-repo", id.sn1, id.sn2, "", "", r1, r2, opts) != nil, "expected repo mismatch")
	rtest.Assert(t, token.Validate(id.repoID, other, id.sn2, "", "", r1, r2, opts) != nil, "expected source mismatch")
	rtest.Assert(t, token.Validate(id.repoID, id.sn1, other, "", "", r1, r2, opts) != nil, "expected target mismatch")
	rtest.Assert(t, token.Validate(id.repoID, id.sn1, id.sn2, "sub", "", r1, r2, opts) != nil, "expected subdir mismatch")
	rtest.Assert(t, token.Validate(id.repoID, id.sn1, id.sn2, "", "", r2, r1, opts) != nil, "expected root mismatch")
	rtest.Assert(t, token.Validate(id.repoID, id.sn1, id.sn2, "", "", r1, r2, Options{ShowMetadata: false}) != nil, "expected options mismatch")

	// comparer options must match the token
	c := NewComparer(f.repo, Options{ShowMetadata: false})
	_, err := c.RunSegment(context.Background(), token, Budget{}, nil, nil)
	rtest.Assert(t, err != nil, "expected options mismatch error")

	// corrupted serialization is rejected
	_, err = ParseResumeToken([]byte(`{"kind":"bogus","version":1}`))
	rtest.Assert(t, err != nil, "expected invalid token error")

	// a token with an unknown frame kind is rejected instead of being
	// treated as a skipped subtree
	buf, err := token.Marshal()
	rtest.OK(t, err)
	var raw map[string]any
	rtest.OK(t, json.Unmarshal(buf, &raw))
	stack := raw["stack"].([]any)
	stack[0].(map[string]any)["kind"] = "bogus"
	buf, err = json.Marshal(raw)
	rtest.OK(t, err)
	_, err = ParseResumeToken(buf)
	rtest.Assert(t, err != nil, "expected invalid frame kind error")
}

func TestCompletedTokenMarshalRoundtrip(t *testing.T) {
	f := buildFixture(t)
	opts := Options{}
	_, refStats, token := runReference(t, f, opts)
	rtest.Assert(t, token.Done, "reference run must be complete")

	buf, err := token.Marshal()
	rtest.OK(t, err)
	parsed, err := ParseResumeToken(buf)
	rtest.OK(t, err)
	rtest.Assert(t, parsed.Done, "parsed token must remain complete")
	rtest.Equals(t, refStats, parsed.stats())
}

func containsChange(changes []Change, want Change) bool {
	for _, ch := range changes {
		if ch == want {
			return true
		}
	}
	return false
}
