package transcriptsvc

import (
	"context"
	"strings"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/storage"
	"vip.oberon.mandaryn/internal/transcribe"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// store 是 transcribe.Store 的 GORM 实现（L3 唯一的持久化落点）。
//
// 三条硬约束：
//  1. `UpdateTranscription` **只**更新转写轴列，绝不写 `sessions.status`
//     （录制主状态，ARCH §6.4 / TBL-03）；本文件里出现 "status" 字面量即为缺陷。
//  2. 片段写入按 `(session_id, seq)` upsert —— 幂等覆盖靠 TBL-04 的
//     `uq_segments_session_seq` 唯一索引，而不是「先查后写」。
//  3. 人工修正过（`edited=1`）的文本不被重转覆盖：用户的显式修改优先级最高。
type store struct {
	holder *storage.Holder
}

// db 每次取当前句柄（数据目录切换会替换 DB，不能缓存）。
func (s *store) db() *gorm.DB { return s.holder.Current().GORM }

// LoadSession 读取会话转写相关的投影。
func (s *store) LoadSession(ctx context.Context, sessionID string) (transcribe.SessionRow, error) {
	var row storage.Session
	if err := s.db().WithContext(ctx).First(&row, "id = ?", sessionID).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return transcribe.SessionRow{}, apierr.New(apierr.CodeNotFound).
				WithDetail("会话不存在：%s", sessionID)
		}
		return transcribe.SessionRow{}, apierr.Wrap(err, apierr.CodeDBWrite)
	}
	return transcribe.SessionRow{
		ID:                   row.ID,
		Title:                row.Title,
		AudioPath:            row.AudioPath,
		VadPreset:            row.VadPreset,
		DurationMs:           row.DurationMs,
		State:                row.Status,
		TranscriptionStatus:  row.TranscriptionStatus,
		TranscribeProgressMs: row.TranscribeProgressMs,
		TranscribeEngineID:   row.TranscribeEngineID,
		TranscribedAt:        row.TranscribedAt,
		EngineID:             row.EngineID,
		LiveTranscribed:      row.LiveTranscribed,
		SegmentCount:         row.SegmentCount,
		CharCount:            row.CharCount,
	}, nil
}

// UpdateTranscription 只更新转写轴（`transcription_status` / `transcribe_progress_ms` /
// `transcribe_engine_id` / `transcribed_at` / 冗余计数）。
//
// 注意：这里**不会**也**不允许**出现 `status` 列（录制主状态）——
// 转写与录制是两条正交的轴，混写会让「已 completed 但还在转写中」变得无法表达。
func (s *store) UpdateTranscription(ctx context.Context, u transcribe.TranscriptionUpdate) error {
	updates := map[string]any{}
	if u.Status != "" {
		updates["transcription_status"] = u.Status
	}
	if u.ProgressMs != nil {
		updates["transcribe_progress_ms"] = *u.ProgressMs
	}
	if u.EngineID != nil {
		updates["transcribe_engine_id"] = *u.EngineID
	}
	if u.TranscribedAt != nil {
		updates["transcribed_at"] = *u.TranscribedAt
	}
	if u.SegmentCount != nil {
		updates["segment_count"] = *u.SegmentCount
	}
	if u.CharCount != nil {
		updates["char_count"] = *u.CharCount
	}
	if len(updates) == 0 {
		return nil
	}
	res := s.db().WithContext(ctx).Model(&storage.Session{}).
		Where("id = ?", u.SessionID).Updates(updates)
	if res.Error != nil {
		return apierr.Wrap(res.Error, apierr.CodeDBWrite)
	}
	if res.RowsAffected == 0 {
		// 会话在任务执行期间被删除（用户删了录音实例）：不是故障，
		// 只是任务无处落地，按 NOT_FOUND 让上层收尾。
		return apierr.New(apierr.CodeNotFound).WithDetail("会话已不存在：%s", u.SessionID)
	}
	return nil
}

// UpsertSegment 按 `(session_id, seq)` 幂等覆盖写入片段。
func (s *store) UpsertSegment(ctx context.Context, row transcribe.SegmentRow) (transcribe.SegmentRow, error) {
	model := storage.Segment{
		SessionID:  row.SessionID,
		Seq:        row.Seq,
		StartMs:    row.StartMs,
		EndMs:      row.EndMs,
		Text:       row.Text,
		SpeakerID:  row.SpeakerID,
		Confidence: float64(row.Confidence),
		EngineID:   row.EngineID,
		IsFinal:    row.IsFinal,
	}
	// `excluded.` 是 SQLite 对「本应插入的那一行」的引用；`segments.` 是现有行。
	// 人工修正过的文本保留用户版本（FR-06 的人工修正不该被重转抹掉）。
	upsert := s.db().WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "session_id"}, {Name: "seq"}},
		DoUpdates: clause.Assignments(map[string]any{
			"start_ms":   gorm.Expr("excluded.start_ms"),
			"end_ms":     gorm.Expr("excluded.end_ms"),
			"text":       gorm.Expr("CASE WHEN segments.edited THEN segments.text ELSE excluded.text END"),
			"engine_id":  gorm.Expr("excluded.engine_id"),
			"confidence": gorm.Expr("excluded.confidence"),
			"is_final":   gorm.Expr("excluded.is_final"),
		}),
	}).Create(&model)
	if upsert.Error != nil {
		return transcribe.SegmentRow{}, apierr.Wrap(upsert.Error, apierr.CodeDBWrite)
	}

	// 回读落库后的行：OnConflict 更新时不保证 model.ID 正确（可能是插入尝试的 id），
	// 而且需要把 edited 等「由他人修改过」的状态带回给调用方。
	var got storage.Segment
	if err := s.db().WithContext(ctx).
		First(&got, "session_id = ? AND seq = ?", row.SessionID, row.Seq).Error; err != nil {
		return transcribe.SegmentRow{}, apierr.Wrap(err, apierr.CodeDBWrite)
	}
	return toSegmentRow(got), nil
}

