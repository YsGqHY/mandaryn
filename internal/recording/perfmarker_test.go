package recording

import (
	"os"
	"testing"
	"time"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/capture"
	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/storage"
)

// ——— MarkPerf（NFR-08 档位 → 会话时间轴）———

// perfEvents 读出该会话的预算标记（`kind` 以 perf- 开头）。
func perfEvents(t *testing.T, rec *Recorder, sessionID string) []storage.SessionEvent {
	t.Helper()
	store, ok := rec.opts.Store.(*GORMStore)
	if !ok {
		t.Fatalf("测试需要 GORMStore，实际 %T", rec.opts.Store)
	}
	var out []storage.SessionEvent
	if err := store.holder.Current().GORM.
		Where("session_id = ? AND kind LIKE ?", sessionID, "perf-%").
		Order("id ASC").Find(&out).Error; err != nil {
		t.Fatalf("读取预算标记失败：%v", err)
	}
	return out
}

// allEvents 读出该会话的全部时间轴标记（用于断言「零写入」）。
func allEvents(t *testing.T, rec *Recorder, sessionID string) []storage.SessionEvent {
	t.Helper()
	store, ok := rec.opts.Store.(*GORMStore)
	if !ok {
		t.Fatalf("测试需要 GORMStore，实际 %T", rec.opts.Store)
	}
	var out []storage.SessionEvent
	if err := store.holder.Current().GORM.
		Where("session_id = ?", sessionID).Find(&out).Error; err != nil {
		t.Fatalf("读取时间轴标记失败：%v", err)
	}
	return out
}

// audioFileSize 返回落盘音频文件当前大小（读磁盘，不碰内存监控器）。
func audioFileSize(t *testing.T, rec *Recorder, rel string) int64 {
	t.Helper()
	info, err := os.Stat(mustAudioPath(t, rec, rel))
	if err != nil {
		t.Fatalf("读取音频文件状态失败：%v", err)
	}
	return info.Size()
}

// TestMarkPerfWritesFrameBasedAtMs 断言标记写完、`atMs` 等于当时的帧计数换算值。
//
// 断言时先暂停并等时长稳定：这样「读到的 ElapsedMs」与「MarkPerf 内部读到的
// writer.DurationMs()」必然同值，能把「时间基是帧计数而不是墙钟」钉死。
// 若实现改用墙钟，暂停时段会让标记漂移，本用例立刻失败。
func TestMarkPerfWritesFrameBasedAtMs(t *testing.T) {
	fake := newFakeChannel(capture.AudioFormat{SampleRate: 16000, Channels: 1}, fillConst(0.3))
	fake.delay = 200 * time.Microsecond
	rec := newTestRecorder(t, func(o *Options) {
		o.OpenChannel = func(string) (capture.Channel, error) { return fake, nil }
	})

	st, err := rec.Start(contract.StartRequest{SourceID: "dev:a"})
	if err != nil {
		t.Fatalf("开始录制失败：%v", err)
	}
	sessionID := st.SessionID

	waitFor(t, 5*time.Second, "录到 400ms", func() bool { return rec.State().ElapsedMs >= 400 })
	// 暂停 + 等稳定：此后时间轴不再推进，帧计数与墙上时间解耦，正好验证时间基。
	if _, err := rec.Pause(); err != nil {
		t.Fatalf("暂停失败：%v", err)
	}
	stableAt := waitStableElapsed(t, rec, rec.State().ElapsedMs)

	if err := rec.MarkPerf(contract.MarkerPerfThrottle, "预算降档 L2"); err != nil {
		t.Fatalf("MarkPerf 失败：%v", err)
	}

	events := perfEvents(t, rec, sessionID)
	if len(events) != 1 {
		t.Fatalf("应写入 1 条预算标记，实际 %d 条", len(events))
	}
	ev := events[0]
	if ev.Kind != contract.MarkerPerfThrottle {
		t.Fatalf("kind 应为 %s，实际 %s", contract.MarkerPerfThrottle, ev.Kind)
	}
	if ev.AtMs != stableAt {
		t.Fatalf("atMs 应为帧计数换算值 %d，实际 %d（用墙钟会得到别的时间）", stableAt, ev.AtMs)
	}
	if ev.Note == "" {
		t.Fatal("note 应保留，便于回放里说明原因")
	}
	if got := rec.State().ElapsedMs; got != stableAt {
		t.Fatalf("预算标记不得推进时间轴：%d → %d", stableAt, got)
	}

	if _, err := rec.Stop(); err != nil {
		t.Fatalf("停止失败：%v", err)
	}
}

