package perf

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"sync"
	"time"

	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/utils/logx"
)

// SchedulerOptions 是调度器的装配参数。
type SchedulerOptions struct {
	// SampleInterval 采样周期，默认 500ms（DOC-02 §4.7.2）。
	SampleInterval time.Duration
	// Window 控制判据的滑动平均窗口，默认 5s / 10 点。
	Window time.Duration
	// Cap 每次判定都重新读整机预算 N（**每次读库**，改设置立即生效）。
	//
	// 为什么用函数而不是值：设置页改滑杆后不重启、不重建调度器就要生效；
	// 传值就得靠事件通知 + 加锁同步，反而更容易漏一次更新。
	Cap func() int
	// OnBudget 档位**变化**时回调（不变化不回调；当前状态随时可用 State() 取）。
	//
	// 只读、必须快速返回：它在采样循环里同步调用，堵住它就会让采样掉拍。
	OnBudget func(BudgetState)
	// OnUsage 读数发布回调，约 1Hz（EV-15 `perf:usage` 由 L3 薄壳转发）。
	OnUsage func(contract.PerfUsage)
	// IdleOtherPctMax 判定「整机空闲」时 otherPct 的上限，默认 10。
	IdleOtherPctMax float64
	// Budget 档位判定参数（SampleInterval 会以本结构的 SampleInterval 为准）。
	Budget BudgetOptions
	// Now 时间源（测试注入）。
	Now func() time.Time
	// Logger 日志器（默认 logx.For("perf")）。
	Logger *slog.Logger
}

// withDefaults 补齐零值。
func (o SchedulerOptions) withDefaults() SchedulerOptions {
	if o.SampleInterval <= 0 {
		o.SampleInterval = DefaultSampleInterval
	}
	if o.Window <= 0 {
		o.Window = DefaultWindow
	}
	if o.IdleOtherPctMax <= 0 {
		o.IdleOtherPctMax = DefaultIdleOtherPctMax
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = logx.For("perf")
	}
	o.Budget.SampleInterval = o.SampleInterval
	return o
}

// Scheduler 定期采样 → 判定档位 → 通知上层，并对外提供空闲判定。
//
// 它不落库、不发事件（那是 L3 的事），因此可被单测直接驱动。
type Scheduler struct {
	sampler *Sampler
	win     *avgWindow
	budget  *Budget
	opts    SchedulerOptions

	mu        sync.RWMutex
	latest    Snapshot
	hasLatest bool
	state     BudgetState
	hasState  bool
	usageAt   time.Time

	// degraded 表示采样源不可用（macOS / 句柄开不出来）。
	degraded       bool
	degradedReason string

	loopMu  sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
	started bool
}

// newScheduler 用给定采样器构造调度器（sampler 可为 nil = 降级模式）。
func newScheduler(sampler *Sampler, opts SchedulerOptions, degradedReason string) *Scheduler {
	opts = opts.withDefaults()
	s := &Scheduler{
		sampler:        sampler,
		win:            newAvgWindow(opts.Window, opts.SampleInterval),
		budget:         NewBudget(opts.Budget),
		opts:           opts,
		degraded:       sampler == nil,
		degradedReason: degradedReason,
	}
	s.state = s.budget.State()
	s.hasState = true
	return s
}

// Start 启动采样循环（非阻塞）。重复调用无副作用。
func (s *Scheduler) Start(ctx context.Context) {
	s.loopMu.Lock()
	defer s.loopMu.Unlock()
	if s.started {
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.done = make(chan struct{})
	s.started = true
	go s.loop(ctx)
}

// Stop 停止采样循环并等待其退出；未启动时无副作用，可重复调用。
func (s *Scheduler) Stop() {
	s.loopMu.Lock()
	if !s.started {
		s.loopMu.Unlock()
		return
	}
	cancel, done := s.cancel, s.done
	s.started, s.cancel = false, nil
	s.loopMu.Unlock()

	cancel()
	<-done
}

// loop 是采样循环本体。
//
// 只用一个 goroutine + ticker：采样开销要在「不干扰正常使用」的量级（DOC-02 §4.7.2
// 要求 < 0.3% CPU），起多个 goroutine 做同一件事没有收益。
func (s *Scheduler) loop(ctx context.Context) {
	defer close(s.done)

	// 立刻取一拍：否则启动后的第一个 500ms 里 Idle() 只能靠缺省值，
	// 而自动转写恰好可能在这时被触发。
	s.tickLogged()

	ticker := time.NewTicker(s.opts.SampleInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.tickLogged()
		}
	}
}

