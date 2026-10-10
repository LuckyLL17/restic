package restorer

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/restic/restic/internal/debug"
	"github.com/restic/restic/internal/errors"
	"github.com/restic/restic/internal/fs"
)

// Temporary staging/trash directories are created as hidden siblings of the
// restore target. This guarantees that they reside on the same filesystem and
// therefore allows the final switch to be implemented using renames.
const (
	atomicStagingDirPrefix = ".restic-restore-"
	atomicTrashDirPrefix   = ".restic-restore-old-"
)

// prepareAtomicStage validates the target directory and creates an empty
// staging directory on the same filesystem as the target. rootMeta describes
// the target directory itself and must be restored onto the staging directory
// before it is switched into place.
func (res *Restorer) prepareAtomicStage(dst string) (stage string, targetExisted bool, rootMeta dirMetadata, err error) {
	if filepath.Clean(dst) == string(filepath.Separator) {
		return "", false, dirMetadata{}, errors.New("refusing to atomically restore into the root directory")
	}

	fi, err := fs.Lstat(dst)
	switch {
	case err == nil:
		if !fi.IsDir() {
			return "", false, dirMetadata{}, fmt.Errorf("cannot create target directory: %w", os.ErrExist)
		}
		targetExisted = true
		rootMeta = dirMetadataOf(fi)
	case !errors.Is(err, os.ErrNotExist):
		return "", false, dirMetadata{}, fmt.Errorf("cannot access target directory: %w", err)
	}

	parent := filepath.Dir(dst)
	if err := fs.MkdirAll(parent, 0o700); err != nil {
		return "", false, dirMetadata{}, fmt.Errorf("cannot create target directory: %w", err)
	}

	stage, err = os.MkdirTemp(parent, atomicStagingDirPrefix+"*")
	if err != nil {
		return "", false, dirMetadata{}, fmt.Errorf("cannot create staging directory: %w", err)
	}
	return stage, targetExisted, rootMeta, nil
}

// seedFromTarget hardlinks (or recreates, for symlinks) an unchanged item from
// the final target path into the staging directory. The regular restore logic
// then runs against the staging directory, which makes the overwrite and
// content verification checks reach exactly the same decisions as during an
// in-place restore. Items created by the restore itself are never seeded.
//
// A seeded regular file shares its inode with the target. The inode is detached
// again by detachSeededFiles before any metadata is restored, so a failed
// atomic restore can never leave modified metadata behind in the target.
func (res *Restorer) seedFromTarget(stagingPath string) error {
	if res.atomicStage == "" {
		return nil
	}

	if _, err := fs.Lstat(stagingPath); err == nil {
		// the restore already created the item
		return nil
	}

	rel, err := filepath.Rel(res.atomicStage, stagingPath)
	if err != nil {
		return err
	}
	targetPath := filepath.Join(res.atomicTarget, rel)

	fi, err := fs.Lstat(targetPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		// fall back to restoring the item from scratch
		debug.Log("failed to inspect %v for atomic restore seeding: %v", targetPath, err)
		return nil
	}

	switch {
	case fi.Mode().IsRegular():
		if err := fs.Link(targetPath, stagingPath); err != nil {
			debug.Log("failed to hardlink %v for atomic restore seeding: %v", targetPath, err)
		}
	case fi.Mode()&os.ModeSymlink != 0:
		linkTarget, err := os.Readlink(targetPath)
		if err != nil {
			debug.Log("failed to read symlink %v for atomic restore seeding: %v", targetPath, err)
			return nil
		}
		if err := os.Symlink(linkTarget, stagingPath); err != nil {
			debug.Log("failed to recreate symlink %v for atomic restore seeding: %v", targetPath, err)
		}
	}
	return nil
}

