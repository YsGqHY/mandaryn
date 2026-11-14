package models

// 代理配置助手（FR-05-5 / ARCH §4.6.2「代理」行）。
//
// 约束：
//   - 密钥/密码只在内存里以明文存在，绝不写日志、写 state 文件或写 outbound_log；
//   - 下载与在线转写**分别开关**（ProxyForDownload / ProxyForOnline）；
//   - 代理配置每次出站前重新读取，用户改完设置无需重启。

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/proxy"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/storage"
	"vip.oberon.mandaryn/internal/utils/cryptox"
	"vip.oberon.mandaryn/internal/utils/httpx"
)

// 代理用途（决定读哪个开关）。
const (
	// PurposeDownload 模型下载。
	PurposeDownload = "download"
	// PurposeOnline 在线转写。
	PurposeOnline = "online"
)

// ProxyConfigFromSettings 把 app_settings 行转成代理配置（顺带解密密码）。
//
// purpose 对应的开关关闭时返回 Type=none，让调用方不必再判一遍。
func ProxyConfigFromSettings(row *storage.AppSettings, purpose string) (contract.ProxyConfig, error) {
	if row == nil {
		return contract.ProxyConfig{Type: contract.ProxyNone}, nil
	}
	typ := strings.ToLower(strings.TrimSpace(row.ProxyType))
	if typ == "" {
		typ = contract.ProxyNone
	}
	cfg := contract.ProxyConfig{
		Type:           typ,
		Host:           row.ProxyHost,
		Port:           row.ProxyPort,
		Username:       row.ProxyUsername,
		NoProxy:        row.ProxyNoProxy,
		UseForDownload: row.ProxyForDownload,
		UseForOnline:   row.ProxyForOnline,
	}
	if typ == contract.ProxyNone {
		return cfg, nil
	}
	if purpose == PurposeDownload && !row.ProxyForDownload {
		cfg.Type = contract.ProxyNone
		return cfg, nil
	}
	if purpose == PurposeOnline && !row.ProxyForOnline {
		cfg.Type = contract.ProxyNone
		return cfg, nil
	}
	if row.ProxyPasswordEnc != "" {
		plain, err := cryptox.DecryptString(row.ProxyPasswordEnc)
		if err != nil {
			// 坏密文不应静默走直连（那会让流量绕过用户设定的代理）。
			return cfg, apierr.New(apierr.CodeDownloadProxy).
				WithDetail("stored proxy password cannot be decrypted; re-enter it in settings").
				WithCause(err)
		}
		cfg.Password = plain
	}
	return cfg, nil
}

// NewTransport 按代理配置构造 Transport；base 为 nil 时以应用共享 Transport 为底。
func NewTransport(cfg contract.ProxyConfig, base *http.Transport) (*http.Transport, error) {
	tr := cloneTransport(base)

	switch cfg.Type {
	case "", contract.ProxyNone:
		// 显式直连：克隆来的 Transport 可能带着 http.ProxyFromEnvironment，
		// 那会让「设置为无代理」的用户仍然走系统环境代理，与设置页自相矛盾。
		// 代理只以 app_settings 为准（FR-05-5）。
		tr.Proxy = nil
		return tr, nil
	case contract.ProxyHTTP, contract.ProxyHTTPS, contract.ProxySOCKS5:
	default:
		return nil, apierr.Newf(apierr.CodeDownloadProxy, "unknown proxy type %q", cfg.Type)
	}

	if strings.TrimSpace(cfg.Host) == "" || cfg.Port <= 0 || cfg.Port > 65535 {
		return nil, apierr.New(apierr.CodeDownloadProxy).
			WithDetail("invalid proxy host or port")
	}
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))

	if cfg.Type == contract.ProxySOCKS5 {
		u := &url.URL{Scheme: "socks5", Host: addr}
		if cfg.Username != "" {
			u.User = url.UserPassword(cfg.Username, cfg.Password)
		}
		dialer, err := proxy.FromURL(u, proxy.Direct)
		if err != nil {
			return nil, apierr.New(apierr.CodeDownloadProxy).
				WithDetail("cannot build socks5 dialer").WithCause(err)
		}
		if cd, ok := dialer.(proxy.ContextDialer); ok {
			tr.DialContext = cd.DialContext
		} else {
			tr.DialContext = func(_ context.Context, network, address string) (net.Conn, error) {
				return dialer.Dial(network, address)
			}
		}
		tr.Proxy = nil
		return tr, nil
	}

	scheme := "http"
	if cfg.Type == contract.ProxyHTTPS {
		scheme = "https"
	}
	proxyURL := &url.URL{Scheme: scheme, Host: addr}
	if cfg.Username != "" {
		proxyURL.User = url.UserPassword(cfg.Username, cfg.Password)
	}
	noProxy := splitNoProxy(cfg.NoProxy)
	if len(noProxy) == 0 {
		tr.Proxy = http.ProxyURL(proxyURL)
	} else {
		tr.Proxy = func(req *http.Request) (*url.URL, error) {
			if matchNoProxy(req.URL.Hostname(), noProxy) {
				return nil, nil
			}
			return proxyURL, nil
		}
	}
	return tr, nil
}

