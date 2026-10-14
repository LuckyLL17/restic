// Package differ implements the comparison of two snapshot trees.
//
// The comparison is driven by an explicit, serializable frame stack (see
// resume.go). The same engine powers the one-shot comparison used by the
// "diff" command and the segmented, resumable comparison used by other
// in-repository callers such as audit tasks. Suspending the engine is only
// possible at completed directory-entry boundaries, so every emitted change
// and every accumulated statistic belongs to an indivisible unit of work and
// can never be produced twice when a segment is continued.
package differ

import (
	"context"
	"iter"
	"path"
	"reflect"

	"github.com/restic/restic/internal/data"
	"github.com/restic/restic/internal/debug"
	"github.com/restic/restic/internal/restic"
)

// Options controls how two snapshots are compared.
type Options struct {
	ShowMetadata bool
}

// Change reports a single changed item.
type Change struct {
	MessageType string `json:"message_type"` // always "change"
	Path        string `json:"path"`
	Modifier    string `json:"modifier"`
}

// NewChange returns a Change for path with the given modifier.
func NewChange(path string, mode string) *Change {
	return &Change{MessageType: "change", Path: path, Modifier: mode}
}

// Stat collects statistics for one side of the comparison (added or removed
// items).
type Stat struct {
	Files     int    `json:"files"`
	Dirs      int    `json:"dirs"`
	Others    int    `json:"others"`
	DataBlobs int    `json:"data_blobs"`
	TreeBlobs int    `json:"tree_blobs"`
	Bytes     uint64 `json:"bytes"`
}

// Add adds the node type information to s.
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

// Stats contains the accumulated result of a comparison.
type Stats struct {
	MessageType    string `json:"message_type"` // always "statistics"
	SourceSnapshot string `json:"source_snapshot"`
	TargetSnapshot string `json:"target_snapshot"`
	ChangedFiles   int    `json:"changed_files"`
	Added          Stat   `json:"added"`
	Removed        Stat   `json:"removed"`

	Before restic.AssociatedBlobSet `json:"-"`
	After  restic.AssociatedBlobSet `json:"-"`
	Common restic.AssociatedBlobSet `json:"-"`
}

// ChangeReporter is called once for every detected change.
type ChangeReporter func(*Change)

// ErrorReporter is called for non-fatal errors encountered in subtrees.
type ErrorReporter func(string, ...any)

// Repository is the subset of restic.Repository required for comparisons.
type Repository interface {
	restic.BlobLoader
	LookupBlobSize(bh restic.BlobHandle) (uint, bool)
	NewAssociatedBlobSet() restic.AssociatedBlobSet
}

// Comparer compares two snapshots. A Comparer is safe to reuse across several
// comparisons, but each comparison (one-shot or segmented) must use its own
// Checkpoint and Stats.
type Comparer struct {
	repo Repository
	opts Options
}

// New returns a new Comparer.
func New(repo Repository, opts Options) *Comparer {
	return &Comparer{repo: repo, opts: opts}
}

// addBlobs records the blobs referenced by node in bs.
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
		bs.Insert(restic.BlobHandle{ID: *node.Subtree, Type: restic.TreeBlob})
	}
}

// updateBlobs resolves the blob sizes and updates the counters in stats.
func updateBlobs(repo Repository, blobs restic.AssociatedBlobSet, stats *Stat, printError ErrorReporter) {
	for h := range blobs.Keys() {
		switch h.Type {
		case restic.DataBlob:
			stats.DataBlobs++
		case restic.TreeBlob:
			stats.TreeBlobs++
		}

		size, found := repo.LookupBlobSize(h)
		if !found {
			printError("unable to find blob size for %v", h)
			continue
		}

		stats.Bytes += uint64(size)
	}
}

