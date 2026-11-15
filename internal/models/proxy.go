package models

// 代理配置助手（FR-05-5 / ARCH §4.6.2「代理」行）。
//
// 两套来源，优先级从高到低：
//  1. **全局代理**（环境变量 `MANDARYN_HTTP_PROXY`）：模型下载与模型清单源获取
//     **共用同一份设置**，不分别配置；运维级接管，配了就不用设置页。
//  2. **设置页**（`app_settings.proxy*`）：按「用于下载 / 用于在线转写」分别开关。
//  3. 都没有 → 直连（`Type=none`，与历史行为一致）。
//
// 约束：
//   - 密钥/密码只在内存里以明文存在，绝不写日志、写 state 文件或写 outbound_log；
//   - 代理配置每次出站前重新读取，改完设置 / 改完环境变量无需重启；
//   - 配置存在但非法（坏密文 / 坏 URL）时**报错而不静默直连** —— 那会让用户
//     以为代理已生效，实际流量裸奔。

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
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

// 全局代理配置项（环境变量形式，运维级统一入口）。
//
// 作用范围：**模型下载**与**模型清单源获取**共用同一份设置 —— 两者都经由
// Manager.httpClient() 出站，因此「配一处、两处生效」，不存在按用途各配一次。
// 在线转写不在此列，仍由设置页的「用于在线转写」开关单独控制（FR-05-5）。
const (
	// EnvHTTPProxy 全局 HTTP 代理地址，标准 URL 形式：
	//   http://host:port
	//   http://user:pass@host:port     内嵌凭据（密码可含百分号转义）
	//   https://host:port              以 TLS 连接代理
	//   socks5://[user:pass@]host:port SOCKS5 代理
	// 为空（未设 / 设为空串）时保持原有行为：直连，或按设置页配置走代理。
	EnvHTTPProxy = "MANDARYN_HTTP_PROXY"
	// EnvHTTPNoProxy 全局代理的直连例外，逗号分隔主机（支持 `*` 与 `.suffix`）。
	EnvHTTPNoProxy = "MANDARYN_HTTP_NO_PROXY"
)

// ProxyConfigFromURL 把标准代理 URL 解析成 contract.ProxyConfig。
//
// raw 为空表示「未配置代理」，返回 Type=none（调用方按直连处理）。
// 省略 scheme 的 `host:port` 按 http 代理处理，而不是当成非法值 —— 那样会
// 静默回落到直连，用户以为走了代理而流量实际裸奔。
//
// 安全：错误信息**绝不回显原始 URL**，也不把 url.Parse 的原始错误挂成 cause
// —— *url.Error 的文本里带着含密码的完整 URL，而本包的失败路径会打日志。
func ProxyConfigFromURL(raw, noProxy string) (contract.ProxyConfig, error) {
	noProxy = strings.TrimSpace(noProxy)
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return contract.ProxyConfig{Type: contract.ProxyNone, NoProxy: noProxy}, nil
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, parseErr := url.Parse(raw)
	if parseErr != nil || u == nil {
		return contract.ProxyConfig{}, apierr.New(apierr.CodeDownloadProxy).
			WithDetail("invalid proxy url; expected http://host:port with optional user:pass@")
	}

	typ := strings.ToLower(strings.TrimSpace(u.Scheme))
	if typ == "socks5h" {
		// socks5h 只是「DNS 也交给代理解析」的别名，落在同一实现上。
		typ = contract.ProxySOCKS5
	}
	switch typ {
	case contract.ProxyHTTP, contract.ProxyHTTPS, contract.ProxySOCKS5:
	default:
		return contract.ProxyConfig{}, apierr.Newf(apierr.CodeDownloadProxy,
			"unsupported proxy scheme %q; use http, https or socks5", u.Scheme)
	}

	host := strings.TrimSpace(u.Hostname())
	if host == "" {
		return contract.ProxyConfig{}, apierr.New(apierr.CodeDownloadProxy).
			WithDetail("proxy url has no host")
	}
	portStr := u.Port()
	if portStr == "" {
		portStr = strconv.Itoa(defaultProxyPort(typ))
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		return contract.ProxyConfig{}, apierr.New(apierr.CodeDownloadProxy).
			WithDetail("proxy url has an invalid port")
	}
	// 代理 URL 只允许 scheme://[userinfo@]host[:port][/]。
	// 出现 path / query 通常是把 PAC 地址或目标地址误填成代理，明确拒绝而不是猜。
	if p := strings.TrimSuffix(u.Path, "/"); p != "" || u.RawQuery != "" || u.Fragment != "" {
		return contract.ProxyConfig{}, apierr.New(apierr.CodeDownloadProxy).
			WithDetail("proxy url must not contain a path, query or fragment")
	}

	cfg := contract.ProxyConfig{
		Type:    typ,
		Host:    host,
		Port:    port,
		NoProxy: noProxy,
		// 这条来源只服务下载与清单获取；在线转写是否走代理仍看设置页开关。
		UseForDownload: true,
		UseForOnline:   false,
	}
	if u.User != nil {
		name := strings.TrimSpace(u.User.Username())
		pass, _ := u.User.Password()
		if name == "" {
			// `http://:pass@host` 这种写法会让认证信息被静默丢掉，直接报错。
			return contract.ProxyConfig{}, apierr.New(apierr.CodeDownloadProxy).
				WithDetail("proxy url has a password but no username")
		}
		cfg.Username = name
		cfg.Password = pass
	}
	return cfg, nil
}

