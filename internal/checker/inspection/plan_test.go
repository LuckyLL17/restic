package inspection

import (
	"bytes"
	stderrors "errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/restic/restic/internal/restic"
	"github.com/restic/restic/internal/test"
)

func testPacks(n int) map[restic.ID]int64 {
	packs := make(map[restic.ID]int64, n)
	for i := range n {
		id := restic.NewRandomID()
		// avoid duplicate random IDs in theory
		for {
			if _, ok := packs[id]; !ok {
				break
			}
			id = restic.NewRandomID()
		}
		packs[id] = int64(i+1) * 1024
	}
	return packs
}

func TestNewPlanStableFragments(t *testing.T) {
	packs := testPacks(55)

	now := time.Unix(1700000000, 0)
	p := New("fp", packs, 25, now)

	test.Equals(t, 55, p.PackCount)
	test.Equals(t, 3, len(p.Fragments))
	test.Equals(t, 25, len(p.Fragments[0].Packs))
	test.Equals(t, 25, len(p.Fragments[1].Packs))
	test.Equals(t, 5, len(p.Fragments[2].Packs))
	test.Equals(t, "fp", p.Fingerprint)

	// fragment indices and pack order are stable
	for i, f := range p.Fragments {
		test.Equals(t, i, f.Index)
		test.Equals(t, StatusPending, f.Status)
		for j := 1; j < len(f.Packs); j++ {
			test.Assert(t, bytes.Compare(f.Packs[j-1][:], f.Packs[j][:]) < 0, "packs within fragment are not sorted")
		}
	}
	test.Assert(t, bytes.Compare(p.Fragments[0].Packs[24][:], p.Fragments[1].Packs[0][:]) < 0,
		"fragments are not ordered")

	// the fragments cover every pack exactly once
	seen := restic.NewIDSet()
	for _, f := range p.Fragments {
		for _, id := range f.Packs {
			test.Assert(t, !seen.Has(id), "pack %v covered twice", id)
			seen.Insert(id)
			_, ok := packs[id]
			test.Assert(t, ok, "fragment covers unknown pack %v", id)
		}
	}
	test.Equals(t, len(packs), len(seen))

	// building the plan again with the same packs yields the same fragments
	p2 := New("fp", packs, 25, now)
	for i := range p.Fragments {
		test.Equals(t, p.Fragments[i].Packs, p2.Fragments[i].Packs)
	}

	// an invalid fragment size falls back to the default
	p3 := New("fp", packs, 0, now)
	test.Equals(t, DefaultPacksPerFragment, p3.FragmentSize)
}

func TestNewPlanEmptyRepository(t *testing.T) {
	p := New("fp", nil, DefaultPacksPerFragment, time.Now())
	test.Equals(t, 0, len(p.Fragments))
	test.Equals(t, 0, p.PackCount)
	test.Assert(t, p.Claim(time.Now(), DefaultLeaseDuration) == nil, "empty plan must have no claimable fragment")
}

func TestFingerprintStableAndSensitive(t *testing.T) {
	packs := testPacks(5)
	repoPacks := make(map[restic.ID]int64, len(packs))
	for id, size := range packs {
		repoPacks[id] = size
	}
	indexIDs := restic.IDs{restic.NewRandomID(), restic.NewRandomID()}

	base := Fingerprint("config-id", indexIDs, packs, packs, repoPacks)
	test.Equals(t, base, Fingerprint("config-id", indexIDs, packs, packs, repoPacks))

	// the order of inputs must not matter
	test.Equals(t, base, Fingerprint("config-id", restic.IDs{indexIDs[1], indexIDs[0]}, packs, packs, repoPacks))

	// any change to the repository state changes the fingerprint
	test.Assert(t, Fingerprint("other-config", indexIDs, packs, packs, repoPacks) != base,
		"fingerprint must depend on the config ID")

	test.Assert(t, Fingerprint("config-id", restic.IDs{indexIDs[0]}, packs, packs, repoPacks) != base,
		"fingerprint must depend on the index IDs")

	changedIndexPacks := make(map[restic.ID]int64, len(packs))
	for id, size := range packs {
		changedIndexPacks[id] = size
	}
	var someID restic.ID
	for id := range changedIndexPacks {
		changedIndexPacks[id]++
		someID = id
		break
	}
	test.Assert(t, Fingerprint("config-id", indexIDs, changedIndexPacks, changedIndexPacks, repoPacks) != base,
		"fingerprint must depend on indexed pack sizes")

	addedPack := make(map[restic.ID]int64, len(packs)+1)
	for id, size := range packs {
		addedPack[id] = size
	}
	addedPack[restic.NewRandomID()] = 42
	test.Assert(t, Fingerprint("config-id", indexIDs, addedPack, addedPack, repoPacks) != base,
		"fingerprint must detect added packs")

	// a physically removed pack changes the fingerprint even if the index still references it
	removedStored := make(map[restic.ID]int64, len(repoPacks)-1)
	for id, size := range repoPacks {
		if id != someID {
			removedStored[id] = size
		}
	}
	test.Assert(t, Fingerprint("config-id", indexIDs, packs, packs, removedStored) != base,
		"fingerprint must detect removed packs")

	// changing only the coverage (snapshot filter) invalidates the plan too
	partialCoverage := make(map[restic.ID]int64, len(packs)-1)
	for id, size := range packs {
		if id != someID {
			partialCoverage[id] = size
		}
	}
	test.Assert(t, Fingerprint("config-id", indexIDs, partialCoverage, packs, repoPacks) != base,
		"fingerprint must depend on the covered pack set")
}

