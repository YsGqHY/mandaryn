// Package transcriptsvc 是「延后转写 / 时间轴」的 L3 薄壳（FR-03-4 / FR-04 / API §2.4）。
//
// 职责边界（铁律，与 enginesvc 一致）：
//  1. 参数校验与错误码映射；
//  2. 持久化读写（本目录的 store.go：GORM ↔ transcribe.Store 端口）；
//  3. 路径解析（音频路径必须经 paths.Resolver，ADR-02）；
//  4. 事件转发素材（EV-16 `transcription:state` 载荷由 L2 产出，本层交给装配层
//     注入的 OnState 去 Emit —— 本层**不直接**发 Wails 事件）。
//
// **不做**任何转写决策（分句 / 幂等 / 续跑 / 让位 / 并发上限全在 internal/transcribe）。
package transcriptsvc

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/asr"
	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/paths"
	"vip.oberon.mandaryn/internal/storage"
	"vip.oberon.mandaryn/internal/transcribe"
	"vip.oberon.mandaryn/internal/utils/logx"
)

// Deps 是装配层注入的依赖。
//
// Engine 用最小接口（transcribe.Recognizer）：asr.Router 同时满足它，
// 但本层不需要 Feed/Warm，因此不必把整个 Engine 契约带进来。
type Deps struct {
	// Holder 数据库句柄持有器（切换数据目录后 Current() 会变，**不要**缓存 *DB）。
	Holder *storage.Holder
	// Engine 转写引擎（asr.Router）。
	Engine transcribe.Recognizer
	// Resolver 路径裁决器（dataRoot 用来把 sessions.audio_path 拼成绝对路径）。
	Resolver *paths.Resolver
	// OnState 状态变化回调（EV-16 载荷）。装配层接成 app.Event.Emit。
	OnState func(contract.TranscriptionState)
	// Idle 整机空闲判定（NFR-08 由 S8 提供）；nil 视为永远空闲。
	Idle func() bool
	// IdleFor 按任务来源判定（见 transcribe.Options.IdleFor）；nil 时回退 Idle。
	//
	// 装配层传 perf.Service.IdleFor：整机忙时只让「空闲自动」任务让位，
	// 用户手动发起的转写照常执行（FR-03-4）。
	IdleFor func(trigger string) bool
	// Allowed 推理预算闸门（NFR-08）；nil 不限制。
	//
	// 与 IdleFor 的区别：IdleFor 是「现在不划算，先让位」，Allowed 是「现在禁止」——
	// 预算耗尽（P 档）时连手动任务也拒绝，返回可重试的 ERR_PERF_SUSPENDED。
	// 装配层传 perf.Service.CheckInferenceAllowed。
	Allowed func() error
	// PollInterval 调度器空闲轮询间隔；0 用默认值。
	PollInterval time.Duration
	// Live 是边录边转（FR-03-10，默认关）的可选接线。
	//
	// 零值 = 只做延后转写（默认路径完全不变）。装配层把事件桥接到 EV-04 / EV-05，
	// 并传预算闸门（L1 及以上关第二遍离线校正），即完成接入。
	Live LiveDeps
}

// LiveDeps 是边录边转的接线点（全部可选）。
type LiveDeps struct {
	// OnPartial partial 上报（EV-04 `transcript:partial`，`finalized=false`）。
	//
	// partial **不落库**：它只是「正在说的话」，前端覆盖显示即可。
	OnPartial func(contract.Segment)
	// OnSegment 定稿上报（EV-05 `transcript:segment`，`finalized=true`）。
	OnSegment func(contract.Segment)
	// OnState 运行期状态（推理失败已停用 / 正常结束）；用于诊断与非阻塞提示。
	OnState func(transcribe.LiveState)
	// Refine 第二遍离线校正（ARCH §2.3）：拿到一句音频用离线模型重算。
	//
	// nil 表示不做第二遍（只保留流式结果，相当于 L1 档的形态）。
	Refine func(seg asr.AudioSegment) (asr.FinalResult, error)
	// RefineAllowed 预算闸门：false 时跳过第二遍校正（NFR-08）。
	// nil 视为允许。
	RefineAllowed func() bool
	// Vad 分句参数：**必须与延后转写一致**，否则 `(session_id, seq)` 会错位。
	// 零值 = 用默认参数（与 Runner 的默认一致）。
	Vad transcribe.VadConfig
}

