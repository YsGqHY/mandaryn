package asr

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/utils/logx"
)

// FailureKind 是**在线**失败的分类，直接对应 ARCH §4.2.4 处置矩阵的行。
type FailureKind string

const (
	// FailNetwork 网络不可达（DNS / TCP / TLS 前的连接层错误）。
	FailNetwork FailureKind = "network"
	// FailTimeout 超时（请求超过首字节超时仍无响应）。
	FailTimeout FailureKind = "timeout"
	// FailAuth 鉴权失败（401/403）。
	FailAuth FailureKind = "auth"
	// FailRateLimit 限流（429）。
	FailRateLimit FailureKind = "rate-limit"
	// FailServer 端点 5xx。
	FailServer FailureKind = "server"
	// FailRejected 内容被拒（4xx 非 401/403/429）。
	FailRejected FailureKind = "rejected"
	// FailLocal 本地引擎失败（模型缺失 / 加载失败 / 推理失败）。
	FailLocal FailureKind = "local"
	// FailBlocked 被 MANDARYN_OFFLINE 拦截，请求根本没发出去。
	FailBlocked FailureKind = "blocked"
)

// retryable 报告该失败类别是否重试（ARCH §4.2.4「重试」列）。
func (k FailureKind) retryable() bool {
	switch k {
	case FailNetwork, FailTimeout, FailRateLimit, FailServer:
		return true
	default:
		// 鉴权失败重试无意义；内容被拒换格式/语种才有意义；
		// 本地失败与离线拦截都不是「重发一次就好」的问题。
		return false
	}
}

// fallbackable 报告该失败类别是否允许降级到本地（ARCH §4.2.4「降级到本地」列）。
//
// FailLocal / FailBlocked 一律 false：本地失败**绝不**降级在线（隐私默认），
// 离线拦截是配置状态而不是故障，必须由用户显式切来源。
func (k FailureKind) fallbackable() bool {
	switch k {
	case FailNetwork, FailTimeout, FailAuth, FailRateLimit, FailServer, FailRejected:
		return true
	default:
		return false
	}
}

// EngineError 是带分类的引擎错误。
//
// Router 靠 Kind 决定「重不重试、降不降级」；Err 里带 ERR_* 码，
// 直接透传给前端做 i18n 与「下一步动作」按钮。
type EngineError struct {
	// Kind 失败类别。
	Kind FailureKind
	// Err 结构化错误（含错误码）。
	Err *apierr.Err
	// RetryAfter 端点给的 Retry-After（仅 429 有，已解析并钳到上限）。
	RetryAfter time.Duration
}

func (e *EngineError) Error() string {
	if e == nil || e.Err == nil {
		return "asr: engine error"
	}
	return e.Err.Error()
}

// Unwrap 让 errors.Is / apierr.HasCode 能穿透。
func (e *EngineError) Unwrap() error {
	if e == nil || e.Err == nil {
		return nil
	}
	return e.Err
}

// AsEngineError 提取分类错误；不是 EngineError 时返回 (nil, false)。
func AsEngineError(err error) (*EngineError, bool) {
	var ee *EngineError
	if errors.As(err, &ee) {
		return ee, true
	}
	return nil, false
}

// 降级与重试的默认参数（对齐 ARCH §4.2.4 与 NFR-01）。
const (
	// DefaultUnloadDelay 热切换成功后旧引擎延迟卸载时长（容错在途请求）。
	DefaultUnloadDelay = 2 * time.Second
	// DefaultMaxRetries 矩阵里「2 次」的默认值。
	DefaultMaxRetries = 2
	// MaxRetriesCap 用户可配的上限，防止把 429 放大成雪崩。
	MaxRetriesCap = 10
	// DefaultRetryAfterCap 429 的 Retry-After 上限（ARCH §4.2.4：上限 30s）。
	DefaultRetryAfterCap = 30 * time.Second
	// DefaultSwitchBackWindow AutoSwitchBack=true 时的回升观察窗口。
	DefaultSwitchBackWindow = 5 * time.Minute
	// warmTimeout 单次预热的最长时间上限（本地加载 + 在线握手）。
	warmTimeout = 60 * time.Second
)

// backoffFor 返回第 attempt 次重试前的等待时长（1s / 3s / 之后 3s）。
func backoffFor(attempt int) time.Duration {
	if attempt <= 0 {
		return time.Second
	}
	return 3 * time.Second
}

