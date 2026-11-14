package transcribe

import (
	"context"
	"sync"
	"time"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/utils/logx"
)

// 触发时机与调度（FR-03-4「触发时机」/ NFR-08 预算让位）。
//
// 规则：
//   - 触发来源两种：`manual`（用户点「转写」）与 `idle`（整机空闲自动排队）；
//   - **并发上限 1**：任何时刻只有一个任务在跑（多个任务并行会互相抢 CPU，
//     与「空闲时才转」的产品前提直接冲突）；
//   - 空闲判定由**注入的 `idle func() bool`** 提供；真实 CPU 采样归 NFR-08（S8），
//     本包只消费判定结果，因此可以在单测里精确模拟「前台忙 / 空闲」；
//   - 被前台占用抢占时**让位并保留进度**：状态回 `queued`，已转句子不丢，
//     下次空闲从断点续跑。
const (
	// DefaultPollInterval 空闲等待轮询间隔。
	DefaultPollInterval = 3 * time.Second
)

// SchedulerOptions 是调度器参数。
type SchedulerOptions struct {
	// Poll 空闲等待时的轮询间隔（测试可调小）。
	Poll time.Duration
}

// Scheduler 是转写任务队列。
//
// 队列只存在于内存：重启后靠 `transcription_status=queued` 的持久状态 + 启动扫描
// 重新入队（扫描属装配层 / S9 的启动自检，本包不主动扫描，避免启动即跑推理）。
type Scheduler struct {
	runner *Runner
	opts   SchedulerOptions

	mu        sync.Mutex
	queue     []TaskRequest
	running   bool
	runningID string
	cancel    context.CancelFunc
	closed    bool

	wake chan struct{}
	stop chan struct{}
	loop sync.WaitGroup
}

// NewScheduler 构造调度器并启动后台循环。
func NewScheduler(runner *Runner, opts SchedulerOptions) (*Scheduler, error) {
	if runner == nil {
		return nil, apierr.New(apierr.CodeEngineNotReady).WithDetail("调度器未拿到 Runner")
	}
	if opts.Poll <= 0 {
		opts.Poll = DefaultPollInterval
	}
	s := &Scheduler{
		runner: runner,
		opts:   opts,
		wake:   make(chan struct{}, 1),
		stop:   make(chan struct{}),
	}
	s.loop.Add(1)
	go s.runLoop()
	return s, nil
}

// Enqueue 把任务加入队列并把持久状态置 `queued`。
//
// 可能错误：
//   - ERR_NOT_FOUND：会话不存在；
//   - ERR_STATE_FORBIDDEN：会话已有排队 / 运行中的任务（不区分「内存里」还是
//     「库里写着 queued」，两者都算「同一会话两个任务」）；
//   - ERR_DB_WRITE：状态落库失败。
//
// 顺序很重要：**先**在内存里占位去重，**再**落库。反过来的话，落库成功但去重失败
// 会让状态卡在 `queued` 而队列里没有任务，前端永远等不到结果。
func (s *Scheduler) Enqueue(ctx context.Context, req TaskRequest) (contract.TranscriptionState, error) {
	if req.SessionID == "" {
		return contract.TranscriptionState{}, apierr.New(apierr.CodeInvalidArgument).
			WithDetail("sessionId 为空")
	}
	if req.Trigger == "" {
		req.Trigger = contract.TriggerManual
	}

	session, err := s.runner.store.LoadSession(ctx, req.SessionID)
	if err != nil {
		return contract.TranscriptionState{}, apierr.From(err, apierr.CodeNotFound)
	}
	if !CanStart(session.TranscriptionStatus) {
		return newState(session, req.Trigger, session.SegmentCount), errBusy(req.SessionID)
	}
	// 会话已 done 时点「转写」= 整段重转。
	if ShouldRestart(session.TranscriptionStatus) {
		req.Restart = true
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return newState(session, req.Trigger, session.SegmentCount),
			apierr.New(apierr.CodeStateForbidden).WithDetail("调度器已关闭")
	}
	if s.running && s.runningID == req.SessionID || s.queuedLocked(req.SessionID) >= 0 {
		s.mu.Unlock()
		return newState(session, req.Trigger, session.SegmentCount), errBusy(req.SessionID)
	}
	s.queue = append(s.queue, req)
	s.mu.Unlock()

	// 落库 queued（只动转写轴）。
	if err := s.runner.store.UpdateTranscription(ctx, TranscriptionUpdate{
		SessionID: req.SessionID,
		Status:    StatusQueued,
	}); err != nil {
		s.drop(req.SessionID)
		return contract.TranscriptionState{}, apierr.From(err, apierr.CodeDBWrite)
	}

	row := session
	row.TranscriptionStatus = StatusQueued
	st := newState(row, req.Trigger, session.SegmentCount)
	s.runner.emit(st)
	s.signal()
	logx.For("transcribe").Info("转写已入队", "sessionId", req.SessionID,
		"trigger", req.Trigger, "restart", req.Restart)
	return st, nil
}

