package differ

import (
	"bytes"
	"context"
	"encoding/json"
	"path"
	"slices"

	"github.com/restic/restic/internal/errors"
	"github.com/restic/restic/internal/restic"
)

// checkpointVersion is the version of the serialized checkpoint format.
const checkpointVersion = 1

// Status describes the state a checkpoint was left in after the last segment.
type Status string

const (
	// StatusRunning means the comparison has a valid continuation position.
	StatusRunning Status = "running"
	// StatusComplete means the whole comparison is finished and the stats
	// are final.
	StatusComplete Status = "complete"
	// StatusCanceled means the segment stopped at a safe boundary because the
	// context was canceled. The continuation position is valid.
	StatusCanceled Status = "canceled"
)

// FrameKind identifies one of the traversal modes recorded by a frame.
type FrameKind string

const (
	// FrameDiff compares two directories with different tree IDs.
	FrameDiff FrameKind = "diff"
	// FrameCommon traverses a subtree identical in both snapshots.
	FrameCommon FrameKind = "common"
	// FrameAdded traverses a subtree present only in the target snapshot.
	FrameAdded FrameKind = "added"
	// FrameRemoved traverses a subtree present only in the source snapshot.
	FrameRemoved FrameKind = "removed"
)

// SnapshotRef binds a comparison to one resolved snapshot side.
type SnapshotRef struct {
	// ID is the full snapshot identity.
	ID restic.ID
	// Tree is the snapshot's root tree (Snapshot.Tree).
	Tree restic.ID
	// Subfolder is the "snapshotID:subfolder" path within the snapshot. It is
	// empty when the whole snapshot tree is compared.
	Subfolder string
	// Resolved is the comparison root after resolving Subfolder.
	Resolved restic.ID
}

// FailureBoundary records a subtree that could not be read completely.
// Processing of the owning directory entry itself finished (its change and
// own blob references are accounted for), while the subtree below it was
// abandoned, exactly like the non-fatal error handling of the diff command.
type FailureBoundary struct {
	Kind   FrameKind `json:"kind"`
	Prefix string    `json:"prefix"`
	// Entry is the name of the owning directory entry, empty when the root
	// frame failed.
	Entry string `json:"entry,omitempty"`
	// After is the last fully processed entry within the failed frame.
	After string `json:"after,omitempty"`
	Tree1 string `json:"tree1,omitempty"`
	Tree2 string `json:"tree2,omitempty"`
	Error string `json:"error"`
}

// blobRef is the serializable identity of a blob within a repository. Blob IDs
// are content-addressed, so the identity stays valid in another process after
// the index has been reloaded.
type blobRef struct {
	Type restic.BlobType `json:"type"`
	ID   restic.ID       `json:"id"`
}

type statsState struct {
	ChangedFiles int       `json:"changed_files"`
	Added        Stat      `json:"added"`
	Removed      Stat      `json:"removed"`
	BlobsBefore  []blobRef `json:"blobs_before,omitempty"`
	BlobsAfter   []blobRef `json:"blobs_after,omitempty"`
	BlobsCommon  []blobRef `json:"blobs_common,omitempty"`
}

// Checkpoint is the resumable comparison credential. It binds the comparison
// to a repository, the two complete snapshot identities, the compared
// subdirectories and the comparison options, and carries the continuation
// position and the accumulated result.
type Checkpoint struct {
	Version int `json:"version"`

	RepositoryID string `json:"repository_id"`

	Snapshot1     string `json:"source_snapshot"`
	OriginalTree1 string `json:"source_original_tree"`
	Subfolder1    string `json:"source_subfolder,omitempty"`
	RootTree1     string `json:"source_tree"`

	Snapshot2     string `json:"target_snapshot"`
	OriginalTree2 string `json:"target_original_tree"`
	Subfolder2    string `json:"target_subfolder,omitempty"`
	RootTree2     string `json:"target_tree"`

	ShowMetadata bool `json:"show_metadata"`

	// Segments counts the segments finished at a safe boundary.
	Segments uint64 `json:"segments"`
	// EntriesDone counts directory entries whose comparison is complete.
	EntriesDone int64 `json:"entries_done"`

	Status Status `json:"status"`

	// Stack is the continuation position: root frame first, innermost frame
	// last. Completed subtrees have been popped and are never revisited.
	Stack []frame `json:"stack"`

	Stats    statsState        `json:"stats"`
	Failures []FailureBoundary `json:"failures,omitempty"`
}

// normalizeSubfolder canonicalizes the subfolder part of a
// "snapshotID:subfolder" descriptor for credential comparison.
func normalizeSubfolder(subfolder string) string {
	cleaned := path.Clean("/" + subfolder)
	if cleaned == "/" {
		return ""
	}
	return cleaned
}