// Service 暴露给前端：发起 / 取消 / 查询转写，以及时间轴读取。
type Service struct {
	holder   *storage.Holder
	resolver *paths.Resolver
	engine   transcribe.Recognizer

	runner    *transcribe.Runner
	scheduler *transcribe.Scheduler
	reader    *transcribe.TimelineReader
	store     *store
	live      LiveDeps

	mu      sync.RWMutex
	onState func(contract.TranscriptionState)
}

// New 构造服务。
func New(d Deps) (*Service, error) {
	if d.Holder == nil {
		return nil, apierr.New(apierr.CodeDBWrite).WithDetail("transcriptsvc 未拿到 storage.Holder")
	}
	if d.Engine == nil {
		return nil, apierr.New(apierr.CodeEngineNotReady).WithDetail("transcriptsvc 未拿到转写引擎")
	}
	if d.Resolver == nil {
		return nil, apierr.New(apierr.CodePathUnwritable).WithDetail("transcriptsvc 未拿到 paths.Resolver")
	}

	st := &store{holder: d.Holder}
	s := &Service{
		holder:   d.Holder,
		resolver: d.Resolver,
		engine:   d.Engine,
		store:    st,
		live:     d.Live,
		onState:  d.OnState,
	}

	reader, err := transcribe.NewTimelineReader(st)
	if err != nil {
		return nil, err
	}
	s.reader = reader

	runner, err := transcribe.NewRunner(st, d.Engine, s.openAudio, transcribe.Options{
		Idle:    d.Idle,
		IdleFor: d.IdleFor,
		Allowed: d.Allowed,
		OnState: s.emit,
	})
	if err != nil {
		return nil, err
	}
	s.runner = runner

	sched, err := transcribe.NewScheduler(runner, transcribe.SchedulerOptions{Poll: d.PollInterval})
	if err != nil {
		return nil, err
	}
	s.scheduler = sched
	return s, nil
}

// String 让 Wails 的服务名稳定（与其它 service 一致）。
func (s *Service) String() string { return "transcriptsvc" }

// setOnState 允许装配层在构造后再接线（避免装配顺序造成的回调丢失）。
//
// 刻意**不导出**：func 类型的导出方法会被 Wails 绑定生成器扫进前端 API，
// 既触发 `function types are not supported by encoding/json` 警告，
// 又会生成无法调用的畸形前端方法（与 storagesvc / capturesvc 同类问题的处置一致）。
// 装配层走 Deps.OnState 注入即可。
func (s *Service) setOnState(fn func(contract.TranscriptionState)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onState = fn
	s.runner.SetOnState(s.emit)
}

// Close 停止调度器（取消在跑任务；已落库的进度保留）。
func (s *Service) Close() {
	if s.scheduler != nil {
		s.scheduler.Close()
	}
}

// —— API-4.5 ——

// StartTranscription 对已存录音实例发起转写（手动触发）。
//
// 状态机：`none | failed | done → queued → running`；已 `done` 时等价整段重转。
//
// 可能错误：ERR_NOT_FOUND、ERR_ENGINE_NOT_READY、ERR_STATE_FORBIDDEN（已有任务）、
// ERR_DB_WRITE。ERR_MODEL_NOT_FOUND / ERR_PERF_SUSPENDED 由引擎侧与调度侧在
// 执行期产生（本方法只做「能不能开始」的前置校验）。
func (s *Service) StartTranscription(ctx context.Context, sessionID string) (contract.TranscriptionState, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return contract.TranscriptionState{}, apierr.New(apierr.CodeInvalidArgument).
			WithDetail("sessionId 为空")
	}
	// 先确认会话存在，否则会先报引擎错误再报找不到，前端提示会误导。
	if _, err := s.store.LoadSession(ctx, sessionID); err != nil {
		return contract.TranscriptionState{}, apierr.From(err, apierr.CodeNotFound)
	}
	if err := s.requireEngine(ctx); err != nil {
		return s.currentState(ctx, sessionID), err
	}
	st, err := s.scheduler.Enqueue(ctx, transcribe.TaskRequest{
		SessionID: sessionID,
		Trigger:   contract.TriggerManual,
	})
	if err != nil {
		if cur := s.currentState(ctx, sessionID); cur.SessionID != "" {
			return cur, apierr.From(err, apierr.CodeStateForbidden)
		}
		return st, apierr.From(err, apierr.CodeStateForbidden)
	}
	logx.For("transcriptsvc").Info("已发起转写", "sessionId", sessionID, "trigger", contract.TriggerManual)
	return st, nil
}

// —— API-4.6 ——

