package asr

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/outbound"
	"vip.oberon.mandaryn/internal/utils/httpx"
	"vip.oberon.mandaryn/internal/utils/logx"
)

// 在线引擎的默认参数。
const (
	// DefaultOnlineTimeoutMs 首字节超时（ARCH §4.2.4：请求超 8s 无首字节判超时）。
	DefaultOnlineTimeoutMs int64 = 8000
	// probeTimeout 握手探测的短超时：探测只用来判断「能不能连上」，不该等 8s。
	probeTimeout = 5 * time.Second
	// maxResponseBytes 转写响应上限（1 段文本，1MB 足够）。
	maxResponseBytes int64 = 1 << 20
)

// OnlineOptions 是在线引擎的装配参数（由 enginesvc 从 appsettings 读出后传入）。
//
// 注意：APIKey 必须是**明文**。解密在 enginesvc 层完成，本包不接触密文，
// 也绝不把密钥写进日志 / 错误 Detail（NFR-03-5）。
type OnlineOptions struct {
	// Endpoint 端点根地址（必须 https），例如 https://api.openai.com。
	Endpoint string
	// Model 端点模型名。
	Model string
	// Language auto | zh | en | ...（auto 时不发该字段）。
	Language string
	// APIKey 明文密钥；空表示不鉴权（自建兼容端点常见）。
	APIKey string
	// TimeoutMs 首字节超时；0 用 DefaultOnlineTimeoutMs。
	TimeoutMs int64
	// Client 可注入的 HTTP 客户端（代理感知）；nil 时用 httpx.Client()。
	Client *http.Client
	// ProxyURL 代理地址（http/https/socks5）；非空时基于 httpx.Client() 克隆
	// 一个带 Proxy 的 Transport（Client 非空时以 Client 为准）。
	ProxyURL string
	// Recorder 出站留痕（可为 nil，此时只受离线开关约束、不落库）。
	Recorder *outbound.Recorder
	// RetryAfterCap 429 的 Retry-After 上限；0 用 DefaultRetryAfterCap。
	RetryAfterCap time.Duration
}

// OnlineEngine 是 OpenAI 兼容「/v1/audio/transcriptions」端点的引擎实现。
//
// 非流式：延后转写模式下每次 Finalize 一次性上传整句 WAV。
// 「边录边转」要流式时走另一路（websocket / 增量上传），不在此实现范围。
type OnlineEngine struct {
	opt    OnlineOptions
	client *http.Client
	base   string // 去掉尾部斜杠的端点根地址
	host   string // 用于出站留痕的域名

	mu     sync.RWMutex
	state  string // ready | loading | failed | paused
	loadMs int64
	last   *apierr.Err
	closed bool
}

// NewOnlineEngine 构造在线引擎。只做**参数校验**，不联网（联网发生在 Warm / Finalize）。
func NewOnlineEngine(opt OnlineOptions) (*OnlineEngine, error) {
	ep := strings.TrimSpace(opt.Endpoint)
	if ep == "" {
		return nil, apierr.New(apierr.CodeInvalidArgument).WithDetail("在线端点未配置")
	}
	u, err := url.Parse(ep)
	if err != nil || u.Host == "" {
		return nil, apierr.New(apierr.CodeInvalidArgument).WithDetail("在线端点不是合法 URL")
	}
	if u.Scheme != "https" {
		// 传输层保护不可关闭（ARCH §8.2）；http 端点会让音频明文出机。
		return nil, apierr.New(apierr.CodeInvalidArgument).WithDetail("在线端点必须使用 https")
	}
	if strings.TrimSpace(opt.Model) == "" {
		return nil, apierr.New(apierr.CodeInvalidArgument).WithDetail("在线模型名未配置")
	}
	if opt.TimeoutMs <= 0 {
		opt.TimeoutMs = DefaultOnlineTimeoutMs
	}
	if opt.RetryAfterCap <= 0 {
		opt.RetryAfterCap = DefaultRetryAfterCap
	}

	client := opt.Client
	if client == nil {
		if strings.TrimSpace(opt.ProxyURL) != "" {
			c, perr := clientWithProxy(opt.ProxyURL)
			if perr != nil {
				return nil, perr
			}
			client = c
		} else {
			client = httpx.Client()
		}
	}

	return &OnlineEngine{
		opt:    opt,
		client: client,
		base:   strings.TrimRight(ep, "/"),
		host:   u.Host,
		state:  contract.EngineStateLoading,
	}, nil
}

