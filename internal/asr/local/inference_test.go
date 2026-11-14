//go:build windows && cgo && sherpa_models && !nollm

// 真实推理集成测试（默认**不参与** `go test ./internal/asr/...`）。
//
// 之所以加 build tag：`internal/asr/local` 的测试二进制会绑定 sherpa-onnx 的
// 3 个 DLL（sherpa-onnx-c-api.dll / sherpa-onnx-cxx-api.dll / onnxruntime.dll），
// 缺 DLL 时 Windows 直接 0xc0000135，连「没有测试可跑」都做不到。
//
// 运行方式（详见 docs/DEV-ISSUES-S3.md S3-02）：
//
//	cp "$HOME/go/pkg/mod/github.com/k2-fsa/sherpa-onnx-go-windows@v1.13.8/lib/x86_64-pc-windows-gnu/"*.dll .
//	MANDARYN_TEST_MODEL_DIR="D:/models/sherpa-onnx-sense-voice-zh-en" \
//	  go test -tags sherpa_models ./internal/asr/local/ -run TestRealInference -v
package local

import (
	"context"
	"os"
	"testing"

	"vip.oberon.mandaryn/internal/asr"
	"vip.oberon.mandaryn/internal/contract"
)

// TestRealInference 用真实模型包跑一次端到端：Warm → Finalize → Close。
//
// 没有设置 MANDARYN_TEST_MODEL_DIR 时跳过（本机通常没有已装模型）。
func TestRealInference(t *testing.T) {
	dir := os.Getenv("MANDARYN_TEST_MODEL_DIR")
	if dir == "" {
		t.Skip("未设置 MANDARYN_TEST_MODEL_DIR，跳过真实推理测试（本机无已装模型）")
	}
	spec, err := asr.DetectBundle(dir)
	if err != nil {
		t.Fatalf("模型包识别失败: %v", err)
	}
	t.Logf("识别到模型包类型: %s", spec.Kind)

	eng, err := New(Spec{BundleID: "test-bundle", Dir: dir, Language: "zh"})
	if err != nil {
		t.Fatalf("构造本地引擎失败: %v", err)
	}
	defer func() { _ = eng.Close() }()

	if err := eng.Warm(context.Background()); err != nil {
		t.Fatalf("Warm 失败: %v", err)
	}
	if h := eng.Health(context.Background()); h.State != contract.EngineStateReady {
		t.Fatalf("State = %q, 期望 ready", h.State)
	}

	// 1 秒静音：只验证整条链路不崩，不校验识别内容（静音本就该是空文本）。
	silence := make([]float32, 16000)
	res, err := eng.Finalize(asr.AudioSegment{Samples: silence, SampleRate: 16000})
	if err != nil {
		t.Fatalf("Finalize 失败: %v", err)
	}
	t.Logf("识别结果长度=%d 字，engineId=%s，耗时=%dms",
		len([]rune(res.Text)), res.EngineID, res.ElapsedMs)
}
