//go:build windows

package models

// Windows 的可用空间与网络盘探测。
//
// 只用 golang.org/x/sys/windows（已是项目依赖），不引入第三方库。

import (
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// freeBytes 返回 path 所在卷的可用字节数。
func freeBytes(path string) (int64, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return 0, err
	}
	p, err := windows.UTF16PtrFromString(abs)
	if err != nil {
		return 0, err
	}
	var freeAvail, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &freeAvail, &total, &totalFree); err != nil {
		return 0, err
	}
	return int64(freeAvail), nil
}

// isNetworkMount 判断路径是否为网络驱动器（DRIVE_REMOTE）。
//
// 为什么重要：跨设备 / 网络盘上 rename 不是原子操作，迁移必须降级为
// 「复制模式（不删源）」并提示用户（ARCH §4.6.3 一致性保障 4）。
func isNetworkMount(path string) bool {
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	vol := filepath.VolumeName(abs)
	if vol == "" || len(vol) < 2 || vol[1] != ':' {
		return strings.HasPrefix(strings.ReplaceAll(abs, `\`, "/"), "//")
	}
	root, err := windows.UTF16PtrFromString(vol + `\`)
	if err != nil {
		return false
	}
	// DRIVE_REMOTE = 4（winbase.h）
	return windows.GetDriveType(root) == windows.DRIVE_REMOTE
}
