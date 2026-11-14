package storage

import (
	"sync/atomic"

	"gorm.io/gorm"
)

// FTS5 全文索引（DOC-04 §5）。
//
// AutoMigrate 无法创建虚表，因此这里用**幂等 SQL** 手工建立：
//   - 外部内容表 + 触发器保持与 segments 同步（不冗余存正文副本）；
//   - 分词器用 unicode61：中文按字切分可用，且不引入 ICU 依赖（纯 Go 构建）。
//
// 兼容性注意：modernc 的 SQLite 未必然编译了 FTS5。这里**不允许**因为 FTS5
// 不可用而阻断启动 —— 降级为 LIKE 检索并记录状态，由上层在 UI 上体现。
var ftsAvailable atomic.Bool

// ftsStatements 建立虚表与三个同步触发器。
var ftsStatements = []string{
	`CREATE VIRTUAL TABLE IF NOT EXISTS segments_fts USING fts5(
		text,
		content='segments',
		content_rowid='id',
		tokenize='unicode61'
	)`,
	// INSERT：新增片段写入索引
	`CREATE TRIGGER IF NOT EXISTS segments_fts_ai AFTER INSERT ON segments BEGIN
		INSERT INTO segments_fts(rowid, text) VALUES (new.id, new.text);
	END`,
	// DELETE：把旧行从索引中移除（FTS5 外部内容表的删除语法）
	`CREATE TRIGGER IF NOT EXISTS segments_fts_ad AFTER DELETE ON segments BEGIN
		INSERT INTO segments_fts(segments_fts, rowid, text) VALUES('delete', old.id, old.text);
	END`,
	// UPDATE：先删旧再插新
	`CREATE TRIGGER IF NOT EXISTS segments_fts_au AFTER UPDATE ON segments BEGIN
		INSERT INTO segments_fts(segments_fts, rowid, text) VALUES('delete', old.id, old.text);
		INSERT INTO segments_fts(rowid, text) VALUES (new.id, new.text);
	END`,
}

// ensureFTS 幂等建立 FTS5 虚表与触发器；失败时降级（返回 nil，只记录状态）。
func ensureFTS(db *gorm.DB) error {
	for _, stmt := range ftsStatements {
		if err := db.Exec(stmt).Error; err != nil {
			ftsAvailable.Store(false)
			return nil
		}
	}
	ftsAvailable.Store(true)
	return nil
}

// FTSAvailable 报告本次进程内 FTS5 是否可用。
// false 时检索层必须降级为 LIKE 查询（结果精度下降但不失败）。
func FTSAvailable() bool { return ftsAvailable.Load() }
