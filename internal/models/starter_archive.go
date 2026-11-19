package models

import (
	"archive/tar"
	"compress/bzip2"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// These constants identify the only remote archive that is allowed to use the
// built-in starter-model extraction path. They are also used by the catalog.
const (
	SenseVoiceINT8ArchivePath   = "sensevoice-int8.tar.bz2"
	SenseVoiceINT8ArchiveSHA256 = "7d1efa2138a65b0b488df37f8b89e3d91a60676e416f515b952358d83dfd347e"

	senseVoiceStarterBundleID = "sensevoice-small"
	senseVoiceStarterVersion  = "1.2.0"

	maxSenseVoiceArchiveBytes   int64 = 512 << 20
	maxSenseVoiceExpandedBytes  int64 = (512 << 20) + (1 << 20)
	maxSenseVoiceModelBytes     int64 = 512 << 20
	maxSenseVoiceArchiveEntries       = 4096
)

var senseVoiceStarterFiles = [...]string{"model.int8.onnx", "tokens.txt"}

func isSenseVoiceStarterBundle(bundle ManifestBundle) bool {
	return bundle.BundleID == senseVoiceStarterBundleID && bundle.Version == senseVoiceStarterVersion
}

// isApprovedSenseVoiceStarterArchive deliberately accepts no remote extraction
// rules: identity, one archive file, its fixed path, and its compiled digest
// must all match before the special extraction path is enabled.
func isApprovedSenseVoiceStarterArchive(bundle ManifestBundle) bool {
	if !isSenseVoiceStarterBundle(bundle) || len(bundle.Files) != 1 {
		return false
	}
	file := bundle.Files[0]
	return file.Path == SenseVoiceINT8ArchivePath &&
		strings.EqualFold(file.SHA256, SenseVoiceINT8ArchiveSHA256) &&
		file.Size >= 0 && file.Size <= maxSenseVoiceArchiveBytes
}

// extractSenseVoiceStarterArchive inflates the bounded bzip2 stream to a
// private staging file, then extracts only the two expected regular files.
func extractSenseVoiceStarterArchive(ctx context.Context, archivePath, stagingDir string) ([]ManifestFile, int64, error) {
	info, err := os.Lstat(archivePath)
	if err != nil {
		return nil, 0, err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxSenseVoiceArchiveBytes {
		return nil, 0, fmt.Errorf("models: starter archive is not a bounded regular file")
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}

	compressed, err := os.Open(archivePath)
	if err != nil {
		return nil, 0, err
	}
	defer compressed.Close()

	tarFile, err := os.CreateTemp(stagingDir, ".sensevoice-expanded-*.tar")
	if err != nil {
		return nil, 0, err
	}
	tarPath := tarFile.Name()
	defer os.Remove(tarPath)

	expandedSize, copyErr := copyContextBounded(ctx, tarFile, bzip2.NewReader(compressed), maxSenseVoiceExpandedBytes)
	if copyErr == nil {
		copyErr = tarFile.Sync()
	}
	closeErr := tarFile.Close()
	if copyErr != nil {
		return nil, 0, fmt.Errorf("models: decompress starter archive: %w", copyErr)
	}
	if closeErr != nil {
		return nil, 0, closeErr
	}

	tarFile, err = os.Open(tarPath)
	if err != nil {
		return nil, 0, err
	}
	defer tarFile.Close()
	return extractSenseVoiceStarterTar(ctx, tarFile, expandedSize, stagingDir)
}

// extractSenseVoiceStarterTar separates tar validation from bzip2 so malformed
// archives can be covered by small stdlib-only unit fixtures.
func extractSenseVoiceStarterTar(ctx context.Context, source io.ReaderAt, sourceSize int64, stagingDir string) ([]ManifestFile, int64, error) {
	if sourceSize < 1024 || sourceSize%512 != 0 || sourceSize > maxSenseVoiceExpandedBytes {
		return nil, 0, fmt.Errorf("models: invalid or truncated starter tar size")
	}
	countedTar := &starterTarCountingReader{reader: io.NewSectionReader(source, 0, sourceSize)}
	tarReader := tar.NewReader(countedTar)
	seenEntries := make(map[string]bool, 16)
	files := make(map[string]ManifestFile, len(senseVoiceStarterFiles)+1)
	root := ""
	var extractedBytes int64
	var archiveContentBytes int64
	entryCount := 0

	for {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		// Next consumes the preceding entry's padding and both EOF blocks.
		endStart := (countedTar.bytesRead + 511) &^ 511
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			if err := validateSenseVoiceTarEnd(source, sourceSize, endStart); err != nil {
				return nil, 0, err
			}
			break
		}
		if err != nil {
			return nil, 0, fmt.Errorf("models: read starter tar entry: %w", err)
		}
		entryCount++
		if entryCount > maxSenseVoiceArchiveEntries {
			return nil, 0, fmt.Errorf("models: starter tar has too many entries")
		}

		name, err := cleanSenseVoiceTarPath(header.Name, header.Typeflag == tar.TypeDir)
		if err != nil {
			return nil, 0, err
		}
		parts := strings.Split(name, "/")
		if root == "" {
			root = parts[0]
		} else if root != parts[0] {
			return nil, 0, fmt.Errorf("models: starter tar contains more than one top-level directory")
		}

		isDir := header.Typeflag == tar.TypeDir
		if err := registerSenseVoiceTarEntry(seenEntries, name, isDir); err != nil {
			return nil, 0, err
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if header.Size != 0 {
				return nil, 0, fmt.Errorf("models: starter tar directory has content")
			}
		case tar.TypeReg, tar.TypeRegA:
			if header.Size < 0 || header.Size > maxSenseVoiceModelBytes || archiveContentBytes > maxSenseVoiceModelBytes-header.Size {
				return nil, 0, fmt.Errorf("models: starter archive contents exceed the size limit")
			}
			archiveContentBytes += header.Size

			base := parts[len(parts)-1]
			preserve := len(parts) == 2 && (isExpectedSenseVoiceFile(base) || base == "LICENSE")
			if preserve {
				if isExpectedSenseVoiceFile(base) && header.Size == 0 {
					return nil, 0, fmt.Errorf("models: required starter model file %q is empty", base)
				}
				dest := filepath.Join(stagingDir, base)
				out, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
				if err != nil {
					return nil, 0, fmt.Errorf("models: create starter file %q: %w", base, err)
				}
				hash := sha256.New()
				written, writeErr := copyExactContext(ctx, io.MultiWriter(out, hash), tarReader, header.Size)
				if writeErr == nil && written != header.Size {
					writeErr = io.ErrUnexpectedEOF
				}
				if writeErr == nil {
					writeErr = out.Sync()
				}
				closeErr := out.Close()
				if writeErr != nil {
					return nil, 0, fmt.Errorf("models: extract starter file %q: %w", base, writeErr)
				}
				if closeErr != nil {
					return nil, 0, closeErr
				}
				extractedBytes += written
				files[base] = ManifestFile{
					Path:   base,
					SHA256: hex.EncodeToString(hash.Sum(nil)),
					Size:   written,
				}
			} else if _, err := copyExactContext(ctx, io.Discard, tarReader, header.Size); err != nil {
				return nil, 0, fmt.Errorf("models: skip starter archive member %q: %w", name, err)
			}
		default:
			return nil, 0, fmt.Errorf("models: starter tar contains unsupported entry type %d", header.Typeflag)
		}
	}

	if root == "" {
		return nil, 0, fmt.Errorf("models: starter tar has no top-level directory")
	}
	manifestFiles := make([]ManifestFile, 0, len(senseVoiceStarterFiles)+1)
	for _, name := range senseVoiceStarterFiles {
		file, ok := files[name]
		if !ok {
			return nil, 0, fmt.Errorf("models: starter tar is missing %q", name)
		}
		manifestFiles = append(manifestFiles, file)
	}
	if license, ok := files["LICENSE"]; ok {
		manifestFiles = append(manifestFiles, license)
	}
	return manifestFiles, extractedBytes, nil
}

type starterTarCountingReader struct {
	reader    io.Reader
	bytesRead int64
}

func (r *starterTarCountingReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.bytesRead += int64(n)
	return n, err
}

func registerSenseVoiceTarEntry(seen map[string]bool, name string, isDir bool) error {
	if _, exists := seen[name]; exists {
		return fmt.Errorf("models: duplicate starter tar destination %q", name)
	}
	for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
		if parentIsDir, exists := seen[parent]; exists && !parentIsDir {
			return fmt.Errorf("models: starter tar path %q is below a regular file", name)
		}
	}
	if !isDir {
		prefix := name + "/"
		for existing := range seen {
			if strings.HasPrefix(existing, prefix) {
				return fmt.Errorf("models: starter tar file %q conflicts with a child path", name)
			}
		}
	}
	seen[name] = isDir
	return nil
}

