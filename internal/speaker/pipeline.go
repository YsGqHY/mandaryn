package speaker

import (
	"context"
	"errors"

	"gorm.io/gorm"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/storage"
	"vip.oberon.mandaryn/internal/utils/logx"
)

// 「对某个会话跑说话人分离」的编排（FR-06-1 ~ FR-06-3）。
//
// 全流程：读片段区间 → 逐片段取音频 → 提取声纹 → 会话内聚类 → 逐个簇做跨会话归并
// → 关联角色 → 落库（speakers / session_speakers / segments.speaker_id）。
//
// 三条不可妥协的性质：
//  1. **幂等重跑**：重跑先把本会话的映射与片段身份清空再写，行数不翻倍；
//  2. **可取消**：每处理一个片段检查 ctx，取消后整体回滚（事务未提交，库不变）；
//  3. **诚实降级**：提取器 / 音频端口缺失时返回明确错误码，绝不 panic、绝不假装成功。
func (r *Repo) Run(ctx context.Context, sessionID string) (contract.DiarizeResult, error) {
	sid := trimID(sessionID)
	if sid == "" {
		return contract.DiarizeResult{}, errInvalid("sessionId 不能为空")
	}
	// 降级路径（用户可见的诚实提示，不是 panic，也不是「假装 0 个说话人」）。
	if r.extractor == nil {
		return contract.DiarizeResult{}, errExtractorUnavailable(
			"本机未装配声纹提取器（未安装声纹模型包），说话人分离不可用；角色改名等操作不受影响")
	}
	if r.audio == nil {
		return contract.DiarizeResult{}, errExtractorUnavailable(
			"未装配片段音频读取端口，无法提取声纹")
	}
	if !r.acquire(sid) {
		return contract.DiarizeResult{}, apierr.New(apierr.CodeStateForbidden).
			WithAction("wait-diarize").
			WithDetail("该会话的说话人分离正在进行中")
	}
	defer r.release(sid)

	// 取消要在**任何查询之前**判一次：否则 GORM 会把 context.Canceled 包成
	// ERR_DB_WRITE，让「用户取消」看起来像数据库故障。
	if err := ctx.Err(); err != nil {
		return contract.DiarizeResult{}, errCancelled(err)
	}

	db := r.db(ctx)
	var sess storage.Session
	if err := db.First(&sess, "id = ?", sid).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return contract.DiarizeResult{}, errNotFound("会话不存在：%s", sid)
		}
		return contract.DiarizeResult{}, wrapWrite(err)
	}

	var segs []storage.Segment
	if err := db.Where("session_id = ? AND is_final = ?", sid, true).
		Order("seq ASC").Find(&segs).Error; err != nil {
		return contract.DiarizeResult{}, wrapWrite(err)
	}

	modelID := r.extractor.ID()
	result := contract.DiarizeResult{SessionID: sid, SegmentCount: len(segs), ModelID: modelID}
	if len(segs) == 0 {
		// 未转写的会话没有任何片段：不是故障，返回零结果让 UI 显示「先转写」。
		return result, nil
	}

	// —— 步骤 1：逐片段提取声纹 ——
	vectors := make([]SegmentVector, 0, len(segs))
	var firstErr error
	for _, seg := range segs {
		if err := ctx.Err(); err != nil {
			return result, errCancelled(err)
		}
		if spanMs(seg.StartMs, seg.EndMs) < r.minSpeechMs {
			// ARCH §4.4：过短片段的 embedding 不可靠，直接归「未识别」。
			result.TooShortSegments++
			continue
		}
		samples, err := r.audio.LoadSegment(ctx, sid, seg.StartMs, seg.EndMs)
		if err != nil {
			if cerr := ctx.Err(); cerr != nil {
				return result, errCancelled(cerr)
			}
			// 单条片段读不出来（文件被截断 / 区间越界）：跳过该句，不打断整场。
			result.FailedSegments++
			logx.For("speaker").Warn("片段音频不可读，跳过声纹提取",
				"sessionId", sid, "segmentId", seg.ID, "err", codeOnly(err))
			continue
		}
		if len(samples) == 0 {
			result.FailedSegments++
			continue
		}
		v, err := r.extractor.Embed(samples, SampleRate)
		if err != nil {
			result.FailedSegments++
			if firstErr == nil {
				firstErr = err
			}
			// 只记维度与错误码，**不记向量内容**（NFR-03-5）。
			logx.For("speaker").Warn("声纹提取失败",
				"sessionId", sid, "segmentId", seg.ID, "err", codeOnly(err))
			continue
		}
		if len(v) == 0 {
			result.FailedSegments++
			continue
		}
		vectors = append(vectors, SegmentVector{
			SegmentID: seg.ID,
			Seq:       seg.Seq,
			StartMs:   seg.StartMs,
			EndMs:     seg.EndMs,
			Vector:    v,
		})
	}
	if len(vectors) == 0 {
		if firstErr != nil {
			// 全部片段都提不出声纹（模型加载失败 / 音频格式不支持）：
			// 把第一个真实错误抛出去，让用户看到原因而不是「0 个说话人」。
			return result, apierr.From(firstErr, apierr.CodeLocalUnavailable)
		}
		return result, nil
	}

	// —— 步骤 2：会话内聚类 ——
	maxSpk := r.maxSpeakers(ctx)
	part := ClusterVectors(vectors, ClusterOptions{Threshold: r.clusterThreshold, MaxSpeakers: maxSpk})

	// —— 步骤 3：跨会话归并 + 落库（单事务，失败整体回滚）——
	identities, err := r.loadIdentities(ctx)
	if err != nil {
		return result, err
	}
	SortIdentities(identities)

	now := r.nowMs()
	err = db.Transaction(func(tx *gorm.DB) error {
		// 先快照本会话既有的「身份 → 角色」映射：重跑**不能丢掉用户的手工指派**
		// （否则每次重跑都会新建一批角色，用户之前的命名就白做了）。
		prevRole := make(map[string]string, 8)
		{
			var links []storage.SessionSpeaker
			if err := tx.Where("session_id = ?", sid).Find(&links).Error; err != nil {
				return wrapWrite(err)
			}
			for _, l := range links {
				prevRole[l.SpeakerID] = l.RoleID
			}
		}

		// 幂等重置：本会话旧映射全删、片段身份清空（重跑覆盖，不重复插入）。
		//
		// 注意这里删的是 `session_speakers` 与片段的 `speaker_id`，
		// **不动** `speakers` / `roles`：那是跨会话资产，重跑不该丢失用户的命名。
		if err := tx.Where("session_id = ?", sid).Delete(&storage.SessionSpeaker{}).Error; err != nil {
			return wrapWrite(err)
		}
		if err := tx.Model(&storage.Segment{}).Where("session_id = ?", sid).
			Update("speaker_id", "").Error; err != nil {
			return wrapWrite(err)
		}

		touched := make([]string, 0, len(part.Clusters))
		for _, cl := range part.Clusters {
			if err := ctx.Err(); err != nil {
				return errCancelled(err)
			}
			speakerID, err := r.bindCluster(tx, identities, cl, modelID, now)
			if err != nil {
				return err
			}
			touched = append(touched, speakerID)

			// 角色解析顺序：本会话旧映射 → 该身份在其它会话的映射 → 新建。
			roleID := prevRole[speakerID]
			if roleID == "" {
				roleID, err = roleOfSpeakerTx(tx, speakerID)
				if err != nil {
					return err
				}
			}
			if roleID == "" {
				role, err := r.createRoleTx(tx)
				if err != nil {
					return err
				}
				roleID = role.ID
			}

			// 会话内映射（句数 / 时长按该簇实际成员统计）。
			if err := tx.Create(&storage.SessionSpeaker{
				SessionID:      sid,
				SpeakerID:      speakerID,
				RoleID:         roleID,
				UtteranceCount: len(cl.Members),
				SpeechMs:       cl.SpeechMs,
			}).Error; err != nil {
				return wrapWrite(err)
			}
			// 片段只写身份，不写名字（铁律 1）。
			if err := tx.Model(&storage.Segment{}).
				Where("session_id = ? AND id IN ?", sid, cl.Members).
				Update("speaker_id", speakerID).Error; err != nil {
				return wrapWrite(err)
			}
		}

		for _, spk := range uniqueStrings(touched) {
			if err := recomputeSpeakerCountersTx(tx, spk); err != nil {
				return err
			}
		}
		if err := recomputeSessionSpeakerCountTx(tx, sid); err != nil {
			return err
		}
		result.SpeakerCount = len(part.Clusters)
		result.AssignedSegments = len(part.LabelOf)
		return nil
	})
	if err != nil {
		return result, err
	}

	result.UnidentifiedSegments = result.SegmentCount - result.AssignedSegments
	// 日志只写元数据：片段数 / 说话人数 / 模型 ID（不含音频、不含向量）。
	logx.For("speaker").Info("说话人分离完成",
		"sessionId", sid, "segments", result.SegmentCount, "speakers", result.SpeakerCount,
		"unidentified", result.UnidentifiedSegments, "tooShort", result.TooShortSegments,
		"failed", result.FailedSegments, "maxSpeakers", maxSpk, "modelId", modelID)
	return result, nil
}

