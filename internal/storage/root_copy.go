package storage

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"syscall"
)

// CopyDataRoot copies a quiescent data root to a new or empty directory. The
// target's parent must already exist. Exclusions are literal root-level names.
// The caller must keep the source quiescent and must not move either root or
// its parent during the copy. verify must close any staged files before returning.
func CopyDataRoot(source, target string, exclude []string, verify func(stage string) error) (err error) {
	if !filepath.IsAbs(source) || !filepath.IsAbs(target) {
		return errors.New("storage: source and target must be absolute paths")
	}
	source, target = filepath.Clean(source), filepath.Clean(target)
	for _, name := range exclude {
		if !validCopyName(name) {
			return fmt.Errorf("storage: invalid root-level exclusion %q", name)
		}
	}
	if filepath.Dir(target) == target || !validCopyName(filepath.Base(target)) {
		return fmt.Errorf("storage: invalid target data root %q", target)
	}

	sourceInfo, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if !plainCopyDir(sourceInfo) {
		return fmt.Errorf("storage: source is not a plain directory: %q", source)
	}
	sourcePath, err := filepath.EvalSymlinks(source)
	if err != nil {
		return fmt.Errorf("storage: resolve source: %w", err)
	}
	src, err := os.OpenRoot(sourcePath)
	if err != nil {
		return err
	}
	defer src.Close()
	if err := checkCopyRoot(src, sourceInfo); err != nil {
		return err
	}

	parentPath, err := filepath.EvalSymlinks(filepath.Dir(target))
	if err != nil {
		return fmt.Errorf("storage: resolve target parent: %w", err)
	}
	parent, err := os.OpenRoot(parentPath)
	if err != nil {
		return err
	}
	defer parent.Close()
	parentInfo, err := parent.Stat(".")
	if err != nil {
		return err
	}
	name := filepath.Base(target)
	targetInfo, err := emptyCopyTarget(parent, name)
	if err != nil {
		return err
	}
	// Compare directory identity rather than just strings: junctions in a
	// parent path, Windows case and short names can spell the same location.
	if err := checkCopyAncestors(parentPath, sourceInfo); err != nil {
		return err
	}
	if targetInfo != nil {
		if err := checkCopyAncestors(sourcePath, targetInfo); err != nil {
			return err
		}
	}

	stageName := ".mandaryn-copy-" + rand.Text()
	if err := parent.Mkdir(stageName, 0o700); err != nil {
		return fmt.Errorf("storage: create copy stage: %w", err)
	}
	stageInfo, err := parent.Lstat(stageName)
	if err != nil {
		return err
	}
	if !plainCopyDir(stageInfo) {
		return errors.New("storage: copy stage is not a plain directory")
	}
	published := false
	defer func() {
		if published {
			return
		}
		current, statErr := parent.Lstat(stageName)
		if errors.Is(statErr, os.ErrNotExist) {
			return
		}
		if statErr != nil || !sameCopyDir(current, stageInfo) {
			err = errors.Join(err, errors.New("storage: cannot identify copy stage for cleanup"), statErr)
			return
		}
		if cleanupErr := parent.RemoveAll(stageName); cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("storage: clean copy stage: %w", cleanupErr))
		}
	}()

	stage, err := parent.OpenRoot(stageName)
	if err != nil {
		return err
	}
	if err := func() (err error) {
		defer func() { err = errors.Join(err, stage.Close()) }()
		if err := checkCopyRoot(stage, stageInfo); err != nil {
			return err
		}
		if err := copyDataDirectory(src, stage, ".", sourceInfo, exclude); err != nil {
			return err
		}
		if verify != nil {
			stagePath := filepath.Join(parentPath, stageName)
			if err := checkCopyPath(stagePath, stageInfo); err != nil {
				return err
			}
			if err := verify(stagePath); err != nil {
				return fmt.Errorf("storage: verify copy stage: %w", err)
			}
		}
		return nil
	}(); err != nil {
		return err
	}
	if err := checkCopyPath(parentPath, parentInfo); err != nil {
		return err
	}
	if current, err := parent.Lstat(stageName); err != nil || !sameCopyDir(current, stageInfo) {
		return errors.Join(errors.New("storage: copy stage changed before publication"), err)
	}
	current, err := emptyCopyTarget(parent, name)
	if err != nil {
		return err
	}
	if (targetInfo == nil && current != nil) || (targetInfo != nil && !sameCopyDir(current, targetInfo)) {
		return errors.New("storage: target changed during copy")
	}
	if err := publishDataRoot(parent, stageName, name, targetInfo); err != nil {
		return fmt.Errorf("storage: publish data root: %w", err)
	}
	published = true
	return nil
}

