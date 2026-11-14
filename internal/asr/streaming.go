package asr

import (
	"context"
	"sync"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/utils/logx"
)

// 边录边转的**可选**流式能力（FR-03-10，默认关）。
//
// 为什么单独一个接口、而不是给 Engine 加两个方法：
//   - 默认路径（延后转写）只需要 `Finalize`，绝大多数构建与模型包都没有流式解码器；
//   - 把 `Accept` / `Reset` 塞进 Engine 会逼所有实现（在线端点、纯离线本地引擎）
//     写两个永远返回空的空方法，而「本构建没有流式能力」这种**正常状态**就没法用类型表达；
//   - 更重要的是防冒充：`Engine.Feed` 现在返回已关闭的空 channel（见 E8-01），
//     如果拿它当流式能力用，边录边转就会「显示成功但一句字都出不来」。
//     所以流式能力必须**显式**实现，缺能力就明确报不可用。

// StreamingEngine 是「流式增量解码」的可选能力。
//
// 实现方约定：
//   - `Accept` 只接受 16kHz 单声道 [-1,1]，返回**本次新增**的增量文本（可为空串）；
//   - `Accept` 允许在内部阻塞一小段时间（一帧解码量级），但调用方只会从**推理
//     goroutine** 调用，且输入队列有界，因此不会阻塞采集与落盘；
//   - `Reset` 清空解码状态（开始新会话前调用），幂等；
//   - 没有流式模型包时**不得**用离线引擎伪装：要让工厂报 `ERR_MODEL_NOT_FOUND`。
//
// 时间基：`Accept` 不接收时间戳，时间轴由调用方按**帧计数**换算（ARCH §4.3）。
type StreamingEngine interface {
	Engine
	// Accept 送入一段 16kHz 单声道样本，返回本次新增的增量文本（可能为空）。
	Accept(samples []float32) (partial string, err error)
	// Reset 开始新会话前清空解码状态。
	Reset() error
}

// StreamingFactory 构造流式引擎（每次调用新建实例）。
//
// 装配层注入：本机某构建含流式包（`zipformer-zh-streaming` 角色）时给真实实现；
// 否则**保持 nil** —— nil 会让 `Router.Streaming` 返回 `ERR_LOCAL_UNAVAILABLE`。
// 工厂自身在模型包缺失时应返回 `ERR_MODEL_NOT_FOUND`。
type StreamingFactory func() (StreamingEngine, error)

// errStreamingUnavailable 本构建（或本平台）不含流式引擎。
//
// 复用 `ERR_LOCAL_UNAVAILABLE` 而不是新造错误码：语义就是「本地这条能力没有」，
// 前端按既有错误码表提示「下载模型 / 换来源」即可（见 apierr 的错误码表）。
func errStreamingUnavailable(detail string) *apierr.Err {
	return apierr.New(apierr.CodeLocalUnavailable).
		WithAction("download-model").
		WithDetail("%s", detail)
}

// streamState 是流式引擎的惰性持有状态（与在线/本地降级矩阵**无关**）。
//
// 刻意不塞进 Router 的降级矩阵：流式引擎既不是「来源」，也不参与
// local ↔ online 的降级判定。它只是边录边转的专用旁路，失败就报不可用。
type streamState struct {
	mu  sync.Mutex
	eng StreamingEngine
}

// Streaming 返回流式引擎（首次调用时构造 + 预热，之后复用）。
//
// 可能错误：
//   - ERR_LOCAL_UNAVAILABLE：本构建不含流式能力（工厂为 nil）；
//   - ERR_MODEL_NOT_FOUND：流式模型包未安装（工厂返回）；
//   - ERR_ENGINE_LOAD：预热失败。
//
// 说明：预热在本方法内同步完成（本地流式模型加载数秒），因此**不要在 UI 主路径上
// 反复调用**；需要便宜的可用性判定请用 `StreamingAvailable`。
func (r *Router) Streaming() (StreamingEngine, error) {
	r.stream.mu.Lock()
	defer r.stream.mu.Unlock()
	if r.stream.eng != nil {
		return r.stream.eng, nil
	}
	if r.streamingFactory == nil {
		return nil, errStreamingUnavailable(
			"本构建不含流式引擎，边录边转不可用（需要流式模型包）")
	}
	eng, err := r.streamingFactory()
	if err != nil {
		e := apierr.From(err, apierr.CodeLocalUnavailable)
		// 只记错误码与包标识，不记任何音频内容（NFR-03-5）。
		logx.For("asr").Warn("流式引擎不可用", "code", e.Code)
		return nil, e
	}
	if eng == nil {
		return nil, errStreamingUnavailable("流式引擎工厂返回空实例")
	}
	if err := eng.Warm(context.Background()); err != nil {
		_ = eng.Close()
		e := apierr.From(err, apierr.CodeEngineLoad)
		logx.For("asr").Warn("流式引擎预热失败", "code", e.Code)
		return nil, e
	}
	r.stream.eng = eng
	logx.For("asr").Info("流式引擎已就绪", "engineId", eng.ID())
	return eng, nil
}

