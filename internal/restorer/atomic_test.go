package restorer

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/restic/restic/internal/archiver"
	"github.com/restic/restic/internal/fs"
	"github.com/restic/restic/internal/repository"
	"github.com/restic/restic/internal/restic"
	rtest "github.com/restic/restic/internal/test"
)

type treeEntry struct {
	isDir     bool
	isSymlink bool
	content   string
	link      string
	mode      os.FileMode
	mtime     time.Time
	inode     uint64
	device    uint64
}

func snapshotTreeState(t testing.TB, root string) map[string]treeEntry {
	state := make(map[string]treeEntry)
	root = filepath.Clean(root)

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}

		fi, err := os.Lstat(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}

		entry := treeEntry{
			isDir: d.IsDir(),
			mode:  fi.Mode(),
			mtime: fi.ModTime().UTC(),
		}
		if ex := fs.ExtendedStat(fi); ex != nil {
			entry.inode = ex.Inode
			entry.device = ex.DeviceID
		}

		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			entry.isSymlink = true
			entry.link, err = os.Readlink(path)
			rtest.OK(t, err)
			// symlink timestamps cannot be set on all platforms
			entry.mtime = time.Time{}
		case fi.Mode().IsRegular():
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			entry.content = string(data)
		}

		state[filepath.ToSlash(rel)] = entry
		return nil
	})
	rtest.OK(t, err)
	return state
}

func assertTreesEqual(t testing.TB, want, got map[string]treeEntry) {
	t.Helper()
	for path, wantEntry := range want {
		gotEntry, ok := got[path]
		if !ok {
			t.Errorf("expected %v to exist", path)
			continue
		}
		if wantEntry.isDir != gotEntry.isDir {
			t.Errorf("%v: isDir = %v, want %v", path, gotEntry.isDir, wantEntry.isDir)
		}
		if wantEntry.isSymlink != gotEntry.isSymlink {
			t.Errorf("%v: isSymlink = %v, want %v", path, gotEntry.isSymlink, wantEntry.isSymlink)
		}
		if wantEntry.content != gotEntry.content {
			t.Errorf("%v: content = %q, want %q", path, gotEntry.content, wantEntry.content)
		}
		if wantEntry.link != gotEntry.link {
			t.Errorf("%v: symlink target = %q, want %q", path, gotEntry.link, wantEntry.link)
		}
		// POSIX permissions and exact timestamps are not represented on Windows
		if runtime.GOOS == "windows" {
			continue
		}
		if wantEntry.mode != gotEntry.mode {
			t.Errorf("%v: mode = %v, want %v", path, gotEntry.mode, wantEntry.mode)
		}
		if !wantEntry.mtime.IsZero() && !wantEntry.mtime.Equal(gotEntry.mtime) {
			t.Errorf("%v: mtime = %v, want %v", path, gotEntry.mtime, wantEntry.mtime)
		}
	}
	for path := range got {
		if _, ok := want[path]; !ok {
			t.Errorf("unexpected extra entry %v", path)
		}
	}
}

func assertNoAtomicLeftovers(t testing.TB, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	rtest.OK(t, err)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), atomicStagingDirPrefix) || strings.HasPrefix(entry.Name(), atomicTrashDirPrefix) {
			t.Errorf("found leftover temporary directory %v", filepath.Join(dir, entry.Name()))
		}
	}
}

func writeTestFile(t testing.TB, path, content string, mode os.FileMode, mtime time.Time) {
	t.Helper()
	rtest.OK(t, os.MkdirAll(filepath.Dir(path), 0o700))
	rtest.OK(t, os.WriteFile(path, []byte(content), mode))
	rtest.OK(t, os.Chmod(path, mode))
	rtest.OK(t, os.Chtimes(path, mtime, mtime))
}

