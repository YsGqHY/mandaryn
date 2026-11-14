package speaker

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/storage"
)

// TestRunWithoutExtractorDegrades 无模型时诚实降级：
// 返回明确错误码、不 panic，且手动角色操作仍然可用（铁律 3）。
func TestRunWithoutExtractorDegrades(t *testing.T) {
	f := newFixture(t)
	audio := newFakeAudio()
	f.seedVoiceSession("s_1", audio, [][]float32{voiceA1, voiceB1})

	// Extractor 与 Audio 都缺 = 本机没装声纹模型包的典型状态。
	repo := f.repo(Options{})

	res, err := repo.Run(context.Background(), "s_1")
	if err == nil {
		t.Fatalf("无提取器时必须返回错误，实际成功：%+v", res)
	}
	if !apierr.HasCode(err, apierr.CodeLocalUnavailable) {
		t.Fatalf("期望 ERR_LOCAL_UNAVAILABLE，实际 %v", err)
	}
	// 不得有任何半成品落库。
	if n := f.count(&storage.Speaker{}); n != 0 {
		t.Fatalf("降级路径不应写入声纹身份，实际 %d 行", n)
	}
	if n := f.count(&storage.Role{}); n != 1 { // 只有内置 rl_unknown
		t.Fatalf("降级路径不应新建角色，实际 %d 个角色", n)
	}

	// 纯 DB 操作必须仍然可用：建角色 → 指派 → 改名。
	sess := f.db.GORM
	role := storage.Role{ID: "rl_manual", Name: "说话人 1", ColorToken: "role-1"}
	if err := sess.Create(&role).Error; err != nil {
		t.Fatalf("建角色失败: %v", err)
	}
	if err := repo.RenameRole(context.Background(), "rl_manual", "小王"); err != nil {
		t.Fatalf("无模型时改名应仍可用: %v", err)
	}
	var got storage.Role
	if err := sess.First(&got, "id = ?", "rl_manual").Error; err != nil {
		t.Fatalf("读角色失败: %v", err)
	}
	if got.Name != "小王" {
		t.Fatalf("改名未生效：%s", got.Name)
	}
}

// TestRunUnknownSession 会话不存在 → ERR_NOT_FOUND。
func TestRunUnknownSession(t *testing.T) {
	f := newFixture(t)
	repo := f.repo(Options{Extractor: &fakeExtractor{id: "m1", dim: 4}, Audio: newFakeAudio()})
	_, err := repo.Run(context.Background(), "s_missing")
	if !apierr.HasCode(err, apierr.CodeNotFound) {
		t.Fatalf("期望 ERR_NOT_FOUND，实际 %v", err)
	}
}

// TestRunEmptySessionID 参数校验。
func TestRunEmptySessionID(t *testing.T) {
	f := newFixture(t)
	repo := f.repo(Options{Extractor: &fakeExtractor{id: "m1", dim: 4}, Audio: newFakeAudio()})
	_, err := repo.Run(context.Background(), "   ")
	if !apierr.HasCode(err, apierr.CodeInvalidArgument) {
		t.Fatalf("期望 ERR_INVALID_ARGUMENT，实际 %v", err)
	}
}

// TestRunNoSegments 未转写的会话：返回零结果且不报错（UI 提示「先转写」）。
func TestRunNoSegments(t *testing.T) {
	f := newFixture(t)
	f.seedSession("s_empty")
	repo := f.repo(Options{Extractor: &fakeExtractor{id: "m1", dim: 4}, Audio: newFakeAudio()})

	res, err := repo.Run(context.Background(), "s_empty")
	if err != nil {
		t.Fatalf("无片段不应报错: %v", err)
	}
	if res.SegmentCount != 0 || res.SpeakerCount != 0 {
		t.Fatalf("应为零结果，实际 %+v", res)
	}
}

// TestRunTooShortSkipped 过短片段（< 1.2s）不提取声纹，计入 TooShortSegments。
func TestRunTooShortSkipped(t *testing.T) {
	f := newFixture(t)
	audio := newFakeAudio()
	f.seedSession("s_1")
	// 0.5s 与 2.0s 两句
	f.seedSegment("s_1", 1, 0, 500, "")
	f.seedSegment("s_1", 2, 3000, 5000, "")
	audio.put("s_1", 0, voiceA1)
	audio.put("s_1", 3000, voiceA1)

	repo := f.repo(Options{Extractor: &fakeExtractor{id: "m1", dim: 4}, Audio: audio})
	res, err := repo.Run(context.Background(), "s_1")
	if err != nil {
		t.Fatalf("分离失败: %v", err)
	}
	if res.TooShortSegments != 1 {
		t.Fatalf("应有 1 条过短片段被跳过，实际 %d", res.TooShortSegments)
	}
	if res.AssignedSegments != 1 || res.SpeakerCount != 1 {
		t.Fatalf("应只有 1 条片段归属 1 个说话人，实际 %+v", res)
	}
}

