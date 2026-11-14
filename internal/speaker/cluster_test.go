package speaker

import (
	"fmt"
	"math/rand"
	"testing"
)

// sv 构造一个测试用片段向量。
func sv(segmentID int64, seq int, vec []float32) SegmentVector {
	start := int64(seq) * 3000
	return SegmentVector{
		SegmentID: segmentID,
		Seq:       seq,
		StartMs:   start,
		EndMs:     start + 2000,
		Vector:    vec,
	}
}

// TestClusterDeterministic 锁死「同一输入 → 同一编号」。
//
// 这条性质是产品级的：用户手工命名过角色之后重跑分离，如果编号漂移，
// 已有角色会挂到别人身上。因此跑 100 次 + 打乱输入顺序两种方式都要一致。
func TestClusterDeterministic(t *testing.T) {
	input := []SegmentVector{
		sv(1, 1, voiceA1),
		sv(2, 2, voiceB1),
		sv(3, 3, voiceA2),
		sv(4, 4, voiceB2),
		sv(5, 5, voiceA3),
	}

	want := ClusterVectors(input, ClusterOptions{})
	if len(want.Clusters) != 2 {
		t.Fatalf("期望 2 个簇，实际 %d", len(want.Clusters))
	}

	// 1) 同一输入重复 100 次，编号完全一致。
	for i := 0; i < 100; i++ {
		got := ClusterVectors(input, ClusterOptions{})
		if !sameLabels(want.LabelOf, got.LabelOf) {
			t.Fatalf("第 %d 次结果与首次不一致：want=%v got=%v", i+1, want.LabelOf, got.LabelOf)
		}
		if len(got.Clusters) != len(want.Clusters) {
			t.Fatalf("第 %d 次簇数不一致：want=%d got=%d", i+1, len(want.Clusters), len(got.Clusters))
		}
	}

	// 2) 打乱输入顺序 50 次，编号映射仍一致（内部按 seq 排序 + 按首次出现编号）。
	rng := rand.New(rand.NewSource(20260102))
	for i := 0; i < 50; i++ {
		shuffled := make([]SegmentVector, len(input))
		copy(shuffled, input)
		rng.Shuffle(len(shuffled), func(a, b int) {
			shuffled[a], shuffled[b] = shuffled[b], shuffled[a]
		})
		got := ClusterVectors(shuffled, ClusterOptions{})
		if !sameLabels(want.LabelOf, got.LabelOf) {
			t.Fatalf("第 %d 次打乱后编号漂移：want=%v got=%v", i+1, want.LabelOf, got.LabelOf)
		}
	}
}

// TestClusterNumberingByFirstAppearance 锁死「编号按首次出现顺序」：
// 最早出现的片段属于簇 0，即使它的向量不是第一维方向。
func TestClusterNumberingByFirstAppearance(t *testing.T) {
	// 第一句是 B 类语音 → B 应该拿到编号 0。
	input := []SegmentVector{
		sv(10, 1, voiceB1),
		sv(11, 2, voiceA1),
		sv(12, 3, voiceB2),
		sv(13, 4, voiceA2),
	}
	got := ClusterVectors(input, ClusterOptions{})
	if len(got.Clusters) != 2 {
		t.Fatalf("期望 2 个簇，实际 %d", len(got.Clusters))
	}
	if got.LabelOf[10] != 0 || got.LabelOf[12] != 0 {
		t.Fatalf("先出现的 B 类应拿到编号 0，实际 %d / %d", got.LabelOf[10], got.LabelOf[12])
	}
	if got.LabelOf[11] != 1 || got.LabelOf[13] != 1 {
		t.Fatalf("后出现的 A 类应拿到编号 1，实际 %d / %d", got.LabelOf[11], got.LabelOf[13])
	}
}

