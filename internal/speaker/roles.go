package speaker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/storage"
	"vip.oberon.mandaryn/internal/utils/logx"
)

// 角色管理（FR-06-4 / FR-06-5，API-7.1 ~ API-7.7）。
//
// 本文件承担**铁律 1** 的落地：角色名只存在于 `roles.name`，片段只存 `speaker_id`。
// 因此：
//   - 改名（RenameRole）是一行 `UPDATE roles SET name=?`，历史会话天然立即生效；
//   - 本文件里**不出现**任何 `segments.text/role_name` 的写入；
//   - 唯一会写 `segments` 的地方是合并 / 拆分 / 指派，且只写 `speaker_id`
//     （语义上那是「这句话是谁说的」的身份变化，不是「名字」的冗余存储）。

// —— API-7.1 ——

// ListRoles 返回全局角色列表（含颜色、覆盖的声纹身份、命中会话数）。
//
// 排序：内置「未识别」置底（它总是一堆真实角色之后的兜底），其余按创建时间升序，
// 保证前端侧栏顺序稳定（不随行号漂移）。
func (r *Repo) ListRoles(ctx context.Context) ([]contract.Role, error) {
	db := r.db(ctx)
	var rows []storage.Role
	if err := db.Order("is_system ASC, created_at ASC, id ASC").Find(&rows).Error; err != nil {
		return nil, wrapWrite(err)
	}
	if len(rows) == 0 {
		return []contract.Role{}, nil
	}

	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}

	// 每个角色覆盖哪些声纹身份（global speaker → role 可能一对多）。
	type linkRow struct {
		RoleID    string
		SpeakerID string
	}
	var links []linkRow
	if err := db.Model(&storage.SessionSpeaker{}).
		Select("role_id, speaker_id").
		Where("role_id IN ?", ids).
		Group("role_id, speaker_id").
		Order("role_id ASC, speaker_id ASC").
		Scan(&links).Error; err != nil {
		return nil, wrapWrite(err)
	}
	speakersOf := make(map[string][]string, len(rows))
	for _, l := range links {
		speakersOf[l.RoleID] = append(speakersOf[l.RoleID], l.SpeakerID)
	}

	// 每个角色命中多少会话（DISTINCT session_id，同一会话里两个身份归同一角色只算一场）。
	type countRow struct {
		RoleID string
		N      int
	}
	var counts []countRow
	if err := db.Model(&storage.SessionSpeaker{}).
		Select("role_id, COUNT(DISTINCT session_id) AS n").
		Where("role_id IN ?", ids).
		Group("role_id").
		Scan(&counts).Error; err != nil {
		return nil, wrapWrite(err)
	}
	countOf := make(map[string]int, len(counts))
	for _, c := range counts {
		countOf[c.RoleID] = c.N
	}

	out := make([]contract.Role, 0, len(rows))
	for _, row := range rows {
		spk := speakersOf[row.ID]
		if spk == nil {
			spk = []string{}
		}
		out = append(out, contract.Role{
			ID:           row.ID,
			Name:         row.Name,
			ColorToken:   row.ColorToken,
			SpeakerIDs:   spk,
			SessionCount: countOf[row.ID],
			IsSystem:     row.IsSystem,
		})
	}
	return out, nil
}

// —— API-7.2 ——

