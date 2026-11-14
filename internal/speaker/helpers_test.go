package speaker

// 测试基础设施：临时 SQLite、种子数据、fake 提取器与 fake 音频端口。
//
// 全程离线：不加载任何模型、不需要 CGO / DLL / 音频文件 —— 声纹向量由
// fake 音频端口直接「喂」进 fake 提取器（把向量当作样本的前 dim 个 float32），
// 因此聚类与归并的真实逻辑被完整驱动，而推理被替换成可断言的确定性映射。

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"gorm.io/gorm"

	"vip.oberon.mandaryn/internal/storage"
)

// fixedNow 是测试用的固定时钟，避免时间戳抖动影响断言。
var fixedNow = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// fixture 是「一个临时数据库 + 一个 Repo」的测试夹具。
type fixture struct {
	t      *testing.T
	dir    string
	db     *storage.DB
	holder *storage.Holder
}

// newFixture 建临时库（含 AutoMigrate / 迁移 / FTS）。
func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	db, err := storage.Open(filepath.Join(dir, "mandaryn.db"))
	if err != nil {
		t.Fatalf("打开临时数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &fixture{t: t, dir: dir, db: db, holder: storage.NewHolder(db)}
}

// repo 用给定选项构造 Repo（Holder / Now 由夹具补上）。
func (f *fixture) repo(opts Options) *Repo {
	f.t.Helper()
	opts.Holder = f.holder
	if opts.Now == nil {
		opts.Now = func() time.Time { return fixedNow }
	}
	r, err := New(opts)
	if err != nil {
		f.t.Fatalf("构造 Repo 失败: %v", err)
	}
	return r
}

// seedSession 插入一个最小可用会话。
func (f *fixture) seedSession(sessionID string) {
	f.t.Helper()
	row := storage.Session{
		ID:          sessionID,
		Title:       "测试会话 " + sessionID,
		ModelSource: "local",
		Status:      "completed",
		StartedAt:   fixedNow.UnixMilli(),
		DurationMs:  60000,
		AudioPath:   "sessions/" + sessionID + "/audio.wav",
	}
	if err := f.db.GORM.Create(&row).Error; err != nil {
		f.t.Fatalf("插入会话失败: %v", err)
	}
}

// seedSegment 插入一个定稿片段，返回其 ID。
func (f *fixture) seedSegment(sessionID string, seq int, startMs, endMs int64, speakerID string) int64 {
	f.t.Helper()
	row := storage.Segment{
		SessionID: sessionID,
		Seq:       seq,
		StartMs:   startMs,
		EndMs:     endMs,
		Text:      fmt.Sprintf("片段 %d", seq),
		SpeakerID: speakerID,
		IsFinal:   true,
	}
	if err := f.db.GORM.Create(&row).Error; err != nil {
		f.t.Fatalf("插入片段失败: %v", err)
	}
	return row.ID
}

// count 统计某表的行数。
func (f *fixture) count(model any) int64 {
	f.t.Helper()
	var n int64
	if err := f.db.GORM.Model(model).Count(&n).Error; err != nil {
		f.t.Fatalf("统计行数失败: %v", err)
	}
	return n
}

// snapshotSegments 把 segments 全表快照成静态结构，用于断言「改名没碰片段」。
func (f *fixture) snapshotSegments() []storage.Segment {
	f.t.Helper()
	var rows []storage.Segment
	if err := f.db.GORM.Order("id ASC").Find(&rows).Error; err != nil {
		f.t.Fatalf("读取片段失败: %v", err)
	}
	return rows
}

// —— fake 声纹提取器 ——

// fakeExtractor 把「样本」当成向量本身（取前 dim 个 float32）。
//
// 这样测试可以直接用「样本内容」控制声纹，从而精确构造相似 / 不相似的簇，
// 而不需要真实音频与真实模型。
type fakeExtractor struct {
	id      string
	dim     int
	closed  bool
	failAll bool
}

func (e *fakeExtractor) Embed(samples []float32, sampleRate int) ([]float32, error) {
	if e.closed {
		return nil, errors.New("extractor closed")
	}
	if e.failAll {
		return nil, errors.New("fake: 故意让提取失败")
	}
	if sampleRate != SampleRate {
		return nil, fmt.Errorf("fake: 采样率必须是 %d，实际 %d", SampleRate, sampleRate)
	}
	if len(samples) < e.dim {
		return nil, errors.New("fake: 样本过短")
	}
	out := make([]float32, e.dim)
	copy(out, samples[:e.dim])
	return out, nil
}

func (e *fakeExtractor) ID() string   { return e.id }
func (e *fakeExtractor) Close() error { e.closed = true; return nil }

// —— fake 音频端口 ——

// fakeAudio 按 (sessionID, startMs) 返回「把向量塞进前几个采样」的假样本。
//
// 缺失的区间返回错误，用来模拟「音频文件不可读」的降级路径。
type fakeAudio struct {
	vectors map[string][]float32
	// sampleLen 返回的样本长度（默认 1s = 16000 个采样）。
	sampleLen int
	failKeys  map[string]bool
}

func newFakeAudio() *fakeAudio {
	return &fakeAudio{vectors: map[string][]float32{}, sampleLen: SampleRate, failKeys: map[string]bool{}}
}

// key 生成片段音频的查找键。
func audioKey(sessionID string, startMs int64) string {
	return fmt.Sprintf("%s@%d", sessionID, startMs)
}

// put 登记某个片段区间的声纹向量。
func (a *fakeAudio) put(sessionID string, startMs int64, vec []float32) {
	a.vectors[audioKey(sessionID, startMs)] = vec
}

// fail 让某个片段区间读取失败。
func (a *fakeAudio) fail(sessionID string, startMs int64) {
	a.failKeys[audioKey(sessionID, startMs)] = true
}

func (a *fakeAudio) LoadSegment(_ context.Context, sessionID string, startMs, endMs int64) ([]float32, error) {
	key := audioKey(sessionID, startMs)
	if a.failKeys[key] {
		return nil, errors.New("fake: 区间不可读")
	}
	vec, ok := a.vectors[key]
	if !ok {
		return nil, errors.New("fake: 没有该区间的音频")
	}
	if endMs <= startMs {
		return nil, nil
	}
	out := make([]float32, a.sampleLen)
	copy(out, vec)
	return out, nil
}

// —— 向量构造 ——

// vec4 构造 4 维向量（测试统一用 4 维，便于手算相似度）。
func vec4(a, b, c, d float32) []float32 { return []float32{a, b, c, d} }

// 两组「明显分开」的向量：同组内余弦 ≈ 0.998，跨组 ≈ 0。
var (
	voiceA1 = vec4(1, 0, 0, 0)
	voiceA2 = vec4(0.98, 0.06, 0, 0)
	voiceA3 = vec4(0.97, 0.08, 0, 0)
	voiceB1 = vec4(0, 1, 0, 0)
	voiceB2 = vec4(0.05, 0.99, 0, 0)
	voiceB3 = vec4(0.04, 0.98, 0.02, 0)
	voiceC1 = vec4(0, 0, 1, 0)
)

// seedVoiceSession 建一场会话：每个片段给 (startMs, endMs, 向量)。
//
// 片段间隔 3s，时长 2s（> 1.2s 门槛），seq 从 1 递增。
func (f *fixture) seedVoiceSession(sessionID string, audio *fakeAudio, voices [][]float32) []int64 {
	f.t.Helper()
	f.seedSession(sessionID)
	ids := make([]int64, 0, len(voices))
	for i, v := range voices {
		start := int64(i) * 3000
		end := start + 2000
		ids = append(ids, f.seedSegment(sessionID, i+1, start, end, ""))
		audio.put(sessionID, start, v)
	}
	return ids
}

// findSegment 读取单个片段。
func (f *fixture) findSegment(id int64) storage.Segment {
	f.t.Helper()
	var row storage.Segment
	if err := f.db.GORM.First(&row, "id = ?", id).Error; err != nil {
		f.t.Fatalf("读取片段 %d 失败: %v", id, err)
	}
	return row
}

// speakerIDs 返回会话内出现的声纹身份（升序去重）。
func (f *fixture) speakerIDs(sessionID string) []string {
	f.t.Helper()
	var ids []string
	if err := f.db.GORM.Model(&storage.Segment{}).
		Where("session_id = ? AND speaker_id <> ''", sessionID).
		Distinct("speaker_id").Order("speaker_id ASC").Pluck("speaker_id", &ids).Error; err != nil {
		f.t.Fatalf("读取说话人失败: %v", err)
	}
	return ids
}

// roleOfSegment 通过 session_speakers → roles JOIN 查片段当前展示的角色名
// （这正是「改名全局同步」的实现路径，测试必须走同一条路）。
func (f *fixture) roleOfSegment(segmentID int64) (roleID, roleName string) {
	f.t.Helper()
	var out struct {
		RoleID   string
		RoleName string
	}
	err := f.db.GORM.Table("segments AS seg").
		Select("COALESCE(ss.role_id, '') AS role_id, COALESCE(r.name, '') AS role_name").
		Joins("LEFT JOIN session_speakers ss ON ss.session_id = seg.session_id AND ss.speaker_id = seg.speaker_id").
		Joins("LEFT JOIN roles r ON r.id = ss.role_id").
		Where("seg.id = ?", segmentID).
		Scan(&out).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		f.t.Fatalf("查询片段角色失败: %v", err)
	}
	return out.RoleID, out.RoleName
}

