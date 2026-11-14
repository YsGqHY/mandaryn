//go:build windows && cgo && sherpa_models && !nollm

// 真实模型验证（默认不参与 CI）。
//
// 与 inference_test.go 共用 `sherpa_models` 标签（同一个项目只保留**一套**真机入口；
// 早前这里曾用独立标签 mandaryn_realmodel，已合并）。两者的分工：
//   - inference_test.go：链路不崩（1 秒静音，不校验内容）
//   - 本文件：真实语音产出非空文本 + 包类型判定（更强的断言）
//
// 运行方式（模型与真实语音样本由人工准备，见 docs/DEV-ISSUES-F-realmodel.md）：
//
//	export MANDARYN_TEST_MODEL_DIR="C:/path/to/sensevoice-bundle"  # 含 .onnx + tokens.txt
//	export MANDARYN_REAL_WAV="C:/path/to/zh.wav"                   # 16k 单声道真实中文语音
//	go test -tags sherpa_models -v -run TestRealSenseVoice ./internal/asr/local/
//
// 加 -tags 之外仍需 CGO_ENABLED=1，且可执行文件要能加载 sherpa-onnx 的 DLL/so，
// Windows 上请 `go test -c -o bin/` 后从 bin/ 目录运行（DLL 与 exe 同目录，
// 直接把 bin 加 PATH 无效：System32 同名 DLL 会抢先命中导致 0xc000007b）。
package local

import (
	"context"
	"encoding/binary"
	"os"
	"testing"

	"vip.oberon.mandaryn/internal/asr"
)

// realModelDir 取模型包目录；同时接受既有的 MANDARYN_TEST_MODEL_DIR，
// 使这套断言能直接跑在 inference_test.go 的既有环境里。
func realModelDir() string {
	if d := os.Getenv("MANDARYN_REAL_ASR_DIR"); d != "" {
		return d
	}
	return os.Getenv("MANDARYN_TEST_MODEL_DIR")
}

// readWAV16k 读 16k/单声道/16bit PCM 的 wav（真实样本就是这种格式）。
//
// 只支持验证用的最小子集：遇到非 16k、非单声道、非 16bit 直接 Skip，
// 避免把「样本格式不对」误报成「引擎有问题」。
func readWAV16k(t *testing.T, path string) []float32 {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 wav 失败: %v", err)
	}
	if len(raw) < 44 || string(raw[0:4]) != "RIFF" || string(raw[8:12]) != "WAVE" {
		t.Fatalf("不是 wav: %s", path)
	}
	// 顺序扫 chunk，找 fmt 与 data（真实样本不含 LIST 之外的怪 chunk）。
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
		off = body + size + size%2 // chunk 按偶数字节对齐
	}
	if channels != 1 || bits != 16 || rate != 16000 {
		t.Skipf("样本不是 16k/单声道/16bit（rate=%d ch=%d bits=%d），换一个样本: %s",
			rate, channels, bits, path)
	}
	if len(pcm) == 0 {
		t.Fatalf("wav 里没有 data: %s", path)
	}
	out := make([]float32, len(pcm)/2)
	for i := range out {
		v := int16(binary.LittleEndian.Uint16(pcm[i*2 : i*2+2]))
		out[i] = float32(v) / 32768
	}
	return out
}

// TestRealSenseVoiceTranscribe 用真实 SenseVoice 模型包跑一次真实转写。
//
// 断言的是「能真实产出非空文本」，不比对具体字串：模型版本会更新，
// 硬编码期望文本会让这个测试变成版本牢笼。
func TestRealSenseVoiceTranscribe(t *testing.T) {
	dir := realModelDir()
	wav := os.Getenv("MANDARYN_REAL_WAV")
	if dir == "" || wav == "" {
		t.Skip("未设置 MANDARYN_TEST_MODEL_DIR（或 MANDARYN_REAL_ASR_DIR）/ MANDARYN_REAL_WAV，跳过真实模型验证")
	}

	// 第一步：包识别。官方 SenseVoice 包内文件叫 model.int8.onnx，
	// 这里断言识别结果，顺便守住「识别规则不许退化」。
	spec, err := asr.DetectBundle(dir)
	if err != nil {
		t.Fatalf("DetectBundle 失败: %v", err)
	}
	t.Logf("包识别：kind=%s model=%s tokens=%s", spec.Kind, spec.Model, spec.Tokens)
	if spec.Kind != asr.BundleKindSenseVoice {
		t.Fatalf("真实 SenseVoice 包被识别成 %q，会走错配置分支", spec.Kind)
	}

	// 第二步：真实加载 + 自检。
	eng, err := New(Spec{BundleID: "sensevoice-small", Version: "1.2.0", Dir: dir, Threads: 2})
	if err != nil {
		t.Fatalf("构造本地引擎失败: %v", err)
	}
	defer func() { _ = eng.Close() }()

	ctx := context.Background()
	if err := eng.Warm(ctx); err != nil {
		t.Fatalf("Warm 失败（真实模型加载）: %v", err)
	}
	h := eng.Health(ctx)
	t.Logf("加载后引擎：engineId=%s 状态=%s", eng.ID(), h.State)

	// 第三步：真实推理。
	samples := readWAV16k(t, wav)
	t.Logf("样本：%d 采样点 = %.2fs", len(samples), float64(len(samples))/16000)

	res, err := eng.Finalize(asr.AudioSegment{
		Samples:    samples,
		SampleRate: 16000,
		Seq:        0,
		EndMs:      int64(len(samples)) * 1000 / 16000,
	})
	if err != nil {
		t.Fatalf("真实转写失败: %v", err)
	}
	t.Logf("转写结果：text=%q lang=%q elapsedMs=%d", res.Text, res.Lang, res.ElapsedMs)
	if res.Text == "" {
		t.Fatal("真实模型转写返回空文本")
	}
	if res.EngineID == "" {
		t.Fatal("转写结果缺少 engineId")
	}
}
