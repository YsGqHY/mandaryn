package storage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const rootLockName = ".mandaryn-root.lock"

var ErrRootPlanChanged = errors.New("data root migration request changed; restart to reload it")

// DataRootPlan records the real source even when it is deployment-managed.
// It can be applied only during startup, before any application component writes.
type DataRootPlan struct {
	Source     string `json:"source"`
	Target     string `json:"target"`
	DBName     string `json:"dbName,omitempty"`
	LegacyDB   string `json:"legacyDB,omitempty"`
	ModelRoot  string `json:"modelRoot,omitempty"`
	UseDefault bool   `json:"useDefault,omitempty"`
}

func (c Config) UserRoot() string {
	if c.DataRoot != "" {
		return c.DataRoot
	}
	// The legacy database picker saved a file, not an audio data root.
	if c.legacyDatabase() != "" {
		return ""
	}
	return c.DataPath
}

func (c Config) legacyDatabase() string {
	if c.DataRoot != "" || c.DataPath == "" {
		return ""
	}
	if info, err := os.Stat(c.DataPath); err == nil && info.IsDir() {
		return ""
	}
	switch strings.ToLower(filepath.Ext(c.DataPath)) {
	case ".db", ".sqlite", ".sqlite3":
		return c.DataPath
	}
	return ""
}

func (c Config) DatabaseAt(root string, managed bool) string {
	if db := c.legacyDatabase(); !managed && db != "" {
		return db
	}
	return DBPathIn(root)
}

