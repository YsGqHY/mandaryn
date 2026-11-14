package transcribe

// 进度与断点续跑（FR-03-4「进度与断点续跑」）。
//
// 目标：任务被抢占 / 崩溃 / 用户取消后重新发起，**已完成的句子不重复处理**。
//
// 两个锚点：
//   - `sessions.transcribe_progress_ms`：已确认落库的音频位置（最后一句的 EndMs）。
//     它是「句子级」的，不是「帧级」的——崩溃可能丢当前句，重跑时重算这一句，
//     幂等覆盖保证不产生重复行。
//   - 分句序号 `Utterance.Index`：直接当 seq，保证重跑时同一句命中同一行。
//
// 为什么进度记 ms 而不是「已处理句数」：音频前段可能全是静音（无句子），
// 用句数会停在 0，永远重跑前段；ms 无论有没有句子都能推进。
//
// 判定规则（保守）：一句的 `EndMs <= progressMs` 视为已完成 → 跳过。
// 用 EndMs 而不是 StartMs：句子跨越断点时（EndMs > progress）必须重转，
// 否则会漏掉断点之后那部分音频的文字。

// ResumePlan 是一次执行的起点。
type ResumePlan struct {
	// ProgressMs 已确认落库的音频位置。
	ProgressMs int64
	// MaxSeq 已落库的最大 seq（-1 表示无片段）。
	MaxSeq int
	// Restart 为 true 时忽略进度从 0 重来（`done` 后重转）。
	Restart bool
}

// PlanResume 依据会话行与已落库情况计算起点。
//
// restart=true（会话已 `done`，用户点了重转）时进度归零：
// 否则「全都转完了」会让本次执行一句都不发，表现为点了重转没反应。
func PlanResume(session SessionRow, maxSeq int, restart bool) ResumePlan {
	p := ResumePlan{MaxSeq: maxSeq, Restart: restart}
	if restart {
		return p
	}
	if session.TranscribeProgressMs > 0 {
		p.ProgressMs = session.TranscribeProgressMs
	}
	return p
}

// Completed 报告该句是否已在此前执行中完成（应当跳过）。
func (p ResumePlan) Completed(u Utterance) bool {
	if p.Restart {
		return false
	}
	return u.EndMs <= p.ProgressMs
}

// BaseSeq 返回本次执行的起始 seq（仅诊断用；seq 恒取句子序号）。
//
// 之所以保留它：当历史数据里存在「上一轮 seq 比本轮句子数多」的残留行时，
// 日志里能看出错位（例如音频被替换过），便于定位而不是静默覆盖。
func (p ResumePlan) BaseSeq() int { return p.MaxSeq + 1 }

// advance 把进度推进到这一句的末尾（单调不回退）。
//
// 单调性是必须的：断点续跑期间若出现乱序（多段拼接音频），进度回退会导致
// 已完成的句子被反复重转，且 progressMs 在 UI 上会跳动。
func advance(progress, endMs int64) int64 {
	if endMs > progress {
		return endMs
	}
	return progress
}
