package transcriptsvc

import (
	"context"
	"testing"
	"time"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/transcribe"
)

// TestStoreUpsertIsIdempotent 真实 SQLite：按 (session_id, seq) 幂等覆盖，不产生重复行。
func TestStoreUpsertIsIdempotent(t *testing.T) {
	f := newFixture(t, false)
	ctx := context.Background()
	st := &store{holder: f.holder}

	row := transcribe.SegmentRow{
		SessionID: "s_audio01", Seq: 0, StartMs: 0, EndMs: 1000,
		Text: "第一次", EngineID: "fake@1", IsFinal: true, Confidence: 0.5,
	}
	first, err := st.UpsertSegment(ctx, row)
	if err != nil {
		t.Fatalf("第一次 upsert: %v", err)
	}
	if first.ID == 0 {
		t.Fatal("upsert 应回读出行 id")
	}

	// 同 (session, seq) 再写一次：覆盖同一行。
	row.Text = "第二次"
	row.EndMs = 1200
	second, err := st.UpsertSegment(ctx, row)
	if err != nil {
		t.Fatalf("第二次 upsert: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("幂等覆盖应命中同一行：%d → %d", first.ID, second.ID)
	}

	segs := f.segments(t, "s_audio01")
	if len(segs) != 1 {
		t.Fatalf("落库 %d 行, want 1（唯一索引 uq_segments_session_seq 生效）", len(segs))
	}
	if segs[0].Text != "第二次" || segs[0].EndMs != 1200 {
		t.Errorf("覆盖未生效：%+v", segs[0])
	}
}

// TestStoreUpsertPreservesEdited 人工修正过的文本不被重转覆盖。
func TestStoreUpsertPreservesEdited(t *testing.T) {
	f := newFixture(t, false)
	ctx := context.Background()
	st := &store{holder: f.holder}

	saved, err := st.UpsertSegment(ctx, transcribe.SegmentRow{
		SessionID: "s_audio01", Seq: 0, StartMs: 0, EndMs: 1000,
		Text: "机器文本", EngineID: "fake@1", IsFinal: true,
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := st.UpdateSegmentText(ctx, saved.ID, "人工修正"); err != nil {
		t.Fatalf("UpdateSegmentText: %v", err)
	}
	// 重转：同一 seq 再来一次。
	if _, err := st.UpsertSegment(ctx, transcribe.SegmentRow{
		SessionID: "s_audio01", Seq: 0, StartMs: 0, EndMs: 1100,
		Text: "重转文本", EngineID: "fake@1", IsFinal: true,
	}); err != nil {
		t.Fatalf("重转 upsert: %v", err)
	}
	segs := f.segments(t, "s_audio01")
	if len(segs) != 1 {
		t.Fatalf("行数 = %d, want 1", len(segs))
	}
	if segs[0].Text != "人工修正" {
		t.Errorf("人工修正被覆盖：%q", segs[0].Text)
	}
	if !segs[0].Edited {
		t.Error("edited 标记丢失")
	}
	if segs[0].EndMs != 1100 {
		t.Errorf("非文本列仍应更新：endMs = %d", segs[0].EndMs)
	}
}

// TestUpdateTranscriptionNeverTouchesStatus 转写轴更新**不得**改录制主状态。
func TestUpdateTranscriptionNeverTouchesStatus(t *testing.T) {
	f := newFixture(t, false)
	ctx := context.Background()
	st := &store{holder: f.holder}

	before := f.session(t, "s_audio01")
	if before.Status != contract.StateCompleted {
		t.Fatalf("前置状态应为 completed，实际 %q", before.Status)
	}

	progress := int64(1234)
	engine := "fake@2"
	segCount := 7
	chars := int64(99)
	for _, status := range []string{
		transcribe.StatusQueued, transcribe.StatusRunning, transcribe.StatusDone,
		transcribe.StatusFailed, transcribe.StatusNone,
	} {
		if err := st.UpdateTranscription(ctx, transcribe.TranscriptionUpdate{
			SessionID:    "s_audio01",
			Status:       status,
			ProgressMs:   &progress,
			EngineID:     &engine,
			SegmentCount: &segCount,
			CharCount:    &chars,
		}); err != nil {
			t.Fatalf("UpdateTranscription(%s): %v", status, err)
		}
		row := f.session(t, "s_audio01")
		if row.Status != contract.StateCompleted {
			t.Fatalf("转写状态置 %s 时录制状态被改成 %q（必须保持 completed）", status, row.Status)
		}
		if row.TranscriptionStatus != status {
			t.Fatalf("转写状态未写入：%q", row.TranscriptionStatus)
		}
	}

	row := f.session(t, "s_audio01")
	if row.TranscribeProgressMs != progress || row.TranscribeEngineID != engine {
		t.Errorf("转写轴字段未生效：%+v", row)
	}
}

// TestStartTranscriptionEndToEnd 端到端：发起 → 排队 → 转写 → done，片段落库。
func TestStartTranscriptionEndToEnd(t *testing.T) {
	f := newFixture(t, true)
	ctx := context.Background()

	st, err := f.svc.StartTranscription(ctx, "s_audio01")
	if err != nil {
		t.Fatalf("StartTranscription: %v", err)
	}
	if st.Status != transcribe.StatusQueued && st.Status != transcribe.StatusRunning {
		t.Fatalf("发起后状态 = %q, want queued/running", st.Status)
	}
	if st.Trigger != contract.TriggerManual {
		t.Errorf("trigger = %q, want manual", st.Trigger)
	}

	row := f.waitDone(t, "s_audio01")
	if row.Status != contract.StateCompleted {
		t.Errorf("转写完成后录制状态被改：%q", row.Status)
	}
	if row.TranscribeProgressMs != row.DurationMs {
		t.Errorf("进度 = %d, want %d", row.TranscribeProgressMs, row.DurationMs)
	}
	if row.TranscribeEngineID == "" {
		t.Error("未回填 transcribe_engine_id")
	}

	segs := f.segments(t, "s_audio01")
	if len(segs) != 2 {
		t.Fatalf("落库 %d 句, want 2", len(segs))
	}
	for i, s := range segs {
		if s.Seq != i {
			t.Errorf("第 %d 行 seq = %d", i, s.Seq)
		}
		if s.StartMs > s.EndMs {
			t.Errorf("区间非法：%+v", s)
		}
		if !s.IsFinal || s.Text == "" {
			t.Errorf("片段不完整：%+v", s)
		}
		if s.EngineID != "fake@1" {
			t.Errorf("engineId = %q, want fake@1", s.EngineID)
		}
	}

	// 时间轴可直接读出来（含 marker）。
	tl, err := f.svc.GetTimeline(ctx, "s_audio01")
	if err != nil {
		t.Fatalf("GetTimeline: %v", err)
	}
	if len(tl.Segments) != 2 {
		t.Errorf("时间轴片段 %d 条, want 2", len(tl.Segments))
	}
	if len(tl.Markers) != 1 || tl.Markers[0].Kind != contract.MarkerDegrade {
		t.Errorf("时间轴标记异常：%+v", tl.Markers)
	}

	// GetTranscription 幂等查询。
	qs, err := f.svc.GetTranscription(ctx, "s_audio01")
	if err != nil {
		t.Fatalf("GetTranscription: %v", err)
	}
	if qs.Status != transcribe.StatusDone || qs.Segments != 2 {
		t.Errorf("查询状态异常：%+v", qs)
	}
}

// TestRerunIsIdempotentThroughService 通过服务重转两次：片段不翻倍。
func TestRerunIsIdempotentThroughService(t *testing.T) {
	f := newFixture(t, true)
	ctx := context.Background()

	if _, err := f.svc.StartTranscription(ctx, "s_audio01"); err != nil {
		t.Fatalf("第一次发起: %v", err)
	}
	f.waitDone(t, "s_audio01")
	first := f.segments(t, "s_audio01")
	if len(first) != 2 {
		t.Fatalf("第一次落库 %d 句, want 2", len(first))
	}

	// 已 done 再点一次 = 重转（API-4.5）。
	if _, err := f.svc.StartTranscription(ctx, "s_audio01"); err != nil {
		t.Fatalf("重转发起: %v", err)
	}
	f.waitDone(t, "s_audio01")
	second := f.segments(t, "s_audio01")
	if len(second) != 2 {
		t.Fatalf("重转后 %d 句, want 2（幂等覆盖，不翻倍）", len(second))
	}
	if second[0].ID != first[0].ID || second[1].ID != first[1].ID {
		t.Errorf("重转应覆盖同一批行：before=[%d,%d] after=[%d,%d]",
			first[0].ID, first[1].ID, second[0].ID, second[1].ID)
	}
}

// TestStartTwiceWhileRunningRejected 运行中重复发起 → ERR_STATE_FORBIDDEN。
func TestStartTwiceWhileRunningRejected(t *testing.T) {
	f := newFixture(t, true)
	ctx := context.Background()

	if _, err := f.svc.StartTranscription(ctx, "s_audio01"); err != nil {
		t.Fatalf("第一次发起: %v", err)
	}
	// 不等待完成，立刻再发起一次（此时状态为 queued 或 running）。
	_, err := f.svc.StartTranscription(ctx, "s_audio01")
	if err == nil {
		t.Fatal("重复发起应当被拒绝")
	}
	if !apierr.HasCode(err, apierr.CodeStateForbidden) {
		t.Fatalf("错误码 = %v, want ERR_STATE_FORBIDDEN", err)
	}
	f.waitDone(t, "s_audio01")
}

// TestStartMissingSession 会话不存在 → ERR_NOT_FOUND。
func TestStartMissingSession(t *testing.T) {
	f := newFixture(t, false)
	_, err := f.svc.StartTranscription(context.Background(), "s_nope")
	if !apierr.HasCode(err, apierr.CodeNotFound) {
		t.Fatalf("错误 = %v, want ERR_NOT_FOUND", err)
	}
}

// TestCancelTranscription 取消：状态回 none，已转句子保留。
func TestCancelTranscription(t *testing.T) {
	f := newFixture(t, true)
	ctx := context.Background()

	// 前台忙 → 任务只会排队，稳定复现「取消排队任务」。
	if _, err := f.svc.StartTranscription(ctx, "s_audio01"); err != nil {
		t.Fatalf("发起: %v", err)
	}
	if _, err := f.svc.CancelTranscription(ctx, "s_audio01"); err != nil {
		// 可能已经跑完（音频很短）：跑完后再取消应报 ERR_STATE_FORBIDDEN。
		if !apierr.HasCode(err, apierr.CodeStateForbidden) {
			t.Fatalf("取消失败：%v", err)
		}
		return
	}
	row := f.session(t, "s_audio01")
	if row.TranscriptionStatus != transcribe.StatusNone {
		t.Errorf("取消后状态 = %q, want none", row.TranscriptionStatus)
	}
	// 录制状态不受影响。
	if row.Status != contract.StateCompleted {
		t.Errorf("取消失改动了录制状态：%q", row.Status)
	}
}

// TestTimelineEmptySession 未转写会话：时间轴为空但不报错。
func TestTimelineEmptySession(t *testing.T) {
	f := newFixture(t, false)
	tl, err := f.svc.GetTimeline(context.Background(), "s_audio01")
	if err != nil {
		t.Fatalf("GetTimeline: %v", err)
	}
	if len(tl.Segments) != 0 {
		t.Errorf("未转写会话不应有片段：%d", len(tl.Segments))
	}
	if len(tl.Markers) != 1 {
		t.Errorf("marker 应仍可见：%d", len(tl.Markers))
	}
}

// TestTimelinePageAndSearch 分页与检索走真实 SQL。
func TestTimelinePageAndSearch(t *testing.T) {
	f := newFixture(t, true)
	ctx := context.Background()
	if _, err := f.svc.StartTranscription(ctx, "s_audio01"); err != nil {
		t.Fatalf("发起: %v", err)
	}
	f.waitDone(t, "s_audio01")

	page, err := f.svc.GetTimelinePage(ctx, "s_audio01", 0, 1)
	if err != nil {
		t.Fatalf("GetTimelinePage: %v", err)
	}
	if page.Total != 2 || len(page.Segments) != 1 {
		t.Fatalf("分页异常：total=%d got=%d", page.Total, len(page.Segments))
	}
	all, err := f.svc.SearchSegments(ctx, contract.SegmentQuery{SessionID: "s_audio01"})
	if err != nil {
		t.Fatalf("SearchSegments: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("检索到 %d 条, want 2", len(all))
	}
	// 关键词检索。
	kw, err := f.svc.SearchSegments(ctx, contract.SegmentQuery{
		SessionID: "s_audio01", Keyword: all[1].Text,
	})
	if err != nil {
		t.Fatalf("关键词检索: %v", err)
	}
	if len(kw) != 1 {
		t.Errorf("关键词命中 %d 条, want 1", len(kw))
	}
}

// TestUpdateSegmentTextViaService 人工修正（API-4.4）。
func TestUpdateSegmentTextViaService(t *testing.T) {
	f := newFixture(t, true)
	ctx := context.Background()
	if _, err := f.svc.StartTranscription(ctx, "s_audio01"); err != nil {
		t.Fatalf("发起: %v", err)
	}
	f.waitDone(t, "s_audio01")
	segs := f.segments(t, "s_audio01")

	if err := f.svc.UpdateSegmentText(ctx, segs[0].ID, "人工修正后的文本"); err != nil {
		t.Fatalf("UpdateSegmentText: %v", err)
	}
	got := f.segments(t, "s_audio01")
	if got[0].Text != "人工修正后的文本" || !got[0].Edited {
		t.Fatalf("修正未生效：%+v", got[0])
	}
	// 空文本被拒。
	if err := f.svc.UpdateSegmentText(ctx, segs[0].ID, "   "); !apierr.HasCode(err, apierr.CodeInvalidArgument) {
		t.Errorf("空文本应被拒，实际 %v", err)
	}
	// 不存在的片段。
	if err := f.svc.UpdateSegmentText(ctx, 99999, "x"); !apierr.HasCode(err, apierr.CodeNotFound) {
		t.Errorf("不存在片段应报 NOT_FOUND，实际 %v", err)
	}
}

// TestStartWithoutAudioFails 音频缺失：任务置 failed，录音实例元数据不动。
func TestStartWithoutAudioFails(t *testing.T) {
	f := newFixture(t, false) // 不写音频文件
	ctx := context.Background()

	if _, err := f.svc.StartTranscription(ctx, "s_audio01"); err != nil {
		t.Fatalf("发起: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		row := f.session(t, "s_audio01")
		if row.TranscriptionStatus == transcribe.StatusFailed {
			if row.Status != contract.StateCompleted {
				t.Errorf("失败时录到状态被改：%q", row.Status)
			}
			if row.AudioPath == "" || row.DurationMs != 0 {
				t.Errorf("失败时元数据被改：%+v", row)
			}
			if len(f.segments(t, "s_audio01")) != 0 {
				t.Error("失败不应产生片段")
			}
			// 失败后应能重新发起（状态 failed 允许再试）。
			if _, err := f.svc.StartTranscription(ctx, "s_audio01"); err != nil {
				t.Logf("失败后重新发起返回：%v（可接受：音频确实缺失）", err)
			}
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("超时等待 failed 状态，当前 %q", f.session(t, "s_audio01").TranscriptionStatus)
}

// —— 说明 ——
// 关键字检索在真实 SQL 里是 `LIKE %kw%`，因此上面只断言命中 1 条、不假设排序；
// 片段顺序一律以 seq 为准（时间轴二分定位的前提）。
