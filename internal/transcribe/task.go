package transcribe

import (
	"context"
	"errors"
	"sync"
	"time"
	"unicode/utf8"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/asr"
	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/utils/logx"
)

// 转写任务编排（FR-03-4 / ARCH §1.2 转写阶段）。
//
// 一次执行的形状：
//
//	读会话 → 校验状态轴 → 占会话锁 → 打开音频 → 逐窗读 → VAD 分句
//	      → 逐句: [断点跳过 | 取消 | 让位] → Engine.Finalize → 片段 upsert → 推进进度
//	      → done（回填计数与引擎 ID）
//
// 约束（硬性）：
//   - 只写 `transcription_status` 这条轴，**永不**写 `sessions.state`（ARCH §6.4）；
//   - 失败不改音频、不删片段，「重试」是用户可见动作；
//   - 日志只记时长 / 句数 / 错误码，音频与正文永不进日志（NFR-03-5）。
const (
	// DefaultWindowMs 每次从音频读取的窗口时长。
	//
	// 取 200ms：既让 VAD 的两帧判断（20ms 帧）有足够前瞻，又不会因为窗口过大
	// 让「取消 / 让位」需要等太久（每窗一次可中断点）。
	DefaultWindowMs = 200
)

// TaskRequest 是一次转写请求。
type TaskRequest struct {
	// SessionID 目标录音实例。
	SessionID string
	// Trigger manual | idle（写入 DTO，用于前端区分「我点的」与「空闲自动」）。
	Trigger string
	// Restart true 表示整段重转（会话已 done，进度归零）。由 L3 依状态判定。
	Restart bool
}

// Result 是一次执行的结果。
//
// Outcome 是**运行期结论**（done / failed / canceled / yielded），不是持久状态：
// canceled → 落库 `none`，yielded → 落库 `queued`（见 state.go 的说明）。
type Result struct {
	// Outcome 见 OutcomeXxx 常量。
	Outcome string
	// State 执行结束时的对外状态快照（可直接作为 EV-16 载荷）。
	State contract.TranscriptionState
	// Err 失败时的结构化错误；让位 / 取消时为内部信号错误。
	Err *apierr.Err
	// Written 本次落库句数。
	Written int
	// Skipped 断点续跑跳过句数。
	Skipped int
	// EngineCalls 本次调用引擎次数（= 未跳过的句子数，单测据此断言续跑行为）。
	EngineCalls int
}

// Options 是 Runner 的可调参数。
type Options struct {
	// Vad 分句参数。
	Vad VadConfig
	// WindowMs 音频读取窗口时长（毫秒）。
	WindowMs int
	// Idle 整机空闲判定；nil 视为永远空闲。
	//
	// 真实 CPU 采样属 NFR-08（S8），本期只消费注入的判定：
	// 返回 false 表示「前台忙」，任务应让位并保留进度。
	Idle func() bool
	// IdleFor 按任务来源判定（`contract.TriggerManual` / `contract.TriggerIdle`）。
	//
	// 与 Idle 的关系：**IdleFor 优先**；未注入时统一回退到 Idle。
	// 区分来源是 NFR-08 的硬要求（FR-03-4）：整机忙时只应让「空闲自动」任务让位，
	// 用户手动发起的转写必须照常执行——否则「点了转写没反应」。
	IdleFor func(trigger string) bool
	// Allowed 推理预算闸门（NFR-08）：返回错误表示当前档位不允许起跑推理。
	//
	// 装配层传 `perf.Service.CheckInferenceAllowed`（P 档 → `ERR_PERF_SUSPENDED`）。
	// nil 表示不限制（既有行为不变）。
	//
	// 与 IdleFor 的分工（两道不同的闸，都要有）：
	//   - IdleFor 是**优先级**：整机忙 → 空闲自动任务让位（保留进度，稍后自动重跑）；
	//   - Allowed 是**禁止**：预算耗尽（P）→ 连手动任务也拒绝，返回可重试错误。
	// 少了 Allowed 会出现「P 档下用户点重新转写，任务照跑，把整机压得更狠」。
	//
	// 它**只拦推理**：任何录制侧动作都不该走这里（录制永不因预算中断）。
	Allowed func() error

	// OnState 状态变化回调（L3 转发为 EV-16 `transcription:state`）。
	OnState func(contract.TranscriptionState)
	// Now 时间源（测试注入）。
	Now func() time.Time
}

