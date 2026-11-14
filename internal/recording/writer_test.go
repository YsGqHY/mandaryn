package recording

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// TestWriterWAVHeader 断言落盘文件的头字段全部正确，且帧计数换算的时长与样本数一致。
func TestWriterWAVHeader(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audio.wav")
	w, err := CreateWriter(path)
	if err != nil {
		t.Fatalf("创建 WAV 失败：%v", err)
	}

	// 写入 1 秒的 440Hz 正弦（16kHz）。
	const frames = InternalSampleRate
	samples := make([]float32, frames)
	for i := range samples {
		samples[i] = 0.5 * float32(math.Sin(2*math.Pi*440*float64(i)/float64(InternalSampleRate)))
	}
	if err := w.Write(samples); err != nil {
		t.Fatalf("写入样本失败：%v", err)
	}
	if err := w.Seal(); err != nil {
		t.Fatalf("封口失败：%v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("关闭失败：%v", err)
	}

	info, err := InspectWAV(path)
	if err != nil {
		t.Fatalf("读取 WAV 头失败：%v", err)
	}
	if info.SampleRate != InternalSampleRate {
		t.Fatalf("采样率 %d，期望 %d", info.SampleRate, InternalSampleRate)
	}
	if info.Channels != InternalChannels {
		t.Fatalf("声道数 %d，期望 %d", info.Channels, InternalChannels)
	}
	if info.BitsPerSample != InternalBitsPerSample {
		t.Fatalf("位深 %d，期望 %d", info.BitsPerSample, InternalBitsPerSample)
	}
	wantData := uint32(frames * 2)
	if info.DataBytes != wantData {
		t.Fatalf("data 长度 %d，期望 %d", info.DataBytes, wantData)
	}
	if info.FileBytes != int64(wavHeaderSize)+int64(wantData) {
		t.Fatalf("文件长度 %d，期望 %d", info.FileBytes, wavHeaderSize+int(wantData))
	}
	if info.Frames() != frames {
		t.Fatalf("头里声明的帧数 %d，期望 %d", info.Frames(), frames)
	}
	// 帧计数换算的时长必须与样本数严格一致（1 秒）。
	if got := info.DurationMs(); got != 1000 {
		t.Fatalf("头换算时长 %dms，期望 1000ms", got)
	}
	if got := w.DurationMs(); got != 1000 {
		t.Fatalf("writer 时长 %dms，期望 1000ms", got)
	}

	// 逐样本抽查解码结果（16bit 量化误差 ≤ 1 LSB）。
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读文件失败：%v", err)
	}
	for _, i := range []int{0, 17, 100, frames - 1} {
		got := pcm16ToFloat(int16(binary.LittleEndian.Uint16(raw[wavHeaderSize+i*2:])))
		want := samples[i]
		if math.Abs(float64(got-want)) > 1.0/32767 {
			t.Fatalf("第 %d 个样本读回 %.6f，期望 %.6f", i, got, want)
		}
	}
}

// TestWriterSealIsIdempotent 断言重复封口不会破坏长度字段（收尾与 Close 都会封）。
func TestWriterSealIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audio.wav")
	w, err := CreateWriter(path)
	if err != nil {
		t.Fatalf("创建 WAV 失败：%v", err)
	}
	if err := w.Write(make([]float32, 1600)); err != nil {
		t.Fatalf("写入失败：%v", err)
	}
	for i := 0; i < 3; i++ {
		if err := w.Seal(); err != nil {
			t.Fatalf("第 %d 次封口失败：%v", i+1, err)
		}
	}
	_ = w.Close()
	info, err := InspectWAV(path)
	if err != nil {
		t.Fatalf("读取 WAV 头失败：%v", err)
	}
	if info.DataBytes != 3200 {
		t.Fatalf("data 长度 %d，期望 3200", info.DataBytes)
	}
}

