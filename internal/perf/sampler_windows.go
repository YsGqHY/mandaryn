//go:build windows

package perf

import (
	"fmt"
	"os"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// —— Windows 采样源：GetSystemTimes（整机）+ GetProcessTimes（进程树）——
//
// 两者都以 100ns FILETIME 计数，单位一致，因此差值相除即可得到「整机归一」的
// 占用率（见 sampler.go 的量纲说明），不需要任何换算常数。

var (
	kernel32           = windows.NewLazySystemDLL("kernel32.dll")
	procGetSystemTimes = kernel32.NewProc("GetSystemTimes")
)

// filetimeToUint64 把 FILETIME 的高低 32 位拼成一个 64 位计数。
//
// 必须走 uint64 拼接而不是两个 uint32 相加，否则跨 4.29s（2^32 × 100ns）就会丢高位，
// 8 分钟的机器 uptime 之后每拍都算出垃圾值。这既是「回绕」最现实的一种形态，
// 也是单测里用大计数验证的场景。
func filetimeToUint64(ft windows.Filetime) uint64 {
	return uint64(ft.HighDateTime)<<32 | uint64(ft.LowDateTime)
}

// systemTimes 返回 (idle, total) 两个累计计数。
//
// 注意 Windows 的定义：kernel 时间**已经包含** idle。所以
//
//	total = kernel + user        // 全核可用的累计时间
//	busy  = total − idle
//
// 若把 total 写成 idle + kernel + user，空闲时间被算两遍，整机占用率会被系统性
// 低估近一半，进而永远不降档 —— 这是用 GetSystemTimes 最常见的错。
func systemTimes() (idle, total uint64, err error) {
	var i, k, u windows.Filetime
	r, _, e := procGetSystemTimes.Call(
		uintptr(unsafe.Pointer(&i)),
		uintptr(unsafe.Pointer(&k)),
		uintptr(unsafe.Pointer(&u)),
	)
	if r == 0 {
		return 0, 0, fmt.Errorf("perf: GetSystemTimes 失败: %w", e)
	}
	return filetimeToUint64(i), filetimeToUint64(k) + filetimeToUint64(u), nil
}

// processTimes 取指定进程的 kernel+user 累计时间。
func processTimes(h windows.Handle) (uint64, error) {
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil {
		return 0, err
	}
	return filetimeToUint64(kernel) + filetimeToUint64(user), nil
}

// winReader 持有本进程句柄与缓存的子进程句柄表。
type winReader struct {
	self        windows.Handle
	treeRefresh time.Duration

	mu     sync.Mutex
	tree   []windows.Handle
	treeAt time.Time
}

// newPlatformReader 构造 Windows 读数器。
func newPlatformReader(pid int, treeRefresh time.Duration) (readFunc, error) {
	// PROCESS_QUERY_LIMITED_INFORMATION 而不是 PROCESS_QUERY_INFORMATION：
	// 前者在 Vista+ 对同权限进程就够用，且不会被 UAC 拦。
	self, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return nil, fmt.Errorf("perf: 打开本进程句柄失败: %w", err)
	}
	r := &winReader{self: self, treeRefresh: treeRefresh}
	return r.read, nil
}

// read 取一拍原始计数。
func (r *winReader) read() (rawCounters, error) {
	idle, total, err := systemTimes()
	if err != nil {
		return rawCounters{}, err
	}
	self, err := processTimes(r.self)
	if err != nil {
		// 主进程句柄在本进程存活期间不该失效；真失效说明进程正在退出，
		// 这时继续采样没有意义，直接把错误抛给上层（上层会跳过本拍）。
		return rawCounters{}, fmt.Errorf("perf: GetProcessTimes 失败: %w", err)
	}

	// 推理若以子进程运行，必须并入 selfCpuPct（DOC-02 §4.7.5）。
	// 单个子进程取不到时间不算失败：它可能刚退出（句柄未刷新），
	// 漏算一次 500ms 的影响远小于本拍整体失败。
	sum := self
	for _, h := range r.childHandles() {
		if v, err := processTimes(h); err == nil {
			sum += v
		}
	}

	return rawCounters{SystemTotal: total, SystemIdle: idle, Self: sum, At: time.Now()}, nil
}

// childHandles 返回（可能过期的）子进程句柄表，按 treeRefresh 刷新。
func (r *winReader) childHandles() []windows.Handle {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	if r.tree != nil && r.treeRefresh > 0 && now.Sub(r.treeAt) < r.treeRefresh {
		return r.tree
	}
	for _, h := range r.tree {
		_ = windows.CloseHandle(h)
	}
	r.tree = openDescendants(uint32(os.Getpid()))
	r.treeAt = now
	return r.tree
}

// openDescendants 枚举 pid 的全部后代进程并各开一个查询句柄。
//
// 为什么要递归而不是只看直接子进程：procx 在 Windows 用 JobObject 托管进程组，
// 引擎加载器可能再套一层；只看一层会漏算真正吃 CPU 的那层。
func openDescendants(pid uint32) []windows.Handle {
	parents := snapshotParents()
	if len(parents) == 0 {
		return nil
	}

	// 建「父 → 子」索引后 BFS，避免 O(n²)。
	children := make(map[uint32][]uint32, len(parents))
	for child, parent := range parents {
		children[parent] = append(children[parent], child)
	}

	var out []windows.Handle
	queue := append([]uint32(nil), children[pid]...)
	seen := map[uint32]bool{pid: true}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		if h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, cur); err == nil {
			out = append(out, h)
		}
		queue = append(queue, children[cur]...)
	}
	return out
}

// snapshotParents 返回 pid → 父 pid 的全系统进程表。
func snapshotParents() map[uint32]uint32 {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(snap)

	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	out := make(map[uint32]uint32, 256)
	for err = windows.Process32First(snap, &entry); err == nil; err = windows.Process32Next(snap, &entry) {
		out[entry.ProcessID] = entry.ParentProcessID
	}
	return out
}
