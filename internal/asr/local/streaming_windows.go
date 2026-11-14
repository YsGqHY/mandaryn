//go:build windows && cgo && !nollm

// 本文件是**流式**（online）本地引擎：边录边转（FR-03-10）用。
//
// 与离线引擎（local_windows.go）的关系：
//   - 离线：一句音频进、一句文本出（`OfflineRecognizer`），延后转写用；
//   - 流式：一小块一小块音频进、增量文本出（`OnlineRecognizer`），边录边转用。
//
// 两者**不互相替代**：没有流式模型包时不允许拿离线引擎冒充流式
// （否则边录边转就会「显示成功但一句字都出不来」），必须报不可用。
package local

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"time"

	sherpa "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/asr"
	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/utils/logx"
)

// StreamingEngine 是基于 sherpa-onnx `OnlineRecognizer` 的流式本地引擎。
//
// 并发约束（两层，缺一不可）：
//  1. 语义上：解码器有内部状态，`Accept` 只允许**单一消费者**
//     （调用方 `transcribe.LiveRunner` 只有一条推理 goroutine，且
//     `asr.LazyStreaming` 再加一层 `callMu` 串行化）；
//  2. 实现上：即便如此，`Close` 仍可能与正在跑的 `Accept` 并发（收尾 flush 与
//     退出重叠、关旁路时热切换），所以内部用 mu 保护 recognizer/stream 指针 +
//     `closed` 标志 —— 宁可在关闭后让 `Accept` 返回 `ERR_ENGINE_NOT_READY`，
//     也绝不让 CGO 拿着已释放的指针。
type StreamingEngine struct {
	spec Spec

	mu sync.Mutex
	// recognizer 是 online recognizer（内含 transducer 三件套 + 特征抽取器）。
	recognizer *sherpa.OnlineRecognizer
	// stream 是当前解码流；每开始一个新会话（`Reset`）换一个新的。
	stream *sherpa.OnlineStream
	state  string
	loadMs int64
	last   *apierr.Err
	closed bool

	// delta 把 sherpa 的「全量文本」裁成 `Accept` 需要的「增量文本」。
	//
	// 裁剪逻辑本身放在纯 Go 的 `internal/asr` 里（`asr.StreamingDelta`），
	// 因为它是**引擎无关**的知识、且需要无 CGO 的单测覆盖（见 delta.go 顶注）。
	// 本结构只负责把它的状态跟 recognizer 的生命周期绑在一起（Reset / Close 一起清）。
	delta asr.StreamingDelta
}

// 编译期断言：本实现满足 asr.StreamingEngine（Engine + Accept + Reset）。
var _ asr.StreamingEngine = (*StreamingEngine)(nil)

// NewStreaming 构造流式本地引擎（不加载模型，加载发生在 Warm）。
func NewStreaming(spec Spec) (asr.StreamingEngine, error) {
	if strings.TrimSpace(spec.Dir) == "" {
		return nil, apierr.New(apierr.CodeModelNotFound).WithDetail("未指定流式模型包目录")
	}
	if strings.TrimSpace(spec.BundleID) == "" {
		return nil, apierr.New(apierr.CodeModelNotFound).WithDetail("未指定流式模型包 ID")
	}
	return &StreamingEngine{spec: spec, state: contract.EngineStateLoading}, nil
}

// NewStreamingFactoryWithThreads 构造流式引擎工厂（装配层注入给 `RouterOptions.Streaming`）。
//
// 与 `NewFactoryWithThreads` 同形，两个差别：
//   - 按 **streaming 角色**取包（`asr.RoleStreaming`），不是离线包；
//   - 模型包没装时**不**返回 nil 接口，而是在**调用工厂时**返回
//     `ERR_MODEL_NOT_FOUND`（`LocateBundle` 的错误原样透传，与离线工厂一致）。
//
// threads 语义同离线工厂：**工厂被调用时**读一次档位，因此档位变化在「下一次加载
// 流式引擎」（下一次开启边录边转 / 下一个会话）时带上，不是热生效（见 `asr.LoadThreads`）。
func NewStreamingFactoryWithThreads(loc asr.BundleLocator, threads func() int) asr.StreamingFactory {
	return func() (asr.StreamingEngine, error) {
		if loc == nil {
			return nil, apierr.New(apierr.CodeLocalUnavailable).
				WithDetail("未装配模型包定位器")
		}
		dir, bundleID, err := loc.LocateBundle(asr.RoleStreaming)
		if err != nil {
			return nil, err
		}
		return NewStreaming(Spec{
			BundleID: bundleID,
			Dir:      dir,
			Version:  versionFromBundleID(bundleID),
			Threads:  asr.LoadThreads(threads),
		})
	}
}

