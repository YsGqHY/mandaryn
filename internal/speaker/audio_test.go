package speaker

import (
	"bytes"
	"context"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/mewkiz/flac"
	"github.com/mewkiz/flac/frame"
	"github.com/mewkiz/flac/meta"

	"vip.oberon.mandaryn/internal/apierr"
)

const (
	wavFormatPCM       = 1
	wavFormatIEEEFloat = 3
)

type speakerFLACFixture struct{ *bytes.Reader }

func (speakerFLACFixture) Close() error { return nil }

func TestSegmentReaderReusesFLACSource(t *testing.T) {
	const size = 64
	var b bytes.Buffer
	enc, err := flac.NewEncoder(&b, &meta.StreamInfo{BlockSizeMin: size, BlockSizeMax: size, SampleRate: 16000, NChannels: 1, BitsPerSample: 16, NSamples: 2 * size})
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []int32{8192, -16384} {
		pcm := make([]int32, size)
		for i := range pcm {
			pcm[i] = value
		}
		if err := enc.WriteFrame(&frame.Frame{
			Header:    frame.Header{HasFixedBlockSize: true, BlockSize: size, SampleRate: 16000, Channels: frame.ChannelsMono, BitsPerSample: 16},
			Subframes: []*frame.Subframe{{SubHeader: frame.SubHeader{Pred: frame.PredVerbatim}, Samples: pcm, NSamples: size}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	opens := 0
	loader, err := NewWAVLoader(func(context.Context, string) (AudioSource, error) {
		opens++
		return speakerFLACFixture{bytes.NewReader(b.Bytes())}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	load, closeAudio := segmentReader(context.Background(), loader, "s_flac")
	defer closeAudio()
	for i, want := range []float32{0.25, -0.5} {
		got, err := load(int64(i*4+1), int64(i*4+3))
		if err != nil || len(got) != 32 {
			t.Fatalf("segment %d len=%d err=%v", i, len(got), err)
		}
		for _, x := range got {
			if x != want {
				t.Fatalf("segment %d sample=%g want %g", i, x, want)
			}
		}
	}
	if opens != 1 {
		t.Fatalf("opened %d times, want 1", opens)
	}
}

// writeTestWAV 生成一个 WAV 文件并返回路径。
//
// 参数：
//   - samples：单声道采样（[-1,1]）
//   - sampleRate：采样率
//   - channels：1 或 2（2 时两声道写入相同采样）
//   - ieeeFloat：true 写 32bit float，false 写 16bit PCM
//   - extraChunk：在 fmt 与 data 之间插入 LIST 块，模拟真实录音的附加块
func writeTestWAV(t *testing.T, dir string, samples []float32, sampleRate, channels int,
	ieeeFloat, extraChunk bool) string {

	t.Helper()
	path := filepath.Join(dir, "audio.wav")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("创建 WAV 失败: %v", err)
	}
	defer func() { _ = f.Close() }()

	bps := 2
	format := uint16(wavFormatPCM)
	if ieeeFloat {
		bps = 4
		format = wavFormatIEEEFloat
	}
	dataLen := len(samples) * channels * bps
	extra := 0
	if extraChunk {
		extra = 8 + 4
	}
	riffSize := 4 + (8 + 16) + extra + (8 + dataLen)

	hdr := make([]byte, 0, 64)
	hdr = append(hdr, "RIFF"...)
	hdr = appendLE32(hdr, uint32(riffSize))
	hdr = append(hdr, "WAVE"...)
	hdr = append(hdr, "fmt "...)
	hdr = appendLE32(hdr, 16)
	hdr = appendLE16(hdr, format)
	hdr = appendLE16(hdr, uint16(channels))
	hdr = appendLE32(hdr, uint32(sampleRate))
	hdr = appendLE32(hdr, uint32(sampleRate*channels*bps))
	hdr = appendLE16(hdr, uint16(channels*bps))
	hdr = appendLE16(hdr, uint16(bps*8))
	if extraChunk {
		hdr = append(hdr, "LIST"...)
		hdr = appendLE32(hdr, 4)
		hdr = append(hdr, "INFO"...)
	}
	hdr = append(hdr, "data"...)
	hdr = appendLE32(hdr, uint32(dataLen))

	body := make([]byte, 0, dataLen)
	for _, s := range samples {
		for c := 0; c < channels; c++ {
			if ieeeFloat {
				var buf [4]byte
				binary.LittleEndian.PutUint32(buf[:], math.Float32bits(s))
				body = append(body, buf[:]...)
			} else {
				v := int16(s * 32767)
				var buf [2]byte
				binary.LittleEndian.PutUint16(buf[:], uint16(v))
				body = append(body, buf[:]...)
			}
		}
	}
	if _, err := f.Write(append(hdr, body...)); err != nil {
		t.Fatalf("写 WAV 失败: %v", err)
	}
	return path
}

func appendLE16(b []byte, v uint16) []byte {
	var buf [2]byte
	binary.LittleEndian.PutUint16(buf[:], v)
	return append(b, buf[:]...)
}

func appendLE32(b []byte, v uint32) []byte {
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], v)
	return append(b, buf[:]...)
}

// openerFor 构造一个总是打开指定文件的 AudioOpener。
func openerFor(path string) AudioOpener {
	return func(context.Context, string) (AudioSource, error) { return os.Open(path) }
}

// TestWAVLoaderRange 按毫秒区间读取（PCM16，附加 LIST 块）。
func TestWAVLoaderRange(t *testing.T) {
	dir := t.TempDir()
	// 1 秒 16kHz，恒定 0.25
	samples := make([]float32, SampleRate)
	for i := range samples {
		samples[i] = 0.25
	}
	path := writeTestWAV(t, dir, samples, SampleRate, 1, false, true)

	loader, err := NewWAVLoader(openerFor(path))
	if err != nil {
		t.Fatalf("构造读取器失败: %v", err)
	}

	got, err := loader.LoadSegment(context.Background(), "s_1", 0, 500)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	// 500ms @16kHz = 8000 个采样
	if len(got) != 8000 {
		t.Fatalf("应读到 8000 个采样，实际 %d", len(got))
	}
	if math.Abs(float64(got[0])-0.25) > 0.01 {
		t.Fatalf("采样值应为 0.25，实际 %v", got[0])
	}

	// 起始偏移生效：从 500ms 起读 200ms → 3200 个采样（内容相同，只校验长度语义）。
	got, err = loader.LoadSegment(context.Background(), "s_1", 500, 700)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if len(got) != 3200 {
		t.Fatalf("应读到 3200 个采样，实际 %d", len(got))
	}

	// 超出文件尾 → 截断到最后，不报错。
	got, err = loader.LoadSegment(context.Background(), "s_1", 900, 5000)
	if err != nil {
		t.Fatalf("越界区间不应报错: %v", err)
	}
	if len(got) != 1600 {
		t.Fatalf("应截断到 1600 个采样，实际 %d", len(got))
	}

	// 完全在文件之外 → 空样本，不报错。
	got, err = loader.LoadSegment(context.Background(), "s_1", 5000, 6000)
	if err != nil || len(got) != 0 {
		t.Fatalf("文件外区间应返回空，实际 len=%d err=%v", len(got), err)
	}

	// 非法区间 → 空（不 panic）。
	got, err = loader.LoadSegment(context.Background(), "s_1", 700, 700)
	if err != nil || len(got) != 0 {
		t.Fatalf("零长区间应返回空，实际 len=%d err=%v", len(got), err)
	}
}

// TestWAVLoaderFloatStereo 32bit float + 立体声下混单声道。
func TestWAVLoaderFloatStereo(t *testing.T) {
	dir := t.TempDir()
	samples := make([]float32, SampleRate/4) // 0.25s
	for i := range samples {
		samples[i] = 0.5
	}
	path := writeTestWAV(t, dir, samples, SampleRate, 2, true, false)

	loader, err := NewWAVLoader(openerFor(path))
	if err != nil {
		t.Fatalf("构造读取器失败: %v", err)
	}
	got, err := loader.LoadSegment(context.Background(), "s_1", 0, 250)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if len(got) != 4000 {
		t.Fatalf("应读到 4000 帧，实际 %d", len(got))
	}
	if math.Abs(float64(got[0])-0.5) > 0.001 {
		t.Fatalf("下混后应保持 0.5，实际 %v", got[0])
	}
}

// TestWAVLoaderRejectsWrongSampleRate 采样率不是 16k 时明确报错（不做重采样）。
func TestWAVLoaderRejectsWrongSampleRate(t *testing.T) {
	dir := t.TempDir()
	path := writeTestWAV(t, dir, make([]float32, 8000), 8000, 1, false, false)
	loader, err := NewWAVLoader(openerFor(path))
	if err != nil {
		t.Fatalf("构造读取器失败: %v", err)
	}
	_, err = loader.LoadSegment(context.Background(), "s_1", 0, 100)
	if !apierr.HasCode(err, apierr.CodeInvalidArgument) {
		t.Fatalf("期望 ERR_INVALID_ARGUMENT，实际 %v", err)
	}
}

// TestWAVLoaderRejectsNonWAV 非 WAV 文件报错而不是 panic。
func TestWAVLoaderRejectsNonWAV(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audio.wav")
	if err := os.WriteFile(path, []byte("这不是音频文件，只是一段文本"), 0o644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
	loader, err := NewWAVLoader(openerFor(path))
	if err != nil {
		t.Fatalf("构造读取器失败: %v", err)
	}
	if _, err := loader.LoadSegment(context.Background(), "s_1", 0, 100); err == nil {
		t.Fatal("非 WAV 文件应报错")
	}
}

// TestNewWAVLoaderRequiresOpener nil opener 直接报参数错误。
func TestNewWAVLoaderRequiresOpener(t *testing.T) {
	if _, err := NewWAVLoader(nil); !apierr.HasCode(err, apierr.CodeInvalidArgument) {
		t.Fatalf("期望 ERR_INVALID_ARGUMENT，实际 %v", err)
	}
}
