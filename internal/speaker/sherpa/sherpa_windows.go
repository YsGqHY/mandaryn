//go:build windows && cgo && !nollm

// Package sherpa 是声纹提取器的 sherpa-onnx 实现（Windows + CGO，且非 `nollm` 精简产物）。
//
// `-tags nollm` 时本文件被排除，改由 sherpa_stub.go 提供同签名的桩：
// 目的与 `internal/asr/local` 完全同构 —— **裁剪体积与依赖**，不是 CGO 兜底（ADR-01）。
//
// 为什么单独成包而不是放进 internal/speaker：
// CGO 实现会把它所在的**整个测试二进制**绑上 sherpa-onnx 的 3 个 DLL
// （sherpa-onnx-c-api.dll / sherpa-onnx-cxx-api.dll / onnxruntime.dll），
// 缺 DLL 时连 `go test ./internal/speaker/...` 都起不来（Windows 0xc0000135）。
// 拆成子包后，internal/speaker 保持纯 Go，聚类 / 归并 / 角色逻辑可以无依赖地跑单测。
// 与 internal/asr/local 的取舍完全同构（见 docs/DEV-ISSUES-S7.md S7-01）。
package sherpa

import (
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	sherpaonnx "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/speaker"
)

// Extractor 是基于 sherpa-onnx SpeakerEmbeddingExtractor 的声纹提取器。
//
// 并发约束：extractor 与 stream 都不是并发安全的，且 stream 必须在同一次调用内
// 用完即释放，因此用 mu 串行化整个 Embed。
type Extractor struct {
	mu     sync.Mutex
	ex     *sherpaonnx.SpeakerEmbeddingExtractor
	id     string
	dim    int
	closed bool
}

// 编译期断言：本实现满足 speaker.Extractor。
var _ speaker.Extractor = (*Extractor)(nil)

// NewSherpaExtractor 用某个模型包目录构造提取器。
//
// 目录里必须有一个声纹 embedding 模型（.onnx，如
// `3dspeaker_speech_campplus_sv_zh-cn_16k-common.onnx`）。
//
// 错误映射：
//   - 目录不存在 / 没有 .onnx        → ERR_MODEL_NOT_FOUND
//   - 模型加载失败（损坏 / 缺 DLL）  → ERR_ENGINE_LOAD
func NewSherpaExtractor(bundleDir string) (speaker.Extractor, error) {
	model, err := findEmbeddingModel(bundleDir)
	if err != nil {
		return nil, err
	}
	cfg := sherpaonnx.SpeakerEmbeddingExtractorConfig{
		Model:      model,
		NumThreads: 1, // 声纹提取只跑片段级短音频，1 线程足够，且不抢 CPU 预算
		Debug:      0,
		Provider:   "cpu",
	}
	ex := sherpaonnx.NewSpeakerEmbeddingExtractor(&cfg)
	if ex == nil {
		return nil, apierr.New(apierr.CodeEngineLoad).WithDetail(
			"声纹模型加载失败（模型损坏、缺 onnxruntime.dll 或内存不足）：%s",
			filepath.Base(model))
	}
	dim := ex.Dim()
	if dim <= 0 {
		sherpaonnx.DeleteSpeakerEmbeddingExtractor(ex)
		return nil, apierr.New(apierr.CodeEngineLoad).
			WithDetail("声纹模型返回非法维度 %d", dim)
	}
	return &Extractor{
		ex:  ex,
		id:  "speaker:" + strings.TrimSuffix(filepath.Base(model), filepath.Ext(model)),
		dim: dim,
	}, nil
}