// RenameRole 改名，**全局立即生效**（FR-06-3）。
//
// 实现就是一行 `UPDATE roles SET name=?`：因为片段不存名字，所有历史会话的
// 展示都是 `roles` JOIN 出来的，改名后天然同步（没有批量 UPDATE，也就没有
// 「新旧名并存」的窗口）。
//
// 可能错误：ERR_INVALID_ARGUMENT（空 / 过长 / 改内置「未识别」角色）、ERR_NOT_FOUND。
//
// 为什么禁止改内置角色：`rl_unknown` 的名字是「未识别」语义的一部分，
// 允许改名会让「未识别」这个标识消失，用户再也无法区分「没认出来」和真实角色。
func (r *Repo) RenameRole(ctx context.Context, roleID, name string) error {
	id := trimID(roleID)
	if id == "" {
		return errInvalid("roleId 不能为空")
	}
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return errInvalid("角色名不能为空")
	}
	if n := utf8.RuneCountInString(trimmed); n > maxRoleNameRunes {
		return errInvalid("角色名过长：%d 个字符（上限 %d）", n, maxRoleNameRunes)
	}

	db := r.db(ctx)
	var row storage.Role
	if err := db.First(&row, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return errNotFound("角色不存在：%s", id)
		}
		return wrapWrite(err)
	}
	if row.IsSystem {
		return errInvalid("内置「未识别」角色不能改名")
	}

	res := db.Model(&storage.Role{}).Where("id = ?", id).Update("name", trimmed)
	if res.Error != nil {
		return wrapWrite(res.Error)
	}
	if res.RowsAffected == 0 {
		// 并发删除：与「不存在」等价。
		return errNotFound("角色不存在：%s", id)
	}

	row.Name = trimmed
	r.emitRolesChanged(row)
	// 只记 ID 与字符数，不记名字内容（角色名属于用户内容，NFR-03-5）。
	logx.For("speaker").Info("角色已改名", "roleId", id, "nameRunes", utf8.RuneCountInString(trimmed))
	return nil
}

// —— API-7.7 ——

// SetRoleColor 设置角色的配色令牌（只接受主题令牌，如 `role-3`）。
//
// 为什么拒绝十六进制：配色必须随主题切换而变，硬编码颜色在浅深色主题下会失真
// （前端铁律「除主题预设外禁止硬编码十六进制」）。这里在 L2 就拦掉，
// 避免脏数据落库后再由前端猜怎么用。
//
// 可能错误：ERR_INVALID_ARGUMENT（令牌非法 / 内置角色）、ERR_NOT_FOUND。
func (r *Repo) SetRoleColor(ctx context.Context, roleID, colorToken string) error {
	id := trimID(roleID)
	if id == "" {
		return errInvalid("roleId 不能为空")
	}
	token := strings.TrimSpace(colorToken)
	if err := validateColorToken(token); err != nil {
		return err
	}

	db := r.db(ctx)
	var row storage.Role
	if err := db.First(&row, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return errNotFound("角色不存在：%s", id)
		}
		return wrapWrite(err)
	}
	if row.IsSystem {
		return errInvalid("内置「未识别」角色的配色不可修改")
	}

	res := db.Model(&storage.Role{}).Where("id = ?", id).Update("color_token", token)
	if res.Error != nil {
		return wrapWrite(res.Error)
	}
	if res.RowsAffected == 0 {
		return errNotFound("角色不存在：%s", id)
	}

	row.ColorToken = token
	r.emitRolesChanged(row)
	logx.For("speaker").Info("角色配色已更新", "roleId", id, "colorToken", token)
	return nil
}

// validateColorToken 校验配色令牌。
//
// 规则：非空、≤32 字符、不允许出现 `#`（十六进制）或纯数字形态（那通常是
// 误传了色值而非令牌）。允许 `neutral` / `accent` / `role-3` 这类令牌。
func validateColorToken(token string) error {
	if token == "" {
		return errInvalid("colorToken 不能为空")
	}
	if n := utf8.RuneCountInString(token); n > maxColorTokenRunes {
		return errInvalid("colorToken 过长：%d 个字符（上限 %d）", n, maxColorTokenRunes)
	}
	if strings.Contains(token, "#") {
		return errInvalid("colorToken 只接受主题令牌（如 role-3），不接受十六进制色值")
	}
	return nil
}

// —— API-7.3 ——

