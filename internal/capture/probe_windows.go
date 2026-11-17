//go:build windows

package capture

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/go-ole/go-ole"
	"github.com/moutend/go-wca/pkg/wca"
	"golang.org/x/sys/windows"
)

// 本文件实现「进程级回环（按应用分路捕获）」的**异步激活底层**与**能力探测**。
//
// 为什么需要探测而不是查版本号：
//   - 微软文档把 AUDIOCLIENT_ACTIVATION_TYPE_PROCESS_LOOPBACK 的 Minimum supported
//     client 标为 Windows 10 Build 20348，但那是「头文件随 SDK 公开发布」的版本，
//     不等于 audiosrv 到 20348 才实现该路径（本机 Build 19045 实测可用）；
//   - 版本号是**推断**，探测是**事实**。能力声明的硬约束是「Open 必须能兑现」
//     （FR-02-4），所以唯一可靠的判据是真的激活一次、看 GetActivateResult 的 HRESULT。
//
// 因此 caps_windows.go 只认这里的结论，版本号退化为 Reason 文案的素材。

// —— 进程级回环相关常量（audioclientactivationparams.h / mmdeviceapi.h）——

const (
	// virtualAudioDeviceProcessLoopback 是进程回环的虚拟设备接口路径
	// （C 侧宏 VIRTUAL_AUDIO_DEVICE_PROCESS_LOOPBACK）。
	virtualAudioDeviceProcessLoopback = `VAD\Process_Loopback`

	// audioClientActivationTypeProcessLoopback AUDIOCLIENT_ACTIVATION_TYPE_PROCESS_LOOPBACK。
	audioClientActivationTypeProcessLoopback = 1

	// processLoopbackModeIncludeTargetProcessTree PROCESS_LOOPBACK_MODE_INCLUDE_TARGET_PROCESS_TREE：
	// 采集目标进程**及其子进程**，对应 FR-02-1。
	processLoopbackModeIncludeTargetProcessTree = 0

	// vtBlob PROPVARIANT 的 VT_BLOB，激活参数通过它传递。
	vtBlob = 65

	// activateWaitMs 等待异步激活完成的超时。
	// 激活通常毫秒级返回；2s 足够覆盖 audiosrv 冷启动，又不至于让
	// 「不支持」的机器长时间卡在探测上。
	activateWaitMs = 2000
)

// HRESULT 常量（只取本文件用到的）。
const (
	hrS_OK         = 0x00000000
	hrNoInterface  = 0x80004002 // E_NOINTERFACE
	hrPointer      = 0x80004003 // E_POINTER
	hrTimeoutProbe = 0x800705B4 // HRESULT_FROM_WIN32(ERROR_TIMEOUT)
)

// ptSize uintptr 的字节宽度（虚表槽位步长）。
const ptSize = unsafe.Sizeof(uintptr(0))

// IActivateAudioInterfaceCompletionHandler / IActivateAudioInterfaceAsyncOperation 的 IID。
var (
	iidActivateAudioInterfaceCompletionHandler = ole.NewGUID("{41D949AB-9862-444A-80F6-C261334DA5EB}")
	iidActivateAudioInterfaceAsyncOperation    = ole.NewGUID("{72A22D78-CDE4-431D-B8CC-843A71199B6D}")
	// iidIAgileObject 声明对象是「敏捷」的（无套间亲和性）。
	//
	// 这一条是**必须**的：缺少它时 ActivateAudioInterfaceAsync 会直接返回
	// E_ILLEGAL_METHOD_CALL（实测），因为系统无法把回调对象送进音频服务套间。
	// 官方样例通过 FtmBase 隐含满足同样的语义。
	iidIAgileObject = ole.NewGUID("{94EA2B94-E9CC-49E0-C0FF-EE64CA8F5B90}")
	// iidIMarshal 自由线程封送器接口；同时支持它可与 FtmBase 行为对齐。
	iidIMarshal = ole.NewGUID("{00000003-0000-0000-C000-000000000046}")
)

// ptrFromUintptr 把 COM 回调参数里的 uintptr 还原成带类型的指针。
//
// 为什么不用 (*T)(unsafe.Pointer(v))：那种直接转换会被 go vet 的 unsafeptr
// 分析判为 possible misuse（uintptr → unsafe.Pointer），项目 CI 要求 vet 干净。
// 这里先取指针变量的**地址**（指针→unsafe.Pointer，合法），再把地址值写进去，
// 语义等价且 vet 认可。
func ptrFromUintptr[T any](v uintptr) *T {
	var p *T
	*(*uintptr)(unsafe.Pointer(&p)) = v
	return p
}

