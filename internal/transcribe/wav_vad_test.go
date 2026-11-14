package transcribe

import (
	"math"
	"os"
	"testing"
)

// TestWAVParseAndRead 验证头解析与采样归一化。
func TestWAVParseAndRead(t *testing.T) {
	const sr = 16000
	dir := t.TempDir()
	// 用已知波形：前半 0.5、后半 -0.5（PCM16 量化误差在 1/32767 内）。
	samples := make([]float32, sr) // 1 秒
	for i := 0; i < sr/2; i++ {
		samples[i] = 0.5
	}
	for i := sr / 2; i < sr; i++ {
		samples[i] = -0.5
	}
	path := writeWAV(t, dir, samples, sr, false)

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	src, err := openWAV(f)
	if err != nil {
		t.Fatalf("parse wav: %v", err)
	}
	defer func() { _ = src.Close() }()

	if got := src.SampleRate(); got != sr {
		t.Fatalf("sample rate = %d, want %d", got, sr)
	}
	if got := src.TotalFrames(); got != int64(sr) {
		t.Fatalf("frames = %d, want %d", got, sr)
	}
	if got := src.TotalMs(); got != 1000 {
		t.Fatalf("totalMs = %d, want 1000", got)
	}

	pcm, err := src.ReadFrames(0, 160)
	if err != nil {
		t.Fatalf("read frames: %v", err)
	}
	if len(pcm) != 160 {
		t.Fatalf("read %d frames, want 160", len(pcm))
	}
	for _, v := range pcm {
		if math.Abs(float64(v)-0.5) > 1e-3 {
			t.Fatalf("sample = %v, want ≈0.5", v)
		}
	}

	// 跨中点读取：应能读到负值。
	late, err := src.ReadFrames(int64(sr-8), 8)
	if err != nil {
		t.Fatalf("read late frames: %v", err)
	}
	if len(late) == 0 || late[len(late)-1] >= 0 {
		t.Fatalf("late samples should be negative, got %v", late)
	}
}

// TestWAVSkipsExtraChunks 验证解析器按块扫描，而不是硬编码 44 字节偏移。
func TestWAVSkipsExtraChunks(t *testing.T) {
	const sr = 16000
	dir := t.TempDir()
	samples := make([]float32, sr/10) // 100ms
	for i := range samples {
		samples[i] = 0.25
	}
	path := writeWAV(t, dir, samples, sr, true) // 带 LIST 块

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	src, err := openWAV(f)
	if err != nil {
		t.Fatalf("parse wav with extra chunk: %v", err)
	}
	defer func() { _ = src.Close() }()

	if got := src.TotalFrames(); got != int64(len(samples)) {
		t.Fatalf("frames = %d, want %d（说明 LIST 块被误当成音频数据）", got, len(samples))
	}
	pcm, err := src.ReadFrames(0, 16)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for _, v := range pcm {
		if math.Abs(float64(v)-0.25) > 1e-3 {
			t.Fatalf("sample = %v, want ≈0.25（说明读到了元数据）", v)
		}
	}
}

