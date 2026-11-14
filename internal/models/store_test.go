package models

// 仓库域测试：扫描 / 校验 / 删除 / 统计 / 离线导入 / 按角色定位 / 注册表持久化。

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/storage"
)

// installBundle 在模型根下手工装一个包（模拟「已下载 / 已导入」状态）。
func installBundle(t *testing.T, root string, b ManifestBundle, files map[string][]byte) string {
	t.Helper()
	dir := filepath.Join(root, DirModels, bundleDirName(b.BundleID, b.Version))
	for name, data := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	if err := writeJSONAtomic(filepath.Join(dir, FileBundleManifest), b); err != nil {
		t.Fatalf("write bundle manifest: %v", err)
	}
	return dir
}

// bundleWithFiles 构造和 files 一致的包元数据。
func bundleWithFiles(id, version string, roles []string, files map[string][]byte) (ManifestBundle, map[string][]byte) {
	b := ManifestBundle{BundleID: id, Name: id, Version: version, Roles: roles, License: "MIT"}
	for name, data := range files {
		b.Files = append(b.Files, ManifestFile{
			Path:   name,
			SHA256: sha256Hex(data),
			Size:   int64(len(data)),
		})
		b.SizeBytes += int64(len(data))
	}
	return b, files
}

func TestStoreListVerifyRemove(t *testing.T) {
	root := t.TempDir()
	b, files := bundleWithFiles("sensevoice-small", "1.2.0", []string{RoleOffline},
		map[string][]byte{"model.onnx": payload(1, 32), "tokens.txt": payload(2, 8)})
	dir := installBundle(t, root, b, files)

	db, derr := storage.Open(filepath.Join(t.TempDir(), "mandaryn.db"))
	if derr != nil {
		t.Fatalf("open db: %v", derr)
	}
	defer db.Close()

	opts := testOptions(root, "http://127.0.0.1:1")
	opts.Holder = storage.NewHolder(db)
	m, _ := newTestManager(t, opts)

	list, err := m.ListInstalled(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("installed = %d, want 1", len(list))
	}
	if list[0].State != contract.BundleInstalled {
		t.Fatalf("state = %s, want installed", list[0].State)
	}
	// SizeBytes 是**磁盘占用**（含包内 manifest.json），因此比净负载略大。
	if list[0].SizeBytes < int64(40) {
		t.Fatalf("sizeBytes = %d, want >= 40 (payload size)", list[0].SizeBytes)
	}
	if list[0].Path != filepath.ToSlash(filepath.Join(DirModels, "sensevoice-small@1.2.0")) {
		t.Fatalf("path = %s", list[0].Path)
	}

	// 篡改一个文件 → 校验必须发现并标记 corrupt。
	if err := os.WriteFile(filepath.Join(dir, "model.onnx"), []byte("tampered"), 0o644); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	res, err := m.Verify(context.Background(), "sensevoice-small")
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if res.OK {
		t.Fatal("verify reported OK for a tampered bundle")
	}
	if len(res.BadFiles) != 1 || res.BadFiles[0] != "model.onnx" {
		t.Fatalf("badFiles = %v, want [model.onnx]", res.BadFiles)
	}

	list, _ = m.ListInstalled(context.Background())
	if list[0].State != contract.BundleCorrupt {
		t.Fatalf("state after verify = %s, want corrupt", list[0].State)
	}

	// 修复后应重新验证通过。
	if err := os.WriteFile(filepath.Join(dir, "model.onnx"), files["model.onnx"], 0o644); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if res, err = m.Verify(context.Background(), "sensevoice-small"); err != nil || !res.OK {
		t.Fatalf("verify after restore = %+v, %v", res, err)
	}

	// Stats 应统计到占用（先刷新列表，拿当前磁盘占用作对照）。
	list, _ = m.ListInstalled(context.Background())
	stats, err := m.Stats(context.Background())
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.TotalBytes != list[0].SizeBytes || stats.Root != root {
		t.Fatalf("stats = %+v (bundle size %d)", stats, list[0].SizeBytes)
	}

	// 删除后列表为空。
	if err := m.Remove(context.Background(), "sensevoice-small"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	list, _ = m.ListInstalled(context.Background())
	if len(list) != 0 {
		t.Fatalf("installed after remove = %+v", list)
	}
	// 再删一次必须是 NOT_FOUND。
	if err := m.Remove(context.Background(), "sensevoice-small"); !apierr.HasCode(err, apierr.CodeModelNotFound) {
		t.Fatalf("second remove error = %v, want %s", err, apierr.CodeModelNotFound)
	}
}

func TestLocateBundleByRole(t *testing.T) {
	root := t.TempDir()
	off, offFiles := bundleWithFiles("sensevoice-small", "1.2.0", []string{RoleOffline},
		map[string][]byte{"model.onnx": payload(1, 16)})
	installBundle(t, root, off, offFiles)
	str, strFiles := bundleWithFiles("zipformer-zh-streaming", "1.0.3", []string{RoleStreaming},
		map[string][]byte{"model.onnx": payload(2, 16)})
	installBundle(t, root, str, strFiles)

	m, _ := newTestManager(t, testOptions(root, "http://127.0.0.1:1"))

	dir, id, err := m.LocateBundle(RoleOffline)
	if err != nil {
		t.Fatalf("locate offline: %v", err)
	}
	if id != "sensevoice-small" {
		t.Fatalf("bundleID = %s", id)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "model.onnx")); statErr != nil {
		t.Fatalf("located dir does not contain the model: %v", statErr)
	}

	if _, id, err = m.LocateBundle(RoleStreaming); err != nil || id != "zipformer-zh-streaming" {
		t.Fatalf("locate streaming = %s, %v", id, err)
	}
	if _, _, err = m.LocateBundle(RoleVAD); !apierr.HasCode(err, apierr.CodeModelNotFound) {
		t.Fatalf("locate vad error = %v, want %s", err, apierr.CodeModelNotFound)
	}
}

