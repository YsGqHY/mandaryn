//go:build windows && cgo && !nollm

// Package local 是 sherpa-onnx 的本地引擎实现（Windows + CGO，且非 `nollm` 精简产物）。
//
// `-tags nollm` 时本文件被排除，改由 local_stub.go 提供同签名的桩：
// 目的是**裁剪体积与依赖**（不链接 sherpa-onnx / onnxruntime），
// 不是「CGO 不可用时的兜底」（ADR-01）。桩恒返回 ERR_LOCAL_UNAVAILABLE，
// 不静默降级去调在线引擎。
//
// 为什么单独成包而不是放进 internal/asr：
// CGO 实现会把它所在的**整个测试二进制**绑上 sherpa-onnx 的 3 个 DLL
// （sherpa-onnx-c-api.dll / sherpa-onnx-cxx-api.dll / onnxruntime.dll），
// 缺 DLL 时连 `go test ./internal/asr/...` 都起不来（Windows 0xc0000135）。
// 拆成子包后，internal/asr 保持纯 Go，路由与降级逻辑可以无依赖地跑单测。
package local

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	sherpa "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/asr"
	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/utils/logx"
)

// Spec 描述「要用哪个模型包做本地转写」。
type Spec struct {
	// BundleID 模型包标识；若 models 包把版本拼进去（如 foo@1.2.0），ID() 原样返回。
	BundleID string
	// Version 可选版本号；非空时 ID() 返回 bundleID@version。
	Version string
	// Dir 模型包目录（绝对路径）。
	Dir string
	// Threads onnx 推理线程数；0 → 用 NumCPU/2（至少 1）。
	Threads int
	// Language sensevoice / whisper 的语种；空或 auto 由各自模型默认。
	Language string
}

// Engine 是基于 sherpa-onnx offline recognizer 的本地引擎（非流式）。
//
// 并发约束：OfflineRecognizer 不是并发安全的，且 stream 必须在同一次调用内
// 用完即释放，因此用 mu 串行化整个 Decode。
type Engine struct {
	spec Spec

	mu         sync.Mutex
	recognizer *sherpa.OfflineRecognizer
	state      string
	loadMs     int64
	last       *apierr.Err
	closed     bool
}

// 编译期断言：本实现满足 asr.Engine（含 Close）。
var _ asr.Engine = (*Engine)(nil)

// New 构造本地引擎（不加载模型，加载发生在 Warm）。
func New(spec Spec) (asr.Engine, error) {
	if strings.TrimSpace(spec.Dir) == "" {
		return nil, apierr.New(apierr.CodeModelNotFound).WithDetail("未指定模型包目录")
	}
	if strings.TrimSpace(spec.BundleID) == "" {
		return nil, apierr.New(apierr.CodeModelNotFound).WithDetail("未指定模型包 ID")
	}
	return &Engine{spec: spec, state: contract.EngineStateLoading}, nil
}

// NewFactory 用注入的 BundleLocator 构造本地引擎工厂。
//
// 每次调用都重新 LocateBundle：换模型包后热切换能拿到新目录，不缓存旧路径。
// role 固定 RoleOffline —— 延后转写（默认模式）用非流式模型。
//
// 等价于 `NewFactoryWithThreads(loc, nil)`：不注入线程数时用引擎默认值
// （`threadsOrDefault(0)` = NumCPU/2）。保留本函数是为了不打断既有调用方与测试。
func NewFactory(loc asr.BundleLocator) asr.LocalFactory {
	return NewFactoryWithThreads(loc, nil)
}

// NewFactoryWithThreads 构造一个**每次加载都重新读档位**的本地引擎工厂。
//
// threads 返回当前档位对应的 onnx 线程数（装配层传 `perf.Service.Threads`）。
//
// 关键语义：`threads()` 在**工厂被调用时**读一次，不是构造工厂时读一次。
// 于是「下一次加载引擎」（热切换来源 / Reload / 冷启动）自然带上最新档位，
// 而已经加载的引擎保持原线程数——线程数写进 `NewOfflineRecognizer` 后不可改，
// 详见 `asr.LoadThreads` 与 `perf.ThreadsFor` 的注释。**不要**把它说成热生效。
//
// threads 为 nil 或返回 ≤0 时，Spec.Threads 为 0，由 `threadsOrDefault` 给默认值。
func NewFactoryWithThreads(loc asr.BundleLocator, threads func() int) asr.LocalFactory {
	return func() (asr.Engine, error) {
		if loc == nil {
			return nil, apierr.New(apierr.CodeLocalUnavailable).
				WithDetail("未装配模型包定位器")
		}
		dir, bundleID, err := loc.LocateBundle(asr.RoleOffline)
		if err != nil {
			return nil, err
		}
		return New(Spec{
			BundleID: bundleID,
			Dir:      dir,
			Version:  versionFromBundleID(bundleID),
			// 定位成功后才读档位：加载必然失败时不必惊动采样侧。
			Threads: asr.LoadThreads(threads),
		})
	}
}

