// Package speaker 是「说话人分离与角色命名」的 L2 领域包（FR-06，ARCH §4.4）。
//
// 职责边界：
//   - 声纹提取抽象（Extractor）与会话内聚类（cluster.go）；
//   - 跨会话全局归并（merge.go）；
//   - 角色管理：列表 / 改名 / 合并 / 拆分 / 指派（roles.go）；
//   - 「对某个会话跑说话人分离」的编排（pipeline.go）。
//
// 分层理由（CLAUDE.md「领域逻辑不进 service」）：
//   - 聚类、归并、幂等重跑、角色映射的语义全部在本包；`internal/services/speakersvc`
//     只做参数校验、错误码映射与 DTO 转换，它不拼一行 SQL、不算一次相似度；
//   - 本包**不 import Wails**，也不 import `internal/models`（模型目录经
//     `BundleLocator` 接口注入，避免模型域与本域互相依赖）。因此可被单测直接驱动
//     （临时 SQLite + fake Extractor 即可，全程离线）。
//
// 铁律（违反即缺陷）：
//  1. **角色名不冗余落 `segments`**：改名全局同步靠 `roles` 表 JOIN，片段只引用
//     `speaker_id`。本包任何写入路径都不允许把名字写进片段（FR-06-3）。
//  2. **声纹向量永不进日志、永不进错误 Detail、永不进导出**：日志只写维度、
//     模型 ID、片段数、错误码（NFR-03-5）。
//  3. **无模型时诚实降级**：提取器为 nil → 返回错误码，绝不 panic、绝不假装成功、
//     绝不自动下载；纯 DB 操作（改名 / 合并 / 拆分 / 指派）必须仍然可用。
//  4. **时间基用帧计数**：片段区间一律来自 `segments.start_ms/end_ms`
//     （`frameCount/16000*1000` 换算），不使用 wall clock（ARCH §4.5.1）。
package speaker

import (
	"encoding/base64"
	"encoding/binary"
	"math"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/utils/cryptox"
)

// RoleSpeaker 是声纹模型包在模型注册表里的角色值（DOC-04 TBL-08 `roles` 数组）。
//
// 与 asr.RoleOffline / asr.RoleStreaming 同一命名体系，但本包刻意不 import asr：
// 声纹模型与转写模型是两条独立的模型链（一处换了不该影响另一处）。
const RoleSpeaker = "speaker"

// SampleRate 是本域的内部唯一采样率（ARCH §4.3：内部统一 16kHz / 16bit / 单声道）。
//
// 声纹模型按 16k 训练，喂 8k 或 44.1k 会得到无意义向量，因此提取入口只接受 16k。
const SampleRate = 16000

// Extractor 是声纹提取抽象。
//
// 用接口而不是直接调 sherpa-onnx：
//   - L2 保持纯 Go，单测可用 fake 驱动（不需要 CGO / 模型文件 / DLL）；
//   - 真正的 sherpa 实现在子包 `internal/speaker/sherpa`（Windows + CGO），
//     原因见 docs/DEV-ISSUES-S7.md S7-01（CGO 会把整个测试二进制绑上 DLL）。
type Extractor interface {
	// Embed 返回一条声纹向量（长度 = 模型维度，通常 192/256）。
	// 样本过短 / 静音时返回错误，调用方应把该片段归「未识别」而不是编造向量。
	Embed(samples []float32, sampleRate int) ([]float32, error)
	// ID 返回模型标识（`bundleId@version` 或 `speaker:<file>`），落 `speakers.model_id`。
	// 跨模型不比较相似度（DOC-04 §4.3）。
	ID() string
	// Close 释放资源；幂等。
	Close() error
}

// BundleLocator 把「角色」解析成模型包目录，由装配层注入（实现方是 models 域）。
//
// 与 asr.BundleLocator 刻意分开：本包不 import asr，也不 import models。
type BundleLocator interface {
	// LocateBundle 返回该角色对应模型包的目录与 bundleId。
	// 目录不存在 → ERR_MODEL_NOT_FOUND；校验失败 → ERR_MODEL_CORRUPT。
	LocateBundle(role string) (dir string, bundleID string, err error)
}

// ExtractorFactory 由装配层注入：把模型包目录变成提取器。
//
// Windows + CGO 下是 `sherpa.NewSherpaExtractor`，其它平台是返回
// ERR_LOCAL_UNAVAILABLE 的同名实现 —— 保证 `GOOS=darwin` / `GOOS=linux` 能编译。
type ExtractorFactory func(bundleDir string) (Extractor, error)

