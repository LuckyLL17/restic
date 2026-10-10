package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/restic/restic/internal/data"
	"github.com/restic/restic/internal/diff"
	"github.com/restic/restic/internal/global"
	"github.com/restic/restic/internal/restic"
	rtest "github.com/restic/restic/internal/test"
	"github.com/restic/restic/internal/ui/progress"
)

func testRunDiffOutput(t testing.TB, gopts global.Options, firstSnapshotID string, secondSnapshotID string) (string, error) {
	buf, err := withCaptureStdout(t, gopts, func(ctx context.Context, gopts global.Options) error {
		opts := DiffOptions{
			ShowMetadata: false,
		}
		return runDiff(ctx, opts, gopts, []string{firstSnapshotID, secondSnapshotID}, gopts.Term)
	})
	return buf.String(), err
}

func copyFile(dst string, src string) error {
	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}

	dstFile, err := os.Create(dst)
	if err != nil {
		// ignore subsequent errors
		_ = srcFile.Close()
		return err
	}

	_, err = io.Copy(dstFile, srcFile)
	if err != nil {
		// ignore subsequent errors
		_ = srcFile.Close()
		_ = dstFile.Close()
		return err
	}

	err = srcFile.Close()
	if err != nil {
		// ignore subsequent errors
		_ = dstFile.Close()
		return err
	}

	err = dstFile.Close()
	if err != nil {
		return err
	}

	return nil
}

var diffOutputRegexPatterns = []string{
	"-.+modfile",
	"M.+modfile1",
	"\\+.+modfile2",
	"\\+.+modfile3",
	"\\+.+modfile4",
	"-.+submoddir",
	"-.+submoddir.subsubmoddir",
	"\\+.+submoddir2",
	"\\+.+submoddir2.subsubmoddir",
	"Files: +2 new, +1 removed, +1 changed",
	"Dirs: +3 new, +2 removed",
	"Data Blobs: +2 new, +1 removed",
	"Added: +7[0-9]{2}\\.[0-9]{3} KiB",
	"Removed: +2[0-9]{2}\\.[0-9]{3} KiB",
}

func setupDiffRepo(t *testing.T) (*testEnvironment, func(), string, string) {
	env, cleanup := withTestEnvironment(t)
	testRunInit(t, env.gopts)

	datadir := filepath.Join(env.base, "testdata")
	testdir := filepath.Join(datadir, "testdir")
	subtestdir := filepath.Join(testdir, "subtestdir")
	testfile := filepath.Join(testdir, "testfile")

	rtest.OK(t, os.Mkdir(testdir, 0755))
	rtest.OK(t, os.Mkdir(subtestdir, 0755))
	rtest.OK(t, appendRandomData(testfile, 256*1024))

	moddir := filepath.Join(datadir, "moddir")
	submoddir := filepath.Join(moddir, "submoddir")
	subsubmoddir := filepath.Join(submoddir, "subsubmoddir")
	modfile := filepath.Join(moddir, "modfile")
	rtest.OK(t, os.Mkdir(moddir, 0755))
	rtest.OK(t, os.Mkdir(submoddir, 0755))
	rtest.OK(t, os.Mkdir(subsubmoddir, 0755))
	rtest.OK(t, copyFile(modfile, testfile))
	rtest.OK(t, appendRandomData(modfile+"1", 256*1024))

	snapshots := make(map[string]struct{})
	opts := BackupOptions{}
	testRunBackup(t, "", []string{datadir}, opts, env.gopts)
	snapshots, firstSnapshotID := lastSnapshot(snapshots, loadSnapshotMap(t, env.gopts))

	rtest.OK(t, os.Rename(modfile, modfile+"3"))
	rtest.OK(t, os.Rename(submoddir, submoddir+"2"))
	rtest.OK(t, appendRandomData(modfile+"1", 256*1024))
	rtest.OK(t, appendRandomData(modfile+"2", 256*1024))
	rtest.OK(t, os.Mkdir(modfile+"4", 0755))

	testRunBackup(t, "", []string{datadir}, opts, env.gopts)
	_, secondSnapshotID := lastSnapshot(snapshots, loadSnapshotMap(t, env.gopts))

	return env, cleanup, firstSnapshotID, secondSnapshotID
}