func TestRestorerAtomicSuccess(t *testing.T) {
	snapshotTime := time.Date(2020, time.January, 2, 3, 4, 5, 0, time.UTC)
	existingTime := time.Date(2018, time.May, 6, 7, 8, 9, 0, time.UTC)

	snapshot := Snapshot{
		Nodes: map[string]Node{
			"foo":  File{Data: "content: new\n", ModTime: snapshotTime},
			"link": Symlink{Target: "foo", ModTime: snapshotTime},
			"dirtest": Dir{
				Mode:    normalizeFileMode(0o750 | os.ModeDir),
				ModTime: snapshotTime,
				Nodes: map[string]Node{
					"file": File{Data: "content: file\n", ModTime: snapshotTime},
					"a":    File{Data: "hardlinked", Inode: 7, Links: 2, ModTime: snapshotTime},
					"b":    File{Data: "hardlinked", Inode: 7, Links: 2, ModTime: snapshotTime},
				},
			},
		},
	}

	repo := repository.TestRepository(t)
	sn, _ := saveSnapshot(t, repo, snapshot, noopGetGenericAttributes)

	tempdir := rtest.TempDir(t)
	target := filepath.Join(tempdir, "target")
	rtest.OK(t, os.Mkdir(target, 0o755))

	// pre-existing user data, partially overlapping with the snapshot
	writeTestFile(t, filepath.Join(target, "foo"), "content: old\n", 0o644, existingTime)
	writeTestFile(t, filepath.Join(target, "userfile"), "user data\n", 0o600, existingTime)
	writeTestFile(t, filepath.Join(target, "dirtest", "file"), "stale content\n", 0o644, existingTime)
	writeTestFile(t, filepath.Join(target, "dirtest", "oldchild"), "kept\n", 0o644, existingTime)
	// create the contents first, only then set the directory metadata
	rtest.OK(t, os.Mkdir(filepath.Join(target, "userdir"), 0o700))
	writeTestFile(t, filepath.Join(target, "userdir", "keep"), "kept as well\n", 0o644, existingTime)
	rtest.OK(t, os.Chmod(filepath.Join(target, "userdir"), 0o700))
	rtest.OK(t, os.Chtimes(filepath.Join(target, "userdir"), existingTime, existingTime))

	beforeRoot, err := os.Lstat(target)
	rtest.OK(t, err)
	beforeState := snapshotTreeState(t, target)

	ctx := t.Context()
	res := NewRestorer(repo, sn, Options{Atomic: true})
	count, err := res.RestoreTo(ctx, target)
	rtest.OK(t, err)
	rtest.Equals(t, uint64(3), count) // foo, dirtest/file and the hardlinked file group count once

	afterState := snapshotTreeState(t, target)

	// pre-existing user data is unchanged
	for _, path := range []string{"userfile", "userdir/keep", "dirtest/oldchild"} {
		got := afterState[path]
		want := beforeState[path]
		rtest.Equals(t, want.content, got.content, "content of %v", path)
		if runtime.GOOS != "windows" {
			rtest.Equals(t, want.mode, got.mode, "mode of %v", path)
			rtest.Equals(t, want.mtime, got.mtime, "mtime of %v", path)
		}
	}
	userdir := afterState["userdir"]
	rtest.Assert(t, userdir.isDir, "userdir must still be a directory")

	// restored data matches the snapshot
	rtest.Equals(t, "content: new\n", afterState["foo"].content)
	rtest.Equals(t, "content: file\n", afterState["dirtest/file"].content)
	rtest.Assert(t, afterState["link"].isSymlink, "link must be a symlink")
	rtest.Equals(t, "foo", afterState["link"].link)

	if runtime.GOOS != "windows" {
		rtest.Equals(t, os.FileMode(0o700), userdir.mode.Perm(), "userdir mode")
		rtest.Equals(t, existingTime, userdir.mtime, "userdir mtime")
		rtest.Equals(t, snapshotTime, afterState["foo"].mtime)
		rtest.Equals(t, snapshotTime, afterState["dirtest/file"].mtime)
		rtest.Equals(t, normalizeFileMode(0o750|os.ModeDir), afterState["dirtest"].mode)
		rtest.Equals(t, snapshotTime, afterState["dirtest"].mtime)

		// hard links are preserved
		rtest.Equals(t, afterState["dirtest/a"].inode, afterState["dirtest/b"].inode)
		rtest.Assert(t, afterState["dirtest/a"].inode != 0, "inodes must be recorded")

		// the target directory itself is unchanged
		afterRoot, err := os.Lstat(target)
		rtest.OK(t, err)
		rtest.Equals(t, beforeRoot.Mode(), afterRoot.Mode(), "target root mode")
		// utimes only offers microsecond granularity on some platforms
		delta := afterRoot.ModTime().Sub(beforeRoot.ModTime())
		if delta < 0 {
			delta = -delta
		}
		rtest.Assert(t, delta < 2*time.Millisecond, "target root mtime changed: %v -> %v", beforeRoot.ModTime(), afterRoot.ModTime())
	}

	assertNoAtomicLeftovers(t, tempdir)

	// restoring again must succeed and skip all files
	count, err = res.RestoreTo(ctx, target)
	rtest.OK(t, err)
	rtest.Equals(t, uint64(0), count)
	assertTreesEqual(t, afterState, snapshotTreeState(t, target))
	assertNoAtomicLeftovers(t, tempdir)

	if runtime.GOOS != "windows" {
		_, err = res.VerifyFiles(ctx, target, count, restic.NoopCounter)
		rtest.OK(t, err)
	}
}

