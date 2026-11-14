package recording

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/storage"
	"vip.oberon.mandaryn/internal/utils/logx"
)

// RecoverResult 是单个会话的恢复结果（启动自检日志与诊断页用）。
type RecoverResult struct {
	SessionID string
	AudioPath string
	// Status 恢复后的会话状态（interrupted，音频缺失时为 failed）。
	Status string
	// Frames 修补后按实际文件长度换算的帧数。
	Frames int64
	// DurationMs 修补后按帧计数换算的时长。
	DurationMs int64
	// SizeBytes 音频文件实际大小。
	SizeBytes int64
	// PreviousStatus 崩溃前库里的状态（recording / paused / finalizing）。
	PreviousStatus string
	// Missing 音频文件缺失（此时 Status=failed，片段保留）。
	Missing bool
	// Err 本会话恢复过程中的错误（不阻断其它会话）。
	Err error
}

// RecoverUnfinished 启动自检：扫描未正常结束的会话并修补为可回放（UC-07 / FR-03）。
//
// 「未正常结束」= 库里的 status 仍是 recording / paused / finalizing。
// 会话主状态只在 Stop 的收尾里才写成 completed，因此这类行必然意味着
// 上次进程被强杀、断电或崩溃 —— 音频文件很可能停在半帧、且 WAV 头里
// 的 data 长度还是 0（只在 Seal 回填）。
//
// 恢复动作（**只修文件与元数据，不重跑转写**）：
//  1. 按实际文件长度回填 WAV 头并裁掉半帧 → 立即可被标准播放器播放；
//  2. 状态置 interrupted，补 ended_at / duration_ms / size_bytes；
//  3. 保留音频与**已产出的片段**（segment 不删不改），事后可重转。
//
// 单个会话失败不影响其它会话（返回结果里带 Err），整个扫描失败才返回 error。
func (r *Recorder) RecoverUnfinished(ctx context.Context) ([]RecoverResult, error) {
	rows, err := r.opts.Store.ListUnfinished(ctx)
	if err != nil {
		return nil, apierr.From(err, apierr.CodeDBWrite)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	results := make([]RecoverResult, 0, len(rows))
	for i := range rows {
		res := r.repairSession(ctx, &rows[i])
		results = append(results, res)
		logx.For("recording").Info("恢复未完成会话",
			"session", res.SessionID, "previous", res.PreviousStatus, "status", res.Status,
			"durationMs", res.DurationMs, "bytes", res.SizeBytes, "missing", res.Missing)
	}
	return results, nil
}

// RecoverSession 修补**单个**会话（API-2.6）。
//
// 与 RecoverUnfinished 走同一条修补路径，区别只在入口：
//   - 启动时批量扫描（自动）；
//   - 用户在库里点「修复」（显式，传入 sessionID）。
//
// 返回修补后的会话 DTO；会话不存在返回 ERR_NOT_FOUND。
func (r *Recorder) RecoverSession(ctx context.Context, sessionID string) (contract.Session, error) {
	if sessionID == "" {
		return contract.Session{}, apierr.New(apierr.CodeInvalidArgument).
			WithDetail("sessionId 不能为空")
	}
	row, err := r.opts.Store.GetSession(ctx, sessionID)
	if err != nil {
		return contract.Session{}, apierr.From(err, apierr.CodeNotFound)
	}
	res := r.repairSession(ctx, row)
	if res.Err != nil {
		return contract.Session{}, apierr.From(res.Err, apierr.CodeDBWrite)
	}
	updated, err := r.opts.Store.GetSession(ctx, sessionID)
	if err != nil {
		return contract.Session{}, apierr.From(err, apierr.CodeDBWrite)
	}
	return SessionToContract(updated), nil
}

// repairSession 修补单个会话行 + 其音频文件。
func (r *Recorder) repairSession(ctx context.Context, row *storage.Session) RecoverResult {
	res := RecoverResult{
		SessionID:      row.ID,
		AudioPath:      row.AudioPath,
		PreviousStatus: row.Status,
		Status:         contract.StateInterrupted,
	}

	abs, pathErr := r.audioAbsPath(row.AudioPath)
	if pathErr == nil {
		var frames int64
		var rerr error
		frames, res.SizeBytes, rerr = repairAudioFile(abs)
		if rerr != nil {
			if errors.Is(rerr, os.ErrNotExist) {
				res.Missing = true
				res.Status = contract.StateFailed
			} else {
				res.Err = rerr
				res.Status = contract.StateFailed
			}
			logx.For("recording").Warn("修补音频失败", "session", row.ID,
				"missing", res.Missing, "err", rerr.Error())
		} else {
			res.Frames = frames
			res.DurationMs = FramesToMs(frames)
		}
	} else {
		res.Missing = true
		res.Status = contract.StateFailed
		res.Err = pathErr
	}

	if res.Status == contract.StateInterrupted {
		if row.DurationMs > 0 {
			// 库里已有更可信的时长（说明收尾写库失败但音频更短）：取两者较小值，
			// 避免时长大于实际音频导致回放 seek 到静默区。
			if res.DurationMs == 0 || row.DurationMs < res.DurationMs {
				res.DurationMs = row.DurationMs
			}
		}
	}

	fields := map[string]any{
		"status":      res.Status,
		"duration_ms": res.DurationMs,
		"size_bytes":  res.SizeBytes,
	}
	if row.EndedAt == 0 {
		// 崩溃的会话没有 ended_at：用 started_at + 逻辑时长推出来（显示用）。
		fields["ended_at"] = row.StartedAt + res.DurationMs
	}
	if res.Missing {
		fields["degrade_reason"] = truncateNote("音频文件缺失，仅保留已产出片段")
	}
	if err := r.opts.Store.UpdateSession(ctx, row.ID, fields); err != nil {
		res.Err = err
		logx.For("recording").Warn("恢复会话写库失败", "session", row.ID,
			"code", apierr.From(err, apierr.CodeDBWrite).Code)
		return res
	}

	// 时间轴标记：恢复过的事件必须在回放里可见（否则用户不知道哪段是崩溃点）。
	//
	// TBL-10 的 kind 枚举里没有 "interrupted"，这里复用 source-lost 并写明原因；
	// 待 DOC-04 增补 kind 后改（见 docs/DEV-ISSUES-S2.md S2-07）。
	r.appendMarker(&run{id: row.ID}, res.DurationMs, contract.MarkerSourceLost,
		"上次进程异常结束，音频已修补（可回放、可稍后转写）")
	return res
}

// audioAbsPath 把相对 dataRoot 的音频路径解析成绝对路径。
func (r *Recorder) audioAbsPath(rel string) (string, error) {
	if rel == "" {
		return "", apierr.New(apierr.CodeNotFound).WithDetail("会话没有音频文件记录")
	}
	root, err := r.opts.DataRoot()
	if err != nil || root == "" {
		return "", apierr.New(apierr.CodePathUnwritable).
			WithDetail("数据目录不可用：%v", err)
	}
	return filepath.Join(root, filepath.FromSlash(rel)), nil
}

// repairAudioFile 修补单个 WAV 文件，返回 (帧数, 文件字节数)。
//
// 文件不存在时返回 os.ErrNotExist（调用方据此区分「缺失」与「损坏」）。
func repairAudioFile(abs string) (int64, int64, error) {
	st, err := os.Stat(abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, 0, os.ErrNotExist
		}
		return 0, 0, wrapFSError(err, "读取音频文件信息失败")
	}
	frames, err := RepairWAV(abs)
	if err != nil {
		return 0, st.Size(), err
	}
	return frames, st.Size(), nil
}

