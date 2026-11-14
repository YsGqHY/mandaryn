//go:build darwin

package perf

import (
	"fmt"
	"time"
)

// newPlatformReader 在 macOS 上**诚实降级**：返回 ErrUnsupported。
//
// 为什么不做（而是如实报告做不到）：
//   - 整机 CPU 需要 `host_processor_info` / `host_statistics(HOST_CPU_LOAD_INFO)`，
//     这两个是 Mach 陷阱调用；`golang.org/x/sys/unix` 里的 darwin 绑定没有导出它们
//     （只有 unexported 的 host_statistics），要调就得 cgo 或手写 trap ——
//     本域不值得为此引入平台相关汇编，也不许引第三方库。
//   - 只用 `getrusage(RUSAGE_SELF)` 能拿到本进程时间，但拿不到整机总量，
//     两分量缺一就算不出 `otherPct`。
//
// 于是上层的正确行为是「**不采样、不降档**」（`Service.Degraded() == true`，
// 档位恒为 L0、`Idle()` 恒为 true），而不是「假装空闲」——后者会让预算形同虚设，
// 且在 macOS 上悄悄跑满 CPU 却不自知。这条降级路径由 `service_test.go` 覆盖。
// 后续若为 macOS 补实现，只需替换本文件，其余逻辑与测试不动。
func newPlatformReader(_ int, _ time.Duration) (readFunc, error) {
	return nil, fmt.Errorf("%w（macOS 缺少无 cgo 的整机 CPU 采样源）", ErrUnsupported)
}
