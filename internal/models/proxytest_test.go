package models

// 代理连通性测试（API-9.4）的验证。
//
// 口径：必须**真的经代理出站**才算测通 —— 若测试只校验“构造出的 Transport 形态”，
// 就无法证伪「测试链路与实际下载链路是两份实现」这个最常见的假阳性来源。
// 因此这里复用 proxy_global_test.go 的 fakeProxy（最小转发代理），逐条断言命中。

import (
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/utils/httpx"
)

// proxyHostPort 把 httptest 服务地址拆成 ProxyConfig 需要的 host / port。
func proxyHostPort(t *testing.T, rawURL string) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(strings.TrimPrefix(rawURL, "http://"))
	if err != nil {
		t.Fatalf("拆分代理地址失败 %q: %v", rawURL, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("代理端口非法 %q: %v", portStr, err)
	}
	return host, port
}

// errStr 是一个只携带文本的错误，用于分类口径的纯函数断言。
type errStr string

func (e errStr) Error() string { return string(e) }

// TestProxyConnectivityOK 走代理能拿到清单 → OK=true、有延迟、分类为空。
func TestProxyConnectivityOK(t *testing.T) {
	dir := t.TempDir()
	srv := newRangeServer(t)
	prox := newFakeProxy(t)
	bundle := newBundle(srv, "sensevoice-small", "1.2.0", []string{"offline"},
		map[string][]byte{"model.onnx": []byte("model-bytes")})
	srv.setCatalog(t, bundle)

	m, _ := newTestManager(t, testOptions(dir, srv.URL))

	host, port := proxyHostPort(t, prox.url())
	res, err := m.TestProxy(t.Context(), contract.ProxyConfig{
		Type: contract.ProxyHTTP, Host: host, Port: port,
	})
	if err != nil {
		t.Fatalf("TestProxy: %v", err)
	}
	if !res.OK {
		t.Fatalf("want OK=true, got %+v", res)
	}
	if res.Category != "" {
		t.Errorf("成功时 Category 应为空，实际 %q", res.Category)
	}
	if res.LatencyMs < 0 {
		t.Errorf("LatencyMs 不应为负：%d", res.LatencyMs)
	}

	// 真的走代理：代理侧必须看到一条指向清单的请求。
	hits := prox.take()
	if len(hits) != 1 {
		t.Fatalf("经代理的请求数 = %d, want 1", len(hits))
	}
	if !strings.HasSuffix(hits[0].target, "/"+FileCatalog) {
		t.Errorf("测试目标应为清单地址，实际 %q", hits[0].target)
	}
}

// TestProxyConnectivitySendsCredentials 内嵌凭据必须按 Proxy-Authorization 送达。
func TestProxyConnectivitySendsCredentials(t *testing.T) {
	dir := t.TempDir()
	srv := newRangeServer(t)
	prox := newFakeProxy(t)
	srv.setCatalog(t, newBundle(srv, "b", "1.0.0", []string{"offline"},
		map[string][]byte{"m.bin": []byte("x")}))

	wantAuth := basicAuth("alice", "s3cr3t")
	prox.setAuth(wantAuth)

	m, _ := newTestManager(t, testOptions(dir, srv.URL))
	host, port := proxyHostPort(t, prox.url())
	res, err := m.TestProxy(t.Context(), contract.ProxyConfig{
		Type:     contract.ProxyHTTP,
		Host:     host,
		Port:     port,
		Username: "alice",
		Password: "s3cr3t",
	})
	if err != nil {
		t.Fatalf("TestProxy: %v", err)
	}
	if !res.OK {
		t.Fatalf("凭据正确时应连通，实际 %+v", res)
	}
	hits := prox.take()
	if len(hits) != 1 || hits[0].auth != wantAuth {
		t.Fatalf("Proxy-Authorization 未按预期送达：%+v", hits)
	}
}

