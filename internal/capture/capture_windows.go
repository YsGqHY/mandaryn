//go:build windows

package capture

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"runtime"
	"sync"
	"unsafe"

	"github.com/go-ole/go-ole"
	"github.com/moutend/go-wca/pkg/wca"
	"golang.org/x/sys/windows"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
)

// —— WASAPI 相关常量 ——

const (
	// refTimesPerSecond 一个「秒」的 REFERENCE_TIME 单位数（100ns 为单位）。
	refTimesPerSecond = 10_000_000
	// loopbackBufferDuration 请求的回环缓冲时长：200ms。
	// 太小会在系统繁忙时丢包，太大则增加停止延迟与内存占用；200ms 是常见折中。
	loopbackBufferDuration = 200 * refTimesPerSecond / 1000
	// eventWaitMs 每次等待音频事件的超时（毫秒）。
	// 用超时而不是无限等待，是为了能在 200ms 内响应 Close()；
	// 同时即便事件不置位也继续尝试取包（见 pump 注释）。
	eventWaitMs = 200
	// pcmQueueSeconds 内部 PCM 队列的容量（秒）。
	// 上层（录音管线）偶发卡顿不应导致丢音，4 秒足够吸收一次 GC 抖动或磁盘慢写。
	pcmQueueSeconds = 4

	// WAVEFORMATEX 的 wFormatTag 取值。
	waveFormatPCM        = 0x0001
	waveFormatIEEEFloat  = 0x0003
	waveFormatExtensible = 0xFFFE

	// hrDeviceInvalidated AUDCLNT_E_DEVICE_INVALIDATED 的完整 HRESULT（含 FACILITY_AUDCLNT）。
	hrDeviceInvalidated = 0x88890004
	// hrServiceNotRunning AUDCLNT_E_SERVICE_NOT_RUNNING。
	hrServiceNotRunning = 0x88890010
)

// open 实现 Open（kind 已由 ParseSourceID 校验）。
func open(kind, value string) (Channel, error) {
	switch kind {
	case contract.SourceKindApplication:
		// 诚实拒绝：不静默退化成「抓全部声音」，否则用户以为分路成功、
		// 事后才发现会话里录进了别的应用（隐私事故）。
		return nil, sourceUnavailable(fmt.Sprintf(
			"按应用分路捕获尚未实现：Windows 需要 Build %d 及以上（当前 %s），且需 AUDIOCLIENT_ACTIVATION_TYPE_PROCESS_LOOPBACK；请选择设备源（会包含全部应用声音）",
			minProcessLoopbackBuild, windowsBuildLabel()))
	case contract.SourceKindDevice:
		// 继续
	default:
		return nil, apierr.New(apierr.CodeInvalidArgument).
			WithDetail("未知的源类型：%s", kind)
	}

	// 先确认设备仍在线，避免把「ID 写错」误报成「设备故障」。
	exists, err := deviceExists(value)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, sourceNotFound(fmt.Sprintf("未检测到该输出设备（可能已拔出或被禁用）：%s", value))
	}

	c := &loopbackChannel{
		queue: newPCMQueue(pcmQueueSeconds),
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
		errCh: make(chan error, 1),
	}

	ready := make(chan error, 1)
	go c.run(value, ready)
	if err := <-ready; err != nil {
		// 启动失败时 goroutine 已自行退出；这里等它收尾，避免泄漏线程。
		<-c.done
		return nil, err
	}
	return c, nil
}

// loopbackChannel 是 WASAPI 回环（整机混音 / 指定渲染设备）的 Channel 实现。
//
// 线程模型：
//   - 创建与销毁 COM 对象、阻塞等待事件，全部发生在 run() 那个
//     LockOSThread 的 goroutine 里（COM 对象不能跨线程使用）；
//   - 数据通过 pcmQueue 交给任意 goroutine 的 ReadPCM，队列自带锁与条件变量。
type loopbackChannel struct {
	// format/bits/blockAlign 在 ready 通道返回前写入，之后只读，无需加锁。
	format     AudioFormat
	bits       int
	blockAlign int

	queue *pcmQueue

	stop     chan struct{}
	stopOnce sync.Once
	done     chan struct{}
	errCh    chan error

	// scratch 转换用的临时缓冲，只被生产 goroutine 访问。
	scratch []float32
}

