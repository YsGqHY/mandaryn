//go:build nollm || !windows || !cgo

// Package sherpa 是声纹提取器的 sherpa-onnx 实现。
//
// 本文件是**非 Windows / 非 CGO / `-tags nollm`** 三种构建下的同名实现，保证：
//   - `GOOS=darwin go build ./internal/speaker/...` 与 `GOOS=linux ...` 能过；
//   - `-tags nollm` 能裁剪掉整个声纹提取（不链接 sherpa-onnx / onnxruntime）；
//   - 运行时行为统一是「本地声纹提取不可用」，而不是崩溃或静默降级到别的东西。
//
// 铁律（ADR-01）：不为了让 CI 变绿而把本地推理改成桩；这里只是「该构建没有
// 编译进 sherpa-onnx」，上层据此提示用户安装模型 / 换平台 / 换标准包。
// `nollm` 同样**不是** CGO 兜底，见 docs/ARCHITECTURE.md §2.2。
package sherpa

import (
	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/speaker"
)

// NewSherpaExtractor 在本构建里恒失败：返回 ERR_LOCAL_UNAVAILABLE。
func NewSherpaExtractor(_ string) (speaker.Extractor, error) {
	return nil, errUnavailable()
}

// errUnavailable 构造「本构建不含本地声纹提取」错误。
func errUnavailable() *apierr.Err {
	return apierr.New(apierr.CodeLocalUnavailable).
		WithAction("download-model").
		WithDetail("本构建不含本地声纹提取能力（需 Windows + CGO 且非 nollm 精简产物）")
}
