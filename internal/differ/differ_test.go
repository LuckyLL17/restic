package differ_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/restic/restic/internal/data"
	"github.com/restic/restic/internal/differ"
	"github.com/restic/restic/internal/repository"
	"github.com/restic/restic/internal/restic"
	rtest "github.com/restic/restic/internal/test"
)

type collector struct {
	changes []string
	errors  []string
}

func newCollector() *collector {
	return &collector{}
}

func (c *collector) onChange(change *differ.Change) {
	c.changes = append(c.changes, change.Modifier+" "+change.Path)
}

func (c *collector) onError(format string, args ...any) {
	c.errors = append(c.errors, fmt.Sprintf(format, args...))
}

func nodesSeq(nodes ...*data.Node) data.TreeNodeIterator {
	return func(yield func(data.NodeOrError) bool) {
		for _, n := range nodes {
			if !yield(data.NodeOrError{Node: n}) {
				return
			}
		}
	}
}

func fileNode(name string, content ...restic.ID) *data.Node {
	return &data.Node{
		Name:    name,
		Type:    data.NodeTypeFile,
		Mode:    0o644,
		ModTime: time.Unix(1700000000, 0),
		Content: content,
	}
}

func fileNodeMode(name string, mode os.FileMode, content ...restic.ID) *data.Node {
	n := fileNode(name, content...)
	n.Mode = mode
	return n
}

func symlinkNode(name, target string) *data.Node {
	return &data.Node{Name: name, Type: data.NodeTypeSymlink, LinkTarget: target, ModTime: time.Unix(1700000000, 0)}
}

func dirNode(name string, subtree restic.ID) *data.Node {
	return &data.Node{Name: name, Type: data.NodeTypeDir, Subtree: &subtree, ModTime: time.Unix(1700000000, 0)}
}

type fixture struct {
	repo  *repository.Repository
	cfgID string
	ref1  differ.SnapshotRef
	ref2  differ.SnapshotRef

	root1, root2 restic.ID
	// common is a subtree that exists unchanged in both snapshots.
	common restic.ID
}

// buildFixture creates a repository with two snapshots that cover shared
// subtrees, added/removed subtrees, modified files, type changes and
// metadata-only changes.
func buildFixture(t *testing.T) *fixture {
	ctx := context.Background()
	repo, _, _ := repository.TestRepositoryWithVersion(t, 2)

	var treeIDs struct {
		common, commonDeeper restic.ID
		removed, removedOld  restic.ID
		added, addedNested   restic.ID
	}
	var dataIDs map[string]restic.ID
	var root1, root2 restic.ID

	rtest.OK(t, repo.WithBlobUploader(ctx, func(ctx context.Context, up restic.BlobSaverWithAsync) error {
		dataIDs = make(map[string]restic.ID)
		dataID := func(name, content string) restic.ID {
			id, _, _, err := up.SaveBlob(ctx, restic.DataBlob, []byte(content), restic.ID{}, false)
			rtest.OK(t, err)
			dataIDs[name] = id
			return id
		}
		saveTree := func(nodes ...*data.Node) restic.ID {
			id, err := data.SaveTree(ctx, up, nodesSeq(nodes...))
			rtest.OK(t, err)
			return id
		}

		dataID("A", "shared file")
		dataID("B", "deep file")
		dataID("R", "removed file")
		dataID("X", "old nested file")
		dataID("M1", "modified before")
		dataID("M2", "modified after, longer content")
		dataID("T1", "was a file")
		dataID("ME", "same meta content")
		dataID("S", "unchanged")
		dataID("N", "new file")
		dataID("Y", "new nested")
		dataID("O1", "only in first")
		dataID("O2", "only in second")

		// shared subtree, referenced by both root trees
		treeIDs.commonDeeper = saveTree(fileNode("deep.txt", dataIDs["B"]))
		treeIDs.common = saveTree(
			dirNode("deeper", treeIDs.commonDeeper),
			fileNode("shared.txt", dataIDs["A"]),
		)

		treeIDs.removedOld = saveTree(fileNode("x.txt", dataIDs["X"]))
		treeIDs.removed = saveTree(
			fileNode("gone.txt", dataIDs["R"]),
			dirNode("oldsdir", treeIDs.removedOld),
		)

		treeIDs.addedNested = saveTree(fileNode("y.txt", dataIDs["Y"]))
		treeIDs.added = saveTree(
			dirNode("nested", treeIDs.addedNested),
			fileNode("new.txt", dataIDs["N"]),
		)

		root1 = saveTree(
			dirNode("common-dir", treeIDs.common),
			fileNode("meta.txt", dataIDs["ME"]),
			fileNode("modified.txt", dataIDs["M1"]),
			fileNode("only1.txt", dataIDs["O1"]),
			dirNode("removed-dir", treeIDs.removed),
			fileNode("same.txt", dataIDs["S"]),
			fileNode("typechange", dataIDs["T1"]),
		)
		root2 = saveTree(
			dirNode("added-dir", treeIDs.added),
			dirNode("common-dir", treeIDs.common),
			fileNodeMode("meta.txt", 0o600, dataIDs["ME"]),
			fileNode("modified.txt", dataIDs["M2"]),
			fileNode("only2.txt", dataIDs["O2"]),
			fileNode("same.txt", dataIDs["S"]),
			symlinkNode("typechange", "somewhere"),
		)
		return nil
	}))

	rtest.OK(t, repo.LoadIndex(ctx, restic.NoopTerminalCounterFactory))

	saveSnapshot := func(root restic.ID) restic.ID {
		sn := &data.Snapshot{Time: time.Unix(1700000000, 0), Tree: &root, Paths: []string{"/test"}, Hostname: "test"}
		id, err := data.SaveSnapshot(ctx, repo, sn)
		rtest.OK(t, err)
		return id
	}
	id1 := saveSnapshot(root1)
	id2 := saveSnapshot(root2)

	return &fixture{
		repo:   repo,
		cfgID:  repo.Config().ID,
		ref1:   differ.SnapshotRef{ID: id1, Tree: root1, Resolved: root1},
		ref2:   differ.SnapshotRef{ID: id2, Tree: root2, Resolved: root2},
		root1:  root1,
		root2:  root2,
		common: treeIDs.common,
	}
}