// audioClientActivationParams 是 AUDIOCLIENT_ACTIVATION_PARAMS 的 Go 形态。
//
// C 侧该结构是「enum + 匿名 union」，union 里只有 ProcessLoopbackParams 一个成员，
// 因此展平成三个 uint32 后布局与 C 完全一致（12 字节，无填充）。
// 不要用 Go 结构体嵌套去模拟那个 union —— 那会引入对齐填充（S1-06 的教训）。
type audioClientActivationParams struct {
	ActivationType      uint32
	TargetProcessID     uint32
	ProcessLoopbackMode uint32
}

// propVariantBlob 是 PROPVARIANT 中 VT_BLOB 分支的最小可用子集。
//
// C 侧 PROPVARIANT 在 x64 上为 24 字节：vt + 3×wReserved 共 8 字节，
// 随后 union 从偏移 8 开始（BLOB = cbSize + 4 字节填充 + pBlobData）。
// 这里显式写出填充字段，保证与 COM 侧紧凑布局逐字节对齐。
type propVariantBlob struct {
	VT         uint16
	WReserved1 uint16
	WReserved2 uint16
	WReserved3 uint16
	CbSize     uint32
	_          uint32
	BlobData   unsafe.Pointer
}

// activateVtbl 是我们手写 COM 对象（IActivateAudioInterfaceCompletionHandler）的虚表。
//
// 只有这个接口需要我们自己实现（回调）；IActivateAudioInterfaceAsyncOperation
// 由系统实现、我们只按其虚表调用。ActivateAudioInterfaceAsync 本身是
// mmdevapi.dll 的**导出函数**，不需要虚表。
type activateVtbl struct {
	QueryInterface    uintptr
	AddRef            uintptr
	Release           uintptr
	ActivateCompleted uintptr
}

// activateHandler 是我们实现的 IActivateAudioInterfaceCompletionHandler。
//
// 内存布局约束（极重要）：首字段必须是虚表指针，这样 h 的地址才能直接当作
// COM 接口指针交给系统。
//
// 所有**传给系统的出参缓冲**也放在这里，而不是放在栈上，原因见 activateAudioClient
// 顶部关于「栈移动」的注释。
type activateHandler struct {
	vtbl *activateVtbl
	refs int32
	// event 由 ActivateCompleted 置位，唤醒等待中的调用线程。
	event windows.Handle
	// ftm 惰性创建的自由线程封送器（对齐官方样例的 FtmBase）。
	ftm unsafe.Pointer

	// —— 传给系统的参数与出参缓冲（必须在堆上且稳定） ——
	params audioClientActivationParams
	pv     propVariantBlob
	// operation ActivateAudioInterfaceAsync 的输出：IActivateAudioInterfaceAsyncOperation*。
	operation unsafe.Pointer
	// activateResult GetActivateResult 的输出：真正的激活 HRESULT。
	activateResult uintptr
	// activated GetActivateResult 的输出：激活出来的接口指针。
	activated unsafe.Pointer
}

// activateHandlerVtbl 是包级虚表：NewCallback 建的是永久 trampoline，进程内只建一次。
var activateHandlerVtbl = &activateVtbl{
	QueryInterface:    windows.NewCallback(activateQueryInterface),
	AddRef:            windows.NewCallback(activateAddRef),
	Release:           windows.NewCallback(activateRelease),
	ActivateCompleted: windows.NewCallback(activateCompleted),
}

// activateHandlers 让回调对象在异步激活期间**堆分配且不被回收**。
//
// 为什么必须有这张表（踩过的坑，务必不要「优化」掉）：
// 回调对象要作为 COM 接口指针传给系统，写法是 uintptr(unsafe.Pointer(h))。
// 一旦指针被转成 uintptr，Go 的逃逸分析就看不到它「逃出去了」，于是会把 h
// 分配在当前 goroutine 的**栈**上；而激活是异步的，等栈帧回收后系统仍在用这个
// 地址读写 vtable 与参数，结果就是激活莫名失败或直接崩溃。
// 通过 map 持有它，既强制堆分配，又保证整个异步期间可达。
var activateHandlers sync.Map

