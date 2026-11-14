//go:build !windows

package models

// Unix（macOS / Linux）的可用空间与网络盘探测。
//
// 网络挂载的可靠识别需要读 /proc/mounts 或 getmntinfo，各平台接口不同且容易
// 误判，这里只做「POSIX 双斜杠网络路径」这一确定性子集；其余情况返回 false，
// 由迁移的 verify 阶段兜底（校验不过就回滚，不会破坏源目录）。

import "syscall"

// freeBytes 返回 path 所在文件系统的可用字节数。
func freeBytes(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

// isNetworkMount 见文件头说明。
func isNetworkMount(string) bool { return false }