// TestClusterTwoGroups 聚类正确性：两簇明显分开（簇内 ≈0.99，跨簇 ≈0）→ 恰好 2 类且归属正确。
func TestClusterTwoGroups(t *testing.T) {
	input := []SegmentVector{
		sv(1, 1, voiceA1),
		sv(2, 2, voiceB1),
		sv(3, 3, voiceA2),
		sv(4, 4, voiceB2),
		sv(5, 5, voiceA3),
		sv(6, 6, voiceB3),
	}
	got := ClusterVectors(input, ClusterOptions{})
	if len(got.Clusters) != 2 {
		t.Fatalf("期望 2 个簇，实际 %d（%v）", len(got.Clusters), got.LabelOf)
	}

	// 逐条断言归属：A 类三条同簇、B 类三条同簇，且两簇不同。
	if got.LabelOf[1] != got.LabelOf[3] || got.LabelOf[3] != got.LabelOf[5] {
		t.Fatalf("A 类三条本应同簇：%v", got.LabelOf)
	}
	if got.LabelOf[2] != got.LabelOf[4] || got.LabelOf[4] != got.LabelOf[6] {
		t.Fatalf("B 类三条本应同簇：%v", got.LabelOf)
	}
	if got.LabelOf[1] == got.LabelOf[2] {
		t.Fatalf("A 与 B 本应不同簇：%v", got.LabelOf)
	}

	// 簇内相似度的确远高于阈值，跨簇远低于阈值（证明样本构造符合测试意图）。
	if sim := Cosine(voiceA1, voiceA2); sim < 0.95 {
		t.Fatalf("A 类内部相似度应 > 0.95，实际 %.4f", sim)
	}
	if sim := Cosine(voiceA1, voiceB1); sim > 0.1 {
		t.Fatalf("跨簇相似度应 < 0.1，实际 %.4f", sim)
	}

	// 簇的累计时长应等于成员片段时长之和（每段 2s）。
	total := int64(0)
	for _, c := range got.Clusters {
		if c.SpeechMs != 6000 {
			t.Fatalf("簇 %d 时长应为 6000ms，实际 %d", c.Index, c.SpeechMs)
		}
		total += c.SpeechMs
	}
	if total != 12000 {
		t.Fatalf("两簇总时长应为 12000ms，实际 %d", total)
	}
}

// TestThresholdBoundary 锁死阈值边界语义（闭区间：`sim >= threshold` 判为同一人）。
//
// 构造方式：先算出两条向量的真实相似度，再把它**正好**当阈值用。
// 这样不依赖 cos 的十进制取值是否可表示，浮点边界也不会因平台差异抖动。
func TestThresholdBoundary(t *testing.T) {
	a := vec4(1, 0, 0, 0)
	b := vec4(0.8, 0.6, 0, 0) // 与 a 的余弦 = 0.8
	sim := Cosine(a, b)
	if sim <= 0 {
		t.Fatalf("样本构造错误：相似度应为正，实际 %v", sim)
	}

	input := []SegmentVector{sv(1, 1, a), sv(2, 2, b)}

	// 阈值 == 相似度 → 闭区间命中，合并成 1 簇。
	got := ClusterVectors(input, ClusterOptions{Threshold: sim})
	if len(got.Clusters) != 1 {
		t.Fatalf("sim == threshold 应判为同一人（闭区间），实际分了 %d 簇", len(got.Clusters))
	}

	// 阈值略高于相似度 → 不命中，分成 2 簇。
	got = ClusterVectors(input, ClusterOptions{Threshold: sim + 1e-6})
	if len(got.Clusters) != 2 {
		t.Fatalf("sim < threshold 应分属不同人，实际分了 %d 簇", len(got.Clusters))
	}
}