// LocateAndBuild 用定位器 + 工厂拼出提取器（给装配层省一次样板代码）。
//
// loc 或 factory 为 nil 时返回 ERR_LOCAL_UNAVAILABLE：这是「本构建不含该能力」，
// 而不是「用户还没下载模型」（后者由 locator 自己返回 ERR_MODEL_NOT_FOUND）。
func LocateAndBuild(loc BundleLocator, factory ExtractorFactory) (Extractor, error) {
	if loc == nil {
		return nil, errExtractorUnavailable("未装配模型包定位器")
	}
	if factory == nil {
		return nil, errExtractorUnavailable("本构建不含本地声纹提取能力")
	}
	dir, _, err := loc.LocateBundle(RoleSpeaker)
	if err != nil {
		return nil, err
	}
	return factory(dir)
}

// errExtractorUnavailable 构造「声纹功能不可用」错误。
//
// 用 ERR_LOCAL_UNAVAILABLE（DOC-03 §5 既有码）：语义是「本构建 / 本机没有这个能力」，
// 可重试=false，动作键改成 download-model（默认的 switch-online 对本地能力是错误引导）。
func errExtractorUnavailable(detail string) *apierr.Err {
	return apierr.New(apierr.CodeLocalUnavailable).
		WithAction("download-model").
		WithDetail("%s", detail)
}

// —— 向量的持久化编码 ——

// EncodeVector 把声纹向量编码成可落库的密文字符串（TBL-05 `embedding_enc`）。
//
// 两层编码是刻意的：
//  1. float32 → little-endian 字节 → base64：cryptox 只接受 string，
//     而声纹是二进制，必须先转成无歧义的文本（base64 而不是 string(bytes)，
//     后者含任意字节、经过 SQLite / JSON 中转容易被改写）；
//  2. base64 文本 → cryptox.EncryptString：**密文落库**是 TBL-05 的硬要求。
//
// 明文向量绝不落库、绝不进日志（NFR-03-5）。
func EncodeVector(v []float32) (string, error) {
	if len(v) == 0 {
		return "", apierr.New(apierr.CodeDBWrite).WithDetail("声纹向量为空，拒绝落库")
	}
	raw := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(raw[i*4:], math.Float32bits(f))
	}
	enc, err := cryptox.EncryptString(base64.StdEncoding.EncodeToString(raw))
	if err != nil {
		// 只写错误类别，不带向量内容。
		return "", apierr.Wrap(err, apierr.CodeDBWrite)
	}
	return enc, nil
}

// DecodeVector 把 `embedding_enc` 解回向量；dim 用来校验长度（0 表示不校验）。
//
// 解不出来（换机器后主密钥变了 / 数据被截断）时返回 ERR_MODEL_CORRUPT 语义之外的空
// 向量 + 错误，由调用方决定「跳过这个说话人」而不是让整次归并失败。
func DecodeVector(enc string, dim int) ([]float32, error) {
	if enc == "" {
		// 没有向量是合法状态（手工拆分出的说话人没有向量，见 S7-05）。
		return nil, nil
	}
	plain, err := cryptox.DecryptString(enc)
	if err != nil {
		return nil, apierr.Wrap(err, apierr.CodeDBWrite)
	}
	raw, err := base64.StdEncoding.DecodeString(plain)
	if err != nil {
		return nil, apierr.Wrap(err, apierr.CodeDBWrite)
	}
	if len(raw)%4 != 0 {
		return nil, apierr.New(apierr.CodeDBWrite).
			WithDetail("声纹向量长度非法：%d 字节", len(raw))
	}
	out := make([]float32, len(raw)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[i*4:]))
	}
	if dim > 0 && len(out) != dim {
		return nil, apierr.New(apierr.CodeDBWrite).
			WithDetail("声纹向量维度不符：库中 dim=%d，实际 %d", dim, len(out))
	}
	return out, nil
}

// Normalize 对向量做 L2 归一化（原地返回新切片）。
//
// 余弦相似度本身对尺度不敏感，但归一化后：
//   - 质心平均不会因为某条向量模长异常而主导结果；
//   - 相似度计算可以退化成一次点积，聚类内层循环少两次乘加。
func Normalize(v []float32) []float32 {
	if len(v) == 0 {
		return nil
	}
	var sum float64
	for _, f := range v {
		sum += float64(f) * float64(f)
	}
	if sum <= 0 {
		// 全零向量：保持原样（调用方应视为无效输入），避免产生 NaN。
		out := make([]float32, len(v))
		copy(out, v)
		return out
	}
	norm := math.Sqrt(sum)
	out := make([]float32, len(v))
	for i, f := range v {
		out[i] = float32(float64(f) / norm)
	}
	return out
}

// Cosine 计算两个向量的余弦相似度，范围 [-1, 1]。
//
// 维度不一致 / 任一为空 / 任一全零 → 返回 -1（最不相似）而不是 0：
// 0 恰好等于「正交」，用 -1 才能让「不可比」在阈值判断里稳定落选。
func Cosine(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return -1
	}
	var dot, na, nb float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		na += x * x
		nb += y * y
	}
	if na <= 0 || nb <= 0 {
		return -1
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