// TestMarkPerfRejectsUnknownKind 断言非法 kind 返回 ERR_INVALID_ARGUMENT 且不写库。
func TestMarkPerfRejectsUnknownKind(t *testing.T) {
	fake := newFakeChannel(capture.AudioFormat{SampleRate: 16000, Channels: 1}, fillConst(0.3))
	fake.delay = 200 * time.Microsecond
	rec := newTestRecorder(t, func(o *Options) {
		o.OpenChannel = func(string) (capture.Channel, error) { return fake, nil }
	})

	st, err := rec.Start(contract.StartRequest{SourceID: "dev:a"})
	if err != nil {
		t.Fatalf("开始录制失败：%v", err)
	}
	defer func() { _, _ = rec.Stop() }()

	bad := []string{"", "perf-unknown", "perf_throttle", contract.MarkerPaused, "PERF-THROTTLE"}
	for _, kind := range bad {
		err := rec.MarkPerf(kind, "x")
		if !apierr.HasCode(err, apierr.CodeInvalidArgument) {
			t.Fatalf("kind=%q 应返回 ERR_INVALID_ARGUMENT，实际 %v", kind, err)
		}
	}
	if evs := allEvents(t, rec, st.SessionID); len(evs) != 0 {
		t.Fatalf("非法 kind 不得写库，实际 %d 条", len(evs))
	}
}

// TestMarkPerfSilentWithoutActiveSession 断言无活跃会话时返回 nil 且零写入。
func TestMarkPerfSilentWithoutActiveSession(t *testing.T) {
	rec := newTestRecorder(t, nil)

	// 1) 从未录制。
	if err := rec.MarkPerf(contract.MarkerPerfSuspend, "预算耗尽"); err != nil {
		t.Fatalf("空闲时 MarkPerf 应返回 nil，实际 %v", err)
	}

	// 2) 录完并封存之后：run 仍在（用于状态快照）但已 finished，必须同样静默跳过。
	fake := newFakeChannel(capture.AudioFormat{SampleRate: 16000, Channels: 1}, fillConst(0.3))
	fake.delay = 200 * time.Microsecond
	rec2 := newTestRecorder(t, func(o *Options) {
		o.OpenChannel = func(string) (capture.Channel, error) { return fake, nil }
	})
	if _, err := rec2.Start(contract.StartRequest{SourceID: "dev:a"}); err != nil {
		t.Fatalf("开始录制失败：%v", err)
	}
	waitFor(t, 5*time.Second, "录到 1.1s", func() bool { return rec2.State().ElapsedMs >= 1100 })
	finished, err := rec2.Stop()
	if err != nil {
		t.Fatalf("停止失败：%v", err)
	}
	before := len(allEvents(t, rec2, finished.SessionID))
	if err := rec2.MarkPerf(contract.MarkerPerfResume, "预算恢复"); err != nil {
		t.Fatalf("封存后 MarkPerf 应返回 nil，实际 %v", err)
	}
	after := len(allEvents(t, rec2, finished.SessionID))
	if after != before {
		t.Fatalf("封存后不得写入标记：%d → %d", before, after)
	}
}