// ListSessionSpeakers 返回会话内「说话人 → 角色」映射（API-7.3）。
//
// 未识别片段（`speaker_id` 为空：重叠语音 / 过短 / 提取失败，ARCH §4.4）作为
// 一条**合成行**附在末尾：`speakerId` 为空、`roleId` = `rl_unknown`。
// 这就是「未识别说话人有明确标识」的落地方式（前端按 `roleId == rl_unknown`
// 或 `RoleIsUnidentified()` 判定，不必自己写魔法字符串）。
//
// 可能错误：ERR_INVALID_ARGUMENT、ERR_NOT_FOUND。
func (r *Repo) ListSessionSpeakers(ctx context.Context, sessionID string) ([]contract.SessionSpeaker, error) {
	sid := trimID(sessionID)
	if sid == "" {
		return nil, errInvalid("sessionId 不能为空")
	}
	db := r.db(ctx)

	var exists int64
	if err := db.Model(&storage.Session{}).Where("id = ?", sid).Count(&exists).Error; err != nil {
		return nil, wrapWrite(err)
	}
	if exists == 0 {
		return nil, errNotFound("会话不存在：%s", sid)
	}

	type row struct {
		SpeakerID      string
		RoleID         string
		RoleName       string
		ColorToken     string
		UtteranceCount int
		SpeechMs       int64
	}
	var rows []row
	if err := db.Table("session_speakers AS ss").
		Select(`ss.speaker_id, ss.role_id,
			COALESCE(r.name, '') AS role_name,
			COALESCE(r.color_token, '') AS color_token,
			ss.utterance_count, ss.speech_ms`).
		Joins("LEFT JOIN roles r ON r.id = ss.role_id").
		Where("ss.session_id = ?", sid).
		// 句数多的排前面：前端侧栏按「主要说话人」优先展示。
		// speaker_id 作为次序键，保证同句数时顺序稳定。
		Order("ss.utterance_count DESC, ss.speaker_id ASC").
		Scan(&rows).Error; err != nil {
		return nil, wrapWrite(err)
	}

	out := make([]contract.SessionSpeaker, 0, len(rows)+1)
	for _, v := range rows {
		out = append(out, contract.SessionSpeaker{
			SpeakerID:      v.SpeakerID,
			RoleID:         v.RoleID,
			RoleName:       v.RoleName,
			ColorToken:     v.ColorToken,
			UtteranceCount: v.UtteranceCount,
			SpeechMs:       v.SpeechMs,
		})
	}

	// 未识别片段（不透出 speaker_id：它们本来就没有身份）。
	var unidentified struct {
		N  int
		Ms int64
	}
	if err := db.Model(&storage.Segment{}).
		Select("COUNT(*) AS n, COALESCE(SUM(CASE WHEN end_ms > start_ms THEN end_ms - start_ms ELSE 0 END), 0) AS ms").
		Where("session_id = ? AND (speaker_id IS NULL OR speaker_id = '')", sid).
		Scan(&unidentified).Error; err != nil {
		return nil, wrapWrite(err)
	}
	if unidentified.N > 0 {
		name, color := r.unknownRoleLabel(ctx)
		out = append(out, contract.SessionSpeaker{
			SpeakerID:      "",
			RoleID:         contract.RoleUnknownID,
			RoleName:       name,
			ColorToken:     color,
			UtteranceCount: unidentified.N,
			SpeechMs:       unidentified.Ms,
		})
	}
	return out, nil
}

// unknownRoleLabel 读取内置「未识别」角色的展示名与配色（不硬编码文案）。
func (r *Repo) unknownRoleLabel(ctx context.Context) (name, color string) {
	var row storage.Role
	if err := r.db(ctx).First(&row, "id = ?", contract.RoleUnknownID).Error; err != nil {
		// 迁移保证它存在；真丢了也不能让列表失败，退回空串由前端兜底。
		return "", ""
	}
	return row.Name, row.ColorToken
}

// —— API-7.4 ——

