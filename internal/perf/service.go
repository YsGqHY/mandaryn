package perf

import (
	"context"
	"log/slog"
	"time"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/utils/logx"
)

// Options 是装配层构造性能服务的参数（全部可省，零值走默认）。
type Options struct {
	// Cap 读整机预算 N（**每拍读库**，设置改完立即生效）。nil → 恒默认 80。
	Cap func() int
	// OnBudget 档位变化回调（只在变化时触发）。
	OnBudget func(BudgetState)
	// OnUsage 读数发布回调（约 1Hz；装配层接 EV-15 `perf:usage`）。
	OnUsage func(contract.PerfUsage)

	// SampleInterval / Window 采样周期与滑动平均窗口。
	SampleInterval time.Duration
	Window         time.Duration
	// DownHold / UpHold 滞回时长（见 BudgetOptions）。
	DownHold time.Duration
	UpHold   time.Duration
	// IdleOtherPctMax 空闲判定的 otherPct 上限。
	IdleOtherPctMax float64
	// MetricsCapacity 打点环形缓冲容量。
	MetricsCapacity int
	// Now 时间源（测试注入）。
	Now func() time.Time
	// Logger 日志器（默认 logx.For("perf")）。
	Logger *slog.Logger
}

// Service 把采样器、判定器、调度器与打点器组装成装配层可直接用的对象。
//
// 生命周期：`New` → `Start(ctx)` → 运行期读 `State()/Usage()/Idle()` → `Stop()`。
// 它不落库、不发事件；把 BudgetState / PerfUsage 转成 EV-15 与时间轴标记是 L3 的事。
type Service struct {
	sched   *Scheduler
	metrics *Metrics
	log     *slog.Logger
	opts    Options
}

// New 构造性能服务。
//
// **不会因为平台缺采样源而失败**：macOS 上采样器构造不出来时，服务进入「不采样、
// 不降档」的降级模式（`Degraded()` 为 true，档位恒 L0，`Idle()` 恒 true），
// 而不是让整个应用起不来。理由：预算限流是**软约束**，缺它应该降级而不是阻断启动；
// 但必须能被查询到，否则就成了「悄悄失效」。
func New(opts Options) *Service {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = logx.For("perf")
	}
	if opts.MetricsCapacity <= 0 {
		opts.MetricsCapacity = DefaultMetricsCapacity
	}

	sampler, err := NewSampler()
	reason := ""
	if err != nil {
		// 明确记录原因：日志级别用 warn，因为「预算限流失效」是排障时最先要怀疑的事。
		reason = err.Error()
		opts.Logger.Warn("perf 采样源不可用，降级为不采样、不降档", "err", err)
		sampler = nil
	}

	svc := &Service{
		metrics: NewMetrics(opts.MetricsCapacity, opts.Logger),
		log:     opts.Logger,
		opts:    opts,
	}

	onBudget := opts.OnBudget
	schedOpts := SchedulerOptions{
		SampleInterval:  opts.SampleInterval,
		Window:          opts.Window,
		Cap:             opts.Cap,
		OnUsage:         opts.OnUsage,
		IdleOtherPctMax: opts.IdleOtherPctMax,
		Budget: BudgetOptions{
			DownHold: opts.DownHold,
			UpHold:   opts.UpHold,
		},
		Now:    opts.Now,
		Logger: opts.Logger,
		// 档位变化先打点，再交给装配层：顺序保证「诊断时间线」一定有记录，
		// 也保证 OnBudget 里即便出错（panic 会在调度 goroutine 里），打点已经落好。
		OnBudget: func(st BudgetState) {
			svc.metrics.RecordLevel(st, "", 0)
			if onBudget != nil {
				onBudget(st)
			}
		},
	}
	svc.sched = newScheduler(sampler, schedOpts, reason)
	return svc
}

// Start 启动采样循环。
func (s *Service) Start(ctx context.Context) {
	s.sched.Start(ctx)
}

// Stop 停止采样循环（幂等）。
func (s *Service) Stop() { s.sched.Stop() }

// Tick 手动推进一拍（装配层自建循环或单测使用）。
func (s *Service) Tick() (BudgetState, error) { return s.sched.Tick() }

// State 返回当前预算状态。
func (s *Service) State() BudgetState { return s.sched.State() }

// Usage 返回前端读数（EV-15 payload）。
func (s *Service) Usage() contract.PerfUsage { return s.sched.Usage() }

// Idle 返回整机空闲判定（供 `transcribe.Options.Idle` 消费）。
func (s *Service) Idle() bool { return s.sched.Idle() }

// IdleFor 按触发来源给出空闲判定（手动发起恒 true）。
func (s *Service) IdleFor(trigger string) bool { return s.sched.IdleFor(trigger) }

// Degraded 报告采样是否不可用及原因。
func (s *Service) Degraded() (bool, string) { return s.sched.Degraded() }

// Metrics 返回打点收集器（FFD/SLD 由转写链路调用）。
func (s *Service) Metrics() *Metrics { return s.metrics }

// Reset 丢弃采样基准与滑动平均（路径切换、休眠唤醒后调用）。
func (s *Service) Reset() { s.sched.Reset() }

// CheckInferenceAllowed 在需要推理的操作入口做预算闸门（DOC-02 §4.7.4）。
//
// 暂停期间调用需要推理的方法（手动「重新转写」等）应返回 `ERR_PERF_SUSPENDED`
// （可重试），而不是静默排队——否则用户点了按钮却毫无反应，看起来像卡死。
//
// 注意：它**只拦推理**。录制相关操作绝不该调它（录制永不因预算中断）。
func (s *Service) CheckInferenceAllowed() error {
	st := s.sched.State()
	if !st.Suspended {
		return nil
	}
	return apierr.New(apierr.CodePerfSuspended).WithDetail("otherPct=%.1f capPct=%d",
		st.OtherPct, st.CapPct)
}
