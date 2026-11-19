package models

// 清单（catalog）测试：远程失败回退缓存 / 内置兜底、缓存落盘、已装标记。

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
)

func TestCatalogFallsBackToBuiltin(t *testing.T) {
	srv := newRangeServer(t)
	root := t.TempDir()

	bundle := newBundle(srv, "sensevoice-small", "1.2.0", []string{RoleOffline},
		map[string][]byte{"model.onnx": payload(1, 16)})
	srv.setCatalog(t, bundle)
	srv.breakCatalog()

	m := newRawManager(t, testOptions(root, srv.URL))

	list, err := m.ListAvailable(context.Background())
	if !apierr.HasCode(err, apierr.CodeDownloadNetwork) {
		t.Fatalf("error = %v, want %s", err, apierr.CodeDownloadNetwork)
	}
	if len(list) != len(builtinManifest().Bundles) {
		t.Fatalf("builtin list = %d bundles, want %d", len(list), len(builtinManifest().Bundles))
	}
	// 内置条目不允许被误当成「可下载」。
	if list[0].BundleID == "" || list[0].Installed {
		t.Fatalf("builtin entry = %+v", list[0])
	}
	// 内置兜底清单只有元数据、没有 URL：必须如实标记 Downloadable=false，
	// 否则前端会渲染一个点了必然失败的「下载」按钮。见 contract.BundleInfo.Downloadable。
	for _, it := range list {
		if it.Downloadable {
			t.Fatalf("builtin entry %q marked downloadable, want false", it.BundleID)
		}
	}

	// SetCatalog 恢复远端，但被动读取在 TTL 内仍返回同一份兜底及告警。
	srv.setCatalog(t, bundle)
	list2, err2 := m.ListAvailable(context.Background())
	if !apierr.HasCode(err2, apierr.CodeDownloadNetwork) || len(list2) != len(list) {
		t.Fatalf("cached fallback = %+v, %v", list2, err2)
	}
	got, err := m.RefreshAvailable(context.Background())
	if err != nil || len(got) != 1 || got[0].BundleID != bundle.BundleID || !got[0].Downloadable {
		t.Fatalf("explicit refresh = %+v, %v", got, err)
	}
}

func TestCatalogCacheWrittenAndReused(t *testing.T) {
	srv := newRangeServer(t)
	root := t.TempDir()
	bundle := newBundle(srv, "zipformer-zh-streaming", "1.0.3", []string{RoleStreaming},
		map[string][]byte{"model.onnx": payload(2, 16)})
	srv.setCatalog(t, bundle)

	m := newRawManager(t, testOptions(root, srv.URL))
	if _, err := m.Fetch(context.Background()); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	cachePath := filepath.Join(root, FileManifestCache)
	if _, err := os.Stat(cachePath); err != nil {
		t.Fatalf("cache file not written: %v", err)
	}

	// 新实例 + 远端故障 → 必须回退到缓存，并且内容一致。
	srv.breakCatalog()
	brokenURL := srv.URL
	m2 := newRawManager(t, testOptions(root, brokenURL))
	list, err := m2.ListAvailable(context.Background())
	if !apierr.HasCode(err, apierr.CodeDownloadNetwork) {
		t.Fatalf("error = %v, want %s", err, apierr.CodeDownloadNetwork)
	}
	if len(list) != 1 || list[0].BundleID != "zipformer-zh-streaming" {
		t.Fatalf("cached list = %+v", list)
	}
}

