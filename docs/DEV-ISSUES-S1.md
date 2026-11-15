# S1 开发问题记录（应用级音频捕获域 / FR-02）

本文件记录 S1 实现过程中**实际踩到**的问题，按遇到顺序编号。每条尽量写清「为什么」，便于后续里程碑避免重复踩坑。

范围：`internal/capture`（L2 领域包）+ `internal/services/capturesvc`（L3 薄壳）。
验证环境：Windows 10 Build **19045**、i5-13600KF、8 个活动渲染端点
（Realtek / NVIDIA / ASUS 虚拟声卡 / ToDesk 虚拟声卡 / HECATE G2000 USB）。
工具链：Go 1.26.3、`CGO_ENABLED=1`、`go-wca v0.3.0`、`go-ole v1.3.0`、`golang.org/x/sys v0.49.0`。

状态口径：
- **未解决** —— 能力缺口或待补契约，需要后续里程碑处理；
- **已绕过** —— 上游库缺陷，用等价手段绕开，行为不受影响；
- **已解决** —— 已在本版本修好，记录是为了防止有人「优化」回去；
- **改设计** —— 与原设想不同，但是**有意为之**的取舍。

---

## S1-01 go-wca 缺少类型化 QueryInterface
- 现象：要拿会话 PID 与会话电平，需要从 `IAudioSessionControl` 转到 `IAudioSessionControl2` / `IAudioMeterInformation`；但 go-wca 只提供 `(*IUnknown).QueryInterface(iid) (*ole.IDispatch, error)`，返回的 `*IDispatch` 无法当 `*IAudioSessionControl2` 用。
- 复现条件：对 `sessions.GetSession(i, &ctl)` 得到的 `*wca.IAudioSessionControl` 调 `QueryInterface(wca.IID_IAudioSessionControl2)`，结果类型不匹配。
- 根因：go-wca 只做了 IDispatch 形态的便捷封装；真正通用的入口是 go-ole 内嵌在 `IUnknown` 上的 `PutQueryInterface(iid, obj interface{})`，它用反射把接口指针写回调用方提供的变量。
- 临时处置：统一改用它——`ctl.PutQueryInterface(wca.IID_IAudioSessionControl2, &ctl2)`，`obj` 传 `**wca.IAudioSessionControl2`。
- 状态：已绕过

## S1-02 IMMDeviceEnumerator.GetDevice 封装不完整
- 现象：想按端点 ID 直接取 `IMMDevice` 时无处下手：go-wca 的 `GetDevice()` 既没有 id 入参也没有出参。
- 复现条件：查看 `pkg/wca/IMMDeviceEnumerator.go` 与 `IMMDeviceEnumerator_windows.go`，可见只透传了 `this` 指针。
- 根因：`go-wca v0.3.0` 的该封装没有实现参数传递（`mmdeGetDevice` 只调用了自身 vtable，未压入 id/out 指针）。
- 临时处置：改为 `EnumAudioEndpoints` + 逐个 `GetId` 比对（`findRenderDevice`）。设备数量是个位数，成本可忽略，且行为可预测，比手写 vtable 调用安全。
- 状态：已绕过

## S1-03 go-wca 的 CreateEventExA 跨平台签名不一致
- 现象：若在共享代码里使用 go-wca 的事件封装，`GOOS=darwin` / `GOOS=linux` 构建会报类型不匹配。
- 复现条件：对比 `com_windows.go` 的 `func CreateEventExA(...) uintptr` 与 `com_func.go` 的 `func CreateEventExA(...) error`。
- 根因：go-wca 的 non-windows stub 与 windows 实现签名不一致，属于上游缺陷。
- 临时处置：事件句柄一律用 `golang.org/x/sys/windows`（`CreateEvent` / `WaitForSingleObject` / `CloseHandle`），不碰 go-wca 的事件封装。
- 状态：已绕过

## S1-04 ole.CoInitializeEx 把 S_FALSE 当错误
- 现象：偶发枚举整体失败，错误文本是「The operation completed successfully」（HRESULT=1）。
- 复现条件：在一个已经初始化为 STA 的线程上再次调用 `ole.CoInitializeEx(0, ole.COINIT_APARTMENTTHREADED)`。
- 根因：go-ole 的 `coInitializeEx` 只判断 `hr != 0`。但 `S_FALSE (1)` 的语义是「本线程已初始化过同模型单元」，属于成功；`RPC_E_CHANGED_MODE (0x80010106)` 才是真失败（线程已在别的 apartment 模型里）。
- 临时处置：`coInitializeSTA()` 显式区分三种返回值；并且**只在 S_OK 时**配对调用 `CoUninitialize`（S_FALSE 时多减一次会让同线程后续的 COM 调用失败）。
- 状态：已解决

