package models

// 测试进程级准备：把 cryptox 主密钥指到临时目录，避免污染用户真实数据目录。

import (
	"os"
	"testing"

	"vip.oberon.mandaryn/internal/utils/cryptox"
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "mandaryn-models-key-*")
	if err == nil {
		// SetBaseDir 只允许在首次加解密前调用；这里在测试进程启动时调用一次。
		_ = cryptox.SetBaseDir(dir)
	}
	code := m.Run()
	if dir != "" {
		_ = os.RemoveAll(dir)
	}
	os.Exit(code)
}