// TestRunCrossSessionMerge 跨会话归并：不同会话里同一人 → 同一全局声纹条目。
func TestRunCrossSessionMerge(t *testing.T) {
	f := newFixture(t)
	audio := newFakeAudio()
	f.seedVoiceSession("s_a", audio, [][]float32{voiceA1, voiceA2, voiceB1})
	f.seedVoiceSession("s_b", audio, [][]float32{voiceA3, voiceB2})

	repo := f.repo(Options{Extractor: &fakeExtractor{id: "m1", dim: 4}, Audio: audio})

	if _, err := repo.Run(context.Background(), "s_a"); err != nil {
		t.Fatalf("会话 A 分离失败: %v", err)
	}
	if _, err := repo.Run(context.Background(), "s_b"); err != nil {
		t.Fatalf("会话 B 分离失败: %v", err)
	}

	idsA := f.speakerIDs("s_a")
	idsB := f.speakerIDs("s_b")
	if len(idsA) != 2 || len(idsB) != 2 {
		t.Fatalf("两场会话都应分出 2 个说话人：A=%v B=%v", idsA, idsB)
	}
	// 全局身份总数必须是 2（不是 4）：同一人跨会话复用同一 sp_ 身份。
	if n := f.count(&storage.Speaker{}); n != 2 {
		t.Fatalf("跨会话归并后应有 2 个全局身份，实际 %d（A=%v B=%v）", n, idsA, idsB)
	}
	// 两场会话里的声纹身份集合必须完全相同（同一批人）。
	if strings.Join(idsA, ",") != strings.Join(idsB, ",") {
		t.Fatalf("两场会话应复用同一批身份：A=%v B=%v", idsA, idsB)
	}
	// 角色也复用：全局只有「说话人 1」「说话人 2」+ 内置未识别。
	if n := f.count(&storage.Role{}); n != 3 {
		t.Fatalf("角色应被复用（2 个 + 内置未识别），实际 %d", n)
	}
}

// TestRunDoesNotMergeAcrossModels 换模型后不归并：同一段语音在 m2 下成为新身份。
func TestRunDoesNotMergeAcrossModels(t *testing.T) {
	f := newFixture(t)
	audio := newFakeAudio()
	f.seedVoiceSession("s_a", audio, [][]float32{voiceA1})
	f.seedVoiceSession("s_b", audio, [][]float32{voiceA1})

	repoV1 := f.repo(Options{Extractor: &fakeExtractor{id: "m1", dim: 4}, Audio: audio})
	if _, err := repoV1.Run(context.Background(), "s_a"); err != nil {
		t.Fatalf("会话 A 分离失败: %v", err)
	}
	repoV2 := f.repo(Options{Extractor: &fakeExtractor{id: "m2", dim: 4}, Audio: audio})
	if _, err := repoV2.Run(context.Background(), "s_b"); err != nil {
		t.Fatalf("会话 B 分离失败: %v", err)
	}

	// 跨模型不可比 → 两场会话各自一个身份，共 2 行。
	if n := f.count(&storage.Speaker{}); n != 2 {
		t.Fatalf("跨模型不应归并，期望 2 个身份，实际 %d", n)
	}
}

// TestRunIdempotentCut 幂等重跑：同一会话跑两次，role / speaker / 映射行都不翻倍。
func TestRunIdempotentCut(t *testing.T) {
	f := newFixture(t)
	audio := newFakeAudio()
	f.seedVoiceSession("s_1", audio, [][]float32{voiceA1, voiceA2, voiceB1, voiceB2})

	repo := f.repo(Options{Extractor: &fakeExtractor{id: "m1", dim: 4}, Audio: audio})

	first, err := repo.Run(context.Background(), "s_1")
	if err != nil {
		t.Fatalf("首次分离失败: %v", err)
	}
	speakers1 := f.count(&storage.Speaker{})
	roles1 := f.count(&storage.Role{})
	links1 := f.count(&storage.SessionSpeaker{})
	ids1 := f.speakerIDs("s_1")

	second, err := repo.Run(context.Background(), "s_1")
	if err != nil {
		t.Fatalf("重跑失败: %v", err)
	}

	if got := f.count(&storage.Speaker{}); got != speakers1 {
		t.Fatalf("重跑不该新增声纹身份：首次 %d，重跑后 %d", speakers1, got)
	}
	if got := f.count(&storage.Role{}); got != roles1 {
		t.Fatalf("重跑不该新增角色：首次 %d，重跑后 %d", roles1, got)
	}
	if got := f.count(&storage.SessionSpeaker{}); got != links1 {
		t.Fatalf("重跑不该新增映射行：首次 %d，重跑后 %d", links1, got)
	}
	if got := f.speakerIDs("s_1"); strings.Join(got, ",") != strings.Join(ids1, ",") {
		t.Fatalf("重跑后说话人身份应保持不变：首次 %v，重跑后 %v", ids1, got)
	}
	if first.SpeakerCount != second.SpeakerCount {
		t.Fatalf("重跑结果应一致：首次 %d，重跑 %d", first.SpeakerCount, second.SpeakerCount)
	}
}