func activateQueryInterface(this, riid, out uintptr) uintptr {
	if out == 0 || riid == 0 {
		return hrPointer
	}
	outPtr := ptrFromUintptr[uintptr](out)
	*outPtr = 0

	h := ptrFromUintptr[activateHandler](this)
	id := ptrFromUintptr[ole.GUID](riid)

	// IMarshal：转发给自由线程封送器（与官方样例的 FtmBase 语义对齐）。
	if guidEqual(id, iidIMarshal) {
		ftm := h.freeThreadedMarshaler()
		if ftm == nil {
			return hrNoInterface
		}
		fn := comMethod(ftm, 0) // IUnknown::QueryInterface
		r1, _, _ := syscall.SyscallN(fn, uintptr(ftm), riid, out)
		return r1 & 0xFFFFFFFF
	}

	// IUnknown、自身 IID 与 IAgileObject 都返回同一个指针；其余如实拒绝。
	if !guidEqual(id, ole.IID_IUnknown) &&
		!guidEqual(id, iidActivateAudioInterfaceCompletionHandler) &&
		!guidEqual(id, iidIAgileObject) {
		return hrNoInterface
	}
	atomic.AddInt32(&h.refs, 1)
	*outPtr = this
	return hrS_OK
}

func activateAddRef(this uintptr) uintptr {
	h := ptrFromUintptr[activateHandler](this)
	return uintptr(atomic.AddInt32(&h.refs, 1))
}

func activateRelease(this uintptr) uintptr {
	h := ptrFromUintptr[activateHandler](this)
	if atomic.AddInt32(&h.refs, -1) <= 0 {
		// 对象本身由 Go GC 回收；这里只保证等待方不会永久阻塞。
		if h.event != 0 {
			_ = windows.SetEvent(h.event)
		}
		activateHandlers.Delete(h)
	}
	return 0
}

// activateCompleted 由系统在激活完成时调用（可能在任何线程上）。
//
// 这里**只置位事件**，不调用 GetActivateResult：
// IActivateAudioInterfaceAsyncOperation::GetActivateResult 只允许调用一次，
// 若在回调里消费掉，等待线程就拿不到接口了。消费统一放在等待方做。
func activateCompleted(this, operation uintptr) uintptr {
	h := ptrFromUintptr[activateHandler](this)
	_ = windows.SetEvent(h.event)
	return hrS_OK
}

// releaseOurs 释放「创建方持有的那一个引用」。
//
// 只有引用计数真正归零才从注册表移除：系统（音频服务代理）可能在我们返回后
// 仍持有引用，提前让对象不可达会造成 use-after-free。
func (h *activateHandler) releaseOurs() {
	if atomic.AddInt32(&h.refs, -1) <= 0 {
		activateHandlers.Delete(h)
	}
}

// freeThreadedMarshaler 惰性创建并缓存自由线程封送器。
func (h *activateHandler) freeThreadedMarshaler() unsafe.Pointer {
	if h.ftm != nil {
		return h.ftm
	}
	proc := windows.NewLazySystemDLL("ole32.dll").NewProc("CoCreateFreeThreadedMarshaler")
	if err := proc.Find(); err != nil {
		return nil
	}
	var ftm unsafe.Pointer
	r1, _, _ := syscall.SyscallN(proc.Addr(),
		uintptr(unsafe.Pointer(h)), // punkOuter：本对象
		uintptr(unsafe.Pointer(&ftm)),
	)
	if r1&0xFFFFFFFF != hrS_OK || ftm == nil {
		return nil
	}
	h.ftm = ftm
	return ftm
}

// comMethod 取 COM 接口指针 p 的第 slot 个虚表槽（0=QueryInterface, 1=AddRef, 2=Release…）。
//
// 极容易写错的一点：COM 对象首字是**虚表指针**，槽位要从虚表再偏移一次；
// 直接给对象地址加偏移读到的是对象字段，不是函数指针（会跳到垃圾地址崩掉）。
func comMethod(p unsafe.Pointer, slot uintptr) uintptr {
	vtbl := *(*unsafe.Pointer)(p)
	return *(*uintptr)(unsafe.Add(vtbl, slot*ptSize))
}

