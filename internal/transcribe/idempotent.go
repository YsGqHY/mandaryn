package transcribe

import (
	"sync"

	"vip.oberon.mandaryn/internal/apierr"
)

// 幂等覆盖（ARCH §6.4 / §4.5.2）。
//
// 两条不变量：
//  1. 片段键是 `(session_id, seq)`：重转必须**覆盖**同一行，不能新增行。
//     落库侧靠 TBL-04 的 `uq_segments_session_seq` 唯一索引 + upsert 实现
//     （见 transcriptsvc 的 store 实现），本文件只负责「编号稳定」与「写入前断言」。
//  2. 同一会话**同时只允许一个转写任务**：第二条并发请求返回 ERR_STATE_FORBIDDEN。
//
// 为什么编号稳定性由 L2 负责：seq 取自**确定性分句**的句子序号（Utterance.Index），
// 而不是「本次已写入条数」。后者在断点续跑时会从 0 重新数，把旧片段全部覆盖成
// 错位内容——这正是幂等覆盖最隐蔽的破坏方式。

// SessionLock 是会话级单飞行锁（进程内）。
//
// 与持久状态 `transcription_status` 的关系：持久状态是**用户可见**的真相，
// 但它在「入队 → 真正开始执行」之间有一段时间窗，同一进程内并发两次发起时
// 光靠状态字段会出现竞态（两个请求都读到 none）。因此这里再压一层内存锁，
// 保证同一会话同时只有一个执行者。
type SessionLock struct {
	mu    sync.Mutex
	held  map[string]struct{}
}

// NewSessionLock 构造会话锁。
func NewSessionLock() *SessionLock {
	return &SessionLock{held: make(map[string]struct{})}
}

// TryAcquire 尝试占用会话；已占用返回 false（调用方映射为 ERR_STATE_FORBIDDEN）。
func (l *SessionLock) TryAcquire(sessionID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.held[sessionID]; ok {
		return false
	}
	l.held[sessionID] = struct{}{}
	return true
}

// Release 释放会话占用；幂等。
func (l *SessionLock) Release(sessionID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.held, sessionID)
}

// Held 报告该会话当前是否被占用。
func (l *SessionLock) Held(sessionID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.held[sessionID]
	return ok
}

// HeldCount 返回当前占用数（测试与诊断用）。
func (l *SessionLock) HeldCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.held)
}

// errBusy 构造「已有转写任务」错误。
//
// 复用 ERR_STATE_FORBIDDEN：错误码表里没有专门的「任务忙」码，
// 语义上「当前状态不允许该操作」最贴近（见 docs/DEV-ISSUES-S4.md S4-03）。
func errBusy(sessionID string) *apierr.Err {
	return apierr.New(apierr.CodeStateForbidden).
		WithAction("wait-transcription").
		WithDetail("会话 %s 已有排队或运行中的转写任务", sessionID)
}

// errYielded 是内部信号：被前台占用抢占，需要让位（不是用户可见错误）。
//
// 用哨兵错误而不是返回值穿透多层：执行循环里句间检查点很多（每句一次），
// 让位要能立刻中断当前句之后的所有处理，又不能被当成失败。
var errYielded = apierr.New(apierr.CodePerfSuspended).
	WithAction("wait-idle").
	WithDetail("整机空闲度不足，转写让位并保留进度")

// errCanceled 是内部信号：用户取消。
var errCanceled = apierr.New(apierr.CodeStateForbidden).
	WithAction("restart-transcription").
	WithDetail("转写任务已取消，已转句子保留")

// assertSegmentOrder 校验片段写入的时间轴不变量（ARCH §4.5.2）。
//
// 前端歌词式回放靠二分定位，前提是 `seq` 升序且 `startMs` 单调不减；
// 一旦后端写出乱序数据，表现是「点击某句跳到别处」，很难回溯，所以在写入前断言。
//
// prev 为 nil 表示这是本次执行的第一句。
func assertSegmentOrder(prev *SegmentRow, next SegmentRow) error {
	if next.StartMs > next.EndMs {
		return apierr.New(apierr.CodeInvalidArgument).
			WithDetail("片段区间非法：startMs=%d > endMs=%d", next.StartMs, next.EndMs)
	}
	if next.EndMs < 0 || next.StartMs < 0 {
		return apierr.New(apierr.CodeInvalidArgument).
			WithDetail("片段区间为负")
	}
	if prev == nil {
		return nil
	}
	if next.Seq <= prev.Seq {
		return apierr.New(apierr.CodeInvalidArgument).
			WithDetail("片段 seq 未递增：prev=%d next=%d", prev.Seq, next.Seq)
	}
	if next.StartMs < prev.StartMs {
		return apierr.New(apierr.CodeInvalidArgument).
			WithDetail("片段 startMs 回退：prev=%d next=%d", prev.StartMs, next.StartMs)
	}
	return nil
}

// segmentFromUtterance 把分句结果 + 引擎结果组装成片段行。
//
// 空文本不落库（静音误判 / 引擎返回空串）：保留空片段只会让回放出现空行，
// 且会白白占用 seq。调用方需要据此决定是否推进进度。
func segmentFromUtterance(sessionID string, u Utterance, engineID, text string, confidence float32) SegmentRow {
	return SegmentRow{
		SessionID:  sessionID,
		Seq:        u.Index,
		StartMs:    u.StartMs,
		EndMs:      u.EndMs,
		Text:       text,
		EngineID:   engineID,
		Confidence: confidence,
		IsFinal:    true,
	}
}