## S1-05 AUDCLNT_E_* 常量缺 FACILITY_AUDCLNT 高位
- 现象：按 `wca.AUDCLNT_E_DEVICE_INVALIDATED`（0x004）比较真实错误永远不成立，拔设备/切默认设备会被误报成「未知 HRESULT」。
- 复现条件：录制中拔掉默认输出设备或切换默认设备，观察 `Channel.Err()` 的错误码。
- 根因：go-wca `constants.go` 只保留了低 16 位；真实 HRESULT 是 `0x88890004`（`FACILITY_AUDCLNT` = 0x8889）。
- 临时处置：自行定义 `hrDeviceInvalidated = 0x88890004`、`hrServiceNotRunning = 0x88890010`，映射到 `ERR_CAPTURE_DEVICE_LOST` / `ERR_CAPTURE_UNAVAILABLE`。
- 状态：已解决

## S1-06 WAVEFORMATEX 的 Go 结构体填充与 COM 紧凑布局不一致（测试陷阱）
- 现象：用「Go 结构体嵌套」构造 `WAVEFORMATEXTENSIBLE` 做单元测试时，PCM(16bit) 格式被判成「不支持」；但同一份代码在真实设备上正常。
- 复现条件：`type X struct { Format wca.WAVEFORMATEX; ValidBits uint16; ChannelMask uint32; SubFormat [16]byte }` 后把 `&X.Format` 传给 `parseMixFormat`。
- 根因：`wca.WAVEFORMATEX` 是 18 字节但最大对齐为 4，Go 会把结构体补到 20 字节，于是 `ValidBits/ChannelMask/SubFormat` 整体后移 2 字节，`SubFormat` 落在偏移 26 而不是 24。真实 COM 缓冲是**紧凑**的 40 字节（`SubFormat` 在偏移 24），所以生产代码读 24 是对的。
- 临时处置：生产代码保持紧凑偏移 24（已在真实设备上验证：`48000Hz 2ch float32=true`，与独立探针一致）；测试改用 40 字节字节数组 + `unsafe.Pointer` 转换（`extFormatHelper`），并在注释里写明原因。
- 状态：已解决

## S1-07 进程级（按应用分路）捕获未实现
- 现象：`Open("app:kook.exe")` 返回 `ERR_CAPTURE_UNAVAILABLE`；`GetCapabilities().ProcessLoopback == false`，Reason 里写明最低系统版本。
- 复现条件：任意时刻调用 `capture.Capabilities()` 或 `capture.Open("app:<任意 exe>")`。
- 根因：进程级回环需要 `AUDIOCLIENT_ACTIVATION_TYPE_PROCESS_LOOPBACK`，且必须走 `ActivateAudioInterfaceAsync` + `AUDIOCLIENT_ACTIVATION_PARAMS` 的**异步激活**路径。`go-wca v0.3.0` 未暴露这套异步 API；同时本机 Build 19045 低于官方要求的 20348，**本机根本无法验证**该路径。
- 临时处置：
  - `Capabilities()` 取「系统版本满足 **且** 已实现」双条件，通过 `processLoopbackImplemented = false` 常量显式表达"未实现"，绝不虚报；
  - `Open` 对应用源**诚实拒绝**，不静默退化成设备回环（否则用户以为分路成功，事后才发现录进了别的应用，属隐私事故）；
  - 拒绝文案包含最低版本与当前实际版本，直接可展示。