// callGetActivateResult 按虚表调用 IActivateAudioInterfaceAsyncOperation::GetActivateResult。
//
// 出参缓冲由调用方提供，且**必须位于堆上**（见 activateAudioClient 的说明）。
func callGetActivateResult(operation unsafe.Pointer, activateResult *uintptr, activated *unsafe.Pointer) uintptr {
	// 虚表第 3 槽（0=QueryInterface, 1=AddRef, 2=Release, 3=GetActivateResult）。
	fn := comMethod(operation, 3)
	// COM 方法返回 HRESULT(int32)；x64 单调用约定下用 uintptr 接收，只取低 32 位。
	r1, _, _ := syscall.SyscallN(fn,
		uintptr(operation),
		uintptr(unsafe.Pointer(activateResult)),
		uintptr(unsafe.Pointer(activated)),
	)
	return r1 & 0xFFFFFFFF
}

// releaseUnknown 调用 IUnknown::Release。
func releaseUnknown(p unsafe.Pointer) {
	if p == nil {
		return
	}
	fn := comMethod(p, 2)
	_, _, _ = syscall.SyscallN(fn, uintptr(p))
}

// guidEqual 比较两个 GUID。ole.GUID 是可直接比较的结构体（无引用类型字段）。
func guidEqual(a, b *ole.GUID) bool { return a != nil && b != nil && *a == *b }

// activateProcessAudioClient 为指定进程树激活一个 IAudioClient。
// 返回的 hr 是**真实激活结果**（GetActivateResult 给出的）。
//
// 返回的 client 所有权归调用方（用完需 Release）。
func activateProcessAudioClient(pid uint32) (*wca.IAudioClient, uintptr, error) {
	params := audioClientActivationParams{
		ActivationType:      audioClientActivationTypeProcessLoopback,
		TargetProcessID:     pid,
		ProcessLoopbackMode: processLoopbackModeIncludeTargetProcessTree,
	}
	return activateAudioClient(virtualAudioDeviceProcessLoopback, &params)
}

// activateAudioClient 是 ActivateAudioInterfaceAsync 的通用封装。
//
// params 为 nil 时按 DEFAULT 激活（activationParams 传空）。
//
// ⚠ 关于「栈移动」的重要约定（这是本文件最隐蔽的坑，改代码前务必读）：
// 调用 Windows API 时我们只能用 uintptr 传指针。如果这个指针指向**当前 goroutine
// 的栈**（例如局部变量 &operation），那么一旦在调用前后发生栈增长/栈拷贝，
// 那个地址就失效了：实测表现为 ActivateAudioInterfaceAsync 返回 S_OK，
// 但出参没被写入（调用方看到 operation == nil），而且**是否复现取决于调用栈深度
// 与 GC 时机**（例如 snapshotProcesses 触发一次 GC 之后就必挂，不触发时就好）。
// 因此：所有交给系统的**参数与出参缓冲**一律放在堆对象里（activateHandler 的字段），
// 绝不使用栈上局部变量的地址。字符串路径由 UTF16PtrFromString 分配到堆，安全。
func activateAudioClient(devicePath string, params *audioClientActivationParams) (*wca.IAudioClient, uintptr, error) {
	proc := windows.NewLazySystemDLL("Mmdevapi.dll").NewProc("ActivateAudioInterfaceAsync")
	if err := proc.Find(); err != nil {
		return nil, 0, fmt.Errorf("Mmdevapi.dll 没有 ActivateAudioInterfaceAsync 导出：%w", err)
	}

	event, err := windows.CreateEvent(nil, 0, 0, nil) // 自动复位
	if err != nil {
		return nil, 0, fmt.Errorf("创建激活等待事件失败：%w", err)
	}
	defer windows.CloseHandle(event)

	h := &activateHandler{
		vtbl:  activateHandlerVtbl,
		refs:  1,
		event: event,
	}
	// 强制堆分配 + 保活（详见 activateHandlers 注释）。
	activateHandlers.Store(h, struct{}{})
	defer h.releaseOurs()

	var paramsPtr uintptr
	if params != nil {
		h.params = *params
		h.pv = propVariantBlob{
			VT:       vtBlob,
			CbSize:   uint32(unsafe.Sizeof(h.params)),
			BlobData: unsafe.Pointer(&h.params),
		}
		paramsPtr = uintptr(unsafe.Pointer(&h.pv))
	}

	pathPtr, err := windows.UTF16PtrFromString(devicePath)
	if err != nil {
		return nil, 0, fmt.Errorf("构造设备路径失败：%w", err)
	}

	// 出参缓冲在 h 上（堆），不是栈上局部变量 —— 见函数顶部说明。
	hrActivate, _, _ := syscall.SyscallN(proc.Addr(),
		uintptr(unsafe.Pointer(pathPtr)),
		uintptr(unsafe.Pointer(wca.IID_IAudioClient)),
		paramsPtr,
		uintptr(unsafe.Pointer(h)), // 接口指针即对象地址（首字段是虚表）
		uintptr(unsafe.Pointer(&h.operation)),
	)
	if hr := hrActivate & 0xFFFFFFFF; hr != hrS_OK {
		// 异步激活根本没被受理。
		runtime.KeepAlive(pathPtr)
		return nil, hr, nil
	}
	operation := h.operation
	if operation == nil {
		runtime.KeepAlive(pathPtr)
		return nil, hrPointer, nil
	}
	defer releaseUnknown(operation)

	rc, werr := windows.WaitForSingleObject(event, activateWaitMs)
	// KeepAlive 必须在等待**之后**：激活是异步的，系统可能到 ActivateCompleted
	// 之前才去读设备路径串与 PROPVARIANT blob。
	runtime.KeepAlive(pathPtr)
	if werr != nil {
		return nil, 0, fmt.Errorf("等待激活回调失败：%w", werr)
	}
	if rc == uint32(windows.WAIT_TIMEOUT) {
		return nil, hrTimeoutProbe, nil
	}

	// 唯一一次消费激活结果。请求的 riid 就是 IID_IAudioClient，
	// 所以拿到的指针可直接按 wca.IAudioClient 使用。
	if hrGet := callGetActivateResult(operation, &h.activateResult, &h.activated); hrGet != hrS_OK {
		return nil, hrGet, nil
	}
	if h.activateResult != hrS_OK {
		if h.activated != nil {
			releaseUnknown(h.activated)
			h.activated = nil
		}
		return nil, h.activateResult, nil
	}
	if h.activated == nil {
		return nil, hrPointer, nil
	}
	return (*wca.IAudioClient)(h.activated), hrS_OK, nil
}