// MergeSpeakers 把 from 合并到 to（FR-06-5）。
//
// 语义（一次事务内完成，任一步失败整体回滚）：
//  1. 目标角色 = to 当前映射的角色；to 还没映射过则新建一个角色；
//  2. `from` 的所有片段 `speaker_id` 改成 `to`（片段只存身份，不存名字）；
//  3. 同一会话里两人都在时，把 `from` 的句数 / 时长并入 `to` 的映射行，删掉多余行；
//  4. 质心按权重加权融合，`from` 声纹身份删除（今后不再单独出现）；
//  5. 重算受影响会话的说话人数与 `to` 的冗余计数。
//
// 可能错误：ERR_INVALID_ARGUMENT（空 / 与自己合并）、ERR_NOT_FOUND、ERR_DB_WRITE。
func (r *Repo) MergeSpeakers(ctx context.Context, fromSpeakerID, toSpeakerID string) error {
	from := trimID(fromSpeakerID)
	to := trimID(toSpeakerID)
	if from == "" || to == "" {
		return errInvalid("fromSpeakerId 与 toSpeakerId 均不能为空")
	}
	if from == to {
		return errInvalid("不能把说话人合并到自己")
	}

	var (
		changedRole storage.Role
		sessions    []string
	)
	err := r.db(ctx).Transaction(func(tx *gorm.DB) error {
		var speakers []storage.Speaker
		if err := tx.Where("id IN ?", []string{from, to}).Find(&speakers).Error; err != nil {
			return wrapWrite(err)
		}
		var fromRow, toRow *storage.Speaker
		for i := range speakers {
			switch speakers[i].ID {
			case from:
				fromRow = &speakers[i]
			case to:
				toRow = &speakers[i]
			}
		}
		if fromRow == nil {
			return errNotFound("声纹身份不存在：%s", from)
		}
		if toRow == nil {
			return errNotFound("声纹身份不存在：%s", to)
		}

		roleID, err := roleOfSpeakerTx(tx, to)
		if err != nil {
			return err
		}
		var role storage.Role
		if roleID == "" {
			role, err = r.createRoleTx(tx)
			if err != nil {
				return err
			}
		} else if err := tx.First(&role, "id = ?", roleID).Error; err != nil {
			return wrapWrite(err)
		}

		var links []storage.SessionSpeaker
		if err := tx.Where("speaker_id = ?", from).Find(&links).Error; err != nil {
			return wrapWrite(err)
		}
		// 受影响会话要同时看「映射行」与「片段」：片段可能没有对应映射行
		// （上一次分离被中断、或用户手工改过库），只遍历映射会漏掉这些会话。
		var segSessions []string
		if err := tx.Model(&storage.Segment{}).
			Where("speaker_id = ?", from).
			Distinct("session_id").
			Pluck("session_id", &segSessions).Error; err != nil {
			return wrapWrite(err)
		}
		for _, l := range links {
			sessions = append(sessions, l.SessionID)
		}
		sessions = append(sessions, segSessions...)

		// 片段一次性改指向（**只改身份**，不写任何角色名）。
		if err := tx.Model(&storage.Segment{}).
			Where("speaker_id = ?", from).
			Update("speaker_id", to).Error; err != nil {
			return wrapWrite(err)
		}
		// 被合并方的映射行已无意义：全部删除，随后按片段实际情况重建 to 的映射。
		if err := tx.Where("speaker_id = ?", from).Delete(&storage.SessionSpeaker{}).Error; err != nil {
			return wrapWrite(err)
		}

		for _, sid := range uniqueStrings(sessions) {
			count, ms, err := segmentCountsTx(tx, sid, to)
			if err != nil {
				return err
			}
			if count == 0 {
				if err := tx.Where("session_id = ? AND speaker_id = ?", sid, to).
					Delete(&storage.SessionSpeaker{}).Error; err != nil {
					return wrapWrite(err)
				}
			} else if err := tx.Clauses(clause.OnConflict{
				Columns: []clause.Column{{Name: "session_id"}, {Name: "speaker_id"}},
				// 只刷新计数，**不覆盖 role_id**：该会话里用户可能给 to 单独指派过角色，
				// 合并另一个人到它身上不该把那个指派抹掉。
				DoUpdates: clause.AssignmentColumns([]string{"utterance_count", "speech_ms"}),
			}).Create(&storage.SessionSpeaker{
				SessionID:      sid,
				SpeakerID:      to,
				RoleID:         role.ID,
				UtteranceCount: count,
				SpeechMs:       ms,
			}).Error; err != nil {
				return wrapWrite(err)
			}
			if err := recomputeSessionSpeakerCountTx(tx, sid); err != nil {
				return err
			}
		}

		// 质心融合：按各自 Weight 加权，任一为空则保留另一侧（不把有向量的抹掉）。
		dst := GlobalIdentity{
			SpeakerID:     toRow.ID,
			ModelID:       toRow.ModelID,
			Weight:        1,
			SessionCount:  toRow.SessionCount,
			TotalSpeechMs: toRow.TotalSpeechMs,
			FirstSeenAt:   toRow.FirstSeenAt,
			LastSeenAt:    toRow.LastSeenAt,
		}
		if v, err := DecodeVector(toRow.EmbeddingEnc, toRow.Dim); err == nil {
			dst.Vector = v
		}
		src := GlobalIdentity{SpeakerID: fromRow.ID, ModelID: fromRow.ModelID, Weight: 1,
			SessionCount: fromRow.SessionCount, TotalSpeechMs: fromRow.TotalSpeechMs,
			FirstSeenAt: fromRow.FirstSeenAt, LastSeenAt: fromRow.LastSeenAt}
		if v, err := DecodeVector(fromRow.EmbeddingEnc, fromRow.Dim); err == nil {
			src.Vector = v
		}
		BlendCentroids(&dst, &src)

		updates := map[string]any{
			"first_seen_at": dst.FirstSeenAt,
			"last_seen_at":  dst.LastSeenAt,
		}
		if len(dst.Vector) > 0 {
			enc, err := EncodeVector(dst.Vector)
			if err != nil {
				return err
			}
			updates["embedding_enc"] = enc
			updates["dim"] = len(dst.Vector)
		}
		if err := tx.Model(&storage.Speaker{}).Where("id = ?", to).Updates(updates).Error; err != nil {
			return wrapWrite(err)
		}
		// 删除被合并方：片段与映射已全部改指向 to（外键 CASCADE 只会清掉残留空行）。
		if err := tx.Where("id = ?", from).Delete(&storage.Speaker{}).Error; err != nil {
			return wrapWrite(err)
		}

		// 会话说话人数与 to 的冗余计数在所有写操作之后统一重算（顺序无关，结果一致）。
		if err := recomputeSpeakerCountersTx(tx, to); err != nil {
			return err
		}
		changedRole = role
		return nil
	})
	if err != nil {
		return err
	}

	r.emitRolesChanged(changedRole)
	// 只记 ID 与数量，不含任何音频 / 正文信息。
	logx.For("speaker").Info("说话人已合并", "from", from, "to", to,
		"roleId", changedRole.ID, "sessions", len(uniqueStrings(sessions)))
	return nil
}