func validateSenseVoiceTarEnd(source io.ReaderAt, sourceSize, endStart int64) error {
	if endStart < 512 || endStart > sourceSize || sourceSize-endStart < 1024 {
		return fmt.Errorf("models: starter tar is missing end markers or is truncated")
	}
	buf := make([]byte, 32<<10)
	for offset := endStart; offset < sourceSize; {
		count := int64(len(buf))
		if remaining := sourceSize - offset; remaining < count {
			count = remaining
		}
		n, err := source.ReadAt(buf[:int(count)], offset)
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("models: read starter tar end markers: %w", err)
		}
		if int64(n) != count {
			return fmt.Errorf("models: truncated starter tar end markers")
		}
		for _, b := range buf[:n] {
			if b != 0 {
				return fmt.Errorf("models: starter tar has non-zero data after its model files")
			}
		}
		offset += count
	}
	return nil
}

func cleanSenseVoiceTarPath(name string, isDir bool) (string, error) {
	if name == "" || strings.ContainsRune(name, 0) || strings.Contains(name, "\\") || strings.HasPrefix(name, "/") || path.IsAbs(name) {
		return "", fmt.Errorf("models: unsafe starter tar path")
	}
	if isDir && strings.HasSuffix(name, "/") {
		name = strings.TrimSuffix(name, "/")
	}
	if name == "" || strings.HasSuffix(name, "/") {
		return "", fmt.Errorf("models: unsafe starter tar path")
	}
	parts := strings.Split(name, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.Contains(part, ":") {
			return "", fmt.Errorf("models: unsafe starter tar path")
		}
	}
	if path.Clean(name) != name {
		return "", fmt.Errorf("models: non-canonical starter tar path")
	}
	return name, nil
}

