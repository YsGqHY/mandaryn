package storage

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

func TestCopyDataRootSuccess(t *testing.T) {
	for _, existingTarget := range []bool{false, true} {
		name := "absent target"
		if existingTarget {
			name = "empty target"
		}
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			src, dst := filepath.Join(parent, "source"), filepath.Join(parent, "target")
			copyTestMkdir(t, filepath.Join(src, "recordings", "empty"))
			payload := strings.Repeat("recording\x00", 32768)
			copyTestWrite(t, filepath.Join(src, "recordings", "audio.flac"), payload)
			copyTestWrite(t, filepath.Join(src, "recordings", "storage.json"), "leave nested files")
			copyTestWrite(t, filepath.Join(src, "storage.json"), "configuration")
			copyTestWrite(t, filepath.Join(src, ".run.lock"), "lock")
			copyTestMkdir(t, filepath.Join(src, "webview"))
			copyTestWrite(t, filepath.Join(src, "webview", "cache"), "cache")
			if existingTarget {
				copyTestMkdir(t, dst)
			}
			before, err := os.Stat(filepath.Join(src, "recordings", "audio.flac"))
			if err != nil {
				t.Fatal(err)
			}
			verified := false
			err = CopyDataRoot(src, dst, []string{"storage.json", ".run.lock", "webview"}, func(stage string) error {
				verified = true
				stageParent, err := os.Stat(filepath.Dir(stage))
				if err != nil {
					return err
				}
				wantParent, err := os.Stat(parent)
				if err != nil {
					return err
				}
				if !os.SameFile(stageParent, wantParent) {
					t.Fatal("verification stage is not beside the destination")
				}
				copyTestCheckFile(t, filepath.Join(stage, "recordings", "audio.flac"), payload)
				copyTestAbsent(t, filepath.Join(dst, "recordings"))
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if !verified {
				t.Fatal("verification was not called")
			}
			copyTestCheckFile(t, filepath.Join(dst, "recordings", "audio.flac"), payload)
			copyTestCheckFile(t, filepath.Join(dst, "recordings", "storage.json"), "leave nested files")
			if fi, err := os.Stat(filepath.Join(dst, "recordings", "empty")); err != nil || !fi.IsDir() {
				t.Fatalf("empty directory missing: %v", err)
			}
			for _, name := range []string{"storage.json", ".run.lock", "webview"} {
				copyTestAbsent(t, filepath.Join(dst, name))
			}
			copyTestCheckFile(t, filepath.Join(src, "recordings", "audio.flac"), payload)
			copyTestCheckFile(t, filepath.Join(src, "storage.json"), "configuration")
			copyTestCheckFile(t, filepath.Join(src, ".run.lock"), "lock")
			copyTestCheckFile(t, filepath.Join(src, "webview", "cache"), "cache")
			after, err := os.Stat(filepath.Join(src, "recordings", "audio.flac"))
			if err != nil || !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
				t.Fatalf("original file identity or modification time changed: %v", err)
			}
			copyTestNoStage(t, parent)
		})
	}
}