func TestCleanupRemovesStaleStaging(t *testing.T) {
	root := t.TempDir()
	stale := filepath.Join(root, DirStagingPrefix+"deadbeef")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	m, _ := newTestManager(t, testOptions(root, "http://127.0.0.1:1"))
	m.Cleanup(context.Background())
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale staging not removed: %v", err)
	}
}

// —— 离线导入 ——

// makeZip 构造一个离线包 zip。
func makeZip(t *testing.T, path, topDir string, b ManifestBundle, files map[string][]byte) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create zip: %v", err)
	}
	zw := zip.NewWriter(f)
	add := func(name string, data []byte) {
		w, werr := zw.Create(name)
		if werr != nil {
			t.Fatalf("zip create %s: %v", name, werr)
		}
		if _, werr = w.Write(data); werr != nil {
			t.Fatalf("zip write %s: %v", name, werr)
		}
	}
	manifestBytes, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	add(topDir+"/"+FileBundleManifest, manifestBytes)
	for name, data := range files {
		add(topDir+"/"+name, data)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close file: %v", err)
	}
}

// makeTarGz 构造一个离线包 tar.gz。
func makeTarGz(t *testing.T, path, topDir string, b ManifestBundle, files map[string][]byte) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create tar.gz: %v", err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)

	writeEntry := func(name string, data []byte) {
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("tar header %s: %v", name, err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatalf("tar write %s: %v", name, err)
		}
	}
	manifestBytes, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	writeEntry(topDir+"/"+FileBundleManifest, manifestBytes)
	for name, data := range files {
		writeEntry(topDir+"/"+name, data)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close file: %v", err)
	}
}

func TestImportOfflineZip(t *testing.T) {
	root := t.TempDir()
	b, files := bundleWithFiles("sensevoice-small", "1.2.0", []string{RoleOffline},
		map[string][]byte{"model.onnx": payload(5, 24)})
	archive := filepath.Join(t.TempDir(), "bundle.zip")
	makeZip(t, archive, "sensevoice-small-1.2.0", b, files)

	m, _ := newTestManager(t, testOptions(root, "http://127.0.0.1:1"))
	installed, err := m.ImportOffline(context.Background(), archive)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if installed.BundleID != "sensevoice-small" || installed.State != contract.BundleInstalled {
		t.Fatalf("installed = %+v", installed)
	}
	dir := filepath.Join(root, DirModels, "sensevoice-small@1.2.0")
	got, err := os.ReadFile(filepath.Join(dir, "model.onnx"))
	if err != nil {
		t.Fatalf("read imported file: %v", err)
	}
	if string(got) != string(files["model.onnx"]) {
		t.Fatal("imported content mismatch")
	}
	assertNoStaging(t, root)
}

