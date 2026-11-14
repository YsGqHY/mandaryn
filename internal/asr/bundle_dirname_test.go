package asr

import (
	"os"
	"path/filepath"
	"testing"
)

// writeFilesIn 在**指定名字**的子目录里造文件，用来复现「包目录名本身带类型提示」的场景。
//
// 为什么需要它：官方 sherpa-onnx 的 SenseVoice 包内 onnx 就叫 model.int8.onnx，
// 而产品安装目录是 models/sensevoice-small@1.2.0/ —— 类型信息只在目录名上，
// 只看文件名会把真实包误判成 paraformer（见 docs/DEV-ISSUES-F-realmodel.md）。
func writeFilesIn(t *testing.T, dirName string, names ...string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), dirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("造目录失败 %s: %v", dirName, err)
	}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatalf("造文件失败 %s: %v", n, err)
		}
	}
	return dir
}

// TestDetectBundle_DirNameHint 守住「目录名也参与类型判定」。
//
// 这条规则来自真实模型：官方 SenseVoice 包解压后是 model.int8.onnx + tokens.txt，
// 与 paraformer 同名文件完全无法区分，只有包目录名（bundleId@version）能定性。
func TestDetectBundle_DirNameHint(t *testing.T) {
	t.Run("官方包名在目录上 → sensevoice", func(t *testing.T) {
		dir := writeFilesIn(t, "sensevoice-small@1.2.0", "model.int8.onnx", "tokens.txt")
		spec, err := DetectBundle(dir)
		if err != nil {
			t.Fatalf("识别失败: %v", err)
		}
		if spec.Kind != BundleKindSenseVoice {
			t.Fatalf("Kind = %q, 期望 %q（真实 SenseVoice 包会被塞进 paraformer 配置分支）",
				spec.Kind, BundleKindSenseVoice)
		}
	})

	t.Run("连字符写法也认", func(t *testing.T) {
		dir := writeFilesIn(t, "sense-voice-big", "model.onnx", "tokens.txt")
		spec, err := DetectBundle(dir)
		if err != nil {
			t.Fatalf("识别失败: %v", err)
		}
		if spec.Kind != BundleKindSenseVoice {
			t.Fatalf("Kind = %q, 期望 %q", spec.Kind, BundleKindSenseVoice)
		}
	})

	t.Run("中性目录名仍归 paraformer", func(t *testing.T) {
		dir := writeFilesIn(t, "paraformer-zh@1.0.0", "model.int8.onnx", "tokens.txt")
		spec, err := DetectBundle(dir)
		if err != nil {
			t.Fatalf("识别失败: %v", err)
		}
		if spec.Kind != BundleKindParaformer {
			t.Fatalf("Kind = %q, 期望 %q", spec.Kind, BundleKindParaformer)
		}
	})

	t.Run("三件套优先于目录名提示", func(t *testing.T) {
		// 目录名带 sense 但实际是 transducer 三件套：必须按结构判定，
		// 否则把 encoder/decoder/joiner 塞进 SenseVoice.Model 会加载失败。
		dir := writeFilesIn(t, "sensevoice-typo", "encoder.onnx", "decoder.onnx", "joiner.onnx", "tokens.txt")
		spec, err := DetectBundle(dir)
		if err != nil {
			t.Fatalf("识别失败: %v", err)
		}
		if spec.Kind != BundleKindTransducer {
			t.Fatalf("Kind = %q, 期望 %q", spec.Kind, BundleKindTransducer)
		}
	})
}
