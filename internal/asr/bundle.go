package asr

import (
	"os"
	"path/filepath"
	"strings"

	"vip.oberon.mandaryn/internal/apierr"
)

// 本地模型包类型（由目录内文件名启发式判定，ARCH §4.2.5 的「识别模型类型」）。
//
// 之所以用文件名而不是清单字段：ModelScope / HF 下载下来的包目录结构不统一，
// 而 sherpa-onnx 官方包里文件名是稳定的（见下方匹配规则）。
const (
	BundleKindSenseVoice = "sensevoice"
	BundleKindParaformer = "paraformer"
	BundleKindWhisper    = "whisper"
	BundleKindTransducer = "transducer"
)

// BundleSpec 是「一个已装模型包该喂给 sherpa-onnx 什么」的纯数据描述。
//
// 与 sherpa_onnx 的具体 config 结构解耦：这里只做文件名 → 路径映射，
// 让**判定逻辑可以跨平台单测**（不需要 CGO / 不需要模型文件内容）。
type BundleSpec struct {
	// Dir 包目录（绝对路径）。
	Dir string
	// Kind 见 BundleKind* 常量。
	Kind string
	// Tokens tokens.txt 路径（所有类型都需要）。
	Tokens string
	// Model paraformer / sensevoice 的单文件模型路径。
	Model string
	// Encoder / Decoder / Joiner transducer 与 whisper 的分文件路径。
	Encoder string
	Decoder string
	Joiner  string
}