func TestDataRootMigrationLegacyDatabase(t *testing.T) {
	parent := t.TempDir()
	source, target := filepath.Join(parent, "live"), filepath.Join(parent, "target")
	legacy := filepath.Join(parent, "external", "old.sqlite")
	mgr := &ConfigManager{path: filepath.Join(parent, "config", ConfigFileName), cfg: Config{DataPath: legacy}}
	copyTestMkdir(t, source)
	copyTestWrite(t, filepath.Join(source, ".master.key"), "key fixture")
	db, err := Open(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.GORM.Create(&Session{ID: "fixture", Title: "legacy session", ModelSource: "local", Status: "completed", AudioPath: "sessions/fixture/audio.wav"}).Error; err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if mgr.Get().DatabaseAt(source, false) != legacy || mgr.Get().DatabaseAt(source, true) != DBPathIn(source) {
		t.Fatal("legacy database must not override a managed data root")
	}
	if err := mgr.StageDataRoot(source, legacy, target, false, ""); err != nil {
		t.Fatal(err)
	}
	plan := *mgr.Get().PendingRoot
	if plan.SourceDatabase() != legacy {
		t.Fatalf("lost external database: %+v", plan)
	}
	// A sidecar from a different database must not be applied to the copied database.
	conflict := DBPathIn(source) + "-wal"
	copyTestWrite(t, conflict, "unrelated WAL")
	if _, err := mgr.ApplyRootMigration(plan); err == nil || mgr.Get().PendingRoot == nil {
		t.Fatal("conflicting database file consumed the migration")
	}
	copyTestAbsent(t, target)
	copyTestNoStage(t, parent)
	if err := os.Remove(conflict); err != nil {
		t.Fatal(err)
	}
	lock, err := mgr.ApplyRootMigration(plan)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	cfg, err := readConfig(mgr.path)
	if err != nil || cfg.DataRoot != target || cfg.PendingRoot != nil || cfg.DataPath != "" {
		t.Fatalf("unexpected migration config: %+v, %v", cfg, err)
	}
	copyTestCheckFile(t, filepath.Join(target, ".master.key"), "key fixture")
	copyTestAbsent(t, filepath.Join(target, filepath.Base(legacy)))
	for _, name := range []string{legacy, DBPathIn(target)} {
		db, err := Open(name)
		if err != nil {
			t.Fatal(err)
		}
		var row Session
		readErr := db.GORM.First(&row, "id = ?", "fixture").Error
		if err := errors.Join(readErr, db.Close()); err != nil || row.AudioPath != "sessions/fixture/audio.wav" {
			t.Fatalf("%s lost its relative audio reference: %q, %v", name, row.AudioPath, err)
		}
	}
}

func TestDataRootMigrationRejectsStalePlan(t *testing.T) {
	parent := t.TempDir()
	source, target := filepath.Join(parent, "live"), filepath.Join(parent, "target")
	copyTestMkdir(t, source)
	path := filepath.Join(parent, "config", ConfigFileName)
	mgr, other := &ConfigManager{path: path}, &ConfigManager{path: path}
	if err := mgr.StageDataRoot(source, DBPathIn(source), target, false, ""); err != nil {
		t.Fatal(err)
	}
	plan := *mgr.Get().PendingRoot
	if err := other.CancelRootMigration(); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.ApplyRootMigration(plan); !errors.Is(err, ErrRootPlanChanged) {
		t.Fatalf("stale startup request should be rejected, got %v", err)
	}
	copyTestAbsent(t, target)
}

func TestDataRootMigrationConfigWriteFailureKeepsOldSelection(t *testing.T) {
	path := t.TempDir() // An existing directory cannot be replaced by a config file.
	plan := &DataRootPlan{Source: "old", Target: "new"}
	before := Config{DataRoot: plan.Source, PendingRoot: plan}
	mgr := &ConfigManager{path: path, cfg: before}
	if err := mgr.writeRootConfig(Config{DataRoot: plan.Target}); err == nil {
		t.Fatal("writing over an existing directory unexpectedly succeeded")
	}
	if got := mgr.Get(); got != before {
		t.Fatalf("write failure changed the active or pending selection: %+v", got)
	}
}

func TestCopyDataRootTargetConflicts(t *testing.T) {
	parent := t.TempDir()
	src, dst := filepath.Join(parent, "source"), filepath.Join(parent, "target")
	copyTestMkdir(t, src)
	copyTestMkdir(t, dst)
	copyTestWrite(t, filepath.Join(src, "source.txt"), "original source")
	copyTestWrite(t, filepath.Join(dst, "user.txt"), "existing contents")
	outsideFile := filepath.Join(parent, "existing-file")
	copyTestWrite(t, outsideFile, "existing file")
	for _, tc := range []struct{ name, source, target string }{
		{"relative source", "source", dst},
		{"relative target", src, "target"},
		{"same directory", src, src},
		{"target below source", src, filepath.Join(src, "child")},
		{"source below target", src, parent},
		{"nonempty target", src, dst},
		{"target is file", src, outsideFile},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CopyDataRoot(tc.source, tc.target, nil, func(string) error {
				t.Fatal("verification ran with invalid roots")
				return nil
			})
			if err == nil {
				t.Fatal("unexpected success")
			}
		})
	}
	if runtime.GOOS == "windows" {
		if err := CopyDataRoot(src, filepath.Join(parent, "SOURCE", "child"), nil, nil); err == nil {
			t.Fatal("accepted case-insensitive overlap")
		}
	}
	copyTestCheckFile(t, filepath.Join(src, "source.txt"), "original source")
	copyTestCheckFile(t, filepath.Join(dst, "user.txt"), "existing contents")
	copyTestCheckFile(t, outsideFile, "existing file")
	copyTestAbsent(t, filepath.Join(src, "child"))
	copyTestNoStage(t, parent)
	copyTestNoStage(t, src)
}

