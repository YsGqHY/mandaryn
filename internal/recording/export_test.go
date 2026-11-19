package recording

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/capture"
	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/storage"
)

// TestExportAudioCopiesWithoutTouchingDB 断言导出：产出可用文件、**不改库记录**。
func TestExportAudioCopiesWithoutTouchingDB(t *testing.T) {
	fake := newFakeChannel(capture.AudioFormat{SampleRate: 16000, Channels: 1}, fillConst(0.25))
	fake.delay = 200 * time.Microsecond
	rec := newTestRecorder(t, func(o *Options) {
		o.OpenChannel = func(string) (capture.Channel, error) { return fake, nil }
	})
	ctx := context.Background()

	if _, err := rec.Start(contract.StartRequest{SourceID: "dev:a", Title: "导出的会话"}); err != nil {
		t.Fatalf("开始失败：%v", err)
	}
	waitFor(t, 5*time.Second, "录到 1.2s", func() bool { return rec.State().ElapsedMs >= 1200 })
	st, err := rec.Stop()
	if err != nil {
		t.Fatalf("停止失败：%v", err)
	}

	before := getSession(t, rec, st.SessionID)
	target := filepath.Join(t.TempDir(), "导出.wav")

	got, err := rec.ExportAudio(ctx, st.SessionID, target)
	if err != nil {
		t.Fatalf("导出失败：%v", err)
	}
	if _, statErr := os.Stat(got); statErr != nil {
		t.Fatalf("导出文件不存在：%v", statErr)
	}

	// 导出文件与源文件内容一致（逐字节复制）。
	src := mustAudioPath(t, rec, before.AudioPath)
	srcBytes, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("读源文件失败：%v", err)
	}
	outBytes, err := os.ReadFile(got)
	if err != nil {
		t.Fatalf("读导出文件失败：%v", err)
	}
	if len(srcBytes) != len(outBytes) {
		t.Fatalf("导出文件长度 %d ≠ 源文件 %d", len(outBytes), len(srcBytes))
	}

	info, err := InspectWAV(got)
	if err != nil {
		t.Fatalf("导出文件不是合法 WAV：%v", err)
	}
	if info.DurationMs() != before.DurationMs {
		t.Fatalf("导出文件时长 %dms ≠ 会话时长 %dms", info.DurationMs(), before.DurationMs)
	}

	// **不改库**（FR-03-11）：导出前后会话行完全一致。
	after := getSession(t, rec, st.SessionID)
	if after.SizeBytes != before.SizeBytes || after.DurationMs != before.DurationMs ||
		after.Status != before.Status || after.AudioPath != before.AudioPath ||
		after.UpdatedAt != before.UpdatedAt {
		t.Fatalf("导出不应改动数据库记录：\nbefore=%+v\nafter=%+v", before, after)
	}
}

// TestExportAudioRepairsHeaderOnCopy 断言源文件头未回填时，只在**副本**上补头。
func TestExportAudioRepairsHeaderOnCopy(t *testing.T) {
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

	const id = "s_EXPORTCRASH000000000000000"
	rel := "sessions/" + id + "/" + AudioFileName
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("创建目录失败：%v", err)
	}
	writeCrashedWAV(t, abs, 16000)

	if err := store.CreateSession(ctx, &storage.Session{
		ID: id, Title: "崩溃未修", SourceID: "dev:a", Status: contract.StateInterrupted,
		StartedAt: 1_700_000_000_000, AudioPath: rel, AudioFormat: AudioFormatWAV,
		TranscriptionStatus: contract.TranscriptionNone,
	}); err != nil {
		t.Fatalf("写入会话失败：%v", err)
	}
	srcBefore, err := os.Stat(abs)
	if err != nil {
		t.Fatalf("取源文件信息失败：%v", err)
	}

	target := filepath.Join(t.TempDir(), "fixed.wav")
	got, err := rec.ExportAudio(ctx, id, target)
	if err != nil {
		t.Fatalf("导出失败：%v", err)
	}
	info, err := InspectWAV(got)
	if err != nil {
		t.Fatalf("导出文件不是合法 WAV：%v", err)
	}
	if int64(info.DataBytes) != info.DataFileBytes {
		t.Fatalf("副本头未补：声明 %d ≠ 实际 %d", info.DataBytes, info.DataFileBytes)
	}
	// 源文件保持不变（修补源文件是 Recover 的职责，不是导出的副作用）。
	srcAfter, err := os.Stat(abs)
	if err != nil {
		t.Fatalf("取源文件信息失败：%v", err)
	}
	if srcBefore.Size() != srcAfter.Size() || !srcBefore.ModTime().Equal(srcAfter.ModTime()) {
		t.Fatal("导出不应修改源音频文件")
	}
}