// ID 返回 bundleId@version（无版本时只返回 bundleId）。
func (e *StreamingEngine) ID() string {
	if strings.TrimSpace(e.spec.Version) == "" {
		return e.spec.BundleID
	}
	return e.spec.BundleID + "@" + e.spec.Version
}

// Warm 加载流式模型并建一条初始解码流（幂等）。
//
// 失败分类照离线实现（ARCH §4.2.5）：
//   - 目录里认不出 transducer 三件套 / 缺 tokens.txt → ERR_MODEL_NOT_FOUND / ERR_MODEL_CORRUPT
//   - 构造 recognizer 失败（内存不足、缺 onnxruntime.dll）→ ERR_ENGINE_LOAD
func (e *StreamingEngine) Warm(ctx context.Context) error {
	e.mu.Lock()
	ready := e.recognizer != nil && e.stream != nil && e.state == contract.EngineStateReady
	closed := e.closed
	e.mu.Unlock()
	if ready {
		return nil // 幂等：已加载
	}
	if closed {
		return apierr.New(apierr.CodeEngineNotReady).WithDetail("流式引擎已关闭")
	}

	started := time.Now()

	spec, err := asr.DetectBundle(e.spec.Dir)
	if err != nil {
		e.fail(err)
		return err
	}
	// 流式解码器只认 transducer 三件套（encoder/decoder/joiner）。
	// 单文件模型（sensevoice / paraformer）**不能**当流式用：宁可明确报错，
	// 也不要构造一个「能加载但永不出字」的 recognizer。
	if spec.Kind != asr.BundleKindTransducer {
		apiErr := apierr.New(apierr.CodeModelCorrupt).WithDetail(
			"流式模型包必须是 transducer 三件套（encoder/decoder/joiner），当前识别为 %s",
			spec.Kind)
		e.fail(apiErr)
		return apiErr
	}

	cfg := buildOnlineConfig(spec, e.spec)
	rec := sherpa.NewOnlineRecognizer(&cfg)
	if rec == nil {
		apiErr := apierr.New(apierr.CodeEngineLoad).WithDetail(
			"流式引擎加载失败（可能缺少 onnxruntime.dll、内存不足或模型损坏）: %s",
			filepath.Base(e.spec.Dir))
		e.fail(apiErr)
		return apiErr
	}
	stream := sherpa.NewOnlineStream(rec)
	if stream == nil {
		sherpa.DeleteOnlineRecognizer(rec)
		apiErr := apierr.New(apierr.CodeEngineLoad).WithDetail("无法创建 online stream")
		e.fail(apiErr)
		return apiErr
	}

	e.mu.Lock()
	// 双检：拿锁期间可能已有人加载完（Warm 应当幂等，但别依赖调用方守规矩）。
	if e.recognizer != nil && !e.closed {
		e.mu.Unlock()
		sherpa.DeleteOnlineStream(stream)
		sherpa.DeleteOnlineRecognizer(rec)
		return nil
	}
	e.recognizer = rec
	e.stream = stream
	e.delta.Reset()
	e.state = contract.EngineStateReady
	e.loadMs = time.Since(started).Milliseconds()
	e.last = nil
	e.mu.Unlock()

	// 只记元数据（模型 ID / 类型 / 线程数 / 加载耗时），不记任何音频与正文（NFR-03-5）。
	logx.For("asr.local").Info("流式引擎加载完成",
		"engineId", e.ID(), "kind", spec.Kind, "loadMs", e.loadMs,
		"threads", threadsOrDefault(e.spec.Threads))
	return nil
}