// Cancel 取消排队或运行中的任务。
//
// 返回 false 表示该会话没有可取消的任务（调用方映射为 ERR_STATE_FORBIDDEN）。
// 取消后：已转句子保留，持久状态回 `none`（API-4.6）。
func (s *Scheduler) Cancel(ctx context.Context, sessionID string) (bool, error) {
	s.mu.Lock()
	idx := s.queuedLocked(sessionID)
	isRunning := s.running && s.runningID == sessionID
	cancel := s.cancel
	if idx >= 0 {
		s.queue = append(s.queue[:idx], s.queue[idx+1:]...)
	}
	s.mu.Unlock()

	switch {
	case idx >= 0:
		// 排队中：直接从队列移除并落库 none。
		if err := s.runner.store.UpdateTranscription(ctx, TranscriptionUpdate{
			SessionID: sessionID,
			Status:    cancelState(),
		}); err != nil {
			return false, apierr.From(err, apierr.CodeDBWrite)
		}
		session, err := s.runner.store.LoadSession(ctx, sessionID)
		if err == nil {
			s.runner.emit(newState(session, "", session.SegmentCount))
		}
		return true, nil
	case isRunning:
		// 运行中：让执行侧自己收尾（落库 none + 保留进度），避免两处同时写状态。
		if cancel != nil {
			cancel()
		}
		return true, nil
	default:
		// 内存里没有该会话，但库里仍是 queued / running（典型：进程重启后残留，
		// 或崩溃时状态没来得及收尾）。必须允许取消，否则该会话会卡在
		// 「不能发起（状态是 busy）、也不能取消（内存里没有任务）」的死角。
		session, err := s.runner.store.LoadSession(ctx, sessionID)
		if err != nil {
			return false, apierr.From(err, apierr.CodeNotFound)
		}
		if !IsBusy(session.TranscriptionStatus) {
			return false, nil
		}
		if err := s.runner.store.UpdateTranscription(ctx, TranscriptionUpdate{
			SessionID: sessionID,
			Status:    cancelState(),
		}); err != nil {
			return false, apierr.From(err, apierr.CodeDBWrite)
		}
		row := session
		row.TranscriptionStatus = cancelState()
		s.runner.emit(newState(row, "", session.SegmentCount))
		logx.For("transcribe").Info("清理残留转写状态", "sessionId", sessionID,
			"prevStatus", session.TranscriptionStatus)
		return true, nil
	}
}

// Settled 报告调度器是否「暂时无事可做」：没在跑，且（队列空 或 当前不允许跑）。
//
// 这是给测试与诊断用的观测点：让位测试要能确定「任务已回到 queued 且不会立刻重启」，
// 否则只能靠 sleep，测试会 flaky。
func (s *Scheduler) Settled() bool {
	s.mu.Lock()
	running := s.running
	queue := append([]TaskRequest(nil), s.queue...)
	s.mu.Unlock()
	if running {
		return false
	}
	if len(queue) == 0 {
		return true
	}
	// 只要有任何一个排队任务当前允许执行，就不算 settled（按来源逐个判定）。
	for _, cand := range queue {
		if s.runner.idleFor(cand.Trigger) {
			return false
		}
	}
	return true
}

