package privacysvc

// 测试小工具：避免为两个 helper 引入额外依赖。

import (
	"os"
	"strconv"
)

// itoa 是整数格式化（测试里拼 URL / 行号用）。
func itoa(n int) string { return strconv.Itoa(n) }

// writeFile 写测试用文件。
func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}
