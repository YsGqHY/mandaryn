package perf

import (
	"log/slog"
	"sort"
	"sync"
	"time"

	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/utils/logx"
)

// —— 性能打点（DOC-02 §7.2 / §7.3）——
//
// 本文件只提供**打点入口与聚合**，不产生现成数值：
//   - FFD（首字延迟）= t(首个 partial 渲染到前端) − t(用户触发) —— 前半段在后端、
//     后半段在前端，只有录制/转写链路能给出这个时刻，而且**必须含前端渲染**；
//   - SLD（句子延迟）= t(segment 落库 finalized) − t(VAD 判定句末帧) —— 同样由
//     转写链路提供。
//
// 因此本域不假装能自己算 FFD/SLD：谁有真实时刻谁调 RecordFFD/RecordSLD，
// 这里负责记日志、进内存环形时间线、算 P50/P95 供诊断面板导出。
//
// 隐私：只记录**元数据**（时长、会话 ID、引擎 ID、档位），不记音频、正文、声纹向量
// （FR-08 铁律）。

// 打点类型，取值与 DOC-02 §7.2 的表对齐。
const (
	// PointLevel 档位变化（对应 §7.2 的降档/暂停/恢复语义）。
	PointLevel = "perf.level"
	// PointFFD 首个 partial（§7.2 `rec.firstPartial`）。
	PointFFD = "rec.firstPartial"
	// PointSLD 句子定稿（§7.2 `seg.finalized`）。
	PointSLD = "seg.finalized"
)

// DefaultMetricsCapacity 是内存环形时间线的默认容量。
//
// 为什么是 512：一次 1 小时录制大约产生几十次档位变化与上千条 SLD；
// 诊断面板只需要最近一段，512 条足够覆盖一次典型会话且占用可忽略
// （每条 ~80B，总计 ~40KB）。
const DefaultMetricsCapacity = 512

// Event 是一条打点记录。
type Event struct {
	// Kind PointLevel / PointFFD / PointSLD。
	Kind string `json:"kind"`
	// AtMs 会话内相对时刻（毫秒），由调用方按帧计数时基给出。
	AtMs int64 `json:"atMs"`
	// At 记录时刻（wall clock，仅诊断展示）。
	At time.Time `json:"at"`
	// SessionID 相关会话（档位变化可能不属于任何会话，为空）。
	SessionID string `json:"sessionId"`
	// Level 档位（PointLevel 有效）。
	Level string `json:"level"`
	// Reason 判定原因 token（PointLevel 有效）。
	Reason string `json:"reason"`
	// ValueMs 度量值（FFD / SLD 毫秒）。
	ValueMs float64 `json:"valueMs"`
	// AudioMs 该句的音频时长（SLD 有效，用于算 RTF）。
	AudioMs int64 `json:"audioMs"`
	// EngineID 产出该片段的引擎。
	EngineID string `json:"engineId"`
}

// Summary 是聚合结果（诊断面板 / 验收脚本用）。
type Summary struct {
	// LevelChanges 档位变化次数。
	LevelChanges int `json:"levelChanges"`
	// SuspendCount 进入暂停的次数。
	SuspendCount int `json:"suspendCount"`
	// FFDCount / FFDMaxMs 首字延迟样本数与其最大值。
	FFDCount int     `json:"ffdCount"`
	FFDMaxMs float64 `json:"ffdMaxMs"`
	// SLDCount / SLDP50Ms / SLDP95Ms 句子延迟样本数与分位。
	SLDCount int     `json:"sldCount"`
	SLDP50Ms float64 `json:"sldP50Ms"`
	SLDP95Ms float64 `json:"sldP95Ms"`
}

// Metrics 是性能打点收集器。并发安全。
type Metrics struct {
	mu   sync.Mutex
	cap  int
	pos  int
	ring []Event

	ffd        []float64
	sld        []float64
	levelUp    int
	suspendCnt int

	log *slog.Logger
}

// NewMetrics 构造收集器；capacity ≤ 0 用默认 512。
func NewMetrics(capacity int, logger *slog.Logger) *Metrics {
	if capacity <= 0 {
		capacity = DefaultMetricsCapacity
	}
	if logger == nil {
		logger = logx.For("perf")
	}
	return &Metrics{cap: capacity, log: logger}
}

