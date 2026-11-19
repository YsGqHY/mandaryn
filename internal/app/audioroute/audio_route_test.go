package audioroute

// 音频路由单测。
//
// 测试策略说明（任务要求「自己选更轻的做法并说明理由」）：
//   - 纯逻辑（safeJoinUnderRoot / audioContentType）直接表驱动，零依赖；
//   - HTTP 层**不搭 SQLite**，而是走 newAudioHandler(spa, lookup, dataRoot) 注入假会话查询器。
//     理由：本文件要验的是「Range / 状态码 / 越界 404 / SPA 透传」这些 HTTP 语义，
//     与 GORM 查询无关；拉起整套 storage.Open + AutoMigrate 只会让这些用例变慢、
//     并引入并发开发期间别人改动 storage 时的连带失败。GORM 那一层只有一个
//     `Where("id = ?").Select("audio_path")`，属于薄到不值得为它建库的部分。

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// wavBytes 生成足够长的假 WAV 内容（不需要真音频头，ServeContent 只看字节）。
func wavBytes(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i % 251) // 非零、可预测，便于断言 Range 片段
	}
	return b
}

// newTestAudioHandler 组装一个音频处理器，音频文件放在临时 dataRoot 下。
func newTestAudioHandler(t *testing.T, rel string, content []byte, spa http.Handler) (http.Handler, string) {
	t.Helper()
	root := t.TempDir()
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(abs, content, 0o600); err != nil {
		t.Fatalf("write audio: %v", err)
	}
	lookup := func(_ context.Context, id string) (string, bool, error) {
		if id != "s_test" {
			return "", false, nil
		}
		return rel, true, nil
	}
	return newAudioHandler(spa, lookup, func() (string, error) { return root, nil }), root
}

func TestAudioRouteGetFullFile(t *testing.T) {
	content := wavBytes(8192)
	h, _ := newTestAudioHandler(t, "sessions/s_test/audio.wav", content, http.NotFoundHandler())

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/audio/s_test", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("Accept-Ranges = %q, want bytes", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "audio/wav" {
		t.Errorf("Content-Type = %q, want audio/wav", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if !bytes.Equal(rec.Body.Bytes(), content) {
		t.Errorf("body 长度 %d 与源文件 %d 不一致或内容不同", rec.Body.Len(), len(content))
	}
}

// 查询串必须被忽略（前端用 ?v= 破缓存）。
func TestAudioRouteIgnoresQueryString(t *testing.T) {
	content := wavBytes(1024)
	h, _ := newTestAudioHandler(t, "sessions/s_test/audio.wav", content, http.NotFoundHandler())

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/audio/s_test?v=1700000000", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), content) {
		t.Error("带查询串时返回内容不一致")
	}
}

func TestAudioRouteRange(t *testing.T) {
	content := wavBytes(4096)
	h, _ := newTestAudioHandler(t, "sessions/s_test/audio.wav", content, http.NotFoundHandler())

	req := httptest.NewRequest(http.MethodGet, "/audio/s_test", nil)
	req.Header.Set("Range", "bytes=100-199")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", rec.Code)
	}
	if got := rec.Header().Get("Content-Length"); got != "100" {
		t.Errorf("Content-Length = %q, want 100", got)
	}
	if got := rec.Header().Get("Content-Range"); got != "bytes 100-199/4096" {
		t.Errorf("Content-Range = %q, want bytes 100-199/4096", got)
	}
	if !bytes.Equal(rec.Body.Bytes(), content[100:200]) {
		t.Error("206 片段内容与源文件对应区间不一致")
	}
}