// detachSeededFiles replaces regular files in the staging directory that still
// share their inode with the target and only need their metadata updated by a
// private copy. Restored files already use a fresh inode and do not need any
// special handling.
func (res *Restorer) detachSeededFiles() error {
	if res.atomicStage == "" {
		return nil
	}

	for location, metadataOnly := range res.fileList {
		if !metadataOnly {
			continue
		}

		stagePath := filepath.Join(res.atomicStage, location)
		targetPath := filepath.Join(res.atomicTarget, location)
		stageInfo, err := os.Lstat(stagePath)
		if err != nil {
			continue
		}
		targetInfo, err := os.Lstat(targetPath)
		if err != nil {
			continue
		}
		if !stageInfo.Mode().IsRegular() || !os.SameFile(stageInfo, targetInfo) {
			continue
		}

		if err := copyRegularFile(targetPath, stagePath); err != nil {
			return fmt.Errorf("failed to stage file %v: %w", location, err)
		}
	}
	return nil
}

// copyRegularFile replaces dst with a private copy of src. The replacement is
// done via a temporary file in the same directory followed by a rename.
func copyRegularFile(src, dst string) error {
	in, err := fs.OpenFile(src, fs.O_RDONLY|fs.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}

	dir := filepath.Dir(dst)
	tmp, err := os.CreateTemp(dir, atomicStagingDirPrefix+"copy-*")
	if err != nil {
		_ = in.Close()
		return err
	}
	tmpName := tmp.Name()

	cleanup := func() {
		_ = tmp.Close()
		_ = fs.Remove(tmpName)
	}

	if _, err := io.Copy(tmp, in); err != nil {
		_ = in.Close()
		cleanup()
		return err
	}
	if err := in.Close(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = fs.Remove(tmpName)
		return err
	}

	if err := os.Rename(tmpName, dst); err != nil {
		_ = fs.Remove(tmpName)
		return err
	}
	return nil
}

// stagingProgress rewrites absolute paths reported by the restore logic from
// the staging directory to the final target directory.
type stagingProgress struct {
	ProgressReporter
	stage  string
	target string
}

func (p *stagingProgress) ReportDeletion(name string) {
	if rel, err := filepath.Rel(p.stage, name); err == nil && !filepath.IsAbs(rel) && rel != "." {
		name = filepath.Join(p.target, rel)
	}
	p.ProgressReporter.ReportDeletion(name)
}

// dirMetadata records the metadata of a directory that must be reapplied after
// the merge into the staging directory is complete.
type dirMetadata struct {
	mode  os.FileMode
	atime time.Time
	mtime time.Time
}

func dirMetadataOf(fi os.FileInfo) dirMetadata {
	meta := dirMetadata{
		mode:  fi.Mode(),
		mtime: fi.ModTime(),
		atime: fi.ModTime(),
	}
	if ex := fs.ExtendedStat(fi); ex != nil {
		meta.atime = ex.AccessTime
	}
	return meta
}

// atomicSwitch merges the completely restored staging directory with the
// existing target directory and then switches them into place. All operations
// except the final rename are journaled and reversed on error or cancellation,
// leaving the target in its original state.
type atomicSwitch struct {
	ctx    context.Context
	stage  string
	target string
	trash  string

	// deleteExpected maps snapshot locations to the names the snapshot
	// contains at that location. Entries of the target that are missing from
	// the staging directory and this list are moved to the trash directory
	// when --delete is active and the select filter selects them.
	deleteExpected       map[string][]string
	allowRecursiveDelete bool
	selectFilter         func(item string, isDir bool) (selectedForRestore bool, childMayBeSelected bool)
	warn                 func(message string)

	ops []switchOperation
}

type switchOperation interface {
	undo(s *atomicSwitch) error
}

type moveOperation struct{ from, to string }

// targetDirOperation records the original metadata of a target directory
// participating in the merge. It is journaled before any entry of the
// directory is moved and therefore restored last during a rollback, once all
// entries are back in place.
type targetDirOperation struct {
	path string
	meta dirMetadata
}

// mkdirOperation recreates a target directory that was emptied and removed
// during the merge. It must be journaled after the directory removal so that
// rollback recreates the (writable) directory before moving its entries back.
type mkdirOperation struct{ path string }

func (op moveOperation) undo(_ *atomicSwitch) error { return os.Rename(op.to, op.from) }

