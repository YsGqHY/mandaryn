package transcribe

import (
	"context"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/asr"
	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/utils/logx"
)

// 边录边转（FR-03-10，**默认关**）。
//
// 与「延后转写」的关系（很重要，别混）：
//   - 边录边转是**旁路**：录制仍然只做采集与落盘，推理在独立 goroutine 里跑；
//   - 它是「流式一遍 + 句末定稿」（ARCH §2.3 的两遍法：第二遍离线校正由
//     `Refine` 可选注入，预算档位 `L1` 及以上会关掉它 —— 见 perf.RefinementAllowed）；
//   - 只有**定稿**（`finalized=true`）才落 `segments`；partial 只经回调上报，
//     **不落库**（否则定稿行会被自己的半成品覆盖，回放里出现「同一句两行」）。
//
// 三条硬约束：
//  1. 输入队列有界，满则丢**最旧**并计数 —— 绝不阻塞采集与落盘；
//  2. 推理报错只记日志 + 回调上报，**不影响录制**（不 panic、不回灌错误）；
//  3. 停止时未定稿的尾部「够长就定稿、否则丢弃」，goroutine 必须全部退出。

const (
	// DefaultLiveQueueMs 边录边转输入队列容量（毫秒音频），默认 8 秒。
	//
	// 8 秒的依据：流式引擎正常比实时快一个数量级，队列几乎不会积压；
	// 一旦积压到 8 秒，说明推理已经跟不上，这时候「丢掉最旧的样本」比
	// 「让队列无限长大」更有价值 —— 前者只是丢字，后者会把内存与延迟一起拖垮。
	DefaultLiveQueueMs = 8000

	// liveWriteTimeout 单次落库写的超时（定稿行 / 进度）。
	//
	// 用独立超时而不是外部 ctx：录制停止不能因为一次慢写把收尾拖住，
	// 但「写坏了」也只是这一句没落库，下一次重转还能补。
	liveWriteTimeout = 3 * time.Second
)

// LiveState 是边录边转的运行期状态（供装配层记日志 / 非阻塞提示）。
//
// 刻意**不新增事件**：EV-04 / EV-05 已经够用，推理失败属于「功能性旁路掉线」，
// 不影响任何主流程，用回调 + 日志表达即可（events.go 是冻结的跨端契约）。
type LiveState struct {
	// SessionID 所属会话。
	SessionID string
	// Active 旁路当前是否在工作（false = 已停用或已结束）。
	Active bool
	// ErrorCode 停用原因错误码（空 = 正常）。
	ErrorCode string
	// Note 中文短说明。
	Note string
	// Segments 已定稿句数。
	Segments int
	// DroppedMs 因队列溢出丢弃的音频时长（毫秒，仅元数据）。
	DroppedMs int64
}

// LiveStats 是旁路的统计快照（测试与诊断用；只含元数据，不含音频与正文）。
type LiveStats struct {
	PushedSamples  int64
	DroppedSamples int64
	Partials       int64
	Segments       int64
	Refines        int64
	EngineErrors   int64
	LastErrorCode  string
	Chars          int64
	Closed         bool
	Done           bool
}

// LiveOptions 是 LiveRunner 的构造参数。
type LiveOptions struct {
	// Store 片段与转写轴的落库端口（必填）。
	Store Store
	// Engine 流式引擎（必填；没有流式能力就不要构造 LiveRunner）。
	Engine asr.StreamingEngine
	// SessionID 目标会话（必填）。
	SessionID string
	// Vad 分句参数：**必须与延后转写用同一套**，否则 `(session_id, seq)`
	// 编号会与整段重转的编号错位（幂等覆盖要求编号来自确定性分句）。
	Vad VadConfig
	// QueueMs 输入队列容量（毫秒）；<=0 用 DefaultLiveQueueMs。
	QueueMs int
	// Refine 第二遍离线校正（ARCH §2.3）：拿到一句话的音频，用离线模型重算。
	//
	// nil = 不做第二遍（只有流式结果），这是 `L1` 档的形态。
	Refine func(seg asr.AudioSegment) (asr.FinalResult, error)
	// RefineAllowed 预算闸门：返回 false 时跳过第二遍校准（NFR-08 / ARCH §2.3）。
	//
	// nil 视为允许（未接预算域时按 L0 处理）。
	RefineAllowed func() bool

	// OnPartial partial 上报（EV-04，不落库）；可为 nil。
	OnPartial func(contract.Segment)
	// OnSegment 定稿上报（EV-05）；可为 nil。
	OnSegment func(contract.Segment)
	// OnState 运行期状态上报（推理失败 / 结束）；可为 nil。
	OnState func(LiveState)
	// Now 时间源（测试注入）。
	Now func() time.Time
}