func containsRoot(parent, child string) bool {
	canonical := func(p string) string {
		if resolved, err := filepath.EvalSymlinks(p); err == nil {
			return resolved
		}
		if resolved, err := filepath.EvalSymlinks(filepath.Dir(p)); err == nil {
			return filepath.Join(resolved, filepath.Base(p))
		}
		return filepath.Clean(p)
	}
	parent, child = canonical(parent), canonical(child)
	if runtime.GOOS == "windows" {
		parent, child = strings.ToLower(parent), strings.ToLower(child)
	}
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func sameRoot(a, b string) bool {
	ai, aerr := os.Stat(a)
	bi, berr := os.Stat(b)
	return aerr == nil && berr == nil && os.SameFile(ai, bi)
}

// lockRootConfig serializes requests from instances using different data roots.
// Re-read the stable config after locking so a stale instance cannot consume or
// resurrect a plan another instance changed. A missing file still uses the
// legacy configuration loaded at startup.
func (m *ConfigManager) lockRootConfig() (func(), error) {
	m.mu.Lock()
	lock, err := lockRootDirectory(filepath.Dir(m.path), ".mandaryn-config.lock")
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	unlock := func() {
		_ = lock.Close()
		m.mu.Unlock()
	}
	if cfg, err := readConfig(m.path); err == nil {
		m.cfg = cfg
	} else if !errors.Is(err, os.ErrNotExist) {
		unlock()
		return nil, err
	}
	return unlock, nil
}

func (m *ConfigManager) writeRootConfig(next Config) error {
	before := m.cfg
	m.cfg = next
	if err := m.persist(); err != nil {
		m.cfg = before
		return err
	}
	return nil
}

func (m *ConfigManager) StageDataRoot(source, dbPath, target string, useDefault bool, modelRoot string) error {
	if !filepath.IsAbs(target) || !filepath.IsAbs(source) {
		return errors.New("data root must be an absolute directory path")
	}
	target = filepath.Clean(target)
	if !sameRoot(source, target) {
		if err := checkRootDestination(target); err != nil {
			return err
		}
	}
	if filepath.Clean(target) == filepath.Clean(filepath.Dir(m.path)) || containsRoot(target, filepath.Dir(m.path)) {
		return errors.New("destination overlaps the startup configuration directory")
	}
	plan := DataRootPlan{Source: filepath.Clean(source), Target: target, DBName: filepath.Base(dbPath), ModelRoot: modelRoot, UseDefault: useDefault}
	if !sameRoot(source, filepath.Dir(dbPath)) {
		plan.LegacyDB = dbPath
	}
	unlock, err := m.lockRootConfig()
	if err != nil {
		return err
	}
	defer unlock()
	next := m.cfg
	next.PendingRoot = &plan
	return m.writeRootConfig(next)
}

func checkRootDestination(target string) error {
	entries, err := os.ReadDir(target)
	if err == nil && len(entries) != 0 {
		return errors.New("destination must be empty; existing data will not be overwritten")
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	parent, err := os.Stat(filepath.Dir(target))
	if err != nil {
		return err
	}
	if !parent.IsDir() {
		return errors.New("destination parent must be a directory")
	}
	return nil
}

func (m *ConfigManager) CancelRootMigration() error {
	unlock, err := m.lockRootConfig()
	if err != nil {
		return err
	}
	defer unlock()
	next := m.cfg
	next.PendingRoot = nil
	return m.writeRootConfig(next)
}

// ApplyRootMigration is called with the source root locked by the startup caller.
// The returned destination lock must be held until all live handles close.
func (m *ConfigManager) ApplyRootMigration(plan DataRootPlan) (*os.File, error) {
	unlock, err := m.lockRootConfig()
	if err != nil {
		return nil, err
	}
	defer unlock()
	if m.cfg.PendingRoot == nil || *m.cfg.PendingRoot != plan {
		return nil, ErrRootPlanChanged
	}
	if err := m.copyRootMigration(plan); err != nil {
		return nil, err
	}
	var lock *os.File
	if !sameRoot(plan.Source, plan.Target) {
		var err error
		lock, err = LockDataRoot(plan.Target)
		if err != nil {
			return nil, err
		}
	}
	// Keep the exact target even for Reset; otherwise portable mode could win at the next launch.
	if err := m.writeRootConfig(Config{DataRoot: plan.Target}); err != nil {
		if lock != nil {
			_ = lock.Close()
			// Do not remove a published copy: another process or the user may
			// already have opened or added files to the destination.
			return nil, fmt.Errorf("save migration configuration: %w (source still active; a verified copy remains at %s)", err, plan.Target)
		}
		return nil, err
	}
	return lock, nil
}

// SourceDatabase returns the database actually used when the move was queued.
func (plan DataRootPlan) SourceDatabase() string {
	if plan.LegacyDB != "" {
		return plan.LegacyDB
	}
	return filepath.Join(plan.Source, plan.DBName)
}

func (m *ConfigManager) copyRootMigration(plan DataRootPlan) error {
	if !filepath.IsAbs(plan.Source) || !filepath.IsAbs(plan.Target) || plan.DBName == "" || filepath.Base(plan.DBName) != plan.DBName || !filepath.IsLocal(plan.DBName) {
		return errors.New("invalid data root migration plan")
	}
	if plan.ModelRoot != "" && (containsRoot(plan.Source, plan.ModelRoot) || containsRoot(plan.ModelRoot, plan.Source) || containsRoot(plan.Target, plan.ModelRoot) || containsRoot(plan.ModelRoot, plan.Target)) {
		return errors.New("data root and model root must not overlap")
	}
	if plan.LegacyDB != "" && (!filepath.IsAbs(plan.LegacyDB) || filepath.Base(plan.LegacyDB) != plan.DBName) {
		return errors.New("invalid legacy database path")
	}
	if sameRoot(plan.Source, plan.Target) && (plan.DBName != DBFileName || plan.LegacyDB != "") {
		return errors.New("choose a new directory to migrate a legacy database")
	}
	if !sameRoot(plan.Source, plan.Target) {
		dbPath := plan.SourceDatabase()
		st, err := os.Lstat(dbPath)
		if err != nil || !plainCopyFile(st) || st.Size() == 0 {
			return errors.New("the source database is missing or unreadable")
		}
		excluded := []string{ConfigFileName, rootLockName, ".mandaryn-config.lock", "webview"}
		if plan.LegacyDB == "" {
			excluded = append(excluded, plan.DBName+"-shm")
		}
		if err := CopyDataRoot(plan.Source, plan.Target, excluded, func(stage string) error {
			// Never mix a legacy database with sidecars belonging to a different database.
			var conflicts []string
			if plan.LegacyDB != "" || plan.DBName != DBFileName {
				conflicts = append(conflicts, DBFileName)
			}
			if plan.LegacyDB != "" && plan.DBName != DBFileName {
				conflicts = append(conflicts, plan.DBName)
			}
			for _, base := range conflicts {
				for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
					name := base + suffix
					if _, err := os.Lstat(filepath.Join(stage, name)); !errors.Is(err, os.ErrNotExist) {
						return fmt.Errorf("conflicting legacy database member: %s", name)
					}
				}
			}
			if plan.LegacyDB != "" {
				legacy, err := os.OpenRoot(filepath.Dir(plan.LegacyDB))
				if err != nil {
					return err
				}
				defer legacy.Close()
				dest, err := os.OpenRoot(stage)
				if err != nil {
					return err
				}
				defer dest.Close()
				for _, suffix := range []string{"", "-wal", "-journal"} {
					name := plan.DBName + suffix
					fi, err := legacy.Lstat(name)
					if errors.Is(err, os.ErrNotExist) && suffix != "" {
						continue
					}
					if err != nil || !plainCopyFile(fi) {
						return fmt.Errorf("invalid legacy database member: %s", name)
					}
					if err := copyDataFile(legacy, dest, name, fi); err != nil {
						return err
					}
				}
			}
			if plan.DBName != DBFileName {
				for _, suffix := range []string{"", "-wal", "-journal"} {
					from, to := filepath.Join(stage, plan.DBName+suffix), filepath.Join(stage, DBFileName+suffix)
					if _, err := os.Lstat(from); errors.Is(err, os.ErrNotExist) && suffix != "" {
						continue
					}
					if _, err := os.Lstat(to); !errors.Is(err, os.ErrNotExist) {
						return errors.New("conflicting legacy database files")
					}
					if err := os.Rename(from, to); err != nil {
						return err
					}
				}
			}
			db, err := Open(DBPathIn(stage))
			if err != nil {
				return fmt.Errorf("verify copied database: %w", err)
			}
			return db.Close()
		}); err != nil {
			return err
		}
	}
	return nil
}

// LockDataRoot excludes other Mandaryn processes while a root is live or is
// being copied. Closing the file or exiting the process releases the OS lock.
func LockDataRoot(root string) (*os.File, error) {
	return lockRootDirectory(root, rootLockName)
}

func lockRootDirectory(root, name string) (*os.File, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	base, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer base.Close()
	if st, err := base.Lstat(name); err == nil && !plainCopyFile(st) {
		return nil, errors.New("invalid directory lock file")
	}
	f, err := base.OpenFile(name, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockRootFile(f); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("data directory in use: %w", err)
	}
	return f, nil
}