// ReadPCM 实现 Channel。
func (c *loopbackChannel) ReadPCM(dst []float32) (int, error) { return c.queue.read(dst) }

// Format 实现 Channel。
func (c *loopbackChannel) Format() AudioFormat { return c.format }

// Err 实现 Channel：外部原因导致采集终止时收到一次错误，随后通道关闭。
func (c *loopbackChannel) Err() <-chan error { return c.errCh }

// Close 实现 Channel：幂等。返回时采集线程已退出、COM 已释放。
func (c *loopbackChannel) Close() error {
	c.stopOnce.Do(func() {
		close(c.stop)
		<-c.done
	})
	return nil
}

// run 是采集线程主体。
func (c *loopbackChannel) run(deviceID string, ready chan<- error) {
	// 钉住 OS 线程：COM 对象有线程亲和性，goroutine 迁移线程会导致
	// RPC_E_WRONG_THREAD 甚至进程崩溃（本包最危险的坑）。
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer close(c.done)
	// 兜底：任何退出路径都要让阻塞中的 ReadPCM 收到 EOF。
	defer c.queue.closeWith(nil)

	owned, err := coInitializeSTA()
	if err != nil {
		c.fail(err)
		ready <- err
		return
	}
	if owned {
		defer ole.CoUninitialize()
	}

	lb, err := openLoopback(deviceID)
	if err != nil {
		c.fail(err)
		ready <- err
		return
	}
	// 释放顺序（LIFO）：先停流与释放接口，最后 CoUninitialize（上面已注册）。
	defer lb.close()

	c.format = lb.format
	c.bits = lb.bits
	c.blockAlign = lb.blockAlign
	ready <- nil

	c.pump(lb)
}

// pump 循环等待音频事件并抽取 PCM。
func (c *loopbackChannel) pump(lb *loopbackStream) {
	for {
		select {
		case <-c.stop:
			c.queue.closeWith(nil)
			return
		default:
		}

		// 有超时地等待：即使事件不置位也继续往下取包。
		// 共享模式回环在「全系统静音」时事件触发频率会下降，若只在
		// WAIT_OBJECT_0 时取包，会把静音期误判成「抓不到音频」。
		rc, err := windows.WaitForSingleObject(lb.event, eventWaitMs)
		if err != nil {
			c.fail(mapWASAPIError(err, "等待音频事件"))
			return
		}
		// windows.WAIT_TIMEOUT 在 x/sys/windows 里是 syscall.Errno 类型，需显式转换。
		if rc != windows.WAIT_OBJECT_0 && rc != uint32(windows.WAIT_TIMEOUT) {
			c.fail(apierr.New(apierr.CodeCaptureDeviceLost).
				WithDetail("音频事件句柄异常（WaitForSingleObject 返回 %d），设备可能已被移除", rc))
			return
		}

		if err := c.drain(lb); err != nil {
			c.fail(err)
			return
		}
	}
}

// drain 把当前缓冲区里所有完整数据包取空。
func (c *loopbackChannel) drain(lb *loopbackStream) error {
	for {
		var packet uint32
		if err := lb.capture.GetNextPacketSize(&packet); err != nil {
			return mapWASAPIError(err, "读取数据包大小")
		}
		if packet == 0 {
			return nil
		}

		var (
			data   *byte
			frames uint32
			flags  uint32
			devPos uint64
			qpcPos uint64
		)
		if err := lb.capture.GetBuffer(&data, &frames, &flags, &devPos, &qpcPos); err != nil {
			return mapWASAPIError(err, "读取音频数据包")
		}

		c.convertAndQueue(data, frames, flags)

		if err := lb.capture.ReleaseBuffer(frames); err != nil {
			return mapWASAPIError(err, "释放音频数据包")
		}
	}
}

