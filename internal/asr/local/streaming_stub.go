//go:build nollm || !windows || !cgo

// 流式引擎在**非 Windows / 非 CGO** 构建下的同名实现。
//
// 与 local_stub.go 同一套逻辑，只是把「本地引擎」换成「本地**流式**引擎」：
// 保证 `GOOS=darwin go build ./internal/asr/...` 与 `GOOS=linux ...` 能过，
// 且运行时行为是「流式不可用」（ERR_LOCAL_UNAVAILABLE），而不是崩溃或静默降级。
//
// 铁律（ADR-01）：不为了让 CI 变绿而把流式推理改成桩；这里只是「该平台没有
// 编译进 sherpa-onnx」。上层据此在 UI 上把「边录边转」标成不可用。
package local

import (
	"vip.oberon.mandaryn/internal/asr"
)

// NewStreaming 在本构建里恒失败：返回 ERR_LOCAL_UNAVAILABLE。
func NewStreaming(spec Spec) (asr.StreamingEngine, error) {
	return nil, errUnavailable()
}

// NewStreamingFactoryWithThreads 与 Windows 实现**签名一致**，同样恒失败。
//
// 桩也读一次 threads：让「工厂每次被调用读一次档位」这条契约在两个构建里形状一致，
// 跨平台单测才能验证它（与 NewFactoryWithThreads 的理由相同）。
func NewStreamingFactoryWithThreads(loc asr.BundleLocator, threads func() int) asr.StreamingFactory {
	return func() (asr.StreamingEngine, error) {
		_ = asr.LoadThreads(threads)
		return nil, errUnavailable()
	}
}