func TestRestorerAtomicToMissingTarget(t *testing.T) {
	snapshot := Snapshot{
		Nodes: map[string]Node{
			"foo": File{Data: "content: foo\n"},
			"dir": Dir{Nodes: map[string]Node{
				"file": File{Data: "content: file\n"},
			}},
		},
	}

	repo := repository.TestRepository(t)
	sn, _ := saveSnapshot(t, repo, snapshot, noopGetGenericAttributes)

	tempdir := rtest.TempDir(t)
	target := filepath.Join(tempdir, "nested", "restore")

	res := NewRestorer(repo, sn, Options{Atomic: true})
	count, err := res.RestoreTo(t.Context(), target)
	rtest.OK(t, err)
	rtest.Equals(t, uint64(2), count)

	data, err := os.ReadFile(filepath.Join(target, "dir", "file"))
	rtest.OK(t, err)
	rtest.Equals(t, "content: file\n", string(data))
	data, err = os.ReadFile(filepath.Join(target, "foo"))
	rtest.OK(t, err)
	rtest.Equals(t, "content: foo\n", string(data))

	assertNoAtomicLeftovers(t, tempdir)
	assertNoAtomicLeftovers(t, filepath.Join(tempdir, "nested"))
}

func TestRestorerAtomicOverwriteBehavior(t *testing.T) {
	newTime := time.Date(2021, time.January, 2, 3, 4, 5, 0, time.UTC)
	oldTime := time.Date(2019, time.January, 2, 3, 4, 5, 0, time.UTC)

	snapshot := Snapshot{
		Nodes: map[string]Node{
			"newer": File{Data: "snapshot\n", ModTime: newTime},
			"older": File{Data: "snapshot\n", ModTime: oldTime},
		},
	}

	repo := repository.TestRepository(t)
	sn, _ := saveSnapshot(t, repo, snapshot, noopGetGenericAttributes)

	type expectation struct {
		newer string
		older string
	}
	for _, test := range []struct {
		overwrite OverwriteBehavior
		want      expectation
	}{
		{OverwriteNever, expectation{newer: "existing older\n", older: "existing newer\n"}},
		{OverwriteIfNewer, expectation{newer: "snapshot\n", older: "existing newer\n"}},
	} {
		t.Run(test.overwrite.String(), func(t *testing.T) {
			tempdir := rtest.TempDir(t)
			target := filepath.Join(tempdir, "target")
			rtest.OK(t, os.Mkdir(target, 0o755))
			// "newer" on disk predates the snapshot, "older" on disk is newer
			writeTestFile(t, filepath.Join(target, "newer"), "existing older\n", 0o644, oldTime)
			writeTestFile(t, filepath.Join(target, "older"), "existing newer\n", 0o644, newTime)
			writeTestFile(t, filepath.Join(target, "user"), "untouched\n", 0o644, newTime)

			res := NewRestorer(repo, sn, Options{Atomic: true, Overwrite: test.overwrite})
			_, err := res.RestoreTo(t.Context(), target)
			rtest.OK(t, err)

			checkContent := func(name, want string) {
				data, err := os.ReadFile(filepath.Join(target, name))
				rtest.OK(t, err)
				rtest.Equals(t, want, string(data), "content of %v", name)
			}
			checkContent("newer", test.want.newer)
			checkContent("older", test.want.older)
			checkContent("user", "untouched\n")
			assertNoAtomicLeftovers(t, tempdir)
		})
	}
}