- 状态：**已解决**（S1 收尾，方案 A「原生进程回环 + 运行时探测」）。

  实现要点：
  - 新增 `probe_windows.go`：手写 `IActivateAudioInterfaceCompletionHandler`（虚表 + 事件通知），用
    `windows.NewLazySystemDLL("Mmdevapi.dll")` 直接调**导出函数** `ActivateAudioInterfaceAsync`
    （它不是 COM 方法，不需要虚表），参数经 `PROPVARIANT(VT_BLOB)` 传入
    `AUDIOCLIENT_ACTIVATION_PARAMS{PROCESS_LOOPBACK, TargetProcessId, INCLUDE_TARGET_PROCESS_TREE}`。
  - 新增 `capture_process_windows.go`：进程树根 PID → 异步激活 → **固定 44.1kHz/16bit/2ch PCM**
    初始化（**不得调 `GetMixFormat`**，VAD 虚拟设备没有混合格式）→ 复用既有
    `loopbackStream` / `loopbackChannel` 的 pump / drain / 队列 / 溢出丢最旧 / Close 语义。
  - `open()` 改为按源类型选择 COM 套间：设备回环 STA，进程回环 **MTA**
    （`ActivateAudioInterfaceAsync` 的硬性要求）。
  - **能力判定改为「实测探测」**：`caps_windows.go` 不再用 Build 号推断，只认
    `processLoopbackAvailable()` 的结论；`20348` 退化为 `Reason` 文案素材。
    `Reason` 现在能区分「本机实测不支持」与「本版本尚未实现」两种原因。
  - 新增独立诊断工具 `cmd/probe-processloopback`，可先拿结论再排障。
- 实测结论（本机 Windows 10 Build **19045**）：**可用**。
  `ActivateAudioInterfaceAsync + PROCESS_LOOPBACK` 返回 `S_OK` —— 官方文档标注的
  20348 是「头文件随 SDK 公开发布」的版本，不代表运行时到该版本才支持。
  这正是「能力只认探测、不认版本号」的直接依据（FR-02-4）。
- 端到端验证（`.narrafork/14761.wav`，44.1k/16bit/2ch/17.12s，由 powershell SoundPlayer 播放）：
  - 抓 6 秒得 264600 帧，峰值 0.5384；
  - 源与捕获各取 10ms RMS 包络做归一化互相关，最佳对齐 -0.73s，**相关系数 0.9942**
    → 抓到的内容与目标应用播放的音频同源，不是混音或静音流；
  - 目标进程**重启后**（PID 变化）同一 `app:powershell.exe` 仍能续抓（两轮各 66150 帧）；
  - 进程在跑但不出声时可正常打开（Present ≠ Producing）。

## S1-18 PROCESS_LOOPBACK 被拒：E_ILLEGAL_METHOD_CALL（0x8000000E）
- 现象：首次实现时 `ActivateAudioInterfaceAsync` 同步返回 `E_ILLEGAL_METHOD_CALL`，
  连「真实设备 + `ACTIVATION_TYPE_DEFAULT`」这种一定支持的操作也失败；把
  `completionHandler` 传 `NULL` 则返回 `E_INVALIDARG`（说明 API 本身是活的、参数校验在跑）。
- 根因：**回调对象缺少 `IAgileObject`**。系统需要把 completion handler 封送进音频服务的套间，
  官方样例通过 `RuntimeClassFlags<ClassicCom> + FtmBase` 隐含满足该语义；裸 `IUnknown`
  会被拒。`E_INVALIDARG`/`E_ILLEGAL_METHOD_CALL` 的差异只取决于 handler 是否为空，
  与设备路径、PROPVARIANT、套间模型无关。
- 处置：`QueryInterface` 额外接受 `IID_IAgileObject`（并转发 `IID_IMarshal` 给
  `CoCreateFreeThreadedMarshaler`，与 FtmBase 行为对齐）。
- 状态：已解决

## S1-19 手写 COM 虚表槽位寻址错误
- 现象：激活后进程崩溃，`Exception 0xc0000005`，`PC=0xffffffffffffffff`（跳到 -1）。
- 根因：`Release` / `GetActivateResult` 的槽位写法是「对象地址 + slot 偏移」，
  但 COM 对象首字是**虚表指针**，槽位必须从**虚表**再偏移一次。给对象地址加偏移
  读到的是对象字段（垃圾），当成函数指针调用就跳到野地址。
- 处置：统一走 `comMethod(p, slot)`：先 `vtbl := *(*unsafe.Pointer)(p)`，再 `unsafe.Add(vtbl, slot*8)`。
- 状态：已解决

## S1-20 uintptr 转换导致回调对象被分配到栈上
- 现象：能力探测偶发失败，且「测试通过、生产失败」——同样代码在 `_test.go` 里正常，
  在 `open()` 路径里失败。