// versionFromBundleID 从 `id@version` 形态里拆版本号。
func versionFromBundleID(bundleID string) string {
	if i := strings.LastIndex(bundleID, "@"); i > 0 && i < len(bundleID)-1 {
		return bundleID[i+1:]
	}
	return ""
}

// ID 返回 bundleId@version（无版本时只返回 bundleId）。
func (e *Engine) ID() string {
	if strings.TrimSpace(e.spec.Version) == "" {
		return e.spec.BundleID
	}
	return e.spec.BundleID + "@" + e.spec.Version
}

// Warm 加载模型 + 自检一次空跑（ARCH §4.2.2 步骤 3）。幂等。
//
// 失败分类（ARCH §4.2.5）：
//   - 扫描不到模型文件 / tokens.txt → ERR_MODEL_NOT_FOUND / ERR_MODEL_CORRUPT
//   - 构造 recognizer 失败（内存不足、缺 onnxruntime.dll）→ ERR_ENGINE_LOAD
func (e *Engine) Warm(ctx context.Context) error {
	e.mu.Lock()
	ready := e.recognizer != nil && e.state == contract.EngineStateReady
	e.mu.Unlock()
	if ready {
		return nil // 幂等：已加载
	}

	started := time.Now()

	spec, err := asr.DetectBundle(e.spec.Dir)
	if err != nil {
		e.fail(err)
		return err
	}
	cfg := buildConfig(spec, e.spec)
	rec := sherpa.NewOfflineRecognizer(&cfg)
	if rec == nil {
		apiErr := apierr.New(apierr.CodeEngineLoad).WithDetail(
			"本地引擎加载失败（可能缺少 onnxruntime.dll、内存不足或模型损坏）: %s",
			filepath.Base(e.spec.Dir))
		e.fail(apiErr)
		return apiErr
	}

	if err := selfCheck(rec); err != nil {
		sherpa.DeleteOfflineRecognizer(rec)
		apiErr := apierr.New(apierr.CodeEngineLoad).
			WithDetail("本地引擎自检失败: %s", err.Error())
		e.fail(apiErr)
		return apiErr
	}

	e.mu.Lock()
	e.recognizer = rec
	e.state = contract.EngineStateReady
	e.loadMs = time.Since(started).Milliseconds()
	e.last = nil
	e.mu.Unlock()

	logx.For("asr.local").Info("本地引擎加载完成",
		"engineId", e.ID(), "kind", spec.Kind, "loadMs", e.loadMs)
	return nil
}

// selfCheck 用 0.3s 静音跑一遍完整链路，避免「加载成功但一解码就崩」。
func selfCheck(rec *sherpa.OfflineRecognizer) error {
	const frames = 16000 * 3 / 10 // 0.3s @16k
	silence := make([]float32, frames)
	stream := sherpa.NewOfflineStream(rec)
	if stream == nil {
		return apierr.New(apierr.CodeEngineLoad).WithDetail("无法创建 offline stream")
	}
	defer sherpa.DeleteOfflineStream(stream)
	stream.AcceptWaveform(16000, silence)
	rec.Decode(stream)
	_ = stream.GetResult() // 只验证不崩，不关心内容
	return nil
}

// buildConfig 按模型包类型拼 sherpa-onnx 配置。
//
// 说明：OfflineModelConfig 里未填的字段是零值，库内部按非空的那一组判定模型类型，
// 因此这里只需填对应子结构。
func buildConfig(spec asr.BundleSpec, local Spec) sherpa.OfflineRecognizerConfig {
	var cfg sherpa.OfflineRecognizerConfig
	cfg.FeatConfig.SampleRate = 16000
	cfg.FeatConfig.FeatureDim = 80
	cfg.ModelConfig.Tokens = spec.Tokens
	cfg.ModelConfig.NumThreads = threadsOrDefault(local.Threads)
	cfg.ModelConfig.Debug = 0
	cfg.ModelConfig.Provider = "cpu"
	cfg.DecodingMethod = "greedy_search"

	lang := strings.TrimSpace(local.Language)
	switch spec.Kind {
	case asr.BundleKindSenseVoice:
		cfg.ModelConfig.SenseVoice.Model = spec.Model
		cfg.ModelConfig.SenseVoice.Language = lang
		// ITN 打开才能拿到数字/标点后处理，中文会议记录更易读。
		cfg.ModelConfig.SenseVoice.UseInverseTextNormalization = 1
	case asr.BundleKindParaformer:
		cfg.ModelConfig.Paraformer.Model = spec.Model
	case asr.BundleKindTransducer:
		cfg.ModelConfig.Transducer.Encoder = spec.Encoder
		cfg.ModelConfig.Transducer.Decoder = spec.Decoder
		cfg.ModelConfig.Transducer.Joiner = spec.Joiner
	case asr.BundleKindWhisper:
		cfg.ModelConfig.Whisper.Encoder = spec.Encoder
		cfg.ModelConfig.Whisper.Decoder = spec.Decoder
		// whisper 需要显式语种；auto 退化为 zh（产品主场景是中文语音）。
		if lang == "" || lang == "auto" {
			lang = "zh"
		}
		cfg.ModelConfig.Whisper.Language = lang
		cfg.ModelConfig.Whisper.Task = "transcribe"
		cfg.ModelConfig.Whisper.TailPaddings = -1
	}
	return cfg
}