// StreamingAvailable 是**便宜**的可用性探测：不构造、不预热、不占 CPU。
//
// 判定顺序：
//  1. 工厂为 nil → 本构建不含流式能力（ERR_LOCAL_UNAVAILABLE）；
//  2. 已构造过 → 直接可用；
//  3. 有 `StreamingProbe` → 用它的结论（通常是「流式模型包是否已安装」）；
//  4. 以上都没有 → 视为可用（未知，真正加载时才知道）。
//
// 探测必须是纯查表式的（例如查模型目录），**不得**在这里加载模型：
// 录制页每次挂载都会问一次，加载几秒的模型不可接受。
func (r *Router) StreamingAvailable() error {
	r.stream.mu.Lock()
	eng := r.stream.eng
	r.stream.mu.Unlock()
	if eng != nil {
		return nil
	}
	if r.streamingFactory == nil {
		return errStreamingUnavailable("本构建不含流式引擎，边录边转不可用")
	}
	if r.streamingProbe == nil {
		return nil
	}
	if err := r.streamingProbe(); err != nil {
		return apierr.From(err, apierr.CodeModelNotFound)
	}
	return nil
}

// StreamingEngineID 返回已加载的流式引擎标识（未加载时为空）。
func (r *Router) StreamingEngineID() string {
	r.stream.mu.Lock()
	defer r.stream.mu.Unlock()
	if r.stream.eng == nil {
		return ""
	}
	return r.stream.eng.ID()
}

// LazyStreaming 把一个「构造即加载」的流式引擎包成惰性实例。
//
// 为什么需要它（而不是装配层直接调 `Router.Streaming`）：
//   - `Streaming` 会同步构造 + `Warm`（本地流式模型加载要数秒）；
//   - 装配层取实例的位置是 `recording.Start` 的同步路径，而那时
//     `OpenChannel` 已经把采集管道打开了 —— 在这里阻塞数秒，
//     会话开头的音频就会顶满管道缓冲被丢掉，等于**阻塞了采集**。
//
// 所以加载推迟到真正用它的地方：`LiveRunner` 的**推理 goroutine** 第一次
// `Accept` 时。那时队列满了只丢最旧样本，落盘与采集完全不受影响。
//
// 语义约定：
//   - `ID` 在未加载时返回空串（日志里显示「待加载」，不撒谎）；
//   - `Reset` 在未加载时**不触发**加载，只记下「加载后先清状态」；
//   - `Close` 在未加载时是空操作（不为了关闭而先把模型加载起来）；
//   - **加载**本身并发安全（只发生一次），但**引擎实例不是并发安全的**：
//     `Accept` 只允许单一消费者（流式解码器有内部状态，同 StreamingEngine 的约定）。
//     本应用同一时刻只有一个会话在录，因此实际只有一条 Accept 循环。
func LazyStreaming(resolve func() (StreamingEngine, error)) StreamingEngine {
	return &lazyStreaming{resolve: resolve}
}

// lazyStreaming 是 StreamingEngine 的惰性代理（并发安全：多个会话可能同时进来）。
type lazyStreaming struct {
	resolve func() (StreamingEngine, error)

	// callMu 把**对引擎的调用**串行化（Accept / Reset / Warm / Finalize）。
	//
	// 为什么需要：流式解码器有内部状态，不是并发安全的。而两个会话可能真的重叠 ——
	// 上一个会话 `Close` 是非阻塞的，它的推理 goroutine 可能还在收尾 flush（还在跑
	// Accept），此时用户立刻开始下一个会话，就会有两个 Accept 循环同时进同一个引擎，
	// 在 CGO 层是实打实的崩溃风险。串行化让后一个会话等前一个让出（每片几十毫秒），
	// 换来「任何时刻只有一个调用在引擎里」。
	callMu sync.Mutex

	mu      sync.Mutex
	eng     StreamingEngine
	loadErr error
	// needReset 表示「加载后第一次使用前要 Reset」（未加载时的 Reset 请求）。
	needReset bool
}