func TestExportFLACKeepsOriginalFormat(t *testing.T) {
	root := t.TempDir()
	store := newTestStore(t)
	rec, err := New(Options{Store: store, DataRoot: func() (string, error) { return root, nil }})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const id = "s_FLACEXPORT000000000000000"
	const rel = "audio.flac"
	src := filepath.Join(root, rel)
	// Encoding/decoding is exercised separately; export must preserve bytes exactly.
	want := []byte("fLaC-original-bytes")
	if err := os.WriteFile(src, want, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(ctx, &storage.Session{ID: id, AudioPath: rel, AudioFormat: AudioFormatFLAC}); err != nil {
		t.Fatal(err)
	}
	if _, err := rec.ExportAudio(ctx, id, filepath.Join(root, "wrong.wav")); !apierr.HasCode(err, apierr.CodeInvalidArgument) {
		t.Fatalf("mismatched extension: %v", err)
	}
	if _, err := rec.ExportAudio(ctx, id, src); !apierr.HasCode(err, apierr.CodeInvalidArgument) {
		t.Fatalf("same file: %v", err)
	}
	path, err := rec.ExportAudio(ctx, id, filepath.Join(root, "copy.flac"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{path, src} {
		if got, err := os.ReadFile(name); err != nil || !bytes.Equal(got, want) {
			t.Fatalf("copy/source differs: %v", err)
		}
	}
}

// TestExportAudioRejectsUnsupported 断言格式与参数的拒绝路径都有明确说明。
func TestExportAudioRejectsUnsupported(t *testing.T) {
	rec := newTestRecorder(t, nil)
	ctx := context.Background()

	t.Run("空 sessionId", func(t *testing.T) {
		if _, err := rec.ExportAudio(ctx, "", "a.wav"); !apierr.HasCode(err, apierr.CodeInvalidArgument) {
			t.Fatalf("应返回 ERR_INVALID_ARGUMENT，实际 %v", err)
		}
	})
	t.Run("空 targetPath", func(t *testing.T) {
		_, err := rec.ExportAudio(ctx, "s_x", "")
		if !apierr.HasCode(err, apierr.CodeInvalidArgument) {
			t.Fatalf("应返回 ERR_INVALID_ARGUMENT，实际 %v", err)
		}
		if !contains(mustDetail(t, err), "另存为") {
			t.Fatalf("应说明需要前端弹原生另存为，实际 %q", mustDetail(t, err))
		}
	})
	t.Run("未知扩展名", func(t *testing.T) {
		if _, err := rec.ExportAudio(ctx, "s_x", "a.mp3"); !apierr.HasCode(err, apierr.CodeInvalidArgument) {
			t.Fatalf("应返回 ERR_INVALID_ARGUMENT，实际 %v", err)
		}
	})
	t.Run("无扩展名", func(t *testing.T) {
		if _, err := rec.ExportAudio(ctx, "s_x", "a"); !apierr.HasCode(err, apierr.CodeInvalidArgument) {
			t.Fatalf("应返回 ERR_INVALID_ARGUMENT，实际 %v", err)
		}
	})
	t.Run("会话不存在", func(t *testing.T) {
		if _, err := rec.ExportAudio(ctx, "s_nope", filepath.Join(t.TempDir(), "a.wav")); !apierr.HasCode(err, apierr.CodeNotFound) {
			t.Fatalf("应返回 ERR_NOT_FOUND，实际 %v", err)
		}
	})
}

// TestExportFileName 断言导出文件名清洗掉非法字符（标题来自源应用名，可能含 : / 等）。
func TestExportFileName(t *testing.T) {
	cases := []struct {
		title string
		want  string
	}{
		{"2026-11-11 15:04 · KOOK", "2026-11-11 15_04 · KOOK.wav"},
		{"a/b\\c:d*e?f\"g<h>i|j", "a_b_c_d_e_f_g_h_i_j.wav"},
		{"", "s_idle.wav"},
	}
	for _, c := range cases {
		sess := contract.Session{ID: "s_idle", Title: c.title}
		if got := ExportFileName(sess); got != c.want {
			t.Fatalf("ExportFileName(%q)=%q，期望 %q", c.title, got, c.want)
		}
	}
}

func mustDetail(t *testing.T, err error) string {
	t.Helper()
	var e *apierr.Err
	if !asErr(err, &e) {
		t.Fatalf("错误应为 *apierr.Err：%v", err)
	}
	return e.Detail
}
