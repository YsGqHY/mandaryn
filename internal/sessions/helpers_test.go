package sessions

import (
	"regexp"
	"strings"
)

// srtTimePattern 是 SubRip 时间码的严格格式：hh:mm:ss,mmm --> hh:mm:ss,mmm。
// 逗号是规范要求（不是点），因此单测用它校验生成结果。
var srtTimePattern = regexp.MustCompile(`^\d{2,}:\d{2}:\d{2},\d{3} --> \d{2,}:\d{2}:\d{2},\d{3}$`)

// containsStr 判断子串是否存在。
func containsStr(haystack, needle string) bool { return strings.Contains(haystack, needle) }

// indexOf 返回子串位置（不存在返回 -1）。
func indexOf(haystack, needle string) int { return strings.Index(haystack, needle) }

// splitLines 按 \n 切分并去掉末尾空行。
func splitLines(s string) []string {
	lines := strings.Split(s, "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}
