package hotkeys

import (
	"context"
	"errors"
	"strings"
	"sync"

	"vip.oberon.mandaryn/internal/storage"

	"gorm.io/gorm"
)

// 持久化键（DOC-04 TBL-01.1 / TBL-01）。
const (
	// KeyBindingOverride preferences 里存快捷键覆盖表的键。
	KeyBindingOverride = "hotkeys.override"
	// KeyLeanMode preferences 里存精简模式开关的键。
	KeyLeanMode = "ux.leanMode"
)

// Store 是热键域需要的最小键值持久化能力。
//
// 抽象成接口而不是直接吃 *storage.Holder 的两个理由：
//  1. 单测可以不碰 SQLite（内存实现即可覆盖全部业务分支）；
//  2. 让「谁负责持久化」显式化 —— L2 不直接摸 *gorm.DB（CLAUDE.md 铁律）。
//
// 生产实现是 PreferenceStore（preferences 表，DOC-04 TBL-01.1 明确不新建表）。
type Store interface {
	Get(ctx context.Context, key string) (string, error) // 缺失返回 ""，不返回错误
	Set(ctx context.Context, key, value string) error
	Delete(ctx context.Context, key string) error
}

// MemoryStore 是内存实现（测试 / 无持久化场景）。
type MemoryStore struct {
	mu sync.RWMutex
	kv map[string]string
}

// NewMemoryStore 构造内存 store。
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{kv: map[string]string{}}
}

// Get 读键；缺失返回空串。
func (s *MemoryStore) Get(_ context.Context, key string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.kv[key], nil
}

// Set 写键。
func (s *MemoryStore) Set(_ context.Context, key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.kv[key] = value
	return nil
}

// Delete 删键。
func (s *MemoryStore) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.kv, key)
	return nil
}

// PreferenceStore 用既有 preferences 表存热键域的两条 JSON，**不新建表**。
//
// 为什么按 DOC-04 TBL-01.1 存 preferences 而不是扩 app_settings：
// 快捷键是「条目固定但用户可能一条都不改」的开放映射，塞进单行强 schema
// 表会变成 17 个列，且每加一个动作都要迁移 schema。
type PreferenceStore struct {
	holder *storage.Holder
}

// NewPreferenceStore 构造基于 storage.Holder 的实现。
//
// 持 Holder 而不是 *gorm.DB：切换数据路径会替换整个进程的 DB 句柄，
// 捕获具体句柄会写到旧库（PERSISTENCE.md 铁律）。
func NewPreferenceStore(holder *storage.Holder) *PreferenceStore {
	return &PreferenceStore{holder: holder}
}

func (s *PreferenceStore) db() (*gorm.DB, error) {
	if s == nil || s.holder == nil {
		return nil, errors.New("hotkeys: storage holder is not ready")
	}
	db := s.holder.Current()
	if db == nil || db.GORM == nil {
		return nil, errors.New("hotkeys: database is not open")
	}
	return db.GORM, nil
}

// Get 读偏好键（缺失返回 ""）。
func (s *PreferenceStore) Get(ctx context.Context, key string) (string, error) {
	db, err := s.db()
	if err != nil {
		return "", err
	}
	var row storage.Preference
	if err := db.WithContext(ctx).First(&row, "key = ?", key).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", nil
		}
		return "", err
	}
	return row.Value, nil
}

// Set upsert 偏好键。
func (s *PreferenceStore) Set(ctx context.Context, key, value string) error {
	db, err := s.db()
	if err != nil {
		return err
	}
	if strings.TrimSpace(key) == "" {
		return errors.New("hotkeys: preference key required")
	}
	return db.WithContext(ctx).Save(&storage.Preference{Key: key, Value: value}).Error
}

// Delete 删除偏好键（不返回「不存在」错误，调用方按幂等处理）。
func (s *PreferenceStore) Delete(ctx context.Context, key string) error {
	db, err := s.db()
	if err != nil {
		return err
	}
	return db.WithContext(ctx).Where("key = ?", key).Delete(&storage.Preference{}).Error
}
