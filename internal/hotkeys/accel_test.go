package hotkeys

import (
	"errors"
	"strings"
	"testing"
)

// TestParseEquivalence 断言同一物理组合的不同书写形式得到同一规范形。
func TestParseEquivalence(t *testing.T) {
	groups := [][]string{
		{"Ctrl+Shift+R", "ctrl + shift + r", "Shift+Ctrl+R", "CONTROL+SHIFT+r", "Control + Shift + R"},
		{"CmdOrCtrl+Alt+1", "mod+alt+1", "Alt+CmdOrCtrl+1", "alt + mod + 1"},
		{"F9", "f9", " F9 "},
		{"Escape", "Esc", "escape"},
		{"Enter", "Return"},
		{"Space", "spacebar"},
		{"Left", "leftarrow", "←"},
		{"Delete", "Del", "⌦"},
		{"PageUp", "pgup", "page up"},
		{"Meta+Alt+R", "⌘⌥R", "Alt+Meta+R"},
		{"Meta+Shift+L", "⌘⇧L", "Shift+Meta+L"},
	}
	for _, group := range groups {
		want, err := Normalize(group[0])
		if err != nil {
			t.Fatalf("Normalize(%q) 失败: %v", group[0], err)
		}
		for _, in := range group[1:] {
			got, err := Normalize(in)
			if err != nil {
				t.Fatalf("Normalize(%q) 失败: %v", in, err)
			}
			if got != want {
				t.Errorf("等价书写不一致: %q → %q，而 %q → %q", group[0], want, in, got)
			}
		}
	}
}

// TestFormatIdempotent 断言 Format(Parse(x)) 幂等：
// Format(Parse(x)) == Format(Parse(Format(Parse(x))))。
func TestFormatIdempotent(t *testing.T) {
	inputs := []string{
		"Ctrl+Shift+R", "ctrl + shift + r", "Shift+Ctrl+R",
		"CmdOrCtrl+Alt+R", "⌘⌥R", "⌘⇧L", "⇧⌘F",
		"Space", "F2", "F35", "Delete", "⌦", "←", "PageDown", "CmdOrCtrl+,",
		"Meta+Alt+Shift+Ctrl+K", "Ctrl+1", "a", "1", ";",
	}
	for _, in := range inputs {
		once, err := Format(in)
		if err != nil {
			t.Fatalf("Format(%q) 失败: %v", in, err)
		}
		twice, err := Format(once)
		if err != nil {
			t.Fatalf("Format(%q) 失败: %v", once, err)
		}
		if once != twice {
			t.Errorf("幂等失败: %q → %q → %q", in, once, twice)
		}
	}
}

// TestParseInvalid 逐个断言非法输入的原因键，并确认不 panic。
func TestParseInvalid(t *testing.T) {
	cases := []struct {
		in   string
		want Reason
	}{
		{"", ReasonEmpty},
		{"   ", ReasonEmpty},
		{"\t\n", ReasonEmpty},
		{"Ctrl", ReasonNoKey},
		{"Shift+Alt", ReasonNoKey},
		{"CmdOrCtrl", ReasonNoKey},
		{"⌘", ReasonNoKey},
		{"Ctrl+Ctrl", ReasonDuplicateModifier},
		{"Ctrl+Shift+Ctrl+R", ReasonDuplicateModifier},
		{"⌘⌘R", ReasonDuplicateModifier},
		{"Ctrl++", ReasonEmptyKey},
		{"Ctrl+", ReasonEmptyKey},
		{"+R", ReasonEmptyKey},
		{"Ctrl+A+B", ReasonUnknownToken},
		{"Ctrl+Foo", ReasonUnknownKey},
		{"Ctrl+plus", ReasonUnknownKey},
		{"Ctrl+A+B+C+D+E", ReasonTooManyParts},
		{strings.Repeat("Ctrl+", 20) + "R", ReasonTooLong},
		{"CmdOrCtrl+Ctrl+R", ReasonConflictingModifier},
		{"CmdOrCtrl+Meta+R", ReasonConflictingModifier},
	}
	for _, c := range cases {
		_, err := Parse(c.in)
		if err == nil {
			t.Errorf("Parse(%q) 期望失败，实际通过", c.in)
			continue
		}
		var ae *AccelError
		if !errors.As(err, &ae) {
			t.Errorf("Parse(%q) 错误类型不是 *AccelError: %T", c.in, err)
			continue
		}
		if ae.Reason != c.want {
			t.Errorf("Parse(%q) 原因 = %s，期望 %s", c.in, ae.Reason, c.want)
		}
	}
}