// Runner 执行单个转写任务。可被并发调用（内部用 SessionLock 限制同会话单飞行）。
type Runner struct {
	store  Store
	engine Recognizer
	open   AudioOpener
	lock   *SessionLock

	mu   sync.RWMutex
	opts Options
	// live 是运行期状态快照（会话 → 最新状态）。
	//
	// 为什么需要：库里的 progress 是「每句落一次」，比内存落后一句；
	// GetTranscription 直接读库会让进度条看起来卡住。任务结束即清除，
	// 避免重启前的陈旧快照盖住库里的真实值。
	live map[string]contract.TranscriptionState
}

// NewRunner 构造执行器。
func NewRunner(store Store, engine Recognizer, open AudioOpener, opts Options) (*Runner, error) {
	if store == nil {
		return nil, apierr.New(apierr.CodeDBWrite).WithDetail("transcribe 未拿到 Store")
	}
	if engine == nil {
		return nil, apierr.New(apierr.CodeEngineNotReady).WithDetail("transcribe 未拿到引擎")
	}
	if open == nil {
		return nil, apierr.New(apierr.CodeNotFound).WithDetail("transcribe 未拿到音频打开器")
	}
	if opts.WindowMs <= 0 {
		opts.WindowMs = DefaultWindowMs
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Runner{
		store:  store,
		engine: engine,
		open:   open,
		opts:   opts,
		lock:   NewSessionLock(),
		live:   make(map[string]contract.TranscriptionState),
	}, nil
}

// Lock 暴露会话锁（调度器用同一把锁做入队去重）。
func (r *Runner) Lock() *SessionLock { return r.lock }

// Store 返回后端存储（时间轴查询复用同一实现）。
func (r *Runner) Store() Store { return r.store }

// idle 返回当前是否允许执行。
func (r *Runner) idle() bool {
	if r.opts.Idle == nil {
		return true
	}
	return r.opts.Idle()
}

// idleFor 判断某个来源的任务当前是否允许执行。
//
// 注入 IdleFor 时按来源区分（手动任务应恒真）；未注入时回退 idle()，
// 保持「只有一个全局空闲判定」的旧行为不变。
func (r *Runner) idleFor(trigger string) bool {
	if r.opts.IdleFor != nil {
		return r.opts.IdleFor(trigger)
	}
	return r.idle()
}

// allowed 返回推理预算闸门的结论（未注入时恒允许）。
//
// 每次 Run 读一次：档位在采样 goroutine 里随时变化，用最新快照而不是构造时的快照。
func (r *Runner) allowed() error {
	r.mu.RLock()
	fn := r.opts.Allowed
	r.mu.RUnlock()
	if fn == nil {
		return nil
	}
	return fn()
}

// SetAllowed 替换推理预算闸门（装配层迟到接线时用，等价于 Options.Allowed）。
func (r *Runner) SetAllowed(fn func() error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.opts.Allowed = fn
}

// SetOnState 替换状态回调（装配层迟到接线时用）。
func (r *Runner) SetOnState(fn func(contract.TranscriptionState)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.opts.OnState = fn
}

// Live 返回运行期状态快照（仅排队中 / 转写中有意义）。
func (r *Runner) Live(sessionID string) (contract.TranscriptionState, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	st, ok := r.live[sessionID]
	return st, ok
}

// emit 推送状态快照（回调可为 nil），并更新运行期快照。
func (r *Runner) emit(st contract.TranscriptionState) {
	r.mu.Lock()
	if r.live == nil {
		r.live = make(map[string]contract.TranscriptionState)
	}
	r.live[st.SessionID] = st
	fn := r.opts.OnState
	r.mu.Unlock()

	if fn != nil {
		fn(st)
	}
}

// clearLive 丢弃运行期快照（任务结束后以库为准）。
func (r *Runner) clearLive(sessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.live, sessionID)
}