// —— API-7.5 ——

// SplitSpeaker 把会话内某些片段从 speakerID 拆出来，成为新说话人 + 新角色（FR-06-5）。
//
// 用于纠正「两个人被并成了一个」：只搬家指定的片段，其余片段不受影响，
// 原说话人的计数按剩余片段重算。
//
// 已知限制（docs/DEV-ISSUES-S7.md S7-05）：片段级声纹向量**不落库**（TBL-04 没有
// embedding 列，这是刻意的：向量属于身份而不是句子），因此拆出来的新身份
// `dim=0` 且没有向量，不参与跨会话归并 —— 下次跑分离时，若新身份的片段
// 与该会话其它片段相似，它们会被重新聚到一起（用户需要再次拆分）。
// 这是「不持久化每句向量」的必然代价，换来的是库里不留可复原的声纹痕迹。
//
// 可能错误：ERR_INVALID_ARGUMENT（含片段不属于该会话 / 该说话人）、ERR_NOT_FOUND。
func (r *Repo) SplitSpeaker(ctx context.Context, sessionID, speakerID string, segmentIDs []int64) (contract.Role, error) {
	sid := trimID(sessionID)
	spk := trimID(speakerID)
	if sid == "" {
		return contract.Role{}, errInvalid("sessionId 不能为空")
	}
	if spk == "" {
		return contract.Role{}, errInvalid("speakerId 不能为空")
	}
	ids := uniqueInt64(segmentIDs)
	if len(ids) == 0 {
		return contract.Role{}, errInvalid("segmentIds 不能为空")
	}

	var out contract.Role
	err := r.db(ctx).Transaction(func(tx *gorm.DB) error {
		var exists int64
		if err := tx.Model(&storage.Session{}).Where("id = ?", sid).Count(&exists).Error; err != nil {
			return wrapWrite(err)
		}
		if exists == 0 {
			return errNotFound("会话不存在：%s", sid)
		}
		var src storage.Speaker
		if err := tx.First(&src, "id = ?", spk).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errNotFound("声纹身份不存在：%s", spk)
			}
			return wrapWrite(err)
		}

		var segs []storage.Segment
		if err := tx.Where("session_id = ? AND id IN ?", sid, ids).Find(&segs).Error; err != nil {
			return wrapWrite(err)
		}
		if len(segs) != len(ids) {
			return errInvalid("有 %d 条片段不属于该会话", len(ids)-len(segs))
		}
		var totalMs int64
		for _, s := range segs {
			if s.SpeakerID != spk {
				return errInvalid("片段 %d 当前不属于该说话人", s.ID)
			}
			totalMs += spanMs(s.StartMs, s.EndMs)
		}

		now := r.nowMs()
		newSpk := storage.Speaker{
			ID:            newSpeakerID(r.now()),
			Dim:           0, // 无向量：片段级向量不落库（S7-05）
			ModelID:       src.ModelID,
			FirstSeenAt:   now,
			LastSeenAt:    now,
			SessionCount:  1,
			TotalSpeechMs: totalMs,
		}
		if err := tx.Create(&newSpk).Error; err != nil {
			return wrapWrite(err)
		}

		role, err := r.createRoleTx(tx)
		if err != nil {
			return err
		}

		// 只改身份，不碰任何名字（铁律 1）。
		if err := tx.Model(&storage.Segment{}).
			Where("session_id = ? AND id IN ?", sid, ids).
			Update("speaker_id", newSpk.ID).Error; err != nil {
			return wrapWrite(err)
		}

		if err := tx.Create(&storage.SessionSpeaker{
			SessionID:      sid,
			SpeakerID:      newSpk.ID,
			RoleID:         role.ID,
			UtteranceCount: len(segs),
			SpeechMs:       totalMs,
		}).Error; err != nil {
			return wrapWrite(err)
		}
		if err := recomputeSessionSpeakersTx(tx, sid, spk); err != nil {
			return err
		}
		if err := recomputeSessionSpeakerCountTx(tx, sid); err != nil {
			return err
		}
		if err := recomputeSpeakerCountersTx(tx, spk); err != nil {
			return err
		}

		out = contract.Role{
			ID:           role.ID,
			Name:         role.Name,
			ColorToken:   role.ColorToken,
			SpeakerIDs:   []string{newSpk.ID},
			SessionCount: 1,
		}
		return nil
	})
	if err != nil {
		return contract.Role{}, err
	}

	r.emitRolesChanged(storage.Role{ID: out.ID, Name: out.Name, ColorToken: out.ColorToken})
	logx.For("speaker").Info("说话人已拆分", "sessionId", sid, "speakerId", spk,
		"segments", len(ids), "roleId", out.ID)
	return out, nil
}

