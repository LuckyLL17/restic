package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/restic/restic/internal/backend"
	"github.com/restic/restic/internal/global"
	"github.com/restic/restic/internal/restic"
	rtest "github.com/restic/restic/internal/test"
	"github.com/restic/restic/internal/ui/progress"
)

type retainedJSON struct {
	ObjectType string `json:"object_type"`
	ID         string `json:"id"`
	Status     string `json:"status"`
	Reason     string `json:"reason"`
	InBackend  bool   `json:"in_backend"`
	InIndex    bool   `json:"in_index"`
	References []struct {
		Snapshot string   `json:"snapshot"`
		Path     string   `json:"path"`
		Chain    []string `json:"chain"`
	} `json:"references"`
	Locations []struct {
		BlobID      string `json:"blob_id"`
		BlobType    string `json:"blob_type"`
		PackID      string `json:"pack_id"`
		PackPresent bool   `json:"pack_present"`
	} `json:"locations"`
}

func testRunRetainedJSON(t *testing.T, gopts global.Options, opts RetainedOptions, ids ...string) []retainedJSON {
	t.Helper()
	buf, err := withCaptureStdout(t, gopts, func(ctx context.Context, gopts global.Options) error {
		gopts.JSON = true
		return runRetained(ctx, opts, gopts, ids)
	})
	rtest.OK(t, err)
	var results []retainedJSON
	rtest.OK(t, json.Unmarshal(buf.Bytes(), &results))
	return results
}

func firstPackID(t *testing.T, gopts global.Options, tpe restic.BlobType) restic.ID {
	t.Helper()
	var packID restic.ID
	rtest.OK(t, withTermStatus(t, gopts, func(ctx context.Context, gopts global.Options) error {
		printer := progress.NewTerminalPrinter(gopts.JSON, gopts.Verbosity, gopts.Term)
		_, repo, unlock, err := openWithReadLock(ctx, gopts, false, printer)
		rtest.OK(t, err)
		defer unlock()
		rtest.OK(t, repo.LoadIndex(ctx, restic.NoopTerminalCounterFactory))
		return repo.ListBlobs(ctx, func(pb restic.PackBlob) {
			if pb.Handle().Type == tpe {
				packID = pb.PackID()
			}
		})
	}))
	rtest.Assert(t, !packID.IsNull(), "expected to find a pack containing a %v blob", tpe)
	return packID
}

// allBlobStatuses queries all data blobs and returns the first result with the
// given status.
func blobWithStatus(t *testing.T, gopts global.Options, status string) retainedJSON {
	t.Helper()
	var dataIDs []string
	rtest.OK(t, withTermStatus(t, gopts, func(ctx context.Context, gopts global.Options) error {
		printer := progress.NewTerminalPrinter(gopts.JSON, gopts.Verbosity, gopts.Term)
		_, repo, unlock, err := openWithReadLock(ctx, gopts, false, printer)
		rtest.OK(t, err)
		defer unlock()
		rtest.OK(t, repo.LoadIndex(ctx, restic.NoopTerminalCounterFactory))
		return repo.ListBlobs(ctx, func(pb restic.PackBlob) {
			if pb.Handle().Type == restic.DataBlob {
				dataIDs = append(dataIDs, pb.Handle().ID.String())
			}
		})
	}))

	for _, id := range dataIDs {
		results := testRunRetainedJSON(t, gopts, RetainedOptions{BlobID: true}, id)
		if results[0].Status == status {
			return results[0]
		}
	}
	t.Fatalf("no data blob with status %q found", status)
	return retainedJSON{}
}

func TestRetainedReferencedBlobAndPack(t *testing.T) {
	env, cleanup := withTestEnvironment(t)
	defer cleanup()

	testSetupBackupData(t, env)
	testRunBackup(t, "", []string{filepath.Join(env.testdata, "0", "0", "9")}, BackupOptions{}, env.gopts)

	dataPack := firstPackID(t, env.gopts, restic.DataBlob)

	// an existing data blob in that pack is referenced by the snapshot
	results := testRunRetainedJSON(t, env.gopts, RetainedOptions{PackID: true}, dataPack.String())
	rtest.Assert(t, len(results) == 1, "expected one result")
	rtest.Equals(t, results[0].ObjectType, "pack")
	rtest.Equals(t, results[0].Status, "referenced")
	rtest.Assert(t, results[0].InBackend && results[0].InIndex, "pack must be present and indexed")
	rtest.Assert(t, len(results[0].References) > 0, "expected references to snapshots")
	for _, ref := range results[0].References {
		rtest.Assert(t, len(ref.Chain) > 0, "reference must include intermediate trees")
	}

	// automatic detection (no --pack/--blob) resolves the same pack id
	auto := testRunRetainedJSON(t, env.gopts, RetainedOptions{}, dataPack.String()[:16])
	rtest.Equals(t, auto[0].Status, "referenced")
	rtest.Equals(t, auto[0].ObjectType, "pack")
}