func TestAudioRouteFLACRange(t *testing.T) {
	content := append([]byte("fLaC"), wavBytes(60)...)
	h, _ := newTestAudioHandler(t, "sessions/s_test/audio.flac", content, http.NotFoundHandler())
	for _, ranged := range []bool{false, true} {
		req := httptest.NewRequest(http.MethodGet, "/audio/s_test", nil)
		wantStatus, wantBody := http.StatusOK, content
		if ranged {
			req.Header.Set("Range", "bytes=5-12")
			wantStatus, wantBody = http.StatusPartialContent, content[5:13]
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != wantStatus || rec.Header().Get("Content-Type") != "audio/flac" || !bytes.Equal(rec.Body.Bytes(), wantBody) {
			t.Fatalf("range=%t: status=%d headers=%v body length=%d", ranged, rec.Code, rec.Header(), rec.Body.Len())
		}
		if ranged && rec.Header().Get("Content-Range") != "bytes 5-12/64" {
			t.Fatalf("Content-Range=%q", rec.Header().Get("Content-Range"))
		}
	}
}

// seek 的另一半：开放结尾 Range。
func TestAudioRouteRangeOpenEnded(t *testing.T) {
	content := wavBytes(1000)
	h, _ := newTestAudioHandler(t, "sessions/s_test/audio.wav", content, http.NotFoundHandler())

	req := httptest.NewRequest(http.MethodGet, "/audio/s_test", nil)
	req.Header.Set("Range", "bytes=900-")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), content[900:]) {
		t.Error("开放结尾 Range 片段内容不正确")
	}
}

func TestAudioRouteHead(t *testing.T) {
	content := wavBytes(2048)
	h, _ := newTestAudioHandler(t, "sessions/s_test/audio.wav", content, http.NotFoundHandler())

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, "/audio/s_test", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD 响应带 body（%d 字节），应为空", rec.Body.Len())
	}
	if got := rec.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("HEAD 也应带 Accept-Ranges，得到 %q", got)
	}
}

func TestAudioRouteUnknownSession(t *testing.T) {
	h, _ := newTestAudioHandler(t, "sessions/s_test/audio.wav", wavBytes(64), http.NotFoundHandler())

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/audio/s_unknown", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// 库里查不到 ≠ 读库出错：两者都必须 404（不暴露内部状态）。
func TestAudioRouteLookupError(t *testing.T) {
	h := newAudioHandler(
		http.NotFoundHandler(),
		func(context.Context, string) (string, bool, error) {
			return "", false, errors.New("db exploded at /secret/path")
		},
		func() (string, error) { return t.TempDir(), nil },
	)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/audio/s_test", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "secret") {
		t.Error("404 响应体泄漏了内部错误详情")
	}
}

// 路径越界：`..` 穿越与绝对路径都必须 404，且响应体不回显真实路径。
func TestAudioRouteRejectsEscape(t *testing.T) {
	cases := []struct {
		name string
		rel  string
	}{
		{"parent traversal", "../../etc/passwd.wav"},
		{"deep traversal", "sessions/../../../etc/passwd.wav"},
		{"absolute unix", "/etc/passwd.wav"},
		{"absolute windows", `C:\Windows\win.ini.wav`},
		{"unc", `//server/share/audio.wav`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			// 在 root 之外造一个真实文件，确认不是「文件不存在」而挡住了它。
			outside := filepath.Join(filepath.Dir(root), "outside.wav")
			_ = os.WriteFile(outside, wavBytes(32), 0o600)
			t.Cleanup(func() { _ = os.Remove(outside) })

			h := newAudioHandler(
				http.NotFoundHandler(),
				func(context.Context, string) (string, bool, error) { return tc.rel, true, nil },
				func() (string, error) { return root, nil },
			)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/audio/s_test", nil))

			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", rec.Code)
			}
			if strings.Contains(rec.Body.String(), root) || strings.Contains(rec.Body.String(), "passwd") ||
				strings.Contains(rec.Body.String(), "win.ini") {
				t.Errorf("404 响应体泄漏了路径信息：%q", rec.Body.String())
			}
		})
	}
}

// 符号链接越界：字面路径合法、真实指向根外 → 也必须 404。
// Windows 建符号链接需要开发者模式/管理员权限，建不出来就跳过。
func TestAudioRouteRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "secret.wav")
	if err := os.WriteFile(outsideFile, wavBytes(128), 0o600); err != nil {
		t.Fatalf("write outside: %v", err)
	}

	linkDir := filepath.Join(root, "sessions", "s_link")
	if err := os.MkdirAll(linkDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	linkPath := filepath.Join(linkDir, "audio.wav")
	if err := os.Symlink(outsideFile, linkPath); err != nil {
		t.Skipf("无法创建符号链接（Windows 需开发者模式）：%v", err)
	}

	h := newAudioHandler(
		http.NotFoundHandler(),
		func(context.Context, string) (string, bool, error) {
			return "sessions/s_link/audio.wav", true, nil
		},
		func() (string, error) { return root, nil },
	)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/audio/s_link", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("符号链接越界 status = %d, want 404", rec.Code)
	}
}

