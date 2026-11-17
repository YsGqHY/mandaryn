//go:build windows

package capture

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"unsafe"

	"github.com/go-ole/go-ole"
	"github.com/moutend/go-wca/pkg/wca"
	"golang.org/x/sys/windows"

	"vip.oberon.mandaryn/internal/contract"
)

// clsctxAll = CLSCTX_ALL（inproc server | inproc handler | local server | remote server）。
// 激活 COM 接口时的常规取值；COM 会挑可用的那一种。
const clsctxAll = 0x17

// stgmRead = STGM_READ，打开 IMMDevice 属性存储时的只读模式。
const stgmRead = 0

// minPrefixStemLen 子进程并入的最小可执行名主干长度。
//
// 前缀匹配太短的串会误吞无关进程（例如 "qq.exe" 的主干 "qq" 只有 2 个字符，
// 很容易撞上 "qqq.exe"），所以只对 3 字符以上的主干做「同前缀」合并。
const minPrefixStemLen = 3

// enumerate 实现 Enumerate：应用源 + 设备源。
//
// 所有 COM 调用都在 withCOM 的专用 STA 线程里完成（见 withCOM 注释）。
func enumerate() ([]contract.Source, error) {
	var (
		deviceSources []contract.Source
		appSources    []contract.Source
	)

	err := withCOM(func() error {
		enumerator, err := newDeviceEnumerator()
		if err != nil {
			return err
		}
		defer enumerator.Release()

		devices, defaultID, err := collectRenderDevices(enumerator)
		if err != nil {
			return err
		}
		// 设备指针必须在 COM 线程内释放。
		defer func() {
			for _, d := range devices {
				d.release()
			}
		}()

		deviceSources = buildDeviceSources(devices, defaultID)

		// 应用源按「所有渲染设备上的音频会话」聚合，而不是只看默认设备：
		// 用户可以把某个应用单独输出到第二块声卡，只看默认设备会漏掉这类应用。
		sessions := make([]audioSession, 0, 16)
		for _, d := range devices {
			sessions = append(sessions, collectDeviceSessions(d.dev)...)
		}

		procs, children, snapErr := snapshotProcesses()
		if snapErr != nil {
			// 进程快照失败不致命：降级为「不合并子进程、不校验存活」，
			// 仍能把在放音频的应用列出来。
			lg().Warn("进程快照失败，应用源将不包含子进程信息", "err", snapErr)
		}
		appSources = buildAppSources(sessions, procs, children)
		return nil
	})
	if err != nil {
		return nil, err
	}

	// 能力门控：本机探测不到进程级捕获时，应用源一律标为不可用。
	// 放在 withCOM 之外做——探测要自己开 MTA 线程，不该占用枚举的 STA 线程。
	capable := processLoopbackCapable()
	for i := range appSources {
		gateAppSource(&appSources[i], capable)
	}

	out := make([]contract.Source, 0, len(appSources)+len(deviceSources))
	out = append(out, appSources...)
	out = append(out, deviceSources...)
	return out, nil
}

// probe 实现 Probe（kind 已由 ParseSourceID 校验）。
func probe(kind, value string) (contract.Source, error) {
	switch kind {
	case contract.SourceKindDevice:
		return probeDevice(value)
	case contract.SourceKindApplication:
		return probeApplication(value)
	default:
		return contract.Source{}, sourceNotFound("未知的源类型：" + kind)
	}
}

// probeDevice 探测单个输出设备。
func probeDevice(deviceID string) (contract.Source, error) {
	var found *contract.Source

	err := withCOM(func() error {
		enumerator, err := newDeviceEnumerator()
		if err != nil {
			return err
		}
		defer enumerator.Release()

		devices, defaultID, err := collectRenderDevices(enumerator)
		if err != nil {
			return err
		}
		defer func() {
			for _, d := range devices {
				d.release()
			}
		}()

		nameSet := make(map[string]bool, len(devices))
		for _, d := range devices {
			nameSet[d.id] = true
			if d.id != deviceID {
				continue
			}
			src := contract.Source{
				ID:          DeviceSourceID(d.id),
				Kind:        contract.SourceKindDevice,
				DisplayName: d.displayName(),
				DeviceID:    d.id,
				Present:     true,
				Producing:   d.peak > ProducingPeakThreshold,
				IsDefault:   d.id == defaultID,
			}
			found = &src
		}
		if found == nil {
			return sourceNotFound(fmt.Sprintf("未检测到该输出设备（可能已拔出、被禁用或不是活动状态）：%s", deviceID))
		}
		return nil
	})
	if err != nil {
		return contract.Source{}, err
	}
	if found == nil {
		return contract.Source{}, sourceNotFound("未检测到该输出设备：" + deviceID)
	}
	return *found, nil
}