func validCopyName(name string) bool {
	if name == "" || name == "." || name == ".." || !filepath.IsLocal(name) || filepath.Base(name) != name || strings.ContainsRune(name, 0) {
		return false
	}
	return runtime.GOOS != "windows" || (!strings.Contains(name, ":") && strings.TrimRight(name, " .") == name)
}

func copyExcluded(name string, exclude []string) bool {
	for _, item := range exclude {
		if name == item || (runtime.GOOS == "windows" && strings.EqualFold(name, item)) {
			return true
		}
	}
	return false
}

func plainCopyDir(fi os.FileInfo) bool {
	return fi != nil && fi.Mode().Type() == os.ModeDir && !copyReparsePoint(fi)
}

func plainCopyFile(fi os.FileInfo) bool {
	return fi != nil && fi.Mode().IsRegular() && !copyReparsePoint(fi)
}

func copyReparsePoint(fi os.FileInfo) bool {
	if runtime.GOOS != "windows" {
		return false
	}
	// os.FileInfo.Sys returns *syscall.Win32FileAttributeData on Windows.
	// Inspect the attribute in this shared file: Mode alone doesn't mark every
	// reparse point (e.g. NTFS dedup files), and the struct is Windows-only.
	v := reflect.ValueOf(fi.Sys())
	if v.Kind() != reflect.Pointer || v.IsNil() || v.Elem().Kind() != reflect.Struct {
		return true
	}
	attrs := v.Elem().FieldByName("FileAttributes")
	const reparsePoint = 0x400 // FILE_ATTRIBUTE_REPARSE_POINT
	return !attrs.IsValid() || attrs.Kind() != reflect.Uint32 || attrs.Uint()&reparsePoint != 0
}

func sameCopyDir(current, expected os.FileInfo) bool {
	return plainCopyDir(current) && os.SameFile(current, expected)
}

func checkCopyRoot(root *os.Root, expected os.FileInfo) error {
	current, err := root.Stat(".")
	if err != nil {
		return err
	}
	if !sameCopyDir(current, expected) {
		return fmt.Errorf("storage: directory changed: %q", root.Name())
	}
	return nil
}

func checkCopyPath(path string, expected os.FileInfo) error {
	current, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !sameCopyDir(current, expected) {
		return fmt.Errorf("storage: directory changed: %q", path)
	}
	return nil
}

func checkCopyAncestors(path string, other os.FileInfo) error {
	for {
		fi, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !plainCopyDir(fi) {
			return fmt.Errorf("storage: ancestor is not a plain directory: %q", path)
		}
		if os.SameFile(fi, other) {
			return errors.New("storage: source and target data roots overlap")
		}
		parent := filepath.Dir(path)
		if parent == path {
			return nil
		}
		path = parent
	}
}

func emptyCopyTarget(parent *os.Root, name string) (os.FileInfo, error) {
	fi, err := parent.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !plainCopyDir(fi) {
		return nil, fmt.Errorf("storage: target is not a plain directory: %q", name)
	}
	f, err := openCopyEntry(parent, name, fi, true)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	entries, err := f.ReadDir(1)
	if len(entries) != 0 {
		return nil, fmt.Errorf("storage: target is not empty: %q", name)
	}
	if err != io.EOF {
		return nil, fmt.Errorf("storage: cannot verify target directory is empty: %q (%v)", name, err)
	}
	return fi, nil
}