// newStats creates the live statistics tracked while processing cp.
func (c *Comparer) newStats(cp *Checkpoint) *Stats {
	s := &Stats{
		MessageType:    "statistics",
		SourceSnapshot: cp.Snapshot1,
		TargetSnapshot: cp.Snapshot2,
		ChangedFiles:   cp.Stats.ChangedFiles,
		Added:          cp.Stats.Added,
		Removed:        cp.Stats.Removed,
		Before:         c.repo.NewAssociatedBlobSet(),
		After:          c.repo.NewAssociatedBlobSet(),
		Common:         c.repo.NewAssociatedBlobSet(),
	}
	for _, h := range cp.Stats.BlobsBefore {
		s.Before.Insert(restic.BlobHandle{ID: h.ID, Type: h.Type})
	}
	for _, h := range cp.Stats.BlobsAfter {
		s.After.Insert(restic.BlobHandle{ID: h.ID, Type: h.Type})
	}
	for _, h := range cp.Stats.BlobsCommon {
		s.Common.Insert(restic.BlobHandle{ID: h.ID, Type: h.Type})
	}
	return s
}

// Compare runs the complete comparison of the two snapshot (sub)trees in a
// single pass. The returned Stats are not finalized; callers that need the
// blob statistics must invoke Finalize.
func (c *Comparer) Compare(ctx context.Context, root1, root2 restic.ID, onChange ChangeReporter, onError ErrorReporter) (*Stats, error) {
	cp := newTransientCheckpoint(root1, root2, c.opts)
	stats := c.newStats(cp)

	r := newRunner(c, cp, stats, onChange, onError)
	err := r.run(ctx, 0)
	r.syncStack()
	if err != nil {
		return nil, err
	}
	return stats, nil
}

// Finalize computes the added/removed blob counts and sizes from the
// accumulated blob sets. It must be called once after the comparison has
// reached its completion boundary.
func (c *Comparer) Finalize(stats *Stats, onError ErrorReporter) {
	both := stats.Before.Intersect(stats.After)
	updateBlobs(c.repo, stats.Before.Sub(both).Sub(stats.Common), &stats.Removed, onError)
	updateBlobs(c.repo, stats.After.Sub(both).Sub(stats.Common), &stats.Added, onError)
}

// After names the last entry whose own change and entire subtree are fully
// processed. Pending names an entry whose own change is already emitted but
// whose subtree is still represented by frames above it.
type frame struct {
	Kind    FrameKind `json:"kind"`
	Tree1   string    `json:"tree1,omitempty"` // diff/common/added/removed
	Tree2   string    `json:"tree2,omitempty"` // diff only
	Prefix  string    `json:"prefix"`
	After   string    `json:"after,omitempty"`
	Pending string    `json:"pending,omitempty"`
}

// openFrame extends frame with the live, single-use tree iterators. Iterators
// are opened lazily when the frame becomes the top of the stack and are
// rebuilt from After/Pending when a comparison is resumed in another process.
type openFrame struct {
	*frame
	root bool

	// single-tree frames
	nextSingle func() (data.NodeOrError, bool)
	stopSingle func()
	peekSingle *data.Node
	errSingle  error

	// diff frames
	next1, next2 func() (data.NodeOrError, bool)
	stop1, stop2 func()
	peek1, peek2 *data.Node
	err1, err2   error
}

// runner drives the explicit frame stack. Frames are heap allocated so that
// pushing frames cannot invalidate the pointers of frames already on the
// stack; the checkpoint copy is refreshed through syncStack.
type runner struct {
	c        *Comparer
	cp       *Checkpoint
	stats    *Stats
	onChange ChangeReporter
	onError  ErrorReporter
	changes  int

	open []*openFrame
}

func newRunner(c *Comparer, cp *Checkpoint, stats *Stats, onChange ChangeReporter, onError ErrorReporter) *runner {
	r := &runner{
		c:        c,
		cp:       cp,
		stats:    stats,
		onChange: onChange,
		onError:  onError,
	}
	r.onChange = func(change *Change) {
		r.changes++
		onChange(change)
	}
	for i := range cp.Stack {
		f := cp.Stack[i]
		r.open = append(r.open, &openFrame{frame: &f, root: i == 0})
	}
	return r
}

