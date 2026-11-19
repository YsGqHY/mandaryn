// Package storagesvc 暴露"数据存储"管理：路径查看、切换、统计。
//
// 目录切换只在重启时生效；此服务不会替换运行中的数据库或音频目录。
package storagesvc

import (
	"context"
	"fmt"
	"path/filepath"

	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/paths"
	"vip.oberon.mandaryn/internal/storage"
)

// Service 暴露给前端：路径管理 + 统计。
type Service struct {
	holder         *storage.Holder
	cfg            *storage.ConfigManager
	root           contract.ResolvedPath
	resolver       *paths.Resolver
	migrationError string
}

func New(holder *storage.Holder, cfg *storage.ConfigManager, root contract.ResolvedPath, resolver *paths.Resolver, migrationError string) *Service {
	return &Service{holder: holder, cfg: cfg, root: root, resolver: resolver, migrationError: migrationError}
}

// Stats 设置页用于展示当前路径与占用空间。
type Stats struct {
	Path           string `json:"path"`
	IsCustom       bool   `json:"isCustom"`
	DefaultPath    string `json:"defaultPath"`
	DataRoot       string `json:"dataRoot"`
	PendingRoot    string `json:"pendingRoot"`
	ManagedBy      string `json:"managedBy"`
	OverrideHint   string `json:"overrideHint"`
	MigrationError string `json:"migrationError"`
	SizeBytes      int64  `json:"sizeBytes"`
}

// TableInfo 单张表的展示元数据 + 实际占用。
// 元数据来自 storage.AllModels（labelKey / clearable），用量由 dbstat 或估算得出。
type TableInfo struct {
	Name      string `json:"name"`      // SQLite 表名（如 'preferences'）
	LabelKey  string `json:"labelKey"`  // i18n key 后缀（前端拼 settings.database.tables.<key>.label）
	Clearable bool   `json:"clearable"` // 是否允许 ClearTable
	RowCount  int64  `json:"rowCount"`
	SizeBytes int64  `json:"sizeBytes"`
	Estimated bool   `json:"estimated"` // true = 用行数比例估算，false = dbstat 精确值
}

// TableStats 整体快照。Total 是所有表的占用之和；
// 与 Stats.SizeBytes 不同 —— 后者含 .db-wal / .db-shm 文件，前者只算表数据。
type TableStats struct {
	TotalBytes int64       `json:"totalBytes"`
	Tables     []TableInfo `json:"tables"`
}

// GetStats 拉取当前数据存储信息。
func (s *Service) GetStats(_ context.Context) (Stats, error) {
	def, err := storage.DefaultDBPath()
	if err != nil {
		return Stats{}, err
	}
	cfg := s.cfg.Get()
	var pending string
	if cfg.PendingRoot != nil {
		pending = cfg.PendingRoot.Target
	}
	cur := s.holder.Current().Path

	size, _ := storage.FileSize(cur) // 失败时给 0，不阻断 UI

	return Stats{
		Path:           cur,
		IsCustom:       filepath.Clean(s.root.Effective) != filepath.Dir(def),
		DataRoot:       s.root.Effective,
		PendingRoot:    pending,
		ManagedBy:      s.root.EnvName,
		OverrideHint:   s.root.OverrideHint,
		MigrationError: s.migrationError,
		DefaultPath:    def,
		SizeBytes:      size,
	}, nil
}

// GetTableStats 收集每张已注册表的行数 + 占用。
//
// dbstat 可用时给精确值；不可用时按行数比例估算（前端会标注"估算"）。
// 表清单严格从 storage.AllModels 取，避免任意 SQL 注入到 Raw 查询。
func (s *Service) GetTableStats(ctx context.Context) (TableStats, error) {
	current := s.holder.Current()
	descs := storage.AllModels

	names := make([]string, 0, len(descs))
	for _, d := range descs {
		names = append(names, d.TableName)
	}

	usage, err := storage.CollectTableUsage(current.GORM.WithContext(ctx), names, current.Path)
	if err != nil {
		return TableStats{}, err
	}

	// 把 usage 与 descriptor 对齐，组装 TableInfo
	usageByName := make(map[string]storage.TableUsage, len(usage))
	for _, u := range usage {
		usageByName[u.TableName] = u
	}

	tables := make([]TableInfo, 0, len(descs))
	var total int64
	for _, d := range descs {
		u := usageByName[d.TableName]
		tables = append(tables, TableInfo{
			Name:      d.TableName,
			LabelKey:  d.LabelKey,
			Clearable: d.Clearable,
			RowCount:  u.RowCount,
			SizeBytes: u.SizeBytes,
			Estimated: u.Estimated,
		})
		total += u.SizeBytes
	}

	return TableStats{TotalBytes: total, Tables: tables}, nil
}

// ClearTable 清空指定表。仅允许 storage.AllModels 中标记为 Clearable=true 的表。
//
// 安全约束：
//   - 表名必须在白名单（FindDescriptor）
//   - 表必须 clearable=true
//   - 用 GORM 的 Where("1=1").Delete 触发 hooks（如有）；不用 raw TRUNCATE，
//     避免绕过任何业务级钩子。SQLite 没有 TRUNCATE，DELETE 也是逐行；
//     脚手架场景表通常不大，直接 DELETE 足够。
func (s *Service) ClearTable(ctx context.Context, tableName string) error {
	desc := storage.FindDescriptor(tableName)
	if desc == nil {
		return fmt.Errorf("unknown table: %s", tableName)
	}
	if !desc.Clearable {
		return fmt.Errorf("table %s is not clearable", tableName)
	}
	return s.holder.Current().GORM.WithContext(ctx).
		Where("1 = 1").Delete(desc.Model).Error
}

// SetCustomPath stages a directory migration; it does not change live paths.
func (s *Service) SetCustomPath(_ context.Context, dir string) error {
	return s.stageRoot(dir, false)
}

func (s *Service) ResetPath(_ context.Context) error {
	root, err := paths.DefaultDataRoot()
	if err != nil {
		return err
	}
	return s.stageRoot(root, true)
}

func (s *Service) stageRoot(dir string, useDefault bool) error {
	modelRoot, err := s.resolver.Effective(paths.KeyModelRoot)
	if err != nil {
		return err
	}
	return s.cfg.StageDataRoot(s.root.Effective, s.holder.Current().Path, dir, useDefault, modelRoot)
}

func (s *Service) CancelRootMigration(_ context.Context) error {
	return s.cfg.CancelRootMigration()
}
