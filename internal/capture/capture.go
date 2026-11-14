// Package capture 是「应用级音频捕获」领域包（FR-02 / ARCH §4.1）。
//
// 分层约束：
//   - 本包属于 L2 领域层，**禁止 import Wails**，因此可被单测与命令行工具直接驱动；
//   - 与前端共享的 DTO 一律用 internal/contract（Source / CaptureCaps），
//     本包不重复定义，避免前后端字段漂移；
//   - 平台差异全部沉到 *_windows.go / *_darwin.go / *_linux.go，
//     本文件只放跨平台类型与对平台的四个出口（Capabilities / Enumerate / Probe / Open）。
//
// 职责边界（很重要）：
//   - 本包只负责「拿到某个源的 PCM 帧」，**不做重采样、不做混音、不落盘、不推理**。
//     源采样率/声道如实通过 AudioFormat 上报，重采样到 16kHz 单声道是音频管线（S2）的事。
//   - 录制与转写解耦（FR-03）由上层 recordingsvc 负责，本包不认识「会话」概念。
package capture

import (
	"log/slog"
	"strings"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/utils/logx"
)

// lg 返回捕获域的 logger。
//
// 不在包级变量里缓存 logx.For 的结果：logx.Init 晚于包初始化，
// 缓存会把 fallback logger 永久固化，导致日志进不了文件。
func lg() *slog.Logger { return logx.For("capture") }

// AudioFormat 是源的原始音频格式（未做任何转换）。
//
// 为什么不做归一化：重采样会毁掉「原始音频保真」，而落盘与推理需要的
// 16kHz 单声道由音频管线统一处理（ARCH §4.3）。捕获层如实上报才是可测的。
type AudioFormat struct {
	// SampleRate 采样率（Hz），WASAPI 共享模式通常是 48000。
	SampleRate int
	// Channels 声道数，通常是 2。
	Channels int
	// Float32 true=样本是 float32（WAVE_FORMAT_IEEE_FLOAT，
	// 共享模式的混合格式一般是这个）；false=整型 PCM（16/24/32 bit），
	// ReadPCM 已把整型按比例转成 [-1,1] 的 float32。
	Float32 bool
}

// Channel 是一个已打开的捕获通道。
//
// 实现约定：
//   - ReadPCM 阻塞直到至少有 1 个样本可读，返回本次拷贝的**样本数**（不是帧数，
//     已含声道展开：dst 是 interleaved 的，L R L R …）；
//   - 上层关闭通道后 ReadPCM 返回 io.EOF；
//   - Err 用于「录制中设备失效 / 应用退出」的被动通知（ARCH §4.1.4），
//     它不是 ReadPCM 的错误来源：ReadPCM 仍会先吐完缓冲再返回 EOF。
type Channel interface {
	// ReadPCM 把 interleaved float32 样本读进 dst，返回拷贝的样本数。
	ReadPCM(dst []float32) (n int, err error)
	// Format 返回源的原始格式；Open 成功后即可安全调用。
	Format() AudioFormat
	// Err 在通道因外部原因终止时收到一个错误，随后 channel 被关闭。
	// 上层用它触发重连（UC-03）；主动 Close 不会往这里塞错误。
	Err() <-chan error
	// Close 幂等释放 COM / 句柄 / 采集线程。
	Close() error
}

// 源 ID 方案（contract.Source.ID 的编码规则，ARCH §4.1.1）。
//
// 应用源用「可执行名」而不是 PID：目标应用重启后 PID 会变，但用户意图是
// 「继续抓 KOOK」，所以 ID 必须跨进程重启稳定，重连时再按 exe 解析进程树。
const (
	// DeviceIDPrefix 设备源前缀，形如 dev:{0.0.0.00000000}.{guid}。
	DeviceIDPrefix = "dev:"
	// AppIDPrefix 应用源前缀，形如 app:kook.exe（exe 名小写）。
	AppIDPrefix = "app:"
)

// ProducingPeakThreshold 判定「最近是否有音频」的电平阈值（0–1 峰值）。
//
// 依据：共享模式回环在高概率下会持续收到静音帧，光看「有没有数据」无法
// 区分「在抓但无声」与「抓不到」（ARCH §4.1.3），所以用峰值电平均值判断。
// 0.001 约等于 -60dBFS，低于人耳可辨内容，只用于过滤数字静音。
const ProducingPeakThreshold = 0.001