// probeApplication 探测单个应用源：进程是否存活 + 会话是否有音频。
//
// 与 Enumerate 的差别：即使该应用当前没有音频会话（没在放声音），
// 只要进程在跑就返回 Present=true（UI 从「未运行」变「已运行但无声」）。
func probeApplication(exe string) (contract.Source, error) {
	procs, children, err := snapshotProcesses()
	if err != nil {
		return contract.Source{}, sourceUnavailable("无法枚举进程树：" + err.Error())
	}

	roots, exeDisplay := findPIDsByExe(procs, exe)
	if len(roots) == 0 {
		return contract.Source{}, sourceNotFound(fmt.Sprintf("未找到正在运行的应用：%s", exe))
	}
	pids := mergeChildPIDs(roots, procs, children)

	// 会话电平：只在探测应用时才枚举会话（比全量 Enumerate 便宜）。
	// 匹配方式是按 PID：会话直接给出 pid，不需要再解析 exe。
	want := make(map[uint32]bool, len(pids))
	for _, pid := range pids {
		want[uint32(pid)] = true
	}

	var producing bool
	err = withCOM(func() error {
		enumerator, comErr := newDeviceEnumerator()
		if comErr != nil {
			// 拿不到会话信息不影响「进程是否存活」的结论。
			lg().Warn("探测应用电平时无法访问音频设备", "exe", exe, "err", comErr)
			return nil
		}
		defer enumerator.Release()

		devices, _, devErr := collectRenderDevices(enumerator)
		if devErr != nil {
			lg().Warn("探测应用电平时枚举设备失败", "exe", exe, "err", devErr)
			return nil
		}
		defer func() {
			for _, d := range devices {
				d.release()
			}
		}()

		for _, d := range devices {
			for _, s := range collectDeviceSessions(d.dev) {
				if want[s.pid] && s.peak > ProducingPeakThreshold {
					producing = true
				}
			}
		}
		return nil
	})
	if err != nil {
		return contract.Source{}, err
	}

	src := contract.Source{
		ID:          AppSourceID(exe),
		Kind:        contract.SourceKindApplication,
		DisplayName: exeDisplay,
		ExeName:     exe,
		PIDs:        pids,
		Present:     true,
		Producing:   producing,
		ChildCount:  len(pids) - len(roots),
	}
	// 与 Enumerate 同一门控：能力缺失时探测结论必须是「不可用」，
	// 否则前端 2s 一次的探测会把枚举标灰的源重新点亮，形成两处矛盾结论。
	gateAppSource(&src, processLoopbackCapable())
	return src, nil
}

// —— 设备枚举 ——

// renderDevice 是一次设备枚举的中间结果（持有需要 Release 的 COM 指针）。
type renderDevice struct {
	dev      *wca.IMMDevice
	id       string
	friendly string
	peak     float32
}

func (d *renderDevice) release() {
	if d.dev != nil {
		d.dev.Release()
		d.dev = nil
	}
}

// displayName 设备显示名（纯设备名，不带任何标记）。
//
// 「默认设备」由 contract.Source.IsDefault 表达，前端用 i18n 渲染徽标；
// 这里绝不拼人类可读文案（早期版本追加过「（默认）」后缀，属 i18n 违规，已移除）。
func (d *renderDevice) displayName() string {
	if d.friendly != "" {
		return d.friendly
	}
	// 友好名读不到时退化成端点 ID，至少不是空字符串让 UI 崩掉。
	return d.id
}