func TestDiff(t *testing.T) {
	env, cleanup, firstSnapshotID, secondSnapshotID := setupDiffRepo(t)
	defer cleanup()

	// quiet suppresses the diff output except for the summary
	env.gopts.Quiet = false
	_, err := testRunDiffOutput(t, env.gopts, "", secondSnapshotID)
	rtest.Assert(t, err != nil, "expected error on invalid snapshot id")

	out, err := testRunDiffOutput(t, env.gopts, firstSnapshotID, secondSnapshotID)
	rtest.OK(t, err)

	for _, pattern := range diffOutputRegexPatterns {
		r, err := regexp.Compile(pattern)
		rtest.Assert(t, err == nil, "failed to compile regexp %v", pattern)
		rtest.Assert(t, r.MatchString(out), "expected pattern %v in output, got\n%v", pattern, out)
	}

	// check quiet output
	env.gopts.Quiet = true
	outQuiet, err := testRunDiffOutput(t, env.gopts, firstSnapshotID, secondSnapshotID)
	rtest.OK(t, err)

	rtest.Assert(t, len(outQuiet) < len(out), "expected shorter output on quiet mode %v vs. %v", len(outQuiet), len(out))
}

type typeSniffer struct {
	MessageType string `json:"message_type"`
}

func TestDiffJSON(t *testing.T) {
	env, cleanup, firstSnapshotID, secondSnapshotID := setupDiffRepo(t)
	defer cleanup()

	// quiet suppresses the diff output except for the summary
	env.gopts.Quiet = false
	env.gopts.JSON = true
	out, err := testRunDiffOutput(t, env.gopts, firstSnapshotID, secondSnapshotID)
	rtest.OK(t, err)

	var stat DiffStatsContainer
	var changes int

	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		line := scanner.Text()
		var sniffer typeSniffer
		rtest.OK(t, json.Unmarshal([]byte(line), &sniffer))
		switch sniffer.MessageType {
		case "change":
			changes++
		case "statistics":
			rtest.OK(t, json.Unmarshal([]byte(line), &stat))
		default:
			t.Fatalf("unexpected message type %v", sniffer.MessageType)
		}
	}
	rtest.Equals(t, 9, changes)
	rtest.Assert(t, stat.Added.Files == 2 && stat.Added.Dirs == 3 && stat.Added.DataBlobs == 2 &&
		stat.Removed.Files == 1 && stat.Removed.Dirs == 2 && stat.Removed.DataBlobs == 1 &&
		stat.ChangedFiles == 1, "unexpected statistics")

	// check quiet output
	env.gopts.Quiet = true
	outQuiet, err := testRunDiffOutput(t, env.gopts, firstSnapshotID, secondSnapshotID)
	rtest.OK(t, err)

	stat = DiffStatsContainer{}
	rtest.OK(t, json.Unmarshal([]byte(outQuiet), &stat))
	rtest.Assert(t, stat.Added.Files == 2 && stat.Added.Dirs == 3 && stat.Added.DataBlobs == 2 &&
		stat.Removed.Files == 1 && stat.Removed.Dirs == 2 && stat.Removed.DataBlobs == 1 &&
		stat.ChangedFiles == 1, "unexpected statistics")
	rtest.Assert(t, stat.SourceSnapshot == firstSnapshotID && stat.TargetSnapshot == secondSnapshotID, "unexpected snapshot ids")
}

func parseDiffJSON(t *testing.T, out string) ([]Change, DiffStatsContainer) {
	t.Helper()
	var changes []Change
	var stat DiffStatsContainer

	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		var sniffer typeSniffer
		rtest.OK(t, json.Unmarshal(scanner.Bytes(), &sniffer))
		switch sniffer.MessageType {
		case "change":
			var ch Change
			rtest.OK(t, json.Unmarshal(scanner.Bytes(), &ch))
			changes = append(changes, ch)
		case "statistics":
			rtest.OK(t, json.Unmarshal(scanner.Bytes(), &stat))
		default:
			t.Fatalf("unexpected message type %v", sniffer.MessageType)
		}
	}
	rtest.OK(t, scanner.Err())
	return changes, stat
}