// TestMaxSpeakersCap max_speakers 上限：3 个明显分开的簇但上限 2 → 合并到 2 类且不 panic。
func TestMaxSpeakersCap(t *testing.T) {
	input := []SegmentVector{
		sv(1, 1, voiceA1),
		sv(2, 2, voiceB1),
		sv(3, 3, voiceC1),
		sv(4, 4, voiceA2),
	}

	// 不限制 → 3 簇。
	if got := ClusterVectors(input, ClusterOptions{MaxSpeakers: 0}); len(got.Clusters) != 3 {
		t.Fatalf("不限制时应分出 3 簇，实际 %d", len(got.Clusters))
	}

	// 上限 2 → 恰好 2 簇，且所有片段都有归属（不丢片段、不 panic）。
	got := ClusterVectors(input, ClusterOptions{MaxSpeakers: 2})
	if len(got.Clusters) != 2 {
		t.Fatalf("上限 2 时应合并到 2 簇，实际 %d", len(got.Clusters))
	}
	if len(got.LabelOf) != len(input) {
		t.Fatalf("所有片段都必须有归属：期望 %d，实际 %d", len(input), len(got.LabelOf))
	}
	for id, label := range got.LabelOf {
		if label < 0 || label >= len(got.Clusters) {
			t.Fatalf("片段 %d 的簇编号越界：%d", id, label)
		}
	}

	// 上限 1 属于非法配置（API-7.6 只允许 0 或 2–8），但也要不 panic：
	// 传进来就按 1 簇处理，绝不让库层崩溃。
	got = ClusterVectors(input, ClusterOptions{MaxSpeakers: 1})
	if len(got.Clusters) != 1 {
		t.Fatalf("上限 1 时应只有 1 簇，实际 %d", len(got.Clusters))
	}
}

// TestClusterIgnoresEmptyVectors 空向量不参与聚类（调用方据此把它们归「未识别」）。
func TestClusterIgnoresEmptyVectors(t *testing.T) {
	input := []SegmentVector{
		sv(1, 1, voiceA1),
		sv(2, 2, nil),
		sv(3, 3, voiceA2),
	}
	got := ClusterVectors(input, ClusterOptions{})
	if len(got.Clusters) != 1 {
		t.Fatalf("期望 1 个簇（空向量不参与），实际 %d", len(got.Clusters))
	}
	if _, ok := got.LabelOf[2]; ok {
		t.Fatalf("空向量片段不该被分配编号：%v", got.LabelOf)
	}
}

// TestClusterEmptyInput 空输入不 panic，返回空结果。
func TestClusterEmptyInput(t *testing.T) {
	got := ClusterVectors(nil, ClusterOptions{})
	if len(got.Clusters) != 0 || len(got.LabelOf) != 0 {
		t.Fatalf("空输入应返回空结果，实际 %+v", got)
	}
}

// TestCosineEdgeCases 相似度的边界：维度不符 / 空 / 全零一律返回 -1（不可比）。
func TestCosineEdgeCases(t *testing.T) {
	cases := []struct {
		name string
		a, b []float32
	}{
		{"空向量", nil, vec4(1, 0, 0, 0)},
		{"维度不符", vec4(1, 0, 0, 0), vec4(1, 0, 0, 1)[:3]},
		{"全零", vec4(0, 0, 0, 0), vec4(1, 0, 0, 0)},
	}
	for _, c := range cases {
		if got := Cosine(c.a, c.b); got != -1 {
			t.Fatalf("%s：应返回 -1（不可比），实际 %v", c.name, got)
		}
	}
	if got := Cosine(vec4(1, 0, 0, 0), vec4(1, 0, 0, 0)); got < 0.999 {
		t.Fatalf("同一向量相似度应约为 1，实际 %v", got)
	}
}

// sameLabels 比较两份「片段 → 簇编号」映射是否完全一致。
func sameLabels(a, b map[int64]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

// String 便于失败信息里打印簇内容。
func (p Partition) String() string {
	out := ""
	for _, c := range p.Clusters {
		out += fmt.Sprintf("{#%d members=%v ms=%d} ", c.Index, c.Members, c.SpeechMs)
	}
	return out
}
