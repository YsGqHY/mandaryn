package recording

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/storage"
)

// TestRecoverUnfinishedRepairsCrashedSession 构造「状态 recording 但进程已死」的现场：
//
//	库里留一行 status=recording 的会话 + 磁盘上一个头里 data 长度为 0 的半截 WAV。
//
// 断言 Recover 之后：状态 interrupted、能解码出正确帧数、时长/字节数回填、
// 已产出的片段与音频都保留（不重跑转写、不删数据）。
func TestRecoverUnfinishedRepairsCrashedSession(t *testing.T) {
	root := t.TempDir()
	store := newTestStore(t)
	rec, err := New(Options{
		Store:    store,
		DataRoot: func() (string, error) { return root, nil },
	})
	if err != nil {
		t.Fatalf("构造录制器失败：%v", err)
	}
	ctx := context.Background()

	// —— 构造崩溃现场 ——
	const id = "s_CRASHED0000000000000000000"
	rel := filepath.ToSlash(filepath.Join("sessions", id, AudioFileName))
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("创建目录失败：%v", err)
	}
	const frames = 16000 // 1 秒
	writeCrashedWAV(t, abs, frames)

	row := &storage.Session{
		ID:                  id,
		Title:               "崩溃的会话",
		SourceID:            "dev:test",
		SourceName:          "测试设备",
		SourceKind:          contract.SourceKindDevice,
		ModelSource:         contract.ModelSourceLocal,
		Status:              contract.StateRecording, // 进程死在这里
		StartedAt:           1_700_000_000_000,
		AudioPath:           rel,
		AudioFormat:         AudioFormatWAV,
		TranscriptionStatus: contract.TranscriptionNone,
	}
	if err := store.CreateSession(ctx, row); err != nil {
		t.Fatalf("写入会话失败：%v", err)
	}
	// 已产出的片段：恢复不得动它（转写结果独立于音频修补）。
	seg := &storage.Segment{SessionID: id, Seq: 0, StartMs: 0, EndMs: 500, Text: "崩溃前已定稿的一句"}
	if err := store.holder.Current().GORM.Create(seg).Error; err != nil {
		t.Fatalf("写入片段失败：%v", err)
	}

	// —— 执行启动自检 ——
	results, err := rec.RecoverUnfinished(ctx)
	if err != nil {
		t.Fatalf("恢复失败：%v", err)
	}
	if len(results) != 1 {
		t.Fatalf("应恢复 1 个会话，实际 %d", len(results))
	}
	res := results[0]
	if res.Status != contract.StateInterrupted {
		t.Fatalf("恢复后状态应为 interrupted，实际 %s（err=%v）", res.Status, res.Err)
	}
	if res.PreviousStatus != contract.StateRecording {
		t.Fatalf("应记录崩溃前状态 recording，实际 %s", res.PreviousStatus)
	}
	if res.Frames != frames {
		t.Fatalf("修补后帧数 %d，期望 %d", res.Frames, frames)
	}
	if res.DurationMs != 1000 {
		t.Fatalf("修补后时长 %dms，期望 1000ms", res.DurationMs)
	}
	if res.Missing {
		t.Fatal("音频存在时不应标 Missing")
	}

	// 库行已更新。
	updated, err := store.GetSession(ctx, id)
	if err != nil {
		t.Fatalf("读取会话失败：%v", err)
	}
	if updated.Status != contract.StateInterrupted {
		t.Fatalf("库中状态应为 interrupted，实际 %s", updated.Status)
	}
	if updated.DurationMs != 1000 {
		t.Fatalf("库中时长 %dms，期望 1000ms", updated.DurationMs)
	}
	if updated.EndedAt == 0 {
		t.Fatal("应补出 ended_at（started_at + 时长）")
	}
	if updated.TranscriptionStatus != contract.TranscriptionNone {
		t.Fatalf("恢复不得改转写状态轴，实际 %s", updated.TranscriptionStatus)
	}

	// WAV 可解码：头里的 data 长度 == 实际数据长度。
	info, err := InspectWAV(abs)
	if err != nil {
		t.Fatalf("修补后仍无法解析 WAV：%v", err)
	}
	if int64(info.DataBytes) != info.DataFileBytes {
		t.Fatalf("修补后头声明 %d ≠ 实际 %d", info.DataBytes, info.DataFileBytes)
	}
	if info.DurationMs() != 1000 {
		t.Fatalf("修补后可解码时长 %dms，期望 1000ms", info.DurationMs())
	}

	// 片段保留。
	var segCount int64
	if err := store.holder.Current().GORM.Model(&storage.Segment{}).
		Where("session_id = ?", id).Count(&segCount).Error; err != nil {
		t.Fatalf("统计片段失败：%v", err)
	}
	if segCount != 1 {
		t.Fatalf("恢复不得删除已产出片段，实际 %d 条", segCount)
	}

	// 时间轴标记：崩溃点必须回放可见。
	var markers []storage.SessionEvent
	if err := store.holder.Current().GORM.Where("session_id = ?", id).Find(&markers).Error; err != nil {
		t.Fatalf("读取标记失败：%v", err)
	}
	if len(markers) == 0 {
		t.Fatal("恢复应写入一条时间轴标记")
	}

	// 幂等：再跑一次不会把状态改回去，也不会重复报。
	again, err := rec.RecoverUnfinished(ctx)
	if err != nil {
		t.Fatalf("二次恢复失败：%v", err)
	}
	if len(again) != 0 {
		t.Fatalf("interrupted 的会话不应再次被扫到，实际 %d 个", len(again))
	}
}