// TestRunRespectsMaxSpeakers app_settings.max_speakers 上限在编排层生效。
func TestRunRespectsMaxSpeakers(t *testing.T) {
	f := newFixture(t)
	audio := newFakeAudio()
	f.seedVoiceSession("s_1", audio, [][]float32{voiceA1, voiceB1, voiceC1})

	repo := f.repo(Options{
		Extractor: &fakeExtractor{id: "m1", dim: 4},
		Audio:     audio,
		MaxSpeakers: func(context.Context) (int, error) {
			return 2, nil
		},
	})
	res, err := repo.Run(context.Background(), "s_1")
	if err != nil {
		t.Fatalf("分离失败: %v", err)
	}
	if res.SpeakerCount != 2 {
		t.Fatalf("上限 2 时应分出 2 个说话人，实际 %d", res.SpeakerCount)
	}
	if n := f.count(&storage.Speaker{}); n != 2 {
		t.Fatalf("全局身份也应只有 2 个，实际 %d", n)
	}
}

// TestRunCanceled 取消：返回明确状态错误，且库内不留半成品。
func TestRunCanceled(t *testing.T) {
	f := newFixture(t)
	audio := newFakeAudio()
	f.seedVoiceSession("s_1", audio, [][]float32{voiceA1, voiceB1})

	repo := f.repo(Options{Extractor: &fakeExtractor{id: "m1", dim: 4}, Audio: audio})
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 进入即已取消

	if _, err := repo.Run(ctx, "s_1"); !apierr.HasCode(err, apierr.CodeStateForbidden) {
		t.Fatalf("取消应返回 ERR_STATE_FORBIDDEN，实际 %v", err)
	}
	if n := f.count(&storage.Speaker{}); n != 0 {
		t.Fatalf("取消后不应写入身份，实际 %d 行", n)
	}
	if n := f.count(&storage.SessionSpeaker{}); n != 0 {
		t.Fatalf("取消后不应写入映射，实际 %d 行", n)
	}
}

// TestRunPartialAudioFailure 个别片段音频不可读时跳过，其余照常归属。
func TestRunPartialAudioFailure(t *testing.T) {
	f := newFixture(t)
	audio := newFakeAudio()
	f.seedVoiceSession("s_1", audio, [][]float32{voiceA1, voiceA2, voiceB1})
	audio.fail("s_1", 3000) // 第 2 句不可读

	repo := f.repo(Options{Extractor: &fakeExtractor{id: "m1", dim: 4}, Audio: audio})
	res, err := repo.Run(context.Background(), "s_1")
	if err != nil {
		t.Fatalf("个别片段失败不应让整场失败: %v", err)
	}
	if res.FailedSegments != 1 {
		t.Fatalf("应记录 1 条失败片段，实际 %d", res.FailedSegments)
	}
	// 剩下 2 条片段（A 类 + B 类）→ 2 个说话人。
	if res.SpeakerCount != 2 {
		t.Fatalf("剩下两条片段应分出 2 人，实际 %d", res.SpeakerCount)
	}
	// 失败的那句归「未识别」（speaker_id 仍为空）。
	var failed storage.Segment
	if err := f.db.GORM.First(&failed, "session_id = ? AND seq = ?", "s_1", 2).Error; err != nil {
		t.Fatalf("读片段失败: %v", err)
	}
	if failed.SpeakerID != "" {
		t.Fatalf("音频不可读的片段应保持未识别，实际 %s", failed.SpeakerID)
	}
}

// TestRunAllEmbedFailures 全部提取失败时抛出真实错误码，而不是「0 个说话人」。
func TestRunAllEmbedFailures(t *testing.T) {
	f := newFixture(t)
	audio := newFakeAudio()
	f.seedVoiceSession("s_1", audio, [][]float32{voiceA1, voiceB1})

	repo := f.repo(Options{
		Extractor: &fakeExtractor{id: "m1", dim: 4, failAll: true},
		Audio:     audio,
	})
	_, err := repo.Run(context.Background(), "s_1")
	if err == nil {
		t.Fatal("全部提取失败时应返回错误")
	}
	var e *apierr.Err
	if !errors.As(err, &e) || e.Code == "" {
		t.Fatalf("应返回带错误码的 *apierr.Err，实际 %v", err)
	}
}

