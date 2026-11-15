// Package updates 实现「检查更新」的纯逻辑：版本比较与清单解析。
//
// 刻意**不做**网络请求：出站要走 httpx + outbound 留痕 + 离线闸门（FR-08），
// 那是 L3（internal/services/updatesvc）的职责。这里只做可单测的纯函数。
//
// 产品前提：不做静默自更新（docs/DEPLOYMENT.md §6.5）。本包只回答"有没有新版本"。
package updates

import (
	"strings"
	"time"

	"vip.oberon.mandaryn/internal/contract"
)

// ParseVersion 把 "v2.1.0" / "2.1.0" / "2.1.0-beta.1" 解析成可比较的整数三段。
//
// 只比较 MAJOR.MINOR.PATCH：预发布后缀（-beta.1）在本项目的发布策略里不存在，
// 遇到时按"去掉后缀"处理（宁可提示有新版本，也不要因为后缀把用户卡在旧版）。
// 无法解析时 ok=false —— 调用方应把它当作"无法判断"，而不是"没有更新"。
func ParseVersion(v string) (major, minor, patch int, ok bool) {
	s := strings.TrimSpace(v)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "v"), "V")
	if s == "" {
		return 0, 0, 0, false
	}
	// 去掉预发布 / 构建元数据
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) < 3 {
		return 0, 0, 0, false
	}
	nums := make([]int, 3)
	for i := 0; i < 3; i++ {
		n, ok := parseUint(parts[i])
		if !ok {
			return 0, 0, 0, false
		}
		nums[i] = n
	}
	return nums[0], nums[1], nums[2], true
}

func parseUint(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + int(r-'0')
		if n > 1_000_000 {
			return 0, false
		}
	}
	return n, true
}

// IsNewer 判断 latest 是否比 current 新。
//
// 任一方无法解析时返回 false（**不**提示更新）：宁可漏报，也不要因为解析失败
// 反复弹"有新版本"却下载不了。
func IsNewer(current, latest string) bool {
	cMaj, cMin, cPat, ok1 := ParseVersion(current)
	lMaj, lMin, lPat, ok2 := ParseVersion(latest)
	if !ok1 || !ok2 {
		return false
	}
	switch {
	case lMaj != cMaj:
		return lMaj > cMaj
	case lMin != cMin:
		return lMin > cMin
	default:
		return lPat > cPat
	}
}

// GitHubRelease 是 GitHub Releases API 的响应子集（只取用得到的字段）。
type GitHubRelease struct {
	TagName     string `json:"tag_name"`
	HTMLURL     string `json:"html_url"`
	Name        string `json:"name"`
	Prerelease  bool   `json:"prerelease"`
	Draft       bool   `json:"draft"`
	PublishedAt string `json:"published_at"` // RFC3339；解析失败时按"未知"处理
}

// ToInfo 把 GitHub 响应转成 contract.UpdateInfo。
//
// 规则：
//   - 预发布与草稿一律**不**视为更新（用户拿到的应该是正式版）；
//   - 有 tag 但解析不出语义化版本时，Latest 原样回填、HasUpdate=false；
//   - DownloadURL 用 html_url（发布页），后端**不**下载任何东西。
func (r GitHubRelease) ToInfo(current, source string) contract.UpdateInfo {
	latest := strings.TrimSpace(r.TagName)
	info := contract.UpdateInfo{
		Current:     current,
		Latest:      strings.TrimPrefix(latest, "v"),
		DownloadURL: r.HTMLURL,
		NotesURL:    r.HTMLURL,
		Source:      source,
	}
	if info.Latest == "" {
		info.Latest = strings.TrimSpace(r.Name)
	}
	if r.Prerelease || r.Draft {
		return info // 预发布/草稿不提示
	}
	if ts, err := time.Parse(time.RFC3339, r.PublishedAt); err == nil {
		info.PublishedAt = ts.UnixMilli()
	}
	info.HasUpdate = IsNewer(current, info.Latest)
	return info
}
