package models

// 分片下载的行为测试：全部离线，用 httptest 精确构造协议细节。

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
)

// payload 返回一段可预测的内容（长度 40 → 16 字节分片下正好 3 片）。
func payload(seed byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = seed + byte(i)
	}
	return out
}

func TestDownloadWritesFilesAndVerifiesSHA(t *testing.T) {
	srv := newRangeServer(t)
	root := t.TempDir()

	files := map[string][]byte{
		"model.onnx": payload(1, 40),
		"tokens.txt": payload(2, 20),
	}
	bundle := newBundle(srv, "sensevoice-small", "1.2.0", []string{RoleOffline}, files)
	srv.setCatalog(t, bundle)

	m, rec := newTestManager(t, testOptions(root, srv.URL))

	if _, err := m.Download(t.Context(), "sensevoice-small"); err != nil {
		t.Fatalf("start download: %v", err)
	}
	st := rec.waitDownload(t, "sensevoice-small")
	if st.State != contract.DownloadDone {
		t.Fatalf("state = %s, want done (lastError=%+v)", st.State, st.LastError)
	}

	dir := filepath.Join(root, DirModels, "sensevoice-small@1.2.0")
	for name, want := range files {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if string(got) != string(want) {
			t.Fatalf("%s content mismatch: got %d bytes, want %d", name, len(got), len(want))
		}
		if sum, _, err := sha256File(filepath.Join(dir, name)); err != nil || !strings.EqualFold(sum, sha256Hex(want)) {
			t.Fatalf("%s sha256 = %s (%v), want %s", name, sum, err, sha256Hex(want))
		}
	}
	// 包内必须落 manifest.json
	if _, err := os.Stat(filepath.Join(dir, FileBundleManifest)); err != nil {
		t.Fatalf("bundle manifest missing: %v", err)
	}
	// 断点文件与分片目录不得残留
	if _, err := os.Stat(filepath.Join(root, DirDownloads, "sensevoice-small@1.2.0.state.json")); !os.IsNotExist(err) {
		t.Fatalf("state file should be removed, stat err = %v", err)
	}
	// 不允许 .staging 残留
	assertNoStaging(t, root)

	installed, err := m.ListInstalled(t.Context())
	if err != nil {
		t.Fatalf("list installed: %v", err)
	}
	if len(installed) != 1 || installed[0].State != contract.BundleInstalled {
		t.Fatalf("installed = %+v", installed)
	}
}

// assertNoStaging 断言目录下没有 .staging-* 残留。
func assertNoStaging(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), DirStagingPrefix) {
			t.Fatalf("leftover staging dir: %s", e.Name())
		}
	}
}

