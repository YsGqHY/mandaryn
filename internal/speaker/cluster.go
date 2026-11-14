package speaker

import (
	"math"
	"sort"
)

// 会话内聚类（FR-06-2，ARCH §4.4「片段级 embedding 做层次聚类」）。
//
// 算法取舍（为什么不是 DBSCAN / 谱聚类）：
//   - 输入规模很小（一场会话通常几十到几百个片段），而**确定性**比精度更重要：
//     同一批向量必须每次得到同一份编号，否则用户每次重跑看到不同说话人编号，
//     手工命名过的角色会被打乱；
//   - 这里用「按时间顺序的单遍增量聚类 + 余弦阈值 + 质心更新」：O(n·k·d)
//     （n 片段数、k 说话人数、d 维度），比层次聚类的 O(n³) 便宜得多，
//     且不会因为「先合并哪一对」的顺序差异产生不同结果；
//   - 增量法的已知弱点是把「第一句噪声」固化成簇心，因此阈值取 0.72
//     （ARCH §4.4 的全局归并阈值）而不是更松的 0.6；过短片段在 pipeline 里
//     已被 1.2s 门槛滤掉，噪声主要来源已被移除。
const (
	// DefaultClusterThreshold 会话内「同一人」的余弦相似度阈值。
	//
	// 语义是**闭区间**：`sim >= threshold` 即判为同一人（见 ThresholdNote）。
	// 取值依据：同一声纹模型下同一人的片段级相似度通常 0.75~0.95，
	// 不同人在 0.2~0.5；0.72 落在两者之间的空档，且与全局归并保持一致。
	DefaultClusterThreshold = 0.72

	// ThresholdNote 是阈值边界语义的唯一说明处（单测 `TestThresholdBoundary`
	// 锁死它，改这里必须同时改测试）。
	//
	// 选**闭区间**的理由：跨会话归并命中要求「足够像」，把恰好等于阈值判为命中，
	// 才能让 `ClusterVectors` 与 `MatchGlobal` 在边界上表现一致；
	// 若一个开一个闭，同一对向量在会话内算两个人、跨会话又归并成一个人。
	ThresholdNote = "sim >= threshold 判为同一人（闭区间）"
)

// SegmentVector 是一个片段的声纹输入。
//
// 时间字段来自 `segments.start_ms/end_ms`（帧计数换算，ARCH §4.5.1），
// 只用于统计说话人时长，不参与相似度计算。
type SegmentVector struct {
	SegmentID int64
	// Seq 片段序号：聚类内部按它排序，保证「编号按首次出现顺序分配」。
	Seq     int
	StartMs int64
	EndMs   int64
	// Vector 声纹向量（未归一化也可以，内部会归一化）。
	Vector []float32
}

// ClusterOptions 聚类参数。
type ClusterOptions struct {
	// Threshold 余弦相似度阈值；<=0 用 DefaultClusterThreshold。
	Threshold float64
	// MaxSpeakers 会话内说话人数上限（`app_settings.max_speakers`，0 = 不限制）。
	// 达到上限后不再新建簇，新片段并入最相似的既有簇。
	MaxSpeakers int
}

// Cluster 是一个说话人簇。
type Cluster struct {
	// Index 簇编号（0 起，按**首次出现顺序**分配：最先出现的那一簇是 0）。
	Index int
	// Members 片段 ID，按 (Seq, SegmentID) 升序。
	Members []int64
	// SpeechMs 该簇累计语音时长（片段区间之和）。
	SpeechMs int64
	// Centroid 质心（已 L2 归一化），用于跨会话归并。
	Centroid []float32
}

// Partition 是聚类结果。
type Partition struct {
	// Clusters 按 Index 升序。
	Clusters []Cluster
	// LabelOf 片段 ID → 簇编号。
	LabelOf map[int64]int
}