// Run 执行一次转写。
//
// 返回的 error 只表示「任务无法继续」的结构化原因；
// 业务结论（完成 / 失败 / 取消 / 让位）看 Result.Outcome。
func (r *Runner) Run(ctx context.Context, req TaskRequest) (Result, error) {
	sessionID := req.SessionID
	if sessionID == "" {
		return Result{}, apierr.New(apierr.CodeInvalidArgument).WithDetail("sessionId 为空")
	}

	// 第一道闸：推理预算（NFR-08）。放在最前面，理由有三：
	//   1. P 档下不该有任何推理动作，连开音频、读库都不必做（省一次 I/O 与一次小高峰）；
	//   2. 拒绝必须是**无副作用**的：不置 failed、不写片段、不动音频，用户稍后点一次即可重试
	//      （所以这里不调用 r.fail —— 它会把状态写成 failed，看起来像「转写出错了」）；
	//   3. 与让位（IdleFor）语义分开：让位 → 状态 queued（稍后自动跑）；
	//      暂停 → 直接报错（可重试），因为档位恢复时间不由调度器决定。
	if err := r.allowed(); err != nil {
		apiErr := apierr.From(err, apierr.CodePerfSuspended)
		logx.For("transcribe").Info("预算暂停推理，转写被拒绝",
			"session", sessionID, "trigger", req.Trigger, "code", apiErr.Code)
		return Result{}, apiErr
	}

	// 账务 ctx：所有状态/片段写入都用它，不让「取消」把收尾写入一起打断
	// （否则会留下「状态 running 但没有任务」的脏状态）。取消只通过检查点协作生效。
	writeCtx := context.WithoutCancel(ctx)

	session, err := r.store.LoadSession(writeCtx, sessionID)
	if err != nil {
		return Result{}, apierr.From(err, apierr.CodeNotFound)
	}

	// 状态轴准入（执行侧）：`running` 说明正常有任务在跑（真并发则靠下面的会话锁兜底），
	// 其余状态都可以执行——`queued` 正是让位后等待重跑的状态，不能拒。
	if !CanExecute(session.TranscriptionStatus) {
		return Result{State: newState(session, req.Trigger, session.SegmentCount)}, errBusy(sessionID)
	}
	if !r.lock.TryAcquire(sessionID) {
		return Result{State: newState(session, req.Trigger, session.SegmentCount)}, errBusy(sessionID)
	}
	defer r.lock.Release(sessionID)

	startedAt := r.opts.Now().UnixMilli()

	// 打开音频：失败不改动任何记录，直接置 failed（带错误码供重试入口）。
	src, err := r.openAudio(writeCtx, session)
	if err != nil {
		return r.fail(ctx, session, req, startedAt, err)
	}
	defer func() { _ = src.Close() }()

	audioMs := src.TotalMs()
	if audioMs <= 0 {
		audioMs = session.DurationMs
	}
	session.DurationMs = audioMs

	// 进入 running：只更新转写轴（Status 之外一概不动）。
	running := session
	running.TranscriptionStatus = StatusRunning
	if err := r.store.UpdateTranscription(writeCtx, TranscriptionUpdate{
		SessionID: sessionID,
		Status:    StatusRunning,
	}); err != nil {
		return Result{}, apierr.From(err, apierr.CodeDBWrite)
	}
	if req.Restart {
		// 重转从 0 开始：进度清零，否则「全转完了」会让本次一句都不发。
		var zero int64
		running.TranscribeProgressMs = 0
		if err := r.store.UpdateTranscription(writeCtx, TranscriptionUpdate{
			SessionID:  sessionID,
			ProgressMs: &zero,
		}); err != nil {
			return Result{}, apierr.From(err, apierr.CodeDBWrite)
		}
	}
	r.emit(newState(running, req.Trigger, session.SegmentCount))

	maxSeq, err := r.store.MaxSeq(writeCtx, sessionID)
	if err != nil {
		return r.fail(ctx, running, req, startedAt, apierr.From(err, apierr.CodeDBWrite))
	}
	plan := PlanResume(session, maxSeq, req.Restart)

	exec := &execution{
		runner:    r,
		ctx:       ctx,
		writeCtx:  writeCtx,
		row:       running,
		trigger:   req.Trigger,
		plan:      plan,
		progress:  plan.ProgressMs,
		startedAt: startedAt,
	}

	// 逐窗读 + 分句 + 逐句转写。
	splitter := NewUtteranceSplitter(r.opts.Vad)
	window := int64(splitter.Config().SampleRate * r.opts.WindowMs / 1000)
	if window <= 0 {
		window = int64(splitter.Config().SampleRate / 5)
	}
	total := src.TotalFrames()
	for pos := int64(0); pos < total; {
		if err := exec.checkpoint(); err != nil {
			return exec.abort(ctx, session, err)
		}
		pcm, rerr := src.ReadFrames(pos, window)
		if rerr != nil {
			return exec.abort(ctx, session, apierr.From(rerr, apierr.CodeInvalidArgument))
		}
		if len(pcm) == 0 {
			break
		}
		pos += int64(len(pcm))
		if err := exec.consume(splitter.Push(pcm)); err != nil {
			return exec.abort(ctx, session, err)
		}
	}
	if err := exec.consume(splitter.Flush()); err != nil {
		return exec.abort(ctx, session, err)
	}

	return exec.finish(ctx, session, audioMs, src)
}