// RouterOptions 是 Router 的装配参数（由 internal/app 注入）。
type RouterOptions struct {
	// Source 初始来源（local | online），空视为 local。
	Source string
	// Local 本地引擎工厂；nil 表示「本构建不含本地引擎」。
	Local LocalFactory
	// Online 在线引擎工厂；nil 表示「未配置在线端点」。
	Online OnlineFactory
	// Streaming 流式引擎工厂（**可选能力**，边录边转用，FR-03-10）。
	//
	// nil = 本构建不含流式能力 → `Router.Streaming` 返回 ERR_LOCAL_UNAVAILABLE，
	// 边录边转在 UI 上显示「不可用」而**不会**退回离线引擎伪装流式。
	// 它不参与 local ↔ online 的降级矩阵（那条矩阵只管 Finalize 这条路）。
	Streaming StreamingFactory
	// StreamingProbe 便宜的流式能力探测（不加载模型），装配层传「流式模型包是否已安装」。
	//
	// nil 时 `StreamingAvailable` 只能说「本构建没禁用」，真正可用性要到加载时才知道。
	StreamingProbe func() error
	// Policy 降级策略（来自 app_settings，默认 fallbackToLocal=true / maxRetries=2）。
	Policy contract.FallbackPolicy
	// ActiveSession 报告是否有活跃录制会话；SetSource 据此拒绝切换。
	ActiveSession func() bool
	// LoadThreads 返回**当前档位**对应的 onnx 推理线程数（装配层传 `perf.Service.Threads`）。
	//
	// 语义边界（重要）：线程数写进 sherpa 的 `OfflineRecognizerConfig` 后不可改，
	// 因此它只能在**构造引擎时**生效 —— Router 每次加载引擎前读一次本函数，
	// 结果是「档位变化最早在**下一次加载**（热切换 / Reload / 冷启动）带上」，
	// **不是热生效**。详见 `asr.LoadThreads` 与 `perf.ThreadsFor`。
	//
	// 真正的参数注入点是工厂（`local.NewFactoryWithThreads`）；装配层若把同一个
	// 函数也给了工厂（推荐），则 Router 这里的读取只用于日志与诊断——保证
	// 「引擎以几个线程加载」在日志里可查，避免出现「档位改了，但没人知道引擎用了几个线程」。
	//
	// nil → 不注入（引擎用默认线程数），既有行为完全不变。
	LoadThreads func() int
	// OnStatus 状态变化回调（L3 负责转成 EV: engine:status）。
	OnStatus func(contract.EngineStatus)
	// OnDegrade 降级打点回调（S4 录制侧用它往 session_events 写 degrade_marker）。
	OnDegrade func(note string)
	// Sleep 可注入的等待函数（测试用；nil 用真实 time.Sleep）。
	Sleep func(ctx context.Context, d time.Duration) error
	// Now 可注入的时钟（测试用；nil 用 time.Now）。
	Now func() time.Time
	// UnloadDelay 旧引擎延迟卸载时长；0 用 DefaultUnloadDelay。
	UnloadDelay time.Duration
	// SwitchBackWindow 自动回升观察窗口；0 用 DefaultSwitchBackWindow。
	SwitchBackWindow time.Duration
	// RetryAfterCap 429 Retry-After 上限；0 用 DefaultRetryAfterCap。
	RetryAfterCap time.Duration
}

// Router 对上层暴露与 Engine 相同的接口，内部持有 local / online 两个实现，
// 负责来源选择、热切换与降级（ARCH §4.2.1 / §4.2.2 / §4.2.4）。
//
// 并发约定：
//   - opMu 串行化「重操作」（Warm / SetSource / Reload），避免两次热切换交叉；
//   - mu 只保护状态字段，绝不跨越耗时调用持有。
type Router struct {
	opMu sync.Mutex
	mu   sync.RWMutex

	source         string
	local          Engine
	online         *onlineSlot
	onlineRevision uint64 // increments whenever the online factory/config changes
	onlineChanged  bool   // a replacement may recover from a failed initial Warm

	// 状态快照（Source/State/EngineID/LoadMs/Degraded/DegradeNote/LastError）。
	status     contract.EngineStatus
	degraded   bool
	degradedAt time.Time

	localFactory  LocalFactory
	onlineFactory OnlineFactory

	// streamingFactory / streamingProbe 见 RouterOptions（边录边转的可选旁路）。
	streamingFactory StreamingFactory
	streamingProbe   func() error
	// stream 是流式引擎的惰性持有状态（与降级矩阵无关）。
	stream streamState

	policy contract.FallbackPolicy

	activeSession func() bool
	onStatus      func(contract.EngineStatus)
	onDegrade     func(note string)

	// loadThreads 见 RouterOptions.LoadThreads；loadThreadsSeen 是最近一次加载读到的值。
	loadThreads     func() int
	loadThreadsSeen int

	sleep func(ctx context.Context, d time.Duration) error
	now   func() time.Time

	unloadDelay     time.Duration
	switchBackAfter time.Duration
	retryAfterCap   time.Duration
}

// onlineSlot keeps a superseded engine alive until its in-flight work (including retries) ends.
// Its lifecycle fields are protected by Router.mu.
type onlineSlot struct {
	engine   Engine
	revision uint64
	users    int
	retired  bool
}