// ensure 返回已加载的引擎；失败结果会被记住（不反复重试加载）。
func (l *lazyStreaming) ensure() (StreamingEngine, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.eng != nil {
		return l.eng, nil
	}
	if l.loadErr != nil {
		return nil, l.loadErr
	}
	if l.resolve == nil {
		l.loadErr = errStreamingUnavailable("本构建不含流式引擎，边录边转不可用")
		return nil, l.loadErr
	}
	eng, err := l.resolve()
	if err != nil {
		l.loadErr = err
		return nil, err
	}
	if eng == nil {
		l.loadErr = errStreamingUnavailable("流式引擎工厂返回空实例")
		return nil, l.loadErr
	}
	l.eng = eng
	if l.needReset {
		if err := eng.Reset(); err != nil {
			l.needReset = false
			return nil, apierr.From(err, apierr.CodeEngineLoad)
		}
		l.needReset = false
	}
	return eng, nil
}

// ID 未加载时返回空串（调用方据此显示「待加载」，不做额外判定）。
func (l *lazyStreaming) ID() string {
	l.mu.Lock()
	eng := l.eng
	l.mu.Unlock()
	if eng == nil {
		return ""
	}
	return eng.ID()
}

// Warm 显式触发加载（预热按钮 / 诊断用）；不改变惰性语义。
func (l *lazyStreaming) Warm(ctx context.Context) error {
	l.callMu.Lock()
	defer l.callMu.Unlock()
	eng, err := l.ensure()
	if err != nil {
		return err
	}
	return eng.Warm(ctx)
}

// Feed 原样转发（边录边转走 Accept，不用 Feed；这里只为满足 Engine）。
func (l *lazyStreaming) Feed(frame []float32) (<-chan Partial, error) {
	l.callMu.Lock()
	defer l.callMu.Unlock()
	eng, err := l.ensure()
	if err != nil {
		return nil, err
	}
	return eng.Feed(frame)
}

// Finalize 转发离线定稿（同样串行化：它与 Accept 共用解码器状态）。
func (l *lazyStreaming) Finalize(seg AudioSegment) (FinalResult, error) {
	l.callMu.Lock()
	defer l.callMu.Unlock()
	eng, err := l.ensure()
	if err != nil {
		return FinalResult{}, err
	}
	return eng.Finalize(seg)
}

// Health 未加载时如实报「还没加载」，不为了查健康度把模型拉起来。
func (l *lazyStreaming) Health(ctx context.Context) HealthStatus {
	l.mu.Lock()
	eng := l.eng
	l.mu.Unlock()
	if eng == nil {
		return HealthStatus{State: "loading"}
	}
	// 不取 callMu：查状态不该排在一条长解码后面干等（Health 只读，不进解码器）。
	return eng.Health(ctx)
}

// Accept 是真正触发加载的入口：只在推理 goroutine 里被调用。
//
// 串行化保证「同一时刻只有一个会话在喂同一个解码器」：上一个会话收尾 flush 与
// 下一个会话开头重叠时，后来者会等前者让出，而不是并发进 CGO。
func (l *lazyStreaming) Accept(samples []float32) (string, error) {
	l.callMu.Lock()
	defer l.callMu.Unlock()
	eng, err := l.ensure()
	if err != nil {
		return "", err
	}
	return eng.Accept(samples)
}

// Reset 未加载时只打标记（**不**为了清状态先把模型加载起来）。
func (l *lazyStreaming) Reset() error {
	l.mu.Lock()
	if l.eng == nil {
		l.needReset = true
		l.mu.Unlock()
		return nil
	}
	l.mu.Unlock()

	// 已加载：与 Accept 串行，避免在别人解码途中清状态。
	l.callMu.Lock()
	defer l.callMu.Unlock()
	l.mu.Lock()
	eng := l.eng
	l.mu.Unlock()
	if eng == nil {
		return nil
	}
	return eng.Reset()
}

// Close 未加载时为空操作（幂等）。
func (l *lazyStreaming) Close() error {
	// 与 Accept 串行：释放资源时不能还有人正在解码。
	l.callMu.Lock()
	defer l.callMu.Unlock()
	l.mu.Lock()
	eng := l.eng
	l.eng = nil
	l.mu.Unlock()
	if eng == nil {
		return nil
	}
	return eng.Close()
}

// CloseStreaming 释放流式引擎（应用退出时调用；幂等）。
//
// 与 `Close` 分开：流式引擎只在边录边转时存在，随时可能为空，
// 让 `Close` 一并处理会把「关闭整个引擎域」和「关闭一条旁路」耦合起来。
func (r *Router) CloseStreaming() error {
	r.stream.mu.Lock()
	eng := r.stream.eng
	r.stream.eng = nil
	r.stream.mu.Unlock()
	if eng == nil {
		return nil
	}
	return eng.Close()
}
