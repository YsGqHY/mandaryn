package storage

import (
	"path/filepath"
	"testing"
	"time"

	sqlex "github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// TestAutoMigrateNewSchema 验证全新库能建出全部业务表、种子角色与 FTS 虚表。
func TestAutoMigrateNewSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	gdb := db.GORM

	// 1) 所有注册表都应存在（含 FTS5 可用时的 segments_fts 虚表）。
	for _, d := range AllModels {
		if !gdb.Migrator().HasTable(d.TableName) {
			t.Errorf("table %s not created", d.TableName)
		}
	}

	// 2) 内置「未识别」角色必须被种子化，且迁移记录已落库。
	var unknown Role
	if err := gdb.First(&unknown, "id = ?", RoleUnknownID).Error; err != nil {
		t.Fatalf("seed role: %v", err)
	}
	if !unknown.IsSystem {
		t.Error("rl_unknown should be a system role")
	}
	var migrated int64
	if err := gdb.Model(&DataMigration{}).Count(&migrated).Error; err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if migrated == 0 {
		t.Error("expected at least one data_migrations row")
	}

	// 3) 单行设置的非零默认值必须正确（GORM Create 会覆盖 SQL 默认值，容易踩）。
	var st AppSettings
	if err := gdb.First(&st, "id = ?", 1).Error; err != nil {
		t.Fatalf("load settings: %v", err)
	}
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"theme_choice", st.ThemeChoice, "system"},
		{"model_source", st.ModelSource, "local"},
		{"fallback_to_local", st.FallbackToLocal, true},
		{"auto_switch_back", st.AutoSwitchBack, false},
		{"proxy_for_download", st.ProxyForDownload, true},
		{"proxy_for_online", st.ProxyForOnline, true},
		{"save_format", st.SaveFormat, "wav"},
		{"sensitivity", st.Sensitivity, "standard"},
		{"log_level", st.LogLevel, "info"},
		{"resource_cap_pct", st.ResourceCapPct, 80},
		{"auto_name", st.AutoName, true},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

// TestResourceCapCheck 验证越界预算被 SQLite CHECK 拦住，
// 保证「不依赖应用层也能兜住脏值」。
func TestResourceCapCheck(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "cap.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := db.GORM.Model(&AppSettings{}).Where("id = ?", 1).
		Update("resource_cap_pct", 200).Error; err == nil {
		t.Fatal("expected CHECK constraint violation for cap=200")
	}
}

// TestSessionCascade 验证删除会话会级联删除片段与时间轴标记。
func TestSessionCascade(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "cascade.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	gdb := db.GORM

	s := Session{ID: "s_test", Title: "t", ModelSource: "local", Status: "completed",
		StartedAt: time.Now().UnixMilli(), DurationMs: 1000, TranscriptionStatus: "none"}
	if err := gdb.Create(&s).Error; err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := gdb.Create(&Segment{SessionID: s.ID, Seq: 0, StartMs: 0, EndMs: 500,
		Text: "hello", IsFinal: true}).Error; err != nil {
		t.Fatalf("create segment: %v", err)
	}
	if err := gdb.Create(&SessionEvent{SessionID: s.ID, AtMs: 10, Kind: "paused"}).Error; err != nil {
		t.Fatalf("create event: %v", err)
	}

	if err := gdb.Delete(&Session{}, "id = ?", s.ID).Error; err != nil {
		t.Fatalf("delete session: %v", err)
	}
	var segCnt, evCnt int64
	gdb.Model(&Segment{}).Count(&segCnt)
	gdb.Model(&SessionEvent{}).Count(&evCnt)
	if segCnt != 0 || evCnt != 0 {
		t.Errorf("cascade failed: segments=%d events=%d", segCnt, evCnt)
	}
}

// TestSegmentUniqueSeq 验证 (session_id, seq) 唯一约束 —— 重转幂等的前提。
func TestSegmentUniqueSeq(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "uniq.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	gdb := db.GORM

	s := Session{ID: "s_u", Title: "t", ModelSource: "local", Status: "completed",
		StartedAt: 1, TranscriptionStatus: "none"}
	if err := gdb.Create(&s).Error; err != nil {
		t.Fatalf("create session: %v", err)
	}
	seg := Segment{SessionID: s.ID, Seq: 3, StartMs: 0, EndMs: 100, Text: "a", IsFinal: true}
	if err := gdb.Create(&seg).Error; err != nil {
		t.Fatalf("first insert: %v", err)
	}
	dup := Segment{SessionID: s.ID, Seq: 3, StartMs: 100, EndMs: 200, Text: "b", IsFinal: true}
	if err := gdb.Create(&dup).Error; err == nil {
		t.Fatal("expected unique constraint violation on (session_id, seq)")
	}
}

// TestUpgradeFromLegacySchema 模拟旧库（app_settings 只有主题 / 语言三列）
// 被新版本打开：AutoMigrate 必须能加列加表，且不丢已有数据。
func TestUpgradeFromLegacySchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")

	// 1) 造出「旧版」app_settings：刻意只含旧列，表名与现版本一致。
	legacy, err := gorm.Open(sqlex.Open(path), &gorm.Config{})
	if err != nil {
		t.Fatalf("open legacy: %v", err)
	}
	type legacySettings struct {
		ID           uint   `gorm:"primaryKey"`
		ThemeChoice  string `gorm:"size:64;not null;default:'system'"`
		CustomTheme  string `gorm:"type:text"`
		LocaleChoice string `gorm:"size:32;not null;default:'auto'"`
		UpdatedAt    int64  `gorm:"autoUpdateTime:milli"`
	}
	if err := legacy.AutoMigrate(&legacySettings{}); err != nil {
		t.Fatalf("legacy migrate: %v", err)
	}
	// 表名对齐现版本，这样新版本打开的是「同一张需要加列的表」。
	if err := legacy.Table("app_settings").AutoMigrate(&legacySettings{}); err != nil {
		t.Fatalf("legacy migrate app_settings: %v", err)
	}
	if err := legacy.Exec(
		`INSERT OR REPLACE INTO app_settings (id, theme_choice, custom_theme, locale_choice) VALUES (1,'obsidian','','en-US')`,
	).Error; err != nil {
		t.Fatalf("legacy insert: %v", err)
	}
	sqlDB, _ := legacy.DB()
	_ = sqlDB.Close()

	// 2) 新版本打开：新列被加上，旧数据仍在。
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open upgraded: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var st AppSettings
	if err := db.GORM.First(&st, "id = ?", 1).Error; err != nil {
		t.Fatalf("load upgraded settings: %v", err)
	}
	if st.ThemeChoice != "obsidian" || st.LocaleChoice != "en-US" {
		t.Errorf("existing data lost: theme=%q locale=%q", st.ThemeChoice, st.LocaleChoice)
	}
	// 新列对已有行取列默认值（SQLite ADD COLUMN ... DEFAULT 的语义）。
	if st.ResourceCapPct != 80 {
		t.Errorf("resource_cap_pct = %d, want 80 (column default)", st.ResourceCapPct)
	}
	if !st.FallbackToLocal || !st.ProxyForDownload || !st.ProxyForOnline {
		t.Errorf("bool column defaults wrong: fallback=%v dl=%v online=%v",
			st.FallbackToLocal, st.ProxyForDownload, st.ProxyForOnline)
	}
	if st.ModelSource != "local" {
		t.Errorf("model_source = %q, want local", st.ModelSource)
	}
}