// NewRouter 构造路由。Options 里的零值都有合理默认，装配方只需给工厂与回调。
func NewRouter(opt RouterOptions) *Router {
	src := strings.TrimSpace(opt.Source)
	if src != contract.ModelSourceOnline {
		src = contract.ModelSourceLocal
	}
	r := &Router{
		source:           src,
		localFactory:     opt.Local,
		onlineFactory:    opt.Online,
		streamingFactory: opt.Streaming,
		streamingProbe:   opt.StreamingProbe,
		policy:           normalizePolicy(opt.Policy),
		activeSession:    opt.ActiveSession,
		onStatus:         opt.OnStatus,
		onDegrade:        opt.OnDegrade,
		loadThreads:      opt.LoadThreads,
		sleep:            opt.Sleep,
		now:              opt.Now,
		unloadDelay:      opt.UnloadDelay,
		switchBackAfter:  opt.SwitchBackWindow,
		retryAfterCap:    opt.RetryAfterCap,
	}
	if r.sleep == nil {
		r.sleep = sleepCtx
	}
	if r.now == nil {
		r.now = time.Now
	}
	if r.unloadDelay <= 0 {
		r.unloadDelay = DefaultUnloadDelay
	}
	if r.switchBackAfter <= 0 {
		r.switchBackAfter = DefaultSwitchBackWindow
	}
	if r.retryAfterCap <= 0 {
		r.retryAfterCap = DefaultRetryAfterCap
	}
	r.status = contract.EngineStatus{
		Source: src,
		State:  contract.EngineStateLoading,
	}
	return r
}

// normalizePolicy 补齐零值：MaxRetries<=0 用矩阵默认 2（无法用 0 表达「不重试」，
// 因为 contract.FallbackPolicy 的零值必须落到可用默认值；「不重试」用
// FallbackToLocal=false + MaxRetries=1 表达最小重试语义）。
func normalizePolicy(p contract.FallbackPolicy) contract.FallbackPolicy {
	if p.MaxRetries <= 0 {
		p.MaxRetries = DefaultMaxRetries
	}
	if p.MaxRetries > MaxRetriesCap {
		p.MaxRetries = MaxRetriesCap
	}
	return p
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// —— Engine 接口实现 ——

// ID 返回当前生效引擎的标识；未预热时返回空串。
func (r *Router) ID() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.status.EngineID
}

// Warm 预热当前来源的引擎（启动时后台调用，不阻塞 UI）。
func (r *Router) Warm(ctx context.Context) error {
	r.opMu.Lock()
	defer r.opMu.Unlock()

	r.mu.RLock()
	src := r.source
	has := r.engineLocked(src) != nil
	st := r.status.State
	r.mu.RUnlock()
	if has && (st == contract.EngineStateReady || st == contract.EngineStateDegraded) {
		// 已预热：幂等直接返回（避免重复加载模型）。
		return nil
	}
	return r.switchTo(ctx, src, true)
}

// Feed 把帧交给当前生效引擎。
//
// 延后转写模式（默认）下两个实现都返回已关闭的空 channel，见各自的 Feed 注释。
func (r *Router) Feed(frame []float32) (<-chan Partial, error) {
	eng, slot, err := r.route()
	if err != nil {
		return nil, err
	}
	if slot != nil {
		defer r.releaseOnline(slot)
	}
	return eng.Feed(frame)
}

// Finalize 对一整句做离线校正，并实现 ARCH §4.2.4 的重试与降级矩阵。
func (r *Router) Finalize(seg AudioSegment) (FinalResult, error) {
	if err := r.maybeSwitchBack(); err != nil {
		// 回升失败不影响本次转写，只留痕（保持 degraded）。
		logx.For("asr").Warn("自动回升在线失败，继续保持降级", "err", err)
	}
	eng, slot, err := r.route()
	if err != nil {
		return FinalResult{}, err
	}
	if slot == nil {
		// 本地路径：失败只报错，**绝不**降级在线（隐私默认）。
		return r.finalizeLocal(eng, seg)
	}
	defer r.releaseOnline(slot)
	return r.finalizeOnline(slot, seg)
}

// Health 返回当前健康详情。
func (r *Router) Health(ctx context.Context) HealthStatus {
	r.mu.RLock()
	st := r.status
	r.mu.RUnlock()
	return HealthStatus{
		State:    st.State,
		EngineID: st.EngineID,
		LoadMs:   st.LoadMs,
		Note:     st.DegradeNote,
		Err:      fromErrorDTO(st.LastError),
	}
}