func TestRestorerAtomicDelete(t *testing.T) {
	snapshotTime := time.Date(2020, time.January, 2, 3, 4, 5, 0, time.UTC)

	repo := repository.TestRepository(t)
	sn, _ := saveSnapshot(t, repo, Snapshot{
		Nodes: map[string]Node{
			"dir": Dir{
				Mode:    normalizeFileMode(0o755 | os.ModeDir),
				ModTime: snapshotTime,
				Nodes: map[string]Node{
					"file1": File{Data: "one\n", ModTime: snapshotTime},
				},
			},
			"new": File{Data: "new\n", ModTime: snapshotTime},
		},
	}, noopGetGenericAttributes)

	prepareTarget := func(t *testing.T) string {
		tempdir := rtest.TempDir(t)
		target := filepath.Join(tempdir, "target")
		rtest.OK(t, os.Mkdir(target, 0o755))
		writeTestFile(t, filepath.Join(target, "dir", "file1"), "one\n", 0o644, snapshotTime)
		writeTestFile(t, filepath.Join(target, "dir", "anotherfile"), "stale\n", 0o644, snapshotTime)
		writeTestFile(t, filepath.Join(target, "new"), "new\n", 0o644, snapshotTime)
		writeTestFile(t, filepath.Join(target, "user"), "user\n", 0o644, snapshotTime)
		return target
	}

	t.Run("delete-all", func(t *testing.T) {
		target := prepareTarget(t)
		tempdir := filepath.Dir(target)

		res := NewRestorer(repo, sn, Options{Atomic: true, Delete: true})
		_, err := res.RestoreTo(t.Context(), target)
		rtest.OK(t, err)

		state := snapshotTreeState(t, target)
		for _, existing := range []string{"dir", "dir/file1", "new"} {
			_, ok := state[existing]
			rtest.Assert(t, ok, "%v should still exist", existing)
		}
		for _, removed := range []string{"user", "dir/anotherfile"} {
			_, ok := state[removed]
			rtest.Assert(t, !ok, "%v should have been deleted", removed)
		}
		assertNoAtomicLeftovers(t, tempdir)
	})

	t.Run("delete-filtered", func(t *testing.T) {
		target := prepareTarget(t)
		tempdir := filepath.Dir(target)

		res := NewRestorer(repo, sn, Options{Atomic: true, Delete: true})
		// only touch the "dir" subtree
		res.SelectFilter = func(item string, _ bool) (bool, bool) {
			return strings.HasPrefix(item, filepath.FromSlash("/dir")), true
		}
		_, err := res.RestoreTo(t.Context(), target)
		rtest.OK(t, err)

		state := snapshotTreeState(t, target)
		// files outside of the selected subtree are neither restored nor deleted
		rtest.Equals(t, "user\n", state["user"].content)
		rtest.Equals(t, "new\n", state["new"].content)
		// stale data within the selected subtree is removed
		_, ok := state["dir/anotherfile"]
		rtest.Assert(t, !ok, "dir/anotherfile should have been deleted")
		rtest.Equals(t, "one\n", state["dir/file1"].content)
		assertNoAtomicLeftovers(t, tempdir)
	})
}