// NewCheckpoint creates a checkpoint for a new comparison. The snapshot
// references must be resolved to their comparison roots (e.g. via
// data.FindTreeDirectory).
func NewCheckpoint(repoConfigID string, source, target SnapshotRef, opts Options) (*Checkpoint, error) {
	if repoConfigID == "" {
		return nil, errors.New("checkpoint requires a repository identity")
	}
	if source.ID.IsNull() || target.ID.IsNull() {
		return nil, errors.New("checkpoint requires full snapshot identities")
	}

	cp := &Checkpoint{
		Version:       checkpointVersion,
		RepositoryID:  repoConfigID,
		Snapshot1:     source.ID.String(),
		OriginalTree1: source.Tree.String(),
		Subfolder1:    normalizeSubfolder(source.Subfolder),
		RootTree1:     source.Resolved.String(),
		Snapshot2:     target.ID.String(),
		OriginalTree2: target.Tree.String(),
		Subfolder2:    normalizeSubfolder(target.Subfolder),
		RootTree2:     target.Resolved.String(),
		ShowMetadata:  opts.ShowMetadata,
		Status:        StatusRunning,
		Stack: []frame{
			{Kind: FrameDiff, Tree1: source.Resolved.String(), Tree2: target.Resolved.String(), Prefix: "/"},
		},
	}
	cp.Stats.BlobsBefore = []blobRef{{Type: restic.TreeBlob, ID: source.Resolved}}
	cp.Stats.BlobsAfter = []blobRef{{Type: restic.TreeBlob, ID: target.Resolved}}
	return cp, nil
}

// newTransientCheckpoint creates an unbound checkpoint for the one-shot diff
// command, whose progress is never persisted.
func newTransientCheckpoint(root1, root2 restic.ID, opts Options) *Checkpoint {
	return &Checkpoint{
		Version:      checkpointVersion,
		RootTree1:    root1.String(),
		RootTree2:    root2.String(),
		ShowMetadata: opts.ShowMetadata,
		Status:       StatusRunning,
		Stack: []frame{
			{Kind: FrameDiff, Tree1: root1.String(), Tree2: root2.String(), Prefix: "/"},
		},
		Stats: statsState{
			BlobsBefore: []blobRef{{Type: restic.TreeBlob, ID: root1}},
			BlobsAfter:  []blobRef{{Type: restic.TreeBlob, ID: root2}},
		},
	}
}

// Marshal serializes the checkpoint.
func (cp *Checkpoint) Marshal() ([]byte, error) {
	return json.Marshal(cp)
}

// ParseCheckpoint parses a serialized checkpoint.
func ParseCheckpoint(buf []byte) (*Checkpoint, error) {
	cp := &Checkpoint{}
	if err := json.Unmarshal(buf, cp); err != nil {
		return nil, errors.Errorf("invalid diff checkpoint: %v", err)
	}
	if cp.Version != checkpointVersion {
		return nil, errors.Errorf("unsupported diff checkpoint version %d", cp.Version)
	}
	return cp, nil
}

// Verify checks that cp is a valid continuation credential for the given
// repository, snapshot identities, subdirectories and options. A mismatch
// means the checkpoint belongs to a different comparison and must not be
// continued.
func (cp *Checkpoint) Verify(repoConfigID string, source, target SnapshotRef, opts Options) error {
	if cp.Version != checkpointVersion {
		return errors.Errorf("unsupported diff checkpoint version %d", cp.Version)
	}

	var mismatch []string
	check := func(ok bool, what, got, want string) {
		if !ok {
			mismatch = append(mismatch, what+" = "+got+" (want "+want+")")
		}
	}

	check(cp.RepositoryID == repoConfigID, "repository", cp.RepositoryID, repoConfigID)
	check(cp.Snapshot1 == source.ID.String(), "source snapshot", cp.Snapshot1, source.ID.String())
	check(cp.Snapshot2 == target.ID.String(), "target snapshot", cp.Snapshot2, target.ID.String())
	check(cp.OriginalTree1 == source.Tree.String(), "source tree", cp.OriginalTree1, source.Tree.String())
	check(cp.OriginalTree2 == target.Tree.String(), "target tree", cp.OriginalTree2, target.Tree.String())
	check(cp.RootTree1 == source.Resolved.String(), "source comparison root", cp.RootTree1, source.Resolved.String())
	check(cp.RootTree2 == target.Resolved.String(), "target comparison root", cp.RootTree2, target.Resolved.String())
	check(cp.Subfolder1 == normalizeSubfolder(source.Subfolder), "source subfolder", cp.Subfolder1, normalizeSubfolder(source.Subfolder))
	check(cp.Subfolder2 == normalizeSubfolder(target.Subfolder), "target subfolder", cp.Subfolder2, normalizeSubfolder(target.Subfolder))

	wantMeta := "false"
	if opts.ShowMetadata {
		wantMeta = "true"
	}
	gotMeta := "false"
	if cp.ShowMetadata {
		gotMeta = "true"
	}
	check(cp.ShowMetadata == opts.ShowMetadata, "metadata option", gotMeta, wantMeta)

	if len(mismatch) > 0 {
		return errors.Errorf("diff checkpoint does not match comparison: %v", mismatch)
	}
	return nil
}

