//go:build windows && cgo && sherpa_models && !nollm

// 真实声纹模型验证（默认不参与 CI）。
//
//	export MANDARYN_REAL_SPK_DIR="C:/path/to/spk-bundle"  # 目录里一个 .onnx 声纹模型
//	export MANDARYN_REAL_WAV_A="C:/path/to/zh.wav"
//	export MANDARYN_REAL_WAV_B="C:/path/to/en.wav"
//	go test -tags sherpa_models -v -run TestRealSpeaker ./internal/speaker/sherpa/
package sherpa

import (
	"encoding/binary"
	"math"
	"os"
	"testing"
)

// readWAV16kReal 读 16k/单声道/16bit PCM 的真实 wav；格式不符直接 Skip。
func readWAV16kReal(t *testing.T, path string) []float32 {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 wav 失败: %v", err)
	}
	if len(raw) < 44 || string(raw[0:4]) != "RIFF" || string(raw[8:12]) != "WAVE" {
		t.Fatalf("不是 wav: %s", path)
	}
	var (
		channels, bits, rate int
		pcm                  []byte
	)
	for off := 12; off+8 <= len(raw); {
		id := string(raw[off : off+4])
		size := int(binary.LittleEndian.Uint32(raw[off+4 : off+8]))
		body := off + 8
		if body+size > len(raw) {
			size = len(raw) - body
		}
		switch id {
		case "fmt ":
			channels = int(binary.LittleEndian.Uint16(raw[body+2 : body+4]))
			rate = int(binary.LittleEndian.Uint32(raw[body+4 : body+8]))
			bits = int(binary.LittleEndian.Uint16(raw[body+14 : body+16]))
		case "data":
			pcm = raw[body : body+size]
		}
		off = body + size + size%2
	}
	if channels != 1 || bits != 16 || rate != 16000 {
		t.Skipf("样本不是 16k/单声道/16bit（rate=%d ch=%d bits=%d）: %s", rate, channels, bits, path)
	}
	out := make([]float32, len(pcm)/2)
	for i := range out {
		out[i] = float32(int16(binary.LittleEndian.Uint16(pcm[i*2:i*2+2]))) / 32768
	}
	return out
}

// cosine 余弦相似度（声纹比较口径，见 DOC-04 §4.3）。
func cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return math.NaN()
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return math.NaN()
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// TestRealSpeakerEmbed 用真实 3D-Speaker 模型对真实语音提取声纹，验证三件事：
//   - 能真实加载模型并产出固定维度向量（不是空/NaN）
//   - 同一段音频两次提取几乎完全一致（自相似 ≈ 1）
//   - 不同音频的相似度低于自相似（区分度存在，但不是硬阈值断言）
func TestRealSpeakerEmbed(t *testing.T) {
	dir := os.Getenv("MANDARYN_REAL_SPK_DIR")
	wavA := os.Getenv("MANDARYN_REAL_WAV_A")
	wavB := os.Getenv("MANDARYN_REAL_WAV_B")
	if dir == "" || wavA == "" {
		t.Skip("未设置 MANDARYN_REAL_SPK_DIR / MANDARYN_REAL_WAV_A，跳过真实声纹验证")
	}

	ext, err := NewSherpaExtractor(dir)
	if err != nil {
		t.Fatalf("加载真实声纹模型失败: %v", err)
	}
	defer func() { _ = ext.Close() }()
	t.Logf("声纹模型标识：%s", ext.ID())

	sa := readWAV16kReal(t, wavA)
	va, err := ext.Embed(sa, 16000)
	if err != nil {
		t.Fatalf("真实提取声纹失败: %v", err)
	}
	t.Logf("声纹维度：%d", len(va))
	if len(va) == 0 {
		t.Fatal("声纹向量长度为 0")
	}
	for i, x := range va {
		if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
			t.Fatalf("声纹向量第 %d 维是 NaN/Inf", i)
		}
	}

	va2, err := ext.Embed(sa, 16000)
	if err != nil {
		t.Fatalf("二次提取失败: %v", err)
	}
	self := cosine(va, va2)
	t.Logf("同段音频自相似：%.6f", self)
	if self < 0.999 {
		t.Fatalf("同段音频两次提取不一致：%.6f", self)
	}

	if wavB == "" {
		t.Log("未设置 MANDARYN_REAL_WAV_B，跳过跨音频区分度观察")
		return
	}
	sb := readWAV16kReal(t, wavB)
	vb, err := ext.Embed(sb, 16000)
	if err != nil {
		t.Fatalf("第二条音频提取失败: %v", err)
	}
	cross := cosine(va, vb)
	t.Logf("跨音频相似度：%.4f（自相似 %.6f，仅观察不设硬阈值）", cross, self)
	if cross >= self {
		t.Fatalf("跨音频相似度 %.4f 不低于自相似 %.6f，区分度异常", cross, self)
	}
}
