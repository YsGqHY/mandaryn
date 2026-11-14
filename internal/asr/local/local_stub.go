//go:build nollm || !windows || !cgo

// Package local 是 sherpa-onnx 的本地引擎实现。
//
// 本文件是**非 Windows / 非 CGO / `-tags nollm`** 三种构建下的同名实现，保证：
//   - `GOOS=darwin go build ./internal/asr/...` 与 `GOOS=linux ...` 能过；
//   - `-tags nollm` 能裁剪掉整个本地推理（不链接 sherpa-onnx / onnxruntime）；
//   - 运行时行为统一是「本地引擎不可用」，而不是崩溃或静默降级在线。
//
// 铁律（ADR-01）：不为了让 CI 变绿而把本地推理改成桩；这里只是「该构建没有
// 编译进 sherpa-onnx」，上层据此提示用户切换在线来源或换标准包。
// `nollm` 同样**不是** CGO 兜底，见 docs/ARCHITECTURE.md §2.2。
package local

import (
	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/asr"
)

// Spec 与 Windows 实现保持完全相同的字段，避免调用方写平台分支。
type Spec struct {
	// BundleID 模型包标识。
	BundleID string
	// Version 可选版本号。
	Version string
	// Dir 模型包目录。
	Dir string
	// Threads onnx 推理线程数。
	Threads int
	// Language sensevoice / whisper 的语种。
	Language string
}

// New 在本构建里恒失败：返回 ERR_LOCAL_UNAVAILABLE。
func New(spec Spec) (asr.Engine, error) {
	return nil, errUnavailable()
}

// NewFactory 返回一个恒失败的工厂，让 Router 的 SetSource(local) 得到明确的
// 错误码（而不是 nil 指针 panic）。
func NewFactory(loc asr.BundleLocator) asr.LocalFactory {
	return NewFactoryWithThreads(loc, nil)
}

// NewFactoryWithThreads 与 Windows 实现**签名一致**，同样恒失败。
//
// 为什么桩也要读一次 threads：让「工厂每次被调用读一次档位」这条契约在两个构建里
// 形状一致，于是跨平台单测（`//go:build !windows || !cgo`）能验证它。
// 读到的值必然用不上（本构建没有引擎），但 `perf.Service.Threads` 只是一次受锁快照读，
// 代价可忽略；换来的是「契约不会因为平台不同而漂移」。
func NewFactoryWithThreads(loc asr.BundleLocator, threads func() int) asr.LocalFactory {
	return func() (asr.Engine, error) {
		_ = asr.LoadThreads(threads)
		return nil, errUnavailable()
	}
}

func errUnavailable() *apierr.Err {
	return apierr.New(apierr.CodeLocalUnavailable).
		WithDetail("本构建不含本地引擎（需 Windows + CGO 且非 nollm 精简产物）")
}