func TestRestorerAtomicFilter(t *testing.T) {
	snapshot := Snapshot{
		Nodes: map[string]Node{
			"selected": File{Data: "selected\n"},
			"ignored":  File{Data: "ignored\n"},
		},
	}

	repo := repository.TestRepository(t)
	sn, _ := saveSnapshot(t, repo, snapshot, noopGetGenericAttributes)

	tempdir := rtest.TempDir(t)
	target := filepath.Join(tempdir, "target")
	rtest.OK(t, os.Mkdir(target, 0o755))
	writeTestFile(t, filepath.Join(target, "user"), "user\n", 0o644, time.Now())

	res := NewRestorer(repo, sn, Options{Atomic: true})
	res.SelectFilter = func(item string, _ bool) (bool, bool) {
		return item == filepath.FromSlash("/selected"), false
	}
	_, err := res.RestoreTo(t.Context(), target)
	rtest.OK(t, err)

	state := snapshotTreeState(t, target)
	rtest.Equals(t, "selected\n", state["selected"].content)
	_, exists := state["ignored"]
	rtest.Assert(t, !exists, "filtered file must not be restored")
	rtest.Equals(t, "user\n", state["user"].content)
	assertNoAtomicLeftovers(t, tempdir)
}

type cancelAtProgress struct {
	cancel      context.CancelFunc
	cancelAfter int
	addFiles    int
}

func (p *cancelAtProgress) AddFile(uint64) {
	p.addFiles++
	if p.addFiles == p.cancelAfter {
		p.cancel()
	}
}
func (*cancelAtProgress) AddProgress(string, ItemAction, uint64, uint64) {}
func (*cancelAtProgress) AddSkippedFile(string, uint64)                  {}
func (*cancelAtProgress) ReportDeletion(string)                          {}

func TestRestorerAtomicCancelKeepsTarget(t *testing.T) {
	nodes := map[string]Node{}
	for i := range 30 {
		nodes[fmt.Sprintf("file%02d", i)] = File{Data: fmt.Sprintf("content of file %02d\n", i)}
	}

	repo := repository.TestRepository(t)
	sn, _ := saveSnapshot(t, repo, Snapshot{Nodes: nodes}, noopGetGenericAttributes)

	tempdir := rtest.TempDir(t)
	target := filepath.Join(tempdir, "target")
	rtest.OK(t, os.Mkdir(target, 0o755))

	existingTime := time.Date(2018, time.May, 6, 7, 8, 9, 0, time.UTC)
	writeTestFile(t, filepath.Join(target, "existing"), "do not touch\n", 0o600, existingTime)
	writeTestFile(t, filepath.Join(target, "somedir", "child"), "also do not touch\n", 0o644, existingTime)
	beforeState := snapshotTreeState(t, target)

	ctx, cancel := context.WithCancel(t.Context())
	progress := &cancelAtProgress{cancel: cancel, cancelAfter: 8}
	res := NewRestorer(repo, sn, Options{Atomic: true, Progress: progress})
	_, err := res.RestoreTo(ctx, target)
	rtest.Assert(t, err != nil, "expected restore to be aborted")

	afterState := snapshotTreeState(t, target)
	assertTreesEqual(t, beforeState, afterState)

	entries, err := os.ReadDir(tempdir)
	rtest.OK(t, err)
	for _, entry := range entries {
		if entry.Name() != "target" {
			t.Errorf("found leftover entry %v after canceled atomic restore", entry.Name())
		}
	}
}

func TestRestorerAtomicTargetIsFile(t *testing.T) {
	snapshot := Snapshot{Nodes: map[string]Node{"foo": File{Data: "foo\n"}}}
	repo := repository.TestRepository(t)
	sn, _ := saveSnapshot(t, repo, snapshot, noopGetGenericAttributes)

	tempdir := rtest.TempDir(t)
	target := filepath.Join(tempdir, "target")
	rtest.OK(t, os.WriteFile(target, []byte("not a directory"), 0o600))

	res := NewRestorer(repo, sn, Options{Atomic: true})
	_, err := res.RestoreTo(t.Context(), target)
	rtest.Assert(t, err != nil && strings.Contains(err.Error(), "cannot create target directory"),
		"unexpected error %v", err)

	data, err := os.ReadFile(target)
	rtest.OK(t, err)
	rtest.Equals(t, "not a directory", string(data))
	assertNoAtomicLeftovers(t, tempdir)
}