// convertAndQueue 把一包原始字节转成 interleaved float32 并入队。
func (c *loopbackChannel) convertAndQueue(data *byte, frames, flags uint32) {
	total := int(frames) * c.format.Channels
	if total <= 0 {
		return
	}
	if cap(c.scratch) < total {
		c.scratch = make([]float32, total)
	}
	out := c.scratch[:total]

	// SILENT 包可能带 NULL 指针，但帧数有效——语义是「这段时间是静音」，
	// 必须照样输出等长静音，否则时间轴会错位（时间基是帧计数，见 CLAUDE.md）。
	if data == nil || flags&wca.AUDCLNT_BUFFERFLAGS_SILENT != 0 {
		for i := range out {
			out[i] = 0
		}
		c.queue.write(out)
		return
	}

	raw := unsafeByteSlice(data, int(frames)*c.blockAlign)
	if !decodePCM(raw, out, c.format, c.bits) {
		// 格式不认识：输出静音而不是终止采集。
		// 终止会让用户丢掉整段录音，而静音至少保住时间轴与后续包。
		for i := range out {
			out[i] = 0
		}
	}
	c.queue.write(out)
}

// decodePCM 按格式把字节解码成 [-1,1] 的 float32；格式不支持时返回 false。
func decodePCM(raw []byte, out []float32, format AudioFormat, bits int) bool {
	switch {
	case format.Float32 && bits == 32:
		if len(raw) < len(out)*4 {
			return false
		}
		for i := range out {
			out[i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[i*4:]))
		}
	case bits == 16:
		if len(raw) < len(out)*2 {
			return false
		}
		for i := range out {
			out[i] = float32(int16(binary.LittleEndian.Uint16(raw[i*2:]))) / 32768
		}
	case bits == 32:
		if len(raw) < len(out)*4 {
			return false
		}
		for i := range out {
			out[i] = float32(int32(binary.LittleEndian.Uint32(raw[i*4:]))) / 2147483648
		}
	case bits == 24:
		if len(raw) < len(out)*3 {
			return false
		}
		for i := range out {
			b := raw[i*3:]
			// 小端 3 字节，最高位是符号位：先拼到 32 位再算术右移保持符号。
			v := int32(uint32(b[0])<<8 | uint32(b[1])<<16 | uint32(b[2])<<24)
			out[i] = float32(v>>8) / 8388608
		}
	case bits == 8:
		if len(raw) < len(out) {
			return false
		}
		for i := range out {
			// 8bit PCM 是无符号的，128 为中点。
			out[i] = (float32(raw[i]) - 128) / 128
		}
	default:
		return false
	}
	return true
}

// fail 记录终止原因并关闭队列（生产者侧最后一次操作）。
func (c *loopbackChannel) fail(err error) {
	c.queue.closeWith(err)
	select {
	case c.errCh <- err:
	default:
		// 已经塞过一次错误，保留最早的原因。
	}
}

// —— WASAPI 回环打开流程 ——

// loopbackStream 一组需要一起释放的 WASAPI 回环资源。
type loopbackStream struct {
	client     *wca.IAudioClient
	capture    *wca.IAudioCaptureClient
	event      windows.Handle
	format     AudioFormat
	bits       int
	blockAlign int
	// keepAlive 同端点的静音保活渲染流（见 openSilentRender）；打不开时为 nil，回环照常工作。
	keepAlive *silentRender
}

// close 停止并释放；只能在创建它的 COM 线程上调用。
func (s *loopbackStream) close() {
	if s.client != nil {
		_ = s.client.Stop()
	}
	if s.capture != nil {
		s.capture.Release()
		s.capture = nil
	}
	if s.client != nil {
		s.client.Release()
		s.client = nil
	}
	if s.keepAlive != nil {
		s.keepAlive.close()
		s.keepAlive = nil
	}
	if s.event != 0 {
		_ = windows.CloseHandle(s.event)
		s.event = 0
	}
}