func expectedChanges() []string {
	return []string{
		"+ /added-dir/",
		"+ /added-dir/nested/",
		"+ /added-dir/nested/y.txt",
		"+ /added-dir/new.txt",
		"U /meta.txt",
		"M? /modified.txt",
		"- /only1.txt",
		"+ /only2.txt",
		"- /removed-dir/",
		"- /removed-dir/gone.txt",
		"- /removed-dir/oldsdir/",
		"- /removed-dir/oldsdir/x.txt",
		"TU /typechange",
	}
}

func TestCompareFull(t *testing.T) {
	f := buildFixture(t)
	ctx := context.Background()
	c := differ.New(f.repo, differ.Options{ShowMetadata: true})

	col := newCollector()
	stats, err := c.Compare(ctx, f.ref1.Resolved, f.ref2.Resolved, col.onChange, col.onError)
	rtest.OK(t, err)
	c.Finalize(stats, col.onError)

	rtest.Equals(t, expectedChanges(), col.changes)
	rtest.Equals(t, 0, len(col.errors))
	rtest.Equals(t, 3, stats.Added.Files)
	rtest.Equals(t, 2, stats.Added.Dirs)
	rtest.Equals(t, 3, stats.Removed.Files)
	rtest.Equals(t, 2, stats.Removed.Dirs)
	rtest.Equals(t, 1, stats.ChangedFiles)
	rtest.Equals(t, 4, stats.Added.DataBlobs)
	rtest.Equals(t, 5, stats.Removed.DataBlobs)
	rtest.Equals(t, 3, stats.Added.TreeBlobs)
	rtest.Equals(t, 3, stats.Removed.TreeBlobs)
}

// runSegmented runs the comparison in segments of at most limit entries,
// serializing and parsing the checkpoint between segments as another process
// would do.
func runSegmented(t *testing.T, repo restic.Repository, cp *differ.Checkpoint, limit int64, col *collector, ctx context.Context) *differ.Checkpoint {
	t.Helper()
	for {
		buf, err := cp.Marshal()
		rtest.OK(t, err)
		cp, err = differ.ParseCheckpoint(buf)
		rtest.OK(t, err)

		c := differ.New(repo, differ.Options{ShowMetadata: true})
		res, err := c.RunSegment(ctx, cp, limit, col.onChange, col.onError)
		rtest.OK(t, err)
		cp = res.Checkpoint

		if res.Status == differ.StatusComplete {
			rtest.Equals(t, "", cp.ContinuationPosition())
			rtest.Assert(t, cp.Complete(), "expected complete checkpoint")
			return cp
		}
		rtest.Assert(t, res.Entries > 0, "expected the segment to make progress")
		rtest.Assert(t, cp.ContinuationPosition() != "", "expected a continuation position")
	}
}

