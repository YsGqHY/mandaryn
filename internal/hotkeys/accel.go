// Package hotkeys 是「快捷键体系」的 L2 领域包（FR-09 / DOC-05 §2）。
//
// 分层边界（CLAUDE.md 铁律）：
//   - 本包**不 import Wails**。平台注册能力经 Registrar 接口注入，
//     真实现（app.GlobalShortcut / app.KeyBinding）由装配层 internal/app 提供；
//   - 绑定持久化走 SQLite（preferences 表，DOC-04 TBL-01.1），经 Store 接口注入，
//     禁止在 L2 里直接摸 *gorm.DB 或前端 localStorage；
//   - 面向前端的方法签名**不出现 func 类型参数**（绑定生成器会失败），
//     处理函数只经 Registry.SetHandler 在 Go 侧装配。
package hotkeys

import (
	"fmt"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// MaxAccelLen 加速键字符串的长度上限（rune 计）。
//
// 合法组合最长是「5 个修饰键 + 主键」，正常不会超过 32 字符；
// 设 64 是为了在明显异常输入（粘贴了一整段文本）时快速拒绝，而不是逐段解析。
const MaxAccelLen = 64

// maxAccelParts 组合被 "+" 拆开后的段数上限：最多 4 个修饰键 + 1 个主键。
const maxAccelParts = 5

// Mod 是修饰键位掩码。
//
// 用位掩码而不是切片：冲突检测与等价比较只需一次整数比较，
// 且天然去重（同一修饰键不可能被计两次）。
type Mod uint8

const (
	// ModCmdOrCtrl 是跨平台语义键：macOS 上是 Command，其它平台是 Control。
	// 默认绑定全部用它，这样同一份默认表在三平台都成立（DOC-05 §2.2）。
	ModCmdOrCtrl Mod = 1 << iota
	ModCtrl
	ModAlt
	ModShift
	// ModMeta 是「系统键」：macOS Command、Windows Win、Linux Super。
	// 用户在 macOS 上敲 Cmd 会被解析成 Meta（与 Command 同名等价），
	// 因此冲突检测必须按**平台物理键**比较，不能按规范形比较（见 Combo.PlatformKey）。
	ModMeta
)

// canonicalModOrder 决定规范形里修饰键的固定排列顺序。
//
// 顺序一旦固定，Shift+Ctrl+R 与 Ctrl+Shift+R 就天然等价，
// Format(Parse(s)) 也才能幂等（这是本包最重要的一条不变量）。
var canonicalModOrder = []Mod{ModCmdOrCtrl, ModCtrl, ModAlt, ModShift, ModMeta}

// modCanonicalName 规范形里的修饰键名字。
var modCanonicalName = map[Mod]string{
	ModCmdOrCtrl: "CmdOrCtrl",
	ModCtrl:      "Ctrl",
	ModAlt:       "Alt",
	ModShift:     "Shift",
	ModMeta:      "Meta",
}

// modAliases 用户输入别名 → 修饰键。
//
// 大小写由调用方先 Lower 再查表；允许用户写 Option / Command / Super / Win
// 是国内用户实际会用的几种写法（DOC-05 §2.4 的录制态直接收原始按键串）。
var modAliases = map[string]Mod{
	"cmdorctrl": ModCmdOrCtrl,
	"cmdorctl":  ModCmdOrCtrl,
	"mod":       ModCmdOrCtrl,

	"ctrl":    ModCtrl,
	"control": ModCtrl,
	"ctl":     ModCtrl,

	"alt":         ModAlt,
	"option":      ModAlt,
	"opt":         ModAlt,
	"optionoralt": ModAlt,

	"shift": ModShift,

	"cmd":     ModMeta,
	"command": ModMeta,
	"meta":    ModMeta,
	"super":   ModMeta,
	"win":     ModMeta,
	"windows": ModMeta,
	"⌘":       ModMeta,
	"❖":       ModMeta,
	"⌃":       ModCtrl,
	"⌥":       ModAlt,
	"⇧":       ModShift,
}

// glyphMods 是「无 + 号」形态（macOS 习惯，如 ⌘⌥R）的前导修饰键字形。
// 它们必须与 modAliases 里同名条目一致，故这里独立成表避免误把
// ⌘ 当成主键吃掉（⌘ 单字符也是可打印字符）。
var glyphMods = map[rune]Mod{
	'⌘': ModMeta,
	'❖': ModMeta,
	'⌃': ModCtrl,
	'⌥': ModAlt,
	'⇧': ModShift,
}

// keyAliases 主键别名 → 规范主键名。
//
// 规范主键名的选择原则：字母大写、功能键 F1..F35、具名键用 Go 风格驼峰
// （Space / Left / PageUp），这样规范形既适合落库也适合给人看。
var keyAliases = map[string]string{
	"space":      "Space",
	"spacebar":   "Space",
	"enter":      "Enter",
	"return":     "Enter",
	"escape":     "Escape",
	"esc":        "Escape",
	"tab":        "Tab",
	"backspace":  "Backspace",
	"back":       "Backspace",
	"delete":     "Delete",
	"del":        "Delete",
	"⌦":          "Delete",
	"insert":     "Insert",
	"ins":        "Insert",
	"home":       "Home",
	"end":        "End",
	"pageup":     "PageUp",
	"pgup":       "PageUp",
	"page up":    "PageUp",
	"prior":      "PageUp",
	"pagedown":   "PageDown",
	"pgdn":       "PageDown",
	"page down":  "PageDown",
	"next":       "PageDown",
	"left":       "Left",
	"leftarrow":  "Left",
	"←":          "Left",
	"right":      "Right",
	"rightarrow": "Right",
	"→":          "Right",
	"up":         "Up",
	"uparrow":    "Up",
	"↑":          "Up",
	"down":       "Down",
	"downarrow":  "Down",
	"↓":          "Down",
	",":          ",",
	"comma":      ",",
	".":          ".",
	"period":     ".",
	"/":          "/",
	"slash":      "/",
	";":          ";",
	"'":          "'",
	"[":          "[",
	"]":          "]",
	"\\":         "\\",
	"-":          "-",
	"=":          "=",
	"`":          "`",
}

// Reason 是加速键非法/冲突的具体原因键。
//
// 为什么不新造 apierr 码：DOC-03 §5 只登记了 ERR_HOTKEY_CONFLICT /
// ERR_HOTKEY_REGISTER 两个热键码，本包的「格式非法」统一走
// ERR_INVALID_ARGUMENT，用 Reason 放进 Detail 供前端区分文案
// （原因键是稳定字符串，允许前端按它映射 i18n）。
type Reason string

const (
	// ReasonEmpty 空串或纯空白。
	ReasonEmpty Reason = "empty"
	// ReasonTooLong 超长输入（> MaxAccelLen）。
	ReasonTooLong Reason = "too-long"
	// ReasonTooManyParts 段数超过「4 修饰键 + 1 主键」。
	ReasonTooManyParts Reason = "too-many-parts"
	// ReasonEmptyKey 有 + 号但主键段为空（Ctrl+ / Ctrl++）。
	ReasonEmptyKey Reason = "empty-key"
	// ReasonNoKey 只有修饰键没有主键（Ctrl / Shift+Alt）。
	ReasonNoKey Reason = "no-key"
	// ReasonUnknownToken 修饰键位置出现无法识别的段。
	ReasonUnknownToken Reason = "unknown-token"
	// ReasonUnknownKey 主键位置出现无法识别的键名。
	ReasonUnknownKey Reason = "unknown-key"
	// ReasonDuplicateModifier 同一修饰键出现两次（Ctrl+Ctrl）。
	ReasonDuplicateModifier Reason = "duplicate-modifier"
	// ReasonConflictingModifier CmdOrCtrl 与 Ctrl/Meta 同时出现（语义自相矛盾）。
	ReasonConflictingModifier Reason = "conflicting-modifier"
	// ReasonReserved 命中系统保留组合，不允许占用。
	ReasonReserved Reason = "reserved"
)

// AccelError 是加速键解析错误。它实现 error 并携带可判定的 Reason。
type AccelError struct {
	// Reason 具体原因键。
	Reason Reason
	// Input 原始输入（仅组合键文本，不含用户数据，可安全进日志）。
	Input string
	// Token 触发错误的那一段（可为空）。
	Token string
}

// Error 实现 error。
func (e *AccelError) Error() string {
	if e == nil {
		return ""
	}
	if e.Token != "" {
		return fmt.Sprintf("hotkey: %s (%q in %q)", e.Reason, e.Token, e.Input)
	}
	return fmt.Sprintf("hotkey: %s (%q)", e.Reason, e.Input)
}

// Combo 是解析后的组合键：修饰键集合 + 单个主键。
type Combo struct {
	// Mods 修饰键位掩码。
	Mods Mod
	// Key 规范主键名（"R" / "F2" / "Space" / "Left" / ","）。
	Key string
}

// Empty 报告组合是否为空值（零值 Combo 不是合法组合）。
func (c Combo) Empty() bool { return c.Key == "" }

// String 返回规范形（ASCII、修饰键固定顺序、不含平台差异）。
//
// 这是落库与前后端传输的唯一形式：同一物理组合在三平台得到同一字符串，
// 平台差异只在 Display / PlatformString 里体现。
func (c Combo) String() string {
	parts := make([]string, 0, len(canonicalModOrder)+1)
	for _, m := range canonicalModOrder {
		if c.Mods&m != 0 {
			parts = append(parts, modCanonicalName[m])
		}
	}
	if c.Key != "" {
		parts = append(parts, c.Key)
	}
	return strings.Join(parts, "+")
}

// Equal 判断两个组合的规范形是否相等（CmdOrCtrl+Alt+R 与 ⌘⌥R 在 macOS 上
// **不**相等，因为后者被解析成 Meta+Alt+R —— 物理等价但语义不同，
// 冲突检测请用 PlatformKey）。
func (c Combo) Equal(other Combo) bool { return c.String() == other.String() }

// Display 返回给人看的显示形式（DOC-05 §2.2 的「macOS」列）。
//
// os 传空时取 runtime.GOOS。macOS 用字形且不加分隔符（⌘⌥R），
// 其它平台用 Ctrl+Alt+R 形态；Windows 的系统键叫 Win，Linux 叫 Super。
func (c Combo) Display(os string) string {
	os = normalizeOS(os)
	if c.Empty() {
		return ""
	}
	if os == "darwin" {
		// macOS 按 ⌘⌥⇧⌃ 的习惯顺序排（DOC-05 §2.2 写的是 ⌘⌥R / ⌘⇧L，
		// Command 在前）。CmdOrCtrl 与 Meta 在 macOS 是同一个键，不会同时出现。
		var b strings.Builder
		for _, m := range darwinModOrder {
			if c.Mods&m == 0 {
				continue
			}
			if g, ok := darwinModGlyph[m]; ok {
				b.WriteString(g)
			}
		}
		if g, ok := darwinKeyGlyph[c.Key]; ok {
			b.WriteString(g)
		} else {
			b.WriteString(c.Key)
		}
		return b.String()
	}
	parts := make([]string, 0, len(canonicalModOrder)+1)
	for _, m := range canonicalModOrder {
		if c.Mods&m != 0 {
			parts = append(parts, platformModName(m, os))
		}
	}
	parts = append(parts, platformKeyName(c.Key))
	return strings.Join(parts, "+")
}

// platformKeyName 是非 macOS 平台的主键显示名。
//
// 方向键统一显示箭头（DOC-05 §2.2 两列都写的是 ← → ↑ ↓）：
// 箭头本身也能被 Parse 还原，往返不受影响。
func platformKeyName(key string) string {
	if g, ok := arrowGlyph[key]; ok {
		return g
	}
	return key
}

// arrowGlyph 是方向键的箭头字形（跨平台共用）。
var arrowGlyph = map[string]string{
	"Left":  "←",
	"Right": "→",
	"Up":    "↑",
	"Down":  "↓",
}

// PlatformString 返回**平台已解析**的加速键串，可直接交给 Wails
// （app.GlobalShortcut.Register / app.KeyBinding.Add 的 accelerator 参数）。
//
// 词汇表严格对齐 wails v3 的 parseAccelerator：修饰键只认
// cmdorctrl/cmd/command/ctrl/alt/option/shift/super，主键小写（含 page up 这种带空格的）。
//
// 装配层的 Registrar 实现把本函数的结果原样传下去即可，不需要再做转换。
func (c Combo) PlatformString(os string) string {
	os = normalizeOS(os)
	if c.Empty() {
		return ""
	}
	parts := make([]string, 0, len(canonicalModOrder)+1)
	for _, m := range canonicalModOrder {
		if c.Mods&m == 0 {
			continue
		}
		parts = append(parts, wailsModName(m, os))
	}
	parts = append(parts, wailsKeyName(c.Key))
	return strings.Join(parts, "+")
}

// PlatformKey 返回用于**冲突比较**的平台物理键标识。
//
// 为什么不能直接用 String：macOS 上 Cmd（Meta）与 CmdOrCtrl 是同一个物理键，
// 但规范形不同（CmdOrCtrl+Alt+R vs Meta+Alt+R）。若不按平台解析比较，
// 用户会把同一个键绑到两个动作上而检测不出来。
//
// 为什么不能直接用 PlatformString：平台解析后修饰键的排列顺序会随规范顺序
// 而变（⌘⌥R 与 ⌘⌥R 的两种规范形会产生 "Cmd+Option+r" / "Option+Cmd+r"），
// 这里把修饰键**按字典序**排好，保证「同一物理组合只有一种表示」。
func (c Combo) PlatformKey(os string) string {
	os = normalizeOS(os)
	if c.Empty() {
		return ""
	}
	mods := make([]string, 0, len(canonicalModOrder))
	for _, m := range canonicalModOrder {
		if c.Mods&m != 0 {
			mods = append(mods, wailsModName(m, os))
		}
	}
	sort.Strings(mods)
	mods = append(mods, wailsKeyName(c.Key))
	return strings.Join(mods, "+")
}

// Reserved 报告该组合是否属于系统保留组合（不允许被本应用占用）。
//
// 只对 global 作用域有意义：应用内快捷键不劫持系统组合（DOC-05 §2.1），
// 因此服务层只对 global 动作做这个判断。
func (c Combo) Reserved(os string) bool {
	os = normalizeOS(os)
	key := c.PlatformKey(os)
	for _, r := range reservedByOS[os] {
		if r == key {
			return true
		}
	}
	for _, r := range reservedAlways {
		if r == key {
			return true
		}
	}
	return false
}

// Parse 把用户输入解析成 Combo（含合法性校验）。
//
// 接受形态：
//   - 标准形："Ctrl+Shift+R"、"ctrl + shift + r"、"Shift+Ctrl+R"（顺序无关）
//   - 跨平台形："CmdOrCtrl+Alt+1"（macOS 上即 ⌘⌥1）
//   - macOS 字形："⌘⌥R"、"⇧⌘F"（无 + 号，前导字形当修饰键）
//   - 单键："F9"、"Space"、"Esc"、"←"
//
// 拒绝并返回 *AccelError 的典型输入：""、"   "、"Ctrl"、"Ctrl+Ctrl"、
// "Shift+Alt"、"Ctrl++"、"Ctrl+A+B"、超过 MaxAccelLen 的串。**不 panic**。
func Parse(input string) (Combo, error) {
	s := strings.TrimSpace(input)
	if s == "" {
		return Combo{}, &AccelError{Reason: ReasonEmpty, Input: input}
	}
	if len([]rune(s)) > MaxAccelLen {
		return Combo{}, &AccelError{Reason: ReasonTooLong, Input: input}
	}

	var mods Mod
	var keyTok string

	if !strings.Contains(s, "+") {
		// 无 + 号：前导字形当修饰键，其余整体是主键（⌘⌥R / F9 / Esc）。
		runes := []rune(s)
		i := 0
		for i < len(runes) {
			m, ok := glyphMods[runes[i]]
			if !ok {
				break
			}
			if mods&m != 0 {
				return Combo{}, &AccelError{Reason: ReasonDuplicateModifier, Input: input, Token: string(runes[i])}
			}
			mods |= m
			i++
		}
		keyTok = strings.TrimSpace(string(runes[i:]))
		if keyTok == "" {
			return Combo{}, &AccelError{Reason: ReasonNoKey, Input: input}
		}
	} else {
		parts := strings.Split(s, "+")
		if len(parts) > maxAccelParts {
			return Combo{}, &AccelError{Reason: ReasonTooManyParts, Input: input}
		}
		keyTok = strings.TrimSpace(parts[len(parts)-1])
		if keyTok == "" {
			return Combo{}, &AccelError{Reason: ReasonEmptyKey, Input: input}
		}
		for _, p := range parts[:len(parts)-1] {
			tok := strings.TrimSpace(p)
			if tok == "" {
				return Combo{}, &AccelError{Reason: ReasonEmptyKey, Input: input}
			}
			m, ok := lookupModifier(tok)
			if !ok {
				return Combo{}, &AccelError{Reason: ReasonUnknownToken, Input: input, Token: tok}
			}
			if mods&m != 0 {
				return Combo{}, &AccelError{Reason: ReasonDuplicateModifier, Input: input, Token: tok}
			}
			mods |= m
		}
	}

	// CmdOrCtrl 与 Ctrl/Meta 语义矛盾（一个键不可能既是「跨平台主修饰键」
	// 又显式指定了具体主修饰键），按非法处理而不是猜用户意图。
	if mods&ModCmdOrCtrl != 0 && mods&(ModCtrl|ModMeta) != 0 {
		return Combo{}, &AccelError{Reason: ReasonConflictingModifier, Input: input}
	}

	// 主键位置出现了修饰键名：要么是重复（Ctrl+Ctrl），要么是纯修饰键串
	// （Shift+Alt）。前者比后者更具体，优先报重复。
	if m, isMod := lookupModifier(keyTok); isMod {
		if mods&m != 0 {
			return Combo{}, &AccelError{Reason: ReasonDuplicateModifier, Input: input, Token: keyTok}
		}
		return Combo{}, &AccelError{Reason: ReasonNoKey, Input: input, Token: keyTok}
	}

	key, ok := canonicalKey(keyTok)
	if !ok {
		return Combo{}, &AccelError{Reason: ReasonUnknownKey, Input: input, Token: keyTok}
	}
	return Combo{Mods: mods, Key: key}, nil
}

// Normalize 返回规范形字符串，是最常用的「校验 + 规范化」入口。
func Normalize(input string) (string, error) {
	c, err := Parse(input)
	if err != nil {
		return "", err
	}
	return c.String(), nil
}

// Format 是 Normalize 的别名（装配层 / 测试里用 Format(Parse(s)) 表达幂等断言）。
func Format(input string) (string, error) { return Normalize(input) }

// lookupModifier 查修饰键别名表（大小写不敏感，允许前后空格）。
func lookupModifier(tok string) (Mod, bool) {
	m, ok := modAliases[strings.ToLower(strings.TrimSpace(tok))]
	return m, ok
}

// canonicalKey 把主键输入规范化。
//
// 注意：不把 "+" 当主键（"Ctrl++" 必须报错而不是变成 Ctrl 加号），
// 这样 String() 的输出永远能原样 Parse 回来（幂等的前提）。
func canonicalKey(tok string) (string, bool) {
	raw := strings.TrimSpace(tok)
	if raw == "" {
		return "", false
	}
	lower := strings.ToLower(raw)
	if v, ok := keyAliases[lower]; ok {
		return v, true
	}
	// F1..F35（Wails 的具名键表上限是 f35）。
	if len(lower) > 1 && lower[0] == 'f' {
		if n, err := strconv.Atoi(lower[1:]); err == nil && n >= 1 && n <= 35 {
			return "F" + strconv.Itoa(n), true
		}
	}
	runes := []rune(raw)
	if len(runes) != 1 {
		return "", false
	}
	r := runes[0]
	switch {
	case r >= 'a' && r <= 'z':
		return strings.ToUpper(string(r)), true
	case r >= 'A' && r <= 'Z':
		return string(r), true
	case r >= '0' && r <= '9':
		return string(r), true
	}
	// 单字符标点：必须是可打印的非 "+"、非空白字符。
	if r == '+' || r == ' ' || !strconv.IsPrint(r) {
		return "", false
	}
	if _, isGlyph := glyphMods[r]; isGlyph {
		return "", false
	}
	return string(r), true
}

// normalizeOS 把空串归一为运行平台，并把 syscall 风格的别名收敛成 runtime.GOOS 取值。
func normalizeOS(os string) string {
	switch strings.ToLower(strings.TrimSpace(os)) {
	case "":
		return runtime.GOOS
	case "mac", "macos", "osx", "darwin":
		return "darwin"
	case "win", "win32", "win64", "windows":
		return "windows"
	case "linux":
		return "linux"
	default:
		return strings.ToLower(strings.TrimSpace(os))
	}
}

// darwinModOrder 是 macOS 字形显示的顺序（⌘⌥⇧⌃，Command 在前，
// 与 DOC-05 §2.2 的 "⌘⌥R" / "⌘⇧L" 一致）。
// CmdOrCtrl 与 Meta 在 macOS 是同一个键（⌘），不会同时出现。
var darwinModOrder = []Mod{ModCmdOrCtrl, ModMeta, ModAlt, ModShift, ModCtrl}

// darwinModGlyph / darwinKeyGlyph 是 macOS 的字形显示表（按 DOC-05 §2.2）。
var (
	darwinModGlyph = map[Mod]string{
		ModCmdOrCtrl: "⌘",
		ModCtrl:      "⌃",
		ModAlt:       "⌥",
		ModShift:     "⇧",
		ModMeta:      "⌘",
	}
	darwinKeyGlyph = map[string]string{
		"Left":      "←",
		"Right":     "→",
		"Up":        "↑",
		"Down":      "↓",
		"Delete":    "⌦",
		"Enter":     "↩",
		"Escape":    "⎋",
		"Backspace": "⌫",
		"Tab":       "⇥",
		"PageUp":    "⇞",
		"PageDown":  "⇟",
		"Home":      "↖",
		"End":       "↘",
	}
)

// platformModName 是非 macOS 平台的显示名。
func platformModName(m Mod, os string) string {
	switch m {
	case ModCmdOrCtrl, ModCtrl:
		return "Ctrl"
	case ModAlt:
		return "Alt"
	case ModShift:
		return "Shift"
	case ModMeta:
		if os == "windows" {
			return "Win"
		}
		return "Super"
	}
	return modCanonicalName[m]
}

// wailsModName 对齐 wails v3 的 modifierMap 词汇（见 application/keys.go）。
//
// 关键差异：Wails 认 "super" 但**不认** "win"；macOS 上 Meta 与 Cmd 是同一键，
// 故统一发 "Cmd"。
func wailsModName(m Mod, os string) string {
	switch m {
	case ModCmdOrCtrl:
		if os == "darwin" {
			return "Cmd"
		}
		return "Ctrl"
	case ModCtrl:
		return "Ctrl"
	case ModAlt:
		if os == "darwin" {
			return "Option"
		}
		return "Alt"
	case ModShift:
		return "Shift"
	case ModMeta:
		if os == "darwin" {
			return "Cmd"
		}
		return "Super"
	}
	return modCanonicalName[m]
}

// wailsKeyName 把规范主键名转成 wails 认的主键名（小写、具名键带空格）。
func wailsKeyName(key string) string {
	switch key {
	case "PageUp":
		return "page up"
	case "PageDown":
		return "page down"
	case "Space":
		return "space"
	case "Insert":
		return "insert"
	}
	return strings.ToLower(key)
}

// reservedByOS 是各平台的系统保留组合（已按 PlatformKey 归一：修饰键字典序、主键小写）。
//
// 只收录「注册上去也一定失败或严重伤害系统体验」的组合，宁可少收：
// 误判会让用户觉得「明明没人用却不让我绑」。
var reservedByOS = map[string][]string{
	"windows": {
		"Super+l",           // 锁屏
		"Super+tab",         // 任务视图
		"Alt+Ctrl+delete",   // 安全注意序列（系统从不交给应用）
		"Ctrl+Shift+escape", // 任务管理器
	},
	"darwin": {
		"Cmd+tab",           // 应用切换
		"Cmd+space",         // Spotlight
		"Cmd+Option+escape", // 强制退出
		"Cmd+Ctrl+q",        // 锁屏
	},
	"linux": {
		"Super+l",      // 会话锁屏（GNOME/KDE 通用默认）
		"Super+escape", // 部分桌面环境的系统菜单
	},
}

// reservedAlways 与平台无关的保留组合。
var reservedAlways = []string{}