func TestClaimSucceedFailLifecycle(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	p := New("fp", testPacks(3), 1, now)

	f0 := p.Claim(now, DefaultLeaseDuration)
	test.Assert(t, f0 != nil && f0.Index == 0, "expected fragment 0")
	test.Equals(t, StatusClaimed, f0.Status)
	test.Equals(t, 1, f0.Attempts)
	test.Equals(t, p.runID, f0.RunID)
	test.OK(t, p.Succeed(f0, now.Add(time.Minute)))
	test.Equals(t, StatusDone, f0.Status)
	test.Assert(t, f0.CompletedAt != nil, "completed fragment must have a completion time")

	f1 := p.Claim(now.Add(2*time.Minute), DefaultLeaseDuration)
	test.Assert(t, f1 != nil && f1.Index == 1, "expected fragment 1")
	test.OK(t, p.Fail(f1, now.Add(3*time.Minute), os.ErrInvalid))
	test.Equals(t, StatusFailed, f1.Status)
	test.Equals(t, "invalid argument", f1.LastError)

	// failed fragments are not retried within the same run; pending work continues
	f2 := p.Claim(now.Add(4*time.Minute), DefaultLeaseDuration)
	test.Assert(t, f2 != nil && f2.Index == 2, "expected fragment 2, not a retry of fragment 1")
	test.OK(t, p.Succeed(f2, now.Add(5*time.Minute)))

	test.Assert(t, p.Claim(now.Add(6*time.Minute), DefaultLeaseDuration) == nil,
		"no more claimable fragments expected in this run")

	done, failed, pending, claimed := p.Counts()
	test.Equals(t, 2, done)
	test.Equals(t, 1, failed)
	test.Equals(t, 0, pending)
	test.Equals(t, 0, claimed)

	// state transitions outside the claimed state are rejected
	test.Assert(t, p.Succeed(f1, now) != nil, "succeeding a failed fragment must fail")
	test.Assert(t, p.Fail(f1, now, os.ErrInvalid) != nil, "failing a failed fragment must fail")
	test.Assert(t, p.Fail(f2, now, nil) != nil, "failing without an error must fail")
}