func TestRestorerAtomicDirectoryReplacement(t *testing.T) {
	repo := repository.TestRepository(t)
	sn, _ := saveSnapshot(t, repo, Snapshot{
		Nodes: map[string]Node{
			"foo": File{Data: "now a file\n"},
		},
	}, noopGetGenericAttributes)

	prepare := func(t *testing.T) (string, map[string]treeEntry) {
		tempdir := rtest.TempDir(t)
		target := filepath.Join(tempdir, "target")
		rtest.OK(t, os.MkdirAll(filepath.Join(target, "foo"), 0o755))
		rtest.OK(t, os.WriteFile(filepath.Join(target, "foo", "child"), []byte("contents\n"), 0o644))
		return target, snapshotTreeState(t, target)
	}

	t.Run("non-empty-dir-without-delete", func(t *testing.T) {
		target, before := prepare(t)
		res := NewRestorer(repo, sn, Options{Atomic: true})
		_, err := res.RestoreTo(t.Context(), target)
		rtest.Assert(t, err != nil, "expected an error when replacing a non-empty directory")
		assertTreesEqual(t, before, snapshotTreeState(t, target))
		assertNoAtomicLeftovers(t, filepath.Dir(target))
	})

	t.Run("non-empty-dir-with-delete", func(t *testing.T) {
		target, _ := prepare(t)
		res := NewRestorer(repo, sn, Options{Atomic: true, Delete: true})
		_, err := res.RestoreTo(t.Context(), target)
		rtest.OK(t, err)
		data, err := os.ReadFile(filepath.Join(target, "foo"))
		rtest.OK(t, err)
		rtest.Equals(t, "now a file\n", string(data))
		assertNoAtomicLeftovers(t, filepath.Dir(target))
	})
}

func TestRestorerAtomicRollbackAfterPartialMerge(t *testing.T) {
	snapshotTime := time.Date(2021, time.February, 3, 4, 5, 6, 0, time.UTC)

	repo := repository.TestRepository(t)
	sn, _ := saveSnapshot(t, repo, Snapshot{
		Nodes: map[string]Node{
			"d1": Dir{
				Mode:    normalizeFileMode(0o755 | os.ModeDir),
				ModTime: snapshotTime,
				Nodes: map[string]Node{
					"fileA": File{Data: "new A\n", ModTime: snapshotTime},
				},
			},
			// a symlink cannot replace a non-empty directory, so the merge fails
			"d2": Symlink{Target: "nowhere", ModTime: snapshotTime},
		},
	}, noopGetGenericAttributes)

	tempdir := rtest.TempDir(t)
	target := filepath.Join(tempdir, "target")
	rtest.OK(t, os.Mkdir(target, 0o755))

	dirTime := time.Date(2017, time.March, 4, 5, 6, 7, 0, time.UTC)
	rtest.OK(t, os.MkdirAll(filepath.Join(target, "d1"), 0o755))
	writeTestFile(t, filepath.Join(target, "d1", "fileA"), "old A\n", 0o644, dirTime)
	writeTestFile(t, filepath.Join(target, "d1", "untouched"), "keep A\n", 0o600, dirTime)
	rtest.OK(t, os.Chmod(filepath.Join(target, "d1"), 0o555))
	rtest.OK(t, os.Chtimes(filepath.Join(target, "d1"), dirTime, dirTime))
	rtest.OK(t, os.MkdirAll(filepath.Join(target, "d2"), 0o755))
	writeTestFile(t, filepath.Join(target, "d2", "child"), "keep B\n", 0o644, dirTime)

	beforeRoot, err := os.Lstat(target)
	rtest.OK(t, err)
	beforeState := snapshotTreeState(t, target)

	res := NewRestorer(repo, sn, Options{Atomic: true})
	_, err = res.RestoreTo(t.Context(), target)
	rtest.Assert(t, err != nil, "expected the atomic restore to fail")

	assertTreesEqual(t, beforeState, snapshotTreeState(t, target))

	if runtime.GOOS != "windows" {
		afterRoot, err := os.Lstat(target)
		rtest.OK(t, err)
		rtest.Equals(t, beforeRoot.Mode(), afterRoot.Mode(), "target root mode after rollback")
		d1, err := os.Lstat(filepath.Join(target, "d1"))
		rtest.OK(t, err)
		rtest.Equals(t, os.FileMode(0o555), d1.Mode().Perm(), "d1 mode after rollback")
		rtest.Assert(t, dirTime.Equal(d1.ModTime()), "d1 mtime after rollback: got %v", d1.ModTime())
	}
	assertNoAtomicLeftovers(t, tempdir)
}