func (r *runner) top() *openFrame { return r.open[len(r.open)-1] }

func (r *runner) push(f frame) {
	frame := f
	r.open = append(r.open, &openFrame{frame: &frame})
}

// pop removes the top frame. Its iterators have already been consumed or were
// never opened.
func (r *runner) pop() {
	f := r.top()
	f.closeIterators()
	r.open = r.open[:len(r.open)-1]
}

// syncStack writes the live frame positions back into the checkpoint so that
// it can be serialized and continued.
func (r *runner) syncStack() {
	cp := r.cp
	cp.Stack = cp.Stack[:0]
	for _, f := range r.open {
		cp.Stack = append(cp.Stack, *f.frame)
		f.closeIterators()
	}
}

func (f *openFrame) closeIterators() {
	if f.stopSingle != nil {
		f.stopSingle()
		f.stopSingle = nil
	}
	if f.stop1 != nil {
		f.stop1()
		f.stop1 = nil
	}
	if f.stop2 != nil {
		f.stop2()
		f.stop2 = nil
	}
}

// pull advances a single pull-style tree iterator.
func pull(next func() (data.NodeOrError, bool)) (*data.Node, error) {
	item, ok := next()
	if !ok {
		return nil, nil
	}
	return item.Node, item.Error
}

// openIterators loads the tree blob(s) for f and skips all entries that are
// already complete (name <= After) as well as a claimed Pending entry whose
// subtree is still on the stack above f.
func (r *runner) openIterators(ctx context.Context, f *openFrame) error {
	opened := false
	defer func() {
		if !opened {
			f.closeIterators()
		}
	}()

	switch f.Kind {
	case FrameDiff:
		id1, err := restic.ParseID(f.Tree1)
		if err != nil {
			return err
		}
		id2, err := restic.ParseID(f.Tree2)
		if err != nil {
			return err
		}
		seq1, err := data.LoadTree(ctx, r.c.repo, id1)
		if err != nil {
			return err
		}
		seq2, err := data.LoadTree(ctx, r.c.repo, id2)
		if err != nil {
			return err
		}
		f.next1, f.stop1 = iter.Pull(seq1)
		f.next2, f.stop2 = iter.Pull(seq2)
		if f.peek1, f.err1 = pull(f.next1); f.err1 != nil {
			return f.err1
		}
		if f.peek2, f.err2 = pull(f.next2); f.err2 != nil {
			return f.err2
		}
		for f.peek1 != nil && f.peek1.Name <= f.After {
			if f.peek1, f.err1 = pull(f.next1); f.err1 != nil {
				return f.err1
			}
		}
		for f.peek2 != nil && f.peek2.Name <= f.After {
			if f.peek2, f.err2 = pull(f.next2); f.err2 != nil {
				return f.err2
			}
		}
		// the pending entry was already claimed; reskip it on each side
		if f.Pending != "" {
			if f.peek1 != nil && f.peek1.Name == f.Pending {
				if f.peek1, f.err1 = pull(f.next1); f.err1 != nil {
					return f.err1
				}
			}
			if f.peek2 != nil && f.peek2.Name == f.Pending {
				if f.peek2, f.err2 = pull(f.next2); f.err2 != nil {
					return f.err2
				}
			}
		}
	default:
		id, err := restic.ParseID(f.Tree1)
		if err != nil {
			return err
		}
		seq, err := data.LoadTree(ctx, r.c.repo, id)
		if err != nil {
			return err
		}
		f.nextSingle, f.stopSingle = iter.Pull(seq)
		if f.peekSingle, f.errSingle = pull(f.nextSingle); f.errSingle != nil {
			return f.errSingle
		}
		for f.peekSingle != nil && f.peekSingle.Name <= f.After {
			if f.peekSingle, f.errSingle = pull(f.nextSingle); f.errSingle != nil {
				return f.errSingle
			}
		}
		if f.Pending != "" && f.peekSingle != nil && f.peekSingle.Name == f.Pending {
			if f.peekSingle, f.errSingle = pull(f.nextSingle); f.errSingle != nil {
				return f.errSingle
			}
		}
	}
	opened = true
	debug.Log("opened %v frame prefix %q after %q pending %q", f.Kind, f.Prefix, f.After, f.Pending)
	return nil
}