func TestRetainedIndexOnlyBlobAfterForget(t *testing.T) {
	env, cleanup := withTestEnvironment(t)
	defer cleanup()

	testSetupBackupData(t, env)
	first := filepath.Join(env.testdata, "0", "0", "9", "2")
	second := filepath.Join(env.testdata, "0", "0", "9", "3")

	testRunBackup(t, "", []string{first}, BackupOptions{}, env.gopts)
	sn1 := testListSnapshots(t, env.gopts, 1)[0]
	testRunBackup(t, "", []string{second}, BackupOptions{}, env.gopts)

	// remove the first snapshot without running prune
	testRunForget(t, env.gopts, ForgetOptions{}, sn1.String())

	indexOnly := blobWithStatus(t, env.gopts, "index-only")
	rtest.Assert(t, len(indexOnly.Locations) > 0, "index-only blob must have a location")
	rtest.Assert(t, indexOnly.Locations[0].PackPresent, "index-only blob pack must still exist")

	// a pack made up entirely of unused blobs must also be index-only
	packID := indexOnly.Locations[0].PackID
	packResult := testRunRetainedJSON(t, env.gopts, RetainedOptions{PackID: true}, packID)
	rtest.Equals(t, packResult[0].Status, "index-only")
}

func TestRetainedUnreferencedPack(t *testing.T) {
	env, cleanup := withTestEnvironment(t)
	defer cleanup()

	testSetupBackupData(t, env)
	testRunBackup(t, "", []string{filepath.Join(env.testdata, "0", "0", "9")}, BackupOptions{}, env.gopts)

	orphanID := restic.Hash([]byte("orphan-pack"))
	be := captureBackend(&env.gopts)
	rtest.OK(t, withTermStatus(t, env.gopts, func(ctx context.Context, gopts global.Options) error {
		printer := progress.NewTerminalPrinter(gopts.JSON, gopts.Verbosity, gopts.Term)
		_, _, unlock, err := openWithExclusiveLock(ctx, gopts, false, printer)
		rtest.OK(t, err)
		defer unlock()
		h := backend.Handle{Type: backend.PackFile, Name: orphanID.String()}
		return be().Save(ctx, h, backend.NewByteReader([]byte("orphan"), be().Hasher()))
	}))

	results := testRunRetainedJSON(t, env.gopts, RetainedOptions{PackID: true}, orphanID.String())
	rtest.Equals(t, results[0].Status, "unreferenced")
	rtest.Assert(t, results[0].InBackend && !results[0].InIndex, "orphan pack must be present but unindexed")
}

func TestRetainedNotFound(t *testing.T) {
	env, cleanup := withTestEnvironment(t)
	defer cleanup()

	testSetupBackupData(t, env)
	testRunBackup(t, "", []string{filepath.Join(env.testdata, "0", "0", "9")}, BackupOptions{}, env.gopts)

	unknown := restic.TestParseID("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	results := testRunRetainedJSON(t, env.gopts, RetainedOptions{BlobID: true}, unknown.String())
	rtest.Equals(t, results[0].Status, "not-found")
}

func TestRetainedTextOutput(t *testing.T) {
	env, cleanup := withTestEnvironment(t)
	defer cleanup()

	testSetupBackupData(t, env)
	testRunBackup(t, "", []string{filepath.Join(env.testdata, "0", "0", "9")}, BackupOptions{}, env.gopts)
	packID := firstPackID(t, env.gopts, restic.DataBlob)

	buf, err := withCaptureStdout(t, env.gopts, func(ctx context.Context, gopts global.Options) error {
		return runRetained(ctx, RetainedOptions{PackID: true}, gopts, []string{packID.String()})
	})
	rtest.OK(t, err)
	out := buf.String()
	rtest.Assert(t, strings.Contains(out, "state: referenced"), "missing state in output:\n%s", out)
	rtest.Assert(t, strings.Contains(out, "references:"), "missing references in output:\n%s", out)
	rtest.Assert(t, strings.Contains(out, "tree chain:"), "missing tree chain in output:\n%s", out)
}

func TestRetainedInvalidArguments(t *testing.T) {
	env, cleanup := withTestEnvironment(t)
	defer cleanup()

	testSetupBackupData(t, env)
	testRunBackup(t, "", []string{filepath.Join(env.testdata, "0", "0", "9")}, BackupOptions{}, env.gopts)

	err := withTermStatus(t, env.gopts, func(ctx context.Context, gopts global.Options) error {
		return runRetained(ctx, RetainedOptions{}, gopts, nil)
	})
	rtest.Assert(t, err != nil, "expected error without arguments")

	err = withTermStatus(t, env.gopts, func(ctx context.Context, gopts global.Options) error {
		return runRetained(ctx, RetainedOptions{PackID: true, BlobID: true}, gopts, []string{"abcd"})
	})
	rtest.Assert(t, err != nil, "expected error for --pack and --blob")
}

func TestRetainedIsReadOnly(t *testing.T) {
	env, cleanup := withTestEnvironment(t)
	defer cleanup()

	testSetupBackupData(t, env)
	testRunBackup(t, "", []string{filepath.Join(env.testdata, "0", "0", "9")}, BackupOptions{}, env.gopts)

	before := testRunList(t, env.gopts, "packs")
	packID := firstPackID(t, env.gopts, restic.TreeBlob)

	testRunRetainedJSON(t, env.gopts, RetainedOptions{}, packID.String())
	testRunCheck(t, env.gopts)

	after := testRunList(t, env.gopts, "packs")
	rtest.Equals(t, after, before)
}