// Close 关闭两个引擎（应用退出时调用）。
func (r *Router) Close() error {
	r.opMu.Lock()
	defer r.opMu.Unlock()
	r.mu.Lock()
	local, online := r.local, r.online
	r.local, r.online = nil, nil
	r.status.State = contract.EngineStatePaused
	st := r.status
	r.mu.Unlock()
	r.notify(st)

	var firstErr error
	if local != nil {
		if err := local.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if online != nil {
		if err := r.retireOnline(online); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// —— 来源切换（ARCH §4.2.2）——

// SetSource 热切换模型来源。
//
// 严格按 ARCH §4.2.2 的五步执行：
//  1. 校验：有活跃录制会话 → ERR_STATE_FORBIDDEN（录制中不可切换）；
//  2. engine:status=loading；
//  3. 目标引擎 Warm；
//  4. 成功 → 延迟 2s 卸旧引擎 → ready；
//  5. 失败 → 保留旧引擎不动（等价回滚）→ failed + 原因。
func (r *Router) SetSource(next string) error {
	next = strings.TrimSpace(next)
	if next != contract.ModelSourceLocal && next != contract.ModelSourceOnline {
		return apierr.Newf(apierr.CodeInvalidArgument, "model source must be local or online")
	}
	if r.activeSessionFn() != nil && r.activeSessionFn()() {
		return apierr.New(apierr.CodeStateForbidden).
			WithAction("stop-recording").
			WithDetail("录制中不可切换模型来源")
	}
	r.opMu.Lock()
	defer r.opMu.Unlock()
	return r.switchTo(context.Background(), next, false)
}

// Reload 强制重载当前来源的引擎（换模型包 / 改在线端点后调用）。
func (r *Router) Reload() error {
	if r.activeSessionFn() != nil && r.activeSessionFn()() {
		return apierr.New(apierr.CodeStateForbidden).
			WithAction("stop-recording").
			WithDetail("录制中不可重载模型")
	}
	r.opMu.Lock()
	defer r.opMu.Unlock()
	r.mu.RLock()
	src := r.source
	r.mu.RUnlock()
	return r.switchTo(context.Background(), src, true)
}

// SetOnStatus 注入/替换状态回调（L3 用它把状态转成 EV: engine:status）。
//
// 用 setter 而不是导出字段：enginesvc 在装配后要保存回调捕获的 service 指针，
// 构造顺序上做不到先有回调再有 Router。
func (r *Router) SetOnStatus(fn func(contract.EngineStatus)) {
	r.mu.Lock()
	r.onStatus = fn
	r.mu.Unlock()
}

// SetOnDegrade 注入/替换降级打点回调（S4 录制侧用它写 degrade_marker）。
func (r *Router) SetOnDegrade(fn func(note string)) {
	r.mu.Lock()
	r.onDegrade = fn
	r.mu.Unlock()
}

// SetPolicy 更新降级策略（API-5.7 落库后同步到这里）。
func (r *Router) SetPolicy(p contract.FallbackPolicy) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.policy = normalizePolicy(p)
}

// Policy 返回当前生效的降级策略。
func (r *Router) Policy() contract.FallbackPolicy {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.policy
}

// Source 返回用户选择的来源（降级不会改它）。
func (r *Router) Source() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.source
}

// Status 返回当前状态快照。
func (r *Router) Status() contract.EngineStatus {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.status
}

// SetOnlineFactory invalidates the online configuration without changing the source or connecting.
// The next online operation reconstructs the active engine; existing operations keep their lease.
func (r *Router) SetOnlineFactory(f OnlineFactory) {
	r.mu.Lock()
	r.onlineChanged = r.onlineChanged || r.onlineFactory != nil
	r.onlineFactory = f
	r.onlineRevision++
	var old *onlineSlot
	if r.source != contract.ModelSourceOnline {
		old, r.online = r.online, nil
	}
	r.mu.Unlock()
	_ = r.retireOnline(old)
}

func (r *Router) retireOnline(slot *onlineSlot) error {
	if slot == nil {
		return nil
	}
	r.mu.Lock()
	done := !slot.retired && slot.users == 0
	slot.retired = true
	r.mu.Unlock()
	if done {
		return slot.engine.Close()
	}
	return nil
}

func (r *Router) releaseOnline(slot *onlineSlot) {
	r.mu.Lock()
	slot.users--
	done := slot.retired && slot.users == 0
	r.mu.Unlock()
	if done {
		_ = slot.engine.Close()
	}
}

// switchTo 是 SetSource / Reload / Warm 的共同实现（调用方必须持有 opMu）。
//
// force=false 时，若目标来源与当前一致且已 ready，直接返回（幂等）。
func (r *Router) switchTo(ctx context.Context, next string, force bool) error {
	r.mu.RLock()
	curSrc, curState, curEngineID := r.source, r.status.State, r.status.EngineID
	revision := r.onlineRevision
	hasTarget := r.engineLocked(next) != nil
	if next == contract.ModelSourceOnline && r.online != nil && r.online.revision != revision {
		hasTarget = false
	}
	r.mu.RUnlock()

	if !force && next == curSrc && hasTarget && curState == contract.EngineStateReady {
		return nil
	}

	// 步骤 2：先播报 loading，让 UI 立刻能显示「正在加载模型」。
	r.markLoading(next)

	// 步骤 3：构造并预热目标引擎。
	warmCtx, cancel := context.WithTimeout(ctx, warmTimeout)
	defer cancel()
	started := r.now()
	eng, err := r.buildAndWarm(warmCtx, next)
	loadMs := r.now().Sub(started).Milliseconds()
	if err != nil {
		// 步骤 5：失败 → 回滚（旧引擎原地不动）→ failed + 原因。
		apiErr := apierr.From(err, apierr.CodeEngineLoad)
		r.mu.Lock()
		r.source = curSrc
		r.status = contract.EngineStatus{
			Source:    curSrc,
			State:     contract.EngineStateFailed,
			EngineID:  curEngineID,
			LoadMs:    loadMs,
			LastError: toErrorDTO(apiErr),
		}
		st := r.status
		r.mu.Unlock()
		r.notify(st)
		logx.For("asr").Error("引擎预热失败，已回滚",
			"target", next, "code", apiErr.Code, "loadMs", loadMs)
		return apiErr
	}

	// 步骤 4：成功 → 换指针、延迟卸载旧引擎、状态 ready。
	r.mu.Lock()
	var oldLocal Engine
	var oldOnline *onlineSlot
	switch next {
	case contract.ModelSourceLocal:
		oldLocal = r.local
		r.local = eng
	default:
		oldOnline = r.online
		r.online = &onlineSlot{engine: eng, revision: revision}
	}
	r.source = next
	r.degraded = false
	r.degradedAt = time.Time{}
	r.status = contract.EngineStatus{
		Source:   next,
		State:    contract.EngineStateReady,
		EngineID: eng.ID(),
		LoadMs:   loadMs,
	}
	st := r.status
	r.mu.Unlock()
	r.notify(st)

	// 旧引擎延迟卸载，容错在途请求（ARCH §4.2.2 步骤 4）。
	if oldLocal != nil && oldLocal != eng {
		r.scheduleClose(oldLocal)
	}
	if oldOnline != nil && oldOnline.engine != eng {
		time.AfterFunc(r.unloadDelay, func() { _ = r.retireOnline(oldOnline) })
	}
	logx.For("asr").Info("引擎热切换完成", "source", next, "engineId", eng.ID(), "loadMs", loadMs)
	return nil
}

// LoadThreads 返回最近一次加载本地引擎时读到的推理线程数（0 = 交给引擎默认）。
//
// 用途是诊断：档位是慢变量、线程数只在加载时生效，出问题时最先要回答的问题就是
// 「这台机器上引擎到底以几个线程加载的」。没有注入 `RouterOptions.LoadThreads` 时恒 0。
func (r *Router) LoadThreads() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.loadThreadsSeen
}