// silentRender 是与回环同端点、只播放静音的「保活」渲染流。
//
// 为什么需要：WASAPI 共享模式回环只在端点上有活动渲染流时才出包。系统里没有任何应用
// 在放声音时，回环既不置事件也不给 SILENT 包 —— 时间基是帧计数，这段时间会直接从
// 录音时间轴上消失；整段都没人发声时录到的是 0 帧（被判空录音）。
// 在同一端点上开一个渲染流让音频引擎持续运转（OBS 同款做法），回环就会按时产出
// SILENT 包，convertAndQueue 把它们写成等长静音，时间轴保持连续。
//
// 只在启动时写入一整缓冲的静音（AUDCLNT_BUFFERFLAGS_SILENT），之后不再喂数据：
// 欠载时音频引擎对这个流输出静音、流本身保持运行；它不会改变其它应用的混音结果。
type silentRender struct {
	client *wca.IAudioClient
	render *wca.IAudioRenderClient
}

// openSilentRender 在 dev 上打开并启动保活渲染流（必须在 COM 线程上调用）。
func openSilentRender(dev *wca.IMMDevice) (*silentRender, error) {
	var client *wca.IAudioClient
	if err := dev.Activate(wca.IID_IAudioClient, clsctxAll, nil, &client); err != nil || client == nil {
		return nil, mapWASAPIError(err, "激活保活渲染流")
	}
	s := &silentRender{client: client}
	ok := false
	defer func() {
		if !ok {
			s.close()
		}
	}()

	var wfx *wca.WAVEFORMATEX
	if err := client.GetMixFormat(&wfx); err != nil || wfx == nil {
		return nil, mapWASAPIError(err, "读取保活流混合格式")
	}
	// GetMixFormat 的结构体由 COM 分配，用完交还（Initialize 会自行拷贝格式）。
	defer ole.CoTaskMemFree(uintptr(unsafe.Pointer(wfx)))

	if err := client.Initialize(
		wca.AUDCLNT_SHAREMODE_SHARED,
		0, // 普通渲染流：不要事件回调，之后也不再喂数据
		wca.REFERENCE_TIME(loopbackBufferDuration),
		0,
		wfx,
		nil,
	); err != nil {
		return nil, mapWASAPIError(err, "初始化保活渲染流")
	}

	var frames uint32
	if err := client.GetBufferSize(&frames); err != nil {
		return nil, mapWASAPIError(err, "读取保活流缓冲大小")
	}
	if err := client.GetService(wca.IID_IAudioRenderClient, &s.render); err != nil || s.render == nil {
		return nil, mapWASAPIError(err, "获取渲染客户端")
	}
	var data *byte
	if err := s.render.GetBuffer(frames, &data); err != nil {
		return nil, mapWASAPIError(err, "申请保活流缓冲")
	}
	// SILENT 标记：引擎把这段当静音处理，不必真的往 data 里写零。
	if err := s.render.ReleaseBuffer(frames, wca.AUDCLNT_BUFFERFLAGS_SILENT); err != nil {
		return nil, mapWASAPIError(err, "提交保活流静音缓冲")
	}
	if err := client.Start(); err != nil {
		return nil, mapWASAPIError(err, "启动保活渲染流")
	}
	ok = true
	return s, nil
}

// close 停止并释放保活流；只能在创建它的 COM 线程上调用。幂等。
func (s *silentRender) close() {
	if s.client != nil {
		_ = s.client.Stop()
	}
	if s.render != nil {
		s.render.Release()
		s.render = nil
	}
	if s.client != nil {
		s.client.Release()
		s.client = nil
	}
}