// Capabilities 返回平台捕获能力矩阵（API-1.6）。
//
// 各平台实现必须**如实**填写：声称支持而 Open 时失败，会让 UI 给出错误引导。
func Capabilities() contract.CaptureCaps { return capabilities() }

// Enumerate 枚举当前可捕获的源：应用源在前（按显示名排序），设备源在后
// （默认输出设备排最前）。
//
// 应用源基于「当前正在使用音频的会话」推导，因此只列出**正在发声/刚发声**的
// 应用；已安装但没在放音频的应用不会出现（Windows 没有「潜在音频应用」列表）。
func Enumerate() ([]contract.Source, error) { return enumerate() }

// Probe 探测单个源当前是否可用 / 有声（API-1.3）。
//
// 参数非法（不是 dev:/app: 前缀）返回 ERR_INVALID_ARGUMENT；
// 源已消失（设备拔出 / 应用退出）返回 ERR_SOURCE_NOT_FOUND。
func Probe(sourceID string) (contract.Source, error) {
	kind, value, err := ParseSourceID(sourceID)
	if err != nil {
		return contract.Source{}, err
	}
	return probe(kind, value)
}

// Open 打开一个捕获通道（API-2.x 的底层实现）。
//
// 只支持设备回环（整机混音 / 指定渲染设备）。应用级分路捕获在
// Windows 需要 Build 20348+ 且本版本尚未实现，因此对应用源返回
// ERR_CAPTURE_UNAVAILABLE 并提示改用设备源，**不会**静默退化成「抓全部声音」——
// 那会让用户以为分路成功、事后才发现录进了别人的音频（隐私事故）。
func Open(sourceID string) (Channel, error) {
	kind, value, err := ParseSourceID(sourceID)
	if err != nil {
		return nil, err
	}
	return open(kind, value)
}

// ParseSourceID 把 contract.Source.ID 拆成 (kind, value)。
//
// 返回的 err 是 *apierr.Err（ERR_INVALID_ARGUMENT），可直接透传给前端。
func ParseSourceID(sourceID string) (kind, value string, err error) {
	id := strings.TrimSpace(sourceID)
	switch {
	case strings.HasPrefix(id, DeviceIDPrefix):
		v := strings.TrimSpace(id[len(DeviceIDPrefix):])
		if v == "" {
			return "", "", apierr.New(apierr.CodeInvalidArgument).
				WithDetail("设备源 ID 缺少设备标识：%q", sourceID)
		}
		return contract.SourceKindDevice, v, nil
	case strings.HasPrefix(id, AppIDPrefix):
		v := strings.ToLower(strings.TrimSpace(id[len(AppIDPrefix):]))
		if v == "" {
			return "", "", apierr.New(apierr.CodeInvalidArgument).
				WithDetail("应用源 ID 缺少可执行名：%q", sourceID)
		}
		return contract.SourceKindApplication, v, nil
	default:
		return "", "", apierr.New(apierr.CodeInvalidArgument).
			WithDetail("无法识别的源 ID：%q（应以 dev: 或 app: 开头）", sourceID)
	}
}

// DeviceSourceID 构造设备源 ID。
func DeviceSourceID(deviceID string) string { return DeviceIDPrefix + deviceID }

// AppSourceID 构造应用源 ID（exe 名统一小写，保证跨次枚举稳定）。
func AppSourceID(exeName string) string {
	return AppIDPrefix + strings.ToLower(exeName)
}

// sourceUnavailable 构造「此平台/此版本不支持该源」的错误，统一带上原因，
// 便于「查看诊断」与前端提示复用同一段文案。
func sourceUnavailable(reason string) *apierr.Err {
	return apierr.New(apierr.CodeCaptureUnavailable).WithDetail("%s", reason)
}

// sourceNotFound 构造「源已不存在」的错误。
func sourceNotFound(detail string) *apierr.Err {
	return apierr.New(apierr.CodeSourceNotFound).WithDetail("%s", detail)
}