// TestParseDoesNotPanic 用一批随机/畸形输入确认解析器只会返回错误。
func TestParseDoesNotPanic(t *testing.T) {
	inputs := []string{
		"+", "++", "+++", "Ctrl++++R", "  +  ", "，", "😀", "Ctrl+😀",
		"F0", "F36", "f999999999999999999999", "Meta", "super+", "\x00",
		strings.Repeat("x", MaxAccelLen), "Ctrl+" + strings.Repeat("y", MaxAccelLen),
	}
	for _, in := range inputs {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("Parse(%q) panic: %v", in, r)
				}
			}()
			_, _ = Parse(in)
		}()
	}
}

// TestParseCanonicalForms 断言规范形的具体形态（修饰键顺序固定）。
func TestParseCanonicalForms(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Shift+Ctrl+R", "Ctrl+Shift+R"},
		{"⌘⌥R", "Alt+Meta+R"},
		{"shift+alt+ctrl+meta+k", "Ctrl+Alt+Shift+Meta+K"},
		{"ctrl+alt+r", "Ctrl+Alt+R"},
		{"F9", "F9"},
		{"esc", "Escape"},
		{"cmdorctrl+,", "CmdOrCtrl+,"},
		{"cmdorctrl+1", "CmdOrCtrl+1"},
	}
	for _, c := range cases {
		got, err := Normalize(c.in)
		if err != nil {
			t.Fatalf("Normalize(%q): %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("Normalize(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

// TestDisplayPerPlatform 断言三平台显示形与 DOC-05 §2.2 一致。
func TestDisplayPerPlatform(t *testing.T) {
	combo, err := Parse("CmdOrCtrl+Alt+R")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ os, want string }{
		{"darwin", "⌘⌥R"},
		{"windows", "Ctrl+Alt+R"},
		{"linux", "Ctrl+Alt+R"},
	}
	for _, c := range cases {
		if got := combo.Display(c.os); got != c.want {
			t.Errorf("Display(%s) = %q，期望 %q", c.os, got, c.want)
		}
	}

	meta, err := Parse("Meta+L")
	if err != nil {
		t.Fatal(err)
	}
	if got := meta.Display("windows"); got != "Win+L" {
		t.Errorf("Win 键显示 = %q，期望 Win+L", got)
	}
	if got := meta.Display("linux"); got != "Super+L" {
		t.Errorf("Linux 系统键显示 = %q，期望 Super+L", got)
	}
	if got := meta.Display("darwin"); got != "⌘L" {
		t.Errorf("macOS 系统键显示 = %q，期望 ⌘L", got)
	}
}

// TestPlatformStringWailsVocabulary 断言交给 Wails 的串只用它认得的关键字
// （见 wails v3 application/keys.go 的 modifierMap / namedKeys）。
func TestPlatformStringWailsVocabulary(t *testing.T) {
	cases := []struct {
		in   string
		os   string
		want string
	}{
		{"CmdOrCtrl+Alt+R", "windows", "Ctrl+Alt+r"},
		{"CmdOrCtrl+Alt+R", "darwin", "Cmd+Option+r"},
		{"Meta+L", "windows", "Super+l"},
		{"Meta+L", "linux", "Super+l"},
		{"Meta+L", "darwin", "Cmd+l"},
		{"CmdOrCtrl+,", "windows", "Ctrl+,"},
		{"PageUp", "windows", "page up"},
		{"CmdOrCtrl+Shift+L", "windows", "Ctrl+Shift+l"},
	}
	allowedMods := map[string]bool{
		"cmdorctrl": true, "cmd": true, "command": true, "ctrl": true,
		"alt": true, "option": true, "shift": true, "super": true,
	}
	for _, c := range cases {
		combo, err := Parse(c.in)
		if err != nil {
			t.Fatalf("Parse(%q): %v", c.in, err)
		}
		got := combo.PlatformString(c.os)
		if got != c.want {
			t.Errorf("PlatformString(%q, %s) = %q，期望 %q", c.in, c.os, got, c.want)
		}
		for _, part := range strings.Split(got, "+") {
			if allowedMods[strings.ToLower(part)] {
				continue
			}
			// 主键段只允许小写字母 / 数字 / 单个标点 / 具名键。
			if part == "Win" || part == "Meta" || part == "Command" {
				t.Errorf("PlatformString(%q, %s) 使用了 Wails 不认的修饰键名 %q", c.in, c.os, part)
			}
		}
	}
}

// TestPlatformKeyPhysicalIdentity 断言冲突比较用平台物理键：
// macOS 上 CmdOrCtrl 与 Meta 是同一个键。
func TestPlatformKeyPhysicalIdentity(t *testing.T) {
	a, _ := Parse("CmdOrCtrl+Alt+R")
	b, _ := Parse("⌘⌥R")
	if a.String() == b.String() {
		t.Fatalf("前置断言失败：规范形不该相等（%s vs %s）", a.String(), b.String())
	}
	if a.PlatformKey("darwin") != b.PlatformKey("darwin") {
		t.Errorf("macOS 上 CmdOrCtrl+Alt+R 与 ⌘⌥R 应视为同一物理键")
	}
	if a.PlatformKey("windows") == b.PlatformKey("windows") {
		t.Errorf("Windows 上 Ctrl+Alt+R 与 Win+Alt+R 不该视为同一物理键")
	}
}

// TestReservedCombos 断言系统保留组合被识别（只对 global 有意义，判定在服务层）。
func TestReservedCombos(t *testing.T) {
	reserved := []struct{ in, os string }{
		{"Meta+L", "windows"},
		{"Ctrl+Alt+Delete", "windows"},
		{"Cmd+Tab", "darwin"},
		{"Cmd+Space", "darwin"},
	}
	for _, c := range reserved {
		combo, err := Parse(c.in)
		if err != nil {
			t.Fatalf("Parse(%q): %v", c.in, err)
		}
		if !combo.Reserved(c.os) {
			t.Errorf("%q 在 %s 上应判定为系统保留组合", c.in, c.os)
		}
	}
	combo, _ := Parse("CmdOrCtrl+Alt+R")
	if combo.Reserved("windows") {
		t.Errorf("Ctrl+Alt+R 不应被判定为系统保留组合")
	}
}

// TestBuiltinDefaultsValid 断言内置动作表的默认值全部可解析、可往返、ID 唯一。
func TestBuiltinDefaultsValid(t *testing.T) {
	actions := BuiltinActions()
	if len(actions) != 18 {
		t.Errorf("内置动作数 = %d，期望 18（DOC-05 §2.2 的 17 项 + 精简模式开关）", len(actions))
	}
	seen := map[string]bool{}
	globalCount := 0
	for _, a := range actions {
		if seen[a.ID] {
			t.Errorf("动作 ID 重复: %s", a.ID)
		}
		seen[a.ID] = true
		if !strings.Contains(a.ID, ".") {
			t.Errorf("动作 ID 应为 <组>.<动作> 形态: %s", a.ID)
		}
		if a.Scope == "global" {
			globalCount++
		}
		if a.Default == "" {
			continue
		}
		once, err := Format(a.Default)
		if err != nil {
			t.Errorf("动作 %s 的默认值 %q 无法解析: %v", a.ID, a.Default, err)
			continue
		}
		if once != a.Default {
			t.Errorf("动作 %s 的默认值 %q 不是规范形，应为 %q", a.ID, a.Default, once)
		}
		twice, _ := Format(once)
		if once != twice {
			t.Errorf("动作 %s 默认值往返不幂等: %q → %q", a.ID, once, twice)
		}
	}
	if globalCount != 3 {
		t.Errorf("全局动作数 = %d，期望 3（DOC-05 §2.1 只允许 3 个隔空操作）", globalCount)
	}
}

// TestDefaultsMatchUXSpec 逐行核对 DOC-05 §2.2 的默认绑定表（三处必须同步：
// DOC-04 app_settings / DOC-05 / 本包常量）。
func TestDefaultsMatchUXSpec(t *testing.T) {
	// action → {Windows/Linux, macOS}
	want := map[string][2]string{
		ActionRecordToggle:     {"Ctrl+Alt+R", "⌘⌥R"},
		ActionRecordPause:      {"Ctrl+Alt+P", "⌘⌥P"},
		ActionWindowShow:       {"Ctrl+Alt+M", "⌘⌥M"},
		ActionPlaybackPlay:     {"Space", "Space"},
		ActionPlaybackBack:     {"←", "←"},
		ActionPlaybackForward:  {"→", "→"},
		ActionPlaybackPrevSeg:  {"↑", "↑"},
		ActionPlaybackNextSeg:  {"↓", "↓"},
		ActionPlaybackRecenter: {"Ctrl+J", "⌘J"},
		ActionNavRecord:        {"Ctrl+1", "⌘1"},
		ActionNavLibrary:       {"Ctrl+2", "⌘2"},
		ActionNavModels:        {"Ctrl+3", "⌘3"},
		ActionNavSettings:      {"Ctrl+,", "⌘,"},
		ActionSearchFocus:      {"Ctrl+F", "⌘F"},
		ActionRoleRename:       {"F2", "F2"},
		ActionSessionDelete:    {"Delete", "⌦"},
		ActionModelSwitchSrc:   {"Ctrl+Shift+L", "⌘⇧L"},
		ActionLeanMode:         {"", ""}, // 默认不绑定
	}

	actions := BuiltinActions()
	for _, a := range actions {
		expect, ok := want[a.ID]
		if !ok {
			t.Fatalf("内置动作 %s 在 DOC-05 §2.2 里没有对应行（新增动作必须同步文档）", a.ID)
		}
		if a.Default == "" {
			if expect[0] != "" {
				t.Errorf("%s 默认值缺失，文档要求 %q", a.ID, expect[0])
			}
			continue
		}
		c, err := Parse(a.Default)
		if err != nil {
			t.Fatalf("Parse(%q): %v", a.Default, err)
		}
		if got := c.Display("windows"); got != expect[0] {
			t.Errorf("%s 的 Windows/Linux 显示 = %q，文档 = %q", a.ID, got, expect[0])
		}
		if got := c.Display("darwin"); got != expect[1] {
			t.Errorf("%s 的 macOS 显示 = %q，文档 = %q", a.ID, got, expect[1])
		}
	}
}

// TestDefaultsDisplayRoundTrip 断言显示形能被 Set 原样回写（前端把列表里的
// 组合再存回去时不会变成非法值）。
func TestDefaultsDisplayRoundTrip(t *testing.T) {
	svc := New(nil, nil, nil, Emit{})
	for _, os := range []string{"windows", "darwin", "linux"} {
		for _, a := range BuiltinActions() {
			if a.Default == "" {
				continue
			}
			c, err := Parse(a.Default)
			if err != nil {
				t.Fatalf("Parse(%q): %v", a.Default, err)
			}
			shown := c.Display(os)
			back, err := Parse(shown)
			if err != nil {
				t.Errorf("[%s] %s 的显示形 %q 无法解析: %v", os, a.ID, shown, err)
				continue
			}
			// 显示形回写后必须落到**同一物理键**（CmdOrCtrl 与 Meta 在 macOS
			// 是同一个键，规范形不同不算问题）。
			if back.PlatformKey(os) != c.PlatformKey(os) {
				t.Errorf("[%s] %s 显示形 %q 回写后平台键 = %q，期望 %q",
					os, a.ID, shown, back.PlatformKey(os), c.PlatformKey(os))
			}
		}
	}
	_ = svc
}