// 非 .wav 后缀 → 404（按落库路径判断，不信客户端）。
func TestAudioRouteRejectsNonWav(t *testing.T) {
	h, _ := newTestAudioHandler(t, "sessions/s_test/audio.json", []byte(`{"a":1}`), http.NotFoundHandler())

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/audio/s_test", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// /audio/ 下的畸形路径（空 ID、多段）→ 404。
func TestAudioRouteRejectsBadPath(t *testing.T) {
	h, _ := newTestAudioHandler(t, "sessions/s_test/audio.wav", wavBytes(32), http.NotFoundHandler())

	for _, p := range []string{"/audio/", "/audio/s_test/extra"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s status = %d, want 404", p, rec.Code)
		}
	}
}

// 非 /audio/ 前缀必须原样交给 SPA 处理器（前端路由回退不能被打断）。
func TestAudioRouteDelegatesToSPA(t *testing.T) {
	var calls int
	spa := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("spa:" + r.URL.Path))
	})
	h := newAudioHandler(
		spa,
		func(context.Context, string) (string, bool, error) {
			t.Error("SPA 路径不应触发会话查询")
			return "", false, nil
		},
		func() (string, error) {
			t.Error("SPA 路径不应触发 dataRoot 解析")
			return "", nil
		},
	)

	for _, p := range []string{"/", "/library", "/audio", "/audioish/x", "/assets/index.js"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusTeapot {
			t.Errorf("%s status = %d, want 418（未透传给 SPA）", p, rec.Code)
		}
		if got := rec.Body.String(); got != "spa:"+p {
			t.Errorf("%s body = %q，SPA 收到的路径不对", p, got)
		}
	}
	if calls != 5 {
		t.Errorf("SPA 被调用 %d 次，want 5", calls)
	}
}

// audioContentType 表驱动。
func TestAudioContentType(t *testing.T) {
	cases := map[string]string{
		"audio.wav":  "audio/wav",
		"audio.WAV":  "audio/wav",
		"audio.mp3":  "audio/mpeg",
		"audio.opus": "audio/ogg",
		"audio.flac": "audio/flac",
		"audio.m4a":  "audio/mp4",
		"a.json":     "application/octet-stream",
		"noext":      "application/octet-stream",
	}
	for in, want := range cases {
		if got := audioContentType(in); got != want {
			t.Errorf("audioContentType(%q) = %q, want %q", in, got, want)
		}
	}
}

// safeJoinUnderRoot 表驱动：合法路径必须落在根内，越界输入必须报错。
func TestSafeJoinUnderRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "data-root")

	valid := []string{
		"a/b.wav",
		"sessions/s_01/audio.wav",
		"./a/./b.wav",
		"a\\b.wav", // Windows 分隔符也接受
		"sessions/s_1/x.wav",
		"a/b/../b.wav", // Clean 之后仍在根内
	}
	for _, rel := range valid {
		got, err := safeJoinUnderRoot(root, rel)
		if err != nil {
			t.Errorf("safeJoinUnderRoot(%q) 应合法，得到错误：%v", rel, err)
			continue
		}
		ok, relErr := isUnderRoot(root, got)
		if relErr != nil || !ok {
			t.Errorf("safeJoinUnderRoot(%q) = %q 不在根内", rel, got)
		}
	}

	invalid := []string{
		"../x.wav",
		"a/../../b.wav",
		"../../../../etc/passwd.wav",
		`C:\x\y.wav`,
		"C:/x/y.wav",
		"C:x.wav",
		`//server/share/audio.wav`,
		`\\server\share\audio.wav`,
		"/etc/passwd.wav",
		"..",
		".",
		"",
		"   ",
	}
	for _, rel := range invalid {
		if _, err := safeJoinUnderRoot(root, rel); err == nil {
			t.Errorf("safeJoinUnderRoot(%q) 应判越界，却通过了", rel)
		}
	}
}

// dataRoot 取不到（解析器失败）时必须 404 而不是 panic / 500。
func TestAudioRouteNoDataRoot(t *testing.T) {
	h := newAudioHandler(
		http.NotFoundHandler(),
		func(context.Context, string) (string, bool, error) {
			return "sessions/s_test/audio.wav", true, nil
		},
		func() (string, error) { return "", errors.New("resolver unavailable") },
	)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/audio/s_test", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