// defaultProxyPort 返回各 scheme 的标准端口（URL 里省略端口时使用）。
func defaultProxyPort(typ string) int {
	switch typ {
	case contract.ProxyHTTPS:
		return 443
	case contract.ProxySOCKS5:
		return 1080
	default:
		return 80
	}
}

// globalProxyFromEnv 读取全局代理配置；ok=false 表示**未配置**，调用方应回落到
// 设置页（或直连）。配置了但解析失败时返回错误，绝不静默直连。
func globalProxyFromEnv() (cfg contract.ProxyConfig, ok bool, err error) {
	raw := strings.TrimSpace(os.Getenv(EnvHTTPProxy))
	if raw == "" {
		return contract.ProxyConfig{Type: contract.ProxyNone}, false, nil
	}
	cfg, err = ProxyConfigFromURL(raw, os.Getenv(EnvHTTPNoProxy))
	if err != nil {
		return contract.ProxyConfig{}, true, err
	}
	return cfg, true, nil
}

// secretFingerprint 把密码折成短摘要，只用于「代理配置是否变了」的比较。
//
// 为什么不直接拿明文当 key：clientKey 会随 Manager 一直留在内存里，
// 没必要为此多留一份明文密码；摘要也不含可逆信息，误入日志也无法还原。
func secretFingerprint(s string) string {
	if s == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

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

// effectiveProxyConfig 解析当前生效的代理配置（每次出站前调用，改配置无需重启）。
//
// 优先级：
//  1. 全局代理 MANDARYN_HTTP_PROXY —— 下载与清单获取共用这一份；
//  2. Options.Proxy（设置页 proxy_*，由 L3 按「用于下载」开关裁决）；
//  3. 都没有 → Type=none，直连（与未配置代理时的历史行为一致）。
func (m *Manager) effectiveProxyConfig() (contract.ProxyConfig, error) {
	cfg, ok, err := globalProxyFromEnv()
	if err != nil {
		return contract.ProxyConfig{}, err
	}
	if ok {
		return cfg, nil
	}
	if m.opts.Proxy == nil {
		return contract.ProxyConfig{Type: contract.ProxyNone}, nil
	}
	return m.opts.Proxy()
}

// httpClient 返回当前生效的 HTTP 客户端（按代理配置缓存，配置变了重建）。
//
// 为什么不直接用 httpx.Client()：模型下载与清单获取要遵守用户 / 运维设的代理
// （FR-05-5），共享 client 是直连的。请求构造仍走 httpx.NewRequest，
// 只替换底层 client；Transport 的连接池与 TLS 下限由 NewTransport 统一。
func (m *Manager) httpClient() (*http.Client, error) {
	if m.opts.HTTPClient != nil {
		return m.opts.HTTPClient, nil
	}
	cfg, err := m.effectiveProxyConfig()
	if err != nil {
		return nil, err
	}
	// 密码只以摘要进 key：改密码（或改全局代理 URL）同样要重建 client，
	// 否则旧 Transport 会把旧凭据一直用下去。
	key := fmt.Sprintf("%s|%s|%d|%s|%s|%s",
		cfg.Type, cfg.Host, cfg.Port, cfg.Username, cfg.NoProxy, secretFingerprint(cfg.Password))

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