// clientWithProxy 基于 httpx.Client() 克隆一个带代理的客户端。
//
// 只改 Transport.Proxy，其余（连接池、证书校验）保持不变 ——
// 不提供「跳过证书校验」开关（ARCH §8.2）。
func clientWithProxy(proxyURL string) (*http.Client, error) {
	pu, err := url.Parse(strings.TrimSpace(proxyURL))
	if err != nil || pu.Host == "" {
		return nil, apierr.New(apierr.CodeDownloadProxy).WithDetail("代理地址不合法")
	}
	base := httpx.Client()
	tr, ok := base.Transport.(*http.Transport)
	if !ok || tr == nil {
		tr = &http.Transport{}
	} else {
		tr = tr.Clone()
	}
	tr.Proxy = http.ProxyURL(pu)
	return &http.Client{Transport: tr}, nil
}

// ID 返回 online:<model>。
func (e *OnlineEngine) ID() string {
	return "online:" + strings.TrimSpace(e.opt.Model)
}

// Warm 做握手探测（GET {base}/v1/models）。
//
// 探测语义：
//   - 200 → ready；
//   - 401/403 → 鉴权失败（ERR_ENGINE_ONLINE_AUTH），不重试；
//   - 404/405 → 端点没有 models 接口，但**说明连通**，视为 ready
//     （很多 OpenAI 兼容自建端点只实现了 transcriptions）；
//   - 其它 4xx/5xx 按其类别报错。
func (e *OnlineEngine) Warm(ctx context.Context) error {
	if err := outbound.Guard(); err != nil {
		apiErr := apierr.From(err, apierr.CodeOfflineEnforced)
		e.fail(FailBlocked, apiErr)
		return &EngineError{Kind: FailBlocked, Err: apiErr}
	}
	started := time.Now()
	res := e.probe(ctx)
	elapsed := time.Since(started)

	if res.Err != nil {
		e.fail(res.Kind, res.Err)
		return &EngineError{Kind: res.Kind, Err: res.Err, RetryAfter: res.RetryAfter}
	}
	e.mu.Lock()
	e.state = contract.EngineStateReady
	e.loadMs = elapsed.Milliseconds()
	e.last = nil
	e.mu.Unlock()

	logx.For("asr").Info("在线引擎握手成功",
		"host", e.host, "model", e.opt.Model, "latencyMs", elapsed.Milliseconds())
	return nil
}

// probeResult 是探测结果（Warm 与 enginesvc.TestOnline 共用）。
type probeResult struct {
	Kind       FailureKind
	Err        *apierr.Err
	Category   string
	LatencyMs  int64
	RetryAfter time.Duration
	Note       string
}

// probe 探测端点连通性（不重试，重试策略在 Router 层）。
func (e *OnlineEngine) probe(ctx context.Context) probeResult {
	if err := outbound.Guard(); err != nil {
		return probeResult{
			Kind:     FailBlocked,
			Err:      apierr.From(err, apierr.CodeOfflineEnforced),
			Category: "unknown",
			Note:     "离线模式已开启，出站请求被拦截",
		}
	}
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	var respBody []byte
	started := time.Now()
	code, err := e.do(pctx, http.MethodGet, e.base+"/v1/models", nil, "", &respBody)
	latency := time.Since(started).Milliseconds()

	if err != nil {
		kind, apiErr, cat := classifyTransportError(err)
		e.record(0, apiErr.Code)
		return probeResult{Kind: kind, Err: apiErr, Category: cat, LatencyMs: latency,
			Note: "无法连接在线端点"}
	}

	switch {
	case code >= 200 && code < 300:
		e.record(int64(len(respBody)), "ok")
		return probeResult{LatencyMs: latency, Note: "连接正常"}
	case code == http.StatusNotFound || code == http.StatusMethodNotAllowed:
		// 端点没有 /v1/models：不代表不可用，只要连得上就算 ready。
		e.record(int64(len(respBody)), "ok")
		return probeResult{LatencyMs: latency, Note: "端点未提供 /v1/models，已按连通处理"}
	default:
		kind, apiErr, cat := classifyStatus(code, respBody)
		e.record(int64(len(respBody)), apiErr.Code)
		return probeResult{Kind: kind, Err: apiErr, Category: cat, LatencyMs: latency,
			Note: apiErr.Message}
	}
}