// bindCluster 把一个簇对应到一个全局声纹身份：命中既有身份则融合质心，
// 否则新建身份。返回声纹身份 ID。
//
// 命中判定用 `MergeThreshold`（ARCH §4.4 的 0.72，闭区间）。
func (r *Repo) bindCluster(tx *gorm.DB, identities []GlobalIdentity, cl Cluster,
	modelID string, now int64) (string, error) {

	idx, sim := MatchGlobal(identities, cl.Centroid, modelID)
	if idx >= 0 && sim >= r.mergeThreshold {
		e := &identities[idx]
		// 质心按成员数加权融合，避免单句片段主导既有质心。
		MergeCentroid(e, cl.Centroid, len(cl.Members))
		e.LastSeenAt = now
		enc, err := EncodeVector(e.Vector)
		if err != nil {
			return "", err
		}
		if err := tx.Model(&storage.Speaker{}).Where("id = ?", e.SpeakerID).
			Updates(map[string]any{
				"embedding_enc": enc,
				"dim":           len(e.Vector),
				"last_seen_at":  now,
			}).Error; err != nil {
			return "", wrapWrite(err)
		}
		return e.SpeakerID, nil
	}

	// 未命中：新建声纹身份（跨会话的新面孔）。
	id := newSpeakerID(r.now())
	enc, err := EncodeVector(cl.Centroid)
	if err != nil {
		return "", err
	}
	row := storage.Speaker{
		ID:            id,
		EmbeddingEnc:  enc,
		Dim:           len(cl.Centroid),
		ModelID:       modelID,
		FirstSeenAt:   now,
		LastSeenAt:    now,
		SessionCount:  1,
		TotalSpeechMs: cl.SpeechMs,
	}
	if err := tx.Create(&row).Error; err != nil {
		return "", wrapWrite(err)
	}
	return id, nil
}