// buildOnlineConfig 按 transducer 三件套拼 online recognizer 配置。
//
// 几个刻意的选择：
//   - **端点检测（EnableEndpoint）= 0**：分句由上层 `transcribe.UtteranceSplitter`
//     按**帧计数**做（ARCH §2.3 / §4.3）。若让 sherpa 自己也切句，两套分句会互相打架：
//     sherpa 的 endpoint 会在内部 Reset，导致「部分句子凭空消失」，而落库的
//     `(session_id, seq)` 编号来自上层的确定性分句，两边错位就会覆盖错行。
//   - **greedy_search**：与离线实现同款，CPU 占用最低；流式路径要跟着实时跑，
//     不该为了几个千分点准确率去用 modified_beam_search 吃掉一档线程预算。
//   - 特征 16k / 80 维：sherpa 官方预训练模型都是这个口径（与离线一致）。
func buildOnlineConfig(spec asr.BundleSpec, local Spec) sherpa.OnlineRecognizerConfig {
	var cfg sherpa.OnlineRecognizerConfig
	cfg.FeatConfig.SampleRate = 16000
	cfg.FeatConfig.FeatureDim = 80
	cfg.ModelConfig.Transducer.Encoder = spec.Encoder
	cfg.ModelConfig.Transducer.Decoder = spec.Decoder
	cfg.ModelConfig.Transducer.Joiner = spec.Joiner
	cfg.ModelConfig.Tokens = spec.Tokens
	cfg.ModelConfig.NumThreads = threadsOrDefault(local.Threads)
	cfg.ModelConfig.Provider = "cpu"
	cfg.ModelConfig.Debug = 0
	cfg.DecodingMethod = "greedy_search"
	cfg.EnableEndpoint = 0
	return cfg
}

// Accept 送一段 16kHz 单声道样本，返回**本次新增**的增量文本（可能为空串）。
//
// 流程：`AcceptWaveform` → 只要 `IsReady` 就一直 `Decode` → 取全量结果 → 裁掉已发出前缀。
//
// **增量语义的实现假设**（重要，别当成理所当然）：
// sherpa 的 `GetResult` 返回的是「自上次 Reset 以来的全量文本」，不是增量。
// transducer + greedy_search 的输出在实际中是**单调增长**的（已提交的 token 不会撤回），
// 因此「全量文本 = 已发出前缀 + 新字」这个等式基本成立，代码按它裁前缀。
// 万一模型改写了已发出的字（真机上是极少数），接口没有「撤回」能力，只能：
//   - 把差异部分（新文本去掉最长公共前缀后的尾巴）当作新增返回 —— 于是定稿里会留下
//     被改写前的旧字（多字，但不会丢字）；
//   - `revisions` 计数 +1，日志里只记**次数**，绝不记正文。
//
// 空样本直接返回空串：`OnlineStream.AcceptWaveform` 内部要取 `&samples[0]`，
// 传空切片会 panic（CGO 侧越界），这是必须在这一层挡掉的边界。
func (e *StreamingEngine) Accept(samples []float32) (string, error) {
	if len(samples) == 0 {
		return "", nil
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return "", apierr.New(apierr.CodeEngineNotReady).WithDetail("流式引擎已关闭")
	}
	if e.recognizer == nil || e.stream == nil {
		return "", apierr.New(apierr.CodeEngineNotReady).
			WithDetail("流式模型尚未加载完成")
	}

	// 音频只进 CGO，不落日志、不进错误信息（NFR-03-5）。
	e.stream.AcceptWaveform(16000, samples)
	for e.recognizer.IsReady(e.stream) {
		e.recognizer.Decode(e.stream)
	}

	res := e.recognizer.GetResult(e.stream)
	if res == nil {
		return "", nil
	}
	return e.delta.Next(res.Text), nil
}

