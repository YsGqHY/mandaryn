package models

// 代理构造测试（FR-05-5）。不发起真实连接：只验证 Transport 的形态与校验边界。

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/storage"
	"vip.oberon.mandaryn/internal/utils/cryptox"
)

func TestProxyConfigFromSettings(t *testing.T) {
	enc, err := cryptox.EncryptString("s3cr3t")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	row := &storage.AppSettings{
		ProxyType:        contract.ProxyHTTP,
		ProxyHost:        "127.0.0.1",
		ProxyPort:        8080,
		ProxyUsername:    "user",
		ProxyPasswordEnc: enc,
		ProxyForDownload: true,
		ProxyForOnline:   true,
	}

	cfg, err := ProxyConfigFromSettings(row, PurposeDownload)
	if err != nil {
		t.Fatalf("download config: %v", err)
	}
	if cfg.Type != contract.ProxyHTTP || cfg.Host != "127.0.0.1" || cfg.Port != 8080 {
		t.Fatalf("cfg = %+v", cfg)
	}
	if cfg.Password != "s3cr3t" {
		t.Fatalf("password was not decrypted: %q", cfg.Password)
	}

	// 下载开关关闭 → 下载用途视为直连，但在线用途仍用代理。
	row.ProxyForDownload = false
	cfg, err = ProxyConfigFromSettings(row, PurposeDownload)
	if err != nil {
		t.Fatalf("download config (disabled): %v", err)
	}
	if cfg.Type != contract.ProxyNone {
		t.Fatalf("download proxy type = %s, want none", cfg.Type)
	}
	cfg, err = ProxyConfigFromSettings(row, PurposeOnline)
	if err != nil {
		t.Fatalf("online config: %v", err)
	}
	if cfg.Type != contract.ProxyHTTP {
		t.Fatalf("online proxy type = %s, want http", cfg.Type)
	}

	// 坏密文必须报错而不是静默直连。
	row.ProxyPasswordEnc = "not-a-ciphertext"
	if _, err := ProxyConfigFromSettings(row, PurposeOnline); !apierr.HasCode(err, apierr.CodeDownloadProxy) {
		t.Fatalf("bad cipher error = %v, want %s", err, apierr.CodeDownloadProxy)
	}

	// none：忽略其余字段。
	cfg, err = ProxyConfigFromSettings(&storage.AppSettings{ProxyType: contract.ProxyNone, ProxyHost: "x"}, PurposeOnline)
	if err != nil || cfg.Type != contract.ProxyNone {
		t.Fatalf("none cfg = %+v, %v", cfg, err)
	}
}

func TestNewTransportNone(t *testing.T) {
	tr, err := NewTransport(contract.ProxyConfig{Type: contract.ProxyNone}, nil)
	if err != nil {
		t.Fatalf("transport: %v", err)
	}
	if tr.Proxy != nil {
		t.Fatal("none proxy must not set Transport.Proxy")
	}
	if tr.TLSClientConfig == nil || tr.TLSClientConfig.MinVersion == 0 {
		t.Fatal("TLS minimum version not set")
	}
}

func TestNewTransportHTTPAndHTTPS(t *testing.T) {
	for _, typ := range []string{contract.ProxyHTTP, contract.ProxyHTTPS} {
		tr, err := NewTransport(contract.ProxyConfig{
			Type: typ, Host: "proxy.local", Port: 3128,
			Username: "u", Password: "p",
		}, nil)
		if err != nil {
			t.Fatalf("%s transport: %v", typ, err)
		}
		if tr.Proxy == nil {
			t.Fatalf("%s: Proxy func not set", typ)
		}
		req := &http.Request{URL: &url.URL{Scheme: "https", Host: "example.com"}}
		u, perr := tr.Proxy(req)
		if perr != nil || u == nil {
			t.Fatalf("%s: proxy url = %v, %v", typ, u, perr)
		}
		wantScheme := "http"
		if typ == contract.ProxyHTTPS {
			wantScheme = "https"
		}
		if u.Scheme != wantScheme || u.Host != "proxy.local:3128" {
			t.Fatalf("%s: proxy url = %s", typ, u.String())
		}
		if pw, _ := u.User.Password(); pw != "p" {
			t.Fatalf("%s: proxy password lost", typ)
		}
	}
}