func TestDownloadResumeOnlyRemainingChunks(t *testing.T) {
	srv := newRangeServer(t)
	root := t.TempDir()

	data := payload(10, 40) // chunk0=0-15, chunk1=16-31, chunk2=32-39
	bundle := newBundle(srv, "zipformer-zh-streaming", "1.0.3", []string{RoleStreaming},
		map[string][]byte{"model.onnx": data})
	srv.setCatalog(t, bundle)

	// 第一次：第 1 个请求正常，之后断连 → 只下完 chunk0。
	srv.mu.Lock()
	srv.files["model.onnx"].AbortAfter = 1
	srv.mu.Unlock()

	m, rec := newTestManager(t, testOptions(root, srv.URL))
	if _, err := m.Download(t.Context(), "zipformer-zh-streaming"); err != nil {
		t.Fatalf("start download: %v", err)
	}
	st := rec.waitDownload(t, "zipformer-zh-streaming")
	if st.State != contract.DownloadFailed {
		t.Fatalf("first session state = %s, want failed", st.State)
	}

	// 断点必须保留 chunk0 的完成记录。
	sf := m.loadStateFile(filepath.Join(root, DirDownloads, "zipformer-zh-streaming@1.0.3.state.json"))
	if sf == nil {
		t.Fatal("state file not persisted after interruption")
	}
	fp := sf.file("model.onnx")
	if len(fp.PartsDone) != 1 || fp.PartsDone[0] != 0 {
		t.Fatalf("partsDone = %v, want [0]", fp.PartsDone)
	}

	// 第二次：恢复并断言**只请求剩余分片**。
	srv.heal()
	srv.takeRequests()
	rec.resetDownload("zipformer-zh-streaming")

	if _, err := m.Download(t.Context(), "zipformer-zh-streaming"); err != nil {
		t.Fatalf("resume download: %v", err)
	}
	st = rec.waitDownload(t, "zipformer-zh-streaming")
	if st.State != contract.DownloadDone {
		t.Fatalf("resume state = %s, want done (%+v)", st.State, st.LastError)
	}

	reqs := srv.takeRequests()
	var gotRanges []string
	for _, r := range reqs {
		parts := strings.SplitN(r, "|", 3)
		if len(parts) == 3 && parts[0] == "model.onnx" {
			gotRanges = append(gotRanges, parts[1])
		}
	}
	if len(gotRanges) == 0 {
		t.Fatal("resume issued no range requests")
	}
	if gotRanges[0] != "bytes=16-31" {
		t.Fatalf("first resumed range = %q, want bytes=16-31 (chunk0 must not be re-requested)", gotRanges[0])
	}
	for _, r := range gotRanges {
		if r == "bytes=0-15" {
			t.Fatalf("resume re-requested chunk0: %v", gotRanges)
		}
	}

	got, err := os.ReadFile(filepath.Join(root, DirModels, "zipformer-zh-streaming@1.0.3", "model.onnx"))
	if err != nil {
		t.Fatalf("read model: %v", err)
	}
	if string(got) != string(data) {
		t.Fatal("resumed content mismatch")
	}
}

