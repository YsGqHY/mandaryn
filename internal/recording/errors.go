package recording

import (
	"errors"
	"syscall"
)

// errnoENOSPC 返回平台上的「磁盘空间不足」错误值。
//
// 分文件而不是直接写在 writer.go 里，是因为各平台的 errno 常量集合不同：
// 若将来某个平台没有 ENOSPC，只需改这一个函数（返回 errDiskFullSentinel）。
func errnoENOSPC() error { return syscall.ENOSPC }

// ErrDiskFull 暴露给测试的「磁盘满」判定（业务代码请用 wrapFSError 的归类结果）。
func ErrDiskFull(err error) bool { return isDiskFull(err) }

// asDiskFull 把任意错误标记为磁盘满（测试与上层注入故障用）。
func asDiskFull(err error) error {
	if err == nil {
		return errDiskFullSentinel
	}
	return errors.Join(errDiskFullSentinel, err)
}