func TestTargetDirOperationUndo(t *testing.T) {
	tempdir := rtest.TempDir(t)
	dir := filepath.Join(tempdir, "dir")
	mode := os.FileMode(0o550)
	mtime := time.Date(2016, time.January, 2, 3, 4, 5, 0, time.UTC)

	// the directory existed during the merge, with known metadata ...
	rtest.OK(t, os.Mkdir(dir, 0o700))
	rtest.OK(t, os.Chmod(dir, mode))
	rtest.OK(t, os.Chtimes(dir, mtime, mtime))
	fi, err := os.Lstat(dir)
	rtest.OK(t, err)
	op := targetDirOperation{path: dir, meta: dirMetadataOf(fi)}

	// ... it was emptied and removed, so rollback recreates it
	rtest.OK(t, os.Remove(dir))
	rtest.OK(t, op.undo(nil))

	got, err := os.Lstat(dir)
	rtest.OK(t, err)
	rtest.Equals(t, mode, got.Mode().Perm())
	rtest.Assert(t, mtime.Equal(got.ModTime()), "mtime = %v, want %v", got.ModTime(), mtime)

	// undo is also a no-op metadata restore when the directory still exists
	rtest.OK(t, os.Chmod(dir, 0o700))
	rtest.OK(t, op.undo(nil))
	got, err = os.Lstat(dir)
	rtest.OK(t, err)
	rtest.Equals(t, mode, got.Mode().Perm())
}

func TestRestorerAtomicSparse(t *testing.T) {
	var zeros [1<<20 + 13]byte

	repo := repository.TestRepository(t)
	target, err := fs.NewReader("/zeros", io.NopCloser(bytes.NewReader(zeros[:])), fs.ReaderOptions{
		Mode: 0o600,
	})
	rtest.OK(t, err)
	sc := archiver.NewScanner(target)
	rtest.OK(t, sc.Scan(context.Background(), []string{"/zeros"}))

	arch := archiver.New(repo, target, archiver.Options{})
	sn, _, _, err := arch.Snapshot(context.Background(), []string{"/zeros"}, archiver.SnapshotOptions{})
	rtest.OK(t, err)

	restoreTarget := filepath.Join(rtest.TempDir(t), "target")
	res := NewRestorer(repo, sn, Options{Atomic: true, Sparse: true})
	_, err = res.RestoreTo(t.Context(), restoreTarget)
	rtest.OK(t, err)

	content, err := os.ReadFile(filepath.Join(restoreTarget, "zeros"))
	rtest.OK(t, err)
	rtest.Equals(t, len(zeros), len(content))
	rtest.Equals(t, zeros[:], content)
}

func TestRestorerAtomicDryRun(t *testing.T) {
	snapshot := Snapshot{Nodes: map[string]Node{"foo": File{Data: "foo\n"}}}
	repo := repository.TestRepository(t)
	sn, _ := saveSnapshot(t, repo, snapshot, noopGetGenericAttributes)

	tempdir := rtest.TempDir(t)
	target := filepath.Join(tempdir, "target")

	res := NewRestorer(repo, sn, Options{Atomic: true, DryRun: true})
	_, err := res.RestoreTo(t.Context(), target)
	rtest.OK(t, err)

	_, err = os.Stat(target)
	rtest.Assert(t, os.IsNotExist(err), "no target directory should be created during a dry run, got %v", err)
	assertNoAtomicLeftovers(t, tempdir)
}