func TestDownloadETagChangedRestartsWholeFile(t *testing.T) {
	srv := newRangeServer(t)
	root := t.TempDir()

	oldData := payload(20, 40)
	bundle := newBundle(srv, "3dspeaker-zh", "1.1.0", []string{RoleSpeaker},
		map[string][]byte{"spk.onnx": oldData})
	srv.setCatalog(t, bundle)

	// 第一次：下完 chunk0 后断连。
	srv.mu.Lock()
	srv.files["spk.onnx"].AbortAfter = 1
	srv.mu.Unlock()

	m, rec := newTestManager(t, testOptions(root, srv.URL))
	if _, err := m.Download(t.Context(), "3dspeaker-zh"); err != nil {
		t.Fatalf("start: %v", err)
	}
	if st := rec.waitDownload(t, "3dspeaker-zh"); st.State != contract.DownloadFailed {
		t.Fatalf("first session state = %s", st.State)
	}

	// 远端内容与 ETag 都变了：必须整包重下，不能与旧分片拼接。
	newData := payload(90, 40)
	srv.heal()
	srv.setData("spk.onnx", newData, `"spk.onnx-v2"`)
	// 清单里的摘要也要跟着变（否则会被 sha256 复核拦下，测不到 ETag 分支）。
	bundle.Files[0].SHA256 = sha256Hex(newData)
	bundle.Files[0].Size = int64(len(newData))
	srv.setCatalog(t, bundle)

	// 模拟「应用重启后续传」：内存里的旧清单（旧 sha256）必须由新清单取代，
	// 否则会拿旧摘要去校验新内容，测不出 ETag 分支。
	m2, rec2 := newTestManager(t, testOptions(root, srv.URL))
	srv.takeRequests()

	if _, err := m2.Download(t.Context(), "3dspeaker-zh"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	st := rec2.waitDownload(t, "3dspeaker-zh")
	if st.State != contract.DownloadDone {
		t.Fatalf("state = %s, want done (%+v)", st.State, st.LastError)
	}

	got, err := os.ReadFile(filepath.Join(root, DirModels, "3dspeaker-zh@1.1.0", "spk.onnx"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(newData) {
		t.Fatalf("content was spliced instead of re-downloaded: got %x", got[:min(8, len(got))])
	}

	// 断言确实先带旧 ETag 续传（服务端回整包），随后从头重下。
	var sawIfRange, sawRestart bool
	for _, r := range srv.takeRequests() {
		parts := strings.SplitN(r, "|", 3)
		if len(parts) != 3 || parts[0] != "spk.onnx" {
			continue
		}
		if parts[2] == `"spk.onnx-v1"` {
			sawIfRange = true
		}
		if parts[1] == "bytes=0-15" {
			sawRestart = true
		}
	}
	if !sawIfRange {
		t.Error("expected an If-Range request carrying the old ETag")
	}
	if !sawRestart {
		t.Error("expected a restart request from byte 0")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestDownloadChecksumMismatchRetriesThenFails(t *testing.T) {
	srv := newRangeServer(t)
	root := t.TempDir()

	data := payload(30, 40)
	bundle := newBundle(srv, "silero-vad", "2.0.0", []string{RoleVAD}, map[string][]byte{"vad.onnx": data})
	srv.setCatalog(t, bundle)

	// 传输内容被篡改：长度对、内容错 → 只能靠 sha256 发现。
	srv.mu.Lock()
	srv.files["vad.onnx"].Corrupt = true
	srv.mu.Unlock()

	m, rec := newTestManager(t, testOptions(root, srv.URL))
	if _, err := m.Download(t.Context(), "silero-vad"); err != nil {
		t.Fatalf("start: %v", err)
	}
	st := rec.waitDownload(t, "silero-vad")
	if st.State != contract.DownloadFailed {
		t.Fatalf("state = %s, want failed", st.State)
	}
	if st.LastError == nil || st.LastError.Code != apierr.CodeDownloadChecksum {
		t.Fatalf("lastError = %+v, want %s", st.LastError, apierr.CodeDownloadChecksum)
	}

	// 必须**重试过**（maxFileAttempts=2 意味着至少 2 轮 3 个分片请求）。
	count := 0
	for _, r := range srv.takeRequests() {
		if strings.HasPrefix(r, "vad.onnx|") {
			count++
		}
	}
	if count < 2*3 {
		t.Fatalf("requests = %d, want >= %d (must retry once)", count, 2*3)
	}

	// 校验失败不得入库。
	installed, err := m.ListInstalled(t.Context())
	if err != nil {
		t.Fatalf("list installed: %v", err)
	}
	if len(installed) != 0 {
		t.Fatalf("corrupt bundle must not be installed: %+v", installed)
	}
}

func TestDownloadMirrorFailover(t *testing.T) {
	primary := newRangeServer(t)
	mirror := newRangeServer(t)
	root := t.TempDir()

	data := payload(40, 40)
	bundle := newBundle(primary, "sensevoice-small", "1.2.0", []string{RoleOffline},
		map[string][]byte{"model.onnx": data})
	primary.setCatalog(t, bundle)
	// 主镜像的文件接口全 500，镜像健康。
	primary.mu.Lock()
	primary.files["model.onnx"].Always500 = true
	primary.mu.Unlock()
	mirror.setFile("model.onnx", &testFile{Data: data, ETag: `"model.onnx-v1"`})

	opts := testOptions(root, primary.URL)
	opts.Mirrors = []string{mirror.URL}
	m, rec := newTestManager(t, opts)

	if _, err := m.Download(t.Context(), "sensevoice-small"); err != nil {
		t.Fatalf("start: %v", err)
	}
	st := rec.waitDownload(t, "sensevoice-small")
	if st.State != contract.DownloadDone {
		t.Fatalf("state = %s, want done (%+v)", st.State, st.LastError)
	}

	// 镜像确实被使用过。
	mirrorReqs := 0
	for _, r := range mirror.takeRequests() {
		if strings.HasPrefix(r, "model.onnx|") {
			mirrorReqs++
		}
	}
	if mirrorReqs == 0 {
		t.Fatal("mirror received no requests")
	}

	got, err := os.ReadFile(filepath.Join(root, DirModels, "sensevoice-small@1.2.0", "model.onnx"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(data) {
		t.Fatal("content from mirror mismatch")
	}
}

func TestDownloadConcurrencyLimit(t *testing.T) {
	srv := newRangeServer(t)
	root := t.TempDir()

	bundles := make([]ManifestBundle, 0, 3)
	for i := 0; i < 3; i++ {
		id := []string{"bundle-a", "bundle-b", "bundle-c"}[i]
		bundles = append(bundles, newBundle(srv, id, "1.0.0", []string{RoleOffline},
			map[string][]byte{id + ".bin": payload(byte(i), 32)}))
	}
	srv.setCatalog(t, bundles...)
	srv.setDelay(20 * time.Millisecond)

	m, rec := newTestManager(t, testOptions(root, srv.URL))

	var wg sync.WaitGroup
	for _, b := range bundles {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			if _, err := m.Download(t.Context(), id); err != nil {
				t.Errorf("start %s: %v", id, err)
			}
		}(b.BundleID)
	}

	// 采样并发数：必须从不超过 2（ARCH §4.6.2「同时最多 2 个 bundle」）。
	maxActive := 0
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if n := m.ActiveDownloads(); n > maxActive {
			maxActive = n
		}
		if maxActive > maxConcurrentBundles {
			t.Fatalf("active downloads = %d, exceeds limit %d", maxActive, maxConcurrentBundles)
		}
		done := 0
		for _, b := range bundles {
			if st, ok := rec.download(b.BundleID); ok && st.State == contract.DownloadDone {
				done++
			}
		}
		if done == len(bundles) {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	wg.Wait()

	for _, b := range bundles {
		st := rec.waitDownload(t, b.BundleID)
		if st.State != contract.DownloadDone {
			t.Fatalf("%s state = %s (%+v)", b.BundleID, st.State, st.LastError)
		}
	}
	if maxActive != maxConcurrentBundles {
		t.Fatalf("max concurrent downloads = %d, want exactly %d", maxActive, maxConcurrentBundles)
	}
}

func TestDownloadRejectsPathTraversal(t *testing.T) {
	srv := newRangeServer(t)
	base := t.TempDir()
	root := filepath.Join(base, "modelroot")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	evil := payload(1, 16)
	srv.setFile("evil.bin", &testFile{Data: evil, ETag: `"evil"`})
	bundle := ManifestBundle{
		BundleID:  "evil-bundle",
		Name:      "evil",
		Version:   "1.0.0",
		SizeBytes: int64(len(evil)),
		Files: []ManifestFile{{
			// 越界路径：下载器必须拒绝，而不是照写。
			Path:   "../../evil.bin",
			SHA256: sha256Hex(evil),
			Size:   int64(len(evil)),
			URL:    "evil.bin",
		}},
	}
	srv.setCatalog(t, bundle)

	m, _ := newTestManager(t, testOptions(root, srv.URL))

	_, err := m.Download(t.Context(), "evil-bundle")
	if err == nil {
		t.Fatal("expected an error for a traversal path")
	}
	if !apierr.HasCode(err, apierr.CodeInvalidArgument) {
		t.Fatalf("error code = %v, want %s", err, apierr.CodeInvalidArgument)
	}
	// 越界文件绝不能出现在模型根之外。
	for _, p := range []string{
		filepath.Join(base, "evil.bin"),
		filepath.Join(base, "..", "evil.bin"),
	} {
		if _, statErr := os.Stat(p); statErr == nil {
			t.Fatalf("file escaped the model root: %s", p)
		}
	}
}

func TestSanitizeRelPath(t *testing.T) {
	ok := []string{"model.onnx", "sub/dir/f.bin", "./x.txt", "a b/c.txt"}
	for _, p := range ok {
		if _, err := sanitizeRelPath(p); err != nil {
			t.Errorf("sanitizeRelPath(%q) = %v, want ok", p, err)
		}
	}
	bad := []string{"", "  ", "/abs/path", "C:/windows", `\\server\share`, "../up", "a/../../b", "a\x00b", ".."}
	for _, p := range bad {
		if _, err := sanitizeRelPath(p); err == nil {
			t.Errorf("sanitizeRelPath(%q) = nil, want rejection", p)
		}
	}
}

func TestChunkRanges(t *testing.T) {
	got := chunkRanges(40, 16)
	want := []chunkRange{{0, 0, 15}, {1, 16, 31}, {2, 32, 39}}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("chunk %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if n := len(chunkRanges(16, 16)); n != 1 {
		t.Fatalf("exact multiple: %d chunks, want 1", n)
	}
	if n := len(chunkRanges(0, 16)); n != 0 {
		t.Fatalf("zero size: %d chunks, want 0", n)
	}
}

func TestCompareVersion(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.2.0", "1.10.0", -1},
		{"1.10.0", "1.2.0", 1},
		{"2.0.0", "2.0.0", 0},
		{"v1.0.3", "1.0.3", 0},
	}
	for _, c := range cases {
		if got := compareVersion(c.a, c.b); got != c.want {
			t.Errorf("compareVersion(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestMirrorURL(t *testing.T) {
	bases := []string{"https://mirror.example.com/repo"}
	got := mirrorURL("https://github.com/o/r/releases/download/v1/model.onnx", 1, bases)
	want := "https://mirror.example.com/repo/o/r/releases/download/v1/model.onnx"
	if got != want {
		t.Fatalf("mirrorURL = %q, want %q", got, want)
	}
	if got := mirrorURL("https://x/y.bin", 0, bases); got != "https://x/y.bin" {
		t.Fatalf("idx 0 must keep the original url, got %q", got)
	}
}

func TestPauseDownloadKeepsParts(t *testing.T) {
	srv := newRangeServer(t)
	root := t.TempDir()

	bundle := newBundle(srv, "sensevoice-small", "1.2.0", []string{RoleOffline},
		map[string][]byte{"model.onnx": payload(7, 40)})
	srv.setCatalog(t, bundle)
	srv.setDelay(120 * time.Millisecond)

	m, rec := newTestManager(t, testOptions(root, srv.URL))
	if _, err := m.Download(t.Context(), "sensevoice-small"); err != nil {
		t.Fatalf("start: %v", err)
	}
	// 等第一个分片落盘后再暂停。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if sf := m.loadStateFile(filepath.Join(root, DirDownloads, "sensevoice-small@1.2.0.state.json")); sf != nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := m.PauseDownload("sensevoice-small"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	st := rec.waitDownload(t, "sensevoice-small")
	if st.State != contract.DownloadPaused {
		t.Fatalf("state = %s, want paused", st.State)
	}
	// 分片目录必须保留（否则「暂停」等于「重下」）。
	if _, err := os.Stat(filepath.Join(root, DirDownloads, "sensevoice-small@1.2.0")); err != nil {
		t.Fatalf("parts should be kept on pause: %v", err)
	}

	// 取消并清理。
	srv.setDelay(0)
	rec.resetDownload("sensevoice-small")
	if _, err := m.Download(t.Context(), "sensevoice-small"); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if st := rec.waitDownload(t, "sensevoice-small"); st.State != contract.DownloadDone {
		t.Fatalf("resume after pause state = %s", st.State)
	}
	if err := m.CancelDownload("sensevoice-small", true); err != nil {
		// 已完成的任务不在运行表里，返回 NotFound 是合法的，但清理仍应幂等成功。
		if !apierr.HasCode(err, apierr.CodeNotFound) {
			t.Fatalf("cancel: %v", err)
		}
	}
}