func TestSegmentedEqualsFull(t *testing.T) {
	f := buildFixture(t)
	ctx := context.Background()
	opts := differ.Options{ShowMetadata: true}

	full := newCollector()
	c := differ.New(f.repo, opts)
	stats, err := c.Compare(ctx, f.ref1.Resolved, f.ref2.Resolved, full.onChange, full.onError)
	rtest.OK(t, err)
	c.Finalize(stats, full.onError)

	cp, err := differ.NewCheckpoint(f.cfgID, f.ref1, f.ref2, opts)
	rtest.OK(t, err)
	rtest.OK(t, cp.Verify(f.cfgID, f.ref1, f.ref2, opts))

	segCol := newCollector()
	cp = runSegmented(t, f.repo, cp, 1, segCol, ctx)

	// stitched change records must match the complete comparison exactly
	rtest.Equals(t, expectedChanges(), segCol.changes)
	rtest.Equals(t, full.changes, segCol.changes)
	rtest.Equals(t, 0, len(segCol.errors))

	// final tree/data statistics must match as well
	rtest.Equals(t, stats.ChangedFiles, cp.Stats.ChangedFiles)
	rtest.Equals(t, stats.Added, cp.Stats.Added)
	rtest.Equals(t, stats.Removed, cp.Stats.Removed)
}

func TestSegmentedNoWorkRepeated(t *testing.T) {
	f := buildFixture(t)
	ctx := context.Background()
	opts := differ.Options{ShowMetadata: true}

	cp, err := differ.NewCheckpoint(f.cfgID, f.ref1, f.ref2, opts)
	rtest.OK(t, err)

	col := newCollector()
	seen := make(map[string]bool)
	emitted := 0
	for {
		c := differ.New(f.repo, opts)
		res, err := c.RunSegment(ctx, cp, 1, col.onChange, col.onError)
		rtest.OK(t, err)
		cp = res.Checkpoint
		for _, ch := range col.changes[emitted:] {
			rtest.Assert(t, !seen[ch], "change emitted twice after resume: %v", ch)
			seen[ch] = true
		}
		emitted = len(col.changes)
		if res.Status == differ.StatusComplete {
			break
		}
	}
	rtest.Equals(t, expectedChanges(), col.changes)
}

func TestSegmentReplayDeterministic(t *testing.T) {
	f := buildFixture(t)
	ctx := context.Background()
	opts := differ.Options{ShowMetadata: true}

	cp, err := differ.NewCheckpoint(f.cfgID, f.ref1, f.ref2, opts)
	rtest.OK(t, err)

	stop := newCollector()
	res, err := differ.New(f.repo, opts).RunSegment(ctx, cp, 5, stop.onChange, stop.onError)
	rtest.OK(t, err)
	rtest.Assert(t, res.Status == differ.StatusRunning, "expected a continuation position")
	buf, err := res.Checkpoint.Marshal()
	rtest.OK(t, err)

	// replay the same next segment twice from independent checkpoints
	runOnce := func() *collector {
		parsed, err := differ.ParseCheckpoint(buf)
		rtest.OK(t, err)
		col := newCollector()
		_, err = differ.New(f.repo, opts).RunSegment(ctx, parsed, 3, col.onChange, col.onError)
		rtest.OK(t, err)
		return col
	}
	rtest.Equals(t, runOnce().changes, runOnce().changes)
}