// loadIdentities 读取全部声纹身份（TBL-05）并解密向量。
//
// 全量加载的理由：向量是**加密**落库的，无法做 SQL 相似度检索（ARCH §4.4）。
// 规模假设单用户 ≤ 5000 条 ≈ 5MB，可接受。
//
// 解密失败的身份被跳过（保留其行，只是不参与归并）：换机器后主密钥变化会让
// 所有旧向量不可解，此时宁可「新建身份」也不要让整次分离失败。
func (r *Repo) loadIdentities(ctx context.Context) ([]GlobalIdentity, error) {
	var rows []storage.Speaker
	if err := r.db(ctx).Order("id ASC").Find(&rows).Error; err != nil {
		return nil, wrapWrite(err)
	}
	out := make([]GlobalIdentity, 0, len(rows))
	failed := 0
	for _, row := range rows {
		id := GlobalIdentity{
			SpeakerID:     row.ID,
			ModelID:       row.ModelID,
			Weight:        1,
			SessionCount:  row.SessionCount,
			TotalSpeechMs: row.TotalSpeechMs,
			FirstSeenAt:   row.FirstSeenAt,
			LastSeenAt:    row.LastSeenAt,
		}
		v, err := DecodeVector(row.EmbeddingEnc, row.Dim)
		if err != nil {
			failed++
			continue
		}
		id.Vector = Normalize(v)
		out = append(out, id)
	}
	if failed > 0 {
		// 只记数量：解密失败可能意味着数据目录被换过 / 主密钥丢失。
		logx.For("speaker").Warn("部分声纹向量无法解密，本次不参与归并", "failed", failed, "total", len(rows))
	}
	return out, nil
}

// acquire 抢占该会话的分离资格；已有任务在跑时返回 false。
func (r *Repo) acquire(sessionID string) bool {
	r.runMu.Lock()
	defer r.runMu.Unlock()
	if r.running == nil {
		r.running = make(map[string]struct{})
	}
	key := runKey(sessionID)
	if _, busy := r.running[key]; busy {
		return false
	}
	r.running[key] = struct{}{}
	return true
}

// release 释放分离资格。
func (r *Repo) release(sessionID string) {
	r.runMu.Lock()
	defer r.runMu.Unlock()
	delete(r.running, runKey(sessionID))
}

// errCancelled 把 context 取消映射成错误。
//
// 用 ERR_STATE_FORBIDDEN 而不是通用错误：语义是「这次操作没做完（用户取消）」，
// 前端据此显示「已取消」而不是「失败」。
func errCancelled(err error) *apierr.Err {
	return apierr.New(apierr.CodeStateForbidden).
		WithAction("retry").
		WithDetail("说话人分离已取消").
		WithCause(err)
}

// codeOnly 从错误里只取错误码 / 首行，避免把文件路径或音频信息写进日志。
func codeOnly(err error) string {
	if err == nil {
		return ""
	}
	var e *apierr.Err
	if errors.As(err, &e) {
		return e.Code
	}
	return "internal"
}
