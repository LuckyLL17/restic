package retention_test

import (
	"context"
	"testing"
	"time"

	"github.com/restic/restic/internal/backend"
	"github.com/restic/restic/internal/data"
	"github.com/restic/restic/internal/repository"
	"github.com/restic/restic/internal/restic"
	"github.com/restic/restic/internal/retention"
	rtest "github.com/restic/restic/internal/test"
)

type testFixture struct {
	ctx      context.Context
	repo     *repository.Repository
	be       backend.Backend
	snapshot restic.ID
	root     restic.ID
	sub      restic.ID
	data1    restic.BlobHandle
	data2    restic.BlobHandle
	unused   restic.BlobHandle
}

func setupTestRepo(t *testing.T) testFixture {
	t.Helper()
	ctx := context.Background()
	repo, _, be := repository.TestRepositoryWithVersion(t, 2)

	saveData := func(payload string) restic.BlobHandle {
		id := restic.Hash([]byte(payload))
		err := repo.WithBlobUploader(ctx, func(ctx context.Context, uploader restic.BlobSaverWithAsync) error {
			newID, _, _, err := uploader.SaveBlob(ctx, restic.DataBlob, []byte(payload), id, false)
			rtest.OK(t, err)
			rtest.Assert(t, newID.Equal(id), "unexpected blob id")
			return nil
		})
		rtest.OK(t, err)
		return restic.BlobHandle{ID: id, Type: restic.DataBlob}
	}

	data1 := saveData("file-content-1")
	data2 := saveData("file-content-2")
	unused := saveData("unused-content")

	var subID, rootID restic.ID
	rtest.OK(t, repo.WithBlobUploader(ctx, func(ctx context.Context, uploader restic.BlobSaverWithAsync) error {
		subID = data.TestSaveNodes(t, ctx, uploader, []*data.Node{
			{Name: "file1", Type: data.NodeTypeFile, Mode: 0644, Size: 14, Content: restic.IDs{data1.ID}},
			{Name: "file2", Type: data.NodeTypeFile, Mode: 0644, Size: 14, Content: restic.IDs{data2.ID}},
		})
		rootID = data.TestSaveNodes(t, ctx, uploader, []*data.Node{
			{Name: "dir", Type: data.NodeTypeDir, Mode: 0755, Subtree: &subID},
		})
		return nil
	}))

	sn, err := data.NewSnapshot([]string{"/test"}, nil, "testhost", time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC))
	rtest.OK(t, err)
	sn.Tree = &rootID
	snapID, err := data.SaveSnapshot(ctx, repo, sn)
	rtest.OK(t, err)

	rtest.OK(t, repo.LoadIndex(ctx, restic.NoopTerminalCounterFactory))

	return testFixture{
		ctx:      ctx,
		repo:     repo,
		be:       be,
		snapshot: snapID,
		root:     rootID,
		sub:      subID,
		data1:    data1,
		data2:    data2,
		unused:   unused,
	}
}

func analyzeBlob(t *testing.T, f testFixture, id string) *retention.Result {
	t.Helper()
	results, err := retention.Analyze(f.ctx, f.repo, f.repo, []retention.Query{{Kind: retention.KindBlob, ID: id}})
	rtest.OK(t, err)
	rtest.Assert(t, len(results) == 1, "expected one result")
	rtest.Equals(t, results[0].Kind, retention.KindBlob)
	return results[0]
}

func analyzePack(t *testing.T, f testFixture, id string) *retention.Result {
	t.Helper()
	results, err := retention.Analyze(f.ctx, f.repo, f.repo, []retention.Query{{Kind: retention.KindPack, ID: id}})
	rtest.OK(t, err)
	rtest.Assert(t, len(results) == 1, "expected one result")
	rtest.Equals(t, results[0].Kind, retention.KindPack)
	return results[0]
}

func TestAnalyzeReferencedDataBlob(t *testing.T) {
	f := setupTestRepo(t)
	res := analyzeBlob(t, f, f.data1.ID.String())

	rtest.Equals(t, res.Status, retention.StatusReferenced)
	rtest.Assert(t, len(res.References) == 1, "expected one reference, got %d", len(res.References))
	ref := res.References[0]
	rtest.Assert(t, ref.Snapshot.Equal(f.snapshot), "snapshot mismatch")
	rtest.Equals(t, ref.Path, "/dir/file1")
	rtest.Assert(t, ref.ParentTree.Equal(f.sub), "parent tree mismatch")
	rtest.Assert(t, len(ref.Chain) == 2, "expected two intermediate trees, got %d", len(ref.Chain))
	rtest.Assert(t, ref.Chain[0].Equal(f.root), "chain root mismatch")
	rtest.Assert(t, ref.Chain[1].Equal(f.sub), "chain subtree mismatch")
	rtest.Equals(t, ref.BlobType, restic.DataBlob)
	rtest.Assert(t, len(res.Locations) == 1, "expected one blob location")
	rtest.Assert(t, res.Locations[0].PackPresent, "pack should be present")
}