// WaitSettled 等待调度器进入 Settled 状态（ctx 超时则返回错误）。
func (s *Scheduler) WaitSettled(ctx context.Context) error {
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		if s.Settled() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

// WaitStopped 等待某会话彻底停下（既不在跑、也不在队列里）。
//
// 取消是异步的：`Cancel` 只是把 context 取消掉，执行侧要跑到下一个检查点才收尾落库。
// L3 的 CancelTranscription 用它把「取消返回」与「状态已是 none」对齐，
// 否则前端点完取消再刷新，会看到一个短暂的 running/queued 残影。
func (s *Scheduler) WaitStopped(ctx context.Context, sessionID string) error {
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		s.mu.Lock()
		running := s.running && s.runningID == sessionID
		queued := s.queuedLocked(sessionID) >= 0
		s.mu.Unlock()
		if !running && !queued {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

// WaitIdle 等待队列彻底跑空（不依赖 idle 判定）。
func (s *Scheduler) WaitIdle(ctx context.Context) error {
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		s.mu.Lock()
		done := !s.running && len(s.queue) == 0
		s.mu.Unlock()
		if done {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

// Queued 返回当前队列快照（诊断 / 测试）。
func (s *Scheduler) Queued() []TaskRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]TaskRequest, len(s.queue))
	copy(out, s.queue)
	return out
}

// RunningSession 返回正在执行的会话 ID（无则空）。
func (s *Scheduler) RunningSession() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running {
		return ""
	}
	return s.runningID
}

// Close 停止调度器：取消在跑的任务并等待循环退出。
func (s *Scheduler) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	cancel := s.cancel
	s.mu.Unlock()

	close(s.stop)
	if cancel != nil {
		cancel()
	}
	s.loop.Wait()
}

// —— 内部 ——

// queuedLocked 返回该会话在队列中的下标（无则 -1）。调用方需持锁。
func (s *Scheduler) queuedLocked(sessionID string) int {
	for i, r := range s.queue {
		if r.SessionID == sessionID {
			return i
		}
	}
	return -1
}

// drop 从队列里移除某会话（入队后就绪阶段失败时回滚）。
func (s *Scheduler) drop(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if idx := s.queuedLocked(sessionID); idx >= 0 {
		s.queue = append(s.queue[:idx], s.queue[idx+1:]...)
	}
}

// signal 唤醒调度循环（非阻塞）。
func (s *Scheduler) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// runLoop 是唯一的执行循环：**同步**跑任务，并发上限 1 由此天然成立。
func (s *Scheduler) runLoop() {
	defer s.loop.Done()
	for {
		select {
		case <-s.stop:
			return
		default:
		}
		req, ctx, ok := s.pick()
		if !ok {
			select {
			case <-s.stop:
				return
			case <-s.wake:
			case <-time.After(s.opts.Poll):
			}
			continue
		}
		s.execute(ctx, req)
	}
}

// pick 取下一个可执行任务（需同时满足：没在跑、队列非空、当前空闲）。
//
// 任务的 context 与 cancel **在锁内**一起创建/登记：否则「已置 running 但还没
// 登记 cancel」的空窗里 Cancel 会退化成一个空操作（点了取消，任务照样跑完）。
func (s *Scheduler) pick() (TaskRequest, context.Context, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running || len(s.queue) == 0 || s.closed {
		return TaskRequest{}, nil, false
	}
	// 前台忙：不取任务，状态留在 queued（这就是「让位」的排队侧）。
	//
	// 按来源逐个判定：队首可能是「空闲自动」任务而被拦，但后面还有用户手动的任务——
	// 只看队首会让手动任务被前面那个空闲任务挡住（NFR-08 / FR-03-4）。
	idx := -1
	for i, cand := range s.queue {
		if s.runner.idleFor(cand.Trigger) {
			idx = i
			break
		}
	}
	if idx < 0 {
		return TaskRequest{}, nil, false
	}
	req := s.queue[idx]
	s.queue = append(s.queue[:idx:idx], s.queue[idx+1:]...)
	s.running = true
	s.runningID = req.SessionID
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	return req, ctx, true
}

// execute 执行单个任务并处理让位重排队。
func (s *Scheduler) execute(ctx context.Context, req TaskRequest) {
	res, err := s.runner.Run(ctx, req)

	s.mu.Lock()
	s.running = false
	s.runningID = ""
	cancel := s.cancel
	s.cancel = nil
	closed := s.closed
	yielded := res.Outcome == OutcomeYielded
	if yielded && !closed {
		// 让位：放回队首，进度已由执行侧落库，下次从断点续跑。
		s.queue = append([]TaskRequest{req}, s.queue...)
	}
	s.mu.Unlock()
	// cancel 在锁外释放：执行侧可能仍在用 context（例如落库收尾）。
	if cancel != nil {
		cancel()
	}

	switch {
	case err != nil:
		logx.For("transcribe").Warn("转写任务未执行", "sessionId", req.SessionID,
			"code", apierr.From(err, apierr.CodeInvalidArgument).Code)
	case yielded:
		logx.For("transcribe").Info("转写让位，等待空闲", "sessionId", req.SessionID,
			"progressMs", res.State.ProgressMs)
	}
	if yielded && !closed {
		// 让位后不要立刻自旋：等一个轮询周期或下次唤醒。
		select {
		case <-s.stop:
		case <-time.After(s.opts.Poll):
		}
	}
}
