package speaker

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/storage"
)

// TestRenameRoleGlobalSync 角色改名对历史会话立即生效，且**片段行一个字节都没变**。
//
// 这是 FR-06-3 的核心守护测试：名字必须只存在于 `roles`，改名靠 JOIN 同步。
// 如果哪天有人「顺手」把名字冗余进 segments，本测试的 reflect.DeepEqual 会立刻失败。
func TestRenameRoleGlobalSync(t *testing.T) {
	f := newFixture(t)
	f.seedSession("s_old")
	f.seedSpeaker("sp_1", voiceA1)
	f.seedRole("rl_1", "说话人 1", "role-1", false)
	f.seedSpeaker("sp_2", voiceB1)
	f.seedRole("rl_2", "说话人 2", "role-2", false)

	seg1 := f.seedSegment("s_old", 1, 0, 2000, "sp_1")
	seg2 := f.seedSegment("s_old", 2, 3000, 5000, "sp_2")
	f.seedLink("s_old", "sp_1", "rl_1", 1, 2000)
	f.seedLink("s_old", "sp_2", "rl_2", 1, 2000)

	repo := f.repo(Options{})

	// 改名前：历史会话展示的是旧名字。
	if _, name := f.roleOfSegment(seg1); name != "说话人 1" {
		t.Fatalf("改名前角色名应为「说话人 1」，实际 %q", name)
	}

	// 片段表快照（改名前后必须完全一致）。
	before := f.snapshotSegments()

	if err := repo.RenameRole(context.Background(), "rl_1", "张三"); err != nil {
		t.Fatalf("改名失败: %v", err)
	}

	// 改名后：同一片段（未做任何写入）立即展示新名字。
	roleID, name := f.roleOfSegment(seg1)
	if roleID != "rl_1" || name != "张三" {
		t.Fatalf("改名后应展示「张三」（roleId=rl_1），实际 name=%q roleId=%q", name, roleID)
	}
	// 另一个说话人不受影响。
	if _, other := f.roleOfSegment(seg2); other != "说话人 2" {
		t.Fatalf("其它角色不应受影响，实际 %q", other)
	}

	// 铁律守护：segments 表零变化。
	after := f.snapshotSegments()
	if len(before) != len(after) {
		t.Fatalf("片段行数变化：%d → %d", len(before), len(after))
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("改名不得 UPDATE segments（铁律 1 被违反）：\nbefore=%+v\nafter =%+v", before, after)
	}

	// roles 表确实被更新了（单行 UPDATE）。
	var row storage.Role
	if err := f.db.GORM.First(&row, "id = ?", "rl_1").Error; err != nil {
		t.Fatalf("读取角色失败: %v", err)
	}
	if row.Name != "张三" {
		t.Fatalf("roles.name 未更新：%q", row.Name)
	}
}