// run processes work until the stack is empty, the entry budget is exhausted
// or ctx is canceled. A limit of 0 means unlimited. Returning
// context.Canceled means that all work performed so far is captured by the
// checkpoint and may be resumed.
func (r *runner) run(ctx context.Context, limit int64) error {
	for len(r.open) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}

		f := r.top()
		if f.next1 == nil && f.nextSingle == nil {
			if err := r.openIterators(ctx, f); err != nil {
				if err := r.handleFrameError(ctx, f, err); err != nil {
					return err
				}
				continue
			}
		}

		if f.Pending != "" {
			// the subtree of the claimed pending entry has completed
			f.After = f.Pending
			f.Pending = ""
			r.cp.EntriesDone++
			if limit > 0 {
				limit--
				if limit == 0 {
					return nil
				}
			}
			continue
		}

		if err := ctx.Err(); err != nil {
			return err
		}

		var stepErr error
		if f.Kind == FrameDiff {
			stepErr = r.stepDiff(f)
		} else {
			stepErr = r.stepSingle(f)
		}
		if stepErr != nil {
			if err := r.handleFrameError(ctx, f, stepErr); err != nil {
				return err
			}
			continue
		}

		switch {
		case len(r.open) == 0:
			// root frame finished
		case r.top() != f:
			// f finished or a child frame was pushed; no entry was claimed here
		case f.Pending != "":
			// child frame pushed, waiting for its subtree to complete
		default:
			// entry claimed directly at f
			r.cp.EntriesDone++
			if limit > 0 {
				limit--
				if limit == 0 {
					return nil
				}
			}
		}
	}
	return nil
}

// handleFrameError records the failure boundary of f and abandons the frame.
// Errors while loading or iterating the root frame are fatal, matching the
// behavior of the diff command; errors in nested frames are reported and
// leave the owning entry completed with its subtree unvisited.
func (r *runner) handleFrameError(ctx context.Context, f *openFrame, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}

	boundary := FailureBoundary{
		Kind:   f.Kind,
		Prefix: f.Prefix,
		Entry:  f.Pending,
		After:  f.After,
		Tree1:  f.Tree1,
		Tree2:  f.Tree2,
		Error:  err.Error(),
	}
	if f.root {
		// fatal: keep the frame and its safe position in the checkpoint so
		// that callers can judge and retry the failed boundary
		r.cp.RecordFailure(boundary)
		f.closeIterators()
		return err
	}

	// abandon the failed subtree; the owning parent entry stays completed
	r.cp.RecordFailure(boundary)
	r.onError("error: %v", err)
	r.pop()
	return nil
}

// stepSingle processes the next entry of a common/added/removed frame. A nil
// entry means the frame is exhausted and gets popped.
func (r *runner) stepSingle(f *openFrame) error {
	if f.errSingle != nil {
		return f.errSingle
	}
	node := f.peekSingle
	// advance before processing; the entry is claimed by setting After/Pending
	f.peekSingle, f.errSingle = pull(f.nextSingle)
	if node == nil {
		r.pop()
		return nil
	}

	switch f.Kind {
	case FrameCommon:
		addBlobs(r.stats.Common, node)
		if node.Type == data.NodeTypeDir {
			r.push(frame{Kind: FrameCommon, Tree1: node.Subtree.String(), Prefix: path.Join(f.Prefix, node.Name) + "/"})
			f.Pending = node.Name
		} else {
			f.After = node.Name
		}
	case FrameAdded, FrameRemoved:
		name := path.Join(f.Prefix, node.Name)
		mode := "+"
		side := &r.stats.Added
		blobs := r.stats.After
		if f.Kind == FrameRemoved {
			mode = "-"
			side = &r.stats.Removed
			blobs = r.stats.Before
		}
		if node.Type == data.NodeTypeDir {
			name += "/"
		}
		r.onChange(NewChange(name, mode))
		side.Add(node)
		addBlobs(blobs, node)

		if node.Type == data.NodeTypeDir {
			r.push(frame{Kind: f.Kind, Tree1: node.Subtree.String(), Prefix: name})
			f.Pending = node.Name
		} else {
			f.After = node.Name
		}
	}
	return nil
}