func TestCheckpointBinding(t *testing.T) {
	f := buildFixture(t)
	opts := differ.Options{ShowMetadata: true}

	cp, err := differ.NewCheckpoint(f.cfgID, f.ref1, f.ref2, opts)
	rtest.OK(t, err)
	buf, err := cp.Marshal()
	rtest.OK(t, err)

	otherID := restic.NewRandomID()

	cases := []struct {
		name string
		mod  func(s, d *differ.SnapshotRef, o *differ.Options, repoID *string)
	}{
		{"repository", func(s, d *differ.SnapshotRef, o *differ.Options, r *string) { *r = "other-repo" }},
		{"source snapshot", func(s, d *differ.SnapshotRef, o *differ.Options, r *string) { s.ID = otherID }},
		{"target snapshot", func(s, d *differ.SnapshotRef, o *differ.Options, r *string) { d.ID = otherID }},
		{"source tree", func(s, d *differ.SnapshotRef, o *differ.Options, r *string) { s.Tree = otherID }},
		{"target tree", func(s, d *differ.SnapshotRef, o *differ.Options, r *string) { d.Tree = otherID }},
		{"source root", func(s, d *differ.SnapshotRef, o *differ.Options, r *string) { s.Resolved = otherID }},
		{"target root", func(s, d *differ.SnapshotRef, o *differ.Options, r *string) { d.Resolved = otherID }},
		{"source subfolder", func(s, d *differ.SnapshotRef, o *differ.Options, r *string) { s.Subfolder = "sub/dir" }},
		{"target subfolder", func(s, d *differ.SnapshotRef, o *differ.Options, r *string) { d.Subfolder = "sub/dir" }},
		{"metadata option", func(s, d *differ.SnapshotRef, o *differ.Options, r *string) { o.ShowMetadata = false }},
	}

	for _, cas := range cases {
		t.Run(cas.name, func(t *testing.T) {
			parsed, err := differ.ParseCheckpoint(buf)
			rtest.OK(t, err)
			src, dst, o, repoID := f.ref1, f.ref2, opts, f.cfgID
			cas.mod(&src, &dst, &o, &repoID)
			err = parsed.Verify(repoID, src, dst, o)
			rtest.Assert(t, err != nil, "expected binding mismatch for %s", cas.name)
		})
	}

	// equivalent subfolder spellings normalize to the same binding
	src := f.ref1
	src.Subfolder = "./"
	rtest.OK(t, cp.Verify(f.cfgID, src, f.ref2, opts))

	// checkpoint creation requires a full binding
	partial := f.ref1
	partial.ID = restic.ID{}
	_, err = differ.NewCheckpoint(f.cfgID, partial, f.ref2, opts)
	rtest.Assert(t, err != nil, "expected error for partial snapshot identity")
	_, err = differ.NewCheckpoint("", f.ref1, f.ref2, opts)
	rtest.Assert(t, err != nil, "expected error for missing repository identity")

	// repository identity is stable
	rtest.Equals(t, f.cfgID, f.repo.Config().ID)
}

// saveCorruptTree stores an undecodable tree blob and returns its ID.
func saveCorruptTree(t *testing.T, repo *repository.Repository) restic.ID {
	t.Helper()
	ctx := context.Background()
	var id restic.ID
	rtest.OK(t, repo.WithBlobUploader(ctx, func(ctx context.Context, up restic.BlobSaverWithAsync) error {
		var err error
		id, _, _, err = up.SaveBlob(ctx, restic.TreeBlob, []byte("{not a tree"), restic.ID{}, false)
		return err
	}))
	return id
}

// replaceRoot rebuilds the second snapshot root with the added subtree
// pointing at the given tree id while keeping the shared subtree identical.
func replaceRoot(t *testing.T, f *fixture, addedID restic.ID) restic.ID {
	t.Helper()
	ctx := context.Background()
	var root restic.ID
	rtest.OK(t, f.repo.WithBlobUploader(ctx, func(ctx context.Context, up restic.BlobSaverWithAsync) error {
		dataID := func(content string) restic.ID {
			id, _, _, err := up.SaveBlob(ctx, restic.DataBlob, []byte(content), restic.ID{}, false)
			rtest.OK(t, err)
			return id
		}
		saveTree := func(nodes ...*data.Node) restic.ID {
			id, err := data.SaveTree(ctx, up, nodesSeq(nodes...))
			rtest.OK(t, err)
			return id
		}
		root = saveTree(
			dirNode("added-dir", addedID),
			dirNode("common-dir", f.common),
			fileNodeMode("meta.txt", 0o600, dataID("same meta content")),
			fileNode("modified.txt", dataID("modified after, longer content")),
			fileNode("only2.txt", dataID("only in second")),
			fileNode("same.txt", dataID("unchanged")),
			symlinkNode("typechange", "somewhere"),
		)
		return nil
	}))
	return root
}

