// Package storage 提供 SQLite + GORM 持久化层。
//
// 设计目标：让上层完全不需要关心 SQL / 迁移 / 平台差异：
//   - 模型只在 models.go 注册一次，启动自动 AutoMigrate
//   - 路径裁决统一交给 internal/paths（ADR-02），本包只负责「用给定路径开库」
//   - 读写并发由 SQLite WAL + 单连接池兜底（见 db.go / pragma.go）
package storage

import (
	"errors"
	"os"
	"path/filepath"

	"vip.oberon.mandaryn/internal/paths"
)

// 标识常量统一来自 internal/paths（ARCH §10.1：三处必须同时改，现在只有一处）。
// 这里 re-export 是为了让既有调用点（storagesvc 等）不必改动，也避免字符串漂移。
const (
	// AppDirName 数据根目录在用户目录下的子文件夹名。
	AppDirName = paths.AppDirName
	// DataDirEnv 是数据根目录的环境变量名（接管层之一）。
	// 实际解析统一走 paths.Resolver；保留此常量供日志/诊断文案使用。
	DataDirEnv = paths.EnvDataRoot
	// DBFileName SQLite 主文件名。
	DBFileName = paths.DBFileName
	// ConfigFileName 路径配置文件（独立于数据库：鸡生蛋，不能存库）。
	ConfigFileName = paths.ConfigFileName
)

// ErrNoUserDir 表示无法解析用户数据目录。
var ErrNoUserDir = errors.New("storage: cannot resolve user data directory")

// userDataDir 返回**平台默认**数据根目录（不含 cli/env/user 接管）：
//   - Windows: %AppData%\Mandaryn
//   - macOS:   ~/Library/Application Support/Mandaryn
//   - Linux:   ${XDG_DATA_HOME:-~/.local/share}/Mandaryn
//
// 注意：业务路径一律经 paths.Resolver 解析；本函数只用于两处「解析器自身
// 还不可用」的场合——计算默认值、以及 storage.json 的落点。
func userDataDir() (string, error) {
	return paths.DefaultDataRoot()
}

// DefaultDBPath 返回平台默认数据库文件完整路径。
// 该路径所在目录可能尚不存在；调用者通过 ensureDir 创建。
func DefaultDBPath() (string, error) {
	dir, err := userDataDir()
	if err != nil {
		return "", ErrNoUserDir
	}
	return filepath.Join(dir, DBFileName), nil
}

// DBPathIn 返回指定数据根下的数据库文件路径。
// 启动路径（internal/app）用解析器给出的 dataRoot 调它，而不是各自拼字符串。
func DBPathIn(dataRoot string) string {
	return filepath.Join(dataRoot, DBFileName)
}

// configFilePath 返回 storage.json 的完整路径。
//
// Pending requests must remain visible after a deployment override is removed.
func configFilePath() (string, error) {
	base, err := userDataDir()
	if err != nil {
		if managed := os.Getenv(paths.EnvDataRoot); managed != "" {
			return filepath.Join(managed, ConfigFileName), nil
		}
		return "", err
	}
	return filepath.Join(filepath.Dir(base), AppDirName+"-config", ConfigFileName), nil
}

// ensureDir 创建目录（含父级）；已存在不报错。
func ensureDir(path string) error {
	return os.MkdirAll(path, 0o755)
}