func isExpectedSenseVoiceFile(name string) bool {
	for _, expected := range senseVoiceStarterFiles {
		if name == expected {
			return true
		}
	}
	return false
}

func copyContextBounded(ctx context.Context, dst io.Writer, src io.Reader, limit int64) (int64, error) {
	buf := make([]byte, 64<<10)
	var total int64
	emptyReads := 0
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, readErr := src.Read(buf)
		if n > 0 {
			emptyReads = 0
			if int64(n) > limit-total {
				return total, fmt.Errorf("models: expanded starter archive exceeds the size limit")
			}
			written, writeErr := dst.Write(buf[:n])
			if writeErr != nil {
				return total, writeErr
			}
			if written != n {
				return total, io.ErrShortWrite
			}
			total += int64(n)
		} else if readErr == nil {
			emptyReads++
			if emptyReads >= 100 {
				return total, io.ErrNoProgress
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return total, nil
			}
			return total, readErr
		}
	}
}

func copyExactContext(ctx context.Context, dst io.Writer, src io.Reader, size int64) (int64, error) {
	if size < 0 {
		return 0, fmt.Errorf("models: negative starter tar entry size")
	}
	buf := make([]byte, 64<<10)
	var total int64
	for total < size {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		want := int64(len(buf))
		if left := size - total; left < want {
			want = left
		}
		n, readErr := src.Read(buf[:int(want)])
		if n > 0 {
			written, writeErr := dst.Write(buf[:n])
			if writeErr != nil {
				return total, writeErr
			}
			if written != n {
				return total, io.ErrShortWrite
			}
			total += int64(n)
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				if total == size {
					return total, nil
				}
				return total, io.ErrUnexpectedEOF
			}
			return total, readErr
		}
	}
	return total, nil
}

// commitSenseVoiceStarterDir keeps an existing installed version available if
// the final directory rename fails, and only retires it after the new tree lands.
func commitSenseVoiceStarterDir(source, target string) error {
	parent := filepath.Dir(target)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}

	backup := ""
	if _, err := os.Lstat(target); err == nil {
		backup = filepath.Join(parent, fmt.Sprintf("%s%s-%s", DirTrashPrefix, filepath.Base(target), newTempID()))
		if err := os.Rename(target, backup); err != nil {
			return fmt.Errorf("models: preserve existing starter bundle: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	if err := os.Rename(source, target); err != nil {
		if backup != "" {
			if restoreErr := os.Rename(backup, target); restoreErr != nil {
				return errors.Join(fmt.Errorf("models: commit starter bundle: %w", err), fmt.Errorf("models: restore previous starter bundle: %w", restoreErr))
			}
		}
		return fmt.Errorf("models: commit starter bundle: %w", err)
	}
	if backup != "" {
		go func() { _ = os.RemoveAll(backup) }()
	}
	return nil
}
