package models

// 代理连通性测试（API-9.4）。
//
// 设计依据：docs/API.md API-9.4「测试代理连通」与 §2.9.1 的代理契约。
//
// 为什么放在 L2 models 而不是直接写在 appsettings 薄壳里：
//   - 它必须复用**同一套** Transport 构造（`NewTransport`），否则测通了也不代表
//     真正的下载链路能通 —— 测的和用的是两份实现，是最常见的假阳性来源；
//   - 它要打的是**真实目标**（清单地址），这样结论才对应「用户真正会走的路径」；
//   - L2 不 import Wails，可被单测直接驱动（httptest 起个转发型代理即可）。
//
// 安全约束（与 proxy.go 一致）：
//   - 错误与 Note **绝不**回显密码或完整 URL；
//   - 密码只用于 `Proxy-Authorization`，不进日志、不进 outbound_log；
//   - 离线开关优先：开启时在发请求**之前**拒绝（`ERR_OFFLINE_ENFORCED`）。

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/outbound"
	"vip.oberon.mandaryn/internal/utils/httpx"
)

// ProxyTestTimeoutMs 代理连通性测试的超时（毫秒）。
//
// 取值理由：这是**交互式**按钮，用户会盯着看。30s 是「慢到能覆盖一次
// 握手 + 首字节」与「不至于让人以为卡死」的折中；超时后明确归类为 timeout
// 而不是笼统的 network failures。
const ProxyTestTimeoutMs int64 = 15_000

// TestProxy 用给定的代理配置探测一次**真实**出站，返回连通性结论。
//
// 语义（与 API-9.4 / API-5.4 的 TestResult 对齐）：
//   - 只看「能否经该代理拿到目标响应」，不要求业务载荷正确 —— 拿得到 2xx/3xx
//     即视为链路连通；HTTP 4xx/5xx 归为 proxy/network 之外的类别；
//   - 返回的 error 只在**配置本身**非法时非 nil（例如未知 scheme），
//     因为那是调用方必须先改的东西；网络失败以 OK=false + Category 表达，
//     便于前端稳定地渲染「测不通但配置没错」。
//
// cfg 是**待测试**的配置，不落库（保存由 SetProxy 负责）。
func (m *Manager) TestProxy(ctx context.Context, cfg contract.ProxyConfig) (contract.TestResult, error) {
	// 1) 配置校验：未知类型直接拒绝，不做「悄悄直连」的猜测。
	tr, err := NewTransport(cfg, nil)
	if err != nil {
		return contract.TestResult{Category: "proxy", Note: proxyNote(err)}, err
	}

	// 2) 离线闸门：必须在发请求之前（FR-08-5「零出站」）。
	if gerr := outbound.Guard(); gerr != nil {
		return contract.TestResult{
			Category: "network",
			Note:     "离线模式已开启，未发起任何请求",
		}, apierr.From(gerr, apierr.CodeOfflineEnforced)
	}

	// 3) 目标：清单地址。清单拿不到，下载就无从谈起，所以它是下载链路最近的代表。
	target := catalogURL(m.manifestBase())
	if target == "" {
		return contract.TestResult{Category: "unknown", Note: "未配置模型清单基址，无法测试"},
			apierr.New(apierr.CodeInvalidArgument).WithDetail("未配置模型清单基址，无法进行代理连通性测试")
	}

	reqCtx, cancel := context.WithTimeout(ctx, time.Duration(ProxyTestTimeoutMs)*time.Millisecond)
	defer cancel()
	req, cancelReq, rerr := httpx.NewRequest(reqCtx, http.MethodGet, target, nil)
	if rerr != nil {
		return contract.TestResult{Category: "unknown", Note: proxyNote(rerr)}, nil
	}
	defer cancelReq()
	// 只取头部就够判断链路，不下整包。
	req.Header.Set("Range", "bytes=0-0")

	client := &http.Client{Transport: tr}
	start := time.Now()
	resp, derr := client.Do(req)
	latency := time.Since(start).Milliseconds()

	if derr != nil {
		// 目标域名可以留，凭据与完整 URL 不能留。
		m.recordOutbound(target, 0, apierr.CodeDownloadNetwork)
		return contract.TestResult{
			LatencyMs: latency,
			Category:  classifyProxyError(derr),
			Note:      proxyNote(derr),
		}, nil
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		_ = resp.Body.Close()
	}()

	// 连通了：2xx/3xx 才算通。
	//
	// 4xx/5xx 说明「链路通了、但这一跳返回错误」，必须按状态码细分而不能一律
	// 归成 network：407 是**代理认证**问题（用户要改密码），429 是限流（稍后重试），
	// 二者与「连不上」给用户的建议完全不同。
	if resp.StatusCode >= 400 {
		m.recordOutbound(target, 0, apierr.CodeDownloadNetwork)
		return contract.TestResult{
			OK:        false,
			LatencyMs: latency,
			Category:  classifyProxyStatus(resp.StatusCode),
			Note:      "目标返回 " + resp.Status,
		}, nil
	}

	m.recordOutbound(target, 0, "ok")
	return contract.TestResult{OK: true, LatencyMs: latency, Category: "", Note: ""}, nil
}

// classifyProxyStatus 把「拿到了响应但状态码 >=400」的情况归到固定类别上。
//
// 与 classifyProxyError 分开：那是「没拿到响应」的分类，这里是「拿到了错误响应」。
func classifyProxyStatus(code int) string {
	switch code {
	case http.StatusProxyAuthRequired:
		return "auth"
	case http.StatusTooManyRequests:
		return "rate-limit"
	default:
		return "network"
	}
}

// classifyProxyError 把底层错误归到 TestResult.Category 的固定取值上。
//
// 分类口径与 contract.TestResult 注释一致：network | timeout | auth | rate-limit | proxy | tls | unknown。
func classifyProxyError(err error) string {
	var httpErr *httpx.HTTPError
	if errors.As(err, &httpErr) {
		return classifyProxyStatus(httpErr.StatusCode)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	msg := err.Error()
	switch {
	case containsAny(msg, "certificate", "x509", "tls:", "TLS handshake"):
		return "tls"
	case containsAny(msg, "407", "Proxy Authentication", "proxyconnect"):
		return "proxy"
	case containsAny(msg, "timeout", "deadline exceeded"):
		return "timeout"
	}
	return "network"
}
//
// 为什么不直接用 err.Error()：*url.Error 的文本里带着含密码的完整 URL，
// 而这条 Note 会经 L3 直接回到前端展示。只保留「错误类型 + 目标主机」级别信息。
func proxyNote(err error) string {
	if err == nil {
		return ""
	}
	var httpErr *httpx.HTTPError
	if errors.As(err, &httpErr) {
		return "目标返回 " + httpErr.Status
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "请求超时"
	}
	// 兜底：不回显 URL，只给类别化的通用说明。
	return "无法经该代理建立连接（" + classifyProxyError(err) + "）"
}

// containsAny 判断 s 是否包含任意一个子串（错误文本比对，大小写敏感）。
func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if sub != "" && strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