// tickLogged 跑一拍并把「真错误」记为 warn（无数据拍只记 debug，避免刷屏）。
func (s *Scheduler) tickLogged() {
	if _, err := s.Tick(); err != nil {
		s.opts.Logger.Warn("perf 采样失败", "err", err)
	}
}

// Tick 推进一个采样周期并返回判定结果。
//
// 装配层与单测共用：单测不必真的等 500ms 就能逐拍推进档位。
func (s *Scheduler) Tick() (BudgetState, error) {
	snap, err := s.sample()

	var avg Snapshot
	switch {
	case err == nil:
		avg = s.win.Add(snap)
		avg.Valid = true
	case isNoData(err):
		// 「本拍没有可用差值」不是故障：把无效快照交给判定器，它会保持档位不动。
		avg = Snapshot{SampledAt: s.opts.Now(), Valid: false}
	default:
		// 真故障（读数系统调用失败）：同样不降档（我们连 otherPct 都不知道），
		// 但要让调用方知道，便于上层写日志 / 诊断。
		avg = Snapshot{SampledAt: s.opts.Now(), Valid: false}
	}

	capPct := s.capPct()
	state := s.budget.Evaluate(avg, capPct)
	avg = avg.WithCap(capPct)

	s.mu.Lock()
	prev, hadState := s.state, s.hasState
	s.latest, s.hasLatest = avg, true
	s.state, s.hasState = state, true
	now := s.opts.Now()
	publishUsage := !s.usageAt.IsZero() && now.Sub(s.usageAt) >= time.Second
	if s.usageAt.IsZero() {
		publishUsage = true
	}
	if publishUsage {
		s.usageAt = now
	}
	s.mu.Unlock()

	if state.Changed || !hadState || state.Level != prev.Level {
		s.opts.Logger.Info("perf 档位变化",
			"from", prev.Level.String(), "to", state.Level.String(),
			"reason", state.Reason,
			"capPct", state.CapPct,
			"otherPct", round1(state.OtherPct),
			"headRoom", round1(state.HeadRoom),
			// 只写元数据，不写音频/正文（NFR-03-5）。
		)
		if state.Changed && s.opts.OnBudget != nil {
			s.opts.OnBudget(state)
		}
	} else {
		s.opts.Logger.Debug("perf 采样",
			"level", state.Level.String(),
			"otherPct", round1(state.OtherPct),
			"selfPct", round1(state.SelfCpuPct),
			"headRoom", round1(state.HeadRoom),
		)
	}
	if publishUsage && s.opts.OnUsage != nil {
		s.opts.OnUsage(s.Usage())
	}

	if err != nil && !isNoData(err) {
		return state, err
	}
	return state, nil
}

// sample 取一拍；降级模式返回 ErrUnsupported（由 isNoData 吸收）。
func (s *Scheduler) sample() (Snapshot, error) {
	if s.sampler == nil {
		return Snapshot{}, ErrUnsupported
	}
	return s.sampler.Sample()
}

// isNoData 判断是否属于「本拍无数据」而非真故障。
func isNoData(err error) bool {
	return errors.Is(err, ErrNoBaseline) ||
		errors.Is(err, ErrNoInterval) ||
		errors.Is(err, ErrCounterReset) ||
		errors.Is(err, ErrUnsupported)
}

// capPct 读当前 N，读不到（nil）走默认。
func (s *Scheduler) capPct() int {
	if s.opts.Cap == nil {
		return DefaultCapPct
	}
	return normalizeCap(s.opts.Cap())
}

// State 返回当前预算状态。
func (s *Scheduler) State() BudgetState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}

// SnapshotOf 返回最近一次（滑动平均后的）读数。
func (s *Scheduler) SnapshotOf() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.latest
}

// Degraded 报告采样是否不可用及原因（不可用时档位恒为 L0、Idle 恒为 true）。
func (s *Scheduler) Degraded() (bool, string) { return s.degraded, s.degradedReason }

