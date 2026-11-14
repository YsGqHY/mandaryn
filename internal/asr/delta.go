package asr

import (
	"strings"
	"unicode/utf8"
)

// StreamingDelta 把「全量文本」换算成 `StreamingEngine.Accept` 需要的「增量文本」。
//
// # 为什么需要它
//
// sherpa-onnx 的 `OnlineRecognizer.GetResult` 返回的是「自上次 Reset 以来的**全量**
// 文本」（说话人一边说，它一边把整句越写越长），而 `Accept` 的契约是「本次**新增**」。
// 两者差一个已发出前缀，所以要有个地方把前缀记住并裁掉。
//
// 这段逻辑放在 `internal/asr`（纯 Go）而不是 CGO 实现包里，有两个理由：
//   - 它是**引擎无关**的知识：任何「给累计文本的流式后端」（包括将来的远程流式端点）
//     都需要同一套裁剪，不该和 sherpa 绑在一起；
//   - 它可以被无依赖地单测 —— 前缀裁剪的边界（中文多字节、模型改写、空串）
//     正是最容易写错又最难在真机上撞到的地方，必须用表驱动用例守住。
//
// # 单调增长假设
//
// transducer + greedy_search 的输出在实际中**单调增长**：已提交的 token 不会撤回。
// 于是「全量 == 已发出前缀 + 新字」成立，`Next` 直接给尾巴。
//
// # 模型改写已发出的字怎么办
//
// 接口没有「撤回」能力（上层 `LiveRunner` 只会把增量往后拼），所以改写只能退化为
// 「多吐几个字」：取两者的最长公共前缀，把差异部分当新增返回。结果是定稿里会留下
// 被改写前的旧字（**多字，但不会丢字**），同时 `Revisions()` 计数 +1。
// 计数器只记次数、绝不记正文，用途是让真机测试能量化「这个假设成立到什么程度」。
//
// 用法（非并发安全，由调用方持锁）：
//
//	var d asr.StreamingDelta
//	delta := d.Next(full)   // 只拿到新增
//	d.Reset()               // 新会话 / 换了 stream
type StreamingDelta struct {
	// emitted 已经返回给调用方的完整前缀。
	emitted string
	// revisions 发生「改写」的次数。
	revisions int64
}

// Next 输入当前全量文本，返回本次新增（可能为空串）。
func (d *StreamingDelta) Next(full string) string {
	full = strings.TrimSpace(full)
	if full == "" {
		// 空结果：不解构已发出前缀。流式模型偶尔会先给空文本再给内容，
		// 若在这里清 emitted，下一次就会把整句重复吐一遍。
		return ""
	}
	if d.emitted == "" {
		d.emitted = full
		return full
	}
	if strings.HasPrefix(full, d.emitted) {
		// 正常路径：单调增长，直接给尾巴。
		delta := full[len(d.emitted):]
		d.emitted = full
		return delta
	}

	// 改写路径：模型改动了已发出的字。
	d.revisions++
	delta := full[CommonPrefixBytes(d.emitted, full):]
	// emitted 前进到 full：后续仍按「全量 = 前缀 + 新增」裁剪，
	// 否则只要前缀对不上，每次都会重复吐字。
	d.emitted = full
	return delta
}

// Reset 清空状态（开始新会话时调用）。
func (d *StreamingDelta) Reset() {
	d.emitted = ""
	d.revisions = 0
}

// Revisions 已发生的改写次数（诊断用）。
func (d *StreamingDelta) Revisions() int64 { return d.revisions }

// Emitted 已发出的完整文本（诊断 / 测试用；**不要**拿它当结果落库）。
func (d *StreamingDelta) Emitted() string { return d.emitted }

// CommonPrefixBytes 返回两个字符串最长公共前缀的**字节**长度（只在 rune 边界上取值）。
//
// 必须按 rune 走：中文一个字 3 字节，按字节比会截在半个字上，
// 切出非法 UTF-8，前端显示成「�」。返回值仍是字节下标（可直接切片），
// 只是保证它落在 rune 起点。
func CommonPrefixBytes(a, b string) int {
	i := 0
	for i < len(a) && i < len(b) {
		ra, wa := utf8.DecodeRuneInString(a[i:])
		rb, wb := utf8.DecodeRuneInString(b[i:])
		if ra != rb || wa != wb {
			return i
		}
		i += wa
	}
	return i
}