// LiveRunner 是边录边转的接收端：实现 `recording.LiveSink`（结构化满足）。
//
// 并发约定：
//   - `Push` 由**落盘 goroutine** 调用，只持锁拷样本，绝不等推理；
//   - `Close` 任意 goroutine 调用一次即可（幂等、非阻塞）；
//   - 推理与落库全在构造时起的单个 goroutine 里（顺序执行，天然单飞行）。
type LiveRunner struct {
	store      Store
	engine     asr.StreamingEngine
	sessionID  string
	vad        VadConfig
	sampleRate int
	capSamples int

	refine        func(asr.AudioSegment) (asr.FinalResult, error)
	refineAllowed func() bool
	onPartial     func(contract.Segment)
	onSegment     func(contract.Segment)
	onState       func(LiveState)
	now           func() time.Time

	// stopCh 由 Close 关闭，通知消费循环「不再有新数据」。
	stopCh   chan struct{}
	stopOnce sync.Once
	// done 在消费循环退出后关闭（测试据此断言 goroutine 全退）。
	done chan struct{}
	// notify 唤醒消费循环（容量 1，非阻塞投递）。
	notify chan struct{}

	mu sync.Mutex
	// buf 是待推理的样本 FIFO；baseFrame 是 buf[0] 在会话时间轴上的帧偏移。
	buf       []float32
	baseFrame int64
	closed    bool
	stats     LiveStats
}

// NewLiveRunner 构造并启动旁路。
func NewLiveRunner(opts LiveOptions) (*LiveRunner, error) {
	if opts.Store == nil {
		return nil, apierr.New(apierr.CodeDBWrite).WithDetail("边录边转未拿到 Store")
	}
	if opts.Engine == nil {
		return nil, apierr.New(apierr.CodeLocalUnavailable).
			WithAction("download-model").
			WithDetail("边录边转没有流式引擎")
	}
	if strings.TrimSpace(opts.SessionID) == "" {
		return nil, apierr.New(apierr.CodeInvalidArgument).WithDetail("边录边转缺少 sessionId")
	}
	cfg := opts.Vad.Normalize()
	queueMs := opts.QueueMs
	if queueMs <= 0 {
		queueMs = DefaultLiveQueueMs
	}
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	l := &LiveRunner{
		store:         opts.Store,
		engine:        opts.Engine,
		sessionID:     opts.SessionID,
		vad:           cfg,
		sampleRate:    cfg.SampleRate,
		capSamples:    cfg.SampleRate * queueMs / 1000,
		refine:        opts.Refine,
		refineAllowed: opts.RefineAllowed,
		onPartial:     opts.OnPartial,
		onSegment:     opts.OnSegment,
		onState:       opts.OnState,
		now:           nowFn,
		stopCh:        make(chan struct{}),
		done:          make(chan struct{}),
		notify:        make(chan struct{}, 1),
	}
	if l.capSamples <= 0 {
		l.capSamples = cfg.SampleRate
	}
	go l.loop()
	return l, nil
}

// Push 投递一段 16kHz 单声道音频（**非阻塞**，队列满则丢最旧）。
//
// atFrame 是该批样本在会话时间轴上的起始帧偏移（= 已落盘帧数）。
// samples 只在调用期间有效：这里**立刻拷贝**进队列，调用方可以复用缓冲。
func (l *LiveRunner) Push(samples []float32, atFrame int64) {
	if len(samples) == 0 {
		return
	}
	l.mu.Lock()
	if l.closed {
		// 已停止：不再接收，但要计数（诊断能看出「停止后还漏进来多少」）。
		l.stats.DroppedSamples += int64(len(samples))
		l.mu.Unlock()
		return
	}
	if len(l.buf) == 0 {
		// 队列空了：以本批的真实帧偏移为准（暂停 / 丢帧后自动重新对齐）。
		l.baseFrame = atFrame
	}
	l.buf = append(l.buf, samples...)
	if over := len(l.buf) - l.capSamples; over > 0 {
		// 丢最旧：保留最新的一段。用 copy 而不是重切切片，
		// 否则底层数组会随 append 无限长大。
		copy(l.buf, l.buf[over:])
		l.buf = l.buf[:len(l.buf)-over]
		l.baseFrame += int64(over)
		l.stats.DroppedSamples += int64(over)
	}
	l.stats.PushedSamples += int64(len(samples))
	l.mu.Unlock()
	l.signal()
}