// recordLoadThreads 记录本次加载读到的线程数（见 buildAndWarm）。
func (r *Router) recordLoadThreads(n int) {
	r.mu.Lock()
	r.loadThreadsSeen = n
	r.mu.Unlock()
}

// SetLoadThreads 替换档位读取函数（装配层迟到接线时用，等价于 RouterOptions.LoadThreads）。
func (r *Router) SetLoadThreads(fn func() int) {
	r.mu.Lock()
	r.loadThreads = fn
	r.mu.Unlock()
}

// buildAndWarm 用工厂新建引擎并预热；失败时立刻关掉半成品，避免泄漏。
func (r *Router) buildAndWarm(ctx context.Context, src string) (Engine, error) {
	r.mu.RLock()
	lf, of := r.localFactory, r.onlineFactory
	threadsGen := r.loadThreads
	r.mu.RUnlock()

	var (
		eng Engine
		err error
	)
	switch src {
	case contract.ModelSourceLocal:
		if lf == nil {
			return nil, errLocalUnavailable("本构建未装配本地引擎")
		}
		// 每次加载读一次档位参数（**只对本地源有意义**：在线端点没有线程数概念）。
		// 读到的值交给工厂去用；Router 自己保留一份用于日志与诊断，见 LoadThreads()。
		threads := LoadThreads(threadsGen)
		r.recordLoadThreads(threads)
		logx.For("asr").Info("按当前档位加载本地引擎", "threads", threads)
		eng, err = lf()
	default:
		if of == nil {
			return nil, apierr.New(apierr.CodeEngineLoad).
				WithDetail("在线引擎未装配（请先配置端点）")
		}
		eng, err = of()
	}
	if err != nil {
		return nil, apierr.From(err, apierr.CodeEngineLoad)
	}
	if eng == nil {
		return nil, apierr.New(apierr.CodeEngineLoad).WithDetail("引擎工厂返回空实例")
	}
	if err := eng.Warm(ctx); err != nil {
		_ = eng.Close()
		// Warm 失败分类由引擎自己给（ERR_MODEL_NOT_FOUND / ERR_MODEL_CORRUPT /
		// ERR_ENGINE_LOAD）；非结构化错误统一归为 ERR_ENGINE_LOAD。
		return nil, apierr.From(err, apierr.CodeEngineLoad)
	}
	return eng, nil
}

// markLoading 播报 loading（不触碰引擎指针）。
func (r *Router) markLoading(next string) {
	r.mu.Lock()
	prevID := ""
	if e := r.engineLocked(next); e != nil {
		prevID = e.ID()
	}
	r.status = contract.EngineStatus{
		Source:   next,
		State:    contract.EngineStateLoading,
		EngineID: prevID,
	}
	st := r.status
	r.mu.Unlock()
	r.notify(st)
}

// scheduleClose 延迟关闭旧引擎。
func (r *Router) scheduleClose(old Engine) {
	delay := r.unloadDelay
	time.AfterFunc(delay, func() {
		if err := old.Close(); err != nil {
			logx.For("asr").Warn("旧引擎卸载失败", "err", err)
		}
	})
}

