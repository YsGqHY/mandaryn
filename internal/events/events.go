// Package events 集中声明前后端共享的事件名，并按 DOC-03 §3 注册强类型载荷。
//
// 为什么集中：事件名是跨端契约，散落在各处会出现「后端 emit 了但前端没订阅」
// 的静默失败。所有事件名只在这里定义，前端在 services/ 层订阅。
//
// 命名规范：`domain:event`（DOC-03 §1.3），常量名用 PascalCase。
package events

import (
	"github.com/wailsapp/wails/v3/pkg/application"

	"vip.oberon.mandaryn/internal/contract"
)

// 事件名（DOC-03 §3 全表，共 16 个 + 脚手架遗留的 Time）。
const (
	Time = "time"

	// EV-01 状态机每次转换（DOC-02 §6.2）。
	RecordingState = "recording:state"
	// EV-02 采集电平，10Hz 节流。
	RecordingLevel = "recording:level"
	// EV-03 采集节拍，1Hz（UI 计时与已存大小）。
	RecordingTick = "recording:tick"

	// EV-04 流式出字（仅边录边转开启时）。
	TranscriptPartial = "transcript:partial"
	// EV-05 逐句定稿（延后转写与边录边转都会发）。
	TranscriptSegment = "transcript:segment"
	// EV-06 声纹归并完成。
	TranscriptSpeaker = "transcript:speaker"
	// EV-16 转写任务状态（入队/开跑/完成/失败/被抢占）。
	TranscriptionState = "transcription:state"

	// EV-07 角色改名 / 合并 / 拆分（FR-06-3 全局同步）。
	RolesChanged = "roles:changed"

	// EV-08 引擎预热 / 降级 / 恢复 / 失败。
	EngineStatus = "engine:status"

	// EV-09 下载进度，4Hz 节流。
	ModelsDownload = "models:download"
	// EV-10 仓库变化（安装 / 删除 / 校验完成）。
	ModelsChanged = "models:changed"
	// EV-11 迁移进度。
	ModelsMigrate = "models:migrate"

	// EV-12 源列表变化（应用启动/退出、设备增减）。
	CaptureSources = "capture:sources"
	// EV-13 源失效 / 重连中。
	CaptureInvalid = "capture:invalid"

	// EV-14 应用设置变更（既有事件；payload 为字段名，历史例外不扩散）。
	AppSettingsChanged = "appsettings:changed"

	// EV-15 性能预算采样，1Hz。
	PerfUsage = "perf:usage"

	// EV-17 热键触发了「实现位于前端」的动作（导航 / 播放器 / 搜索聚焦）。
	HotkeyTrigger = "hotkey:trigger"

	// EV-18 快捷键绑定变化（单个动作，含注册失败原因）。
	HotkeysChanged = "hotkeys:changed"

	// 精简模式成功持久化；各窗口据此重新读取完整 LeanState。
	LeanModeChanged = "ux:leanChanged"
)

// Register 在 application 初始化前注册所有强类型事件，
// Wails 的绑定生成器会据此为前端生成类型化 API。
//
// 新增事件必须同时：① 在此登记 ② 更新 DOC-03 §3 ③ 在 frontend services/ 订阅。
func Register() {
	application.RegisterEvent[string](Time)
	application.RegisterEvent[string](AppSettingsChanged)

	application.RegisterEvent[contract.RecordingState](RecordingState)
	application.RegisterEvent[contract.RecordingLevel](RecordingLevel)
	application.RegisterEvent[contract.RecordingTick](RecordingTick)

	application.RegisterEvent[contract.Segment](TranscriptPartial)
	application.RegisterEvent[contract.Segment](TranscriptSegment)
	application.RegisterEvent[contract.SpeakerAssigned](TranscriptSpeaker)
	application.RegisterEvent[contract.TranscriptionState](TranscriptionState)

	application.RegisterEvent[contract.RoleChanged](RolesChanged)

	application.RegisterEvent[contract.EngineStatus](EngineStatus)

	application.RegisterEvent[contract.DownloadState](ModelsDownload)
	application.RegisterEvent[contract.ModelChanged](ModelsChanged)
	application.RegisterEvent[contract.MigrateState](ModelsMigrate)

	application.RegisterEvent[contract.CaptureSourcesChanged](CaptureSources)
	application.RegisterEvent[contract.CaptureInvalid](CaptureInvalid)

	application.RegisterEvent[contract.PerfUsage](PerfUsage)

	application.RegisterEvent[contract.HotkeyTrigger](HotkeyTrigger)
	application.RegisterEvent[contract.Binding](HotkeysChanged)
	application.RegisterEvent[bool](LeanModeChanged)
}

// Emitter 抽象「向后端 emit 事件」的能力，便于 L3 服务被单测驱动。
//
// L2 领域层不 import Wails，因此它只暴露回调；L3 把回调桥接到 app.Event.Emit。
// nil 时必须静默跳过（启动早期或测试环境）。
type Emitter interface {
	Emit(name string, data any)
}

// Emit 是带 nil 检查的便捷函数。
func Emit(em Emitter, name string, data any) {
	if em == nil {
		return
	}
	em.Emit(name, data)
}