// openLoopback 在指定渲染设备上建立回环采集流（必须在 COM 线程上调用）。
func openLoopback(deviceID string) (*loopbackStream, error) {
	enumerator, err := newDeviceEnumerator()
	if err != nil {
		return nil, err
	}
	defer enumerator.Release()

	dev, err := findRenderDevice(enumerator, deviceID)
	if err != nil {
		return nil, err
	}
	defer dev.Release()

	var client *wca.IAudioClient
	if err := dev.Activate(wca.IID_IAudioClient, clsctxAll, nil, &client); err != nil || client == nil {
		return nil, mapWASAPIError(err, "激活音频客户端")
	}

	released := false
	defer func() {
		if !released && client != nil {
			client.Release()
		}
	}()

	var wfx *wca.WAVEFORMATEX
	if err := client.GetMixFormat(&wfx); err != nil {
		return nil, mapWASAPIError(err, "读取设备混合格式")
	}

	format, bits, blockAlign, err := parseMixFormat(wfx)
	if err != nil {
		return nil, err
	}

	// 手动复位事件：CreateEvent 的 manualReset=0（自动复位），与 WASAPI 文档一致。
	event, err := windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		return nil, apierr.New(apierr.CodeCaptureUnavailable).
			WithDetail("创建音频事件失败：%v", err).WithCause(err)
	}

	if err := client.Initialize(
		wca.AUDCLNT_SHAREMODE_SHARED,
		wca.AUDCLNT_STREAMFLAGS_LOOPBACK|wca.AUDCLNT_STREAMFLAGS_EVENTCALLBACK,
		wca.REFERENCE_TIME(loopbackBufferDuration),
		0, // 共享模式必须为 0，由音频引擎决定周期
		wfx,
		nil,
	); err != nil {
		_ = windows.CloseHandle(event)
		return nil, mapWASAPIError(err, "初始化回环流")
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
		return nil, mapWASAPIError(err, "启动回环采集")
	}

	// 静音保活：让回环在「没有任何应用发声」时也按时出包（见 silentRender）。
	// 打不开只降级为旧行为（静默时段无包），不影响录制本身，因此只记一条元数据日志。
	keepAlive, kerr := openSilentRender(dev)
	if kerr != nil {
		lg().Warn("静音保活渲染流不可用，静默时段可能缺帧",
			"code", apierr.From(kerr, apierr.CodeCaptureUnavailable).Code)
	}

	released = true // 所有权转交给 loopbackStream
	return &loopbackStream{
		client:     client,
		capture:    capture,
		event:      event,
		format:     format,
		bits:       bits,
		blockAlign: blockAlign,
		keepAlive:  keepAlive,
	}, nil
}

// findRenderDevice 按端点 ID 找一个活动渲染设备。
//
// 不用 IMMDeviceEnumerator.GetDevice：go-wca v0.3.0 里的封装没有实现参数传递
// （GetDevice() 无入参），直接调用会崩；枚举比对是小代价且行为可预测。
func findRenderDevice(enumerator *wca.IMMDeviceEnumerator, deviceID string) (*wca.IMMDevice, error) {
	var collection *wca.IMMDeviceCollection
	if err := enumerator.EnumAudioEndpoints(wca.ERender, wca.DEVICE_STATE_ACTIVE, &collection); err != nil {
		return nil, mapWASAPIError(err, "枚举输出设备")
	}
	defer collection.Release()

	var count uint32
	if err := collection.GetCount(&count); err != nil {
		return nil, mapWASAPIError(err, "读取输出设备数量")
	}

	for i := uint32(0); i < count; i++ {
		var dev *wca.IMMDevice
		if err := collection.Item(i, &dev); err != nil || dev == nil {
			continue
		}
		var id string
		if err := dev.GetId(&id); err != nil || id != deviceID {
			dev.Release()
			continue
		}
		return dev, nil
	}
	return nil, sourceNotFound(fmt.Sprintf("未检测到该输出设备（可能已拔出或被禁用）：%s", deviceID))
}