// Reset 清空解码状态（开始新会话前调用），幂等。
//
// 实现方式选「换一条新 stream」而不是 `recognizer.Reset(stream)`：
// 两者都会清 NN 状态，但换新 stream 连**特征缓冲里尚未解码的尾巴**也一起丢掉 ——
// 上一个会话的残余音频跟着清干净，不会漏进下一个会话的首句。
// 代价是新建 stream 要重新分配一块 buffer，而 Reset 只在会话边界发生（每会话一次），
// 这点开销无关紧要。
func (e *StreamingEngine) Reset() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil // Close 之后再 Reset 不是故障：幂等，什么都不做。
	}
	if e.recognizer == nil {
		return nil // 未加载：没有状态可清（加载后从空状态开始）。
	}
	stream := sherpa.NewOnlineStream(e.recognizer)
	if stream == nil {
		return apierr.New(apierr.CodeEngineLoad).WithDetail("无法创建 online stream")
	}
	old := e.stream
	e.stream = stream
	e.delta.Reset()
	if old != nil {
		sherpa.DeleteOnlineStream(old)
	}
	return nil
}

// Feed 在边录边转路径下不用：增量文本走 `Accept`。
//
// 返回已关闭的空 channel（与离线实现一致），调用方据「channel 立刻结束」判定
// 「本引擎不提供 partial 流」。**不要**把 `Feed` 当成流式能力用 ——
// 那正是 `StreamingEngine` 单独成接口的原因（见 internal/asr/streaming.go 顶注）。
func (e *StreamingEngine) Feed(_ []float32) (<-chan asr.Partial, error) {
	ch := make(chan asr.Partial)
	close(ch)
	return ch, nil
}

// Finalize **不**做离线定稿：本引擎只提供流式增量。
//
// 第二遍离线校正（ARCH §2.3 的两遍法）由 `transcribe.LiveDeps.Refine` 注入 ——
// 那一层才知道「有没有离线模型包、预算档位允不允许」，引擎内硬编码离线推理会把
// 「流式包 + 离线包」两个依赖耦在一起，也让 L1 档（关掉第二遍）无法生效。
//
// 因此这里返回明确的**能力缺失**错误，而不是「成功但空文本」：
// 空文本会静默吞掉调用方的期望（边录边转「显示成功却一个字没有」），
// 错误码则能被上层归类成「旁路掉线、录制继续」。
func (e *StreamingEngine) Finalize(_ asr.AudioSegment) (asr.FinalResult, error) {
	return asr.FinalResult{}, apierr.New(apierr.CodeLocalUnavailable).
		WithDetail("流式引擎不做离线定稿（第二遍校正由 Refine 注入）")
}

// Health 返回当前状态。
func (e *StreamingEngine) Health(_ context.Context) asr.HealthStatus {
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

// Close 释放 recognizer 与 stream（幂等，可与 Accept 并发）。
//
// 释放顺序：先摘指针、再释放 C 资源，且全程持锁 —— 于是并发的 `Accept`
// 要么在释放前拿锁跑完，要么在释放后看到 `closed` 直接返回错误，
// 不会出现「拿着已 delete 的指针进 CGO」。
func (e *StreamingEngine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	rec, stream := e.recognizer, e.stream
	e.recognizer, e.stream = nil, nil
	e.closed = true
	e.state = contract.EngineStatePaused
	e.delta.Reset()
	if stream != nil {
		sherpa.DeleteOnlineStream(stream)
	}
	if rec != nil {
		sherpa.DeleteOnlineRecognizer(rec)
	}
	return nil
}

func (e *StreamingEngine) fail(err error) {
	apiErr := apierr.From(err, apierr.CodeEngineLoad)
	e.mu.Lock()
	e.last = apiErr
	e.state = contract.EngineStateFailed
	e.mu.Unlock()
	logx.For("asr.local").Error("流式引擎失败", "engineId", e.ID(), "code", apiErr.Code)
}

// StreamingRevisions 已发生的「文本改写」次数（诊断用，供真机测试量化增量假设）。
func (e *StreamingEngine) StreamingRevisions() int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.delta.Revisions()
}
