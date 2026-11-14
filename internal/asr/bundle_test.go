package asr

import (
	"os"
	"path/filepath"
	"testing"

	"vip.oberon.mandaryn/internal/apierr"
)

// writeFiles 在临时目录里造出指定文件名（内容无关，识别只看文件名）。
func writeFiles(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		p := filepath.Join(dir, n)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatalf("造文件失败 %s: %v", n, err)
		}
	}
	return dir
}

func TestDetectBundle_KindHeuristics(t *testing.T) {
	cases := []struct {
		name  string
		files []string
		want  string
	}{
		{
			name:  "单文件 model.int8.onnx 归为 paraformer",
			files: []string{"model.int8.onnx", "tokens.txt"},
			want:  BundleKindParaformer,
		},
		{
			name:  "sensevoice 显式命名",
			files: []string{"sense-voice-small.int8.onnx", "tokens.txt"},
			want:  BundleKindSenseVoice,
		},
		{
			name:  "transducer 三件套",
			files: []string{"encoder.int8.onnx", "decoder.onnx", "joiner.int8.onnx", "tokens.txt"},
			want:  BundleKindTransducer,
		},
		{
			name:  "whisper 无 joiner",
			files: []string{"tiny-encoder.onnx", "tiny-decoder.onnx", "tiny-tokens.txt"},
			want:  BundleKindWhisper,
		},
		{
			name:  "paraformer model.onnx",
			files: []string{"model.onnx", "tokens.txt"},
			want:  BundleKindParaformer,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeFiles(t, tc.files...)
			spec, err := DetectBundle(dir)
			if err != nil {
				t.Fatalf("识别失败: %v", err)
			}
			if spec.Kind != tc.want {
				t.Fatalf("Kind = %q, 期望 %q", spec.Kind, tc.want)
			}
			if spec.Tokens == "" {
				t.Fatal("Tokens 路径不应为空")
			}
			// Tokens 必须是能直接交给 sherpa 的**可用路径**。
			// 只断言非空会漏掉「返回裸文件名 tokens.txt」这种真实事故：
			// sherpa 以 cwd 为基准解析，真实模型加载直接报 tokens.txt does not exist。
			if !filepath.IsAbs(spec.Tokens) {
				t.Fatalf("Tokens 必须是绝对路径，实际 %q", spec.Tokens)
			}
			if _, err := os.Stat(spec.Tokens); err != nil {
				t.Fatalf("Tokens 指向的文件不存在: %v", err)
			}
			// 同理：Model/Encoder 等模型文件路径也必须可直接使用。
			for label, p := range map[string]string{
				"Model": spec.Model, "Encoder": spec.Encoder,
				"Decoder": spec.Decoder, "Joiner": spec.Joiner,
			} {
				if p == "" {
					continue
				}
				if !filepath.IsAbs(p) {
					t.Fatalf("%s 必须是绝对路径，实际 %q", label, p)
				}
				if _, err := os.Stat(p); err != nil {
					t.Fatalf("%s 指向的文件不存在: %v", label, err)
				}
			}
		})
	}
}

func TestDetectBundle_Errors(t *testing.T) {
	t.Run("目录不存在", func(t *testing.T) {
		_, err := DetectBundle(filepath.Join(t.TempDir(), "nope"))
		if !apierr.HasCode(err, apierr.CodeModelNotFound) {
			t.Fatalf("期望 ERR_MODEL_NOT_FOUND，实际 %v", err)
		}
	})

	t.Run("目录存在但认不出模型", func(t *testing.T) {
		dir := writeFiles(t, "README.md", "config.json")
		_, err := DetectBundle(dir)
		if !apierr.HasCode(err, apierr.CodeModelCorrupt) {
			t.Fatalf("期望 ERR_MODEL_CORRUPT，实际 %v", err)
		}
	})

	t.Run("缺 tokens.txt", func(t *testing.T) {
		dir := writeFiles(t, "model.int8.onnx")
		_, err := DetectBundle(dir)
		if !apierr.HasCode(err, apierr.CodeModelCorrupt) {
			t.Fatalf("期望 ERR_MODEL_CORRUPT，实际 %v", err)
		}
	})

	t.Run("空目录路径", func(t *testing.T) {
		_, err := DetectBundle("  ")
		if !apierr.HasCode(err, apierr.CodeModelNotFound) {
			t.Fatalf("期望 ERR_MODEL_NOT_FOUND，实际 %v", err)
		}
	})
}

func TestAudioSegment_SampleRateOrDefault(t *testing.T) {
	if got := (AudioSegment{}).SampleRateOrDefault(); got != 16000 {
		t.Fatalf("零值应归一为 16000，实际 %d", got)
	}
	if got := (AudioSegment{SampleRate: 48000}).SampleRateOrDefault(); got != 48000 {
		t.Fatalf("显式采样率被改写: %d", got)
	}
}