// newDeviceEnumerator 创建 IMMDeviceEnumerator（必须在已初始化 COM 的线程调用）。
func newDeviceEnumerator() (*wca.IMMDeviceEnumerator, error) {
	var enumerator *wca.IMMDeviceEnumerator
	if err := wca.CoCreateInstance(
		wca.CLSID_MMDeviceEnumerator, 0, clsctxAll,
		wca.IID_IMMDeviceEnumerator, &enumerator,
	); err != nil {
		return nil, sourceUnavailable("无法创建音频设备枚举器（Windows Audio 服务可能未运行）：" + err.Error())
	}
	return enumerator, nil
}

// collectRenderDevices 枚举所有活动的渲染（输出）设备，并读出默认设备 ID。
//
// 返回的 renderDevice 由调用方负责 Release。
func collectRenderDevices(enumerator *wca.IMMDeviceEnumerator) ([]*renderDevice, string, error) {
	var collection *wca.IMMDeviceCollection
	if err := enumerator.EnumAudioEndpoints(wca.ERender, wca.DEVICE_STATE_ACTIVE, &collection); err != nil {
		return nil, "", sourceUnavailable("枚举输出设备失败：" + err.Error())
	}
	defer collection.Release()

	var count uint32
	if err := collection.GetCount(&count); err != nil {
		return nil, "", sourceUnavailable("读取输出设备数量失败：" + err.Error())
	}

	// 默认设备：可能不存在（例如服务器/远程桌面只有虚拟设备时也可能失败），
	// 失败不致命，仅导致「默认」标记缺失。
	var defaultID string
	var defaultDev *wca.IMMDevice
	if err := enumerator.GetDefaultAudioEndpoint(wca.ERender, wca.DEVICE_STATE_ACTIVE, &defaultDev); err == nil && defaultDev != nil {
		var id string
		if err := defaultDev.GetId(&id); err == nil {
			defaultID = id
		}
		defaultDev.Release()
	}

	devices := make([]*renderDevice, 0, count)
	for i := uint32(0); i < count; i++ {
		var dev *wca.IMMDevice
		if err := collection.Item(i, &dev); err != nil || dev == nil {
			lg().Warn("跳过无法打开的输出设备", "index", i)
			continue
		}
		var id string
		if err := dev.GetId(&id); err != nil || id == "" {
			lg().Warn("跳过无法读取 ID 的输出设备", "index", i)
			dev.Release()
			continue
		}
		devices = append(devices, &renderDevice{
			dev:      dev,
			id:       id,
			friendly: deviceFriendlyName(dev),
			peak:     devicePeak(dev),
		})
	}
	return devices, defaultID, nil
}

// deviceFriendlyName 读取端点友好名（PKEY_Device_FriendlyName）。
func deviceFriendlyName(dev *wca.IMMDevice) string {
	var ps *wca.IPropertyStore
	if err := dev.OpenPropertyStore(stgmRead, &ps); err != nil || ps == nil {
		return ""
	}
	defer ps.Release()

	var pv wca.PROPVARIANT
	if err := ps.GetValue(&wca.PKEY_Device_FriendlyName, &pv); err != nil {
		return ""
	}
	// 只处理 VT_LPWSTR：其它类型走 PROPVARIANT.String() 会把 Val 当字符指针解引用，
	// 属于未定义行为（可能崩）。
	if pv.VT != ole.VT_LPWSTR {
		return ""
	}
	// String() 内部会 CoTaskMemFree 该字符串，所以只能取一次。
	return pv.String()
}

// devicePeak 读取设备当前峰值电平（0–1）。失败返回 0（当作没有音频）。
func devicePeak(dev *wca.IMMDevice) float32 {
	var meter *wca.IAudioMeterInformation
	if err := dev.Activate(wca.IID_IAudioMeterInformation, clsctxAll, nil, &meter); err != nil || meter == nil {
		return 0
	}
	defer meter.Release()

	var peak float32
	if err := meter.GetPeakValue(&peak); err != nil {
		return 0
	}
	return peak
}