// ListSegments 按 seq 升序查询片段。
//
// 时间范围语义是**区间重叠**：`fromMs` 取 `end_ms >= fromMs`、`toMs` 取
// `start_ms <= toMs`。这样跨区间的句子（起点在前、终点在后）也会被包含，
// 否则拖动进度条到一句话中间会看不到这句话。
func (s *store) ListSegments(ctx context.Context, f transcribe.SegmentFilter) (transcribe.SegmentPage, error) {
	scope := func(d *gorm.DB) *gorm.DB {
		q := d.Model(&storage.Segment{})
		if f.SessionID != "" {
			q = q.Where("session_id = ?", f.SessionID)
		}
		if kw := strings.TrimSpace(f.Keyword); kw != "" {
			q = q.Where("text LIKE ?", "%"+kw+"%")
		}
		if f.SpeakerID != "" {
			q = q.Where("speaker_id = ?", f.SpeakerID)
		}
		if f.FromMs > 0 {
			q = q.Where("end_ms >= ?", f.FromMs)
		}
		if f.ToMs > 0 {
			q = q.Where("start_ms <= ?", f.ToMs)
		}
		return q
	}

	var total int64
	if err := scope(s.db().WithContext(ctx)).Count(&total).Error; err != nil {
		return transcribe.SegmentPage{}, apierr.Wrap(err, apierr.CodeDBWrite)
	}

	q := scope(s.db().WithContext(ctx)).Order("session_id asc, seq asc")
	if f.Limit > 0 {
		q = q.Limit(f.Limit)
	}
	if f.Offset > 0 {
		q = q.Offset(f.Offset)
	}
	var rows []storage.Segment
	if err := q.Find(&rows).Error; err != nil {
		return transcribe.SegmentPage{}, apierr.Wrap(err, apierr.CodeDBWrite)
	}
	out := make([]transcribe.SegmentRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, toSegmentRow(r))
	}
	return transcribe.SegmentPage{Total: int(total), Rows: out}, nil
}

// ListMarkers 返回会话事件标记（降级 / 源丢失 / 暂停恢复…），按 at_ms 升序。
func (s *store) ListMarkers(ctx context.Context, sessionID string) ([]transcribe.MarkerRow, error) {
	var rows []storage.SessionEvent
	if err := s.db().WithContext(ctx).
		Where("session_id = ?", sessionID).
		Order("at_ms asc, id asc").Find(&rows).Error; err != nil {
		return nil, apierr.Wrap(err, apierr.CodeDBWrite)
	}
	out := make([]transcribe.MarkerRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, transcribe.MarkerRow{AtMs: r.AtMs, Kind: r.Kind, Note: r.Note})
	}
	return out, nil
}

// UpdateSegmentText 人工修正文本并置 `edited=true`。
func (s *store) UpdateSegmentText(ctx context.Context, segmentID int64, text string) error {
	res := s.db().WithContext(ctx).Model(&storage.Segment{}).
		Where("id = ?", segmentID).
		Updates(map[string]any{"text": text, "edited": true})
	if res.Error != nil {
		return apierr.Wrap(res.Error, apierr.CodeDBWrite)
	}
	if res.RowsAffected == 0 {
		return apierr.New(apierr.CodeNotFound).WithDetail("片段不存在：%d", segmentID)
	}
	return nil
}

// MaxSeq 返回该会话已落库的最大 seq（无片段返回 -1）。
func (s *store) MaxSeq(ctx context.Context, sessionID string) (int, error) {
	var v struct {
		MaxSeq int
	}
	if err := s.db().WithContext(ctx).
		Model(&storage.Segment{}).
		Where("session_id = ?", sessionID).
		Select("COALESCE(MAX(seq), -1) AS max_seq").
		Scan(&v).Error; err != nil {
		return -1, apierr.Wrap(err, apierr.CodeDBWrite)
	}
	return v.MaxSeq, nil
}

// toSegmentRow 把 GORM 行转成领域行。
func toSegmentRow(r storage.Segment) transcribe.SegmentRow {
	return transcribe.SegmentRow{
		ID:         r.ID,
		SessionID:  r.SessionID,
		Seq:        r.Seq,
		StartMs:    r.StartMs,
		EndMs:      r.EndMs,
		Text:       r.Text,
		SpeakerID:  r.SpeakerID,
		Confidence: float32(r.Confidence),
		EngineID:   r.EngineID,
		IsFinal:    r.IsFinal,
		Edited:     r.Edited,
	}
}

// 编译期断言：store 必须满足 transcribe.Store。
var _ transcribe.Store = (*store)(nil)