// TestWAVRejectsNonWAV 验证非 WAV 数据给出明确错误，而不是读成噪声。
func TestWAVRejectsNonWAV(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/bogus.wav"
	if err := os.WriteFile(path, []byte("not a wave file at all"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = f.Close() }()

	if _, err := openWAV(f); err == nil {
		t.Fatal("openWAV 应当拒绝非 WAV 文件")
	}
}

// TestVADSplitTwoSentences 验证「语音 1s + 静音 0.5s + 语音 0.8s」切出 2 句。
func TestVADSplitTwoSentences(t *testing.T) {
	const sr = 16000
	audio := concat(tone(1000, sr), silence(500, sr), tone(800, sr))

	utts := SplitAll(audio, VadConfig{SampleRate: sr})
	if len(utts) != 2 {
		t.Fatalf("切出 %d 句, want 2", len(utts))
	}

	// 第一句：起点约 0，终点约 1000（+100ms padding 的静音尾巴）。
	if utts[0].StartMs > 50 {
		t.Errorf("第 1 句 startMs = %d, want ≈0", utts[0].StartMs)
	}
	if utts[0].EndMs < 950 || utts[0].EndMs > 1200 {
		t.Errorf("第 1 句 endMs = %d, want ≈1000（含 ≤100ms 尾巴）", utts[0].EndMs)
	}
	// 第二句：起点约 1500（静音结束处，允许 padding 前卷 100ms）。
	if utts[1].StartMs < 1400 || utts[1].StartMs > 1560 {
		t.Errorf("第 2 句 startMs = %d, want ≈1500", utts[1].StartMs)
	}
	if utts[1].EndMs < 2250 || utts[1].EndMs > 2400 {
		t.Errorf("第 2 句 endMs = %d, want ≈2300", utts[1].EndMs)
	}
	if utts[0].Index != 0 || utts[1].Index != 1 {
		t.Errorf("序号 = %d,%d, want 0,1", utts[0].Index, utts[1].Index)
	}
	// 时间轴单调不减（前端二分定位的前提）。
	if utts[1].StartMs < utts[0].StartMs || utts[1].EndMs < utts[0].EndMs {
		t.Errorf("时间轴非单调：%+v", utts)
	}
	// 每句的采样数与时间区间自洽（帧计数换算）。
	if got := samplesToMs(int64(len(utts[0].Samples)), sr); got != utts[0].EndMs-utts[0].StartMs {
		t.Errorf("第 1 句样本/时间不自洽：samples→%dms, 区间 %dms",
			got, utts[0].EndMs-utts[0].StartMs)
	}
}

// TestVADIgnoresShortNoise 验证过短噪声不产出句子。
func TestVADIgnoresShortNoise(t *testing.T) {
	const sr = 16000
	// 50ms 咔哒声 < MinSpeechMs(200ms)。
	audio := concat(silence(200, sr), tone(50, sr), silence(600, sr))
	if utts := SplitAll(audio, VadConfig{SampleRate: sr}); len(utts) != 0 {
		t.Fatalf("短噪声被当成句子：%d 句", len(utts))
	}
}

// TestVADSplitsLongSpeech 验证超长语音按 MaxSegmentMs 强制切分。
func TestVADSplitsLongSpeech(t *testing.T) {
	const sr = 16000
	audio := tone(3000, sr)
	utts := SplitAll(audio, VadConfig{SampleRate: sr, MaxSegmentMs: 1000})
	if len(utts) < 3 {
		t.Fatalf("3s 连续语音在 MaxSegmentMs=1000 下切出 %d 句, want ≥3", len(utts))
	}
	for _, u := range utts {
		if u.EndMs-u.StartMs > 1100 {
			t.Errorf("句子超长：%dms > 1100ms", u.EndMs-u.StartMs)
		}
	}
}

// TestVADIncrementalMatchesOneShot 验证逐窗喂入与一次性喂入结果一致
// （转写任务用前者，测试用后者；两者不一致会让「单测通过、实际跑偏」）。
func TestVADIncrementalMatchesOneShot(t *testing.T) {
	const sr = 16000
	audio := concat(tone(1000, sr), silence(500, sr), tone(800, sr))

	oneShot := SplitAll(audio, VadConfig{SampleRate: sr})

	sp := NewUtteranceSplitter(VadConfig{SampleRate: sr})
	var chunked []Utterance
	const step = 3200 // 200ms 窗，与 Runner 一致
	for i := 0; i < len(audio); i += step {
		end := i + step
		if end > len(audio) {
			end = len(audio)
		}
		chunked = append(chunked, sp.Push(audio[i:end])...)
	}
	chunked = append(chunked, sp.Flush()...)

	if len(chunked) != len(oneShot) {
		t.Fatalf("逐窗 %d 句 vs 一次性 %d 句", len(chunked), len(oneShot))
	}
	for i := range chunked {
		if chunked[i].StartMs != oneShot[i].StartMs || chunked[i].EndMs != oneShot[i].EndMs {
			t.Errorf("第 %d 句区间不一致：逐窗 [%d,%d] vs 一次性 [%d,%d]",
				i, chunked[i].StartMs, chunked[i].EndMs, oneShot[i].StartMs, oneShot[i].EndMs)
		}
	}
}