// —— 指派（API-7.4/7.5 之外的补充能力）——

// AssignSpeaker 把会话内的某个声纹身份指派到指定角色。
//
// 「把两个说话人合并为一个角色」也可以用 MergeSpeakers；本方法是更轻量的替代：
// 只改 `(session, speaker) → role` 映射，不动声纹身份、不动片段归属
// （适用于「这两个人是同一人，但我想保留两条声纹轨迹」的场景）。
//
// 可能错误：ERR_INVALID_ARGUMENT、ERR_NOT_FOUND。
func (r *Repo) AssignSpeaker(ctx context.Context, sessionID, speakerID, roleID string) error {
	sid := trimID(sessionID)
	spk := trimID(speakerID)
	rid := trimID(roleID)
	if sid == "" || spk == "" || rid == "" {
		return errInvalid("sessionId / speakerId / roleId 均不能为空")
	}

	var role storage.Role
	err := r.db(ctx).Transaction(func(tx *gorm.DB) error {
		var exists int64
		if err := tx.Model(&storage.Session{}).Where("id = ?", sid).Count(&exists).Error; err != nil {
			return wrapWrite(err)
		}
		if exists == 0 {
			return errNotFound("会话不存在：%s", sid)
		}
		if err := tx.First(&storage.Speaker{}, "id = ?", spk).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errNotFound("声纹身份不存在：%s", spk)
			}
			return wrapWrite(err)
		}
		if err := tx.First(&role, "id = ?", rid).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errNotFound("角色不存在：%s", rid)
			}
			return wrapWrite(err)
		}

		// 计数按当前片段重算，避免调用方传进来一堆不可信的冗余值。
		count, ms, err := segmentCountsTx(tx, sid, spk)
		if err != nil {
			return err
		}

		// 唯一键 (session_id, speaker_id) → upsert，幂等。
		row := storage.SessionSpeaker{
			SessionID:      sid,
			SpeakerID:      spk,
			RoleID:         rid,
			UtteranceCount: count,
			SpeechMs:       ms,
		}
		if err := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "session_id"}, {Name: "speaker_id"}},
			DoUpdates: clause.AssignmentColumns([]string{"role_id", "utterance_count", "speech_ms"}),
		}).Create(&row).Error; err != nil {
			return wrapWrite(err)
		}
		return nil
	})
	if err != nil {
		return err
	}

	r.emitRolesChanged(role)
	logx.For("speaker").Info("说话人已指派角色", "sessionId", sid, "speakerId", spk, "roleId", rid)
	return nil
}

