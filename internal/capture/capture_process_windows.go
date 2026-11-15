//go:build windows

package capture

import (
	"fmt"
	"sort"

	"github.com/moutend/go-wca/pkg/wca"
	"golang.org/x/sys/windows"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
)

// 本文件实现「按应用分路捕获」的采集路径：WASAPI 进程级回环（FR-02）。
//
// 与设备回环（capture_windows.go 的 openLoopback）的区别**只在激活与初始化**：
//   - 设备回环：IMMDevice::Activate（同步）拿到 IAudioClient，且能用 GetMixFormat；
//   - 进程回环：必须走 ActivateAudioInterfaceAsync + AUDIOCLIENT_ACTIVATION_PARAMS
//     （异步，见 probe_windows.go），且**不能**调 GetMixFormat，格式要自己定。
//
// 取到 IAudioClient 之后的一切（事件等待、取包、格式转换、队列、溢出丢最旧、
// Close 语义）与设备回环完全一致，因此复用 loopbackStream / loopbackChannel，
// 不另外造一套采集循环。

const (
	// processLoopbackSampleRate 进程回环使用固定采样率。
	//
	// 为什么不调 GetMixFormat：进程级回环操作的是「虚拟设备」
	//（VAD\Process_Loopback），它没有设备混合格式可查，必须由调用方指定。
	// 官方 ApplicationLoopback 样例同样使用 44100/16bit/2ch，这里保持一致。
	processLoopbackSampleRate = 44100
	// processLoopbackChannels 固定双声道。
	processLoopbackChannels = 2
	// processLoopbackBits 固定 16bit 整数 PCM。
	processLoopbackBits = 16
)

// processLoopbackFormat 返回进程回环的固定 PCM 格式（WAVEFORMATEX + 解析后的 AudioFormat）。
//
// 单独成函数是为了可被单测直接断言：这份「常量格式」是进程回环里最容易写错、
// 又最难在真机上察觉的部分（写错了不会报错，只会得到变调/变速的音频）。
func processLoopbackFormat() (*wca.WAVEFORMATEX, AudioFormat, int, int) {
	blockAlign := processLoopbackChannels * processLoopbackBits / 8
	wfx := &wca.WAVEFORMATEX{
		WFormatTag:      waveFormatPCM,
		NChannels:       processLoopbackChannels,
		NSamplesPerSec:  processLoopbackSampleRate,
		NAvgBytesPerSec: processLoopbackSampleRate * uint32(blockAlign),
		NBlockAlign:     uint16(blockAlign),
		WBitsPerSample:  processLoopbackBits,
		CbSize:          0,
	}
	format := AudioFormat{
		SampleRate: processLoopbackSampleRate,
		Channels:   processLoopbackChannels,
		// 整数 PCM：ReadPCM 已把整型按比例转成 [-1,1] 的 float32，
		// 所以对外的 AudioFormat.Float32 为 false（见 AudioFormat 注释）。
		Float32: false,
	}
	return wfx, format, processLoopbackBits, blockAlign
}

// openProcessLoopback 打开「指定进程（含其子进程）」的进程级回环采集。
//
// roots 是来自进程树的根 PID（同一 exe 的多个实例会有多个根）。
//
// 为什么只取一个根：AUDIOCLIENT_PROCESS_LOOPBACK_PARAMS 只接受**单个** TargetProcessId，
// 其 INCLUDE_TARGET_PROCESS_TREE 模式会自动带上该进程的全部子孙。多个互不相干的
// 实例属于不同的树，采集层又不允许做混音（职责边界：只负责拿到某个源的 PCM 帧），
// 因此这里取第一个根，并在多于一个时打 WARN 明确记录该边界。
func openProcessLoopback(roots []int, exeName string) (*loopbackStream, error) {
	if len(roots) == 0 {
		return nil, sourceNotFound(fmt.Sprintf("应用源没有可用的进程：%s", exeName))
	}
	sort.Ints(roots)
	if len(roots) > 1 {
		// 不是错误，但会影响「抓全」的预期，必须留痕（FR-02-1 的边界）。
		lg().Warn("同一应用存在多个进程树，仅采集第一个根的子进程树",
			"exe", exeName, "roots", len(roots), "chosenPid", roots[0])
	}
	targetPID := uint32(roots[0])

	// 能力声明必须能被兑现（FR-02-4）：探测不过就诚实拒绝，
	// 绝不静默退化成设备回环（FR-02-3）。
	if ok, note := processLoopbackAvailable(); !ok {
		return nil, sourceUnavailable(fmt.Sprintf(
			"本机实测不支持进程级捕获，无法按应用分路采集 %s：%s；请改用设备源（会包含全部应用声音）",
			exeName, note))
	}

	client, hr, err := activateProcessAudioClient(targetPID)
	if err != nil {
		return nil, apierr.New(apierr.CodeCaptureUnavailable).
			WithDetail("激活进程级音频客户端失败：%v", err).WithCause(err)
	}
	if hr != hrS_OK {
		// 激活被拒：可能是目标进程已退出（PID 失效）或系统拒绝该进程。
		return nil, mapProcessLoopbackActivateError(hr, targetPID, exeName)
	}
	if client == nil {
		return nil, apierr.New(apierr.CodeCaptureUnavailable).
			WithDetail("激活进程级音频客户端返回空接口（PID=%d）", targetPID)
	}

	stream, err := initProcessLoopbackStream(client)
	if err != nil {
		client.Release()
		return nil, err
	}
	return stream, nil
}

