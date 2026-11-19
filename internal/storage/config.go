package storage

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"vip.oberon.mandaryn/internal/utils/filex"
)

// Config 是 storage.json 的形状。当前只有数据路径一项；将来若加"备份策略"
// 之类的元配置，请在这里追加字段并保持向后兼容（json:"-,omitempty" 等）。
type Config struct {
	// DataPath 用户自定义的数据库文件路径。空字符串表示走 DefaultDBPath。
	DataPath    string        `json:"dataPath,omitempty"`
	DataRoot    string        `json:"dataRoot,omitempty"`
	PendingRoot *DataRootPlan `json:"pendingRoot,omitempty"`
}

// ConfigManager 管理 storage.json 的读写。它独立于数据库——
// 这层不能依赖 GORM，否则切换路径时会循环依赖。
type ConfigManager struct {
	path string
	mu   sync.Mutex
	cfg  Config
}

// LoadConfig reads the stable path configuration, falling back to the legacy
// default-root location only when no configuration has been written there yet.
func LoadConfig() (*ConfigManager, error) {
	path, err := configFilePath()
	if err != nil {
		return nil, err
	}
	mgr := &ConfigManager{path: path}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if root, err := userDataDir(); err == nil {
			legacy := filepath.Join(root, ConfigFileName)
			if legacy != path {
				old := &ConfigManager{path: legacy}
				if err := old.load(); err != nil {
					return nil, err
				}
				mgr.cfg = old.cfg
				return mgr, nil
			}
		}
	}
	if err := mgr.load(); err != nil {
		return nil, err
	}
	return mgr, nil
}

func (m *ConfigManager) load() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	cfg, err := readConfig(m.path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	m.cfg = cfg
	return nil
}

func readConfig(path string) (Config, error) {
	var cfg Config
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	err = json.Unmarshal(data, &cfg)
	return cfg, err
}

// Get 返回当前 Config 的拷贝（值类型，避免外部修改污染）。
func (m *ConfigManager) Get() Config {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg
}

// EffectiveDBPath 返回当前生效的数据库文件路径：
//   - 用户自定义 → 返回该路径
//   - 未自定义 → 返回 DefaultDBPath
func (m *ConfigManager) EffectiveDBPath() (string, error) {
	cfg := m.Get()
	if cfg.DataPath != "" {
		return cfg.DataPath, nil
	}
	return DefaultDBPath()
}

// SetDataPath 写入新的自定义数据路径并落盘 storage.json。
// 仅更新配置；数据库迁移由调用方负责（参见 storagesvc.SetCustomPath）。
func (m *ConfigManager) SetDataPath(p string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cfg.DataPath = p
	return m.persist()
}

// ResetDataPath 清空自定义路径，回退到平台默认。
func (m *ConfigManager) ResetDataPath() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cfg.DataPath = ""
	return m.persist()
}

// persist 调用方需持有 m.mu。
// 走 filex.WriteAtomic：避免崩溃 / 断电时产生半截 storage.json。
func (m *ConfigManager) persist() error {
	if err := ensureDir(filepath.Dir(m.path)); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m.cfg, "", "  ")
	if err != nil {
		return err
	}
	return filex.WriteAtomic(m.path, data, 0o644)
}