func TestNewTransportSocks5(t *testing.T) {
	tr, err := NewTransport(contract.ProxyConfig{
		Type: contract.ProxySOCKS5, Host: "127.0.0.1", Port: 1080,
		Username: "u", Password: "p",
	}, nil)
	if err != nil {
		t.Fatalf("socks5 transport: %v", err)
	}
	if tr.DialContext == nil {
		t.Fatal("socks5 must override DialContext")
	}
	if tr.Proxy != nil {
		t.Fatal("socks5 must not set Transport.Proxy")
	}
	// 该地址上没人监听：必须返回连接错误而不是 panic。
	if _, derr := tr.DialContext(context.Background(), "tcp", "127.0.0.1:9"); derr == nil {
		t.Fatal("expected a dial error against a closed port")
	}
}

func TestNewTransportInvalid(t *testing.T) {
	if _, err := NewTransport(contract.ProxyConfig{Type: "ftp", Host: "x", Port: 1}, nil); !apierr.HasCode(err, apierr.CodeDownloadProxy) {
		t.Fatalf("unknown type error = %v", err)
	}
	if _, err := NewTransport(contract.ProxyConfig{Type: contract.ProxyHTTP, Host: "x", Port: 0}, nil); !apierr.HasCode(err, apierr.CodeDownloadProxy) {
		t.Fatalf("bad port error = %v", err)
	}
	if _, err := NewTransport(contract.ProxyConfig{Type: contract.ProxyHTTP, Host: "", Port: 80}, nil); !apierr.HasCode(err, apierr.CodeDownloadProxy) {
		t.Fatalf("empty host error = %v", err)
	}
}

func TestNoProxyMatching(t *testing.T) {
	list := splitNoProxy("example.com, .internal.lan, *.wild.dev")
	cases := map[string]bool{
		"example.com":    true,
		"api.example.com": false,
		"host.internal.lan": true,
		"internal.lan":   true,
		"other.dev":      false,
	}
	for host, want := range cases {
		if got := matchNoProxy(host, list); got != want {
			t.Errorf("matchNoProxy(%q) = %v, want %v", host, got, want)
		}
	}
	glob := splitNoProxy("*")
	if !matchNoProxy("anything.example", glob) {
		t.Error("wildcard noProxy must match everything")
	}
}

func TestHTTPClientRebuildsWhenProxyChanges(t *testing.T) {
	dir := t.TempDir()
	cfg := contract.ProxyConfig{Type: contract.ProxyNone}
	opts := testOptions(dir, "http://127.0.0.1:1")
	opts.Proxy = func() (contract.ProxyConfig, error) { return cfg, nil }
	m := newRawManager(t, opts)

	first, err := m.httpClient()
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	again, err := m.httpClient()
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	if first != again {
		t.Fatal("client must be cached while the proxy config is unchanged")
	}

	cfg = contract.ProxyConfig{Type: contract.ProxyHTTP, Host: "127.0.0.1", Port: 3128}
	second, err := m.httpClient()
	if err != nil {
		t.Fatalf("client after change: %v", err)
	}
	if second == first {
		t.Fatal("client must be rebuilt after the proxy config changes")
	}
	if tr, ok := second.Transport.(*http.Transport); !ok || tr.Proxy == nil {
		t.Fatal("rebuilt client does not carry the proxy")
	}
}

func TestProxyErrorDoesNotLeakPassword(t *testing.T) {
	_, err := NewTransport(contract.ProxyConfig{
		Type: contract.ProxySOCKS5, Host: "127.0.0.1", Port: 1080,
		Username: "u", Password: "super-secret-pw",
	}, nil)
	if err != nil {
		if strings.Contains(err.Error(), "super-secret-pw") {
			t.Fatal("password leaked into an error message")
		}
	}
}