// threadsOrDefault 推理线程数：默认留给系统一半核（至少 1）。
//
// 真正的上限由 NFR-08 的性能预算在调度层调，这里只负责不把整机吃满。
func threadsOrDefault(n int) int {
	if n > 0 {
		return n
	}
	num := runtime.NumCPU()
	if num <= 2 {
		return 1
	}
	return num / 2
}

// Feed 在延后转写模式下不做流式：返回已关闭的空 channel。
//
// 「边录边转」（FR-03-10，默认关）打开后才需要 online recognizer 的流式路径，
// 那属于另一个开关 + 另一类模型包（RoleStreaming），不在本引擎范围内。
func (e *Engine) Feed(_ []float32) (<-chan asr.Partial, error) {
	ch := make(chan asr.Partial)
	close(ch)
	return ch, nil
}

// Finalize 把整句样本喂给 offline recognizer，返回定稿文本。
func (e *Engine) Finalize(seg asr.AudioSegment) (asr.FinalResult, error) {
	e.mu.Lock()
	rec := e.recognizer
	closed := e.closed
	e.mu.Unlock()

	if closed {
		return asr.FinalResult{}, apierr.New(apierr.CodeEngineNotReady).
			WithDetail("本地引擎已关闭")
	}
	if rec == nil {
		return asr.FinalResult{}, apierr.New(apierr.CodeEngineNotReady).
			WithDetail("本地模型尚未加载完成")
	}
	if len(seg.Samples) == 0 {
		// 空片段不是故障（VAD 可能切出极短尾音）：返回空文本，不打断转写。
		return asr.FinalResult{Text: "", EngineID: e.ID()}, nil
	}

	started := time.Now()
	stream := sherpa.NewOfflineStream(rec)
	if stream == nil {
		apiErr := apierr.New(apierr.CodeEngineLoad).WithDetail("无法创建 offline stream")
		e.fail(apiErr)
		return asr.FinalResult{}, apiErr
	}
	defer sherpa.DeleteOfflineStream(stream)

	// ARCH §4.3 要求内部统一 16k；SampleRateOrDefault 已把 0 归一为 16000。
	stream.AcceptWaveform(seg.SampleRateOrDefault(), seg.Samples)

	e.mu.Lock()
	rec.Decode(stream)
	e.mu.Unlock()

	res := stream.GetResult()
	out := asr.FinalResult{EngineID: e.ID(), ElapsedMs: time.Since(started).Milliseconds()}
	if res != nil {
		out.Text = strings.TrimSpace(res.Text)
		out.Lang = res.Lang
	}
	return out, nil
}

// Health 返回当前状态。
func (e *Engine) Health(_ context.Context) asr.HealthStatus {
	e.mu.Lock()
	defer e.mu.Unlock()
	note := ""
	if e.last != nil {
		note = e.last.Message
	}
	return asr.HealthStatus{
		State:    e.state,
		EngineID: e.ID(),
		LoadMs:   e.loadMs,
		Note:     note,
		Err:      e.last,
	}
}

// Close 释放 recognizer（幂等）。
func (e *Engine) Close() error {
	e.mu.Lock()
	rec := e.recognizer
	e.recognizer = nil
	e.closed = true
	e.state = contract.EngineStatePaused
	e.mu.Unlock()
	if rec != nil {
		sherpa.DeleteOfflineRecognizer(rec)
	}
	return nil
}

func (e *Engine) fail(err error) {
	apiErr := apierr.From(err, apierr.CodeEngineLoad)
	e.mu.Lock()
	e.last = apiErr
	e.state = contract.EngineStateFailed
	e.mu.Unlock()
	logx.For("asr.local").Error("本地引擎失败", "engineId", e.ID(), "code", apiErr.Code)
}
