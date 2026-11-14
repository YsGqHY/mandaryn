package speaker

import (
	"math"
	"sort"
)

// 跨会话全局归并（FR-06-3，ARCH §4.4「全局归并」）。
//
// 为什么需要它：会话内聚类的编号只在本场会话内有意义（会话 A 的 S1 与会话 B 的 S1
// 毫无关系）。角色是**全局**标签，所以每个聚类质心必须先去 `speakers` 表里找
// 「是不是以前见过这位」，命中就复用既有声纹身份（从而复用既有角色），
// 未命中才新建 —— 否则用户每场会话都要重新命名一遍。
const (
	// DefaultMergeThreshold 跨会话归并的余弦相似度阈值（ARCH §4.4 取 0.72）。
	// 边界语义与聚类一致：**闭区间**，`sim >= threshold` 即命中（见 ThresholdNote）。
	DefaultMergeThreshold = 0.72
)

// GlobalIdentity 是全局声纹条目的内存投影（TBL-05 `speakers`）。
//
// 向量单独放这里而不是直接读 storage.Speaker：TBL-05 存的是**密文**，
// 解密是 L2 的事（且解密失败要能降级成「无向量」而不是崩），
// 装配层 / L3 不必知道加解密细节。
type GlobalIdentity struct {
	SpeakerID string
	// Vector 质心（已 L2 归一化）；nil = 该条目没有可比向量（如手工拆分出来的）。
	Vector []float32
	// ModelID 产出该向量的声纹模型；跨模型不比较（DOC-04 §4.3）。
	ModelID string
	// Weight 参与质心的片段数（>=1）。库表没有这一列，从库重建时按 1 处理
	// （质心已是历史平均，继续按「一条样本」参与平均即可）。
	Weight int
	// SessionCount 命中会话数（TBL-05 冗余计数）。
	SessionCount int
	// TotalSpeechMs 累计语音时长。
	TotalSpeechMs int64
	// FirstSeenAt / LastSeenAt 毫秒时间戳。
	FirstSeenAt int64
	LastSeenAt  int64
}

// SortIdentities 按 SpeakerID 升序排（原地）。
//
// 匹配必须**顺序确定**：`MatchGlobal` 逐条比较、并列时保留先遇到的，
// 因此候选列表的顺序决定了结果。从数据库加载后一定要先过一遍本函数，
// 否则 SQLite 的行序变化会让「同一段音频换台机器归并到不同角色」。
func SortIdentities(entries []GlobalIdentity) {
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].SpeakerID < entries[j].SpeakerID })
}

// MatchGlobal 在既有全局条目里找与 v 最相似的一个。
//
// 返回 (下标, 相似度)；没有可比候选时返回 (-1, -1)。
// 只与**同一模型、同维度、有向量**的条目比较：换模型后旧向量不可比，
// 硬比会把两个不同的人并成一个（比「多一个说话人」严重得多）。
//
// 阈值判断留给调用方（`sim >= threshold`），这样调用方能在日志里同时记录
// 「最相似度」与被拒原因，而不用算两遍。
func MatchGlobal(entries []GlobalIdentity, v []float32, modelID string) (int, float64) {
	if len(v) == 0 {
		return -1, -1
	}
	bestIdx, bestSim := -1, math.Inf(-1)
	for i := range entries {
		e := &entries[i]
		if len(e.Vector) != len(v) {
			continue
		}
		if modelID != "" && e.ModelID != "" && e.ModelID != modelID {
			continue
		}
		sim := Cosine(v, e.Vector)
		if sim > bestSim {
			bestIdx, bestSim = i, sim
		}
	}
	if bestIdx < 0 {
		return -1, -1
	}
	return bestIdx, bestSim
}

// MergeCentroid 把一条新向量按权重融入既有条目的质心（原地更新）。
//
// 加权平均而不是「覆盖成最新」：覆盖会让归并结果依赖处理顺序，
// 且一次噪声片段就能把整位说话人的声纹带偏。
// 新向量若维度不同或为空则**不改质心**（返回 false），由调用方决定是否新建条目。
func MergeCentroid(e *GlobalIdentity, v []float32, weight int) bool {
	if e == nil || len(v) == 0 {
		return false
	}
	if len(e.Vector) == 0 {
		e.Vector = Normalize(v)
		if weight > 0 {
			e.Weight = weight
		}
		if e.Weight <= 0 {
			e.Weight = 1
		}
		return true
	}
	if len(e.Vector) != len(v) {
		return false
	}
	if weight <= 0 {
		weight = 1
	}
	oldWeight := e.Weight
	if oldWeight <= 0 {
		oldWeight = 1
	}
	total := oldWeight + weight
	merged := make([]float32, len(v))
	for i := range v {
		merged[i] = float32((float64(e.Vector[i])*float64(oldWeight) +
			float64(v[i])*float64(weight)) / float64(total))
	}
	e.Vector = Normalize(merged)
	e.Weight = total
	return true
}

// BlendCentroids 融合两个条目的质心（手动「合并说话人」时用）。
//
// 语义与 MergeCentroid 一致（按各自 Weight 加权），只是两边都是质心而非单条向量。
// 任一侧没有向量时保留另一侧（不把有向量的那侧抹掉）。
func BlendCentroids(dst, src *GlobalIdentity) {
	if dst == nil || src == nil {
		return
	}
	if len(src.Vector) == 0 || len(dst.Vector) == 0 || len(src.Vector) != len(dst.Vector) {
		if len(dst.Vector) == 0 {
			dst.Vector = Normalize(src.Vector)
			if dst.Weight <= 0 {
				dst.Weight = src.Weight
			}
			return
		}
		return
	}
	dw, sw := dst.Weight, src.Weight
	if dw <= 0 {
		dw = 1
	}
	if sw <= 0 {
		sw = 1
	}
	total := dw + sw
	merged := make([]float32, len(dst.Vector))
	for i := range dst.Vector {
		merged[i] = float32((float64(dst.Vector[i])*float64(dw) +
			float64(src.Vector[i])*float64(sw)) / float64(total))
	}
	dst.Vector = Normalize(merged)
	dst.Weight = total
	dst.SessionCount += src.SessionCount
	dst.TotalSpeechMs += src.TotalSpeechMs
	if src.FirstSeenAt > 0 && (dst.FirstSeenAt == 0 || src.FirstSeenAt < dst.FirstSeenAt) {
		dst.FirstSeenAt = src.FirstSeenAt
	}
	if src.LastSeenAt > dst.LastSeenAt {
		dst.LastSeenAt = src.LastSeenAt
	}
}