// initProcessLoopbackStream 在已激活的 IAudioClient 上完成初始化并启动采集。
//
// 与 openLoopback 后半段（事件、Initialize、GetService、Start）逻辑一致，
// 差别只有两点：格式是固定 PCM；没有静音保活流（进程回环没有对应端点）。
func initProcessLoopbackStream(client *wca.IAudioClient) (*loopbackStream, error) {
	wfx, format, bits, blockAlign := processLoopbackFormat()

	// 手动复位事件：CreateEvent 的 manualReset=0（自动复位），与 WASAPI 文档一致。
	event, err := windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		return nil, apierr.New(apierr.CodeCaptureUnavailable).
			WithDetail("创建音频事件失败：%v", err).WithCause(err)
	}

	flags := uint32(wca.AUDCLNT_STREAMFLAGS_LOOPBACK | wca.AUDCLNT_STREAMFLAGS_EVENTCALLBACK)
	initErr := client.Initialize(
		wca.AUDCLNT_SHAREMODE_SHARED,
		flags,
		wca.REFERENCE_TIME(loopbackBufferDuration),
		0,
		wfx,
		nil,
	)
	if initErr != nil {
		// 某些系统的进程回环虚拟设备不接受纯 PCM 格式直通，需要引擎做转换。
		// 这时补上 AUTOCONVERTPCM + SRC_DEFAULT_QUALITY 再试一次；
		// 仍失败才如实上报（不要偷偷改成别的格式 —— 那会静默改变音频时序）。
		initErr = client.Initialize(
			wca.AUDCLNT_SHAREMODE_SHARED,
			flags|wca.AUDCLNT_STREAMFLAGS_AUTOCONVERTPCM|wca.AUDCLNT_STREAMFLAGS_SRC_DEFAULT_QUALITY,
			wca.REFERENCE_TIME(loopbackBufferDuration),
			0,
			wfx,
			nil,
		)
	}
	if initErr != nil {
		_ = windows.CloseHandle(event)
		return nil, mapWASAPIError(initErr, "初始化进程级回环流")
	}

	if err := client.SetEventHandle(uintptr(event)); err != nil {
		_ = windows.CloseHandle(event)
		return nil, mapWASAPIError(err, "绑定音频事件")
	}

	var capture *wca.IAudioCaptureClient
	if err := client.GetService(wca.IID_IAudioCaptureClient, &capture); err != nil || capture == nil {
		_ = windows.CloseHandle(event)
		return nil, mapWASAPIError(err, "获取采集客户端")
	}

	if err := client.Start(); err != nil {
		capture.Release()
		_ = windows.CloseHandle(event)
		return nil, mapWASAPIError(err, "启动进程级回环采集")
	}

	return &loopbackStream{
		client:     client,
		capture:    capture,
		event:      event,
		format:     format,
		bits:       bits,
		blockAlign: blockAlign,
		// 进程回环没有物理端点，无法用「静音渲染流保活」；
		// 目标应用不发声时不出包，这是该模式的固有边界。
		keepAlive: nil,
	}, nil
}

// mapProcessLoopbackActivateError 把进程回环的激活 HRESULT 映射成用户可读错误。
//
// 与 mapWASAPIError 分开：这里拿到的是裸 HRESULT，语义集中在「目标进程」与
// 「系统能力」两类上，而设备回环那套映射针对的是 AUDCLNT_E_* 设备故障。
func mapProcessLoopbackActivateError(hr uintptr, pid uint32, exeName string) error {
	switch uint32(hr) {
	case 0x80070057: // E_INVALIDARG
		return apierr.New(apierr.CodeCaptureUnavailable).
			WithDetail("系统拒绝了进程级捕获参数（E_INVALIDARG，PID=%d / %s）", pid, exeName)
	case 0x80070005: // E_ACCESSDENIED
		return apierr.New(apierr.CodeCapturePermission).
			WithDetail("没有权限捕获该应用（%s，PID=%d），可能以更高权限运行", exeName, pid)
	case 0x80070490: // ERROR_NOT_FOUND
		return sourceNotFound(fmt.Sprintf("目标进程已退出或无法被捕获（PID=%d / %s）", pid, exeName))
	default:
		return apierr.New(apierr.CodeCaptureUnavailable).
			WithDetail("激活进程级回环失败（HRESULT=0x%08X，PID=%d / %s）", uint32(hr), pid, exeName)
	}
}

// ensure contract 用于文档化本文件引用的错误码来源，避免 future refactor 误删依赖。
var _ = contract.SourceKindApplication