// nextPair returns the next merged entry of a diff frame, advancing each side
// past the entry it contributes. nil nodes mean that the item is absent on
// that side.
func (f *openFrame) nextPair() (node1, node2 *data.Node, err error) {
	if f.err1 != nil {
		return nil, nil, f.err1
	}
	if f.err2 != nil {
		return nil, nil, f.err2
	}
	if f.peek1 == nil && f.peek2 == nil {
		return nil, nil, nil
	}

	node1, node2 = f.peek1, f.peek2
	if node1 != nil && node2 != nil {
		if node1.Name < node2.Name {
			node2 = nil
		} else if node1.Name > node2.Name {
			node1 = nil
		}
	}

	if node1 != nil {
		f.peek1, f.err1 = pull(f.next1)
	}
	if node2 != nil {
		f.peek2, f.err2 = pull(f.next2)
	}
	return node1, node2, nil
}

// stepDiff processes the next merged entry of a diff frame. When both trees
// are exhausted, the frame gets popped.
func (r *runner) stepDiff(f *openFrame) error {
	node1, node2, err := f.nextPair()
	if err != nil {
		return err
	}
	if node1 == nil && node2 == nil {
		r.pop()
		return nil
	}

	addBlobs(r.stats.Before, node1)
	addBlobs(r.stats.After, node2)

	switch {
	case node1 != nil && node2 != nil:
		entryName := node1.Name
		name := path.Join(f.Prefix, entryName)
		mod := ""

		if node1.Type != node2.Type {
			mod += "T"
		}

		if node2.Type == data.NodeTypeDir {
			name += "/"
		}

		if node1.Type == data.NodeTypeFile &&
			node2.Type == data.NodeTypeFile &&
			!reflect.DeepEqual(node1.Content, node2.Content) {
			mod += "M"
			r.stats.ChangedFiles++

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
			r.onChange(NewChange(name, mod))
		}

		if node1.Type == data.NodeTypeDir && node2.Type == data.NodeTypeDir {
			if (*node1.Subtree).Equal(*node2.Subtree) {
				r.push(frame{Kind: FrameCommon, Tree1: node1.Subtree.String(), Prefix: name})
			} else {
				r.push(frame{Kind: FrameDiff, Tree1: node1.Subtree.String(), Tree2: node2.Subtree.String(), Prefix: name})
			}
			f.Pending = entryName
		} else {
			f.After = entryName
		}
	case node1 != nil && node2 == nil:
		name := path.Join(f.Prefix, node1.Name)
		if node1.Type == data.NodeTypeDir {
			name += "/"
		}
		r.onChange(NewChange(name, "-"))
		r.stats.Removed.Add(node1)

		if node1.Type == data.NodeTypeDir {
			r.push(frame{Kind: FrameRemoved, Tree1: node1.Subtree.String(), Prefix: name})
			f.Pending = node1.Name
		} else {
			f.After = node1.Name
		}
	case node1 == nil && node2 != nil:
		name := path.Join(f.Prefix, node2.Name)
		if node2.Type == data.NodeTypeDir {
			name += "/"
		}
		r.onChange(NewChange(name, "+"))
		r.stats.Added.Add(node2)

		if node2.Type == data.NodeTypeDir {
			r.push(frame{Kind: FrameAdded, Tree1: node2.Subtree.String(), Prefix: name})
			f.Pending = node2.Name
		} else {
			f.After = node2.Name
		}
	}
	return nil
}