// ProbeOnline 探测一次在线端点，供 enginesvc.TestOnline（API-5.4）使用。
//
// 返回结构对齐 contract.TestResult（OK / LatencyMs / Category / Note）；
// 同时把分类后的错误作为 error 返回，让前端能按 ERR_ENGINE_ONLINE_* 弹「下一步动作」。
func ProbeOnline(ctx context.Context, opt OnlineOptions) (contract.TestResult, error) {
	eng, err := NewOnlineEngine(opt)
	if err != nil {
		apiErr := apierr.From(err, apierr.CodeInvalidArgument)
		return contract.TestResult{OK: false, Category: "unknown", Note: apiErr.Message}, apiErr
	}
	res := eng.probe(ctx)
	if res.Err != nil {
		return contract.TestResult{
			OK:        false,
			LatencyMs: res.LatencyMs,
			Category:  res.Category,
			Note:      res.Note,
		}, res.Err
	}
	return contract.TestResult{
		OK:        true,
		LatencyMs: res.LatencyMs,
		Category:  res.Category,
		Note:      res.Note,
	}, nil
}

// Feed 在延后转写模式下无流式能力：返回已关闭的空 channel。
//
// 与 local 引擎保持一致：上层（S4 recording）在延后转写模式下不会调用 Feed；
// 「边录边转」（FR-03-10，默认关）打开后才会改用流式实现。
func (e *OnlineEngine) Feed(frame []float32) (<-chan Partial, error) {
	ch := make(chan Partial)
	close(ch)
	return ch, nil
}

// Finalize 把整句音频编码为 WAV 并 POST 到 /v1/audio/transcriptions。
func (e *OnlineEngine) Finalize(seg AudioSegment) (FinalResult, error) {
	if err := outbound.Guard(); err != nil {
		apiErr := apierr.From(err, apierr.CodeOfflineEnforced)
		e.fail(FailBlocked, apiErr)
		return FinalResult{}, &EngineError{Kind: FailBlocked, Err: apiErr}
	}
	if e.isClosed() {
		return FinalResult{}, &EngineError{
			Kind: FailLocal,
			Err:  apierr.New(apierr.CodeEngineNotReady).WithDetail("在线引擎已关闭"),
		}
	}

	wav := encodeWAV(seg.Samples, seg.SampleRateOrDefault())
	body, contentType, err := buildTranscriptionMultipart(wav, e.opt.Model, e.opt.Language)
	if err != nil {
		apiErr := apierr.From(err, apierr.CodeEngineOnlineRejected)
		e.fail(FailRejected, apiErr)
		return FinalResult{}, &EngineError{Kind: FailRejected, Err: apiErr}
	}

	timeout := time.Duration(e.opt.TimeoutMs) * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	started := time.Now()
	var respBody []byte
	code, err := e.do(ctx, http.MethodPost, e.base+"/v1/audio/transcriptions",
		body, contentType, &respBody)
	elapsed := time.Since(started)

	if err != nil {
		kind, apiErr, _ := classifyTransportError(err)
		e.record(0, apiErr.Code)
		e.fail(kind, apiErr)
		logx.For("asr").Warn("在线转写传输失败",
			"host", e.host, "kind", string(kind), "code", apiErr.Code)
		return FinalResult{}, &EngineError{Kind: kind, Err: apiErr}
	}

	if code < 200 || code >= 300 {
		kind, apiErr, _ := classifyStatus(code, respBody)
		retryAfter := parseRetryAfter(respBody, e.opt.RetryAfterCap)
		e.record(int64(len(respBody)), apiErr.Code)
		e.fail(kind, apiErr)
		logx.For("asr").Warn("在线转写被拒绝",
			"host", e.host, "status", code, "code", apiErr.Code)
		return FinalResult{}, &EngineError{Kind: kind, Err: apiErr, RetryAfter: retryAfter}
	}

	text, perr := parseTranscriptionResponse(respBody)
	if perr != nil {
		apiErr := apierr.New(apierr.CodeEngineOnlineRejected).WithDetail("端点响应格式无法解析")
		e.record(int64(len(respBody)), apiErr.Code)
		e.fail(FailRejected, apiErr)
		return FinalResult{}, &EngineError{Kind: FailRejected, Err: apiErr}
	}

	e.record(int64(len(wav)), "ok")
	e.mu.Lock()
	e.last = nil
	e.state = contract.EngineStateReady
	e.mu.Unlock()

	return FinalResult{
		Text:      text.Text,
		EngineID:  e.ID(),
		Lang:      text.Lang,
		ElapsedMs: elapsed.Milliseconds(),
	}, nil
}

