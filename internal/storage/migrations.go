package storage

import (
	"fmt"
	"time"

	"gorm.io/gorm"
)

// 一次性数据迁移钩子（DOC-04 TBL-11 / §4.5）。
//
// GORM AutoMigrate 只做「加表 / 加列 / 加索引 / 改兼容列类型」；
// **删列、改名、数据搬迁、回填**必须在这里以具名钩子执行，执行前查
// data_migrations 保证幂等（NFR-04-5）。
//
// 新增钩子：在 migrations 切片里追加一项即可，Name 用 `YYYYMMDD_<做什么>`，
// 必须保证「重复执行无副作用」（存在写完数据但没来得及记录成功的情况）。

type migration struct {
	Name string
	Run  func(db *gorm.DB) error
}

// RoleUnknownID 内置「未识别」角色（DOC-04 TBL-07）。
// 它必须始终存在：session_speakers.role_id 在删除角色 / 清空声纹库时回落到它。
const RoleUnknownID = "rl_unknown"

var migrations = []migration{
	{
		Name: "20261111_seed_roles_unknown",
		Run:  seedUnknownRole,
	},
}

func runMigrations(db *gorm.DB) error {
	for _, m := range migrations {
		var cnt int64
		if err := db.Model(&DataMigration{}).Where("name = ?", m.Name).Count(&cnt).Error; err != nil {
			return fmt.Errorf("storage: check migration %s: %w", m.Name, err)
		}
		if cnt > 0 {
			continue
		}
		if err := m.Run(db); err != nil {
			return fmt.Errorf("storage: run migration %s: %w", m.Name, err)
		}
		rec := DataMigration{Name: m.Name, AppliedAt: time.Now().UnixMilli()}
		if err := db.Create(&rec).Error; err != nil {
			return fmt.Errorf("storage: record migration %s: %w", m.Name, err)
		}
	}
	return nil
}

func seedUnknownRole(db *gorm.DB) error {
	var cnt int64
	if err := db.Model(&Role{}).Where("id = ?", RoleUnknownID).Count(&cnt).Error; err != nil {
		return err
	}
	if cnt > 0 {
		return nil
	}
	return db.Create(&Role{
		ID:         RoleUnknownID,
		Name:       "未识别",
		ColorToken: "neutral",
		IsSystem:   true,
	}).Error
}

// PurgeOldOutbound 清理过期出站记录（DOC-04 TBL-12 保留策略：仅最近 24h）。
// 启动时调用一次；失败不影响可用性。
func PurgeOldOutbound(db *gorm.DB, now time.Time, keep time.Duration) error {
	cutoff := now.Add(-keep).UnixMilli()
	return db.Where("at < ?", cutoff).Delete(&OutboundLog{}).Error
}