// openAudio 打开音频并解析 WAV 头。
func (r *Runner) openAudio(ctx context.Context, session SessionRow) (*wavSource, error) {
	raw, err := r.open(ctx, session)
	if err != nil {
		return nil, apierr.From(err, apierr.CodeNotFound)
	}
	src, err := openWAV(raw)
	if err != nil {
		_ = raw.Close()
		return nil, apierr.From(err, apierr.CodeInvalidArgument)
	}
	return src, nil
}

// fail 把任务置为 failed 并返回结果（**不动音频与已有片段**）。
func (r *Runner) fail(ctx context.Context, session SessionRow, req TaskRequest,
	startedAt int64, cause error) (Result, error) {

	e := apierr.From(cause, apierr.CodeInvalidArgument)
	failed := session
	failed.TranscriptionStatus = StatusFailed
	if err := r.store.UpdateTranscription(context.WithoutCancel(ctx), TranscriptionUpdate{
		SessionID: session.ID,
		Status:    StatusFailed,
	}); err != nil {
		// 连失败状态都写不进去：报 DB 错误，但保留原始原因在 Detail 里。
		return Result{}, apierr.From(err, apierr.CodeDBWrite)
	}
	st := newState(failed, req.Trigger, session.SegmentCount)
	st.ErrorCode = e.Code
	st.ProgressMs = session.TranscribeProgressMs
	st.StartedAt = startedAt
	st.FinishedAt = r.opts.Now().UnixMilli()
	r.emit(st)
	r.clearLive(session.ID)
	logx.For("transcribe").Warn("转写失败", "sessionId", session.ID,
		"code", e.Code, "progressMs", st.ProgressMs, "audioMs", session.DurationMs)
	return Result{Outcome: OutcomeFailed, State: st, Err: e}, nil
}

// execution 是单次执行的临时状态。
type execution struct {
	runner *Runner
	// ctx 只用于「是否该停」的判定（取消 / 让位检查点）。
	ctx context.Context
	// writeCtx 是**不受取消影响**的账务写入 context。
	//
	// 为什么分开：取消若同时打断状态落库，会出现「状态停在 queued / running 而
	// 实际没有任务」的脏状态，前端只能靠重启自愈。取消是协作式的——由检查点决定
	// 何时停，停了之后的收尾写入必须做完。
	writeCtx  context.Context
	row       SessionRow
	trigger   string
	plan      ResumePlan
	progress  int64
	startedAt int64

	prev        *SegmentRow
	written     int
	skipped     int
	engineCalls int
	chars       int64
	engineID    string
}