func publishDataRoot(parent *os.Root, stage, name string, targetInfo os.FileInfo) error {
	if runtime.GOOS != "windows" {
		// The directory rename itself refuses to replace a nonempty directory,
		// a file or a symlink, even if it appeared after the final check.
		return parent.Rename(stage, name)
	}
	// Windows does not replace an existing directory in a rename. Use a
	// directory-only removal (never recursively), then a no-replace move.
	// A concurrent creator can win this gap but cannot be overwritten.
	to := copyWindowsLongPath(filepath.Join(parent.Name(), name))
	from := copyWindowsLongPath(filepath.Join(parent.Name(), stage))
	if targetInfo != nil {
		if err := syscall.Rmdir(to); err != nil {
			return err
		}
	}
	if err := syscall.Rename(from, to); err != nil {
		if targetInfo != nil {
			// Restore the empty target on failure when no one else claimed it.
			if restoreErr := parent.Mkdir(name, targetInfo.Mode().Perm()); restoreErr != nil && !errors.Is(restoreErr, os.ErrExist) {
				return errors.Join(err, fmt.Errorf("storage: restore empty target: %w", restoreErr))
			}
		}
		return err
	}
	return nil
}

func copyWindowsLongPath(path string) string {
	if strings.HasPrefix(path, `\\?\`) {
		return path
	}
	if rest, ok := strings.CutPrefix(path, `\\`); ok {
		return `\\?\UNC\` + rest
	}
	return `\\?\` + path
}

func openCopyEntry(root *os.Root, name string, expected os.FileInfo, directory bool) (*os.File, error) {
	match := func(fi os.FileInfo) bool {
		if directory {
			return sameCopyDir(fi, expected)
		}
		return plainCopyFile(fi) && os.SameFile(fi, expected)
	}
	flags := os.O_RDONLY
	if runtime.GOOS != "windows" {
		// Do not hang if a regular entry is replaced with a FIFO before open.
		flags |= syscall.O_NONBLOCK
	}
	f, err := root.OpenFile(name, flags, 0)
	if err != nil {
		return nil, err
	}
	// Compare the opened handle and the un-followed entry before reading.
	opened, statErr := f.Stat()
	current, lstatErr := root.Lstat(name)
	if statErr != nil || lstatErr != nil || !match(opened) || !match(current) {
		_ = f.Close()
		return nil, errors.Join(fmt.Errorf("storage: entry changed: %q", name), statErr, lstatErr)
	}
	return f, nil
}

func copyDataDirectory(src, stage *os.Root, dir string, info os.FileInfo, exclude []string) error {
	f, err := openCopyEntry(src, dir, info, true)
	if err != nil {
		return err
	}
	defer f.Close()
	for {
		entries, readErr := f.ReadDir(128)
		for _, entry := range entries {
			if dir == "." && copyExcluded(entry.Name(), exclude) {
				continue
			}
			if !validCopyName(entry.Name()) {
				return fmt.Errorf("storage: invalid source entry name %q", entry.Name())
			}
			name := filepath.Join(dir, entry.Name())
			fi, err := src.Lstat(name)
			if err != nil {
				return err
			}
			switch {
			case plainCopyDir(fi):
				if err := stage.Mkdir(name, 0o700); err != nil {
					return err
				}
				if err := copyDataDirectory(src, stage, name, fi, nil); err != nil {
					return err
				}
			case plainCopyFile(fi):
				if err := copyDataFile(src, stage, name, fi); err != nil {
					return fmt.Errorf("storage: copy %q: %w", name, err)
				}
			default:
				return fmt.Errorf("storage: unsupported source entry: %q (%s)", name, fi.Mode())
			}
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

func copyDataFile(src, stage *os.Root, name string, info os.FileInfo) error {
	in, err := openCopyEntry(src, name, info, false)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := stage.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	want := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(out, want), in)
	var syncErr error
	if copyErr == nil {
		syncErr = out.Sync()
	}
	if err := errors.Join(copyErr, syncErr, out.Close()); err != nil {
		return err
	}
	current, err := in.Stat()
	if err != nil {
		return err
	}
	if n != info.Size() || current.Size() != n || !current.ModTime().Equal(info.ModTime()) {
		return errors.New("source changed during copy")
	}
	check, err := stage.Open(name)
	if err != nil {
		return err
	}
	got := sha256.New()
	m, readErr := io.Copy(got, check)
	if err := errors.Join(readErr, check.Close()); err != nil {
		return err
	}
	if m != n || !bytes.Equal(want.Sum(nil), got.Sum(nil)) {
		return errors.New("staged copy SHA-256 mismatch")
	}
	return nil
}