// Reset 丢弃采样基准与滑动平均（路径切换、休眠唤醒、N 大幅变化后调用）。
func (s *Scheduler) Reset() {
	if s.sampler != nil {
		s.sampler.Reset()
	}
	s.win.Reset()
	s.budget.Reset()

	s.mu.Lock()
	s.latest, s.hasLatest = Snapshot{}, false
	s.state, s.hasState = s.budget.State(), true
	s.mu.Unlock()
}

// Idle 判定整机当前是否适合**自动排队**的转写任务起跑（供 `transcribe.Options.Idle`）。
//
// 判定规则（三条同时满足才算空闲）：
//  1. 当前档位 ≤ L1——L2/L3 已经要牺牲推理质量了，这时再塞转写任务只会更糟；
//     也不接受 P（`state.Suspended` 时直接 false）。
//  2. 最近一次有效读数的 otherPct < IdleOtherPctMax（默认 10）——沿用 PRD 对
//     「空载」的定义，避免出现第二套空闲口径。
//  3. 采样源可用且已有读数。
//
// 反过来，**没有读数时返回 true**（不阻塞）：刚启动的那几百毫秒不该让自动转写干等，
// 真正的限流由档位负责——任务起跑后若额度告急，`checkpoint` 会让位并保留进度。
//
// ⚠️ 本判定只约束**自动排队**。手动发起的转写必须立即执行（`FR-03-4`），请用
// `IdleFor(trigger)`；直接把它接到 `transcribe.Options.Idle` 会连手动任务一起拦，
// 见 docs/DEV-ISSUES-S8.md S8-01。
func (s *Scheduler) Idle() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.idleLocked()
}

// IdleFor 按触发来源给出空闲判定：**手动发起永不受限**（FR-03-4「立即执行」）。
//
// 这是本域对装配层的推荐接线方式；由于 `transcribe.Options.Idle` 的签名不带触发来源，
// 真正接线还需要 transcribe 侧配合（S8-01）。
func (s *Scheduler) IdleFor(trigger string) bool {
	if trigger == contract.TriggerManual {
		return true
	}
	return s.Idle()
}

// idleLocked 是 Idle 的实现（调用方持锁）。
func (s *Scheduler) idleLocked() bool {
	if s.degraded {
		// 采样不可用 = 不知道忙不忙，此时不该假装空闲也不该假装忙：
		// 选择「不阻塞」（见 Idle 注释），并把原因暴露给诊断。
		return true
	}
	if !s.hasLatest || !s.latest.Valid {
		return true
	}
	if s.state.Suspended {
		return false
	}
	if s.state.Level.Rank() > LevelL1.Rank() {
		return false
	}
	return s.latest.OtherPct < s.opts.IdleOtherPctMax
}

// Usage 返回前端读数（EV-15 `contract.PerfUsage`）。
func (s *Scheduler) Usage() contract.PerfUsage {
	s.mu.RLock()
	snap, st := s.latest, s.state
	s.mu.RUnlock()

	sampledAt := snap.SampledAt
	if sampledAt.IsZero() {
		sampledAt = s.opts.Now()
	}
	return contract.PerfUsage{
		CapPct:      st.CapPct,
		TotalCpuPct: roundInt(snap.TotalCpuPct),
		SelfCpuPct:  roundInt(snap.SelfCpuPct),
		OtherPct:    roundInt(snap.OtherPct),
		// HeadroomPct 保留负值：负 = 已超额，前端据此显示「已超出预算 x%」，
		// 夹到 0 会把「刚刚超额」与「超额很多」抹成一样。
		HeadroomPct: roundInt(snap.HeadRoom),
		Level:       st.Level.String(),
		Suspended:   st.Suspended,
		WindowMs:    s.opts.Window.Milliseconds(),
		SampledAt:   sampledAt.Format(time.RFC3339Nano),
	}
}

// WindowPoints 返回滑动平均窗口的点数（诊断用）。
func (s *Scheduler) WindowPoints() int { return s.win.Size() }

func roundInt(v float64) int {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return int(math.Round(v))
}

func round1(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return math.Round(v*10) / 10
}