// TestRepairWAVAfterCrash 模拟「进程被杀」：文件里有数据，但头里的 data 长度仍是 0。
//
// 这是 Recover（UC-07）必须解决的核心问题：不修补的话播放器认为文件没有音频。
func TestRepairWAVAfterCrash(t *testing.T) {
	path := filepath.Join(t.TempDir(), "crashed.wav")
	const frames = 8000 // 0.5 秒
	// 手工构造「崩溃现场」：占位头（data=0）+ 真实数据。
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("创建文件失败：%v", err)
	}
	if _, err := f.Write(buildWAVHeader(InternalSampleRate, InternalChannels, InternalBitsPerSample, 0)); err != nil {
		t.Fatalf("写头失败：%v", err)
	}
	data := make([]byte, frames*2)
	for i := 0; i < frames; i++ {
		binary.LittleEndian.PutUint16(data[i*2:], uint16(int16(1000)))
	}
	if _, err := f.Write(data); err != nil {
		t.Fatalf("写数据失败：%v", err)
	}
	_ = f.Close()

	before, err := InspectWAV(path)
	if err != nil {
		t.Fatalf("读取未修补的头失败：%v", err)
	}
	if before.DataBytes != 0 {
		t.Fatalf("崩溃现场的头 data 长度应为 0，实际 %d", before.DataBytes)
	}

	repaired, err := RepairWAV(path)
	if err != nil {
		t.Fatalf("修补失败：%v", err)
	}
	if repaired != frames {
		t.Fatalf("修补后帧数 %d，期望 %d", repaired, frames)
	}
	after, err := InspectWAV(path)
	if err != nil {
		t.Fatalf("读取修补后的头失败：%v", err)
	}
	if after.DataBytes != uint32(frames*2) {
		t.Fatalf("修补后 data 长度 %d，期望 %d", after.DataBytes, frames*2)
	}
	if after.DurationMs() != 500 {
		t.Fatalf("修补后时长 %dms，期望 500ms", after.DurationMs())
	}
}

// TestRepairWAVTrimsHalfFrame 断言修补会裁掉「半帧」（奇数字节），
// 否则所有解码器都会在文件尾报错。
func TestRepairWAVTrimsHalfFrame(t *testing.T) {
	path := filepath.Join(t.TempDir(), "half.wav")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("创建文件失败：%v", err)
	}
	_ = binary.Write(f, binary.LittleEndian, buildWAVHeader(InternalSampleRate, InternalChannels, InternalBitsPerSample, 0))
	if _, err := f.Write(make([]byte, 4001)); err != nil { // 4001 = 2000 帧 + 1 字节
		t.Fatalf("写数据失败：%v", err)
	}
	_ = f.Close()

	frames, err := RepairWAV(path)
	if err != nil {
		t.Fatalf("修补失败：%v", err)
	}
	if frames != 2000 {
		t.Fatalf("修补后帧数 %d，期望 2000（半帧被裁掉）", frames)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("取文件信息失败：%v", err)
	}
	if (st.Size()-wavHeaderSize)%2 != 0 {
		t.Fatalf("修补后 data 长度 %d 仍是奇数", st.Size()-wavHeaderSize)
	}
}

// TestRepairWAVRejectsNonWAV 断言非 WAV 文件被明确拒绝（不猜格式、不静默重写）。
func TestRepairWAVRejectsNonWAV(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notwav.bin")
	if err := os.WriteFile(path, make([]byte, 100), 0o644); err != nil {
		t.Fatalf("准备文件失败：%v", err)
	}
	if _, err := RepairWAV(path); err == nil {
		t.Fatal("非 WAV 文件应返回错误")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("拒绝后不应删除原文件：%v", err)
	}
}

// TestWriterRejectsWriteAfterClose 断言关闭后写入被拒（防止收尾后误写）。
func TestWriterRejectsWriteAfterClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audio.wav")
	w, err := CreateWriter(path)
	if err != nil {
		t.Fatalf("创建 WAV 失败：%v", err)
	}
	_ = w.Close()
	if err := w.Write(make([]float32, 10)); err == nil {
		t.Fatal("关闭后写入应返回错误")
	}
}

// TestFloatToPCM16 断言满量程映射不做符号翻转（+1 不能变成 -32768）。
func TestFloatToPCM16(t *testing.T) {
	cases := []struct {
		in   float32
		want int16
	}{
		{0, 0},
		{1, 32767},
		{1.5, 32767},
		{-1, -32768},
		{-1.5, -32768},
		{0.5, 16384}, // round(0.5*32767)=16384
	}
	for _, c := range cases {
		if got := floatToPCM16(c.in); got != c.want {
			t.Fatalf("floatToPCM16(%v)=%d，期望 %d", c.in, got, c.want)
		}
	}
}