// Close 请求退出：**非阻塞**、幂等。
//
// 尾部处理由消费 goroutine 完成（够长就定稿、否则丢弃），因此录制收尾
// 不会被推理拖住。要等它真的结束用 `Wait`。
func (l *LiveRunner) Close() {
	l.mu.Lock()
	l.closed = true
	l.stats.Closed = true
	l.mu.Unlock()
	l.stopOnce.Do(func() { close(l.stopCh) })
}

// Wait 等待消费 goroutine 退出（ctx 超时返回 false）。
func (l *LiveRunner) Wait(ctx context.Context) bool {
	select {
	case <-l.done:
		return true
	case <-ctx.Done():
		return false
	}
}

// CharCount 已定稿的字符数（FR-03-9）。
func (l *LiveRunner) CharCount() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.stats.Chars
}

// Stats 返回统计快照。
func (l *LiveRunner) Stats() LiveStats {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.stats
}

// SessionID 所属会话。
func (l *LiveRunner) SessionID() string { return l.sessionID }

// —— 消费循环 ——

// liveChunk 是一次要推理的样本块。
type liveChunk struct {
	samples    []float32
	startFrame int64
}

// startMs 该块在会话时间轴上的起点（帧计数换算，不用墙钟）。
func (c liveChunk) startMs(rate int) int64 { return framesToMs(c.startFrame, rate) }

// endMs 该块在会话时间轴上的终点。
func (c liveChunk) endMs(rate int) int64 {
	return framesToMs(c.startFrame+int64(len(c.samples)), rate)
}

func (l *LiveRunner) loop() {
	defer close(l.done)

	splitter := NewUtteranceSplitter(l.vad)
	st := &liveWork{sessionID: l.sessionID}

	for {
		chunk, ok := l.take()
		if ok {
			l.consume(st, splitter, chunk)
			continue
		}
		select {
		case <-l.notify:
		case <-l.stopCh:
			// 停止：把队列里剩下的都吃掉，再收尾（不再等新数据）。
			for {
				c, ok := l.take()
				if !ok {
					break
				}
				l.consume(st, splitter, c)
			}
			l.finish(st, splitter)
			return
		}
	}
}

// take 取出一块待推理样本（最多 200ms 一档）。
//
// 一次最多取 200ms：既能让流式引擎以接近实时帧的粒度出字（FFD 指标），
// 又不会因为每 10ms 一次调用把调度开销放大。
func (l *LiveRunner) take() (liveChunk, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.buf) == 0 {
		return liveChunk{}, false
	}
	max := l.sampleRate / 5
	if max <= 0 {
		max = l.sampleRate
	}
	n := len(l.buf)
	if n > max {
		n = max
	}
	samples := make([]float32, n)
	copy(samples, l.buf[:n])
	l.buf = l.buf[n:]
	startFrame := l.baseFrame
	l.baseFrame += int64(n)
	return liveChunk{samples: samples, startFrame: startFrame}, true
}

// consume 推理一块音频并处理分句。
func (l *LiveRunner) consume(w *liveWork, sp *UtteranceSplitter, chunk liveChunk) {
	if w.failed {
		// 已停用：不再喂引擎（省 CPU），但继续丢样本，避免队列假性积压。
		return
	}
	partial, err := l.engine.Accept(chunk.samples)
	if err != nil {
		l.fail(w, err)
		return
	}
	if text := strings.TrimSpace(partial); text != "" {
		w.text += text
		l.bumpPartial()
		// partial **不落库**：只上报，前端覆盖显示（FR-03-10「partial 可被覆盖重写」）。
		//
		// 时间戳是**估算**的（句首取上一句结束点），定稿行的时间戳才是精确的
		// VAD 端点 —— 前端只把 partial 当「正在说的话」，不用于定位。
		seg := contract.Segment{
			SessionID: l.sessionID,
			Seq:       w.nextSeq,
			StartMs:   w.lastEndMs,
			EndMs:     chunk.endMs(l.sampleRate),
			Text:      w.text,
			EngineID:  l.engine.ID(),
			Finalized: false,
		}
		if l.onPartial != nil {
			l.onPartial(seg)
		}
	}
	for _, u := range sp.Push(chunk.samples) {
		l.finalize(w, u)
	}
}