func TestCopyDataRootVerifyFailure(t *testing.T) {
	parent := t.TempDir()
	src, dst := filepath.Join(parent, "source"), filepath.Join(parent, "target")
	copyTestMkdir(t, src)
	copyTestMkdir(t, dst)
	original, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	copyTestWrite(t, filepath.Join(src, "db"), "original database")
	verifyErr := errors.New("invalid database")
	err = CopyDataRoot(src, dst, nil, func(stage string) error {
		copyTestCheckFile(t, filepath.Join(stage, "db"), "original database")
		return verifyErr
	})
	if !errors.Is(err, verifyErr) {
		t.Fatalf("expected wrapped verification error, got %v", err)
	}
	current, err := os.Stat(dst)
	if err != nil || !os.SameFile(original, current) {
		t.Fatalf("existing empty target changed after verify failed: %v", err)
	}
	copyTestAbsent(t, filepath.Join(dst, "db"))
	copyTestCheckFile(t, filepath.Join(src, "db"), "original database")
	copyTestNoStage(t, parent)
}

func TestCopyDataRootTargetAppearsBeforePublish(t *testing.T) {
	for _, existingTarget := range []bool{false, true} {
		name := "target appears"
		if existingTarget {
			name = "empty target populated"
		}
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			src, dst := filepath.Join(parent, "source"), filepath.Join(parent, "target")
			copyTestMkdir(t, src)
			copyTestWrite(t, filepath.Join(src, "db"), "original database")
			if existingTarget {
				copyTestMkdir(t, dst)
			}
			err := CopyDataRoot(src, dst, nil, func(string) error {
				copyTestMkdir(t, dst)
				copyTestWrite(t, filepath.Join(dst, "user.txt"), "new user data")
				return nil
			})
			if err == nil {
				t.Fatal("published over a concurrently populated target")
			}
			copyTestCheckFile(t, filepath.Join(dst, "user.txt"), "new user data")
			copyTestAbsent(t, filepath.Join(dst, "db"))
			copyTestCheckFile(t, filepath.Join(src, "db"), "original database")
			copyTestNoStage(t, parent)
		})
	}
}

func TestCopyDataRootRejectsLinks(t *testing.T) {
	parent := t.TempDir()
	src, outside, dst := filepath.Join(parent, "source"), filepath.Join(parent, "outside"), filepath.Join(parent, "target")
	copyTestMkdir(t, src)
	copyTestMkdir(t, outside)
	copyTestWrite(t, filepath.Join(outside, "user.txt"), "outside data")
	alias := filepath.Join(parent, "alias")
	if err := os.Symlink(src, alias); err != nil {
		if runtime.GOOS == "windows" && errors.Is(err, os.ErrPermission) {
			t.Skipf("no permission to create symlinks: %v", err)
		}
		// Windows may return ERROR_PRIVILEGE_NOT_HELD rather than ErrPermission.
		if runtime.GOOS == "windows" && errors.Is(err, syscall.Errno(1314)) {
			t.Skipf("no privilege to create symlinks: %v", err)
		}
		t.Fatal(err)
	}
	if err := CopyDataRoot(alias, dst, nil, nil); err == nil {
		t.Fatal("accepted a source root symlink")
	}
	if err := CopyDataRoot(src, alias, nil, nil); err == nil {
		t.Fatal("accepted a target root symlink")
	}
	if err := CopyDataRoot(src, filepath.Join(alias, "child"), nil, nil); err == nil {
		t.Fatal("accepted target physically inside source through an alias")
	}
	if err := os.Symlink(outside, filepath.Join(src, "outside-link")); err != nil {
		t.Fatal(err)
	}
	if err := CopyDataRoot(src, dst, nil, nil); err == nil {
		t.Fatal("copied a source symlink")
	}
	copyTestAbsent(t, dst)
	copyTestAbsent(t, filepath.Join(src, "child"))
	copyTestCheckFile(t, filepath.Join(outside, "user.txt"), "outside data")
	copyTestNoStage(t, parent)
}

func copyTestMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

func copyTestWrite(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func copyTestCheckFile(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("unexpected contents in %q: got %d bytes, want %d", path, len(got), len(want))
	}
}

func copyTestAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected absent %q, got %v", path, err)
	}
}

func copyTestNoStage(t *testing.T, parent string) {
	t.Helper()
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".mandaryn-copy-") {
			t.Errorf("leftover staging directory: %s", entry.Name())
		}
	}
}