// TestRunConcurrentRejected 同一会话不允许并发分离。
func TestRunConcurrentRejected(t *testing.T) {
	f := newFixture(t)
	repo := f.repo(Options{Extractor: &fakeExtractor{id: "m1", dim: 4}, Audio: newFakeAudio()})

	if !repo.acquire("s_1") {
		t.Fatal("首次抢占应成功")
	}
	defer repo.release("s_1")
	if _, err := repo.Run(context.Background(), "s_1"); !apierr.HasCode(err, apierr.CodeStateForbidden) {
		t.Fatalf("并发分离应被拒，实际 %v", err)
	}
}

// TestEncryptDecryptVector 声纹向量加密落库 + 可解回（并且明文不出现在密文里）。
func TestEncryptDecryptVector(t *testing.T) {
	enc, err := EncodeVector(voiceA1)
	if err != nil {
		t.Fatalf("编码失败: %v", err)
	}
	// 密文不能等于可读的明文编码（base64 的 float 字节），也不能把向量当成字符串塞进去。
	plain := base64.StdEncoding.EncodeToString([]byte{
		0, 0, 128, 63, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
	})
	if enc == plain || strings.Contains(enc, plain) {
		t.Fatalf("密文疑似就是明文编码：%s", enc)
	}
	// AES-GCM 带随机 nonce：同一向量两次加密结果必须不同。
	enc2, err := EncodeVector(voiceA1)
	if err != nil {
		t.Fatalf("二次编码失败: %v", err)
	}
	if enc == enc2 {
		t.Fatal("同一向量两次加密结果相同，说明 nonce 复用了")
	}
	got, err := DecodeVector(enc, 4)
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("维度不符：%d", len(got))
	}
	for i := range got {
		if got[i] != voiceA1[i] {
			t.Fatalf("第 %d 位不符：%v != %v", i, got[i], voiceA1[i])
		}
	}
	// 空密文是合法状态（手工拆分出的身份）。
	if v, err := DecodeVector("", 0); err != nil || v != nil {
		t.Fatalf("空密文应返回 nil,nil，实际 %v,%v", v, err)
	}
	// 维度不符要报错，而不是静默截断。
	if _, err := DecodeVector(enc, 8); err == nil {
		t.Fatal("维度不符应报错")
	}
	// 非法密文报错（不 panic）。
	if _, err := DecodeVector("not-a-ciphertext", 0); err == nil {
		t.Fatal("非法密文应报错")
	}
}

// cancelingAudio 在第 n 次取音频时取消 context，用来验证「处理中途取消」的路径。
type cancelingAudio struct {
	inner    *fakeAudio
	cancelAt int
	calls    int
	cancel   context.CancelFunc
}

func (a *cancelingAudio) LoadSegment(ctx context.Context, sessionID string, startMs, endMs int64) ([]float32, error) {
	a.calls++
	if a.calls == a.cancelAt {
		a.cancel()
		return nil, ctx.Err()
	}
	return a.inner.LoadSegment(ctx, sessionID, startMs, endMs)
}

// TestRunCancelMidway 处理中途取消：返回「已取消」状态错误，且不写半成品。
func TestRunCancelMidway(t *testing.T) {
	f := newFixture(t)
	audio := newFakeAudio()
	f.seedVoiceSession("s_1", audio, [][]float32{voiceA1, voiceB1, voiceA2})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelAudio := &cancelingAudio{inner: audio, cancelAt: 2, cancel: cancel}

	repo := f.repo(Options{Extractor: &fakeExtractor{id: "m1", dim: 4}, Audio: cancelAudio})
	if _, err := repo.Run(ctx, "s_1"); !apierr.HasCode(err, apierr.CodeStateForbidden) {
		t.Fatalf("中途取消应返回 ERR_STATE_FORBIDDEN，实际 %v", err)
	}
	if n := f.count(&storage.Speaker{}); n != 0 {
		t.Fatalf("取消后不应写入声纹身份，实际 %d", n)
	}
	if n := f.count(&storage.SessionSpeaker{}); n != 0 {
		t.Fatalf("取消后不应写入映射，实际 %d", n)
	}
	// 片段身份保持为空（未识别），而不是被写了半截。
	for _, s := range f.snapshotSegments() {
		if s.SpeakerID != "" {
			t.Fatalf("取消后片段 %d 不应被分配说话人，实际 %q", s.ID, s.SpeakerID)
		}
	}
}
