// Package inspection implements a persistent pack-coverage plan for the
// "restic check --read-data" verification.
//
// The plan divides the packs of a repository into stable fragments and tracks
// each fragment through a pending/claimed/failed/done state machine. This
// allows a verification to claim, complete and retry individual fragments, to
// resume at the first unfinished fragment after an interruption, and to rerun
// only the fragments that failed in a previous invocation.
//
// A plan is bound to a fingerprint of the repository state it was created
// from. When packs are added or removed, or the index changes, the fingerprint
// changes and the stored plan is detected as outdated. It must then be
// rebuilt from scratch instead of trusting partial coverage of the old state.
package inspection

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/restic/restic/internal/errors"
	"github.com/restic/restic/internal/restic"
)

const (
	// Version is the on-disk format version of a plan.
	Version = 1

	// DefaultPacksPerFragment is the default target number of packs per
	// fragment. Smaller fragments mean finer-grained progress and retries.
	DefaultPacksPerFragment = 25

	// DefaultLeaseDuration is the time after which a claimed fragment is
	// considered abandoned even if it was claimed by the current run.
	DefaultLeaseDuration = 24 * time.Hour

	// maxErrorLength bounds the error message recorded for a failed fragment.
	maxErrorLength = 1000
)

// FragmentStatus is the execution state of a fragment.
type FragmentStatus string

const (
	// StatusPending means the fragment has not been checked yet.
	StatusPending FragmentStatus = "pending"
	// StatusClaimed means the fragment is currently being checked.
	StatusClaimed FragmentStatus = "claimed"
	// StatusFailed means the last attempt failed; it is retried in a new run.
	StatusFailed FragmentStatus = "failed"
	// StatusDone means all packs of the fragment were verified successfully.
	StatusDone FragmentStatus = "done"
)

// Fragment is a stable, ordered subset of the repository's packs.
type Fragment struct {
	Index       int            `json:"index"`
	Packs       restic.IDs     `json:"packs"`
	Status      FragmentStatus `json:"status"`
	Attempts    int            `json:"attempts"`
	RunID       string         `json:"run_id,omitempty"`
	ClaimedAt   *time.Time     `json:"claimed_at,omitempty"`
	CompletedAt *time.Time     `json:"completed_at,omitempty"`
	LastError   string         `json:"last_error,omitempty"`
}

// Plan is a persistent pack-coverage plan.
type Plan struct {
	Version      int         `json:"version"`
	Fingerprint  string      `json:"fingerprint"`
	PackCount    int         `json:"pack_count"`
	TotalSize    int64       `json:"total_size"`
	FragmentSize int         `json:"fragment_size"`
	CreatedAt    time.Time   `json:"created_at"`
	UpdatedAt    time.Time   `json:"updated_at"`
	Fragments    []*Fragment `json:"fragments"`

	// path is the file the plan is stored in. It is not serialized.
	path string
	// runID uniquely identifies the current process that opened the plan.
	// Fragments claimed by a different runID are abandoned and reclaimed.
	runID string
}

// Fingerprint computes a stable hash of the repository state a plan covers.
// It incorporates the repository config ID, the set of index files, the packs
// covered by the plan (which depends on snapshot filtering), the packs (and
// sizes) known to the index and the packs physically stored in the backend.
// Any added or removed pack, size change, rewritten index or changed coverage
// filter changes the fingerprint, which invalidates plans derived from
// another state.
func Fingerprint(configID string, indexIDs restic.IDs, coveredPacks, indexedPacks, repositoryPacks map[restic.ID]int64) string {
	h := sha256.New()
	fmt.Fprintf(h, "config %s\n", configID)

	ids := append(restic.IDs(nil), indexIDs...)
	sort.Sort(ids)
	for _, id := range ids {
		fmt.Fprintf(h, "index %s\n", id)
	}

	fingerprintPacks(h, "covered", coveredPacks)
	fingerprintPacks(h, "indexed", indexedPacks)
	fingerprintPacks(h, "stored", repositoryPacks)

	return hex.EncodeToString(h.Sum(nil))
}

