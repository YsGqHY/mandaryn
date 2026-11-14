package asr

import (
	"os"
	"testing"
)

// TestRealLocalInferenceSkippedWithoutModel 记录「真实推理测试为何没跑」。
//
// 真实推理实现放在 internal/asr/local（那里才有 CGO + sherpa-onnx），
// 且用 `sherpa_models` build tag 隔离，避免默认 `go test` 因缺 DLL 直接崩（0xc0000135）。
//
// 本测试的意义：CI / 本机跑 `go test ./internal/asr/...` 时会显式打印跳过原因，
// 而不是让人误以为「本地引擎已经被真实验证过了」。
func TestRealLocalInferenceSkippedWithoutModel(t *testing.T) {
	dir := os.Getenv("MANDARYN_TEST_MODEL_DIR")
	if dir != "" {
		t.Skipf("已设置 MANDARYN_TEST_MODEL_DIR=%s；真实推理请运行：go test -tags sherpa_models ./internal/asr/local/ -run TestRealInference -v", dir)
	}
	t.Skip("本机无已装模型：真实推理测试（internal/asr/local + -tags sherpa_models）未执行")
}