// NewClient 构造带代理的 *http.Client（下载与在线引擎共用同一套构造逻辑）。
func NewClient(cfg contract.ProxyConfig, base *http.Transport) (*http.Client, error) {
	tr, err := NewTransport(cfg, base)
	if err != nil {
		return nil, err
	}
	return &http.Client{Transport: tr}, nil
}

// cloneTransport 克隆底 Transport，并统一连接池与 TLS 下限。
func cloneTransport(base *http.Transport) *http.Transport {
	if base == nil {
		if c := httpx.Client(); c != nil {
			if t, ok := c.Transport.(*http.Transport); ok {
				base = t
			}
		}
	}
	var tr *http.Transport
	if base != nil {
		tr = base.Clone()
	} else {
		tr = &http.Transport{}
	}
	tr.MaxIdleConns = 100
	tr.MaxIdleConnsPerHost = 16
	tr.IdleConnTimeout = 90 * time.Second
	tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	return tr
}

// splitNoProxy 解析逗号分隔的 noProxy 列表。
func splitNoProxy(v string) []string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.ToLower(strings.TrimSpace(p))
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// matchNoProxy 判断 host 是否命中 noProxy（支持 `*`、精确主机、`.suffix` 后缀）。
func matchNoProxy(host string, list []string) bool {
	host = strings.ToLower(host)
	for _, p := range list {
		switch {
		case p == "*":
			return true
		case strings.HasPrefix(p, "."):
			if strings.HasSuffix(host, p) || host == strings.TrimPrefix(p, ".") {
				return true
			}
		case host == p:
			return true
		}
	}
	return false
}

// httpClient 返回当前生效的 HTTP 客户端（按代理配置缓存，配置变了重建）。
//
// 为什么不直接用 httpx.Client()：模型下载要遵守用户设的代理（FR-05-5），
// 共享 client 是直连的。请求构造仍走 httpx.NewRequest，只替换底层 client。
func (m *Manager) httpClient() (*http.Client, error) {
	if m.opts.HTTPClient != nil {
		return m.opts.HTTPClient, nil
	}
	var cfg contract.ProxyConfig
	if m.opts.Proxy != nil {
		c, err := m.opts.Proxy()
		if err != nil {
			return nil, err
		}
		cfg = c
	}
	key := fmt.Sprintf("%s|%s|%d|%s|%s", cfg.Type, cfg.Host, cfg.Port, cfg.Username, cfg.NoProxy)

	m.clientMu.Lock()
	defer m.clientMu.Unlock()
	if m.client != nil && m.clientKey == key {
		return m.client, nil
	}
	client, err := NewClient(cfg, nil)
	if err != nil {
		m.clientErr = err
		return nil, err
	}
	m.client = client
	m.clientKey = key
	m.clientErr = nil
	return client, nil
}

// doWithClient 用指定 client 发请求，并沿用 httpx 的「>=400 即错误」语义。
func doWithClient(client *http.Client, req *http.Request) (*http.Response, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("models: http request: %w", err)
	}
	if resp.StatusCode >= 400 {
		snip, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		_ = resp.Body.Close()
		return nil, &httpx.HTTPError{
			StatusCode: resp.StatusCode,
			Status:     resp.Status,
			URL:        req.URL.String(),
			Method:     req.Method,
			BodySnip:   string(snip),
		}
	}
	return resp, nil
}