// TestRecoverUnfinishedMarksMissingAudioFailed 断言音频文件缺失时标 failed（保留片段）。
func TestRecoverUnfinishedMarksMissingAudioFailed(t *testing.T) {
	root := t.TempDir()
	store := newTestStore(t)
	rec, err := New(Options{
		Store:    store,
		DataRoot: func() (string, error) { return root, nil },
	})
	if err != nil {
		t.Fatalf("构造录制器失败：%v", err)
	}
	ctx := context.Background()

	row := &storage.Session{
		ID: "s_MISSINGAUDIO0000000000000", Title: "音频丢了",
		SourceID: "dev:test", Status: contract.StateFinalizing,
		StartedAt:   1_700_000_000_000,
		AudioPath:   "sessions/s_MISSINGAUDIO0000000000000/audio.wav",
		AudioFormat: AudioFormatWAV, TranscriptionStatus: contract.TranscriptionNone,
	}
	if err := store.CreateSession(ctx, row); err != nil {
		t.Fatalf("写入会话失败：%v", err)
	}

	results, err := rec.RecoverUnfinished(ctx)
	if err != nil {
		t.Fatalf("恢复失败：%v", err)
	}
	if len(results) != 1 {
		t.Fatalf("应恢复 1 个会话，实际 %d", len(results))
	}
	if !results[0].Missing {
		t.Fatal("音频缺失应标 Missing")
	}
	if results[0].Status != contract.StateFailed {
		t.Fatalf("音频缺失应标 failed，实际 %s", results[0].Status)
	}
	updated, _ := store.GetSession(ctx, row.ID)
	if updated.Status != contract.StateFailed {
		t.Fatalf("库中状态应为 failed，实际 %s", updated.Status)
	}
	if updated.DegradeReason == "" {
		t.Fatal("应写明音频缺失原因")
	}
}

// TestRecoverNoUnfinishedIsNoop 断言没有残留会话时什么都不做（启动流程安静）。
func TestRecoverNoUnfinishedIsNoop(t *testing.T) {
	rec := newTestRecorder(t, nil)
	results, err := rec.RecoverUnfinished(context.Background())
	if err != nil {
		t.Fatalf("恢复失败：%v", err)
	}
	if len(results) != 0 {
		t.Fatalf("空库不应恢复任何会话，实际 %d", len(results))
	}
}

// TestRecoverSessionByID 验证 API-2.6 的单会话版本。
func TestRecoverSessionByID(t *testing.T) {
	root := t.TempDir()
	store := newTestStore(t)
	rec, err := New(Options{
		Store:    store,
		DataRoot: func() (string, error) { return root, nil },
	})
	if err != nil {
		t.Fatalf("构造录制器失败：%v", err)
	}
	ctx := context.Background()

	const id = "s_SINGLE0000000000000000000"
	rel := "sessions/" + id + "/" + AudioFileName
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("创建目录失败：%v", err)
	}
	writeCrashedWAV(t, abs, 32000) // 2 秒

	if err := store.CreateSession(ctx, &storage.Session{
		ID: id, Title: "待修复", SourceID: "dev:a", Status: contract.StatePaused,
		StartedAt: 1_700_000_000_000, AudioPath: rel, AudioFormat: AudioFormatWAV,
		TranscriptionStatus: contract.TranscriptionNone,
	}); err != nil {
		t.Fatalf("写入会话失败：%v", err)
	}

	sess, err := rec.RecoverSession(ctx, id)
	if err != nil {
		t.Fatalf("修复单会话失败：%v", err)
	}
	if sess.Status != contract.StateInterrupted {
		t.Fatalf("状态应为 interrupted，实际 %s", sess.Status)
	}
	if sess.DurationMs != 2000 {
		t.Fatalf("时长应为 2000ms，实际 %d", sess.DurationMs)
	}
	if sess.StartedAt == "" {
		t.Fatal("DTO 应带 startedAt")
	}
	if sess.EndedAt == "" {
		t.Fatal("DTO 应带 endedAt（由 startedAt + 时长推出）")
	}

	if _, err := rec.RecoverSession(ctx, "s_NOTEXIST"); !apierr.HasCode(err, apierr.CodeNotFound) {
		t.Fatalf("不存在的会话应返回 ERR_NOT_FOUND，实际 %v", err)
	}
}

// writeCrashedWAV 写一个「崩溃现场」文件：合法头但 data 长度为 0，后面跟真实数据。
func writeCrashedWAV(t *testing.T, path string, frames int) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("创建文件失败：%v", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(buildWAVHeader(InternalSampleRate, InternalChannels, InternalBitsPerSample, 0)); err != nil {
		t.Fatalf("写头失败：%v", err)
	}
	data := make([]byte, frames*2)
	for i := 0; i < frames; i++ {
		binary.LittleEndian.PutUint16(data[i*2:], uint16(int16(2000)))
	}
	if _, err := f.Write(data); err != nil {
		t.Fatalf("写数据失败：%v", err)
	}
}
