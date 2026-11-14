package recording

import (
	"sync"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
)

// transitions 是录制状态机的**全部合法转换**（DOC-02 §6.2 表格）。
//
// 表驱动而不是一堆 if：非法转换必须稳定返回 ERR_STATE_FORBIDDEN，
// 而「哪些允许」是最容易被后续需求悄悄改动的部分，集中在一处才能被测试穷举。
//
// 与文档的差异（两处补充，已在 docs/DEV-ISSUES-S2.md 记录）：
//   - `recording → failed` / `paused → failed` / `finalizing → failed`：
//     ST-09 的语义是「终止性失败（无法继续也无法恢复）」，磁盘满导致写入失败
//     正是这一类；若不允许这几条，写入失败时会卡在 recording 里无法收尾。
//   - `completed/interrupted/failed → preparing`：文档 §6.1 已写明这三种状态下
//     「开始」按钮可用，等价于一条回到 preparing 的转换。
var transitions = map[string][]string{
	contract.StateIdle:        {contract.StatePreparing},
	contract.StatePreparing:   {contract.StateRecording, contract.StateFailed},
	contract.StateRecording:   {contract.StatePaused, contract.StateFinalizing, contract.StateInterrupted, contract.StateFailed},
	contract.StatePaused:      {contract.StateRecording, contract.StateFinalizing, contract.StateInterrupted, contract.StateFailed},
	contract.StateFinalizing:  {contract.StateCompleted, contract.StateInterrupted, contract.StateFailed},
	contract.StateCompleted:   {contract.StatePreparing},
	contract.StateInterrupted: {contract.StatePreparing},
	contract.StateFailed:      {contract.StatePreparing},
}

// StateMachine 是录制主状态机（ST-01~ST-09，DOC-02 §6）。
//
// 并发安全：用互斥保护。注意它只保护**状态值**；上层 Recorder 的锁粒度更大
// （状态 + 会话 + 通道），两者不重叠加锁，避免 ABBA 死锁。
type StateMachine struct {
	mu  sync.Mutex
	cur string
}

// NewStateMachine 构造初始为 idle（ST-01）的状态机。
func NewStateMachine() *StateMachine {
	return &StateMachine{cur: contract.StateIdle}
}

// Current 当前状态。
func (m *StateMachine) Current() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cur
}

// IsKnown 判断是否是可存储的合法状态值（ST-01~ST-06/08/09）。
//
// `degraded` / `throttled` / `suspended` 是**正交标记**，不是状态值，
// 因此这里不认它们（DOC-02 §6.1 铁律：禁止把标记塞进 state 枚举）。
func IsKnownState(s string) bool {
	switch s {
	case contract.StateIdle, contract.StatePreparing, contract.StateRecording,
		contract.StatePaused, contract.StateFinalizing, contract.StateCompleted,
		contract.StateInterrupted, contract.StateFailed:
		return true
	}
	return false
}

// ValidTransition 判断 from → to 是否合法。
func ValidTransition(from, to string) bool {
	if from == to {
		return false
	}
	for _, allowed := range transitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// LegalTransitions 返回 from 的全部合法目标（副本，便于测试穷举）。
func LegalTransitions(from string) []string {
	allowed := transitions[from]
	out := make([]string, len(allowed))
	copy(out, allowed)
	return out
}

// To 尝试转换到 next；非法转换返回 ERR_STATE_FORBIDDEN（detail 带 from/to）。
//
// 注意 from==to 也算非法：文档明确「recording → recording（重复开始）」必须拒绝，
// 否则重复点击「开始」会被静默吞掉，用户看不到任何反馈。
func (m *StateMachine) To(next string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !IsKnownState(next) {
		return apierr.New(apierr.CodeStateForbidden).
			WithDetail("未知的录制状态：%q（from=%s）", next, m.cur)
	}
	if !ValidTransition(m.cur, next) {
		return apierr.New(apierr.CodeStateForbidden).
			WithDetail("非法状态转换：%s → %s", m.cur, next)
	}
	m.cur = next
	return nil
}

// Force 无条件设置状态，仅用于启动自检（进程重启后内存态为 idle，
// 需要把残留会话在内存里对齐到 interrupted）。不在业务路径上使用。
func (m *StateMachine) Force(next string) error {
	if !IsKnownState(next) {
		return apierr.New(apierr.CodeStateForbidden).
			WithDetail("未知的录制状态：%q", next)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cur = next
	return nil
}

// StateToContract 把主状态投影为前端 DTO 的 State 字段。
//
// 之所以要这个函数而不是直接用字符串：状态值必须与 contract 常量同源，
// 任何「前端认识的枚举」都从这里出，禁止各处硬编码 "recording" 字面量。
func StateToContract(state string) string {
	if !IsKnownState(state) {
		// 未知值不外泄给前端（会让 UI 落到 default 分支显示奇怪状态）。
		return contract.StateIdle
	}
	return state
}