// checkpoint 是句间检查点：先看取消，再看是否该让位。
//
// 顺序有意义：用户取消必须立刻生效，不能因为「前台也忙」被降级成让位
// （那会让取消按钮看起来没反应，任务在空闲后又自己跑起来）。
func (e *execution) checkpoint() error {
	if err := e.ctx.Err(); err != nil {
		return errCanceled.WithCause(err)
	}
	if !e.runner.idleFor(e.trigger) {
		return errYielded
	}
	return nil
}

// consume 处理一批已完结的句子。
func (e *execution) consume(utts []Utterance) error {
	for _, u := range utts {
		if err := e.checkpoint(); err != nil {
			return err
		}
		if e.plan.Completed(u) {
			// 断点续跑：已完成句子不重复处理，**不调用引擎**。
			e.skipped++
			continue
		}
		if err := e.transcribeOne(u); err != nil {
			return err
		}
	}
	return nil
}

// transcribeOne 转写单句并落库。
func (e *execution) transcribeOne(u Utterance) error {
	e.engineCalls++
	res, err := e.runner.engine.Finalize(asr.AudioSegment{
		Samples:    u.Samples,
		SampleRate: u.SampleRate,
		SessionID:  e.row.ID,
		Seq:        u.Index,
		StartMs:    u.StartMs,
		EndMs:      u.EndMs,
	})
	if err != nil {
		return apierr.From(err, apierr.CodeEngineLoad)
	}
	if res.EngineID != "" {
		e.engineID = res.EngineID
	}

	if res.Text == "" {
		// 引擎没识别出文字（静音误判）：不落空行，但仍推进进度，
		// 否则下次续跑会一直重试这句、永远卡在同一位置。
		return e.advanceProgress(u.EndMs, false)
	}

	row := segmentFromUtterance(e.row.ID, u, engineOf(res, e.runner.engine), res.Text, res.Confidence)
	if err := assertSegmentOrder(e.prev, row); err != nil {
		return apierr.From(err, apierr.CodeInvalidArgument)
	}
	saved, err := e.runner.store.UpsertSegment(e.writeCtx, row)
	if err != nil {
		return apierr.From(err, apierr.CodeDBWrite)
	}
	e.prev = &saved
	e.written++
	e.chars += int64(utf8.RuneCountInString(res.Text))
	return e.advanceProgress(u.EndMs, true)
}

// advanceProgress 推进并持久化进度（每句一次，崩溃后不丢已完成的量）。
func (e *execution) advanceProgress(endMs int64, countSegment bool) error {
	next := advance(e.progress, endMs)
	if next == e.progress && !countSegment {
		return nil
	}
	e.progress = next
	update := TranscriptionUpdate{SessionID: e.row.ID, ProgressMs: &next}
	if countSegment {
		n := e.row.SegmentCount + e.written
		update.SegmentCount = &n
	}
	if err := e.runner.store.UpdateTranscription(e.writeCtx, update); err != nil {
		return apierr.From(err, apierr.CodeDBWrite)
	}
	e.row.TranscribeProgressMs = next
	// 每句推进后推一次状态快照：前端进度条据此走动（EV-16）。
	// 频率是「一句一次」，不含正文，只有位置与句数。
	st := newState(e.row, e.trigger, e.row.SegmentCount)
	st.Status = StatusRunning
	st.StartedAt = e.startedAt
	e.runner.emit(st)
	return nil
}

