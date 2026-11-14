package transcribe

import (
	"strings"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
)

// 转写状态轴（ARCH §6.4 / DATA-MODEL TBL-03 `transcription_status`）。
//
// 为什么要单独一个文件、单独一条轴：录制主状态机 ST-01~ST-09 只描述**采集侧**，
// 转写是录制结束后的独立任务。把转写进度塞进 `state` 会让「已 completed 但还在
// 转写中」这种完全正常的组合无法表达，前端只能靠猜。
//
// 因此本文件的所有常量都**只**用于 `sessions.transcription_status`，
// 严禁写进 `sessions.state`，也严禁为转写新增 ST 编号（ARCH §6.4 约束）。
const (
	// StatusNone 未转写（刚封存的录音实例默认值）。
	StatusNone = contract.TranscriptionNone
	// StatusQueued 已排队，等待执行（含被抢占后回收）。
	StatusQueued = contract.TranscriptionQueued
	// StatusRunning 正在转写。
	StatusRunning = contract.TranscriptionRunning
	// StatusDone 转写完成。
	StatusDone = contract.TranscriptionDone
	// StatusFailed 转写失败（带错误码与重试入口）。
	StatusFailed = contract.TranscriptionFailed
)

// Outcome 是**单次执行**的结果，不是持久状态。
//
// 为什么需要它：`canceled`（用户取消）按 API-4.6 落库后回到 `none`；`yielded`
// （被前台占用抢占）落库为 `queued`。这两个都只是运行期结论，落库值仍是五值之一。
// 把它们做成持久状态会与 contract / TBL-03 已冻结的取值集合冲突（见
// docs/DEV-ISSUES-S4.md S4-02）。
const (
	// OutcomeDone 全部句子已落库。
	OutcomeDone = "done"
	// OutcomeFailed 中途失败（音频/引擎），进度保留。
	OutcomeFailed = "failed"
	// OutcomeCanceled 用户取消；已转句子保留，落库状态回 `none`。
	OutcomeCanceled = "canceled"
	// OutcomeYielded 被前台占用抢占；进度保留，落库状态回 `queued`。
	OutcomeYielded = "yielded"
)

// normalizeStatus 把空串归一为 `none`（老库 / 未迁移行的兼容）。
func normalizeStatus(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return StatusNone
	}
	return s
}

// CanStart 报告当前转写状态是否允许发起（或重转）任务。
//
// API-4.5：`none / failed / done → queued`；`queued / running` 期间再来一次
// 属于「同一会话两个任务」，必须拒绝（ERR_STATE_FORBIDDEN）。
func CanStart(current string) bool {
	switch normalizeStatus(current) {
	case StatusNone, StatusFailed, StatusDone:
		return true
	default:
		return false
	}
}

// IsBusy 报告是否已有排队中或运行中的任务。
func IsBusy(current string) bool {
	switch normalizeStatus(current) {
	case StatusQueued, StatusRunning:
		return true
	default:
		return false
	}
}

// CanExecute 是**执行侧**准入：除了 `running` 之外的任何状态都可以跑。
//
// 与 CanStart 的区别很关键：`queued` 对**用户发起**是拒绝的（同一会话不要排两个任务），
// 但对**调度器回收执行**必须放行——让位后的任务正是以 `queued` 状态等待重跑，
// 若执行侧也拒绝 queued，任务会永远在「排队 → 被拒 → 再排队」之间打转。
func CanExecute(current string) bool {
	return normalizeStatus(current) != StatusRunning
}

// IsFinished 报告该状态是否已经是一轮任务跑完的结果。
func IsFinished(current string) bool {
	switch normalizeStatus(current) {
	case StatusDone, StatusFailed:
		return true
	default:
		return false
	}
}

// ShouldRestart 报告本次发起是否等价「重转」。
//
// `done` 表示这一轮已全部落库，再点一次就是整段重转：进度必须归零，
// 否则断点续跑会认为「全都转完了」，一次引擎调用都不发。
func ShouldRestart(current string) bool {
	return normalizeStatus(current) == StatusDone
}

// startState 构造「已接受任务」的持久状态：入队。
func startState(current string) (string, error) {
	if !CanStart(current) {
		return normalizeStatus(current), apierr.New(apierr.CodeStateForbidden).
			WithAction("wait-transcription").
			WithDetail("会话已有排队或运行中的转写任务")
	}
	return StatusQueued, nil
}

// cancelState 返回取消后的持久状态（API-4.6：回 `none`，已转句子保留）。
func cancelState() string { return StatusNone }

// ensureNotRecordingState 是给 L3 的自检帮手：确认我们**没有**去改录制主状态。
//
// 它不做任何写入，只在实现审查/测试里用来断言「转写轴的字符串集合与
// 录制主状态集合无交集」——两个集合一旦出现同名值（例如有人把
// `transcription_status` 写成 `completed`），正交性就被破坏了。
func ensureNotRecordingState(status string) bool {
	switch normalizeStatus(status) {
	case StatusNone, StatusQueued, StatusRunning, StatusDone, StatusFailed:
		// `failed` 与录制主状态 ST-09 同名但属于不同列，这是文档允许的
		// （状态轴自身取值），此处只保证调用方用的是本轴取值。
		return true
	default:
		return false
	}
}

// newState 用会话行 + 运行期计数构造对外 DTO（EV-16 / API-4.5~4.7）。
//
// Trigger 为空表示「查询而非本次发起」，前端据此决定是否显示触发来源。
func newState(s SessionRow, trigger string, segments int) contract.TranscriptionState {
	st := contract.TranscriptionState{
		SessionID: s.ID,
		Status:    normalizeStatus(s.TranscriptionStatus),
		ProgressMs: s.TranscribeProgressMs,
		AudioMs:   s.DurationMs,
		Segments:  segments,
		EngineID:  s.TranscribeEngineID,
		Trigger:   trigger,
	}
	if s.TranscribedAt > 0 {
		st.FinishedAt = s.TranscribedAt
	}
	return st
}

// StateOf 是对外的状态快照构造器（L3 的 GetTranscription 用）。
//
// segments 传会话行的冗余计数即可（`sessions.segment_count`），避免每次查询都数一遍片段。
func StateOf(s SessionRow, trigger string, segments int) contract.TranscriptionState {
	return newState(s, trigger, segments)
}
