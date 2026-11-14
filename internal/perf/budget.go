package perf

import (
	"sync"
	"time"

	"vip.oberon.mandaryn/internal/contract"
)

// 档位阈值的**比例**口径（DOC-02 §4.7.3）。
//
// 表里写的是「`headroom` 相对 N 的比例」，所以这里存比例而不是绝对百分点：
// N 从 80 改成 30 时，阈值应当跟着缩，否则 L1/L2/L3 会挤在一起甚至倒挂。
const (
	// ratioL0 L0 的下限：headroom > 60% × N。
	ratioL0 = 0.60
	// ratioL1 L1 的下限：headroom > 40% × N。
	ratioL1 = 0.40
	// ratioL2 L2 的下限：headroom > 20% × N。
	ratioL2 = 0.20
	// ratioL3 L3：headroom > 0 且 ≤ 20% × N；headroom ≤ 0 则是 P。
)

// 判定原因（稳定 token，不是给人看的文案）。
//
// 为什么不直接给中文文案：人类可见字符串必须走前端 i18n（本项目铁律），
// 后端只给 token，由前端映射成 `perf.reason.<token>`。
const (
	// ReasonNormal 额度充足，L0 正常跑。
	ReasonNormal = "normal"
	// ReasonNarrowing 额度收窄或处于降档中（含「已变差但还在等确认」）。
	ReasonNarrowing = "narrowing"
	// ReasonExhausted 额度耗尽，推理已暂停。
	ReasonExhausted = "exhausted"
	// ReasonRecovered 额度回升并已升档（含从 P 恢复）。
	ReasonRecovered = "recovered"
	// ReasonNoData 本拍没有可用读数（首次 / 间隔 0 / 计数器回绕），档位保持不变。
	ReasonNoData = "nodata"
)

// BudgetOptions 是判定器的可调参数。
//
// 时长类参数在这里而**不是**在每次 Evaluate 里传：连续确认次数 = 时长 ÷ 采样周期，
// 换算一次即可，也保证「改采样周期」不会意外改变滞回的时间语义。
type BudgetOptions struct {
	// SampleInterval 采样周期，把 hold 时长换算成连续确认次数。默认 500ms。
	SampleInterval time.Duration
	// DownHold 降档所需的连续确认时长。默认 3s（与 PRD「暂停判据：连续 3s」对齐）。
	DownHold time.Duration
	// UpHold 升档所需的稳定时长。默认 15s（DOC-02 §4.7.3「回升比降档更保守」）。
	UpHold time.Duration
	// UpMarginRatio 升档必须越过目标档阈值再多出的比例（× N）。默认 0.10。
	UpMarginRatio float64
	// ResumeMarginPct 从 P 恢复所需的百分点滞回带（PRD：`otherPct ≤ N − 5`）。默认 5。
	ResumeMarginPct float64
}

// withDefaults 补齐零值。
func (o BudgetOptions) withDefaults() BudgetOptions {
	if o.SampleInterval <= 0 {
		o.SampleInterval = DefaultSampleInterval
	}
	if o.DownHold < 0 {
		o.DownHold = 0
	}
	if o.DownHold == 0 {
		o.DownHold = 3 * time.Second
	}
	if o.UpHold < 0 {
		o.UpHold = 0
	}
	if o.UpHold == 0 {
		o.UpHold = 15 * time.Second
	}
	if o.UpMarginRatio <= 0 {
		o.UpMarginRatio = 0.10
	}
	if o.ResumeMarginPct <= 0 {
		o.ResumeMarginPct = 5
	}
	return o
}

// ticks 把时长换算成连续确认次数（至少 1 次）。
func ticks(hold, interval time.Duration) int {
	if interval <= 0 {
		interval = DefaultSampleInterval
	}
	n := int(hold / interval)
	if n < 1 {
		n = 1
	}
	return n
}