// —— 能力探测（包级缓存：成功结果常驻，失败结果按节流重试）——

// probeRetryInterval 是探测失败后的最小重试间隔。
//
// 为什么需要重试而不是「一次定终身」：应用常随系统开机自启，此时 AudioSrv
// 可能尚未就绪，首次探测会拿到瞬时失败。若不重试，一次瞬时故障就会被当成
// 「本机不支持」并持续整个进程生命周期（现象：主页一直挂版本限制提示，
// 必须重启应用才恢复）。取 10s：足够躲开开机阶段的瞬时不可用，又不会
// 让真正不支持的机器频繁付激活开销。
const probeRetryInterval = 10 * time.Second

var (
	processLoopbackMu    sync.Mutex
	processLoopbackProbe struct {
		// ok 一旦为 true 就常驻：支持是稳定事实，不需要反复确认。
		ok bool
		// note 最近一次失败的一句话诊断（成功时清空）。
		note string
		// at 最近一次探测的时刻，用于失败节流。
		at time.Time
	}
)

// probeFn 是实际执行探测的函数。默认是 probeProcessLoopback；
// 用变量而非直接调用，是为了让「先失败、后成功」这条自愈路径可被单测驱动
// （真机探测恒为 true，无法覆盖失败分支）。
var probeFn = probeProcessLoopback

// resetProbeCache 清空探测缓存（单测用；生产路径不需要）。
func resetProbeCache() {
	processLoopbackMu.Lock()
	defer processLoopbackMu.Unlock()
	processLoopbackProbe.ok = false
	processLoopbackProbe.note = ""
	processLoopbackProbe.at = time.Time{}
}