func TestImportOfflineTarGz(t *testing.T) {
	root := t.TempDir()
	b, files := bundleWithFiles("silero-vad", "2.0.0", []string{RoleVAD},
		map[string][]byte{"vad.onnx": payload(6, 12)})
	archive := filepath.Join(t.TempDir(), "bundle.tar.gz")
	makeTarGz(t, archive, "silero-vad-2.0.0", b, files)

	m, _ := newTestManager(t, testOptions(root, "http://127.0.0.1:1"))
	if _, err := m.ImportOffline(context.Background(), archive); err != nil {
		t.Fatalf("import tar.gz: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, DirModels, "silero-vad@2.0.0", "vad.onnx")); err != nil {
		t.Fatalf("imported file missing: %v", err)
	}
}

func TestImportOfflineRejectsCorruptArchive(t *testing.T) {
	root := t.TempDir()
	b, files := bundleWithFiles("3dspeaker-zh", "1.1.0", []string{RoleSpeaker},
		map[string][]byte{"spk.onnx": payload(7, 20)})
	// 摘要与内容不一致 → 必须拒绝入库。
	b.Files[0].SHA256 = sha256Hex([]byte("something else"))
	archive := filepath.Join(t.TempDir(), "corrupt.zip")
	makeZip(t, archive, "3dspeaker-zh-1.1.0", b, files)

	m, _ := newTestManager(t, testOptions(root, "http://127.0.0.1:1"))
	_, err := m.ImportOffline(context.Background(), archive)
	if !apierr.HasCode(err, apierr.CodeModelCorrupt) {
		t.Fatalf("error = %v, want %s", err, apierr.CodeModelCorrupt)
	}
	if entries, _ := os.ReadDir(filepath.Join(root, DirModels)); len(entries) != 0 {
		t.Fatalf("corrupt bundle must not be installed: %v", entries)
	}
}

func TestImportOfflineRejectsUnsupportedArchive(t *testing.T) {
	root := t.TempDir()
	archive := filepath.Join(t.TempDir(), "bundle.rar")
	if err := os.WriteFile(archive, []byte("not an archive"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	m, _ := newTestManager(t, testOptions(root, "http://127.0.0.1:1"))
	if _, err := m.ImportOffline(context.Background(), archive); !apierr.HasCode(err, apierr.CodeInvalidArgument) {
		t.Fatalf("error = %v, want %s", err, apierr.CodeInvalidArgument)
	}
}

func TestImportOfflineRejectsZipSlip(t *testing.T) {
	root := t.TempDir()
	archive := filepath.Join(t.TempDir(), "slip.zip")
	f, err := os.Create(archive)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	zw := zip.NewWriter(f)
	if _, err := zw.Create("../../escaped.txt"); err != nil {
		t.Fatalf("zip create: %v", err)
	}
	_ = zw.Close()
	_ = f.Close()

	m, _ := newTestManager(t, testOptions(root, "http://127.0.0.1:1"))
	if _, err := m.ImportOffline(context.Background(), archive); !apierr.HasCode(err, apierr.CodeModelCorrupt) {
		t.Fatalf("error = %v, want %s", err, apierr.CodeModelCorrupt)
	}
	if _, statErr := os.Stat(filepath.Join(filepath.Dir(root), "escaped.txt")); statErr == nil {
		t.Fatal("zip slip escaped the model root")
	}
}

// TestRegistryPersistedWithDB 覆盖 TBL-08 / TBL-09 的落库（真实 SQLite）。
func TestRegistryPersistedWithDB(t *testing.T) {
	srv := newRangeServer(t)
	root := t.TempDir()
	db, err := storage.Open(filepath.Join(t.TempDir(), "mandaryn.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	holder := storage.NewHolder(db)

	bundle := newBundle(srv, "sensevoice-small", "1.2.0", []string{RoleOffline},
		map[string][]byte{"model.onnx": payload(3, 24)})
	srv.setCatalog(t, bundle)

	opts := testOptions(root, srv.URL)
	opts.Holder = holder
	m, rec := newTestManager(t, opts)

	if _, err := m.Download(context.Background(), "sensevoice-small"); err != nil {
		t.Fatalf("download: %v", err)
	}
	if st := rec.waitDownload(t, "sensevoice-small"); st.State != contract.DownloadDone {
		t.Fatalf("state = %s (%+v)", st.State, st.LastError)
	}

	var rows []storage.Bundle
	if err := db.GORM.Find(&rows).Error; err != nil {
		t.Fatalf("query bundles: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("bundles rows = %d, want 1", len(rows))
	}
	row := rows[0]
	if row.ID != "sensevoice-small@1.2.0" || row.BundleID != "sensevoice-small" || row.Version != "1.2.0" {
		t.Fatalf("bundle row = %+v", row)
	}
	if row.State != contract.BundleInstalled {
		t.Fatalf("state = %s, want installed", row.State)
	}
	if row.Path != "models/sensevoice-small@1.2.0" {
		t.Fatalf("path = %s", row.Path)
	}
	if row.SizeBytes != 24 || row.VerifiedAt == 0 {
		t.Fatalf("size/verifiedAt = %d/%d", row.SizeBytes, row.VerifiedAt)
	}

	var downloads []storage.Download
	if err := db.GORM.Find(&downloads).Error; err != nil {
		t.Fatalf("query downloads: %v", err)
	}
	if len(downloads) != 1 {
		t.Fatalf("downloads rows = %d, want 1", len(downloads))
	}
	if downloads[0].State != contract.DownloadDone {
		t.Fatalf("download row state = %s", downloads[0].State)
	}

	// 删除同时清理注册表。
	if err := m.Remove(context.Background(), "sensevoice-small"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	rows = nil
	if err := db.GORM.Find(&rows).Error; err != nil {
		t.Fatalf("query bundles: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("bundles after remove = %+v", rows)
	}
}

// TestModTimeMillis 只是把 modTimeMillis 的边界写清楚（目录不存在 → 0）。
func TestModTimeMillis(t *testing.T) {
	if got := modTimeMillis(filepath.Join(t.TempDir(), "nope")); got != 0 {
		t.Fatalf("modTimeMillis(missing) = %d, want 0", got)
	}
	dir := t.TempDir()
	if got := modTimeMillis(dir); got == 0 || got > time.Now().UnixMilli()+1000 {
		t.Fatalf("modTimeMillis(dir) = %d", got)
	}
}