// mustRoleByName 按名字找角色（找不到返回零值）。
func (f *fixture) mustRoleByName(name string) storage.Role {
	f.t.Helper()
	var row storage.Role
	if err := f.db.GORM.First(&row, "name = ?", name).Error; err != nil {
		f.t.Fatalf("找不到角色 %q: %v", name, err)
	}
	return row
}

// seedSpeaker 插入一条声纹身份（vec 为空表示「没有向量」，如手工拆分出来的）。
func (f *fixture) seedSpeaker(speakerID string, vec []float32) {
	f.t.Helper()
	enc, dim := "", 0
	if len(vec) > 0 {
		e, err := EncodeVector(vec)
		if err != nil {
			f.t.Fatalf("编码向量失败: %v", err)
		}
		enc, dim = e, len(vec)
	}
	row := storage.Speaker{
		ID:           speakerID,
		EmbeddingEnc: enc,
		Dim:          dim,
		ModelID:      "m1",
		FirstSeenAt:  fixedNow.UnixMilli(),
		LastSeenAt:   fixedNow.UnixMilli(),
	}
	if err := f.db.GORM.Create(&row).Error; err != nil {
		f.t.Fatalf("插入声纹身份失败: %v", err)
	}
}

// seedRole 插入一个角色。
func (f *fixture) seedRole(roleID, name, colorToken string, system bool) {
	f.t.Helper()
	row := storage.Role{ID: roleID, Name: name, ColorToken: colorToken, IsSystem: system}
	if err := f.db.GORM.Create(&row).Error; err != nil {
		f.t.Fatalf("插入角色失败: %v", err)
	}
}

// seedLink 插入一条「会话 × 说话人 → 角色」映射。
func (f *fixture) seedLink(sessionID, speakerID, roleID string, utteranceCount int, speechMs int64) {
	f.t.Helper()
	row := storage.SessionSpeaker{
		SessionID:      sessionID,
		SpeakerID:      speakerID,
		RoleID:         roleID,
		UtteranceCount: utteranceCount,
		SpeechMs:       speechMs,
	}
	if err := f.db.GORM.Create(&row).Error; err != nil {
		f.t.Fatalf("插入映射失败: %v", err)
	}
}