// Health 返回当前状态。
func (e *OnlineEngine) Health(_ context.Context) HealthStatus {
	e.mu.RLock()
	defer e.mu.RUnlock()
	note := ""
	if e.last != nil {
		note = e.last.Message
	}
	return HealthStatus{
		State:    e.state,
		EngineID: e.ID(),
		LoadMs:   e.loadMs,
		Note:     note,
		Err:      e.last,
	}
}

// Close 释放资源（在线引擎无本地资源，只置状态并禁止后续调用）。
func (e *OnlineEngine) Close() error {
	e.mu.Lock()
	e.closed = true
	e.state = contract.EngineStatePaused
	e.mu.Unlock()
	return nil
}

// —— 内部工具 ——

func (e *OnlineEngine) isClosed() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.closed
}

// fail 记录最后一次错误并把状态置为 failed（不改 Degraded，那是 Router 的职责）。
func (e *OnlineEngine) fail(_ FailureKind, err *apierr.Err) {
	e.mu.Lock()
	e.last = err
	if e.state != contract.EngineStatePaused {
		e.state = contract.EngineStateFailed
	}
	e.mu.Unlock()
}

// record 落出站留痕。audio=true：请求体含音频。
//
// 故意不因留痕失败而影响转写（NFR-04-1 同理）。
func (e *OnlineEngine) record(bytes int64, result string) {
	if e.opt.Recorder == nil {
		return
	}
	e.opt.Recorder.Record(e.host, contract.OutboundOnlineASR, true, bytes, result)
}

// do 发一次请求（走 httpx，不用 net/http 默认 client）。
//
// body 为 nil 时发空体；返回 statusCode，错误只表示**传输层**失败，4xx/5xx 不算错误。
func (e *OnlineEngine) do(ctx context.Context, method, fullURL string, body []byte,
	contentType string, out *[]byte) (int, error) {

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, cancel, err := httpx.NewRequest(ctx, method, fullURL, reader)
	if err != nil {
		return 0, err
	}
	defer cancel()

	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if strings.TrimSpace(e.opt.APIKey) != "" {
		req.Header.Set("Authorization", "Bearer "+e.opt.APIKey)
	}

	// 这里用**注入的 client** 而不是 httpx.Do：
	// httpx.Do 固定使用进程共享 client，无法承载「代理感知」与测试注入。
	// 但 client 的来源仍是 httpx.Client()（或其克隆），绝不 new(http.Client{})。
	resp, err := e.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("httpx: do request: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		_ = resp.Body.Close()
	}()

	if out != nil {
		buf, rerr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		if rerr != nil {
			return resp.StatusCode, fmt.Errorf("httpx: read body: %w", rerr)
		}
		*out = buf
	}
	return resp.StatusCode, nil
}