func TestFailureBoundaryNested(t *testing.T) {
	f := buildFixture(t)
	ctx := context.Background()
	opts := differ.Options{ShowMetadata: true}

	corrupt := saveCorruptTree(t, f.repo)
	rtest.OK(t, f.repo.LoadIndex(ctx, restic.NoopTerminalCounterFactory))
	root2 := replaceRoot(t, f, corrupt)
	target := differ.SnapshotRef{ID: f.ref2.ID, Tree: root2, Resolved: root2}

	fullCol := newCollector()
	_, err := differ.New(f.repo, opts).Compare(ctx, f.ref1.Resolved, root2, fullCol.onChange, fullCol.onError)
	rtest.OK(t, err)
	rtest.Assert(t, len(fullCol.errors) == 1, "expected one non-fatal error, got %v", fullCol.errors)

	cp, err := differ.NewCheckpoint(f.cfgID, f.ref1, target, opts)
	rtest.OK(t, err)
	segCol := newCollector()
	cp = runSegmented(t, f.repo, cp, 1, segCol, ctx)

	rtest.Equals(t, fullCol.changes, segCol.changes)
	rtest.Equals(t, fullCol.errors, segCol.errors)
	rtest.Assert(t, len(cp.Failures) == 1, "expected one failure boundary")
	failure := cp.Failures[0]
	rtest.Equals(t, differ.FrameAdded, failure.Kind)
	rtest.Equals(t, "/added-dir/", failure.Prefix)
	rtest.Assert(t, failure.Error != "", "expected error message")
	rtest.Assert(t, cp.Complete(), "comparison completes despite failed subtree")

	// the abandoned subtree contributes nothing, its parent does
	rtest.Equals(t, 1, cp.Stats.Added.Dirs)
	rtest.Equals(t, 1, cp.Stats.Added.Files) // only2.txt; modified files are not "added"
}

// replaceRootsWithCorruptCommon rebuilds both roots so that the shared
// "common-dir/deeper" subtree points at an unreadable tree.
func replaceRootsWithCorruptCommon(t *testing.T, f *fixture, corrupt restic.ID) (root1, root2 restic.ID) {
	t.Helper()
	ctx := context.Background()
	rtest.OK(t, f.repo.WithBlobUploader(ctx, func(ctx context.Context, up restic.BlobSaverWithAsync) error {
		dataID := func(content string) restic.ID {
			id, _, _, err := up.SaveBlob(ctx, restic.DataBlob, []byte(content), restic.ID{}, false)
			rtest.OK(t, err)
			return id
		}
		saveTree := func(nodes ...*data.Node) restic.ID {
			id, err := data.SaveTree(ctx, up, nodesSeq(nodes...))
			rtest.OK(t, err)
			return id
		}

		common := saveTree(
			dirNode("deeper", corrupt),
			fileNode("shared.txt", dataID("shared file")),
		)
		removedOld := saveTree(fileNode("x.txt", dataID("old nested file")))
		removed := saveTree(
			fileNode("gone.txt", dataID("removed file")),
			dirNode("oldsdir", removedOld),
		)
		addedNested := saveTree(fileNode("y.txt", dataID("new nested")))
		added := saveTree(
			dirNode("nested", addedNested),
			fileNode("new.txt", dataID("new file")),
		)

		root1 = saveTree(
			dirNode("common-dir", common),
			fileNode("meta.txt", dataID("same meta content")),
			fileNode("modified.txt", dataID("modified before")),
			fileNode("only1.txt", dataID("only in first")),
			dirNode("removed-dir", removed),
			fileNode("same.txt", dataID("unchanged")),
			fileNode("typechange", dataID("was a file")),
		)
		root2 = saveTree(
			dirNode("added-dir", added),
			dirNode("common-dir", common),
			fileNodeMode("meta.txt", 0o600, dataID("same meta content")),
			fileNode("modified.txt", dataID("modified after, longer content")),
			fileNode("only2.txt", dataID("only in second")),
			fileNode("same.txt", dataID("unchanged")),
			symlinkNode("typechange", "somewhere"),
		)
		return nil
	}))
	return root1, root2
}

func TestFailureBoundaryCommonSubtree(t *testing.T) {
	f := buildFixture(t)
	ctx := context.Background()
	opts := differ.Options{ShowMetadata: true}

	corrupt := saveCorruptTree(t, f.repo)
	rtest.OK(t, f.repo.LoadIndex(ctx, restic.NoopTerminalCounterFactory))
	root1, root2 := replaceRootsWithCorruptCommon(t, f, corrupt)

	fullCol := newCollector()
	_, err := differ.New(f.repo, opts).Compare(ctx, root1, root2, fullCol.onChange, fullCol.onError)
	rtest.OK(t, err)
	rtest.Assert(t, len(fullCol.errors) == 1, "expected one non-fatal error, got %v", fullCol.errors)

	src := differ.SnapshotRef{ID: f.ref1.ID, Tree: root1, Resolved: root1}
	dst := differ.SnapshotRef{ID: f.ref2.ID, Tree: root2, Resolved: root2}
	cp, err := differ.NewCheckpoint(f.cfgID, src, dst, opts)
	rtest.OK(t, err)
	segCol := newCollector()
	cp = runSegmented(t, f.repo, cp, 1, segCol, ctx)

	rtest.Equals(t, fullCol.changes, segCol.changes)
	rtest.Equals(t, fullCol.errors, segCol.errors)
	rtest.Assert(t, len(cp.Failures) == 1, "expected one failure boundary")
	rtest.Equals(t, differ.FrameCommon, cp.Failures[0].Kind)
	rtest.Equals(t, "/common-dir/deeper/", cp.Failures[0].Prefix)
	rtest.Assert(t, cp.Complete(), "comparison completes despite failed shared subtree")
}