// engineLocked 返回某来源的引擎实例（调用方须持有 r.mu）。
func (r *Router) engineLocked(src string) Engine {
	if src == contract.ModelSourceOnline {
		if r.online == nil {
			return nil
		}
		return r.online.engine
	}
	return r.local
}

// route 决定本次 Finalize / Feed 使用的引擎。调用方必须释放非空的在线租约。
// 已降级仍走本地，保存在线配置不会绕过这个隐私边界。
func (r *Router) route() (Engine, *onlineSlot, error) {
	for {
		r.mu.Lock()
		if r.source != contract.ModelSourceOnline || r.degraded {
			local := r.local
			r.mu.Unlock()
			if local == nil {
				return nil, nil, errNotReady("本地引擎尚未预热")
			}
			return local, nil, nil
		}
		slot := r.online
		if slot == nil && !(r.onlineChanged && r.status.State != contract.EngineStatePaused) {
			r.mu.Unlock()
			return nil, nil, errNotReady("在线引擎尚未预热")
		}
		if slot != nil && slot.revision == r.onlineRevision {
			slot.users++
			r.mu.Unlock()
			return slot.engine, slot, nil
		}
		r.mu.Unlock()
		if err := r.refreshOnline(); err != nil {
			return nil, nil, err
		}
	}
}

// refreshOnline reconstructs an active online engine with the latest configuration. It never calls
// Warm: the next transcription is the first request using the new endpoint, not a separate probe.
func (r *Router) refreshOnline() error {
	r.opMu.Lock()
	defer r.opMu.Unlock()

	r.mu.RLock()
	old, revision, factory := r.online, r.onlineRevision, r.onlineFactory
	stale := old != nil && old.revision != revision
	missing := old == nil && r.onlineChanged && r.status.State != contract.EngineStatePaused
	needed := r.source == contract.ModelSourceOnline && !r.degraded && (stale || missing)
	r.mu.RUnlock()
	if !needed {
		return nil
	}
	if factory == nil {
		return apierr.New(apierr.CodeEngineLoad).WithDetail("在线引擎未装配（请先配置端点）")
	}
	eng, err := factory()
	if err != nil {
		return apierr.From(err, apierr.CodeEngineLoad)
	}
	if eng == nil {
		return apierr.New(apierr.CodeEngineLoad).WithDetail("在线引擎工厂返回空实例")
	}

	same := old != nil && eng == old.engine
	r.mu.Lock()
	if r.onlineRevision != revision || r.degraded {
		r.mu.Unlock()
		if !same {
			_ = eng.Close()
		}
		return nil
	}
	if same {
		old.revision = revision
	} else {
		r.online = &onlineSlot{engine: eng, revision: revision}
	}
	r.status.EngineID = eng.ID()
	r.status.LoadMs = 0
	r.status.LastError = nil
	st := r.status
	r.mu.Unlock()
	if !same {
		_ = r.retireOnline(old)
	}
	r.notify(st)
	return nil
}

// —— 本地路径 ——

// finalizeLocal 直接走本地引擎；失败只报错，不降级在线（隐私默认）。
func (r *Router) finalizeLocal(eng Engine, seg AudioSegment) (FinalResult, error) {
	res, err := eng.Finalize(seg)
	if err != nil {
		apiErr := apierr.From(err, apierr.CodeEngineLoad)
		r.setLastError(apiErr)
		logx.For("asr").Warn("本地转写失败", "engineId", eng.ID(), "code", apiErr.Code)
		return FinalResult{}, apiErr
	}
	return res, nil
}

// —— 在线路径（重试 + 降级矩阵）——

func (r *Router) finalizeOnline(slot *onlineSlot, seg AudioSegment) (FinalResult, error) {
	policy := r.Policy()
	attempts := policy.MaxRetries + 1 // 首次 + 重试次数
	var lastErr *EngineError

	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			wait := backoffFor(attempt - 1)
			if lastErr != nil && lastErr.Kind == FailRateLimit && lastErr.RetryAfter > 0 {
				wait = lastErr.RetryAfter
				if limit := r.retryAfterCap; wait > limit {
					wait = limit
				}
			}
			if err := r.sleep(context.Background(), wait); err != nil {
				return FinalResult{}, apierr.From(err, apierr.CodeEngineOnlineTimeout)
			}
			logx.For("asr").Warn("在线转写重试",
				"attempt", attempt, "kind", string(lastErr.Kind), "waitMs", wait.Milliseconds())
		}

		res, err := slot.engine.Finalize(seg)
		if err == nil {
			r.clearDegraded(slot)
			return res, nil
		}
		lastErr = classifyEngineError(err)
		if lastErr == nil {
			// 不可分类的错误：按端点 5xx 处理（保守：允许重试 + 降级）。
			lastErr = &EngineError{Kind: FailServer, Err: apierr.From(err, apierr.CodeEngineLoad)}
		}
		if !lastErr.Kind.retryable() {
			break
		}
	}

	// 重试耗尽（或不可重试）：按矩阵决定降级。
	return r.handleOnlineFailure(slot, seg, lastErr)
}

