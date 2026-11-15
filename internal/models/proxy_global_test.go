package models

// 全局代理（MANDARYN_HTTP_PROXY）的端到端验证。
//
// 与 proxy_test.go 的分工：那边只验证 Transport 形态与校验边界，不发请求；
// 这里必须**真的经代理转发**，否则「清单与下载是否共用同一份代理」根本无法证伪。
// 用一个最小的转发型 HTTP 代理（httptest），逐条记录目标 URL 与 Proxy-Authorization。

import (
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
)

// proxyHit 是代理记到的一条请求。
type proxyHit struct {
	method string
	// target 代理看到的目标（绝对 URL，不含本代理的凭据）。
	target string
	// auth Proxy-Authorization 原文（用于断言内嵌凭据被正确送达）。
	auth string
	// rangeHdr 便于确认分片下载也真的走了代理。
	rangeHdr string
}

// fakeProxy 是一个只做转发的最小 HTTP 代理。
type fakeProxy struct {
	*httptest.Server

	// forward 直连用的 Transport（自身不能再走代理，否则会绕回自己）。
	forward *http.Transport

	mu   sync.Mutex
	hits []proxyHit
	// wantAuth 非空时校验 Proxy-Authorization，不匹配回 407。
	wantAuth string
}

func newFakeProxy(t *testing.T) *fakeProxy {
	t.Helper()
	p := &fakeProxy{forward: &http.Transport{}}
	p.Server = httptest.NewServer(http.HandlerFunc(p.handle))
	t.Cleanup(func() {
		p.Server.Close()
		p.forward.CloseIdleConnections()
	})
	return p
}

// url 返回可直接填进 MANDARYN_HTTP_PROXY 的代理地址。
func (p *fakeProxy) url() string { return p.Server.URL }

// setAuth 要求代理认证（Basic），auth 为期望的 Proxy-Authorization 值。
func (p *fakeProxy) setAuth(auth string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.wantAuth = auth
}

// take 取出并清空已记录的请求。
func (p *fakeProxy) take() []proxyHit {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.hits
	p.hits = nil
	return out
}

func (p *fakeProxy) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.hits)
}