func TestResumeAcrossRuns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "plan.json")
	now := time.Unix(1700000000, 0).UTC()

	// first run: claim fragment 0, then crash without completing it
	run1 := New("fp", testPacks(3), 1, now)
	run1.SetPath(path)
	f0 := run1.Claim(now, DefaultLeaseDuration)
	test.Equals(t, 0, f0.Index)
	test.OK(t, run1.Save())

	// second run: the interrupted claim is reclaimed
	run2, err := Open(path)
	test.OK(t, err)
	test.Assert(t, run2.runID != run1.runID, "each run must get a unique run ID")
	f0 = run2.Claim(now.Add(time.Minute), DefaultLeaseDuration)
	test.Equals(t, 0, f0.Index)
	test.Equals(t, StatusClaimed, f0.Status)
	test.Equals(t, 2, f0.Attempts, "reclaimed fragment counts a new attempt")
	test.OK(t, run2.Succeed(f0, now.Add(2*time.Minute)))

	f1 := run2.Claim(now.Add(3*time.Minute), DefaultLeaseDuration)
	test.Equals(t, 1, f1.Index)
	test.OK(t, run2.Fail(f1, now.Add(4*time.Minute), os.ErrInvalid))
	test.OK(t, run2.Save())

	// third run: pending work is claimed first, the failed fragment is retried afterwards
	run3, err := Open(path)
	test.OK(t, err)
	done, failed, pending, claimed := run3.Counts()
	test.Equals(t, 1, done)
	test.Equals(t, 1, failed)
	test.Equals(t, 1, pending)
	test.Equals(t, 0, claimed)

	f2 := run3.Claim(now.Add(5*time.Minute), DefaultLeaseDuration)
	test.Equals(t, 2, f2.Index)
	test.Equals(t, 1, f2.Attempts)
	test.OK(t, run3.Succeed(f2, now.Add(6*time.Minute)))

	f1 = run3.Claim(now.Add(7*time.Minute), DefaultLeaseDuration)
	test.Equals(t, 1, f1.Index)
	test.Equals(t, 2, f1.Attempts)
	test.OK(t, run3.Succeed(f1, now.Add(8*time.Minute)))
	test.OK(t, run3.Save())

	// a fourth run finds nothing to do
	run4, err := Open(path)
	test.OK(t, err)
	test.Assert(t, run4.Claim(now.Add(9*time.Minute), DefaultLeaseDuration) == nil,
		"completed plan must not offer fragments")
	done, _, _, _ = run4.Counts()
	test.Equals(t, 3, done)
}

func TestExpiredLeaseReclaim(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	p := New("fp", testPacks(1), 1, now)

	f := p.Claim(now, time.Minute)
	// same run, but the lease expired long ago
	reclaimed := p.Claim(now.Add(time.Hour), time.Minute)
	test.Assert(t, reclaimed != nil && reclaimed.Index == 0, "expired claim must be reclaimed")
	test.Equals(t, 2, f.Attempts)

	// within the lease, a claimed fragment is not handed out twice
	p2 := New("fp", testPacks(1), 1, now)
	f = p2.Claim(now, time.Hour)
	test.Assert(t, p2.Claim(now.Add(time.Minute), time.Hour) == nil, "active claim must not be reclaimed")
	_ = f
}

func TestOpenMissingCorruptAndOutdated(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.json")
	_, err := Open(missing)
	test.Assert(t, stderrors.Is(err, os.ErrNotExist), "expected not-exist error, got %v", err)

	path := filepath.Join(dir, "plan.json")
	p := New("fp", testPacks(2), 1, time.Now())
	p.SetPath(path)
	test.OK(t, p.Save())

	loaded, err := Open(path)
	test.OK(t, err)
	test.Assert(t, loaded.Matches("fp"), "plan must match its own fingerprint")
	test.Assert(t, !loaded.Matches("other"), "plan must not match a different fingerprint")

	test.OK(t, os.WriteFile(path, []byte("{not json"), 0600))
	_, err = Open(path)
	test.Assert(t, err != nil, "corrupt plan must fail to open")

	p = New("fp", testPacks(2), 1, time.Now())
	p.SetPath(path)
	p.Version = Version + 1
	test.OK(t, p.Save())
	_, err = Open(path)
	test.Assert(t, err != nil, "unsupported plan version must fail to open")
}

func TestSaveWithoutPathFails(t *testing.T) {
	p := New("fp", testPacks(1), 1, time.Now())
	test.Assert(t, p.Save() != nil, "saving a plan without a path must fail")
}

func TestSaveIsAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plan.json")
	p := New("fp", testPacks(2), 1, time.Now())
	p.SetPath(path)
	test.OK(t, p.Save())

	info, err := os.Stat(path)
	test.OK(t, err)
	test.Assert(t, info.Mode().Perm() == 0600, "plan must not be world-readable, got %v", info.Mode())

	entries, err := os.ReadDir(filepath.Dir(path))
	test.OK(t, err)
	for _, entry := range entries {
		test.Assert(t, filepath.Ext(entry.Name()) == ".json", "temporary file left behind: %v", entry.Name())
	}
}