// TestRenameRoleEmitsEvent 改名成功后回调收到 EV-07 载荷素材。
func TestRenameRoleEmitsEvent(t *testing.T) {
	f := newFixture(t)
	f.seedRole("rl_1", "说话人 1", "role-1", false)

	var got []contract.RoleChanged
	repo := f.repo(Options{OnRolesChanged: func(e contract.RoleChanged) { got = append(got, e) }})

	if err := repo.RenameRole(context.Background(), "rl_1", "李四"); err != nil {
		t.Fatalf("改名失败: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("应触发 1 次事件，实际 %d", len(got))
	}
	if got[0].RoleID != "rl_1" || got[0].Name != "李四" || got[0].ColorToken != "role-1" {
		t.Fatalf("事件载荷不符：%+v", got[0])
	}
}

// TestRenameRoleValidation 参数与状态校验。
func TestRenameRoleValidation(t *testing.T) {
	f := newFixture(t)
	f.seedRole("rl_1", "说话人 1", "role-1", false)
	repo := f.repo(Options{})
	ctx := context.Background()

	cases := []struct {
		name    string
		roleID  string
		newName string
		code    string
	}{
		{"空角色 ID", "  ", "张三", apierr.CodeInvalidArgument},
		{"空名字", "rl_1", "   ", apierr.CodeInvalidArgument},
		{"名字过长", "rl_1", strings.Repeat("字", 65), apierr.CodeInvalidArgument},
		{"角色不存在", "rl_nope", "张三", apierr.CodeNotFound},
		{"内置未识别角色", contract.RoleUnknownID, "别名", apierr.CodeInvalidArgument},
	}
	for _, c := range cases {
		err := repo.RenameRole(ctx, c.roleID, c.newName)
		if !apierr.HasCode(err, c.code) {
			t.Fatalf("%s：期望 %s，实际 %v", c.name, c.code, err)
		}
	}
}

func TestRoleNamePrefixOnlyAffectsNewRoles(t *testing.T) {
	f := newFixture(t)
	f.seedRole("rl_old", "说话人 1", "role-1", false)
	f.seedRole("rl_used", "主持人 1", "role-2", false)
	repo := f.repo(Options{})
	ctx := context.Background()
	if got, err := repo.RoleNamePrefix(ctx); err != nil || got != "说话人" {
		t.Fatalf("default prefix = %q, %v", got, err)
	}
	if err := repo.SetRoleNamePrefix(ctx, " 主持人 "); err != nil {
		t.Fatal(err)
	}
	fresh := f.repo(Options{})
	if got, err := fresh.RoleNamePrefix(ctx); err != nil || got != "主持人" {
		t.Fatalf("persisted prefix = %q, %v", got, err)
	}
	newRole, err := fresh.createRoleTx(f.db.GORM)
	if err != nil || newRole.Name != "主持人 2" {
		t.Fatalf("created role = %+v, %v", newRole, err)
	}
	if err := repo.SetRoleNamePrefix(ctx, strings.Repeat("字", 21)); !apierr.HasCode(err, apierr.CodeInvalidArgument) {
		t.Fatalf("long prefix error = %v", err)
	}
	if err := repo.SetRoleNamePrefix(ctx, " "); err != nil {
		t.Fatal(err)
	}
	if got, err := fresh.RoleNamePrefix(ctx); err != nil || got != "说话人" {
		t.Fatalf("reset prefix = %q, %v", got, err)
	}
	var old storage.Role
	if err := f.db.GORM.First(&old, "id = ?", "rl_old").Error; err != nil || old.Name != "说话人 1" {
		t.Fatalf("existing role = %+v, %v", old, err)
	}
}

// TestSetRoleColorValidation 配色只接受主题令牌。
func TestSetRoleColorValidation(t *testing.T) {
	f := newFixture(t)
	f.seedRole("rl_1", "说话人 1", "role-1", false)
	repo := f.repo(Options{})
	ctx := context.Background()

	// 十六进制被拒绝（前端铁律：组件不许硬编码颜色）。
	if err := repo.SetRoleColor(ctx, "rl_1", "#ff0000"); !apierr.HasCode(err, apierr.CodeInvalidArgument) {
		t.Fatalf("十六进制应被拒，实际 %v", err)
	}
	if err := repo.SetRoleColor(ctx, "rl_1", "  "); !apierr.HasCode(err, apierr.CodeInvalidArgument) {
		t.Fatalf("空令牌应被拒，实际 %v", err)
	}
	if err := repo.SetRoleColor(ctx, contract.RoleUnknownID, "role-2"); !apierr.HasCode(err, apierr.CodeInvalidArgument) {
		t.Fatalf("内置角色配色应被拒（若不存在也应先命中 not found），实际 %v", err)
	}
	if err := repo.SetRoleColor(ctx, "rl_1", "role-3"); err != nil {
		t.Fatalf("合法令牌应成功: %v", err)
	}
	var row storage.Role
	if err := f.db.GORM.First(&row, "id = ?", "rl_1").Error; err != nil {
		t.Fatalf("读角色失败: %v", err)
	}
	if row.ColorToken != "role-3" {
		t.Fatalf("配色未落库：%q", row.ColorToken)
	}
}

// TestMergeSpeakers 合并：两说话人的片段都属于同一角色，被合并方身份消失。
func TestMergeSpeakers(t *testing.T) {
	f := newFixture(t)
	// 同一会话里两人都在。
	f.seedSession("s_1")
	f.seedSpeaker("sp_a", voiceA1)
	f.seedSpeaker("sp_b", voiceA2)
	f.seedRole("rl_a", "说话人 1", "role-1", false)
	segA1 := f.seedSegment("s_1", 1, 0, 2000, "sp_a")
	segB1 := f.seedSegment("s_1", 2, 3000, 5000, "sp_b")
	f.seedLink("s_1", "sp_a", "rl_a", 1, 2000)
	f.seedLink("s_1", "sp_b", "rl_b", 1, 2000) // sp_b 映射到一个不存在的角色：走「新建」分支也要能合并

	// 另一场会话里只有被合并方。
	f.seedSession("s_2")
	segB2 := f.seedSegment("s_2", 1, 0, 2000, "sp_b")

	repo := f.repo(Options{})
	if err := repo.MergeSpeakers(context.Background(), "sp_b", "sp_a"); err != nil {
		t.Fatalf("合并失败: %v", err)
	}

	// 片段全部改指向 sp_a。
	for _, id := range []int64{segA1, segB1, segB2} {
		if got := f.findSegment(id).SpeakerID; got != "sp_a" {
			t.Fatalf("片段 %d 应改指向 sp_a，实际 %q", id, got)
		}
	}
	// 被合并方身份已删除。
	var n int64
	if err := f.db.GORM.Model(&storage.Speaker{}).Where("id = ?", "sp_b").Count(&n).Error; err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if n != 0 {
		t.Fatalf("被合并的声纹身份应被删除，实际仍有 %d 行", n)
	}
	// 两个片段都属于同一角色（JOIN 结果一致）。
	roleID1, name1 := f.roleOfSegment(segA1)
	roleID2, name2 := f.roleOfSegment(segB1)
	if roleID1 != roleID2 || name1 != name2 || roleID1 == "" {
		t.Fatalf("合并后两片段应同角色：%q/%q vs %q/%q", roleID1, name1, roleID2, name2)
	}
	// 另一场会话的映射也指向同一角色。
	if rid, _ := f.roleOfSegment(segB2); rid != roleID1 {
		t.Fatalf("跨会话映射应同步到同一角色：%q != %q", rid, roleID1)
	}
	// 会话说话人数重算：s_1 只剩 1 人。
	var sess storage.Session
	if err := f.db.GORM.First(&sess, "id = ?", "s_1").Error; err != nil {
		t.Fatalf("读会话失败: %v", err)
	}
	if sess.SpeakerCount != 1 {
		t.Fatalf("合并后会话说话人数应为 1，实际 %d", sess.SpeakerCount)
	}
}

// TestMergeSpeakersValidation 非法参数与不存在。
func TestMergeSpeakersValidation(t *testing.T) {
	f := newFixture(t)
	f.seedSpeaker("sp_a", voiceA1)
	repo := f.repo(Options{})
	ctx := context.Background()

	if err := repo.MergeSpeakers(ctx, "sp_a", "sp_a"); !apierr.HasCode(err, apierr.CodeInvalidArgument) {
		t.Fatalf("合并到自己应被拒，实际 %v", err)
	}
	if err := repo.MergeSpeakers(ctx, "", "sp_a"); !apierr.HasCode(err, apierr.CodeInvalidArgument) {
		t.Fatalf("空 ID 应被拒，实际 %v", err)
	}
	if err := repo.MergeSpeakers(ctx, "sp_nope", "sp_a"); !apierr.HasCode(err, apierr.CodeNotFound) {
		t.Fatalf("不存在的身份应报 NOT_FOUND，实际 %v", err)
	}
}

// TestSplitSpeaker 拆分：选中的片段搬到新说话人 + 新角色，其余不动。
func TestSplitSpeaker(t *testing.T) {
	f := newFixture(t)
	f.seedSession("s_1")
	f.seedSpeaker("sp_a", voiceA1)
	f.seedRole("rl_a", "说话人 1", "role-1", false)
	seg1 := f.seedSegment("s_1", 1, 0, 2000, "sp_a")
	seg2 := f.seedSegment("s_1", 2, 3000, 5000, "sp_a")
	seg3 := f.seedSegment("s_1", 3, 6000, 8000, "sp_a")
	f.seedLink("s_1", "sp_a", "rl_a", 3, 6000)

	repo := f.repo(Options{})
	role, err := repo.SplitSpeaker(context.Background(), "s_1", "sp_a", []int64{seg2})
	if err != nil {
		t.Fatalf("拆分失败: %v", err)
	}
	if role.ID == "" || role.Name == "" || len(role.SpeakerIDs) != 1 {
		t.Fatalf("应返回新角色：%+v", role)
	}
	if role.ID == "rl_a" {
		t.Fatal("拆分应创建新角色，而不是复用原角色")
	}

	// 被拆片段指向新身份，其余仍属原身份。
	moved := f.findSegment(seg2)
	if moved.SpeakerID == "sp_a" || moved.SpeakerID == "" {
		t.Fatalf("被拆片段应改指向新身份，实际 %q", moved.SpeakerID)
	}
	if f.findSegment(seg1).SpeakerID != "sp_a" || f.findSegment(seg3).SpeakerID != "sp_a" {
		t.Fatal("未选中的片段应保持原身份")
	}
	// 归属正确：新片段展示新角色名，原有片段仍展示原角色名。
	if rid, _ := f.roleOfSegment(seg2); rid != role.ID {
		t.Fatalf("被拆片段应映射到新角色 %q，实际 %q", role.ID, rid)
	}
	if rid, _ := f.roleOfSegment(seg1); rid != "rl_a" {
		t.Fatalf("原有片段角色不应变化，实际 %q", rid)
	}
	// 原身份计数按剩余片段重算（2 句 / 4000ms）。
	var link storage.SessionSpeaker
	if err := f.db.GORM.First(&link, "session_id = ? AND speaker_id = ?", "s_1", "sp_a").Error; err != nil {
		t.Fatalf("读映射失败: %v", err)
	}
	if link.UtteranceCount != 2 || link.SpeechMs != 4000 {
		t.Fatalf("原身份计数应重算为 2 句/4000ms，实际 %d/%d", link.UtteranceCount, link.SpeechMs)
	}
	// 会话内说话人数变为 2。
	var sess storage.Session
	if err := f.db.GORM.First(&sess, "id = ?", "s_1").Error; err != nil {
		t.Fatalf("读会话失败: %v", err)
	}
	if sess.SpeakerCount != 2 {
		t.Fatalf("拆分后会话说话人数应为 2，实际 %d", sess.SpeakerCount)
	}
}

// TestSplitSpeakerValidation 片段不属于该会话 / 该说话人时拒绝。
func TestSplitSpeakerValidation(t *testing.T) {
	f := newFixture(t)
	f.seedSession("s_1")
	f.seedSession("s_2")
	f.seedSpeaker("sp_a", voiceA1)
	f.seedSpeaker("sp_b", voiceB1)
	segOwn := f.seedSegment("s_1", 1, 0, 2000, "sp_a")
	segOtherSpeaker := f.seedSegment("s_1", 2, 3000, 5000, "sp_b")
	segOtherSession := f.seedSegment("s_2", 1, 0, 2000, "sp_a")

	repo := f.repo(Options{})
	ctx := context.Background()

	if _, err := repo.SplitSpeaker(ctx, "s_1", "sp_a", nil); !apierr.HasCode(err, apierr.CodeInvalidArgument) {
		t.Fatalf("空片段列表应被拒，实际 %v", err)
	}
	if _, err := repo.SplitSpeaker(ctx, "s_1", "", []int64{segOwn}); !apierr.HasCode(err, apierr.CodeInvalidArgument) {
		t.Fatalf("空说话人应被拒，实际 %v", err)
	}
	if _, err := repo.SplitSpeaker(ctx, "s_nope", "sp_a", []int64{segOwn}); !apierr.HasCode(err, apierr.CodeNotFound) {
		t.Fatalf("会话不存在应报 NOT_FOUND，实际 %v", err)
	}
	if _, err := repo.SplitSpeaker(ctx, "s_1", "sp_nope", []int64{segOwn}); !apierr.HasCode(err, apierr.CodeNotFound) {
		t.Fatalf("身份不存在应报 NOT_FOUND，实际 %v", err)
	}
	if _, err := repo.SplitSpeaker(ctx, "s_1", "sp_a", []int64{segOtherSession}); !apierr.HasCode(err, apierr.CodeInvalidArgument) {
		t.Fatalf("跨会话片段应被拒，实际 %v", err)
	}
	if _, err := repo.SplitSpeaker(ctx, "s_1", "sp_a", []int64{segOtherSpeaker}); !apierr.HasCode(err, apierr.CodeInvalidArgument) {
		t.Fatalf("不属于该说话人的片段应被拒，实际 %v", err)
	}
}

// TestAssignSpeaker 指派：只改映射，不动片段归属。
func TestAssignSpeaker(t *testing.T) {
	f := newFixture(t)
	f.seedSession("s_1")
	f.seedSpeaker("sp_a", voiceA1)
	f.seedRole("rl_1", "说话人 1", "role-1", false)
	f.seedRole("rl_2", "张三", "role-2", false)
	seg := f.seedSegment("s_1", 1, 0, 2000, "sp_a")

	repo := f.repo(Options{})
	ctx := context.Background()

	if err := repo.AssignSpeaker(ctx, "s_1", "sp_a", "rl_2"); err != nil {
		t.Fatalf("指派失败: %v", err)
	}
	if _, name := f.roleOfSegment(seg); name != "张三" {
		t.Fatalf("指派后应展示新角色名，实际 %q", name)
	}
	// 幂等：再指派一次不产生重复映射行。
	if err := repo.AssignSpeaker(ctx, "s_1", "sp_a", "rl_1"); err != nil {
		t.Fatalf("再次指派失败: %v", err)
	}
	if n := f.count(&storage.SessionSpeaker{}); n != 1 {
		t.Fatalf("指派应 upsert，不应产生重复行，实际 %d", n)
	}

	// 校验路径。
	if err := repo.AssignSpeaker(ctx, "s_1", "sp_nope", "rl_1"); !apierr.HasCode(err, apierr.CodeNotFound) {
		t.Fatalf("身份不存在应报 NOT_FOUND，实际 %v", err)
	}
	if err := repo.AssignSpeaker(ctx, "s_1", "sp_a", "rl_nope"); !apierr.HasCode(err, apierr.CodeNotFound) {
		t.Fatalf("角色不存在应报 NOT_FOUND，实际 %v", err)
	}
}

// TestListSessionSpeakers 列表包含未识别合成行，且有明确标识。
func TestListSessionSpeakers(t *testing.T) {
	f := newFixture(t)
	f.seedSession("s_1")
	f.seedSpeaker("sp_a", voiceA1)
	f.seedRole("rl_1", "说话人 1", "role-1", false)
	f.seedSegment("s_1", 1, 0, 2000, "sp_a")
	f.seedSegment("s_1", 2, 3000, 5000, "sp_a")
	f.seedSegment("s_1", 3, 6000, 8000, "") // 未识别
	f.seedLink("s_1", "sp_a", "rl_1", 2, 4000)

	repo := f.repo(Options{})
	list, err := repo.ListSessionSpeakers(context.Background(), "s_1")
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("应有 1 个说话人 + 1 条未识别行，实际 %d 条：%+v", len(list), list)
	}
	if list[0].SpeakerID != "sp_a" || list[0].RoleName != "说话人 1" || list[0].UtteranceCount != 2 {
		t.Fatalf("首条应为 sp_a/说话人 1/2 句，实际 %+v", list[0])
	}
	last := list[len(list)-1]
	if !contract.RoleIsUnidentified(last.RoleID) {
		t.Fatalf("未识别行应使用内置角色 ID，实际 %q", last.RoleID)
	}
	if last.SpeakerID != "" {
		t.Fatalf("未识别行不应暴露 speakerId，实际 %q", last.SpeakerID)
	}
	if last.RoleName != "未识别" {
		t.Fatalf("未识别行应展示「未识别」，实际 %q", last.RoleName)
	}
	if last.UtteranceCount != 1 || last.SpeechMs != 2000 {
		t.Fatalf("未识别行计数不符：%d 句 / %d ms", last.UtteranceCount, last.SpeechMs)
	}
}