// classifyTransportError 把传输层错误分类成矩阵的类别。
func classifyTransportError(err error) (FailureKind, *apierr.Err, string) {
	if err == nil {
		return FailServer, apierr.New(apierr.CodeEngineOnlineRejected), "unknown"
	}
	// 超时：context deadline 或 net.Error.Timeout()。
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return FailTimeout, apierr.New(apierr.CodeEngineOnlineTimeout).WithCause(err), "timeout"
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return FailTimeout, apierr.New(apierr.CodeEngineOnlineTimeout).WithCause(err), "timeout"
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "proxyconnect") || strings.Contains(msg, "proxy"):
		return FailNetwork, apierr.New(apierr.CodeEngineOnlineNetwork).
			WithDetail("代理不可用").WithCause(err), "proxy"
	case strings.Contains(msg, "x509") || strings.Contains(msg, "tls:"):
		return FailNetwork, apierr.New(apierr.CodeEngineOnlineNetwork).
			WithDetail("TLS 校验失败").WithCause(err), "tls"
	default:
		return FailNetwork, apierr.New(apierr.CodeEngineOnlineNetwork).WithCause(err), "network"
	}
}

// classifyStatus 把 HTTP 状态码分类成矩阵的类别（ARCH §4.2.4）。
//
// body 只用来抽取端点给的错误摘要（截断），**不**包含音频或密钥。
func classifyStatus(code int, body []byte) (FailureKind, *apierr.Err, string) {
	snip := bodySnippet(body)
	switch {
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		e := apierr.New(apierr.CodeEngineOnlineAuth)
		if snip != "" {
			e = e.WithDetail("端点返回 %d: %s", code, snip)
		}
		return FailAuth, e, "auth"
	case code == http.StatusTooManyRequests:
		e := apierr.New(apierr.CodeEngineOnlineRateLimit)
		if snip != "" {
			e = e.WithDetail("端点返回 429: %s", snip)
		}
		return FailRateLimit, e, "rate-limit"
	case code >= 500:
		// 端点 5xx 有专用码：语义是「对方服务端出错，可重试并建议切来源」，
		// 与「网络不可达」（ERR_ENGINE_ONLINE_NETWORK）区分开，
		// 否则用户按「检查网络」排障会白忙。
		e := apierr.New(apierr.CodeEngineOnlineServer).WithDetail("端点返回 %d", code)
		if snip != "" {
			e = e.WithDetail("端点返回 %d: %s", code, snip)
		}
		return FailServer, e, "server"
	default:
		e := apierr.New(apierr.CodeEngineOnlineRejected).WithDetail("端点返回 %d", code)
		if snip != "" {
			e = e.WithDetail("端点返回 %d: %s", code, snip)
		}
		return FailRejected, e, "unknown"
	}
}