// SessionToContract 把持久层会话投影为前端 DTO（API-3.x / API-2.6 共用）。
//
// 时间字段统一转 RFC3339Nano；0 值表示「未设置」，输出空串而不是
// 1970-01-01（前端会把 1970 当成真实时间显示出来）。
func SessionToContract(row *storage.Session) contract.Session {
	if row == nil {
		return contract.Session{}
	}
	return contract.Session{
		ID:                   row.ID,
		Title:                row.Title,
		SourceID:             row.SourceID,
		SourceName:           row.SourceName,
		ModelSource:          row.ModelSource,
		EngineID:             row.EngineID,
		Status:               row.Status,
		Degraded:             row.Degraded,
		StartedAt:            unixMilliToRFC3339(row.StartedAt),
		EndedAt:              unixMilliToRFC3339(row.EndedAt),
		DurationMs:           row.DurationMs,
		SegmentCount:         row.SegmentCount,
		SpeakerCount:         row.SpeakerCount,
		AudioPath:            row.AudioPath,
		SizeBytes:            row.SizeBytes,
		TranscriptionStatus:  row.TranscriptionStatus,
		TranscribeProgressMs: row.TranscribeProgressMs,
		CharCount:            row.CharCount,
	}
}

// unixMilliToRFC3339 毫秒时间戳 → RFC3339Nano 字符串（0 → 空串）。
func unixMilliToRFC3339(ms int64) string {
	if ms <= 0 {
		return ""
	}
	return time.UnixMilli(ms).Format(time.RFC3339Nano)
}