- 根因：把 `uintptr(unsafe.Pointer(h))` 传给 syscall 后，Go 的逃逸分析看不到指针「逃出去」，
  于是把 `h` 分配在当前 goroutine 的**栈**上；而激活是异步的，等栈帧回收后系统仍在用这个
  地址读写 vtable 与参数。测试里 `h` 因为被 return 出去才意外逃逸到堆，掩盖了问题。
- 处置：用包级 `activateHandlers sync.Map` 持有回调对象，既强制堆分配又保证异步期间可达。
- 状态：已解决

## S1-21 栈上出参缓冲在 syscall 前后失效（最隐蔽）
- 现象：`ActivateAudioInterfaceAsync` 返回 `S_OK`，但出参 `operation` 始终为 `nil`
  （表现为激活失败）。规律：经 `snapshotProcesses()` 之后**必挂**，不做快照时正常；
  在函数开头多调一次激活后，**第二次反而成功**。
- 根因：拿局部变量的地址 `&operation` 转成 `uintptr` 传给 Windows API。该地址指向
  **当前 goroutine 的栈**，一旦前后发生栈增长/栈拷贝（例如进程快照触发 GC 与栈扩张），
  地址即失效：API 把结果写进旧地址，调用方看到的是「成功但没回填」。
- 处置：所有交给系统的**参数与出参缓冲**一律放在堆对象上（`activateHandler` 的字段：
  `params` / `pv` / `operation` / `activateResult` / `activated`），**禁止**使用栈上局部变量的地址。
  字符串路径由 `UTF16PtrFromString` 分配在堆，安全。
- 状态：已解决

## S1-22 GetActivateResult 只能调用一次
- 现象：回调里读取激活结果后，等待线程再读就失败/为空。
- 根因：`IActivateAudioInterfaceAsyncOperation::GetActivateResult` 规定只能调用一次。
  若在 `ActivateCompleted` 里消费掉，等待线程就拿不到接口。
- 处置：回调**只置位事件**，不消费结果；由等待线程唯一一次调用 `GetActivateResult`。
- 状态：已解决

## S1-23 进程回环在目标退出后不发事件
- 现象：杀掉目标进程后，`Channel.Err()` 在 10s 内没有收到任何错误。
- 根因：进程级回环在目标进程消失后**只是停止出包**，不保证触发
  `AUDCLNT_E_DEVICE_INVALIDATED` 之类的事件（与设备回环拔设备的行为不同）。
- 处置：如实记录能力边界，测试不判失败。**源失效的可靠判定不能只靠 `Channel.Err()`**：
  建议上层（recordingsvc / monitor）对应用源定期校验根进程存活（如 500ms 轮询
  `Probe`），主动触发 `capture:invalid`（FR-02-5 / UC-03）。此项待后续里程碑实现。
- 状态：**未解决**（已知边界，需上层补轮询）


## S1-08 应用源枚举依赖「当前有音频会话」
- 现象：已安装但当前没在放声音的应用不会出现在 `ListSources` 里。
- 复现条件：打开一个不发声的客户端后调 `ListSources`。
- 根因：Windows 没有「潜在音频应用」的注册表/API；唯一可靠来源是 `IAudioSessionManager2` 的会话列表，而会话只在应用真正创建音频流时存在。
- 临时处置：接受该能力边界并在注释中写明。为弥补「应用在跑但没声音」的场景，`ProbeSource("app:xxx.exe")` 改为**按进程树**判断：进程存活 → `Present=true, Producing=false`，UI 用它区分「未运行」与「运行但无声」。
- 状态：改设计

## S1-09 默认设备标记只能塞进 DisplayName（i18n 缺口）
- 现象：默认输出设备的显示名被硬编码追加中文后缀「（默认）」。
- 复现条件：`ListSources` 结果里第一个设备源的 `DisplayName`。
- 根因：`contract.Source` 没有 `IsDefault` 字段，而 contract 属于前后端共享契约，S1 只做后端、不允许改；前端又规定人类可见字符串一律走 `t()`。
- 临时处置：用后缀表达并加注释。**建议**后续给 `contract.Source` 增加 `IsDefault bool`，由前端 i18n 渲染「默认」徽标，然后移除 `DefaultDeviceSuffix`。
- 状态：**已解决**（阶段收尾）。`contract.Source` 增加 `IsDefault bool`，`internal/capture` 移除 `DefaultDeviceSuffix`
  常量和 `displayName(isDefault)` 参数（改为无参 `displayName()`），两处构造设备源都改为写结构化字段；
  前端 `RecordPage` 用 `t('record.sources.defaultDevice')` 渲染「默认设备 / Default device」徽标。
  测试同步改为断言 `IsDefault` 唯一性。