func TestListAvailableMarksInstalledAndUpdate(t *testing.T) {
	srv := newRangeServer(t)
	root := t.TempDir()

	// 已装 1.2.0，清单里是 1.3.0 → Installed=true 且 UpdateAvailable=true。
	installed, files := bundleWithFiles("sensevoice-small", "1.2.0", []string{RoleOffline},
		map[string][]byte{"model.onnx": payload(3, 16)})
	installBundle(t, root, installed, files)

	newer := newBundle(srv, "sensevoice-small", "1.3.0", []string{RoleOffline},
		map[string][]byte{"model.onnx": payload(4, 16)})
	other := newBundle(srv, "silero-vad", "2.0.0", []string{RoleVAD},
		map[string][]byte{"vad.onnx": payload(5, 8)})
	srv.setCatalog(t, newer, other)

	m := newRawManager(t, testOptions(root, srv.URL))
	list, err := m.ListAvailable(context.Background())
	if err != nil {
		t.Fatalf("list available: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("available = %d, want 2", len(list))
	}
	if !list[0].Installed || !list[0].UpdateAvailable {
		t.Fatalf("sensevoice flags = installed:%v update:%v", list[0].Installed, list[0].UpdateAvailable)
	}
	if list[1].Installed || list[1].UpdateAvailable {
		t.Fatalf("vad flags = installed:%v update:%v", list[1].Installed, list[1].UpdateAvailable)
	}
	if list[0].SizeBytes != 16 {
		t.Fatalf("sizeBytes = %d, want 16", list[0].SizeBytes)
	}
	// 远端清单里的条目带 URL 与 sha256 → 必须标记为可下载（前端据此启用按钮）。
	for _, it := range list {
		if !it.Downloadable {
			t.Fatalf("entry %q from served catalog marked not downloadable, want true", it.BundleID)
		}
	}
}

func TestFetchRejectsBadManifest(t *testing.T) {
	srv := newRangeServer(t)
	root := t.TempDir()

	cases := map[string]string{
		"empty":      `{"schemaVersion":1,"bundles":[]}`,
		"no-version": `{"schemaVersion":1,"bundles":[{"bundleId":"x"}]}`,
		"bad-schema": `{"schemaVersion":99,"bundles":[{"bundleId":"x","version":"1"}]}`,
		"not-json":   `not json at all`,
	}
	for name, body := range cases {
		srv.setFile(FileCatalog, &testFile{Data: []byte(body), ETag: `"c"`})
		m := newRawManager(t, testOptions(root, srv.URL))
		// 清单无效 → 回退内置兜底并报告网络类错误（不 panic、不返回空列表）。
		list, err := m.ListAvailable(context.Background())
		if len(list) == 0 {
			t.Fatalf("%s: empty catalog leaked to caller", name)
		}
		if err == nil {
			t.Fatalf("%s: expected a fetch error", name)
		}
	}
}

func TestCatalogHonoursOfflineSwitch(t *testing.T) {
	srv := newRangeServer(t)
	root := t.TempDir()
	bundle := newBundle(srv, "sensevoice-small", "1.2.0", []string{RoleOffline},
		map[string][]byte{"model.onnx": payload(1, 16)})
	srv.setCatalog(t, bundle)

	t.Setenv("MANDARYN_OFFLINE", "1")
	m := newRawManager(t, testOptions(root, srv.URL))

	list, err := m.ListAvailable(context.Background())
	if !apierr.HasCode(err, apierr.CodeOfflineEnforced) {
		t.Fatalf("error = %v, want %s", err, apierr.CodeOfflineEnforced)
	}
	if len(list) == 0 {
		t.Fatal("offline must still yield the built-in catalog")
	}
	// 离线时不允许下载。
	if _, derr := m.Download(context.Background(), "sensevoice-small"); !apierr.HasCode(derr, apierr.CodeOfflineEnforced) {
		t.Fatalf("download error = %v, want %s", derr, apierr.CodeOfflineEnforced)
	}
	// 服务端不应收到任何请求。
	if reqs := srv.takeRequests(); len(reqs) != 0 {
		t.Fatalf("offline mode issued outbound requests: %v", reqs)
	}
}

func TestBuiltinManifestIsSane(t *testing.T) {
	mf := builtinManifest()
	if mf.SchemaVersion != manifestSchemaVersion {
		t.Fatalf("schema = %d", mf.SchemaVersion)
	}
	roles := map[string]bool{}
	for _, b := range mf.Bundles {
		if b.BundleID == "" || b.Version == "" || len(b.Roles) == 0 {
			t.Fatalf("incomplete builtin bundle: %+v", b)
		}
		for _, r := range b.Roles {
			roles[r] = true
		}
		// 兜底清单不带 URL：断网时必须表现为「不可下载」而不是「假装能下」。
		for _, f := range b.Files {
			if f.URL != "" {
				t.Fatalf("builtin bundle %s has a download url", b.BundleID)
			}
		}
	}
	for _, want := range []string{RoleOffline, RoleStreaming, RoleVAD, RoleSpeaker} {
		if !roles[want] {
			t.Fatalf("builtin catalog lacks role %s", want)
		}
	}
}

func TestCatalogURLResolution(t *testing.T) {
	cases := map[string]string{
		"https://host/dir":             "https://host/dir/" + FileCatalog,
		"https://host/dir/":            "https://host/dir/" + FileCatalog,
		"https://host/dir/models.json": "https://host/dir/models.json",
		"":                             "",
	}
	for in, want := range cases {
		if got := catalogURL(in); got != want {
			t.Errorf("catalogURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCatalogDirectURLAndBundlePreflight(t *testing.T) {
	srv := newRangeServer(t)
	bundle := newBundle(srv, "good", "1.0.0", []string{RoleOffline},
		map[string][]byte{"model.onnx": payload(1, 16)})
	bundle.Files[0].URL = "model.onnx"
	broken := bundle
	broken.BundleID = "broken"
	broken.Files = append(append([]ManifestFile(nil), bundle.Files...), ManifestFile{
		Path: "tokens.txt", URL: "tokens.txt", Size: 16,
	})
	srv.setCatalog(t, bundle, broken)
	m := newRawManager(t, testOptions(t.TempDir(), srv.URL+"/"+FileCatalog+"?v=1"))
	ctx := context.Background()
	got, err := m.ListAvailable(ctx)
	if err != nil || len(got) != 2 || !got[0].Downloadable || got[1].Downloadable {
		t.Fatalf("available = %+v, %v", got, err)
	}
	if mf := m.Catalog(ctx); mf.Bundles[0].Files[0].URL != srv.URL+"/model.onnx" {
		t.Fatalf("resolved URL = %q", mf.Bundles[0].Files[0].URL)
	}
	if _, err := m.StartDownload(ctx, broken.BundleID); !apierr.HasCode(err, apierr.CodeDownloadChecksum) {
		t.Fatalf("expected preflight to reject missing later-file checksum, got %v", err)
	}
}

func TestStarterCatalogUsesPinnedSourcesWithoutFetching(t *testing.T) {
	srv := newRangeServer(t)
	opts := testOptions(t.TempDir(), srv.URL)
	opts.UseStarterCatalog = true
	m := newRawManager(t, opts)
	list, err := m.RefreshAvailable(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range list {
		want := b.BundleID == "sensevoice-small" || b.BundleID == "silero-vad"
		if b.Downloadable != want {
			t.Fatalf("unexpected starter bundle: %+v", b)
		}
	}
	if len(list) != 4 || len(srv.takeRequests()) != 0 {
		t.Fatalf("starter list = %+v; want four entries and no requests", list)
	}
}

func TestListAvailableFallsBackAfterTTL(t *testing.T) {
	srv := newRangeServer(t)
	root := t.TempDir()
	bundle := newBundle(srv, "sensevoice-small", "1.2.0", []string{RoleOffline},
		map[string][]byte{"model.onnx": payload(1, 16)})
	srv.setCatalog(t, bundle)

	opts := testOptions(root, srv.URL)
	// 极短 TTL：兜底缓存过期后必须重新尝试远端。
	opts.CatalogTTL = 20 * time.Millisecond
	m := newRawManager(t, opts)

	if _, err := m.Fetch(context.Background()); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	srv.breakCatalog()
	time.Sleep(30 * time.Millisecond)

	list, err := m.ListAvailable(context.Background())
	if !apierr.HasCode(err, apierr.CodeDownloadNetwork) {
		t.Fatalf("error = %v, want %s after TTL", err, apierr.CodeDownloadNetwork)
	}
	if len(list) != 1 {
		t.Fatalf("cached list = %+v", list)
	}
	_ = contract.BundleInstalled
}