// TestProxyConnectivityAuthRejected 407 → Category=auth，且**不**回显密码。
func TestProxyConnectivityAuthRejected(t *testing.T) {
	dir := t.TempDir()
	srv := newRangeServer(t)
	prox := newFakeProxy(t)
	srv.setCatalog(t, newBundle(srv, "b", "1.0.0", []string{"offline"},
		map[string][]byte{"m.bin": []byte("x")}))
	prox.setAuth(basicAuth("alice", "right"))

	m, _ := newTestManager(t, testOptions(dir, srv.URL))
	host, port := proxyHostPort(t, prox.url())
	res, err := m.TestProxy(t.Context(), contract.ProxyConfig{
		Type:     contract.ProxyHTTP,
		Host:     host,
		Port:     port,
		Username: "alice",
		Password: "wrong-password",
	})
	if err != nil {
		t.Fatalf("网络/认证失败不应返回 error（配置本身合法）：%v", err)
	}
	if res.OK {
		t.Fatal("凭据错误时不应判为连通")
	}
	if res.Category != "auth" {
		t.Errorf("Category = %q, want auth", res.Category)
	}
	// 脱敏：Note 不得含密码（本用例的密码串即为探针）。
	if strings.Contains(res.Note, "wrong-password") {
		t.Errorf("Note 泄漏了密码：%q", res.Note)
	}
}

// TestProxyConnectivityInvalidConfig 非法配置 → 返回 error（调用方必须先改配置）。
func TestProxyConnectivityInvalidConfig(t *testing.T) {
	dir := t.TempDir()
	srv := newRangeServer(t)
	m, _ := newTestManager(t, testOptions(dir, srv.URL))

	if _, err := m.TestProxy(t.Context(), contract.ProxyConfig{Type: "ftp", Host: "x", Port: 1}); err == nil {
		t.Fatal("未知代理类型应返回 error")
	}
	if _, err := m.TestProxy(t.Context(), contract.ProxyConfig{
		Type: contract.ProxyHTTP, Host: "", Port: 80,
	}); err == nil {
		t.Fatal("缺主机应返回 error")
	}
}

// TestProxyConnectivityDirectWhenNone 直连（Type=none）也能测：不走任何代理。
func TestProxyConnectivityDirectWhenNone(t *testing.T) {
	dir := t.TempDir()
	srv := newRangeServer(t)
	prox := newFakeProxy(t)
	srv.setCatalog(t, newBundle(srv, "b", "1.0.0", []string{"offline"},
		map[string][]byte{"m.bin": []byte("x")}))

	m, _ := newTestManager(t, testOptions(dir, srv.URL))
	res, err := m.TestProxy(t.Context(), contract.ProxyConfig{Type: contract.ProxyNone})
	if err != nil {
		t.Fatalf("TestProxy: %v", err)
	}
	if !res.OK {
		t.Fatalf("直连应连通，实际 %+v", res)
	}
	if prox.count() != 0 {
		t.Errorf("直连不该经过代理，代理命中 %d 次", prox.count())
	}
}

// TestProxyConnectivityOfflineGate 离线开关开启时**零请求**（FR-08-5）。
func TestProxyConnectivityOfflineGate(t *testing.T) {
	dir := t.TempDir()
	srv := newRangeServer(t)
	prox := newFakeProxy(t)
	srv.setCatalog(t, newBundle(srv, "b", "1.0.0", []string{"offline"},
		map[string][]byte{"m.bin": []byte("x")}))
	t.Setenv("MANDARYN_OFFLINE", "1")

	m, _ := newTestManager(t, testOptions(dir, srv.URL))
	host, port := proxyHostPort(t, prox.url())
	_, err := m.TestProxy(t.Context(), contract.ProxyConfig{
		Type: contract.ProxyHTTP, Host: host, Port: port,
	})
	if !apierr.HasCode(err, apierr.CodeOfflineEnforced) {
		t.Fatalf("离线时应返回 ERR_OFFLINE_ENFORCED，实际 %v", err)
	}
	if prox.count() != 0 {
		t.Errorf("离线时不得发请求，代理命中 %d 次", prox.count())
	}
}

// TestClassifyProxyError 锁死分类口径（前端按 Category 给不同建议）。
func TestClassifyProxyError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"proxy-auth", &httpx.HTTPError{StatusCode: http.StatusProxyAuthRequired, Status: "407 Proxy Authentication Required"}, "auth"},
		{"rate-limit", &httpx.HTTPError{StatusCode: http.StatusTooManyRequests, Status: "429 Too Many Requests"}, "rate-limit"},
		{"server", &httpx.HTTPError{StatusCode: http.StatusInternalServerError, Status: "500 Internal Server Error"}, "network"},
		{"tls", errStr("x509: certificate signed by unknown authority"), "tls"},
		{"timeout-word", errStr("dial tcp: i/o timeout"), "timeout"},
		{"plain", errStr("connection refused"), "network"},
	}
	for _, c := range cases {
		if got := classifyProxyError(c.err); got != c.want {
			t.Errorf("%s: classifyProxyError = %q, want %q", c.name, got, c.want)
		}
	}
}