func TestFailureBoundaryRoot(t *testing.T) {
	f := buildFixture(t)
	ctx := context.Background()
	opts := differ.Options{ShowMetadata: true}
	corrupt := saveCorruptTree(t, f.repo)

	cp, err := differ.NewCheckpoint(f.cfgID, f.ref1, differ.SnapshotRef{ID: f.ref2.ID, Tree: corrupt, Resolved: corrupt}, opts)
	rtest.OK(t, err)

	col := newCollector()
	res, err := differ.New(f.repo, opts).RunSegment(ctx, cp, 10, col.onChange, col.onError)
	rtest.Assert(t, err != nil, "expected fatal error for unreadable root tree")
	rtest.Assert(t, res == nil, "no result on fatal error")
	rtest.Assert(t, !errors.Is(err, context.Canceled), "root failure must not look like cancellation")
	rtest.Assert(t, len(cp.Failures) == 1, "root failure leaves a judgeable boundary")
	rtest.Assert(t, !cp.Complete(), "root failure is not a completion")
	rtest.Equals(t, "/", cp.ContinuationPosition())
}

func TestCancellationBoundary(t *testing.T) {
	f := buildFixture(t)
	opts := differ.Options{ShowMetadata: true}

	fullCol := newCollector()
	fullStats, err := differ.New(f.repo, opts).Compare(context.Background(), f.ref1.Resolved, f.ref2.Resolved, fullCol.onChange, fullCol.onError)
	rtest.OK(t, err)
	differ.New(f.repo, opts).Finalize(fullStats, fullCol.onError)

	cp, err := differ.NewCheckpoint(f.cfgID, f.ref1, f.ref2, opts)
	rtest.OK(t, err)
	col := newCollector()

	// cancel a large segment after seeing a few changes
	ctx, cancel := context.WithCancel(context.Background())
	seen := 0
	res, err := differ.New(f.repo, opts).RunSegment(ctx, cp, 1000, func(change *differ.Change) {
		col.onChange(change)
		seen++
		if seen == 3 {
			cancel()
		}
	}, col.onError)
	rtest.OK(t, err)
	rtest.Equals(t, differ.StatusCanceled, res.Status)
	rtest.Assert(t, res.Entries > 0, "canceled segment completed whole entries")
	cp = res.Checkpoint

	// the canceled boundary survives serialization into another "process"
	buf, err := cp.Marshal()
	rtest.OK(t, err)
	cp, err = differ.ParseCheckpoint(buf)
	rtest.OK(t, err)
	rtest.Equals(t, differ.StatusCanceled, cp.Status)

	// continue to completion with small segments
	cp = runSegmented(t, f.repo, cp, 1, col, context.Background())
	rtest.Equals(t, fullCol.changes, col.changes)
	rtest.Equals(t, fullStats.Added, cp.Stats.Added)
	rtest.Equals(t, fullStats.Removed, cp.Stats.Removed)
	rtest.Equals(t, fullStats.ChangedFiles, cp.Stats.ChangedFiles)
}

func TestInvalidSegmentArguments(t *testing.T) {
	f := buildFixture(t)
	ctx := context.Background()
	opts := differ.Options{ShowMetadata: true}
	c := differ.New(f.repo, opts)
	col := newCollector()

	cp, err := differ.NewCheckpoint(f.cfgID, f.ref1, f.ref2, opts)
	rtest.OK(t, err)

	if _, err := c.RunSegment(ctx, cp, 0, col.onChange, col.onError); err == nil {
		t.Fatal("expected error for non-positive limit")
	}

	res, err := c.RunSegment(ctx, cp, 10000, col.onChange, col.onError)
	rtest.OK(t, err)
	rtest.Assert(t, res.Status == differ.StatusComplete, "comparison should finish")
	if _, err := c.RunSegment(ctx, res.Checkpoint, 1, col.onChange, col.onError); err == nil {
		t.Fatal("expected error when continuing a completed checkpoint")
	}
}