// DetectBundle 扫描目录并判定模型包类型（纯文件名启发式，不读文件内容）。
//
// 判定顺序：先结构、后命名。三件套（encoder/decoder/joiner）与 whisper 两件套
// 是**结构特征**，比名字可靠，必须优先；单文件模型（sensevoice 与 paraformer 都叫
// model.int8.onnx）无法靠文件名区分，因此引入**包目录名**作为命名线索。
//
// 为什么需要目录名线索：官方 sherpa-onnx 的 SenseVoice 包解压后就是
// model.int8.onnx + tokens.txt，与 paraformer 版式完全相同；而本产品把包装在
// models/sensevoice-small@1.2.0/ 下，类型信息只存在于目录名（bundleId@version）。
// 只看文件名会把真实 SenseVoice 包判成 paraformer，进而把模型塞进
// Paraformer.Model 并丢掉 ITN 后处理（见 docs/DEV-ISSUES-F-realmodel.md）。
//
// 错误映射（ARCH §4.2.5）：
//   - 目录不存在 / 空目录        → ERR_MODEL_NOT_FOUND
//   - 目录在但认不出模型文件      → ERR_MODEL_CORRUPT（提示重新下载）
func DetectBundle(dir string) (BundleSpec, error) {
	if strings.TrimSpace(dir) == "" {
		return BundleSpec{}, apierr.New(apierr.CodeModelNotFound).
			WithDetail("bundle dir is empty")
	}
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		return BundleSpec{}, apierr.New(apierr.CodeModelNotFound).
			WithDetail("bundle dir not found: %s", dir).WithCause(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return BundleSpec{}, apierr.New(apierr.CodeModelNotFound).
			WithDetail("bundle dir unreadable: %s", dir).WithCause(err)
	}

	// 只取文件名（小写）用于匹配；onnx 文件必须存在（目录里可能只有 .md / .json）。
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		names = append(names, e.Name())
	}

	// dirHint 是小写的包目录名，仅用于「同版式单文件模型」的定性。
	dirHint := strings.ToLower(filepath.Base(filepath.Clean(dir)))

	spec := BundleSpec{Dir: dir}
	// Tokens 必须是**绝对路径**：sherpa-onnx 的 config 直接吃这个字符串，
	// 只给裸文件名（tokens.txt）会以 cwd 为基准解析从而报
	// "tokens.txt does not exist"（真实模型实测踩到，见 DEV-ISSUES-F-realmodel）。
	if tok := pick(names, func(lower string) bool {
		return lower == "tokens.txt" || strings.HasSuffix(lower, "-tokens.txt")
	}); tok != "" {
		spec.Tokens = filepath.Join(dir, tok)
	}

	// 结构特征优先：sherpa-onnx 官方包的分件命名不统一
	// （zipformer 是 encoder.int8.onnx，whisper 是 tiny.en-encoder.onnx），
	// 因此用**包含**匹配而不是前缀匹配。
	encoder := pick(names, func(lower string) bool {
		return strings.Contains(lower, "encoder") && !strings.Contains(lower, "adaptor") &&
			strings.HasSuffix(lower, ".onnx")
	})
	decoder := pick(names, func(lower string) bool {
		return strings.Contains(lower, "decoder") && strings.HasSuffix(lower, ".onnx")
	})
	joiner := pick(names, func(lower string) bool {
		return strings.Contains(lower, "joiner") && strings.HasSuffix(lower, ".onnx")
	})

	// transducer（zipformer / conformer）：encoder + decoder + joiner 三件套。
	if encoder != "" && decoder != "" && joiner != "" {
		spec.Kind = BundleKindTransducer
		spec.Encoder = filepath.Join(dir, encoder)
		spec.Decoder = filepath.Join(dir, decoder)
		spec.Joiner = filepath.Join(dir, joiner)
		return checkTokens(spec)
	}

	// whisper：*-encoder.onnx / *-decoder.onnx，**没有** joiner。
	if encoder != "" && decoder != "" {
		spec.Kind = BundleKindWhisper
		spec.Encoder = filepath.Join(dir, encoder)
		spec.Decoder = filepath.Join(dir, decoder)
		return checkTokens(spec)
	}

	// 单文件模型：sensevoice 与 paraformer 同名（model.int8.onnx）。
	// 第一优先看文件名是否带 sense（第 79 行历史规则，保持兼容）。
	// senseHint 只认 sensevoice / sense-voice 两种写法：裸 "sense" 会误伤
	// "sensei" 之类无关命名，而真实包名一定是这两种之一。
	senseHint := func(lower string) bool {
		return strings.Contains(lower, "sensevoice") || strings.Contains(lower, "sense-voice")
	}
	if f := pick(names, func(lower string) bool {
		return senseHint(lower) && strings.HasSuffix(lower, ".onnx")
	}); f != "" {
		spec.Kind = BundleKindSenseVoice
		spec.Model = filepath.Join(dir, f)
		return checkTokens(spec)
	}
	// 第二优先看包目录名（真实官方包的唯一线索）。
	// 只在目录里存在 model.int8.onnx / model.onnx 时才采信，避免把混放目录误判。
	if senseHint(dirHint) {
		f := pick(names, func(lower string) bool { return lower == "model.int8.onnx" })
		if f == "" {
			f = pick(names, func(lower string) bool { return lower == "model.onnx" })
		}
		if f != "" {
			spec.Kind = BundleKindSenseVoice
			spec.Model = filepath.Join(dir, f)
			return checkTokens(spec)
		}
	}

	// paraformer / nemo-ctc 风格的单文件模型：model.int8.onnx 优先（体积小、够用）。
	modelFile := pick(names, func(lower string) bool {
		return lower == "model.int8.onnx"
	})
	if modelFile == "" {
		modelFile = pick(names, func(lower string) bool {
			return lower == "model.onnx" ||
				(strings.HasPrefix(lower, "model") && strings.HasSuffix(lower, ".onnx"))
		})
	}
	if modelFile != "" {
		spec.Kind = BundleKindParaformer
		spec.Model = filepath.Join(dir, modelFile)
		return checkTokens(spec)
	}

	return BundleSpec{}, apierr.New(apierr.CodeModelCorrupt).
		WithDetail("no recognizable onnx model file in %s", dir)
}

// checkTokens 校验 tokens.txt 是否存在：缺失也无法解码，同样归为 corrupt。
func checkTokens(spec BundleSpec) (BundleSpec, error) {
	if spec.Tokens == "" {
		return BundleSpec{}, apierr.New(apierr.CodeModelCorrupt).
			WithDetail("tokens.txt missing in %s", spec.Dir)
	}
	return spec, nil
}

// pick 返回第一个满足条件的文件名（保持 ReadDir 的字典序，结果可复现）。
func pick(names []string, ok func(lower string) bool) string {
	for _, n := range names {
		if ok(strings.ToLower(n)) {
			return n
		}
	}
	return ""
}