func fingerprintPacks(h io.Writer, section string, packs map[restic.ID]int64) {
	ids := make(restic.IDs, 0, len(packs))
	for id := range packs {
		ids = append(ids, id)
	}
	sort.Sort(ids)
	for _, id := range ids {
		fmt.Fprintf(h, "%s %s %d\n", section, id, packs[id])
	}
}

// New creates a new plan for the given fingerprint by dividing the provided
// packs into stable, sorted fragments of at most fragmentSize packs.
func New(fingerprint string, packs map[restic.ID]int64, fragmentSize int, now time.Time) *Plan {
	if fragmentSize < 1 {
		fragmentSize = DefaultPacksPerFragment
	}

	ids := make(restic.IDs, 0, len(packs))
	var totalSize int64
	for id, size := range packs {
		ids = append(ids, id)
		totalSize += size
	}
	sort.Sort(ids)

	fragmentCount := 0
	if len(ids) > 0 {
		fragmentCount = (len(ids) + fragmentSize - 1) / fragmentSize
	}
	fragments := make([]*Fragment, 0, fragmentCount)
	for start := 0; start < len(ids); start += fragmentSize {
		end := min(start+fragmentSize, len(ids))
		fragments = append(fragments, &Fragment{
			Index:  len(fragments),
			Packs:  append(restic.IDs(nil), ids[start:end]...),
			Status: StatusPending,
		})
	}

	now = now.UTC()
	return &Plan{
		Version:      Version,
		Fingerprint:  fingerprint,
		PackCount:    len(ids),
		TotalSize:    totalSize,
		FragmentSize: fragmentSize,
		CreatedAt:    now,
		UpdatedAt:    now,
		Fragments:    fragments,
		runID:        newRunID(),
	}
}

// Open reads and validates a plan from the given path. If the file does not
// exist, an error wrapping os.ErrNotExist is returned. A new run ID is
// assigned, so fragments left claimed by a crashed process are reclaimed.
func Open(path string) (*Plan, error) {
	buf, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.Wrap(err, "load inspection plan")
	}

	p := &Plan{}
	if err := json.Unmarshal(buf, p); err != nil {
		return nil, errors.Errorf("inspection plan %v is corrupt: %v", path, err)
	}

	if p.Version != Version {
		return nil, errors.Errorf("inspection plan %v has unsupported format version %d", path, p.Version)
	}
	if err := p.validate(); err != nil {
		return nil, errors.Errorf("inspection plan %v is corrupt: %v", path, err)
	}

	p.path = path
	p.runID = newRunID()
	return p, nil
}

func (p *Plan) validate() error {
	if len(p.Fragments) == 0 && p.PackCount != 0 {
		return errors.New("plan has no fragments")
	}
	seen := 0
	for idx, f := range p.Fragments {
		if f == nil {
			return errors.Errorf("fragment %d is empty", idx)
		}
		if f.Index != idx {
			return errors.Errorf("fragment %d has unexpected index %d", idx, f.Index)
		}
		if len(f.Packs) == 0 {
			return errors.Errorf("fragment %d has no packs", idx)
		}
		switch f.Status {
		case StatusPending, StatusClaimed, StatusFailed, StatusDone:
		default:
			return errors.Errorf("fragment %d has invalid status %q", idx, f.Status)
		}
		seen += len(f.Packs)
	}
	if seen != p.PackCount {
		return errors.Errorf("plan covers %d packs but declares %d", seen, p.PackCount)
	}
	return nil
}

// Path returns the file path of the plan, or an empty string for an unsaved plan.
func (p *Plan) Path() string {
	return p.path
}

// SetPath assigns the file path used by Save.
func (p *Plan) SetPath(path string) {
	p.path = path
}

// Matches reports whether the plan was created for the given repository state.
func (p *Plan) Matches(fingerprint string) bool {
	return p.Fingerprint == fingerprint
}