// buildDeviceSources 把中间结果转成契约 DTO：默认设备排最前，其余按显示名排序。
func buildDeviceSources(devices []*renderDevice, defaultID string) []contract.Source {
	out := make([]contract.Source, 0, len(devices))
	for _, d := range devices {
		isDefault := d.id == defaultID && defaultID != ""
		out = append(out, contract.Source{
			ID:          DeviceSourceID(d.id),
			Kind:        contract.SourceKindDevice,
			DisplayName: d.displayName(),
			DeviceID:    d.id,
			Present:     true,
			Producing:   d.peak > ProducingPeakThreshold,
			IsDefault:   isDefault,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		iDef := out[i].DeviceID == defaultID && defaultID != ""
		jDef := out[j].DeviceID == defaultID && defaultID != ""
		if iDef != jDef {
			return iDef // 默认设备永远排第一
		}
		return strings.ToLower(out[i].DisplayName) < strings.ToLower(out[j].DisplayName)
	})
	return out
}

// —— 应用（会话）枚举 ——

// audioSession 一条音频会话的中间结果。
//
// 只留 pid 与电平：exe 名与进程树一律从 Toolhelp32 快照解析，
// 避免在 COM 循环里再开进程句柄（会被高权限进程挡住）。
type audioSession struct {
	pid  uint32
	peak float32 // 会话峰值电平
}

// collectDeviceSessions 枚举某个渲染设备上的音频会话。
//
// 单个会话出错只跳过该会话（COM 在会话并发过期时很常见），不影响其它会话。
func collectDeviceSessions(dev *wca.IMMDevice) []audioSession {
	var manager *wca.IAudioSessionManager2
	if err := dev.Activate(wca.IID_IAudioSessionManager2, clsctxAll, nil, &manager); err != nil || manager == nil {
		return nil
	}
	defer manager.Release()

	var sessions *wca.IAudioSessionEnumerator
	if err := manager.GetSessionEnumerator(&sessions); err != nil || sessions == nil {
		return nil
	}
	defer sessions.Release()

	var count int
	if err := sessions.GetCount(&count); err != nil || count <= 0 {
		return nil
	}

	self := uint32(os.Getpid())
	out := make([]audioSession, 0, count)
	for i := 0; i < count; i++ {
		var ctl *wca.IAudioSessionControl
		if err := sessions.GetSession(i, &ctl); err != nil || ctl == nil {
			continue
		}

		var ctl2 *wca.IAudioSessionControl2
		// GetSession 返回 IAudioSessionControl，需要 QI 到 2 才能拿到 PID。
		if err := ctl.PutQueryInterface(wca.IID_IAudioSessionControl2, &ctl2); err != nil || ctl2 == nil {
			ctl.Release()
			continue
		}

		var pid uint32
		_ = ctl2.GetProcessId(&pid)

		var state uint32
		_ = ctl.GetState(&state)

		peak := sessionPeak(ctl2)

		ctl2.Release()
		ctl.Release()

		// pid=0 是系统提示音会话；自己的进程不必出现在列表里（本应用不产生音频）。
		if pid == 0 || pid == self {
			continue
		}
		if state == 2 { // AudioSessionStateExpired：会话已过期，进程多半已退出
			continue
		}

		out = append(out, audioSession{pid: pid, peak: peak})
	}
	return out
}

// sessionPeak 读取会话峰值电平；会话不支持电平表时返回 0。
func sessionPeak(ctl2 *wca.IAudioSessionControl2) float32 {
	var meter *wca.IAudioMeterInformation
	if err := ctl2.PutQueryInterface(wca.IID_IAudioMeterInformation, &meter); err != nil || meter == nil {
		return 0
	}
	defer meter.Release()

	var peak float32
	if err := meter.GetPeakValue(&peak); err != nil {
		return 0
	}
	return peak
}

// procInfo 进程快照的一条记录。
type procInfo struct {
	pid  uint32
	ppid uint32
	exe  string // 原始大小写，如 "WeChatAppEx.exe"
}

// snapshotProcesses 用 Toolhelp32 一次性抓全量进程表。
//
// 为什么用 Toolhelp 而不是 OpenProcess + QueryFullProcessImageName：
//   - Toolhelp 直接给出 exe 文件名，不需要打开进程句柄，
//     因此对高权限进程（管理员启动的游戏/客户端）也不会因权限失败；
//   - 同一次快照还能拿到父 PID，用于合并子进程树。
func snapshotProcesses() (map[uint32]procInfo, map[uint32][]uint32, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, nil, err
	}
	defer windows.CloseHandle(snap)

	procs := make(map[uint32]procInfo, 256)
	children := make(map[uint32][]uint32, 256)

	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	if err := windows.Process32First(snap, &entry); err != nil {
		return nil, nil, err
	}
	for {
		exe := windows.UTF16ToString(entry.ExeFile[:])
		info := procInfo{pid: entry.ProcessID, ppid: entry.ParentProcessID, exe: exe}
		procs[info.pid] = info
		if info.ppid != 0 && info.ppid != info.pid {
			children[info.ppid] = append(children[info.ppid], info.pid)
		}
		if err := windows.Process32Next(snap, &entry); err != nil {
			break // ERROR_NO_MORE_FILES 表示枚举结束
		}
	}
	return procs, children, nil
}