// handleOnlineFailure 实现在线失败后的降级（ARCH §4.2.4）。
func (r *Router) handleOnlineFailure(slot *onlineSlot, seg AudioSegment, ee *EngineError) (FinalResult, error) {
	if ee == nil {
		ee = &EngineError{Kind: FailServer, Err: apierr.New(apierr.CodeEngineLoad)}
	}
	apiErr := ee.Err
	if apiErr == nil {
		apiErr = apierr.New(apierr.CodeEngineLoad)
	}

	policy := r.Policy()
	local, localErr := r.ensureLocal()

	canFallback := policy.FallbackToLocal && ee.Kind.fallbackable() && local != nil
	if !canFallback {
		r.mu.Lock()
		if r.currentOnlineLocked(slot) {
			r.status.LastError = toErrorDTO(apiErr)
		}
		r.mu.Unlock()
		logx.For("asr").Error("在线转写失败且不降级",
			"kind", string(ee.Kind), "code", apiErr.Code,
			"fallbackToLocal", policy.FallbackToLocal, "localReady", local != nil)
		if localErr != nil {
			// 本地也不可用：把「为什么没降级」告诉用户（下载模型 / 换小档）。
			return FinalResult{}, apiErr.WithDetail("%s；本地模型也不可用: %s",
				apiErr.Message, localErr.Message)
		}
		return FinalResult{}, apiErr
	}

	// 置降级标记：只打一次点，之后持续用本地直到用户显式切回 / 窗口回升成功。
	note := degradeNote(ee)
	r.markDegraded(slot, note, apiErr)
	logx.For("asr").Warn("在线引擎失败，已降级本地",
		"kind", string(ee.Kind), "code", apiErr.Code)

	res, err := local.Finalize(seg)
	if err != nil {
		localErr := apierr.From(err, apierr.CodeEngineLoad)
		r.mu.Lock()
		if r.currentOnlineLocked(slot) {
			r.status.LastError = toErrorDTO(localErr)
		}
		r.mu.Unlock()
		logx.For("asr").Error("降级后本地转写也失败", "code", localErr.Code)
		return FinalResult{}, localErr
	}
	return res, nil
}

// ensureLocal 拿到一个**可用**的本地引擎，必要时现场懒加载一次。
//
// 为什么要懒加载：用户选 online 时本地引擎可能从未预热过（ARCH §4.2.1 说 local 懒加载），
// 但降级必须有本地可用 —— 因此在第一次需要降级时才加载，代价是这一句转写多等几秒。
//
// 返回 (nil, err) 表示「本地也用不了」（模型缺失 / 加载失败），调用方据此只报在线错误。
func (r *Router) ensureLocal() (Engine, *apierr.Err) {
	r.mu.RLock()
	local := r.local
	r.mu.RUnlock()
	if local != nil {
		return local, nil
	}

	r.opMu.Lock()
	defer r.opMu.Unlock()

	// 双检：拿锁期间可能已被别的路径加载。
	r.mu.RLock()
	local = r.local
	r.mu.RUnlock()
	if local != nil {
		return local, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), warmTimeout)
	defer cancel()
	eng, err := r.buildAndWarm(ctx, contract.ModelSourceLocal)
	if err != nil {
		return nil, apierr.From(err, apierr.CodeEngineLoad)
	}
	r.mu.Lock()
	r.local = eng
	r.mu.Unlock()
	logx.For("asr").Info("降级前懒加载本地引擎完成", "engineId", eng.ID())
	return eng, nil
}

// classifyEngineError 把任意错误归一成带分类的 EngineError。
func classifyEngineError(err error) *EngineError {
	if err == nil {
		return nil
	}
	if ee, ok := AsEngineError(err); ok {
		return ee
	}
	// 引擎实现没分类时，按错误码反推（保持矩阵可用）。
	var apiErr *apierr.Err
	if errors.As(err, &apiErr) {
		switch apiErr.Code {
		case apierr.CodeEngineOnlineNetwork:
			return &EngineError{Kind: FailNetwork, Err: apiErr}
		case apierr.CodeEngineOnlineTimeout:
			return &EngineError{Kind: FailTimeout, Err: apiErr}
		case apierr.CodeEngineOnlineAuth:
			return &EngineError{Kind: FailAuth, Err: apiErr}
		case apierr.CodeEngineOnlineRateLimit:
			return &EngineError{Kind: FailRateLimit, Err: apiErr}
		case apierr.CodeEngineOnlineRejected:
			return &EngineError{Kind: FailRejected, Err: apiErr}
		case apierr.CodeLocalUnavailable, apierr.CodeModelNotFound,
			apierr.CodeModelCorrupt, apierr.CodeEngineLoad:
			return &EngineError{Kind: FailLocal, Err: apiErr}
		case apierr.CodeOfflineEnforced:
			return &EngineError{Kind: FailBlocked, Err: apiErr}
		}
		return &EngineError{Kind: FailServer, Err: apiErr}
	}
	return nil
}