// finish 收尾：回填计数、状态置 done。
func (e *execution) finish(ctx context.Context, session SessionRow, audioMs int64, src *wavSource) (Result, error) {
	page, err := e.runner.store.ListSegments(e.writeCtx, SegmentFilter{SessionID: session.ID, Limit: 1})
	if err != nil {
		return e.abort(ctx, session, apierr.From(err, apierr.CodeDBWrite))
	}
	engineID := e.engineID
	if engineID == "" {
		engineID = e.runner.engine.ID()
	}

	doneAt := e.runner.opts.Now().UnixMilli()
	progress := audioMs
	segCount := page.Total
	chars := e.chars + session.CharCount
	update := TranscriptionUpdate{
		SessionID:     session.ID,
		Status:        StatusDone,
		ProgressMs:    &progress,
		SegmentCount:  &segCount,
		CharCount:     &chars,
		TranscribedAt: &doneAt,
	}
	if engineID != "" {
		update.EngineID = &engineID
	}
	if err := e.runner.store.UpdateTranscription(e.writeCtx, update); err != nil {
		return Result{}, apierr.From(err, apierr.CodeDBWrite)
	}

	row := session
	row.TranscriptionStatus = StatusDone
	row.TranscribeProgressMs = progress
	row.SegmentCount = segCount
	row.CharCount = chars
	row.TranscribedAt = doneAt
	if engineID != "" {
		row.TranscribeEngineID = engineID
	}
	st := newState(row, e.trigger, segCount)
	st.StartedAt = e.startedAt
	st.FinishedAt = doneAt
	e.runner.emit(st)
	e.runner.clearLive(session.ID)

	logx.For("transcribe").Info("转写完成", "sessionId", session.ID,
		"audioMs", audioMs, "written", e.written, "skipped", e.skipped,
		"engineCalls", e.engineCalls, "engineId", engineID,
		"audio", describeWAV(src.Info()))
	return Result{Outcome: OutcomeDone, State: st, Written: e.written, Skipped: e.skipped,
		EngineCalls: e.engineCalls}, nil
}

// abort 处理非完成态的退出：取消 / 让位 / 失败。
func (e *execution) abort(ctx context.Context, session SessionRow, cause error) (Result, error) {
	eErr := apierr.From(cause, apierr.CodeInvalidArgument)

	switch {
	case errors.Is(eErr, errCanceled):
		return e.yieldOrCancel(ctx, session, OutcomeCanceled, cancelState(), eErr)
	case errors.Is(eErr, errYielded):
		// 被前台占用抢占：让位但**保留进度**，回 queued 等下次空闲。
		return e.yieldOrCancel(ctx, session, OutcomeYielded, StatusQueued, eErr)
	default:
		return e.runner.fail(ctx, session, TaskRequest{SessionID: session.ID, Trigger: e.trigger},
			e.startedAt, eErr)
	}
}

// yieldOrCancel 落库取消 / 让位后的状态并返回结果。
//
// 注意：这里用 background-ish 的 context（ctx 可能已被取消），
// 否则「取消」这个动作本身的落库会随 ctx 一起失败，状态停在 running。
func (e *execution) yieldOrCancel(_ context.Context, session SessionRow,
	outcome, status string, cause *apierr.Err) (Result, error) {

	writeCtx, cancel := context.WithTimeout(e.writeCtx, writeTimeout)
	defer cancel()

	progress := e.progress
	update := TranscriptionUpdate{SessionID: session.ID, Status: status}
	if progress > 0 {
		update.ProgressMs = &progress
	}
	if err := e.runner.store.UpdateTranscription(writeCtx, update); err != nil {
		return Result{}, apierr.From(err, apierr.CodeDBWrite)
	}

	row := session
	row.TranscriptionStatus = status
	row.TranscribeProgressMs = progress
	st := newState(row, e.trigger, session.SegmentCount)
	st.StartedAt = e.startedAt
	if outcome == OutcomeYielded {
		// 让位不是错误：错误码留给真正的失败（前端据此决定显示「稍后继续」还是「重试」）。
		st.ErrorCode = ""
	} else {
		st.ErrorCode = cause.Code
	}
	e.runner.emit(st)
	if outcome != OutcomeYielded {
		e.runner.clearLive(session.ID)
	}

	logx.For("transcribe").Info("转写中断", "sessionId", session.ID,
		"outcome", outcome, "status", status, "progressMs", progress,
		"written", e.written, "skipped", e.skipped)
	return Result{Outcome: outcome, State: st, Err: cause,
		Written: e.written, Skipped: e.skipped, EngineCalls: e.engineCalls}, nil
}

// writeTimeout 是取消/让位时落库的超时（不依赖已取消的 ctx）。
const writeTimeout = 3 * time.Second

// engineOf 取片段应记录的引擎 ID：优先用本次结果给的（降级前后可能不同）。
func engineOf(res asr.FinalResult, eng Recognizer) string {
	if res.EngineID != "" {
		return res.EngineID
	}
	return eng.ID()
}