// RecordFailure adds a failure boundary to the checkpoint.
func (cp *Checkpoint) RecordFailure(b FailureBoundary) {
	cp.Failures = append(cp.Failures, b)
}

// Complete reports whether the comparison has reached its final boundary.
func (cp *Checkpoint) Complete() bool {
	return cp.Status == StatusComplete
}

// FramePosition is an entry of the suspended comparison stack, intended for
// progress reporting. Root is first, the currently processed directory last.
type FramePosition struct {
	Kind    FrameKind
	Prefix  string
	After   string
	Pending string
}

// Frames returns the suspended comparison stack. Completed subtrees are not
// part of it.
func (cp *Checkpoint) Frames() []FramePosition {
	out := make([]FramePosition, 0, len(cp.Stack))
	for _, f := range cp.Stack {
		out = append(out, FramePosition{Kind: f.Kind, Prefix: f.Prefix, After: f.After, Pending: f.Pending})
	}
	return out
}

// ContinuationPosition returns the current continuation position as a
// human-readable path, intended for status messages. The empty string means
// the comparison is complete.
func (cp *Checkpoint) ContinuationPosition() string {
	if len(cp.Stack) == 0 {
		return ""
	}
	top := cp.Stack[len(cp.Stack)-1]
	name := top.After
	if top.Pending != "" {
		name = top.Pending
	}
	if name == "" {
		return top.Prefix
	}
	return path.Join(top.Prefix, name)
}

func refs(set restic.AssociatedBlobSet) []blobRef {
	if set == nil || set.Len() == 0 {
		return nil
	}
	out := make([]blobRef, 0, set.Len())
	for h := range set.Keys() {
		out = append(out, blobRef{Type: h.Type, ID: h.ID})
	}
	slices.SortFunc(out, func(a, b blobRef) int {
		if a.Type != b.Type {
			if a.Type < b.Type {
				return -1
			}
			return 1
		}
		return bytes.Compare(a.ID[:], b.ID[:])
	})
	return out
}

// storeStats copies the live statistics (including the deduplicated blob
// sets) into the serializable checkpoint state.
func (cp *Checkpoint) storeStats(s *Stats) {
	cp.Stats.ChangedFiles = s.ChangedFiles
	cp.Stats.Added = s.Added
	cp.Stats.Removed = s.Removed
	cp.Stats.BlobsBefore = refs(s.Before)
	cp.Stats.BlobsAfter = refs(s.After)
	cp.Stats.BlobsCommon = refs(s.Common)
}

// SegmentResult is the result of one successful comparison segment.
type SegmentResult struct {
	// Checkpoint is the continuation credential to persist after the segment.
	// Changes emitted during the segment must be stored together with it.
	Checkpoint *Checkpoint
	// Status is Running, Canceled or Complete.
	Status Status
	// Entries is the number of directory entries completed in this segment.
	Entries int64
	// Changes is the number of changes emitted during this segment.
	Changes int
}

// RunSegment performs at most limit directory entries of comparison work.
// The method stops at a safe boundary when the budget is exhausted or the
// context is canceled and returns the continuation checkpoint. Replaying a
// segment from the same checkpoint deterministically produces the same
// changes, so callers can atomically persist both.
//
// A continued comparison only revisits incomplete frames; finished subtrees
// have been removed from the stack and are not compared again.
func (c *Comparer) RunSegment(ctx context.Context, cp *Checkpoint, limit int64, onChange ChangeReporter, onError ErrorReporter) (*SegmentResult, error) {
	if limit <= 0 {
		return nil, errors.Errorf("segment limit must be greater than zero")
	}
	if cp == nil || cp.Version != checkpointVersion {
		return nil, errors.Errorf("invalid diff checkpoint")
	}
	if cp.Status == StatusComplete {
		return nil, errors.Errorf("diff comparison is already complete")
	}

	stats := c.newStats(cp)
	r := newRunner(c, cp, stats, onChange, onError)

	entriesBefore := cp.EntriesDone
	runErr := r.run(ctx, limit)
	r.syncStack()

	status := StatusRunning
	switch {
	case len(cp.Stack) == 0 && runErr == nil:
		status = StatusComplete
	case errors.Is(runErr, context.Canceled):
		status = StatusCanceled
	case errors.Is(runErr, context.DeadlineExceeded):
		status = StatusCanceled
	case runErr != nil:
		cp.storeStats(stats)
		cp.Status = StatusRunning
		return nil, runErr
	}

	if status == StatusComplete {
		c.Finalize(stats, onError)
	}
	cp.storeStats(stats)
	cp.Status = status
	cp.Segments++

	return &SegmentResult{
		Checkpoint: cp,
		Status:     status,
		Entries:    cp.EntriesDone - entriesBefore,
		Changes:    r.changes,
	}, nil
}