// degradeNote 生成面向用户的中文降级原因（不含音频 / 正文 / 凭据）。
func degradeNote(ee *EngineError) string {
	switch ee.Kind {
	case FailNetwork:
		return "在线端点不可达，已改用本地模型"
	case FailTimeout:
		return "在线端点超时，已改用本地模型"
	case FailAuth:
		return "在线端点鉴权失败（请检查密钥），已改用本地模型"
	case FailRateLimit:
		return "在线端点限流，已改用本地模型"
	case FailServer:
		return "在线端点服务异常，已改用本地模型"
	case FailRejected:
		return "在线端点拒绝了请求（可能不支持该格式或语种），已改用本地模型"
	default:
		return "在线转写失败，已改用本地模型"
	}
}

// currentOnlineLocked ignores state updates from an in-flight, superseded configuration.
func (r *Router) currentOnlineLocked(slot *onlineSlot) bool {
	return r.source == contract.ModelSourceOnline && r.online == slot && r.onlineRevision == slot.revision
}

// markDegraded 置降级标记并播报。
func (r *Router) markDegraded(slot *onlineSlot, note string, cause *apierr.Err) {
	r.mu.Lock()
	if !r.currentOnlineLocked(slot) {
		r.mu.Unlock()
		return
	}
	first := !r.degraded
	r.degraded = true
	r.degradedAt = r.now()
	r.status.Degraded = true
	r.status.DegradeNote = note
	r.status.State = contract.EngineStateDegraded
	r.status.LastError = toErrorDTO(cause)
	st := r.status
	r.mu.Unlock()

	r.notify(st)
	if first {
		r.mu.RLock()
		fn := r.onDegrade
		r.mu.RUnlock()
		if fn != nil {
			fn(note)
		}
	}
}

// clearDegraded 清降级标记（只在在线转写成功后调用）。
func (r *Router) clearDegraded(slot *onlineSlot) {
	r.mu.Lock()
	if !r.currentOnlineLocked(slot) {
		r.mu.Unlock()
		return
	}
	was := r.degraded
	if !was && r.status.State == contract.EngineStateReady {
		r.mu.Unlock()
		return
	}
	r.degraded = false
	r.degradedAt = time.Time{}
	r.status.Degraded = false
	r.status.DegradeNote = ""
	stateChanged := r.status.State != contract.EngineStateReady
	r.status.State = contract.EngineStateReady
	r.status.LastError = nil
	st := r.status
	r.mu.Unlock()
	if was {
		logx.For("asr").Info("在线引擎恢复，降级标记已清除")
	}
	if was || stateChanged {
		r.notify(st)
	}
}

// setLastError 只更新 LastError，不改 State（避免把 ready 误标成 failed）。
func (r *Router) setLastError(err *apierr.Err) {
	r.mu.Lock()
	r.status.LastError = toErrorDTO(err)
	r.mu.Unlock()
}

// maybeSwitchBack 实现 AutoSwitchBack 的**显式阈值**回升。
//
// 之所以默认关闭且必须带窗口：网络抖动会让「失败→回升→再失败」循环刷屏，
// 因此即使 policy.AutoSwitchBack=true，也只在降级持续 ≥ SwitchBackWindow（默认 5 分钟）
// 后才尝试一次在线预热；成功则清除降级，失败则把窗口重新计时。
func (r *Router) maybeSwitchBack() error {
	r.mu.RLock()
	degraded := r.degraded
	policy := r.policy
	since := r.degradedAt
	src := r.source
	r.mu.RUnlock()

	if !degraded || !policy.AutoSwitchBack || src != contract.ModelSourceOnline {
		return nil
	}
	if r.now().Sub(since) < r.switchBackAfter {
		return nil
	}
	// 到点尝试回升：失败也保持降级，只是把窗口重新计时。
	if err := r.Reload(); err != nil {
		r.mu.Lock()
		r.degradedAt = r.now()
		r.mu.Unlock()
		return err
	}
	return nil
}

// activeSessionFn 返回注入的活跃会话判定（可能为 nil）。
func (r *Router) activeSessionFn() func() bool { return r.activeSession }

// notify 调用状态回调（在锁外，回调内可以安全地再读 Router）。
func (r *Router) notify(st contract.EngineStatus) {
	r.mu.RLock()
	fn := r.onStatus
	r.mu.RUnlock()
	if fn != nil {
		fn(st)
	}
}

// toErrorDTO 把 apierr.Err 转成 contract.ErrorDTO（不再持有 *apierr.Err 指针）。
func toErrorDTO(e *apierr.Err) *contract.ErrorDTO {
	if e == nil {
		return nil
	}
	return &contract.ErrorDTO{
		Code:      e.Code,
		Message:   e.Message,
		Action:    e.Action,
		Detail:    e.Detail,
		Retryable: e.Retryable,
	}
}

// fromErrorDTO 是 toErrorDTO 的反向，用于把快照还原成 apierr.Err（Health 用）。
func fromErrorDTO(d *contract.ErrorDTO) *apierr.Err {
	if d == nil {
		return nil
	}
	e := apierr.New(d.Code)
	e.Message = d.Message
	e.Action = d.Action
	e.Detail = d.Detail
	e.Retryable = d.Retryable
	return e
}