// TestListSessionSpeakersNotFound 会话不存在。
func TestListSessionSpeakersNotFound(t *testing.T) {
	f := newFixture(t)
	repo := f.repo(Options{})
	if _, err := repo.ListSessionSpeakers(context.Background(), "s_nope"); !apierr.HasCode(err, apierr.CodeNotFound) {
		t.Fatalf("期望 ERR_NOT_FOUND，实际 %v", err)
	}
	if _, err := repo.ListSessionSpeakers(context.Background(), ""); !apierr.HasCode(err, apierr.CodeInvalidArgument) {
		t.Fatalf("空会话 ID 应报参数错误，实际 %v", err)
	}
}

// TestListRolesCounts 角色列表：覆盖的身份、命中会话数、内置角色置底。
func TestListRolesCounts(t *testing.T) {
	f := newFixture(t)
	f.seedSession("s_1")
	f.seedSession("s_2")
	f.seedSpeaker("sp_a", voiceA1)
	f.seedSpeaker("sp_b", voiceA2)
	f.seedRole("rl_1", "说话人 1", "role-1", false)
	// sp_a 与 sp_b 都归 rl_1，跨 2 场会话（s_1 里两人都在 → 仍只算 1 场）。
	f.seedLink("s_1", "sp_a", "rl_1", 1, 2000)
	f.seedLink("s_1", "sp_b", "rl_1", 1, 2000)
	f.seedLink("s_2", "sp_a", "rl_1", 1, 2000)

	repo := f.repo(Options{})
	roles, err := repo.ListRoles(context.Background())
	if err != nil {
		t.Fatalf("查询角色失败: %v", err)
	}
	if len(roles) != 2 {
		t.Fatalf("应有 2 个角色（含内置），实际 %d", len(roles))
	}
	var main contract.Role
	for _, r := range roles {
		if r.ID == "rl_1" {
			main = r
		}
	}
	if main.ID == "" {
		t.Fatalf("未找到 rl_1：%+v", roles)
	}
	if main.SessionCount != 2 {
		t.Fatalf("命中会话数应为 2（DISTINCT），实际 %d", main.SessionCount)
	}
	if len(main.SpeakerIDs) != 2 {
		t.Fatalf("应覆盖 2 个声纹身份，实际 %v", main.SpeakerIDs)
	}
	// 内置「未识别」排在最后且标记为系统角色。
	lastRole := roles[len(roles)-1]
	if !lastRole.IsSystem || !contract.RoleIsUnidentified(lastRole.ID) {
		t.Fatalf("末位应为内置未识别角色：%+v", lastRole)
	}
}
