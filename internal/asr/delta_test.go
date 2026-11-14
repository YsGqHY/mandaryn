package asr

import (
	"strings"
	"testing"
)

// 增量裁剪的行为契约（`asr.StreamingDelta`）。
//
// 为什么值得单独测：`StreamingEngine.Accept` 的契约是「本次新增」，而几乎所有
// 流式后端（sherpa 的 online recognizer 就是）给的都是「自上次 Reset 以来的全量」。
// 这层换算一旦写错，症状是**整句重复**或**半个汉字（非法 UTF-8）**，
// 而且只在真实语音上才暴露 —— 所以在纯 Go 里用表驱动把边界钉死。

// TestStreamingDeltaMonotonic 覆盖**正常路径**：全量文本单调增长。
func TestStreamingDeltaMonotonic(t *testing.T) {
	var d StreamingDelta
	steps := []struct {
		full  string
		want  string
		byNow string // 累计已发出（用于确认没有丢字 / 重复）
	}{
		{"", "", ""},
		{"你", "你", "你"},
		{"你好", "好", "你好"},
		{"你好世界", "世界", "你好世界"},
		// 同一个全量文本再来一次：没有新字，必须返回空串（不能重复吐）。
		{"你好世界", "", "你好世界"},
		// 前后空白被裁掉：模型可能在词间补空格，空格的去留不该影响增量判定。
		{"  你好世界  ", "", "你好世界"},
	}
	for i, s := range steps {
		got := d.Next(s.full)
		if got != s.want {
			t.Fatalf("第 %d 步 Next(%q) = %q，期望 %q", i, s.full, got, s.want)
		}
		if d.Emitted() != s.byNow {
			t.Fatalf("第 %d 步已发出 %q，期望 %q", i, d.Emitted(), s.byNow)
		}
	}
	if d.Revisions() != 0 {
		t.Fatalf("单调增长路径不该记改写，实际 %d 次", d.Revisions())
	}
}

// TestStreamingDeltaRevision 覆盖**改写路径**：模型改动了已发出的字。
//
// 接口没有撤回能力，约定「保底多字、绝不丢字」：
// 取最长公共前缀，把差异部分当新增返回，并记一次改写。
// 这里用中文（每字 3 字节）专门守 rune 边界：按字节截会切出半个字。
func TestStreamingDeltaRevision(t *testing.T) {
	var d StreamingDelta
	if got := d.Next("今天我气很好"); got != "今天我气很好" {
		t.Fatalf("首句 = %q", got)
	}
	// 模型把「气」改成「天」（第 3 个字），全量变成「今天天气很好」。
	got := d.Next("今天天气很好")
	// 最长公共前缀 = "今天"（6 字节），差异部分 = "天气很好"。
	if got != "天气很好" {
		t.Fatalf("改写增量 = %q，期望 %q", got, "天气很好")
	}
	if d.Revisions() != 1 {
		t.Fatalf("改写次数 = %d，期望 1", d.Revisions())
	}
	// 关键：返回的增量必须是合法 UTF-8（半个汉字会显示成 �）。
	if !isValidUTF8(got) {
		t.Fatalf("改写增量不是合法 UTF-8: %q", got)
	}
	// emitted 必须前进到新全量，否则下一次又会对不上前缀而重复吐字。
	if d.Emitted() != "今天天气很好" {
		t.Fatalf("改写后 emitted = %q", d.Emitted())
	}
	// 续接：在改写后的全量上继续增长，仍走正常路径。
	if got := d.Next("今天天气很好啊"); got != "啊" {
		t.Fatalf("改写后续接增量 = %q，期望 %q", got, "啊")
	}
	if d.Revisions() != 1 {
		t.Fatalf("续接不该再记改写，实际 %d 次", d.Revisions())
	}
}

// TestStreamingDeltaRevisionShrink 覆盖更棘手的一种改写：新全量比旧的**短**。
//
// 例：模型先给出「你今天好吗」，随后收敛成「你」+ 新内容「你好吗」（去掉了「今天」）。
// 公共前缀 = "你"，差异部分 = "你好吗" —— 同样满足「多字不丢字」。
func TestStreamingDeltaRevisionShrink(t *testing.T) {
	var d StreamingDelta
	_ = d.Next("你今天好吗")
	got := d.Next("你好吗今天")
	if got != "你好吗今天"[len("你"):] {
		t.Fatalf("增量 = %q", got)
	}
	if d.Revisions() != 1 {
		t.Fatalf("改写次数 = %d，期望 1", d.Revisions())
	}
	if !isValidUTF8(got) {
		t.Fatalf("增量不是合法 UTF-8: %q", got)
	}
}

// TestStreamingDeltaReset 断言 Reset 之后不重复吐旧文本（每会话一次）。
//
// 这条对应的真实需求：`LiveRunner` 在会话开始时调 `Reset`，
// 若 Reset 没清干净已发出前缀，第二个会话的第一句就会莫名其妙带上一整段旧文本。
func TestStreamingDeltaReset(t *testing.T) {
	var d StreamingDelta
	_ = d.Next("第一句")
	_ = d.Next("第一句话")
	d.Reset()
	if d.Emitted() != "" || d.Revisions() != 0 {
		t.Fatalf("Reset 后状态未清: emitted=%q revisions=%d", d.Emitted(), d.Revisions())
	}
	// 新会话的首句必须是完整的新文本，不是「与旧文本的差集」。
	if got := d.Next("第二句"); got != "第二句" {
		t.Fatalf("Reset 后首句 = %q，期望 %q", got, "第二句")
	}
}

// TestCommonPrefixBytes 直接钉 rune 边界（中文 / 日文 / ASCII 混排）。
func TestCommonPrefixBytes(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"", "abc", 0},
		{"abc", "", 0},
		{"abc", "abd", 2},
		{"你好", "你好世界", len("你好")},
		{"你好", "你坏", len("你")},
		// 首字节相同、第二个字节完全不同：只能是 0，绝不能返回 1（那会切出半个字）。
		{"你", "他", 0},
		{"こんにちは", "こんばんは", len("こん")},
		{"嗯", "嗯", len("嗯")},
	}
	for _, c := range cases {
		if got := CommonPrefixBytes(c.a, c.b); got != c.want {
			t.Fatalf("CommonPrefixBytes(%q, %q) = %d，期望 %d", c.a, c.b, got, c.want)
		}
		// 计算结果必须落在 rune 起点：切出来的两边都是合法 UTF-8。
		n := CommonPrefixBytes(c.a, c.b)
		if !isValidUTF8(c.a[:n]) || !isValidUTF8(c.a[n:]) {
			t.Fatalf("公共前缀长度 %d 切在非 rune 边界上: %q", n, c.a)
		}
	}
}

// isValidUTF8 判合法 UTF-8（不经 strings 包，避免引入无关依赖）。
func isValidUTF8(s string) bool {
	return strings.ToValidUTF8(s, "\uFFFD") == s
}