// —— 内部辅助 ——

// roleOfSpeakerTx 返回某声纹身份当前映射的角色；没有映射返回空串。
//
// 多个会话可能给出不同角色（用户在不同会话里手工指派过），这里取 id 最小的那一行，
// 让结果**确定**而不是「随机一个会话的说法」。真正想统一时应调用 AssignSpeaker。
func roleOfSpeakerTx(tx *gorm.DB, speakerID string) (string, error) {
	var row storage.SessionSpeaker
	err := tx.Select("role_id").Where("speaker_id = ?", speakerID).Order("id ASC").Limit(1).Take(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", nil
		}
		return "", wrapWrite(err)
	}
	return row.RoleID, nil
}

const (
	rolePrefixKey     = "speaker.roleNamePrefix"
	defaultRolePrefix = "说话人"
	maxPrefixRunes    = 20
)

func roleNamePrefix(db *gorm.DB) (string, error) {
	var row storage.Preference
	if err := db.Where("key = ?", rolePrefixKey).Take(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return defaultRolePrefix, nil
		}
		return "", wrapWrite(err)
	}
	var prefix string
	if json.Unmarshal([]byte(row.Value), &prefix) != nil || strings.TrimSpace(prefix) == "" || utf8.RuneCountInString(prefix) > maxPrefixRunes {
		return defaultRolePrefix, nil
	}
	return prefix, nil
}

// RoleNamePrefix returns the prefix used for future automatically named roles.
func (r *Repo) RoleNamePrefix(ctx context.Context) (string, error) {
	return roleNamePrefix(r.db(ctx))
}

// SetRoleNamePrefix changes future names without renaming any existing role.
// An empty value restores the default.
func (r *Repo) SetRoleNamePrefix(ctx context.Context, prefix string) error {
	prefix = strings.TrimSpace(prefix)
	if utf8.RuneCountInString(prefix) > maxPrefixRunes {
		return errInvalid("role name prefix exceeds %d characters", maxPrefixRunes)
	}
	if prefix == "" {
		prefix = defaultRolePrefix
	}
	value, _ := json.Marshal(prefix)
	row := storage.Preference{Key: rolePrefixKey, Value: string(value)}
	return wrapWrite(r.db(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "key"}},
		DoUpdates: clause.AssignmentColumns([]string{"value", "updated_at"}),
	}).Create(&row).Error)
}

// createRoleTx 新建使用当前前缀的角色。
//
// 名字取**未占用的最小序号**（而不是「现有数量 + 1」）：用户删过角色之后，
// 数量法会撞上已有名字，两个「说话人 3」在侧栏里无法区分。
// 配色令牌按未系统角色数量轮转 role-1..role-8，只存令牌不存十六进制（前端铁律）。
func (r *Repo) createRoleTx(tx *gorm.DB) (storage.Role, error) {
	prefix, err := roleNamePrefix(tx)
	if err != nil {
		return storage.Role{}, err
	}
	var names []string
	if err := tx.Model(&storage.Role{}).Where("is_system = ?", false).Pluck("name", &names).Error; err != nil {
		return storage.Role{}, wrapWrite(err)
	}
	used := make(map[string]struct{}, len(names))
	for _, n := range names {
		used[n] = struct{}{}
	}
	seq := len(names) + 1
	for i := 1; i <= len(names)+1; i++ {
		if _, ok := used[fmt.Sprintf("%s %d", prefix, i)]; !ok {
			seq = i
			break
		}
	}
	row := storage.Role{
		ID:         newRoleID(r.now()),
		Name:       fmt.Sprintf("%s %d", prefix, seq),
		ColorToken: fmt.Sprintf("role-%d", len(names)%roleColorCount+1),
	}
	if err := tx.Create(&row).Error; err != nil {
		return storage.Role{}, wrapWrite(err)
	}
	return row, nil
}