// TestDiffResumableSegments verifies against a real repository that running
// the comparison as a sequence of segments, with the token serialized and
// parsed (and the comparer recreated) between segments, produces exactly the
// same changes and final statistics as the one-shot CLI command.
func TestDiffResumableSegments(t *testing.T) {
	env, cleanup, firstSnapshotID, secondSnapshotID := setupDiffRepo(t)
	defer cleanup()

	// one-shot reference via the CLI JSON output
	env.gopts.Quiet = false
	env.gopts.JSON = true
	refOut, err := testRunDiffOutput(t, env.gopts, firstSnapshotID, secondSnapshotID)
	rtest.OK(t, err)
	refChanges, refStat := parseDiffJSON(t, refOut)

	for _, maxNodes := range []int{1, 4, 64} {
		t.Run("", func(t *testing.T) {
			gopts := env.gopts
			err := withTermStatus(t, gopts, func(ctx context.Context, gopts global.Options) error {
				printer := progress.NewTerminalPrinter(gopts.JSON, gopts.Verbosity, gopts.Term)
				ctx, repo, unlock, err := openWithReadLock(ctx, gopts, false, printer)
				rtest.OK(t, err)
				defer unlock()

				be, err := restic.MemorizeList(ctx, repo, restic.SnapshotFile)
				rtest.OK(t, err)

				sn1, sub1, err := data.FindSnapshot(ctx, be, repo, firstSnapshotID)
				rtest.OK(t, err)
				sn2, sub2, err := data.FindSnapshot(ctx, be, repo, secondSnapshotID)
				rtest.OK(t, err)

				rtest.OK(t, repo.LoadIndex(ctx, printer))

				root1, root2, err := diff.ResolveRoots(ctx, repo, sn1.Tree, sn2.Tree, sub1, sub2)
				rtest.OK(t, err)

				cmpOpts := diff.Options{}
				token := diff.NewResumeToken(repo.Config().ID, *sn1.ID(), *sn2.ID(), sub1, sub2, root1, root2, cmpOpts)

				var gotChanges []Change
				segments := 0
				for !token.Done {
					// hand the token to another process: serialize, parse and
					// recreate the comparer
					buf, err := token.Marshal()
					rtest.OK(t, err)
					token, err = diff.ParseResumeToken(buf)
					rtest.OK(t, err)
					rtest.OK(t, token.Validate(repo.Config().ID, *sn1.ID(), *sn2.ID(), sub1, sub2, root1, root2, cmpOpts))

					c := diff.NewComparer(repo, cmpOpts)
					var res *diff.SegmentResult
					res, err = c.RunSegment(ctx, token, diff.Budget{MaxNodes: maxNodes}, func(ch *diff.Change) {
						gotChanges = append(gotChanges, Change{
							MessageType: "change",
							Path:        ch.Path,
							Modifier:    ch.Modifier,
						})
					}, func(err error) {
						t.Errorf("unexpected diff error: %v", err)
					})
					rtest.OK(t, err)
					if !token.Done {
						rtest.Equals(t, diff.StatusIncomplete, res.Status)
						rtest.Assert(t, len(res.Position.Stack) > 0, "expected continuation position")
					}
					segments++
					rtest.Assert(t, segments < 10000, "comparison did not finish")
				}

				rtest.Equals(t, refChanges, gotChanges)
				rtest.Equals(t, refStat.Added, token.Added)
				rtest.Equals(t, refStat.Removed, token.Removed)
				rtest.Equals(t, refStat.ChangedFiles, token.ChangedFiles)
				if maxNodes == 1 {
					rtest.Assert(t, segments > 1, "budget of 1 node must require multiple segments")
				}
				return nil
			})
			rtest.OK(t, err)
		})
	}
}