func (op targetDirOperation) undo(_ *atomicSwitch) error {
	if _, err := os.Lstat(op.path); errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(op.path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
	}
	return applyDirMetadata(op.path, op.meta)
}

func (op mkdirOperation) undo(_ *atomicSwitch) error {
	err := os.Mkdir(op.path, 0o700)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	return err
}

func (s *atomicSwitch) record(op switchOperation) {
	s.ops = append(s.ops, op)
}

// commit performs the switch. It returns an error and leaves the target
// untouched if any step fails.
func (s *atomicSwitch) commit(targetExisted bool, rootMeta dirMetadata) (err error) {
	stagedDirMetadata, err := s.makeStagingDirsWritable()
	if err != nil {
		return err
	}

	s.trash, err = os.MkdirTemp(filepath.Dir(s.target), atomicTrashDirPrefix+"*")
	if err != nil {
		return fmt.Errorf("cannot create trash directory: %w", err)
	}

	committed := false
	defer func() {
		if !committed {
			if rerr := s.rollback(); rerr != nil {
				debug.Log("failed to roll back atomic restore: %v", rerr)
			}
		}
	}()

	if targetExisted {
		if err := s.mergeLevel(s.stage, s.target, string(filepath.Separator), s.trash); err != nil {
			return err
		}

		// the target directory is now empty and can be replaced by the
		// completely restored staging directory
		if err := os.Remove(s.target); err != nil {
			return fmt.Errorf("cannot replace target directory: %w", err)
		}
		// journal the recreation before any merged entries are moved back
		s.record(mkdirOperation{path: s.target})
	}

	if err := s.applyStagedDirMetadata(stagedDirMetadata, targetExisted, rootMeta); err != nil {
		return err
	}

	if err := os.Rename(s.stage, s.target); err != nil {
		return fmt.Errorf("cannot switch restored directory into place: %w", err)
	}
	committed = true

	if err := fs.RemoveAll(s.trash); err != nil && s.warn != nil {
		s.warn(fmt.Sprintf("failed to remove old data in %v: %v", s.trash, err))
	}
	return nil
}