// parseMixFormat 解析 WAVEFORMATEX / WAVEFORMATEXTENSIBLE。
func parseMixFormat(wfx *wca.WAVEFORMATEX) (AudioFormat, int, int, error) {
	if wfx == nil {
		return AudioFormat{}, 0, 0, sourceUnavailable("设备未返回混合格式")
	}
	channels := int(wfx.NChannels)
	rate := int(wfx.NSamplesPerSec)
	bits := int(wfx.WBitsPerSample)
	blockAlign := int(wfx.NBlockAlign)
	if channels <= 0 || rate <= 0 || bits <= 0 {
		return AudioFormat{}, 0, 0, sourceUnavailable(fmt.Sprintf(
			"设备混合格式非法（声道 %d、采样率 %d、位深 %d）", channels, rate, bits))
	}

	var isFloat bool
	switch wfx.WFormatTag {
	case waveFormatPCM:
		isFloat = false
	case waveFormatIEEEFloat:
		isFloat = true
	case waveFormatExtensible:
		sub, ok := subFormatData1(wfx)
		if !ok {
			return AudioFormat{}, 0, 0, sourceUnavailable(
				"设备混合格式是 WAVE_FORMAT_EXTENSIBLE 但缺少子格式信息")
		}
		switch sub {
		case 1: // KSDATAFORMAT_SUBTYPE_PCM
			isFloat = false
		case 3: // KSDATAFORMAT_SUBTYPE_IEEE_FLOAT
			isFloat = true
		default:
			return AudioFormat{}, 0, 0, sourceUnavailable(fmt.Sprintf(
				"不支持的混合格式子类型（%d）：目前只处理整型 PCM 与 32 位浮点", sub))
		}
	default:
		return AudioFormat{}, 0, 0, sourceUnavailable(fmt.Sprintf(
			"不支持的混合格式（wFormatTag=0x%04X）", wfx.WFormatTag))
	}

	if isFloat && bits != 32 {
		return AudioFormat{}, 0, 0, sourceUnavailable(fmt.Sprintf(
			"不支持的浮点位深：%d（只支持 32 位浮点）", bits))
	}
	if !isFloat && bits != 8 && bits != 16 && bits != 24 && bits != 32 {
		return AudioFormat{}, 0, 0, sourceUnavailable(fmt.Sprintf(
			"不支持的整型位深：%d（支持 8/16/24/32）", bits))
	}

	return AudioFormat{SampleRate: rate, Channels: channels, Float32: isFloat}, bits, blockAlign, nil
}

// subFormatData1 读取 WAVEFORMATEXTENSIBLE 的 SubFormat GUID 前 4 字节。
//
// 结构布局（WAVEFORMATEXTENSIBLE）：
//
//	WAVEFORMATEX  fmt        偏移 0..17
//	WORD          wValidBits 偏移 18..19
//	DWORD         channelMask偏移 20..23
//	GUID          SubFormat  偏移 24..39  ← 只需要它的 Data1（PCM=1 / IEEE_FLOAT=3）
func subFormatData1(wfx *wca.WAVEFORMATEX) (uint32, bool) {
	// cbSize 至少 22 才说明后面确实跟着扩展字段，否则读越界。
	if wfx.CbSize < 22 {
		return 0, false
	}
	p := unsafeAdd(unsafe.Pointer(wfx), 24)
	return *(*uint32)(p), true
}

// mapWASAPIError 把 COM/WASAPI 错误映射成契约错误码。
func mapWASAPIError(err error, what string) *apierr.Err {
	if err == nil {
		return apierr.New(apierr.CodeCaptureUnavailable).WithDetail("%s 失败（无错误详情）", what)
	}
	var oleErr *ole.OleError
	if errors.As(err, &oleErr) {
		switch hr := uint32(oleErr.Code()); hr {
		case hrDeviceInvalidated:
			return apierr.New(apierr.CodeCaptureDeviceLost).
				WithDetail("%s 失败：音频设备已失效（可能被拔出或默认设备已切换）", what).
				WithCause(err)
		case hrServiceNotRunning:
			return apierr.New(apierr.CodeCaptureUnavailable).
				WithDetail("%s 失败：Windows 音频服务未运行", what).
				WithCause(err)
		default:
			return apierr.New(apierr.CodeCaptureUnavailable).
				WithDetail("%s 失败（HRESULT=0x%08X）", what, hr).
				WithCause(err)
		}
	}
	return apierr.New(apierr.CodeCaptureUnavailable).
		WithDetail("%s 失败：%v", what, err).
		WithCause(err)
}

// —— 内部 PCM 队列 ——