// CancelTranscription 取消排队 / 运行中的任务；已转句子保留，状态回 `none`。
//
// 可能错误：ERR_NOT_FOUND、ERR_STATE_FORBIDDEN（没有可取消的任务）。
func (s *Service) CancelTranscription(ctx context.Context, sessionID string) (contract.TranscriptionState, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return contract.TranscriptionState{}, apierr.New(apierr.CodeInvalidArgument).
			WithDetail("sessionId 为空")
	}
	if _, err := s.store.LoadSession(ctx, sessionID); err != nil {
		return contract.TranscriptionState{}, apierr.From(err, apierr.CodeNotFound)
	}
	ok, err := s.scheduler.Cancel(ctx, sessionID)
	if err != nil {
		return s.currentState(ctx, sessionID), apierr.From(err, apierr.CodeDBWrite)
	}
	if !ok {
		return s.currentState(ctx, sessionID), apierr.New(apierr.CodeStateForbidden).
			WithAction("start-transcription").
			WithDetail("该会话没有排队或运行中的转写任务")
	}
	// 取消是异步的：等执行侧真正停下，保证「取消返回时状态已经是 none」。
	// 超时也不报错——执行侧会在下一个检查点收尾，状态终将一致。
	waitCtx, cancelWait := context.WithTimeout(context.WithoutCancel(ctx), cancelWaitTimeout)
	defer cancelWait()
	if err := s.scheduler.WaitStopped(waitCtx, sessionID); err != nil {
		logx.For("transcriptsvc").Warn("取消后等待收尾超时", "sessionId", sessionID)
	}
	return s.currentState(ctx, sessionID), nil
}

// cancelWaitTimeout 是「取消后等待执行侧收尾」的上限。
//
// 取值依据：检查点粒度是一句（最长单句 MaxSegmentMs=15s，但那是极端长句；
// 常规一句话 1–3s）。3 秒足够覆盖绝大多数场景，超时也不影响正确性。
const cancelWaitTimeout = 3 * time.Second

// —— API-4.7 ——

// GetTranscription 幂等查询进度（窗口重开 / 断线重连时用）。
//
// 可能错误：ERR_NOT_FOUND。
func (s *Service) GetTranscription(ctx context.Context, sessionID string) (contract.TranscriptionState, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return contract.TranscriptionState{}, apierr.New(apierr.CodeInvalidArgument).
			WithDetail("sessionId 为空")
	}
	row, err := s.store.LoadSession(ctx, sessionID)
	if err != nil {
		return contract.TranscriptionState{}, apierr.From(err, apierr.CodeNotFound)
	}
	// 运行期进度以内存快照为准：库里的 progress 是每句落一次，比内存落后一句；
	// 直接查库会让进度条看起来「卡住」。
	if live, ok := s.runner.Live(sessionID); ok {
		return live, nil
	}
	return transcribe.StateOf(row, "", row.SegmentCount), nil
}

// —— API-4.1 ——

// GetTimeline 一次性取整段（≤2 万条），含时间轴标记。
//
// 可能错误：ERR_NOT_FOUND。
func (s *Service) GetTimeline(ctx context.Context, sessionID string) (contract.Timeline, error) {
	return s.reader.Timeline(ctx, strings.TrimSpace(sessionID))
}

// —— API-4.2 ——

// GetTimelinePage 超大通话分页。
func (s *Service) GetTimelinePage(ctx context.Context, sessionID string, offset, limit int) (contract.TimelinePage, error) {
	return s.reader.Page(ctx, strings.TrimSpace(sessionID), offset, limit)
}

// —— API-4.3 ——

// SearchSegments 片段级检索（带跳转时间）。
func (s *Service) SearchSegments(ctx context.Context, q contract.SegmentQuery) ([]contract.Segment, error) {
	return s.reader.Search(ctx, q)
}

// —— API-4.4 ——

// UpdateSegmentText 人工修正片段文本（P1），置 edited=true。
func (s *Service) UpdateSegmentText(ctx context.Context, segmentID int64, text string) error {
	return s.reader.UpdateSegmentText(ctx, segmentID, text)
}

// —— 边录边转（FR-03-10，默认关）——

// streamingRouter 是**可选**的流式能力入口（asr.Router 实现了它）。
//
// 用类型断言而不是把 asr.Router 写进 Deps：本域的默认路径只需要 Finalize，
// 把整个 Router 拖进来会让「测试用假引擎」也必须实现流式能力。
type streamingRouter interface {
	Streaming() (asr.StreamingEngine, error)
	StreamingAvailable() error
	StreamingEngineID() string
}