// Counts returns the number of done, failed, pending and claimed fragments.
func (p *Plan) Counts() (done, failed, pending, claimed int) {
	for _, f := range p.Fragments {
		switch f.Status {
		case StatusDone:
			done++
		case StatusFailed:
			failed++
		case StatusPending:
			pending++
		case StatusClaimed:
			claimed++
		}
	}
	return done, failed, pending, claimed
}

// claimable reports whether the fragment can be checked in the current run.
// A failed fragment is only retried in a new run; a claimed fragment is only
// reclaimed if it was claimed by a previous (crashed) run or its lease
// expired.
func (p *Plan) claimable(f *Fragment, now time.Time, lease time.Duration) bool {
	switch f.Status {
	case StatusPending:
		return true
	case StatusFailed:
		return f.RunID != p.runID
	case StatusClaimed:
		if f.RunID != p.runID {
			return true
		}
		return f.ClaimedAt == nil || now.Sub(*f.ClaimedAt) >= lease
	default:
		return false
	}
}

// Claim marks the next claimable fragment as being checked by the current run
// and returns it. It returns nil if no fragment can currently be claimed.
// Pending fragments are claimed first in index order.
func (p *Plan) Claim(now time.Time, lease time.Duration) *Fragment {
	now = now.UTC()
	var retry *Fragment
	for _, f := range p.Fragments {
		if !p.claimable(f, now, lease) {
			continue
		}
		if f.Status == StatusPending || f.Status == StatusClaimed {
			p.markClaimed(f, now)
			return f
		}
		if retry == nil {
			retry = f
		}
	}
	if retry != nil {
		p.markClaimed(retry, now)
		return retry
	}
	return nil
}

func (p *Plan) markClaimed(f *Fragment, now time.Time) {
	f.Status = StatusClaimed
	f.Attempts++
	f.RunID = p.runID
	f.ClaimedAt = &now
	f.CompletedAt = nil
	f.LastError = ""
	p.UpdatedAt = now
}

// Succeed records that the claimed fragment was checked without errors.
func (p *Plan) Succeed(f *Fragment, now time.Time) error {
	if f.Status != StatusClaimed || f.RunID != p.runID {
		return errors.Errorf("fragment %d is not claimed by the current run", f.Index)
	}
	now = now.UTC()
	f.Status = StatusDone
	f.CompletedAt = &now
	f.LastError = ""
	p.UpdatedAt = now
	return nil
}

// Fail records that checking the claimed fragment failed. The fragment is
// retried on the next invocation, not within the current run.
func (p *Plan) Fail(f *Fragment, now time.Time, cause error) error {
	if f.Status != StatusClaimed || f.RunID != p.runID {
		return errors.Errorf("fragment %d is not claimed by the current run", f.Index)
	}
	if cause == nil {
		return errors.Errorf("fragment %d marked as failed without an error", f.Index)
	}
	now = now.UTC()
	msg := cause.Error()
	if len(msg) > maxErrorLength {
		msg = msg[:maxErrorLength]
	}
	f.Status = StatusFailed
	f.LastError = msg
	f.CompletedAt = nil
	p.UpdatedAt = now
	return nil
}

// Save atomically writes the plan to its path. The file is replaced in a
// single rename so that readers never observe a partially written plan.
func (p *Plan) Save() error {
	if p.path == "" {
		return errors.New("cannot save inspection plan without a path")
	}

	buf, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return errors.Wrap(err, "marshal inspection plan")
	}

	dir := filepath.Dir(p.path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return errors.Wrap(err, "create inspection plan directory")
	}

	tmp, err := os.CreateTemp(dir, ".inspection-plan-*.tmp")
	if err != nil {
		return errors.Wrap(err, "create inspection plan file")
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return errors.Wrap(err, "chmod inspection plan file")
	}
	if _, err := tmp.Write(buf); err != nil {
		_ = tmp.Close()
		return errors.Wrap(err, "write inspection plan")
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return errors.Wrap(err, "sync inspection plan")
	}
	if err := tmp.Close(); err != nil {
		return errors.Wrap(err, "close inspection plan")
	}

	if err := os.Rename(tmpName, p.path); err != nil {
		return errors.Wrap(err, "replace inspection plan")
	}
	return nil
}

func newRunID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