// bodySnippet 取响应体前 200 字符用于诊断（丢弃换行，避免把整段 JSON 塞进日志）。
func bodySnippet(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	s := strings.Join(strings.Fields(string(body)), " ")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// parseRetryAfter 从响应体里尽力抽取 Retry-After 秒数（端点不统一，body 里常见）。
//
// 注意：真正的绍上游 Retry-After 响应头在 do() 里没有暴露出来，
// 因此这里退化为解析 body；上限由 opt.RetryAfterCap 兜住（ARCH §4.2.4：≤30s）。
func parseRetryAfter(body []byte, limit time.Duration) time.Duration {
	if limit <= 0 {
		limit = DefaultRetryAfterCap
	}
	var payload struct {
		RetryAfter float64 `json:"retry_after"`
		Error      struct {
			RetryAfter float64 `json:"retry_after"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err == nil {
		secs := payload.RetryAfter
		if secs == 0 {
			secs = payload.Error.RetryAfter
		}
		if secs > 0 {
			d := time.Duration(secs * float64(time.Second))
			if d > limit {
				d = limit
			}
			return d
		}
	}
	// 兼容纯数字响应体（部分网关只回 "3"）。
	if n, err := strconv.Atoi(strings.TrimSpace(string(body))); err == nil && n > 0 {
		d := time.Duration(n) * time.Second
		if d > limit {
			d = limit
		}
		return d
	}
	return 0
}

// transcriptionResponse 覆盖常见 OpenAI 兼容返回形态。
type transcriptionResponse struct {
	Text     string `json:"text"`
	Language string `json:"language"`
	// 部分端点把结果包在 data 里。
	Data struct {
		Text string `json:"text"`
	} `json:"data"`
}

type transcriptionText struct {
	Text string
	Lang string
}

func parseTranscriptionResponse(body []byte) (transcriptionText, error) {
	var r transcriptionResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return transcriptionText{}, err
	}
	text := r.Text
	if strings.TrimSpace(text) == "" {
		text = r.Data.Text
	}
	if strings.TrimSpace(text) == "" && !bytes.Contains(body, []byte(`"text"`)) {
		return transcriptionText{}, errors.New("asr: 响应里没有 text 字段")
	}
	return transcriptionText{Text: strings.TrimSpace(text), Lang: r.Language}, nil
}

// buildTranscriptionMultipart 组 multipart/form-data：model / language / file。
func buildTranscriptionMultipart(wav []byte, model, language string) ([]byte, string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	if err := mw.WriteField("model", model); err != nil {
		return nil, "", err
	}
	if language != "" && language != "auto" {
		if err := mw.WriteField("language", language); err != nil {
			return nil, "", err
		}
	}
	if err := mw.WriteField("response_format", "json"); err != nil {
		return nil, "", err
	}

	h := make(map[string][]string)
	h["Content-Disposition"] = []string{`form-data; name="file"; filename="audio.wav"`}
	h["Content-Type"] = []string{"audio/wav"}
	part, err := mw.CreatePart(h)
	if err != nil {
		return nil, "", err
	}
	if _, err := part.Write(wav); err != nil {
		return nil, "", err
	}
	if err := mw.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), mw.FormDataContentType(), nil
}

// encodeWAV 把 float32 单声道样本编码成 16-bit PCM WAV（标准 44 字节头）。
//
// 用标准库手写而不是引入音频库：只需一个格式，交给端点解析。
func encodeWAV(samples []float32, sampleRate int) []byte {
	if sampleRate <= 0 {
		sampleRate = 16000
	}
	const bitsPerSample = 16
	const numChannels = 1
	dataLen := len(samples) * 2

	buf := make([]byte, 44+dataLen)
	copy(buf[0:4], "RIFF")
	binary.LittleEndian.PutUint32(buf[4:8], uint32(36+dataLen))
	copy(buf[8:12], "WAVE")

	copy(buf[12:16], "fmt ")
	binary.LittleEndian.PutUint32(buf[16:20], 16) // fmt chunk size
	binary.LittleEndian.PutUint16(buf[20:22], 1)  // PCM
	binary.LittleEndian.PutUint16(buf[22:24], numChannels)
	binary.LittleEndian.PutUint32(buf[24:28], uint32(sampleRate))
	byteRate := sampleRate * numChannels * bitsPerSample / 8
	binary.LittleEndian.PutUint32(buf[28:32], uint32(byteRate))
	binary.LittleEndian.PutUint16(buf[32:34], numChannels*bitsPerSample/8) // block align
	binary.LittleEndian.PutUint16(buf[34:36], bitsPerSample)

	copy(buf[36:40], "data")
	binary.LittleEndian.PutUint32(buf[40:44], uint32(dataLen))

	for i, s := range samples {
		// 先钳到 [-1, 1]，再按 16bit 有符号量化（32767）。
		if s > 1 {
			s = 1
		} else if s < -1 {
			s = -1
		}
		v := int16(s * 32767)
		binary.LittleEndian.PutUint16(buf[44+i*2:], uint16(v))
	}
	return buf
}
