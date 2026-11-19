package models

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type starterTarEntry struct {
	header tar.Header
	data   []byte
}

func makeStarterTar(t *testing.T, entries ...starterTarEntry) []byte {
	t.Helper()
	var out bytes.Buffer
	writer := tar.NewWriter(&out)
	for _, entry := range entries {
		header := entry.header
		if header.Typeflag == 0 {
			header.Typeflag = tar.TypeReg
		}
		if header.Mode == 0 {
			header.Mode = 0o644
		}
		if header.Typeflag == tar.TypeReg || header.Typeflag == tar.TypeRegA {
			header.Size = int64(len(entry.data))
		}
		if err := writer.WriteHeader(&header); err != nil {
			t.Fatalf("write tar header: %v", err)
		}
		if len(entry.data) > 0 {
			if _, err := writer.Write(entry.data); err != nil {
				t.Fatalf("write tar data: %v", err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}
	return out.Bytes()
}

func starterFilesTar(t *testing.T) []byte {
	t.Helper()
	return makeStarterTar(t,
		starterTarEntry{header: tar.Header{Name: "sensevoice-int8", Typeflag: tar.TypeDir}},
		starterTarEntry{header: tar.Header{Name: "sensevoice-int8/README.md"}, data: []byte("readme")},
		starterTarEntry{header: tar.Header{Name: "sensevoice-int8/test_wavs", Typeflag: tar.TypeDir}},
		starterTarEntry{header: tar.Header{Name: "sensevoice-int8/test_wavs/zh.wav"}, data: []byte("audio fixture")},
		starterTarEntry{header: tar.Header{Name: "sensevoice-int8/tokens.txt"}, data: []byte("token list\n")},
		starterTarEntry{header: tar.Header{Name: "sensevoice-int8/model.int8.onnx"}, data: []byte("onnx-model")},
		starterTarEntry{header: tar.Header{Name: "sensevoice-int8/LICENSE"}, data: []byte("license text")},
	)
}

func TestExtractSenseVoiceStarterTarWritesVerifiedManifestFiles(t *testing.T) {
	tarData := starterFilesTar(t)
	stage := t.TempDir()
	files, total, err := extractSenseVoiceStarterTar(context.Background(), bytes.NewReader(tarData), int64(len(tarData)), stage)
	if err != nil {
		t.Fatalf("extract valid starter tar: %v", err)
	}
	wantTotal := int64(len("onnx-model") + len("token list\n") + len("license text"))
	if len(files) != 3 || total != wantTotal {
		t.Fatalf("files/size = %d/%d, want 3/%d", len(files), total, wantTotal)
	}
	for _, file := range files {
		got, err := os.ReadFile(filepath.Join(stage, file.Path))
		if err != nil {
			t.Fatalf("read extracted %s: %v", file.Path, err)
		}
		if int64(len(got)) != file.Size || file.SHA256 != sha256Hex(got) {
			t.Errorf("manifest for %s = size %d sha %s, want size %d sha %s", file.Path, file.Size, file.SHA256, len(got), sha256Hex(got))
		}
	}
	for _, skipped := range []string{"README.md", filepath.Join("test_wavs", "zh.wav")} {
		if _, err := os.Stat(filepath.Join(stage, skipped)); !os.IsNotExist(err) {
			t.Errorf("non-model archive member %q should not be installed, stat err=%v", skipped, err)
		}
	}
}

func TestExtractSenseVoiceStarterTarRejectsMalformedAndIncompleteArchives(t *testing.T) {
	valid := starterFilesTar(t)
	truncated := append([]byte(nil), valid[:len(valid)-512]...)
	invalidHeader := make([]byte, 1536)
	for i := 0; i < 512; i++ {
		invalidHeader[i] = 'x'
	}
	missingFile := makeStarterTar(t,
		starterTarEntry{header: tar.Header{Name: "sensevoice-int8/model.int8.onnx"}, data: []byte("onnx-model")},
	)

	cases := []struct {
		name string
		data []byte
	}{
		{name: "bad format", data: invalidHeader},
		{name: "missing required file", data: missingFile},
		{name: "truncated trailer", data: truncated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := extractSenseVoiceStarterTar(context.Background(), bytes.NewReader(tc.data), int64(len(tc.data)), t.TempDir())
			if err == nil {
				t.Fatal("expected archive rejection")
			}
		})
	}
}

func TestExtractSenseVoiceStarterTarRejectsUnsafeAndLinkedEntries(t *testing.T) {
	cases := []struct {
		name    string
		entries []starterTarEntry
	}{
		{
			name: "path traversal",
			entries: []starterTarEntry{
				{header: tar.Header{Name: "sensevoice-int8/../model.int8.onnx"}, data: []byte("bad")},
			},
		},
		{
			name: "symlink",
			entries: []starterTarEntry{
				{header: tar.Header{Name: "sensevoice-int8/model.int8.onnx", Typeflag: tar.TypeSymlink, Linkname: "outside"}},
			},
		},
		{
			name: "duplicate destination",
			entries: []starterTarEntry{
				{header: tar.Header{Name: "sensevoice-int8/model.int8.onnx"}, data: []byte("first")},
				{header: tar.Header{Name: "sensevoice-int8/model.int8.onnx"}, data: []byte("second")},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tarData := makeStarterTar(t, tc.entries...)
			stage := t.TempDir()
			_, _, err := extractSenseVoiceStarterTar(context.Background(), bytes.NewReader(tarData), int64(len(tarData)), stage)
			if err == nil {
				t.Fatal("expected archive rejection")
			}
			entries, readErr := os.ReadDir(stage)
			if readErr != nil {
				t.Fatal(readErr)
			}
			for _, entry := range entries {
				if strings.Contains(entry.Name(), "..") {
					t.Fatalf("unsafe path escaped into staging: %s", entry.Name())
				}
			}
		})
	}
}

func TestSenseVoiceArchiveApprovalIsPinnedToIdentityPathAndDigest(t *testing.T) {
	approved := ManifestBundle{
		BundleID: senseVoiceStarterBundleID,
		Version:  senseVoiceStarterVersion,
		Files: []ManifestFile{{
			Path:   SenseVoiceINT8ArchivePath,
			SHA256: SenseVoiceINT8ArchiveSHA256,
			Size:   1,
		}},
	}
	if !isApprovedSenseVoiceStarterArchive(approved) {
		t.Fatal("expected pinned starter archive to be approved")
	}
	mutations := []func(*ManifestBundle){
		func(b *ManifestBundle) { b.BundleID = "other" },
		func(b *ManifestBundle) { b.Version = "1.2.1" },
		func(b *ManifestBundle) { b.Files[0].Path = "custom.tar.bz2" },
		func(b *ManifestBundle) { b.Files[0].SHA256 = strings.Repeat("0", 64) },
		func(b *ManifestBundle) { b.Files = append(b.Files, ManifestFile{Path: "extra"}) },
		func(b *ManifestBundle) { b.Files[0].Size = maxSenseVoiceArchiveBytes + 1 },
	}
	for i, mutate := range mutations {
		candidate := approved
		candidate.Files = append([]ManifestFile(nil), approved.Files...)
		mutate(&candidate)
		if isApprovedSenseVoiceStarterArchive(candidate) {
			t.Errorf("mutation %d unexpectedly approved", i)
		}
	}
}

func TestCommitSenseVoiceStarterDirRestoresExistingTargetOnFailure(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "sensevoice-small@1.2.0")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	oldFile := filepath.Join(target, "model.int8.onnx")
	if err := os.WriteFile(oldFile, []byte("old-version"), 0o644); err != nil {
		t.Fatal(err)
	}

	missingSource := filepath.Join(parent, "missing-staging")
	if err := commitSenseVoiceStarterDir(missingSource, target); err == nil {
		t.Fatal("expected commit failure")
	}
	got, err := os.ReadFile(oldFile)
	if err != nil {
		t.Fatalf("old installed file was not restored: %v", err)
	}
	if string(got) != "old-version" {
		t.Fatalf("old installed contents changed: %q", got)
	}
}