func (p *fakeProxy) handle(w http.ResponseWriter, r *http.Request) {
	// 走代理的明文 HTTP 请求，请求行是绝对 URL；不是绝对 URL 说明客户端没在用它。
	if r.URL == nil || !r.URL.IsAbs() {
		http.Error(w, "expected an absolute-uri proxy request", http.StatusBadRequest)
		return
	}
	auth := r.Header.Get("Proxy-Authorization")
	rangeHdr := r.Header.Get("Range")

	p.mu.Lock()
	want := p.wantAuth
	p.hits = append(p.hits, proxyHit{
		method: r.Method, target: r.URL.String(), auth: auth, rangeHdr: rangeHdr,
	})
	p.mu.Unlock()

	if want != "" && auth != want {
		w.Header().Set("Proxy-Authenticate", `Basic realm="fake"`)
		w.WriteHeader(http.StatusProxyAuthRequired)
		return
	}

	out, err := http.NewRequestWithContext(r.Context(), r.Method, r.URL.String(), nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	// 只转发下游真正需要的头：Proxy-Authorization 属于「客户端↔代理」一跳，
	// 按 RFC 不应透给目标服务器。
	for _, h := range []string{"Range", "If-Range", "Accept", "User-Agent"} {
		if v := r.Header.Get(h); v != "" {
			out.Header.Set(h, v)
		}
	}
	resp, err := p.forward.RoundTrip(out)
	if err != nil {
		http.Error(w, "upstream unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for _, h := range []string{"Content-Type", "Content-Range", "Content-Length", "ETag", "Accept-Ranges"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// basicAuth 生成 Basic 认证头值。
func basicAuth(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

// TestGlobalProxyServesCatalogAndDownload 是本需求的核心用例：
// 清单获取与分片下载必须**都**经同一份全局代理出站。
func TestGlobalProxyServesCatalogAndDownload(t *testing.T) {
	dir := t.TempDir()
	srv := newRangeServer(t)
	prox := newFakeProxy(t)

	bundle := newBundle(srv, "sensevoice-small", "1.2.0", []string{"offline"},
		map[string][]byte{"model.onnx": []byte("model-bytes"), "tokens.txt": []byte("tokens")})
	srv.setCatalog(t, bundle)

	t.Setenv(EnvHTTPProxy, prox.url())
	opts := testOptions(dir, srv.URL)
	m, rec := newTestManager(t, opts)

	// 1. 清单获取
	if _, err := m.Fetch(t.Context()); err != nil {
		t.Fatalf("fetch catalog: %v", err)
	}
	hits := prox.take()
	if len(hits) != 1 {
		t.Fatalf("catalog requests through proxy = %d, want 1", len(hits))
	}
	if !strings.HasSuffix(hits[0].target, "/"+FileCatalog) {
		t.Fatalf("catalog target = %q", hits[0].target)
	}

	// 2. 分片下载（含 Range 请求）
	if _, err := m.Download(t.Context(), bundle.BundleID); err != nil {
		t.Fatalf("start download: %v", err)
	}
	if st := rec.waitDownload(t, bundle.BundleID); st.State != contract.DownloadDone {
		t.Fatalf("download state = %s (%+v)", st.State, st.LastError)
	}
	hits = prox.take()
	if len(hits) == 0 {
		t.Fatal("download sent no request through the proxy")
	}
	sawRange := false
	for _, h := range hits {
		if h.rangeHdr != "" {
			sawRange = true
		}
		if strings.Contains(h.target, FileCatalog) {
			t.Fatalf("download should not refetch the catalog here: %q", h.target)
		}
	}
	if !sawRange {
		t.Fatal("chunked download did not use Range through the proxy")
	}
}

// TestGlobalProxyWithCredentials 覆盖 http://user:pass@host:port 的内嵌凭据。
func TestGlobalProxyWithCredentials(t *testing.T) {
	dir := t.TempDir()
	srv := newRangeServer(t)
	prox := newFakeProxy(t)

	bundle := newBundle(srv, "silero-vad", "2.0.0", []string{"vad"},
		map[string][]byte{"vad.onnx": []byte("vad-bytes")})
	srv.setCatalog(t, bundle)

	wantAuth := basicAuth("proxy-user", "p@ss:word")
	prox.setAuth(wantAuth)

	// 密码里带 @ 与 : 时必须按 URL 规则转义，解析后要还原成明文。
	t.Setenv(EnvHTTPProxy, "http://proxy-user:p%40ss%3Aword@"+strings.TrimPrefix(prox.url(), "http://"))
	m, rec := newTestManager(t, testOptions(dir, srv.URL))

	if _, err := m.Download(t.Context(), bundle.BundleID); err != nil {
		t.Fatalf("start download: %v", err)
	}
	st := rec.waitDownload(t, bundle.BundleID)
	if st.State != contract.DownloadDone {
		t.Fatalf("download state = %s (%+v)", st.State, st.LastError)
	}
	hits := prox.take()
	if len(hits) == 0 {
		t.Fatal("no request reached the proxy")
	}
	for _, h := range hits {
		if h.auth != wantAuth {
			t.Fatalf("Proxy-Authorization = %q, want %q", h.auth, wantAuth)
		}
	}
}

// TestGlobalProxyEmptyKeepsDirect 验证「为空 = 原行为」：不设该变量时一个请求
// 都不应进代理，原有直连下载必须照常成功。
func TestGlobalProxyEmptyKeepsDirect(t *testing.T) {
	dir := t.TempDir()
	srv := newRangeServer(t)
	prox := newFakeProxy(t)

	bundle := newBundle(srv, "3dspeaker-zh", "1.1.0", []string{"speaker"},
		map[string][]byte{"spk.onnx": []byte("spk-bytes")})
	srv.setCatalog(t, bundle)

	t.Setenv(EnvHTTPProxy, "")
	m, rec := newTestManager(t, testOptions(dir, srv.URL))

	if _, err := m.Download(t.Context(), bundle.BundleID); err != nil {
		t.Fatalf("start download: %v", err)
	}
	if st := rec.waitDownload(t, bundle.BundleID); st.State != contract.DownloadDone {
		t.Fatalf("direct download state = %s (%+v)", st.State, st.LastError)
	}
	if n := prox.count(); n != 0 {
		t.Fatalf("empty proxy config still sent %d request(s) to the proxy", n)
	}
	tr, err := m.httpClient()
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	htr, ok := tr.Transport.(*http.Transport)
	if !ok || htr.Proxy != nil {
		t.Fatal("empty proxy config must leave Transport.Proxy nil (direct)")
	}
}

// TestGlobalProxyInvalidConfigFailsLoudly 验证非法配置**不静默直连**。
func TestGlobalProxyInvalidConfigFailsLoudly(t *testing.T) {
	cases := map[string]string{
		"unsupported scheme": "ftp://127.0.0.1:21",
		"no host":            "http://:8080",
		"bad port":           "http://127.0.0.1:99999",
		"has path":           "http://127.0.0.1:8080/pac.js",
		"password only":      "http://:secret@127.0.0.1:8080",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv(EnvHTTPProxy, raw)
			m, _ := newTestManager(t, testOptions(t.TempDir(), "http://127.0.0.1:1"))
			if _, err := m.httpClient(); !apierr.HasCode(err, apierr.CodeDownloadProxy) {
				t.Fatalf("httpClient error = %v, want %s", err, apierr.CodeDownloadProxy)
			}
		})
	}
}

// TestGlobalProxyOverridesSettings 验证运维接管：全局代理优先于设置页配置。
func TestGlobalProxyOverridesSettings(t *testing.T) {
	prox := newFakeProxy(t)
	t.Setenv(EnvHTTPProxy, prox.url())

	opts := testOptions(t.TempDir(), "http://127.0.0.1:1")
	opts.Proxy = func() (contract.ProxyConfig, error) {
		// 设置页配了另一台代理：全局代理必须赢。
		return contract.ProxyConfig{Type: contract.ProxyHTTP, Host: "127.0.0.1", Port: 3128}, nil
	}
	m := newRawManager(t, opts)

	client, err := m.httpClient()
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	tr, ok := client.Transport.(*http.Transport)
	if !ok || tr.Proxy == nil {
		t.Fatal("global proxy was not applied to the transport")
	}
	req, _ := http.NewRequest(http.MethodGet, "http://example.com/models.json", nil)
	u, perr := tr.Proxy(req)
	if perr != nil || u == nil {
		t.Fatalf("proxy url = %v, %v", u, perr)
	}
	if got, want := u.Host, strings.TrimPrefix(prox.url(), "http://"); got != want {
		t.Fatalf("proxy host = %q, want global %q", got, want)
	}

	// 清空后回落到设置页配置（原有行为不变）。
	t.Setenv(EnvHTTPProxy, "")
	client2, err := m.httpClient()
	if err != nil {
		t.Fatalf("client after clearing env: %v", err)
	}
	tr2, ok := client2.Transport.(*http.Transport)
	if !ok || tr2.Proxy == nil {
		t.Fatal("settings proxy not applied after the global proxy was cleared")
	}
	u2, _ := tr2.Proxy(req)
	if u2 == nil || u2.Host != "127.0.0.1:3128" {
		t.Fatalf("settings proxy host = %v", u2)
	}
}

// TestGlobalProxyClientRebuildsOnPasswordChange 验证密码变化也会重建 client。
func TestGlobalProxyClientRebuildsOnPasswordChange(t *testing.T) {
	base := strings.TrimPrefix(newFakeProxy(t).url(), "http://")

	t.Setenv(EnvHTTPProxy, "http://u:first@"+base)
	m := newRawManager(t, testOptions(t.TempDir(), "http://127.0.0.1:1"))
	first, err := m.httpClient()
	if err != nil {
		t.Fatalf("client: %v", err)
	}

	// 同一 host/user，只换密码：不重建就会一直用旧凭据。
	t.Setenv(EnvHTTPProxy, "http://u:second@"+base)
	second, err := m.httpClient()
	if err != nil {
		t.Fatalf("client after password change: %v", err)
	}
	if second == first {
		t.Fatal("client must be rebuilt when the proxy password changes")
	}

	// 配置没变时仍然复用（不能每次出站都新建连接池）。
	again, err := m.httpClient()
	if err != nil {
		t.Fatalf("client again: %v", err)
	}
	if again != second {
		t.Fatal("client must be cached while the config is unchanged")
	}
}