// LiveAvailability 报告边录边转当前是否可用（API-4.8）。
//
// **便宜**：只做查表式探测，不加载模型。录制页每次挂载都会问一次，
// 所以这里绝不能顺手去预热流式引擎（加载数秒）。
//
// 它**不**读开关值：开关在 `app_settings.live_transcribe`，前端从 appsettings
// 快照里拿。两个问题分开答，前端才能给出「开关开着但缺模型包」这种提示。
func (s *Service) LiveAvailability(ctx context.Context) (contract.LiveAvailability, error) {
	router, ok := s.engine.(streamingRouter)
	if !ok {
		return contract.LiveAvailability{
			ErrorCode: apierr.CodeLocalUnavailable,
			Reason:    "本构建不含流式引擎，边录边转不可用",
		}, nil
	}
	if err := router.StreamingAvailable(); err != nil {
		e := apierr.From(err, apierr.CodeLocalUnavailable)
		return contract.LiveAvailability{
			ErrorCode: e.Code,
			Reason:    firstNonEmpty(e.Detail, e.Message, "流式引擎不可用"),
		}, nil
	}
	return contract.LiveAvailability{
		Available: true,
		EngineID:  router.StreamingEngineID(),
	}, nil
}

// NewLiveSink 为一个会话创建边录边转接收端（FR-03-10）。
//
// 由装配层在 `recording.Options.NewLiveSink` 里调用；返回的 `*transcribe.LiveRunner`
// 结构化满足 `recording.LiveSink`（本层不 import recording，避免 L3 → L2 的横向依赖）。
//
// **快速返回**：本方法在 `recording.Start` 的同步路径上，那时采集管道已经打开，
// 所以这里**绝不加载**流式模型（本地流式模型加载数秒，会把会话开头的音频顶掉）。
// 引擎用 `asr.LazyStreaming` 包一层，真正加载发生在 LiveRunner 的推理 goroutine
// 第一次 `Accept` 时；期间的音频按「队满丢最旧」处理，采集与落盘不受影响。
//
// 可能错误：ERR_LOCAL_UNAVAILABLE（本构建无流式能力）/ ERR_INVALID_ARGUMENT。
// 模型缺失、加载失败都不在这里报（那时还没有加载），而是由 LiveRunner 通过
// `LiveDeps.OnState` 报 `failed`。**调用方应当忽略这些错误**：
// 录制绝不因推理能力缺失而起不来（FR-03-6），只是本会话没有边录边转。
func (s *Service) NewLiveSink(ctx context.Context, sessionID string) (*transcribe.LiveRunner, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, apierr.New(apierr.CodeInvalidArgument).WithDetail("sessionId 为空")
	}
	router, ok := s.engine.(streamingRouter)
	if !ok {
		return nil, apierr.New(apierr.CodeLocalUnavailable).
			WithAction("download-model").
			WithDetail("本构建不含流式引擎，边录边转不可用")
	}
	// 只做**便宜**判定（查模型包在不在，不加载）；不可用就当场放弃本会话的旁路。
	if err := router.StreamingAvailable(); err != nil {
		return nil, apierr.From(err, apierr.CodeLocalUnavailable)
	}
	eng := asr.LazyStreaming(router.Streaming)
	// 每个会话先清空解码状态：流式实例在 Router 里复用，上一个会话的尾音
	// 不能让下一个会话的第一句串味。未加载时 Reset 只是打标记（见 LazyStreaming）。
	if err := eng.Reset(); err != nil {
		return nil, apierr.From(err, apierr.CodeEngineLoad)
	}

	opts := transcribe.LiveOptions{
		Store:         s.store,
		Engine:        eng,
		SessionID:     sessionID,
		Vad:           s.live.Vad,
		Refine:        s.live.Refine,
		RefineAllowed: s.live.RefineAllowed,
		OnPartial:     s.live.OnPartial,
		OnSegment:     s.live.OnSegment,
		OnState:       s.live.OnState,
	}
	// 没有显式给 Refine 时用本域引擎做第二遍（ARCH §2.3 的默认形态）：
	// `RefineAllowed` 把「预算够不够」这件事留给预算域，本层不判档位。
	if opts.Refine == nil && opts.RefineAllowed != nil {
		engine := s.engine
		opts.Refine = func(seg asr.AudioSegment) (asr.FinalResult, error) {
			return engine.Finalize(seg)
		}
	}
	runner, err := transcribe.NewLiveRunner(opts)
	if err != nil {
		return nil, apierr.From(err, apierr.CodeLocalUnavailable)
	}
	logx.For("transcriptsvc").Info("边录边转已启动", "sessionId", sessionID,
		"engineId", firstNonEmpty(eng.ID(), "待加载"))
	return runner, nil
}