// finalize 把一句定稿写库（`finalized=true`）。
func (l *LiveRunner) finalize(w *liveWork, u Utterance) {
	text := strings.TrimSpace(w.text)
	w.text = ""
	engineID := l.engine.ID()
	confidence := float32(0)

	// 第二遍离线校正（ARCH §2.3）：只有预算允许时才做。
	if l.refine != nil && (l.refineAllowed == nil || l.refineAllowed()) {
		res, err := l.refine(asr.AudioSegment{
			Samples:    u.Samples,
			SampleRate: u.SampleRate,
			SessionID:  l.sessionID,
			Seq:        u.Index,
			StartMs:    u.StartMs,
			EndMs:      u.EndMs,
		})
		if err != nil {
			// 校正失败不影响这一句：保留流式结果（宁可少一分准确，也不要丢字）。
			logx.For("transcribe").Warn("边录边转二遍校正失败，保留流式结果",
				"session", l.sessionID, "seq", u.Index,
				"code", apierr.From(err, apierr.CodeEngineLoad).Code)
		} else {
			if t := strings.TrimSpace(res.Text); t != "" {
				text = t
			}
			if res.EngineID != "" {
				engineID = res.EngineID
			}
			confidence = res.Confidence
			l.bumpRefine()
		}
	}

	if text == "" {
		// 引擎没识别出文字（静音误判）：不落空行，但 seq 仍然推进
		// （seq 来自分句序号，不能因为空文本回退，否则会撞号覆盖别人）。
		w.nextSeq = u.Index + 1
		w.lastEndMs = u.EndMs
		l.bumpProgress(u.EndMs)
		return
	}

	row := SegmentRow{
		SessionID:  l.sessionID,
		Seq:        u.Index,
		StartMs:    u.StartMs,
		EndMs:      u.EndMs,
		Text:       text,
		EngineID:   engineID,
		Confidence: confidence,
		IsFinal:    true,
	}
	if err := assertSegmentOrder(w.prev, row); err != nil {
		logx.For("transcribe").Warn("边录边转片段乱序，丢弃该句",
			"session", l.sessionID, "seq", u.Index)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), liveWriteTimeout)
	defer cancel()
	saved, err := l.store.UpsertSegment(ctx, row)
	if err != nil {
		// 落库失败：不 panic、不改音频，只记错误码（下一次重转可补）。
		l.noteError(w, apierr.From(err, apierr.CodeDBWrite))
		return
	}
	w.prev = &saved
	w.nextSeq = u.Index + 1
	w.lastEndMs = u.EndMs
	chars := int64(utf8.RuneCountInString(text))
	l.bumpSegment(chars)

	if l.onSegment != nil {
		l.onSegment(contract.Segment{
			ID:         saved.ID,
			SessionID:  l.sessionID,
			Seq:        saved.Seq,
			StartMs:    saved.StartMs,
			EndMs:      saved.EndMs,
			Text:       saved.Text,
			EngineID:   saved.EngineID,
			Confidence: saved.Confidence,
			Finalized:  true,
		})
	}
	l.bumpProgress(u.EndMs)
}

// finish 收尾：定稿尾部 + 落 `transcription_status`。
func (l *LiveRunner) finish(w *liveWork, sp *UtteranceSplitter) {
	// 未定稿的尾部：Flush 会按最短语音时长过滤 —— 「够长就定稿，否则丢弃」。
	if !w.failed {
		for _, u := range sp.Flush() {
			l.finalize(w, u)
		}
	}

	stats := l.Stats()
	update := TranscriptionUpdate{SessionID: l.sessionID}
	if w.failed {
		// 推理挂了：状态置 failed（带错误码，用户可手动重转），已落句子保留。
		update.Status = StatusFailed
	} else {
		// ARCH §6.4：「边录边转正常结束时也置 done」。
		update.Status = StatusDone
	}
	progress := w.lastEndMs
	// 句数按**库里实际条数**回填（边录边转可能只转了一部分，或用同一 seq 覆盖过）。
	ctx, cancel := context.WithTimeout(context.Background(), liveWriteTimeout)
	defer cancel()
	segCount := int(stats.Segments)
	if page, err := l.store.ListSegments(ctx, SegmentFilter{
		SessionID: l.sessionID, Limit: 1,
	}); err == nil {
		segCount = page.Total
	}
	chars := stats.Chars
	doneAt := l.now().UnixMilli()
	update.ProgressMs = &progress
	update.SegmentCount = &segCount
	update.CharCount = &chars
	update.TranscribedAt = &doneAt
	if id := l.engine.ID(); id != "" {
		update.EngineID = &id
	}

	if err := l.store.UpdateTranscription(ctx, update); err != nil {
		logx.For("transcribe").Warn("边录边转收尾写状态失败",
			"session", l.sessionID, "code", apierr.From(err, apierr.CodeDBWrite).Code)
	}

	l.mu.Lock()
	l.stats.Done = true
	l.mu.Unlock()

	logx.For("transcribe").Info("边录边转已结束",
		"session", l.sessionID,
		"segments", segCount, "chars", chars,
		"partials", stats.Partials, "engineErrors", stats.EngineErrors,
		"droppedMs", stats.DroppedSamples*1000/int64(l.sampleRate),
		"failed", w.failed)
	l.emitState(LiveState{
		SessionID: l.sessionID,
		Active:    false,
		ErrorCode: stats.LastErrorCode,
		Segments:  segCount,
		DroppedMs: stats.DroppedSamples * 1000 / int64(l.sampleRate),
	})
}

