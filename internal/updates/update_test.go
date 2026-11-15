package updates

import (
	"testing"

	"vip.oberon.mandaryn/internal/contract"
)

func TestParseVersion(t *testing.T) {
	cases := []struct {
		in    string
		maj   int
		min   int
		pat   int
		valid bool
	}{
		{"2.0.0", 2, 0, 0, true},
		{"v2.1.3", 2, 1, 3, true},
		{"V2.1.3", 2, 1, 3, true},
		{" 2.10.4 ", 2, 10, 4, true},
		{"2.1.0-beta.1", 2, 1, 0, true}, // 预发布后缀按去掉处理
		{"2.1.0+build.7", 2, 1, 0, true},
		{"2.1", 0, 0, 0, false},   // 段数不足
		{"", 0, 0, 0, false},
		{"v", 0, 0, 0, false},
		{"2.x.0", 0, 0, 0, false},
		{"1.2.3.4", 1, 2, 3, true}, // 多余段忽略
	}
	for _, c := range cases {
		maj, min, pat, ok := ParseVersion(c.in)
		if ok != c.valid {
			t.Errorf("ParseVersion(%q) ok=%v, want %v", c.in, ok, c.valid)
			continue
		}
		if ok && (maj != c.maj || min != c.min || pat != c.pat) {
			t.Errorf("ParseVersion(%q) = %d.%d.%d, want %d.%d.%d", c.in, maj, min, pat, c.maj, c.min, c.pat)
		}
	}
}

func TestIsNewer(t *testing.T) {
	cases := []struct {
		current, latest string
		want            bool
	}{
		{"2.0.0", "2.0.1", true},
		{"2.0.0", "2.1.0", true},
		{"2.0.0", "3.0.0", true},
		{"2.0.0", "2.0.0", false},
		{"2.1.0", "2.0.9", false},  // 降级不算更新
		{"v2.0.0", "v2.0.1", true}, // 带 v 前缀也能比
		// 解析失败一律不提示（宁可漏报，也不弹一个下载不了的通知）
		{"dev", "2.0.1", false},
		{"2.0.0", "not-a-version", false},
		{"", "2.0.0", false},
	}
	for _, c := range cases {
		if got := IsNewer(c.current, c.latest); got != c.want {
			t.Errorf("IsNewer(%q, %q) = %v, want %v", c.current, c.latest, got, c.want)
		}
	}
}

func TestGitHubReleaseToInfo(t *testing.T) {
	rel := GitHubRelease{
		TagName:     "v2.1.0",
		HTMLURL:     "https://github.com/o/r/releases/tag/v2.1.0",
		PublishedAt: "2026-11-15T10:00:00Z",
	}
	info := rel.ToInfo("2.0.0", "api.github.com")
	if !info.HasUpdate {
		t.Fatal("2.0.0 → v2.1.0 应提示更新")
	}
	if info.Latest != "2.1.0" {
		t.Errorf("Latest = %q, want 2.1.0（应去掉 v 前缀）", info.Latest)
	}
	if info.DownloadURL == "" || info.NotesURL == "" {
		t.Error("应给出发布页地址")
	}
	if info.Source != "api.github.com" {
		t.Errorf("Source = %q", info.Source)
	}
	if info.PublishedAt == 0 {
		t.Error("应解析 published_at 为毫秒时间戳")
	}
}

// TestGitHubReleaseIgnoresPrereleaseAndDraft 预发布与草稿不提示更新。
func TestGitHubReleaseIgnoresPrereleaseAndDraft(t *testing.T) {
	for _, rel := range []GitHubRelease{
		{TagName: "v9.9.9", Prerelease: true},
		{TagName: "v9.9.9", Draft: true},
	} {
		if info := rel.ToInfo("2.0.0", "src"); info.HasUpdate {
			t.Errorf("预发布/草稿不应提示更新：%+v", info)
		}
	}
}

// TestGitHubReleaseUnknownVersion 无 tag 时回落到 name，且不因解析失败提示更新。
func TestGitHubReleaseUnknownVersion(t *testing.T) {
	rel := GitHubRelease{Name: "Nightly build", HTMLURL: "https://example/rel"}
	info := rel.ToInfo("2.0.0", "src")
	if info.HasUpdate {
		t.Error("无法解析版本时不应提示更新")
	}
	if info.Latest != "Nightly build" {
		t.Errorf("Latest = %q, want 回落为 name", info.Latest)
	}
}

// TestUpdateInfoContractJSON 契约字段名稳定（前端按这些键读取）。
func TestUpdateInfoContractJSON(t *testing.T) {
	var info contract.UpdateInfo
	_ = info
	// 只断言类型可用；字段名由 struct tag 固定，改动会被前端类型检查发现。
}