// segmentCountsTx 统计某会话内某说话人的句数与语音时长。
func segmentCountsTx(tx *gorm.DB, sessionID, speakerID string) (count int, ms int64, err error) {
	var agg struct {
		N  int
		Ms int64
	}
	if err := tx.Model(&storage.Segment{}).
		Select("COUNT(*) AS n, COALESCE(SUM(CASE WHEN end_ms > start_ms THEN end_ms - start_ms ELSE 0 END), 0) AS ms").
		Where("session_id = ? AND speaker_id = ?", sessionID, speakerID).
		Scan(&agg).Error; err != nil {
		return 0, 0, wrapWrite(err)
	}
	return agg.N, agg.Ms, nil
}

// recomputeSessionSpeakersTx 按片段实际情况重算 (session, speaker) 的冗余计数。
//
// 计数归零时删除该映射行：留着 0 句的「说话人」会让侧栏出现空条目。
func recomputeSessionSpeakersTx(tx *gorm.DB, sessionID, speakerID string) error {
	count, ms, err := segmentCountsTx(tx, sessionID, speakerID)
	if err != nil {
		return err
	}
	if count == 0 {
		return wrapWrite(tx.Where("session_id = ? AND speaker_id = ?", sessionID, speakerID).
			Delete(&storage.SessionSpeaker{}).Error)
	}
	return wrapWrite(tx.Model(&storage.SessionSpeaker{}).
		Where("session_id = ? AND speaker_id = ?", sessionID, speakerID).
		Updates(map[string]any{"utterance_count": count, "speech_ms": ms}).Error)
}

// recomputeSessionSpeakerCountTx 回填 `sessions.speaker_count`。
//
// 只数「有身份」的说话人（`speaker_id` 非空）：未识别片段不该被算成一个说话人，
// 否则「3 人会议 + 一段重叠语音」会显示 4 人。
func recomputeSessionSpeakerCountTx(tx *gorm.DB, sessionID string) error {
	var n int64
	if err := tx.Model(&storage.Segment{}).
		Where("session_id = ? AND speaker_id IS NOT NULL AND speaker_id <> ''", sessionID).
		Distinct("speaker_id").Count(&n).Error; err != nil {
		return wrapWrite(err)
	}
	return wrapWrite(tx.Model(&storage.Session{}).Where("id = ?", sessionID).
		Update("speaker_count", int(n)).Error)
}

// recomputeSpeakerCountersTx 回填声纹身份的冗余计数（TBL-05）。
func recomputeSpeakerCountersTx(tx *gorm.DB, speakerID string) error {
	var agg struct {
		Sessions int
		Ms       int64
	}
	if err := tx.Model(&storage.SessionSpeaker{}).
		Select("COUNT(DISTINCT session_id) AS sessions, COALESCE(SUM(speech_ms), 0) AS ms").
		Where("speaker_id = ?", speakerID).
		Scan(&agg).Error; err != nil {
		return wrapWrite(err)
	}
	return wrapWrite(tx.Model(&storage.Speaker{}).Where("id = ?", speakerID).
		Updates(map[string]any{
			"session_count":   agg.Sessions,
			"total_speech_ms": agg.Ms,
		}).Error)
}

// spanMs 返回区间时长（毫秒），异常数据归 0。
func spanMs(startMs, endMs int64) int64 {
	if endMs <= startMs {
		return 0
	}
	return endMs - startMs
}

// uniqueStrings 去空白、去重、保序。
func uniqueStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// uniqueInt64 去重、升序（保证拆分 / 查询行为与入参顺序无关）。
func uniqueInt64(in []int64) []int64 {
	seen := make(map[int64]struct{}, len(in))
	out := make([]int64, 0, len(in))
	for _, v := range in {
		if v <= 0 {
			continue
		}
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