func TestAnalyzeReferencedTreeBlobs(t *testing.T) {
	f := setupTestRepo(t)

	sub := analyzeBlob(t, f, f.sub.String())
	rtest.Equals(t, sub.Status, retention.StatusReferenced)
	rtest.Assert(t, len(sub.References) == 1, "expected one reference")
	rtest.Equals(t, sub.References[0].Path, "/dir")
	rtest.Assert(t, sub.References[0].ParentTree.Equal(f.root), "parent tree mismatch")
	rtest.Assert(t, len(sub.References[0].Chain) == 1 && sub.References[0].Chain[0].Equal(f.root), "chain mismatch")

	root := analyzeBlob(t, f, f.root.String())
	rtest.Equals(t, root.Status, retention.StatusReferenced)
	rtest.Assert(t, len(root.References) == 1, "expected one reference")
	rtest.Equals(t, root.References[0].Path, "/")
	rtest.Assert(t, root.References[0].ParentTree.Equal(f.root), "root parent mismatch")
}

func TestAnalyzeIndexOnlyBlob(t *testing.T) {
	f := setupTestRepo(t)
	res := analyzeBlob(t, f, f.unused.ID.String())

	rtest.Equals(t, res.Status, retention.StatusIndexOnly)
	rtest.Assert(t, len(res.References) == 0, "expected no references")
	rtest.Assert(t, len(res.Locations) >= 1, "expected an index location")
	rtest.Assert(t, res.InBackend, "pack should be present")
}

func TestAnalyzeReferencedPack(t *testing.T) {
	f := setupTestRepo(t)
	blobs := f.repo.LookupBlob(f.data1)
	rtest.Assert(t, len(blobs) >= 1, "expected pack location")
	packID := blobs[0].PackID()

	res := analyzePack(t, f, packID.String())
	rtest.Equals(t, res.Status, retention.StatusReferenced)
	rtest.Assert(t, res.InBackend && res.InIndex, "pack should be present and indexed")
	rtest.Assert(t, len(res.References) >= 1, "expected snapshot references")
	rtest.Assert(t, res.PhysicalSize > 0, "expected physical pack size")
}

func TestAnalyzeIndexOnlyPack(t *testing.T) {
	f := setupTestRepo(t)
	// the unused blob was saved in its own upload session, so its pack only
	// contains blobs that are not referenced by any snapshot
	blobs := f.repo.LookupBlob(f.unused)
	rtest.Assert(t, len(blobs) == 1, "expected exactly one pack for unused blob")
	packID := blobs[0].PackID()

	res := analyzePack(t, f, packID.String())
	rtest.Equals(t, res.Status, retention.StatusIndexOnly)
	rtest.Assert(t, len(res.References) == 0, "expected no references")
}

func TestAnalyzeUnreferencedPack(t *testing.T) {
	f := setupTestRepo(t)
	orphanID := restic.Hash([]byte("orphan-pack-content"))
	h := backend.Handle{Type: backend.PackFile, Name: orphanID.String()}
	rtest.OK(t, f.be.Save(f.ctx, h, backend.NewByteReader([]byte("not-a-real-pack"), f.be.Hasher())))

	res := analyzePack(t, f, orphanID.String())
	rtest.Equals(t, res.Status, retention.StatusUnreferenced)
	rtest.Assert(t, res.InBackend && !res.InIndex, "orphan pack must be present but unindexed")
	rtest.Assert(t, len(res.Warnings) <= 1, "invalid pack header should at most warn")
}