// fail 处理推理错误：只记日志 + 上报，**不回灌录制**。
func (l *LiveRunner) fail(w *liveWork, cause error) {
	e := apierr.From(cause, apierr.CodeEngineLoad)
	w.failed = true
	l.mu.Lock()
	l.stats.EngineErrors++
	l.stats.LastErrorCode = e.Code
	segs := int(l.stats.Segments)
	dropped := l.stats.DroppedSamples * 1000 / int64(l.sampleRate)
	l.mu.Unlock()

	logx.For("transcribe").Warn("边录边转推理失败，已停用（录制不受影响）",
		"session", l.sessionID, "code", e.Code, "segments", segs)
	l.emitState(LiveState{
		SessionID: l.sessionID,
		Active:    false,
		ErrorCode: e.Code,
		Note:      "边录边转已停用，录制继续",
		Segments:  segs,
		DroppedMs: dropped,
	})
}

// noteError 记录一次落库失败（不改变 failed 状态：这不是推理能力问题）。
func (l *LiveRunner) noteError(_ *liveWork, e *apierr.Err) {
	l.mu.Lock()
	l.stats.EngineErrors++
	l.stats.LastErrorCode = e.Code
	l.mu.Unlock()
	logx.For("transcribe").Warn("边录边转片段落库失败", "session", l.sessionID, "code", e.Code)
}

// —— 统计与工具 ——

func (l *LiveRunner) bumpPartial() {
	l.mu.Lock()
	l.stats.Partials++
	l.mu.Unlock()
}

func (l *LiveRunner) bumpRefine() {
	l.mu.Lock()
	l.stats.Refines++
	l.mu.Unlock()
}

func (l *LiveRunner) bumpSegment(chars int64) {
	l.mu.Lock()
	l.stats.Segments++
	l.stats.Chars += chars
	l.mu.Unlock()
}

// bumpProgress 推进会话的转写进度（每句一次，崩溃后不丢已完成的量）。
//
// 只写转写轴，**绝不**碰 `sessions.state`（ARCH §6.4）。
func (l *LiveRunner) bumpProgress(endMs int64) {
	ctx, cancel := context.WithTimeout(context.Background(), liveWriteTimeout)
	defer cancel()
	progress := endMs
	if err := l.store.UpdateTranscription(ctx, TranscriptionUpdate{
		SessionID:  l.sessionID,
		ProgressMs: &progress,
	}); err != nil {
		logx.For("transcribe").Warn("边录边转进度落库失败",
			"session", l.sessionID, "code", apierr.From(err, apierr.CodeDBWrite).Code)
	}
}

func (l *LiveRunner) emitState(st LiveState) {
	if l.onState != nil {
		l.onState(st)
	}
}

// signal 唤醒消费循环（非阻塞）。
func (l *LiveRunner) signal() {
	select {
	case l.notify <- struct{}{}:
	default:
	}
}

// liveWork 是一次会话内的可变状态（只在消费 goroutine 里读写）。
type liveWork struct {
	sessionID string
	text      string
	nextSeq   int
	lastEndMs int64
	prev      *SegmentRow
	failed    bool
}

// framesToMs 帧 → 毫秒（与 recording.FramesToMs 同一口径：整数换算，不用墙钟）。
func framesToMs(frames int64, rate int) int64 {
	if frames <= 0 || rate <= 0 {
		return 0
	}
	return frames * 1000 / int64(rate)
}

// ShouldAutoTranscribe 报告某个会话是否还应该被「空闲自动转写」排队（FR-03-4）。
//
// 开启边录边转的会话在录制结束时已经产出定稿片段（`live_transcribed=true`），
// 再自动整段重跑等于把同一段音频推理两遍 —— 这正是「录制后不再自动整段转写」
// 要挡掉的情况。用户仍可**手动**重转（幂等覆盖，`CanStart(done)` 为真）。
func ShouldAutoTranscribe(row SessionRow) bool {
	if row.LiveTranscribed {
		return false
	}
	return CanStart(row.TranscriptionStatus)
}