## S1-10 linux/darwin 的 DeviceLoopback 报 false（能力 = 系统前提 && 已实现）
- 现象：Linux 上 PulseAudio/PipeWire 的 monitor 源系统层面几乎总是可用，但 `Capabilities()` 报 `DeviceLoopback=false`。
- 复现条件：`GOOS=linux go build` 后阅读 `caps_linux.go`，或在 Linux 上调用 `GetCapabilities`。
- 根因：本版本未实现任何非 Windows 采集路径。若报 `true` 而 `Open` 必然失败，就是欺骗性能力声明（UI 会引导用户点一个必然失败的选项）。
- 临时处置：`Reason` 文案里明确区分「平台限制」与「代码缺口」——Linux 的设备回环属于**代码缺口**（等版本即可），darwin 的设备回环属于平台限制（需虚拟声卡）。实现时同时改 `capabilities()` 与 `open()`。
- 状态：改设计

## S1-11 枚举跨所有设备做会话聚合 + 全量进程快照（开销）
- 现象：单次 `Enumerate()` 实测约 **24ms**（本机 8 个输出设备、3 个应用会话）。
- 复现条件：`go test ./internal/capture/ -run XXX -bench BenchmarkEnumerate -benchtime 5x`。
- 根因：为不漏掉「把音频输出到第二块声卡的应用」，会话按**所有**渲染设备聚合（而非只看默认设备）；进程树用 Toolhelp32 全表遍历。
- 临时处置：判定为可接受——2s 轮询下约占 **1.2%** 占空比。若将来需要优化，先降级为「默认设备 + 已选源所在设备」，**不要**降低轮询频率（会让应用启停检测变迟钝）。
- 状态：已绕过

## S1-12 PCM 队列溢出策略选择「丢最旧」
- 现象：消费者（录音管线）卡顿时，内部队列会写满。
- 复现条件：`TestPCMQueue` 中写入超过 4 秒容量的样本而不读取。
- 根因：生产者**绝不能阻塞**——阻塞 WASAPI 采集线程会导致驱动侧溢出丢包，并拖慢系统混音。
- 临时处置：容量按 4 秒（约 384k 样本）设计以吸收 GC/磁盘抖动；写满时丢弃最旧数据，并只记录**一次** WARN（避免刷屏）。丢数据的事实通过日志可诊断，而非静默。
- 状态：改设计

## S1-13 Toolhelp32 需要显式设置 ProcessEntry32.Size
- 现象：忘记设置 `Size` 时 `Process32First` 直接报错（`ERROR_INSUFFICIENT_BUFFER`/`ERROR_INVALID_PARAMETER`），拿到空进程表，结果是应用源列表全空但不报任何错——非常隐蔽。
- 复现条件：删掉 `entry.Size = uint32(unsafe.Sizeof(entry))`。
- 根因：Win32 的 `*32First` 系列要求调用方填写 `dwSize`（用于 ABI 版本兼容），Go 不会自动填。
- 临时处置：显式赋值；快照失败时降级为「不合并子进程、不校验存活」并打 WARN，而不是让整个枚举失败。
- 状态：已解决

## S1-14 windows.WAIT_TIMEOUT 的类型不是 uint32
- 现象：编译错误 `mismatched types uint32 and syscall.Errno`。
- 复现条件：写 `rc != windows.WAIT_TIMEOUT`（`rc` 来自 `windows.WaitForSingleObject`，是 `uint32`）。
- 根因：`golang.org/x/sys/windows` 里 `WAIT_TIMEOUT = Errno(0x102)`，而不是无类型常量。
- 临时处置：`rc != uint32(windows.WAIT_TIMEOUT)`，并加注释说明。
- 状态：已解决

## S1-15 系统提示音会话与自身进程需过滤
- 现象：会话列表里出现 `pid=0`（系统提示音）与本应用自身进程；`pid=0` 在进程快照里查不到，会让应用源出现空 exe 条目。
- 复现条件：默认设备上有系统提示音播放时调 `Enumerate()`（本机实测会话数为 4，其中 `pid=0`）。
- 根因：`IAudioSessionManager2` 会把系统音效会话与每个拥有音频流的进程都列出来。
- 临时处置：过滤 `pid == 0`、`pid == os.Getpid()`、以及 `AudioSessionStateExpired(2)`；被过滤原因写在注释里，避免后人当作 bug「修掉」。
- 状态：已解决