// buildAppSources 把会话聚合为应用源。
func buildAppSources(sessions []audioSession, procs map[uint32]procInfo, children map[uint32][]uint32) []contract.Source {
	type agg struct {
		exe       string
		display   string
		roots     []int
		rootSeen  map[int]bool
		producing bool
	}
	groups := make(map[string]*agg, 8)
	order := make([]string, 0, 8)

	for _, s := range sessions {
		info, ok := procs[s.pid]
		if !ok || info.exe == "" {
			// 会话指向的进程已不在快照里：多半刚退出，忽略本次。
			continue
		}
		exe := strings.ToLower(info.exe)
		g, exists := groups[exe]
		if !exists {
			g = &agg{
				exe:      exe,
				display:  strings.TrimSuffix(info.exe, filepath.Ext(info.exe)),
				rootSeen: make(map[int]bool, 4),
			}
			groups[exe] = g
			order = append(order, exe)
		}
		pid := int(s.pid)
		if !g.rootSeen[pid] {
			g.rootSeen[pid] = true
			g.roots = append(g.roots, pid)
		}
		if s.peak > ProducingPeakThreshold {
			g.producing = true
		}
	}

	out := make([]contract.Source, 0, len(groups))
	for _, exe := range order {
		g := groups[exe]
		pids := mergeChildPIDs(g.roots, procs, children)
		out = append(out, contract.Source{
			ID:          AppSourceID(g.exe),
			Kind:        contract.SourceKindApplication,
			DisplayName: g.display,
			ExeName:     g.exe,
			PIDs:        pids,
			Present:     true,
			Producing:   g.producing,
			// 单进程应用为 0；微信这类有 WeChatAppEx.exe 子进程的应用 >0。
			ChildCount: len(pids) - len(g.roots),
		})
	}

	sort.SliceStable(out, func(i, j int) bool {
		a, b := strings.ToLower(out[i].DisplayName), strings.ToLower(out[j].DisplayName)
		if a != b {
			return a < b
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// mergeChildPIDs 把「可执行名同前缀」的后代进程并入捕获集合（ARCH §4.1.3）。
//
// 背景：进程级回环只能抓**同一进程树**的输出，但有的应用把音频渲染放在独立
// 子进程（微信的 WeChatAppEx.exe 就是典型），只抓根进程会一点声音都收不到。
// 这里以根进程 exe 的主干（去掉扩展名）做前缀匹配，把后代进程一并纳入。
func mergeChildPIDs(roots []int, procs map[uint32]procInfo, children map[uint32][]uint32) []int {
	pids := make([]int, 0, len(roots)+2)
	seen := make(map[int]bool, len(roots)+2)
	for _, r := range roots {
		if !seen[r] {
			seen[r] = true
			pids = append(pids, r)
		}
	}

	for _, root := range roots {
		rootInfo, ok := procs[uint32(root)]
		if !ok {
			continue
		}
		stem := strings.ToLower(strings.TrimSuffix(rootInfo.exe, filepath.Ext(rootInfo.exe)))
		if len(stem) < minPrefixStemLen {
			continue
		}

		// 广度优先遍历后代，避免深递归。
		queue := append([]uint32(nil), children[uint32(root)]...)
		for len(queue) > 0 {
			pid := queue[0]
			queue = queue[1:]
			if seen[int(pid)] {
				continue
			}
			info, exists := procs[pid]
			if !exists {
				continue
			}
			childStem := strings.ToLower(strings.TrimSuffix(info.exe, filepath.Ext(info.exe)))
			if strings.HasPrefix(childStem, stem) {
				seen[int(pid)] = true
				pids = append(pids, int(pid))
			}
			// 无论是否命中前缀都继续往下走：中间层可能是通用宿主进程
			// （例如 shell 宿主），真正的音频子进程在更深一层。
			queue = append(queue, children[pid]...)
		}
	}
	return pids
}

// findPIDsByExe 按 exe（小写）找出所有存活进程，返回 PID 列表与展示名。
func findPIDsByExe(procs map[uint32]procInfo, exe string) ([]int, string) {
	var (
		pids    []int
		display string
	)
	for pid, info := range procs {
		if strings.ToLower(info.exe) != exe {
			continue
		}
		if display == "" {
			display = strings.TrimSuffix(info.exe, filepath.Ext(info.exe))
		}
		pids = append(pids, int(pid))
	}
	sort.Ints(pids)
	if display == "" {
		display = strings.TrimSuffix(exe, filepath.Ext(exe))
	}
	return pids, display
}

// deviceExists 判断某个端点 ID 是否仍是活动输出设备。
func deviceExists(deviceID string) (bool, error) {
	exists := false
	err := withCOM(func() error {
		enumerator, err := newDeviceEnumerator()
		if err != nil {
			return err
		}
		defer enumerator.Release()

		devices, _, err := collectRenderDevices(enumerator)
		if err != nil {
			return err
		}
		defer func() {
			for _, d := range devices {
				d.release()
			}
		}()

		for _, d := range devices {
			if d.id == deviceID {
				exists = true
			}
		}
		return nil
	})
	return exists, err
}

// —— COM 线程管理 ——

// withCOM 在一条**独立的 STA 线程**上执行 fn，并在返回前完成 CoUninitialize。
//
// 为什么必须这么做（这是本包最容易崩的地方）：
//   - WASAPI / MMDevice 的 COM 对象有线程亲和性，只能在创建它的线程上调用。
//     Go 的 goroutine 会在 M 之间迁移，如果直接在当前 goroutine 里
//     CoInitialize + 调接口，中途换线程就会得到 RPC_E_WRONG_THREAD 或直接崩。
//   - 因此这里用一个新 goroutine + runtime.LockOSThread 钉住线程，
//     所有 COM 创建与释放都在同一线程内完成。
//   - 阻塞式等待（WaitForSingleObject）也只发生在这种线程里，不占用调度器。
//
// 代价：每次调用都会新建/销毁线程（约几十微秒级），
// 但枚举频率是 2s 一次（monitor）或用户手动刷新，可以接受。
func withCOM(fn func() error) error {
	done := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		owned, err := coInitializeSTA()
		if err != nil {
			done <- err
			return
		}
		if owned {
			// 只在自己初始化成功（S_OK）时配对 CoUninitialize；
			// S_FALSE 表示线程早已初始化过，多减一次会让别人后续调用失败。
			defer ole.CoUninitialize()
		}
		done <- fn()
	}()
	return <-done
}

// coInitializeSTA 把当前线程初始化为 STA。返回值 owned 表示是否由本次调用完成初始化。
func coInitializeSTA() (owned bool, err error) {
	initErr := ole.CoInitializeEx(0, ole.COINIT_APARTMENTTHREADED)
	if initErr == nil {
		return true, nil
	}

	var oleErr *ole.OleError
	if !errors.As(initErr, &oleErr) {
		return false, sourceUnavailable("初始化 COM 失败：" + initErr.Error())
	}
	switch hr := uint32(oleErr.Code()); hr {
	case 0: // S_OK
		return true, nil
	case 1: // S_FALSE：该线程已经初始化过同模型单元，可继续使用
		return false, nil
	case 0x80010106: // RPC_E_CHANGED_MODE：线程已在别的单元模型里初始化
		return false, sourceUnavailable("当前线程已处于其它 COM 单元模型（RPC_E_CHANGED_MODE），无法按 STA 访问音频设备")
	default:
		return false, sourceUnavailable(fmt.Sprintf("初始化 COM 失败（HRESULT=0x%08X）", hr))
	}
}