// BudgetState 是预算判定的结果。
//
// ⚠️ 语义边界：它只回答「**推理**该怎么让步」。**不要、也不能**从这里推导出「要不要
// 停录制」——`Suspended` 指的是暂停推理任务（转写、声纹提取），音频采集与落盘必须
// 照旧继续（`NFR-08` 硬要求，DOC-02 §1.2）。之所以在类型上就不提供任何录制相关字段，
// 是为了让「顺手把录制停掉」这种改动无法通过类型检查，而不是靠注释提醒。
type BudgetState struct {
	// Level 当前档位 L0|L1|L2|L3|P。
	Level Level `json:"level"`
	// Reason 判定原因 token（normal|narrowing|exhausted|recovered|nodata）。
	Reason string `json:"reason"`
	// Throttled 正交标记：正在降档（Level > L0）。
	Throttled bool `json:"throttled"`
	// Suspended 正交标记：预算耗尽，**推理**暂停。
	Suspended bool `json:"suspended"`
	// Changed 本次判定是否改变了档位。
	Changed bool `json:"changed"`
	// Resumed 本次判定是否从 P 恢复（用于区分 perf-resume 与普通升档）。
	Resumed bool `json:"resumed"`
	// CapPct 当次判定用的 N。
	CapPct int `json:"capPct"`
	// TotalCpuPct / SelfCpuPct / OtherPct 判定所用的（滑动平均后的）分量。
	TotalCpuPct float64 `json:"totalCpuPct"`
	SelfCpuPct  float64 `json:"selfCpuPct"`
	OtherPct    float64 `json:"otherPct"`
	// HeadRoom 本应用可用额度 = N − OtherPct，可为负。
	HeadRoom float64 `json:"headRoom"`
	// Since 进入当前档位的时刻。
	Since time.Time `json:"since"`
}

// Marker 返回该状态应写入会话时间轴的标记类型（DOC-04 TBL-10），无变化时为空。
func (s BudgetState) Marker() string {
	if !s.Changed {
		return ""
	}
	switch {
	case s.Suspended:
		return contract.MarkerPerfSuspend
	case s.Resumed:
		return contract.MarkerPerfResume
	default:
		return contract.MarkerPerfThrottle
	}
}

// Budget 是档位判定器（带滞回）。并发安全。
type Budget struct {
	opts BudgetOptions
	// downTicks / upTicks 是连续确认次数。
	downTicks int
	upTicks   int

	mu      sync.Mutex
	cur     Level
	since   time.Time
	downN   int
	upN     int
	last    BudgetState
	hasLast bool
}

// NewBudget 构造判定器（零值参数走默认）。
func NewBudget(opts BudgetOptions) *Budget {
	opts = opts.withDefaults()
	return &Budget{
		opts:      opts,
		downTicks: ticks(opts.DownHold, opts.SampleInterval),
		upTicks:   ticks(opts.UpHold, opts.SampleInterval),
		cur:       LevelL0,
	}
}

// Options 返回生效的参数（诊断/测试用）。
func (b *Budget) Options() BudgetOptions { return b.opts }

// Level 返回当前档位。
func (b *Budget) Level() Level {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cur
}

// State 返回最近一次判定结果（尚未判定过则给出 L0 的合成状态）。
func (b *Budget) State() BudgetState {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.hasLast {
		return b.last
	}
	return BudgetState{
		Level:     b.cur,
		Reason:    reasonFor(b.cur),
		Throttled: b.cur.Throttled(),
		Suspended: b.cur.Paused(),
		Since:     b.since,
		CapPct:    DefaultCapPct,
	}
}

// Reset 把档位复位到 L0 并清空确认计数（采样源重建 / 用户大幅调整 N 后调用）。
func (b *Budget) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cur = LevelL0
	b.since = time.Time{}
	b.downN, b.upN = 0, 0
	// 必须一起清掉最近一次判定：否则 State() 会把复位前的降档状态当成现状报出去，
	// 让上层以为「刚 Reset 完还在 L2」。这也是当初 Reset 单测抓到的真问题。
	b.last, b.hasLast = BudgetState{}, false
}