## S1-16 静音期事件不置位时仍必须取包
- 现象：如果只在 `WAIT_OBJECT_0` 时抽取数据，全系统静音期间会把「在抓但无声」误判成「抓不到音频」（ARCH §4.1.3 明确要求区分这两者）。
- 复现条件：静音一段时间后观察帧数与 `Producing`。
- 根因：共享模式回环在静音期的包到达节奏与事件信号不严格对应。
- 临时处置：`WaitForSingleObject` 用 200ms 超时，超时后**同样**执行一次 drain（顺带把 Close 的响应延迟上界压到 200ms）。实测 500ms 收到 48960 样本 ≈ 24480 帧 ≈ 48.96kHz，与声明的 48000Hz 吻合。
- 状态：已解决

## S1-17 go.mod 里 go-wca / go-ole 仍标注 `// indirect`
- 现象：`go.mod` 中 `github.com/moutend/go-wca v0.3.0` 与 `github.com/go-ole/go-ole v1.3.0` 位于间接依赖块并带 `// indirect` 注释，但 S1 已经在 `internal/capture` 里直接 import 它们。
- 复现条件：`go list -m all` 或直接看 `go.mod`。
- 根因：这两个库此前只作为其它依赖的间接依赖出现；S1 首次直接引用，但 S1 的约束是**不得修改 go.mod / go.sum**（由主 agent 统一处理）。
- 临时处置：不改动。`go build` / `go vet` / `go test` 均正常（`// indirect` 只是说明性注释，不影响编译）。**主 agent 收口时**跑一次 `go mod tidy` 会把两条移到直接依赖块，不引起版本变化、不新增下载。
- 状态：**已解决**。收口时执行 `go mod tidy`，`go-wca` / `go-ole` / `sherpa-onnx-go` / `golang.org/x/net` 均已移入直接依赖块。

---

## 汇总

| 状态 | 条目 |
|------|------|
| 未解决 | S1-23（进程回环目标退出后不发事件，需上层轮询补）、S1-09（默认设备标记的 i18n 缺口）、S1-17（go.mod 的 indirect 注释）|
| 已绕过 | S1-01、S1-02、S1-03、S1-11 |
| 已解决 | S1-04、S1-05、S1-06、S1-07、S1-13、S1-14、S1-15、S1-16、S1-18、S1-19、S1-20、S1-21、S1-22 |
| 改设计 | S1-08、S1-10、S1-12 |

**未解决 3 条**：
- S1-23 进程回环在目标进程退出后不触发事件，源失效必须由上层主动轮询根进程存活；
  这是平台行为，不是实现缺陷（设备回环拔设备时仍会正常上报）；
- S1-09 需要给 `contract.Source` 增字段（由主 agent 统一改契约）；
- S1-17 需要主 agent 在收口时跑 `go mod tidy`（轻微，不影响编译）。

**给后续里程碑的提醒**
- **能力判定只认实测探测结果，不要退回用 Build 号推断**。本机 Build 19045 实测支持
  `PROCESS_LOOPBACK`，而文档标注的门槛是 20348 —— 靠版本号判断会得出错误结论（S1-07）。
- 判断「能不能用」时优先跑 `go run ./cmd/probe-processloopback`，它会把真实 HRESULT 打出来。
- 不要把 `ProcessLoopback` 改成 true 而不实现 `Open`：那会引导用户选择必然失败的源（FR-02-4）。
  `processLoopbackImplemented` 与实际实现必须同时变动。
- 若将来给 `contract.Source` 加 `IsDefault`，记得同步删除 `capture.DefaultDeviceSuffix` 与对应的测试断言。
- 装配 `capturesvc` 时（`internal/app`）需要两步：`capturesvc.New(holder)`，再
  `SetSourcesChangedHandler(...)` 把源变化接到事件总线；退出时调 `StopMonitor()`。
  不接回调则只有「UI 请求时枚举」的被动行为。
- 写任何 Win32/COM 互操作代码前，先读 S1-19 / S1-20 / S1-21 三条——它们都是「编译通过、
  逻辑看着对、但运行时随机失败」的类型，且都源于同一个约束：**指针一旦转成 uintptr
  交给系统，对象就必须在堆上且生命周期由调用方保证**。