// Embed 对一段 16kHz 单声道样本提取声纹向量。
//
// 样本为空时直接返回错误：sherpa 的 `AcceptWaveform` 内部取 `&samples[0]`，
// 传空切片会 panic（不是错误返回），因此空样本必须在进入库之前拦掉。
func (e *Extractor) Embed(samples []float32, sampleRate int) ([]float32, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.closed || e.ex == nil {
		return nil, apierr.New(apierr.CodeEngineNotReady).WithDetail("声纹提取器已关闭")
	}
	if len(samples) == 0 {
		return nil, apierr.New(apierr.CodeInvalidArgument).WithDetail("样本为空，无法提取声纹")
	}
	if sampleRate != speaker.SampleRate {
		// 声纹模型按 16k 训练；别的采样率会得到无意义向量，宁可报错。
		return nil, apierr.New(apierr.CodeInvalidArgument).
			WithDetail("仅支持 %dHz 样本，实际 %dHz", speaker.SampleRate, sampleRate)
	}

	stream := e.ex.CreateStream()
	if stream == nil {
		return nil, apierr.New(apierr.CodeEngineLoad).WithDetail("无法创建声纹 stream")
	}
	defer sherpaonnx.DeleteOnlineStream(stream)

	stream.AcceptWaveform(sampleRate, samples)
	stream.InputFinished()
	if !e.ex.IsReady(stream) {
		// 样本过短（库内部有最小帧数要求）：不是故障，让上层把这句归「未识别」。
		return nil, apierr.New(apierr.CodeInvalidArgument).
			WithDetail("样本过短，无法提取声纹（时长不足模型最小帧数）")
	}

	v := e.ex.Compute(stream)
	if len(v) != e.dim {
		return nil, apierr.New(apierr.CodeEngineLoad).
			WithDetail("声纹向量维度异常：期望 %d，实际 %d", e.dim, len(v))
	}
	// L2 归一化：后续只看余弦相似度，尺度无意义；在源头归一化可省掉下游重复计算。
	return normalize(v), nil
}

// ID 返回模型标识（`speaker:<文件名>`），落 `speakers.model_id`。
func (e *Extractor) ID() string { return e.id }

// Dim 返回向量维度（192 / 256 等，随模型）。
func (e *Extractor) Dim() int { return e.dim }

// Close 释放模型（幂等）。
func (e *Extractor) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil
	}
	if e.ex != nil {
		sherpaonnx.DeleteSpeakerEmbeddingExtractor(e.ex)
		e.ex = nil
	}
	e.closed = true
	return nil
}

// normalize 做 L2 归一化（全零向量原样返回，避免产生 NaN）。
func normalize(v []float32) []float32 {
	var sum float64
	for _, f := range v {
		sum += float64(f) * float64(f)
	}
	if sum <= 0 {
		return v
	}
	norm := math.Sqrt(sum)
	out := make([]float32, len(v))
	for i, f := range v {
		out[i] = float32(float64(f) / norm)
	}
	return out
}

// findEmbeddingModel 在模型包目录里挑出声纹 embedding 模型。
//
// 判定规则（按优先级，同优先级取字典序最小的文件名，保证结果可复现）：
//  1. 文件名同时含 "speaker" 与 "embedding"；
//  2. 含 "campplus" / "3dspeaker" / "titanet" / "wespeaker"（常见声纹模型族）；
//  3. 含 "speaker" 或 "sv"；
//  4. 目录里唯一的 .onnx。
//
// 之所以不读清单：与 asr.DetectBundle 同样的理由 —— 下载来的包目录结构不统一，
// 而 sherpa-onnx 官方包的命名是稳定的。
func findEmbeddingModel(bundleDir string) (string, error) {
	dir := strings.TrimSpace(bundleDir)
	if dir == "" {
		return "", apierr.New(apierr.CodeModelNotFound).WithDetail("模型包目录为空")
	}
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		return "", apierr.New(apierr.CodeModelNotFound).
			WithDetail("模型包目录不存在：%s", dir).WithCause(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", apierr.New(apierr.CodeModelNotFound).
			WithDetail("模型包目录不可读：%s", dir).WithCause(err)
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".onnx") {
			continue
		}
		names = append(names, e.Name())
	}
	if len(names) == 0 {
		return "", apierr.New(apierr.CodeModelNotFound).
			WithDetail("模型包里没有 .onnx 声纹模型：%s", dir)
	}
	sort.Strings(names)

	preferences := []func(string) bool{
		func(n string) bool { return strings.Contains(n, "speaker") && strings.Contains(n, "embedding") },
		func(n string) bool {
			for _, k := range []string{"campplus", "3dspeaker", "titanet", "wespeaker", "embedding"} {
				if strings.Contains(n, k) {
					return true
				}
			}
			return false
		},
		func(n string) bool { return strings.Contains(n, "speaker") || strings.Contains(n, "sv") },
	}
	for _, ok := range preferences {
		for _, n := range names {
			if ok(strings.ToLower(n)) {
				return filepath.Join(dir, n), nil
			}
		}
	}
	if len(names) == 1 {
		return filepath.Join(dir, names[0]), nil
	}
	// 多个 .onnx 且认不出哪个是声纹模型：宁可报「不认识」，也不要瞎猜一个转写模型去提取声纹。
	return "", apierr.New(apierr.CodeModelCorrupt).
		WithDetail("无法在 %s 中识别声纹 embedding 模型（候选 %d 个 .onnx）", dir, len(names))
}