// TestMarkPerfThreeKindsCoexist 断言三种预算标记能同时存在且顺序按写入顺序。
func TestMarkPerfThreeKindsCoexist(t *testing.T) {
	fake := newFakeChannel(capture.AudioFormat{SampleRate: 16000, Channels: 1}, fillConst(0.3))
	fake.delay = 200 * time.Microsecond
	rec := newTestRecorder(t, func(o *Options) {
		o.OpenChannel = func(string) (capture.Channel, error) { return fake, nil }
	})

	st, err := rec.Start(contract.StartRequest{SourceID: "dev:a"})
	if err != nil {
		t.Fatalf("开始录制失败：%v", err)
	}
	waitFor(t, 5*time.Second, "录到 300ms", func() bool { return rec.State().ElapsedMs >= 300 })

	want := []string{
		contract.MarkerPerfThrottle,
		contract.MarkerPerfSuspend,
		contract.MarkerPerfResume,
	}
	for _, kind := range want {
		if err := rec.MarkPerf(kind, "档位变化"); err != nil {
			t.Fatalf("MarkPerf(%s) 失败：%v", kind, err)
		}
	}

	events := perfEvents(t, rec, st.SessionID)
	if len(events) != len(want) {
		t.Fatalf("应有 %d 条预算标记，实际 %d 条", len(want), len(events))
	}
	prev := int64(-1)
	for i, ev := range events {
		if ev.Kind != want[i] {
			t.Fatalf("第 %d 条 kind 应为 %s，实际 %s", i, want[i], ev.Kind)
		}
		if ev.AtMs < prev {
			t.Fatalf("时间轴标记应单调不减：%d → %d", prev, ev.AtMs)
		}
		prev = ev.AtMs
	}

	if _, err := rec.Stop(); err != nil {
		t.Fatalf("停止失败：%v", err)
	}
}

// TestMarkPerfDoesNotTouchStateOrAudio 断言标记只写时间轴：状态、时长、音频都不受影响。
//
// 这条是 NFR-08 的硬约束在录制侧的落地：降档/暂停推理**绝不**能改动录制。
func TestMarkPerfDoesNotTouchStateOrAudio(t *testing.T) {
	fake := newFakeChannel(capture.AudioFormat{SampleRate: 16000, Channels: 1}, fillConst(0.3))
	fake.delay = 200 * time.Microsecond
	rec := newTestRecorder(t, func(o *Options) {
		o.OpenChannel = func(string) (capture.Channel, error) { return fake, nil }
	})

	st, err := rec.Start(contract.StartRequest{SourceID: "dev:a"})
	if err != nil {
		t.Fatalf("开始录制失败：%v", err)
	}
	waitFor(t, 5*time.Second, "录到 300ms", func() bool { return rec.State().ElapsedMs >= 300 })
	beforeState := rec.State().State
	beforeRow := getSession(t, rec, st.SessionID)
	// 落盘进度直接量文件大小，而不是 `rec.Metrics()`：后者会与落盘 goroutine 的
	// Monitor.Feed 竞争（见 docs/DEV-ISSUES-E-s8.md E8-06），而文件大小本身
	// 恰好是「音频还在继续写」最直接的证据。
	beforeBytes := audioFileSize(t, rec, beforeRow.AudioPath)

	for _, kind := range []string{contract.MarkerPerfThrottle, contract.MarkerPerfSuspend, contract.MarkerPerfResume} {
		if err := rec.MarkPerf(kind, "档位变化"); err != nil {
			t.Fatalf("MarkPerf(%s) 失败：%v", kind, err)
		}
	}

	if got := rec.State().State; got != beforeState {
		t.Fatalf("预算标记不得改录制状态：%s → %s", beforeState, got)
	}
	if got := audioFileSize(t, rec, beforeRow.AudioPath); got < beforeBytes {
		t.Fatalf("预算标记不得让落盘倒退：%d → %d", beforeBytes, got)
	}

	stopped, err := rec.Stop()
	if err != nil {
		t.Fatalf("停止失败：%v", err)
	}
	afterRow := getSession(t, rec, stopped.SessionID)
	if afterRow.Status != contract.StateCompleted {
		t.Fatalf("封存状态应为 completed，实际 %s", afterRow.Status)
	}
	if afterRow.DurationMs < beforeRow.DurationMs || afterRow.AudioPath != beforeRow.AudioPath {
		t.Fatalf("预算标记不得改动会话行的时长/路径：%+v → %+v", beforeRow, afterRow)
	}
	info, err := InspectWAV(mustAudioPath(t, rec, afterRow.AudioPath))
	if err != nil {
		t.Fatalf("读取落盘文件失败：%v", err)
	}
	if info.DurationMs() != afterRow.DurationMs {
		t.Fatalf("落盘时长 %dms 与库中 %dms 不一致（音频被改写了？）", info.DurationMs(), afterRow.DurationMs)
	}
}