// rollback reverses all journaled merge operations to restore the original
// target directory.
func (s *atomicSwitch) rollback() error {
	var firstErr error
	for i := len(s.ops) - 1; i >= 0; i-- {
		if err := s.ops[i].undo(s); err != nil {
			debug.Log("rollback failed: %v", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// mergeLevel moves all entries of an existing target directory into the
// restored staging directory. Entries missing from staging are preserved,
// entries replaced by the restore are moved to the trash directory and
// directories present in both are merged recursively.
func (s *atomicSwitch) mergeLevel(stageDir, targetDir, location, trashDir string) error {
	if err := s.ctx.Err(); err != nil {
		return err
	}

	// remember the directory metadata and make it writable so that its
	// entries can be moved during the merge
	dirInfo, err := os.Lstat(targetDir)
	if err != nil {
		return err
	}
	s.record(targetDirOperation{path: targetDir, meta: dirMetadataOf(dirInfo)})
	if dirInfo.Mode().Perm() != 0o700 {
		if err := os.Chmod(targetDir, 0o700); err != nil {
			return err
		}
	}

	names, err := fs.Readdirnames(fs.NewLocal(), targetDir, fs.O_NOFOLLOW)
	if err != nil {
		return err
	}

	expected, tracked := s.deleteExpected[location]
	keep := make(map[string]struct{}, len(expected))
	for _, name := range expected {
		keep[toComparableFilename(name)] = struct{}{}
	}

	for _, name := range names {
		if err := s.ctx.Err(); err != nil {
			return err
		}

		from := filepath.Join(targetDir, name)
		stagePath := filepath.Join(stageDir, name)

		targetInfo, err := os.Lstat(from)
		if err != nil {
			return err
		}
		stageInfo, err := os.Lstat(stagePath)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		existsInStaging := err == nil

		if !existsInStaging {
			var to string
			if tracked && s.shouldDelete(location, name, keep) {
				if err := fs.MkdirAll(trashDir, 0o700); err != nil {
					return err
				}
				to = filepath.Join(trashDir, name)
			} else {
				to = stagePath
			}

			if err := os.Rename(from, to); err != nil {
				return err
			}
			s.record(moveOperation{from: from, to: to})
			continue
		}

		if targetInfo.IsDir() && stageInfo.IsDir() {
			childTrash := filepath.Join(trashDir, name)
			if err := s.mergeLevel(stagePath, from, filepath.Join(location, name), childTrash); err != nil {
				return err
			}
			if err := os.Remove(from); err != nil {
				return err
			}
			// recreate the writable directory before the child moves are
			// rolled back; the metadata is restored by the directory's own
			// operation later
			s.record(mkdirOperation{path: from})
			continue
		}

		if targetInfo.IsDir() {
			// a non-directory snapshot item replaces a target directory. An
			// in-place restore can only remove a non-empty directory when
			// restoring a regular file with --delete enabled; refuse
			// otherwise and roll back
			entries, err := fs.Readdirnames(fs.NewLocal(), from, fs.O_NOFOLLOW)
			if err != nil {
				return err
			}
			if len(entries) > 0 && !(s.allowRecursiveDelete && stageInfo.Mode().IsRegular()) {
				return fmt.Errorf("cannot replace directory %v with non-directory %v", from, stagePath)
			}
		}

		if err := fs.MkdirAll(trashDir, 0o700); err != nil {
			return err
		}
		to := filepath.Join(trashDir, name)
		if err := os.Rename(from, to); err != nil {
			return err
		}
		s.record(moveOperation{from: from, to: to})
	}

	return nil
}

// shouldDelete mirrors the decision of Restorer.removeUnexpectedFiles.
func (s *atomicSwitch) shouldDelete(location, name string, expected map[string]struct{}) bool {
	if _, ok := expected[toComparableFilename(name)]; ok {
		return false
	}

	nodeTarget := filepath.Join(location, name)
	if filepath.Clean(nodeTarget) == name || !fs.HasPathPrefix(s.target, filepath.Join(s.target, name)) {
		return false
	}

	selectedForRestore, _ := s.selectFilter(nodeTarget, false)
	return selectedForRestore
}

// makeStagingDirsWritable ensures that all directories in the staging tree can
// be modified during the merge and remembers their current metadata so that it
// can be restored afterwards. Directories are traversed top-down to be able to
// enter directories without write permissions.
func (s *atomicSwitch) makeStagingDirsWritable() (map[string]dirMetadata, error) {
	metadata := make(map[string]dirMetadata)
	var prepare func(path string) error
	prepare = func(path string) error {
		fi, err := os.Lstat(path)
		if err != nil {
			return err
		}
		metadata[path] = dirMetadataOf(fi)

		if fi.Mode().Perm() != 0o700 {
			if err := os.Chmod(path, 0o700); err != nil {
				return err
			}
		}

		entries, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			if err := prepare(filepath.Join(path, entry.Name())); err != nil {
				return err
			}
		}
		return nil
	}

	if err := prepare(s.stage); err != nil {
		return nil, fmt.Errorf("cannot prepare staging directory: %w", err)
	}
	return metadata, nil
}

// applyStagedDirMetadata restores the directory permissions and timestamps that
// were in effect before the merge. The staging root itself receives the
// metadata of the target directory.
func (s *atomicSwitch) applyStagedDirMetadata(staged map[string]dirMetadata, targetExisted bool, rootMeta dirMetadata) error {
	for path, meta := range staged {
		if path == s.stage {
			continue
		}
		if err := applyDirMetadata(path, meta); err != nil {
			return fmt.Errorf("cannot restore metadata of %v: %w", path, err)
		}
	}

	if targetExisted {
		if err := applyDirMetadata(s.stage, rootMeta); err != nil {
			return fmt.Errorf("cannot restore metadata of target directory: %w", err)
		}
	}
	return nil
}

func applyDirMetadata(path string, meta dirMetadata) error {
	if err := os.Chmod(path, meta.mode.Perm()); err != nil {
		return err
	}
	if err := os.Chtimes(path, meta.atime, meta.mtime); err != nil {
		return err
	}
	return nil
}