// RecordLevel 记录一次档位变化（含进入暂停 / 从暂停恢复）。
//
// 只有 `state.Changed` 的状态才该记录：调用方通常接在 OnBudget 上，
// 那里已经保证只收到变化事件。
func (m *Metrics) RecordLevel(state BudgetState, sessionID string, atMs int64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.levelUp++
	if state.Suspended {
		m.suspendCnt++
	}
	m.pushLocked(Event{
		Kind:      PointLevel,
		AtMs:      atMs,
		At:        time.Now(),
		SessionID: sessionID,
		Level:     state.Level.String(),
		Reason:    state.Reason,
	})
	m.mu.Unlock()

	// 打点即日志：DOC-02 §7.2 要求关键路径埋点同时进 logx 与内存时间线。
	m.log.Info("perf 档位打点",
		"level", state.Level.String(),
		"reason", state.Reason,
		"marker", state.Marker(),
		"capPct", state.CapPct,
		"otherPct", round1(state.OtherPct),
		"headRoom", round1(state.HeadRoom),
		"sessionId", sessionID,
		"atMs", atMs,
	)
}

// RecordFFD 记录一次首字延迟（毫秒）。
//
// 调用方必须是**真正知道首个 partial 渲染完成时刻**的一方；本域只聚合。
func (m *Metrics) RecordFFD(sessionID string, ffdMs float64, atMs int64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.ffd = append(m.ffd, ffdMs)
	m.pushLocked(Event{
		Kind: PointFFD, AtMs: atMs, At: time.Now(), SessionID: sessionID, ValueMs: ffdMs,
	})
	m.mu.Unlock()

	m.log.Info("perf 首字延迟打点", "ffdMs", round1(ffdMs), "sessionId", sessionID)
}

// RecordSLD 记录一次句子定稿延迟（毫秒）。
func (m *Metrics) RecordSLD(sessionID string, sldMs float64, audioMs int64, engineID string, atMs int64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.sld = append(m.sld, sldMs)
	m.pushLocked(Event{
		Kind: PointSLD, AtMs: atMs, At: time.Now(), SessionID: sessionID,
		ValueMs: sldMs, AudioMs: audioMs, EngineID: engineID,
	})
	m.mu.Unlock()

	m.log.Info("perf 句子延迟打点", "sldMs", round1(sldMs), "audioMs", audioMs,
		"sessionId", sessionID, "engineId", engineID)
}

// pushLocked 写环形缓冲（调用方持锁）。
func (m *Metrics) pushLocked(e Event) {
	if len(m.ring) < m.cap {
		m.ring = append(m.ring, e)
		return
	}
	m.ring[m.pos%m.cap] = e
	m.pos++
}

// Events 按时间顺序返回环形缓冲里的打点（最旧的在前）。
func (m *Metrics) Events() []Event {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	n := len(m.ring)
	if n < m.cap || m.pos == 0 {
		out := make([]Event, n)
		copy(out, m.ring)
		return out
	}
	out := make([]Event, 0, n)
	start := m.pos % m.cap
	out = append(out, m.ring[start:]...)
	out = append(out, m.ring[:start]...)
	return out
}

// Summary 返回聚合结果。
func (m *Metrics) Summary() Summary {
	if m == nil {
		return Summary{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	s := Summary{
		LevelChanges: m.levelUp,
		SuspendCount: m.suspendCnt,
		FFDCount:     len(m.ffd),
		SLDCount:     len(m.sld),
	}
	for _, v := range m.ffd {
		if v > s.FFDMaxMs {
			s.FFDMaxMs = v
		}
	}
	if len(m.sld) > 0 {
		sorted := append([]float64(nil), m.sld...)
		sort.Float64s(sorted)
		s.SLDP50Ms = percentile(sorted, 0.50)
		s.SLDP95Ms = percentile(sorted, 0.95)
	}
	return s
}

// Reset 清空所有打点与聚合（新会话开始 / 诊断面板清屏）。
func (m *Metrics) Reset() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ring = m.ring[:0]
	m.ffd = m.ffd[:0]
	m.sld = m.sld[:0]
	m.pos = 0
	m.levelUp, m.suspendCnt = 0, 0
}

// percentile 取已排序样本的分位值（线性插值，与常见监控口径一致）。
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if len(sorted) == 1 {
		return sorted[0]
	}
	pos := p * float64(len(sorted)-1)
	lo := int(pos)
	hi := lo + 1
	if hi >= len(sorted) {
		return sorted[len(sorted)-1]
	}
	frac := pos - float64(lo)
	return sorted[lo] + (sorted[hi]-sorted[lo])*frac
}

// MarkerFor 是给装配层的小工具：把一次档位变化转成可落库的时间轴标记。
//
// 放在本包是因为「哪种变化对应哪个 marker」属于本域的语义；落库仍由装配层做。
func MarkerFor(state BudgetState) (kind string, note string, ok bool) {
	if !state.Changed {
		return "", "", false
	}
	marker := state.Marker()
	if marker == "" {
		return "", "", false
	}
	return marker, state.Reason, true
}

// 确保 contract 的 marker 常量被本包引用（编译期约定：三处字符串必须存在）。
var _ = []string{
	contract.MarkerPerfThrottle,
	contract.MarkerPerfSuspend,
	contract.MarkerPerfResume,
}
