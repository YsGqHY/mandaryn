package speaker

import (
	"testing"
)

// TestMatchGlobalThreshold 跨会话归并判定：相近则命中同一全局条目，不相近则不命中。
func TestMatchGlobalThreshold(t *testing.T) {
	entries := []GlobalIdentity{
		{SpeakerID: "sp_a", ModelID: "m1", Vector: Normalize(voiceA1), Weight: 1},
		{SpeakerID: "sp_b", ModelID: "m1", Vector: Normalize(voiceB1), Weight: 1},
	}

	// 与会话 A 同一人的向量 → 命中 sp_a。
	idx, sim := MatchGlobal(entries, Normalize(voiceA2), "m1")
	if idx < 0 {
		t.Fatalf("应命中既有条目，实际没有候选")
	}
	if entries[idx].SpeakerID != "sp_a" {
		t.Fatalf("应命中 sp_a，实际 %s（sim=%.4f）", entries[idx].SpeakerID, sim)
	}
	if sim < DefaultMergeThreshold {
		t.Fatalf("同人相似度应 >= %.2f，实际 %.4f", DefaultMergeThreshold, sim)
	}

	// 完全不同的向量 → 最相似度仍很低，调用方据此不归并。
	idx2, sim2 := MatchGlobal(entries, Normalize(vec4(0, 0, 0, 1)), "m1")
	if idx2 >= 0 && sim2 >= DefaultMergeThreshold {
		t.Fatalf("不同说话人不应命中归并阈值，实际 sim=%.4f", sim2)
	}
}

// TestMatchGlobalSkipsForeignModel 换模型后不比较相似度（DOC-04 §4.3）。
func TestMatchGlobalSkipsForeignModel(t *testing.T) {
	entries := []GlobalIdentity{
		{SpeakerID: "sp_a", ModelID: "m1", Vector: Normalize(voiceA1), Weight: 1},
	}
	// 向量完全相同，但模型不同 → 没有可比候选。
	idx, sim := MatchGlobal(entries, Normalize(voiceA1), "m2")
	if idx != -1 || sim != -1 {
		t.Fatalf("跨模型不应参与匹配，实际 idx=%d sim=%v", idx, sim)
	}
}

// TestMergeCentroidWeighted 质心按权重加权更新，而不是被最新一条覆盖。
func TestMergeCentroidWeighted(t *testing.T) {
	e := GlobalIdentity{SpeakerID: "sp_a", ModelID: "m1", Vector: Normalize(vec4(1, 0, 0, 0)), Weight: 3}
	ok := MergeCentroid(&e, vec4(0, 1, 0, 0), 1)
	if !ok {
		t.Fatal("融合应成功")
	}
	if e.Weight != 4 {
		t.Fatalf("权重应累加为 4，实际 %d", e.Weight)
	}
	// 加权后更靠近原来的方向（3:1），y 分量应约为 0.25。
	y := e.Vector[1]
	if y < 0.15 || y > 0.35 {
		t.Fatalf("加权融合结果偏离预期：y=%.4f（期望 ≈0.25）", y)
	}
	// 维度不符时不改质心。
	before := append([]float32(nil), e.Vector...)
	if ok := MergeCentroid(&e, vec4(1, 0, 0, 0)[:2], 1); ok {
		t.Fatal("维度不符应返回 false")
	}
	if len(e.Vector) != len(before) {
		t.Fatal("维度不符时不应改动质心")
	}
}

// TestMergeCentroidIntoEmpty 目标没有向量时直接采用新向量（手工拆分出的身份会走到这里）。
func TestMergeCentroidIntoEmpty(t *testing.T) {
	e := GlobalIdentity{SpeakerID: "sp_split", ModelID: "m1", Weight: 0}
	if ok := MergeCentroid(&e, voiceA2, 2); !ok {
		t.Fatal("目标无向量时应成功接管新向量")
	}
	if len(e.Vector) == 0 || e.Weight != 2 {
		t.Fatalf("应写入向量与权重，实际 len=%d weight=%d", len(e.Vector), e.Weight)
	}
}

// TestBlendCentroids 两个质心融合：计数与时长累加，空向量侧不抹掉有向量侧。
func TestBlendCentroids(t *testing.T) {
	dst := GlobalIdentity{SpeakerID: "sp_a", Vector: Normalize(voiceA1), Weight: 1,
		SessionCount: 2, TotalSpeechMs: 5000, FirstSeenAt: 100, LastSeenAt: 200}
	src := GlobalIdentity{SpeakerID: "sp_b", Vector: Normalize(voiceA2), Weight: 1,
		SessionCount: 3, TotalSpeechMs: 7000, FirstSeenAt: 50, LastSeenAt: 300}
	BlendCentroids(&dst, &src)

	if dst.SessionCount != 5 || dst.TotalSpeechMs != 12000 {
		t.Fatalf("计数应累加：sessions=%d ms=%d", dst.SessionCount, dst.TotalSpeechMs)
	}
	if dst.FirstSeenAt != 50 || dst.LastSeenAt != 300 {
		t.Fatalf("时间范围应取并集：first=%d last=%d", dst.FirstSeenAt, dst.LastSeenAt)
	}
	if len(dst.Vector) == 0 {
		t.Fatal("融合后应保有向量")
	}

	// src 没有向量 → dst 保留自己的向量。
	keep := append([]float32(nil), dst.Vector...)
	BlendCentroids(&dst, &GlobalIdentity{SpeakerID: "sp_empty"})
	if len(dst.Vector) != len(keep) {
		t.Fatal("另一侧无向量时不应抹掉已有向量")
	}
}