// ClusterVectors 把片段级声纹聚成会话内说话人划分。
//
// 确定性保证（单测锁死，见 cluster_test.go）：
//   - 输入先按 `(Seq, SegmentID)` 稳定排序，因此**打乱输入顺序不改变结果**；
//   - 簇编号按首次出现顺序分配，不依赖任何 map 迭代顺序；
//   - 并列最优时取**编号最小**的簇（`sim > best` 才替换），不留浮点抖动空间。
//
// 无向量（nil / 空）的片段被忽略：它们不该参与聚类，调用方应把它们归「未识别」。
func ClusterVectors(in []SegmentVector, opts ClusterOptions) Partition {
	threshold := opts.Threshold
	if threshold <= 0 {
		threshold = DefaultClusterThreshold
	}

	items := make([]SegmentVector, 0, len(in))
	for _, s := range in {
		if len(s.Vector) == 0 {
			continue
		}
		items = append(items, s)
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Seq != items[j].Seq {
			return items[i].Seq < items[j].Seq
		}
		return items[i].SegmentID < items[j].SegmentID
	})

	labelOf := make(map[int64]int, len(items))
	states := make([]*clusterState, 0, len(items))

	for _, s := range items {
		v := Normalize(s.Vector)

		bestIdx, bestSim := -1, math.Inf(-1)
		for i, c := range states {
			sim := cosineToSum(v, c.sum)
			// 严格大于才替换：并列时保留编号更小的簇，结果可复现。
			if sim > bestSim {
				bestIdx, bestSim = i, sim
			}
		}

		atCap := opts.MaxSpeakers > 0 && len(states) >= opts.MaxSpeakers
		switch {
		case bestIdx >= 0 && (bestSim >= threshold || atCap):
			// 命中既有簇；atCap 时即使是「最相似的陌生人」也强制并入，
			// 这是 FR-06-6「上限 2–8」的落地：宁可少分一个说话人，
			// 也不能突破用户设定的上限（且绝不 panic）。
			c := states[bestIdx]
			c.ids = append(c.ids, s.SegmentID)
			for i := range c.sum {
				c.sum[i] += float64(v[i])
			}
			c.speechMs += span(s)
			labelOf[s.SegmentID] = bestIdx
		default:
			sum := make([]float64, len(v))
			for i := range v {
				sum[i] = float64(v[i])
			}
			states = append(states, &clusterState{
				ids:      []int64{s.SegmentID},
				sum:      sum,
				speechMs: span(s),
			})
			labelOf[s.SegmentID] = len(states) - 1
		}
	}

	out := Partition{
		Clusters: make([]Cluster, 0, len(states)),
		LabelOf:  labelOf,
	}
	for i, c := range states {
		out.Clusters = append(out.Clusters, Cluster{
			Index:    i,
			Members:  append([]int64(nil), c.ids...),
			SpeechMs: c.speechMs,
			Centroid: centroid(c.sum, len(c.ids)),
		})
	}
	return out
}

// clusterState 是聚类过程中的簇状态。
//
// 用 float64 累加和而不是 float32 质心：片段几十条时 float32 的累积误差足以让
// 相似度在阈值附近抖动，从而让「同一次输入跑两次结果不同」。
type clusterState struct {
	ids      []int64
	sum      []float64
	speechMs int64
}

// cosineToSum 计算归一化向量与「未归一化累加和」之间的余弦相似度。
//
// 质心 c = sum/n，故 cos(v, c) = (v·sum/n) / (|v| · |sum|/n)，其中 |v| = 1
// （调用方已归一化），n 约掉后就是 (v·sum) / |sum| —— 省掉一次开方与除法。
// 维度不一致返回 -1（不可比）。
func cosineToSum(v []float32, sum []float64) float64 {
	if len(sum) != len(v) || len(v) == 0 {
		return -1
	}
	var dot, ns float64
	for i, f := range v {
		x := float64(f)
		dot += x * sum[i]
		ns += sum[i] * sum[i]
	}
	if ns <= 0 {
		return -1
	}
	return dot / math.Sqrt(ns)
}

// centroid 由累加和还原归一化质心。
func centroid(sum []float64, count int) []float32 {
	if count <= 0 || len(sum) == 0 {
		return nil
	}
	mean := make([]float32, len(sum))
	for i, s := range sum {
		mean[i] = float32(s / float64(count))
	}
	return Normalize(mean)
}

// span 返回片段时长（毫秒）；异常数据（end < start）归 0，不让负数污染统计。
func span(s SegmentVector) int64 {
	d := s.EndMs - s.StartMs
	if d < 0 {
		return 0
	}
	return d
}