// ShouldAutoTranscribe 报告某会话是否还应参与「空闲自动转写」（FR-03-4）。
//
// 开启过边录边转的会话在录制结束时已产出定稿片段，再自动整段重跑等于推理两遍；
// 用户仍可手动重转（幂等覆盖）。
func (s *Service) ShouldAutoTranscribe(ctx context.Context, sessionID string) (bool, error) {
	row, err := s.store.LoadSession(ctx, strings.TrimSpace(sessionID))
	if err != nil {
		return false, apierr.From(err, apierr.CodeNotFound)
	}
	return transcribe.ShouldAutoTranscribe(row), nil
}

// firstNonEmpty 返回第一个非空串（错误说明的兜底链）。
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// —— 内部 ——

// emit 把 L2 的状态快照交给装配层注入的回调。
func (s *Service) emit(st contract.TranscriptionState) {
	s.mu.RLock()
	fn := s.onState
	s.mu.RUnlock()
	if fn != nil {
		fn(st)
	}
}

// currentState 读取当前状态快照（读失败时返回零值，不覆盖主错误）。
func (s *Service) currentState(ctx context.Context, sessionID string) contract.TranscriptionState {
	row, err := s.store.LoadSession(ctx, strings.TrimSpace(sessionID))
	if err != nil {
		return contract.TranscriptionState{}
	}
	return transcribe.StateOf(row, "", row.SegmentCount)
}

// engineHealth 是可选的引擎健康接口（asr.Router 实现了它）。
type engineHealth interface {
	Health(ctx context.Context) asr.HealthStatus
}

// engineWarmer 是可选的预热接口（asr.Router 实现了它；已就绪时幂等直接返回）。
type engineWarmer interface {
	Warm(ctx context.Context) error
}

// requireEngine 校验引擎可承接 Finalize（API-4.5 的 ERR_ENGINE_NOT_READY）。
//
// 只在**发起时**检查：执行期引擎掉线由引擎层自己的降级矩阵处理，
// 那时报错会走 failed + 错误码，而不是把已排队的任务悄悄扔掉。
func (s *Service) requireEngine(ctx context.Context) error {
	h, ok := s.engine.(engineHealth)
	if !ok {
		return nil
	}
	st := h.Health(ctx)
	if st.Ready() {
		return nil
	}
	// 未就绪时补一次预热：模型可能是启动之后才下载 / 修复的，不补的话用户只能
	// 重启应用或来回切换模型来源才能转写。启动预热尚在进行时，Router 内部串行，
	// 这里会等它结束再看结果，而不是立刻报「未就绪」。结果以 Health 为准。
	if w, ok := s.engine.(engineWarmer); ok {
		_ = w.Warm(ctx)
		st = h.Health(ctx)
		if st.Ready() {
			return nil
		}
	}
	// 模型缺失 / 损坏时透出该码：它对应「去模型页下载」这条可行动提示。
	// 包在 ERR_ENGINE_NOT_READY（「请稍后重试」）里会把用户导向无效重试（G-E2E-03）。
	if st.Err != nil {
		switch st.Err.Code {
		case apierr.CodeModelNotFound, apierr.CodeModelCorrupt:
			return apierr.New(st.Err.Code).WithCause(st.Err).WithDetail("%s", st.Err.Detail)
		}
	}
	e := apierr.New(apierr.CodeEngineNotReady)
	if st.Err != nil && st.Err.Code != "" {
		e = e.WithCause(st.Err)
	}
	return e.WithDetail("%s", st.Note)
}

// openAudio 把会话的音频路径解析成可读文件（ADR-02：路径一律经 Resolver）。
//
// 错误码：音频缺失 → ERR_NOT_FOUND；dataRoot 解析失败 → ERR_PATH_UNWRITABLE。
func (s *Service) openAudio(_ context.Context, session transcribe.SessionRow) (transcribe.AudioSource, error) {
	rel := strings.TrimSpace(session.AudioPath)
	if rel == "" {
		return nil, apierr.New(apierr.CodeNotFound).WithDetail("会话没有音频路径")
	}
	root, err := s.resolver.Effective(paths.KeyDataRoot)
	if err != nil {
		return nil, apierr.From(err, apierr.CodePathUnwritable)
	}
	full := rel
	if !filepath.IsAbs(full) {
		full = filepath.Join(root, rel)
	}
	f, err := os.Open(full)
	if err != nil {
		// 只记相对路径（不含用户主目录，避免诊断包泄露路径信息）。
		return nil, apierr.New(apierr.CodeNotFound).
			WithAction("re-record").
			WithDetail("音频文件不可读：%s", rel).
			WithCause(err)
	}
	return f, nil
}