// Evaluate 用一拍（通常是滑动平均后的）读数判定档位。
//
// 滞回设计（为什么升档与降档用两套不同的门）：
//   - **降档**：`rawLevel` 比当前差就累计 downN，连续 `downTicks` 次才动，且一次落到
//     目标档（可跨级）。理由：额度真的被别的程序吃掉时应当尽快让路，逐级爬要 4×3s，
//     会让 PRD 的「3s 内进入暂停」落空；而在滑动平均之上再多等 3s 足以滤掉突变。
//   - **升档**：必须连续 `upTicks` 次（默认 15s）满足「越过目标档阈值 + UpMargin×N」，
//     且一次只升一级。理由：升早了会立刻被下一次抖动打回来，用户看到的是转写在
//     L0/L1 之间反复横跳（每次切换都要重配引擎线程数，代价远大于晚 15s 恢复）；
//     从 P 恢复另加 `N − ResumeMarginPct` 的百分点滞回（PRD 恢复判据）。
//
// `s.Valid == false`（首次 / 间隔 0 / 计数器回绕）时**不动档位也不清计数**：
// 一次采样空洞不代表负载变了，清计数会把「已连续确认 2.9s」白白作废；
// 但也不累计（否则空洞会被当成「持续降档」的证据）。
func (b *Budget) Evaluate(s Snapshot, capPct int) BudgetState {
	n := normalizeCap(capPct)

	b.mu.Lock()
	defer b.mu.Unlock()

	if !s.Valid {
		return b.state(ReasonNoData, false, false, s, n)
	}

	headRoom := float64(n) - s.OtherPct
	target := levelFor(headRoom, float64(n))
	cur := b.cur

	switch {
	case target.Rank() > cur.Rank():
		// —— 变差：等连续确认后一次落到目标档 ——
		b.upN = 0
		b.downN++
		if b.downN < b.downTicks {
			return b.state(ReasonNarrowing, false, false, s, n)
		}
		b.downN = 0
		b.cur = target
		b.since = s.SampledAt
		return b.state(reasonFor(target), true, false, s, n)

	case target.Rank() < cur.Rank():
		// —— 变好：更保守，只升一级且要求越过余量带 ——
		b.downN = 0
		next := cur.stepUp()
		if !upwardAllowed(cur, next, headRoom, s.OtherPct, float64(n), b.opts) {
			// 已经比当前档好、但还没好到值得升：既不升也不降，保持现状。
			// 这里清零 upN 是有意的——「刚好差一点点」不算稳定，不能攒进度。
			b.upN = 0
			return b.state(reasonFor(cur), false, false, s, n)
		}
		b.upN++
		if b.upN < b.upTicks {
			return b.state(reasonFor(cur), false, false, s, n)
		}
		b.upN = 0
		wasPaused := cur.Paused()
		b.cur = next
		b.since = s.SampledAt
		return b.state(ReasonRecovered, true, wasPaused, s, n)

	default:
		// —— 同档：计数清零，避免「隔拍抖动」攒够确认次数 ——
		b.downN, b.upN = 0, 0
		return b.state(reasonFor(cur), false, false, s, n)
	}
}

// state 组装对外状态并记为「最近一次判定」（调用方持有 b.mu）。
func (b *Budget) state(reason string, changed, resumed bool, s Snapshot, n int) BudgetState {
	headRoom := float64(n) - s.OtherPct
	st := BudgetState{
		Level:       b.cur,
		Reason:      reason,
		Throttled:   b.cur.Throttled(),
		Suspended:   b.cur.Paused(),
		Changed:     changed,
		Resumed:     resumed,
		CapPct:      n,
		TotalCpuPct: s.TotalCpuPct,
		SelfCpuPct:  s.SelfCpuPct,
		OtherPct:    s.OtherPct,
		HeadRoom:    headRoom,
		Since:       b.since,
	}
	b.last, b.hasLast = st, true
	return st
}

// reasonFor 按档位给出默认原因。
func reasonFor(l Level) string {
	switch {
	case l.Paused():
		return ReasonExhausted
	case l.Throttled():
		return ReasonNarrowing
	default:
		return ReasonNormal
	}
}

// levelFor 按 headroom 相对 N 的比例给出「无滞回的」目标档位。
func levelFor(headRoom, n float64) Level {
	if n <= 0 {
		n = DefaultCapPct
	}
	if headRoom <= 0 {
		return LevelP
	}
	r := headRoom / n
	switch {
	case r > ratioL0:
		return LevelL0
	case r > ratioL1:
		return LevelL1
	case r > ratioL2:
		return LevelL2
	default:
		return LevelL3
	}
}

// upwardRatio 返回「从该档往下进入」的阈值比例，升档时以它为基准加余量。
func upwardRatio(target Level) float64 {
	switch target {
	case LevelL0:
		return ratioL0
	case LevelL1:
		return ratioL1
	case LevelL2:
		return ratioL2
	default:
		return 0
	}
}

// upwardAllowed 判定是否满足升一档的门槛（含 P → L3 的特殊恢复判据）。
func upwardAllowed(cur, next Level, headRoom, otherPct, n float64, opts BudgetOptions) bool {
	if headRoom <= 0 {
		return false
	}
	// 从 P 恢复：PRD 的恢复判据是 otherPct ≤ N − 5（5 个百分点的滞回带）。
	if cur.Paused() {
		return otherPct <= n-opts.ResumeMarginPct
	}
	// 其余档位：headroom 必须超过目标档阈值 + 余量，才有资格计数。
	return headRoom/n > upwardRatio(next)+opts.UpMarginRatio
}