// processLoopbackAvailable 返回本机是否**实测**支持进程级回环，以及一句话诊断。
//
// 用自身 PID 做探测目标：只要系统受理该激活类型并成功激活音频客户端即可判定可用，
// 与「本进程当前有没有在放声音」无关（我们没有渲染流，本来就不出声）。
//
// 缓存策略（这里踩过一次坑，改过一版）：
//   - **只缓存成功**。旧实现用 `sync.Once`，把失败也永久固化 —— 应用若在开机早期
//     启动（音频服务 AudioSrv 尚未就绪），首次探测拿到的是瞬时失败，于是整个会话
//     都被判成「本机不支持按应用分路」，主页一直挂着版本限制提示，直到重启应用。
//   - 失败按 `probeRetryInterval` 节流重试。真正不支持的机器每次都会失败，
//     结论仍然稳定为「不支持」（FR-02-4 的诚实声明不破），只是不会把瞬时故障
//     当成永久结论。
//
// 探测会真的走一次 COM 异步激活，因此成功结果常驻、不做重复探测。
func processLoopbackAvailable() (bool, string) {
	processLoopbackMu.Lock()
	defer processLoopbackMu.Unlock()

	if processLoopbackProbe.ok {
		return true, ""
	}
	// 失败结果在节流窗口内直接复用，避免每次 GetCapabilities 都付一次激活开销。
	if !processLoopbackProbe.at.IsZero() && time.Since(processLoopbackProbe.at) < probeRetryInterval {
		return false, processLoopbackProbe.note
	}

	ok, note := probeFn()
	processLoopbackProbe.ok = ok
	processLoopbackProbe.note = note
	processLoopbackProbe.at = time.Now()
	if ok {
		// 成功即有定论：清掉失败备注，后续直接命中缓存。
		processLoopbackProbe.note = ""
	}
	return ok, note
}

// ProbeProcessLoopback 是 processLoopbackAvailable 的导出形式，
// 供 cmd/probe-processloopback 这类独立诊断工具使用。
func ProbeProcessLoopback() (bool, string) { return processLoopbackAvailable() }

// probeProcessLoopback 真正执行一次探测。
//
// 刻意在**独立 goroutine + LockOSThread** 上跑：探测要求 MTA，但调用方
// （例如枚举路径）可能已经在该线程上把 COM 初始化成 STA，直接就地初始化会
// 拿到 RPC_E_CHANGED_MODE，把一个「其实支持」的系统误判成不支持。
func probeProcessLoopback() (ok bool, note string) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		defer func() {
			if r := recover(); r != nil {
				ok, note = false, fmt.Sprintf("探测进程回环时发生 panic：%v", r)
			}
		}()

		owned, err := coInitializeMTA()
		if err != nil {
			ok, note = false, err.Error()
			return
		}
		if owned {
			defer ole.CoUninitialize()
		}

		client, hr, err := activateProcessAudioClient(uint32(os.Getpid()))
		if err != nil {
			ok, note = false, err.Error()
			return
		}
		if hr != hrS_OK {
			// 音频服务未运行是**瞬时**状态（开机早期、服务重启、被手动停掉），
			// 不是「本机不支持」。文案要说清可恢复，否则用户会以为是系统版本问题。
			if uint32(hr) == hrServiceNotRunning {
				ok, note = false, "Windows 音频服务（AudioSrv）未就绪，稍后会自动重试"
				return
			}
			ok, note = false, fmt.Sprintf("ActivateAudioInterfaceAsync 返回 HRESULT 0x%08X", hr)
			return
		}
		if client != nil {
			client.Release()
		}
		ok, note = true, ""
	}()
	<-done
	return ok, note
}

// coInitializeMTA 把当前线程初始化为 MTA。
//
// 为什么进程回环必须用 MTA 而不是本包其它地方用的 STA：
// ActivateAudioInterfaceAsync 明确要求调用线程处于多线程套间，STA 下激活会失败。
// 因此这条路径必须**独占**一个钉住的线程，不复用 STA 的采集线程。
//
// 返回值 owned 表示是否由本次调用完成初始化（只有 S_OK 才配对 CoUninitialize）。
func coInitializeMTA() (owned bool, err error) {
	initErr := ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED)
	if initErr == nil {
		return true, nil
	}

	var oleErr *ole.OleError
	if !errors.As(initErr, &oleErr) {
		return false, fmt.Errorf("初始化 COM (MTA) 失败：%w", initErr)
	}
	switch hr := uint32(oleErr.Code()); hr {
	case 0: // S_OK
		return true, nil
	case 1: // S_FALSE：该线程已初始化过 MTA，可继续使用
		return false, nil
	case 0x80010106: // RPC_E_CHANGED_MODE：线程已在 STA 里
		return false, fmt.Errorf("当前线程已处于 STA 单元模型（RPC_E_CHANGED_MODE），进程回环激活需要 MTA")
	default:
		return false, fmt.Errorf("初始化 COM (MTA) 失败（HRESULT=0x%08X）", hr)
	}
}