func TestAnalyzeUntraceableBlobAndPack(t *testing.T) {
	f := setupTestRepo(t)
	blobs := f.repo.LookupBlob(f.unused)
	rtest.Assert(t, len(blobs) == 1, "expected exactly one pack for unused blob")
	packID := blobs[0].PackID()

	rtest.OK(t, f.be.Remove(f.ctx, backend.Handle{Type: backend.PackFile, Name: packID.String()}))

	blobRes := analyzeBlob(t, f, f.unused.ID.String())
	rtest.Equals(t, blobRes.Status, retention.StatusUntraceable)
	rtest.Assert(t, !blobRes.Locations[0].PackPresent, "pack must be reported missing")

	packRes := analyzePack(t, f, packID.String())
	rtest.Equals(t, packRes.Status, retention.StatusUntraceable)
	rtest.Assert(t, !packRes.InBackend && packRes.InIndex, "missing pack must still be indexed")
}

func TestAnalyzeReferencedPackMissingFromBackend(t *testing.T) {
	f := setupTestRepo(t)
	blobs := f.repo.LookupBlob(f.data1)
	rtest.Assert(t, len(blobs) == 1, "expected exactly one pack for data1")
	packID := blobs[0].PackID()
	rtest.OK(t, f.be.Remove(f.ctx, backend.Handle{Type: backend.PackFile, Name: packID.String()}))

	res := analyzePack(t, f, packID.String())
	rtest.Equals(t, res.Status, retention.StatusUntraceable)
	rtest.Assert(t, !res.InBackend && res.InIndex, "pack must be indexed but missing")
	rtest.Assert(t, len(res.References) > 0, "snapshot references for indexed blobs must be reported")

	blobRes := analyzeBlob(t, f, f.data1.ID.String())
	rtest.Equals(t, blobRes.Status, retention.StatusReferenced)
	rtest.Assert(t, !blobRes.Locations[0].PackPresent, "location must be flagged as missing")
}

func TestAnalyzeNotFound(t *testing.T) {
	f := setupTestRepo(t)
	unknown := restic.TestParseID("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")

	blobRes := analyzeBlob(t, f, unknown.String())
	rtest.Equals(t, blobRes.Status, retention.StatusNotFound)

	packRes := analyzePack(t, f, unknown.String())
	rtest.Equals(t, packRes.Status, retention.StatusNotFound)
}

func TestAnalyzePrefixResolution(t *testing.T) {
	f := setupTestRepo(t)
	fullID := f.data1.ID.String()

	// derive a short prefix that uniquely identifies data1
	others := []string{f.data2.ID.String(), f.unused.ID.String(), f.sub.String(), f.root.String()}
	unique := 1
	for _, other := range others {
		for unique < len(fullID) && fullID[:unique] == other[:unique] {
			unique++
		}
	}
	// use an odd prefix length to verify those are accepted as well
	if unique%2 == 0 {
		unique++
	}
	results, err := retention.Analyze(f.ctx, f.repo, f.repo, []retention.Query{{Kind: retention.KindBlob, ID: fullID[:unique]}})
	rtest.OK(t, err)
	rtest.Assert(t, results[0].ID.Equal(f.data1.ID), "prefix resolved to wrong blob")
	rtest.Equals(t, results[0].Status, retention.StatusReferenced)
}

func TestAnalyzeUnknownPrefix(t *testing.T) {
	f := setupTestRepo(t)
	_, err := retention.Analyze(f.ctx, f.repo, f.repo, []retention.Query{{Kind: retention.KindBlob, ID: "deadbeef"}})
	rtest.Assert(t, err != nil, "expected error for unknown prefix")
}

func TestAnalyzeInvalidID(t *testing.T) {
	f := setupTestRepo(t)
	_, err := retention.Analyze(f.ctx, f.repo, f.repo, []retention.Query{{ID: "not-hex!!"}})
	rtest.Assert(t, err != nil, "expected error for invalid ID")
}

func TestAnalyzeIsReadOnly(t *testing.T) {
	f := setupTestRepo(t)
	packsBefore := listBackendPacks(t, f)

	_, err := retention.Analyze(f.ctx, f.repo, f.repo, []retention.Query{
		{Kind: retention.KindBlob, ID: f.data1.ID.String()},
		{Kind: retention.KindBlob, ID: f.unused.ID.String()},
	})
	rtest.OK(t, err)

	packsAfter := listBackendPacks(t, f)
	rtest.Equals(t, packsAfter, packsBefore)
}

func listBackendPacks(t *testing.T, f testFixture) map[restic.ID]int64 {
	t.Helper()
	packs := make(map[restic.ID]int64)
	rtest.OK(t, f.repo.List(f.ctx, restic.PackFile, func(id restic.ID, size int64) error {
		packs[id] = size
		return nil
	}))
	return packs
}