// pcmQueue 是生产者（采集线程）→ 消费者（ReadPCM）之间的定长环形缓冲。
//
// 设计取舍：
//   - 生产者**永不阻塞**：采集线程被阻塞会导致 WASAPI 溢出丢包，
//     所以队列满时丢弃最旧数据并记一次警告；
//   - 消费者阻塞等待：ReadPCM 语义是「有数据就返回，没数据就等」。
type pcmQueue struct {
	mu   sync.Mutex
	cond *sync.Cond
	ring []float32
	head int
	size int

	closed   bool
	err      error
	overflow bool // 是否已记录过溢出日志，避免刷屏
}

// newPCMQueue 按「秒」创建队列容量（假设 48kHz 立体声，容量再放宽一倍）。
func newPCMQueue(seconds int) *pcmQueue {
	const assumedSamplesPerSecond = 48000 * 2
	if seconds <= 0 {
		seconds = 1
	}
	q := &pcmQueue{ring: make([]float32, assumedSamplesPerSecond*seconds)}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// write 追加样本（生产者调用，永不阻塞）。
func (q *pcmQueue) write(samples []float32) {
	if len(samples) == 0 {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}

	capacity := len(q.ring)
	if len(samples) > capacity {
		// 单包就超过整个队列：只保留最新的部分，避免下标计算越界。
		samples = samples[len(samples)-capacity:]
		q.noteOverflowLocked(len(samples))
	}
	if q.size+len(samples) > capacity {
		drop := q.size + len(samples) - capacity
		q.head = (q.head + drop) % capacity
		q.size -= drop
		q.noteOverflowLocked(drop)
	}

	writeAt := (q.head + q.size) % capacity
	written := copy(q.ring[writeAt:], samples)
	copy(q.ring[0:], samples[written:])
	q.size += len(samples)
	q.cond.Broadcast()
}

// noteOverflowLocked 记录溢出（只记首次）。
func (q *pcmQueue) noteOverflowLocked(dropped int) {
	if q.overflow {
		return
	}
	q.overflow = true
	lg().Warn("PCM 队列溢出，已丢弃最旧的音频数据", "droppedSamples", dropped)
}

// read 阻塞读取，返回拷贝的样本数；关闭且无数据时返回 io.EOF。
func (q *pcmQueue) read(dst []float32) (int, error) {
	if len(dst) == 0 {
		return 0, nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()

	for q.size == 0 && !q.closed {
		q.cond.Wait()
	}
	if q.size == 0 {
		// 终止原因通过 Channel.Err() 告知，ReadPCM 统一返回 EOF，
		// 让上层用同一套「读到尾」逻辑收尾。
		return 0, io.EOF
	}

	n := 0
	for n < len(dst) && q.size > 0 {
		dst[n] = q.ring[q.head]
		q.head = (q.head + 1) % len(q.ring)
		q.size--
		n++
	}
	return n, nil
}

// closeWith 关闭队列并记录终止原因（生产者或 Close 调用）。
func (q *pcmQueue) closeWith(err error) {
	q.mu.Lock()
	if !q.closed {
		q.closed = true
		q.err = err
	}
	q.mu.Unlock()
	q.cond.Broadcast()
}

// cause 返回队列的终止原因（主动 Close 为 nil）。供测试与诊断使用。
func (q *pcmQueue) cause() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.err
}

// —— 指针工具 ——

// unsafeByteSlice 把 COM 返回的裸指针包装成只读字节切片。
//
// 生命周期由 WASAPI 保证：这段内存在 ReleaseBuffer 之前有效，
// 调用方必须在 ReleaseBuffer 前用完。
func unsafeByteSlice(p *byte, n int) []byte {
	if p == nil || n <= 0 {
		return nil
	}
	return unsafe.Slice(p, n)
}

// unsafeAdd 在结构体指针上做字节偏移（用于读 WAVEFORMATEXTENSIBLE 的扩展字段）。
func unsafeAdd(base unsafe.Pointer, offset uintptr) unsafe.Pointer {
	return unsafe.Add(base, offset)
}
