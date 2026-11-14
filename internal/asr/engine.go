// Package asr 是「转写引擎」的 L2 领域层（FR-01 / ARCH §4.2）。
//
// 职责边界：
//   - 定义统一引擎接口 Engine，以及本地（local）/ 在线（online）两种实现；
//   - 用 Router 实现来源选择、热切换与降级（严格按 ARCH §4.2.4 矩阵）；
//   - 只做转写，不碰录制、落盘、持久化（那些属于 recording / storage 域）。
//
// 铁律：
//   - 本包**不 import Wails**、不 import internal/models —— 因此可被单测直接驱动；
//     状态通知走注入回调（`func(contract.EngineStatus)`），事件由 L3 的 enginesvc 发。
//   - 本地引擎失败**绝不**降级到在线（隐私默认，ARCH §4.2.4 最后一行）。
//   - 任何出站请求都经 httpx + outbound 留痕，并受 MANDARYN_OFFLINE 约束。
package asr

import (
	"context"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
)

// 模型包角色（modelssvc 的 BundleInfo.Roles 用同一套取值）。
const (
	// RoleStreaming 流式模型（边录边转，live_transcribed）。
	RoleStreaming = "streaming"
	// RoleOffline 非流式（整句/整段）模型，延后转写用这个。
	RoleOffline = "offline"
	// RoleVAD 语音活动检测模型。
	RoleVAD = "vad"
	// RoleSpeaker 声纹模型。
	RoleSpeaker = "speaker"
)

// HealthState 与 contract.EngineStatus.State 的字符串集合保持一致
// （loading/ready/failed/degraded/paused），不另造枚举（见 docs/API.md §2.5）。
type HealthState = string

// AudioSegment 是送进引擎的一「句」音频。
//
// 时间基说明：StartMs/EndMs 由调用方按**帧计数**换算（`frameCount / 16000 * 1000`），
// 不使用 wall clock（ARCH §4.3）。
type AudioSegment struct {
	// Samples 16kHz 单声道 PCM，取值范围 [-1, 1]。
	Samples []float32
	// SampleRate 采样率；0 视为 16000。
	SampleRate int
	// SessionID 所属录音实例；可为空（离线试转）。
	SessionID string
	// Seq 会话内片段序号（从 0），用于幂等覆盖。
	Seq int
	// StartMs / EndMs 时间轴区间（毫秒，帧计数换算）。
	StartMs int64
	EndMs   int64
}

// SampleRateOrDefault 返回实际采样率（0 归一为 16000）。
func (s AudioSegment) SampleRateOrDefault() int {
	if s.SampleRate <= 0 {
		return 16000
	}
	return s.SampleRate
}

// Partial 是流式部分结果（Feed 返回的 channel 元素）。
//
// 延后转写模式（默认）不产生 partial；只有「边录边转」开关（FR-03-10）
// 打开时才用得上。
type Partial struct {
	// Text 当前累计文本。
	Text string
	// EngineID 产出该结果的引擎（bundleId@version 或 online:<model>）。
	EngineID string
	// AtMs 该结果对应的时间轴位置。
	AtMs int64
	// FinalIndex 该句在会话内的序号（与 AudioSegment.Seq 对齐）。
	FinalIndex int
}

// FinalResult 是定稿结果（Finalize 的返回值）。
type FinalResult struct {
	// Text 定稿文本。
	Text string
	// EngineID 产出该片段的引擎（降级前后可能不同，落 segments.engine_id 以便追溯）。
	EngineID string
	// Confidence 置信度 0–1；端点不给则为 0。
	Confidence float32
	// Lang 识别出的语种（sensevoice / whisper 会给，其余为空）。
	Lang string
	// LoadMs 该次识别耗时（毫秒），仅用于诊断日志，不落库。
	ElapsedMs int64
}

// HealthStatus 引擎健康详情。
type HealthStatus struct {
	// State loading | ready | failed | degraded | paused。
	State HealthState
	// EngineID 当前引擎标识。
	EngineID string
	// LoadMs 预热耗时（毫秒）。
	LoadMs int64
	// Note 中文短说明（面向用户，可进非阻塞提示）。
	Note string
	// Err 最近一次错误（已归类，含 ERR_* 码）。
	Err *apierr.Err
}

// Ready 报告引擎当前是否可承接 Finalize。
func (h HealthStatus) Ready() bool {
	return h.State == contract.EngineStateReady || h.State == contract.EngineStateDegraded
}

// Engine 是本地 / 在线引擎的统一接口（ARCH §4.2.1）。
//
// 实现方约定：
//   - Warm 幂等：重复调用不得重复加载；
//   - Feed 在延后转写模式下可返回一个**已关闭**的空 channel；
//   - Close 幂等，且必须释放底层资源（本地：onnx recognizer；在线：无）。
type Engine interface {
	// ID 返回 bundleId@version 或 online:<model>。
	ID() string
	// Warm 加载模型 / 握手探测；幂等。
	Warm(ctx context.Context) error
	// Feed 流式送帧，返回部分结果流（延后转写模式返回已关闭的空 channel）。
	Feed(frame []float32) (<-chan Partial, error)
	// Finalize 对一整句做离线校正，返回定稿文本。
	Finalize(seg AudioSegment) (FinalResult, error)
	// Health 返回当前健康状态。
	Health(ctx context.Context) HealthStatus
	// Close 释放资源；幂等。
	Close() error
}

// BundleLocator 由 app 装配时注入，用来把「角色」解析成模型包目录。
//
// 之所以用接口而不是直接 import internal/models：asr 是 L2 领域包，
// 不允许依赖 models 包（双向依赖 + 单测难驱动）。S6 的 models 包实现它。
type BundleLocator interface {
	// LocateBundle 返回该角色对应模型包的目录与 bundleId。
	// 目录不存在 → ERR_MODEL_NOT_FOUND；校验失败 → ERR_MODEL_CORRUPT。
	LocateBundle(role string) (dir string, bundleID string, err error)
}

// LocalFactory 构造本地引擎（每次热切换都新建实例，旧实例随后被关闭）。
//
// 由 app 装配注入：Windows+CGO 下给真正的 sherpa-onnx 实现，
// 其它平台给「本地不可用」实现（ERR_LOCAL_UNAVAILABLE）。
type LocalFactory func() (Engine, error)

// OnlineFactory 用当前在线配置构造在线引擎。
//
// 配置变更（SetOnlineConfig）后 Router 会重新调用它，因此工厂必须
// **每次读取最新配置**（enginesvc 在闭包里读 appsettings）。
type OnlineFactory func() (Engine, error)

// —— 错误构造 ——

// errNotReady 引擎未就绪（可直接告诉用户「等模型加载完」）。
func errNotReady(detail string) *apierr.Err {
	return apierr.New(apierr.CodeEngineNotReady).WithDetail("%s", detail)
}

// errLocalUnavailable 本构建不含本地引擎（非 Windows 平台 / 未编译 CGO）。
func errLocalUnavailable(detail string) *apierr.Err {
	return apierr.New(apierr.CodeLocalUnavailable).WithDetail("%s", detail)
}

// errEngineLoad 引擎加载失败（内存不足 / 库缺失）。
func errEngineLoad(detail string) *apierr.Err {
	return apierr.New(apierr.CodeEngineLoad).WithDetail("%s", detail)
}
