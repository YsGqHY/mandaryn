package models

// 迁移状态机测试（ARCH §4.6.3）：阶段顺序、原子切换、失败/取消回滚、
// 全局互斥（ERR_MODEL_BUSY）、临时区不残留。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/storage"
)

// newRawManager 构造 Manager 但不覆盖回调，便于测试注入自己的 OnMigrate。
func newRawManager(t *testing.T, opts Options) *Manager {
	t.Helper()
	m := New(opts)
	t.Cleanup(m.shutdownForTest)
	return m
}

// writeFile 写一个测试文件（自动建目录）。
func writeFile(t *testing.T, p string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
}

// seedModelRoot 造一个可迁移的模型根（含清单缓存与一个包）。
func seedModelRoot(t *testing.T) (root string, payloads map[string][]byte) {
	t.Helper()
	root = filepath.Join(t.TempDir(), "modelroot")
	payloads = map[string][]byte{
		"models/sensevoice-small@1.2.0/model.onnx": payload(1, 64),
		"models/sensevoice-small@1.2.0/tokens.txt": payload(2, 16),
		"manifest.cache.json":                      []byte(`{"schemaVersion":1,"bundles":[]}`),
	}
	for rel, data := range payloads {
		writeFile(t, filepath.Join(root, filepath.FromSlash(rel)), data)
	}
	return root, payloads
}

func TestPlanMigrateRejectsInvalidTargets(t *testing.T) {
	root, _ := seedModelRoot(t)
	m := newRawManager(t, testOptions(root, "http://127.0.0.1:1"))

	if _, err := m.PlanMigrate(context.Background(), ""); !apierr.HasCode(err, apierr.CodeMigratePrecheck) {
		t.Fatalf("empty target error = %v", err)
	}
	if _, err := m.PlanMigrate(context.Background(), root); !apierr.HasCode(err, apierr.CodeMigratePrecheck) {
		t.Fatalf("same target error = %v", err)
	}
	inside := filepath.Join(root, "nested")
	if _, err := m.PlanMigrate(context.Background(), inside); !apierr.HasCode(err, apierr.CodeMigratePrecheck) {
		t.Fatalf("nested target error = %v", err)
	}
	// 源在目标之内同样必须拒绝（staging 会落在目标里）。
	if _, err := m.PlanMigrate(context.Background(), filepath.Dir(root)); !apierr.HasCode(err, apierr.CodeMigratePrecheck) {
		t.Fatalf("parent target error = %v", err)
	}

	target := filepath.Join(t.TempDir(), "target")
	plan, err := m.PlanMigrate(context.Background(), target)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if !plan.Feasible {
		t.Fatalf("plan not feasible: %+v", plan)
	}
	if plan.BundleCount != 1 {
		t.Fatalf("bundleCount = %d, want 1", plan.BundleCount)
	}
	if plan.TotalBytes <= 0 || plan.FreeBytes <= 0 {
		t.Fatalf("bytes = %d / free = %d", plan.TotalBytes, plan.FreeBytes)
	}
	if len(plan.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", plan.Warnings)
	}
}

func TestMigrateHappyPath(t *testing.T) {
	root, payloads := seedModelRoot(t)
	target := filepath.Join(t.TempDir(), "target")

	db, err := storage.Open(filepath.Join(t.TempDir(), "mandaryn.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	rec := newStateRecorder()
	opts := testOptions(root, "http://127.0.0.1:1")
	opts.Holder = storage.NewHolder(db)
	opts.OnMigrate = rec.onMigrate

	var invalidated []string
	opts.OnModelRootChanged = func(newRoot string) error {
		invalidated = append(invalidated, newRoot)
		return nil
	}
	m := newRawManager(t, opts)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := m.RunMigration(ctx, contract.MigrateRequest{Target: target}); err != nil {
		t.Fatalf("run migration: %v", err)
	}
	st := m.AwaitMigration(ctx)
	if st.Phase != contract.MigrateDone {
		t.Fatalf("final phase = %s (err=%+v)", st.Phase, st.LastError)
	}

	// 阶段顺序必须严格是 copy → verify → switch → clean。
	order := []string{
		contract.MigrateCopying,
		contract.MigrateVerifying,
		contract.MigrateSwitching,
		contract.MigrateCleaning,
		contract.MigrateDone,
	}
	phases := rec.phases()
	idx := 0
	for _, p := range phases {
		if idx < len(order) && p == order[idx] {
			idx++
		}
	}
	if idx != len(order) {
		t.Fatalf("phase order = %v, want subsequence %v", phases, order)
	}
	if phases[0] != contract.MigratePreflight {
		t.Fatalf("first phase = %s, want preflight", phases[0])
	}

	// 目标必须完整。
	for rel, want := range payloads {
		got, rerr := os.ReadFile(filepath.Join(target, filepath.FromSlash(rel)))
		if rerr != nil {
			t.Fatalf("read %s in target: %v", rel, rerr)
		}
		if string(got) != string(want) {
			t.Fatalf("%s content mismatch after migration", rel)
		}
	}
	assertNoStaging(t, target)

	// 源目录已被改名删除（copyOnly=false）。
	if _, statErr := os.Stat(root); !os.IsNotExist(statErr) {
		t.Fatalf("source root should be gone, stat err = %v", statErr)
	}

	// 本包立刻使用新根，并回调上层失效缓存。
	if got, _ := m.Root(); !samePath(got, target) {
		t.Fatalf("manager root = %s, want %s", got, target)
	}
	if len(invalidated) == 0 || !samePath(invalidated[len(invalidated)-1], target) {
		t.Fatalf("OnModelRootChanged calls = %v", invalidated)
	}

	// DB 单行值必须指向新根。
	var row storage.AppSettings
	if err := db.GORM.First(&row, 1).Error; err != nil {
		t.Fatalf("read app_settings: %v", err)
	}
	if !samePath(row.ModelRoot, target) {
		t.Fatalf("app_settings.model_root = %s, want %s", row.ModelRoot, target)
	}
}

func TestMigrateRollbackOnCopyFailure(t *testing.T) {
	root, payloads := seedModelRoot(t)
	target := filepath.Join(t.TempDir(), "target")

	victim := filepath.Join(root, filepath.FromSlash("models/sensevoice-small@1.2.0/tokens.txt"))
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("victim missing: %v", err)
	}

	var once sync.Once
	opts := testOptions(root, "http://127.0.0.1:1")
	opts.OnMigrate = func(st contract.MigrateState) {
		// 时机：包内第一个文件已复制完（model.onnx），此时 tokens.txt 已被
		// WalkDir 枚列为普通文件；把它换成同名目录，复制时读目录必然报错。
		if st.Phase != contract.MigrateCopying || st.Current != "models/sensevoice-small@1.2.0/model.onnx" {
			return
		}
		once.Do(func() {
			_ = os.Remove(victim)
			_ = os.MkdirAll(victim, 0o755)
		})
	}
	m := newRawManager(t, opts)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := m.RunMigration(ctx, contract.MigrateRequest{Target: target}); err != nil {
		t.Fatalf("run migration: %v", err)
	}
	st := m.AwaitMigration(ctx)
	if st.Phase != contract.MigrateRollback {
		t.Fatalf("final phase = %s, want rollback", st.Phase)
	}
	if st.LastError == nil {
		t.Fatal("rollback without lastError")
	}

	// 旧路径必须仍然可用（除被测试删掉的那个文件以外）。
	if got, rerr := os.ReadFile(filepath.Join(root, filepath.FromSlash("models/sensevoice-small@1.2.0/model.onnx"))); rerr != nil {
		t.Fatalf("old root unusable after rollback: %v", rerr)
	} else if string(got) != string(payloads["models/sensevoice-small@1.2.0/model.onnx"]) {
		t.Fatal("old root content changed")
	}
	if _, rerr := os.Stat(filepath.Join(root, "manifest.cache.json")); rerr != nil {
		t.Fatalf("old root lost manifest cache: %v", rerr)
	}
	// 临时区不得残留。
	assertNoStaging(t, target)
	// 根仍指向旧路径。
	if got, _ := m.Root(); !samePath(got, root) {
		t.Fatalf("manager root = %s, want %s", got, root)
	}
}

func TestMigrateCancelRollsBack(t *testing.T) {
	root, _ := seedModelRoot(t)
	target := filepath.Join(t.TempDir(), "target")

	var (
		once    sync.Once
		cancelM *Manager
		cancelErr error
	)
	opts := testOptions(root, "http://127.0.0.1:1")
	opts.OnMigrate = func(st contract.MigrateState) {
		if st.Phase != contract.MigrateCopying || st.Current == "" {
			return
		}
		once.Do(func() { cancelErr = cancelM.CancelMigration(context.Background()) })
	}
	cancelM = newRawManager(t, opts)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := cancelM.RunMigration(ctx, contract.MigrateRequest{Target: target}); err != nil {
		t.Fatalf("run migration: %v", err)
	}
	st := cancelM.AwaitMigration(ctx)
	if cancelErr != nil {
		t.Fatalf("cancel: %v", cancelErr)
	}
	if st.Phase != contract.MigrateRollback {
		t.Fatalf("final phase = %s, want rollback", st.Phase)
	}
	if _, err := os.Stat(filepath.Join(root, "manifest.cache.json")); err != nil {
		t.Fatalf("old root unusable after cancel: %v", err)
	}
	assertNoStaging(t, target)

	// 没有迁移在跑时取消必须报 NOT_FOUND。
	if err := cancelM.CancelMigration(context.Background()); !apierr.HasCode(err, apierr.CodeNotFound) {
		t.Fatalf("idle cancel error = %v, want %s", err, apierr.CodeNotFound)
	}
}

func TestMigrationBusyBlocksOtherOperations(t *testing.T) {
	root, _ := seedModelRoot(t)
	target := filepath.Join(t.TempDir(), "target")

	var (
		once    sync.Once
		mu      sync.Mutex
		results map[string]error
		migM    *Manager
	)
	opts := testOptions(root, "http://127.0.0.1:1")
	opts.OnMigrate = func(st contract.MigrateState) {
		if st.Phase != contract.MigrateCopying || st.Current == "" {
			return
		}
		once.Do(func() {
			mu.Lock()
			defer mu.Unlock()
			results = map[string]error{
				"download":      func() error { _, err := migM.Download(context.Background(), "sensevoice-small"); return err }(),
				"remove":        migM.Remove(context.Background(), "sensevoice-small"),
				"listInstalled": func() error { _, err := migM.ListInstalled(context.Background()); return err }(),
				"locate":        func() error { _, _, err := migM.LocateBundle(RoleOffline); return err }(),
			}
		})
	}
	migM = newRawManager(t, opts)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := migM.RunMigration(ctx, contract.MigrateRequest{Target: target}); err != nil {
		t.Fatalf("run migration: %v", err)
	}
	if st := migM.AwaitMigration(ctx); st.Phase != contract.MigrateDone {
		t.Fatalf("final phase = %s", st.Phase)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(results) != 4 {
		t.Fatalf("busy probes did not run: %v", results)
	}
	for name, err := range results {
		if !apierr.HasCode(err, apierr.CodeModelBusy) {
			t.Fatalf("%s error = %v, want %s", name, err, apierr.CodeModelBusy)
		}
	}
}

func TestMigrationRejectsConcurrentRun(t *testing.T) {
	root, _ := seedModelRoot(t)
	target := filepath.Join(t.TempDir(), "target")

	var (
		once     sync.Once
		secondErr error
		migM     *Manager
	)
	opts := testOptions(root, "http://127.0.0.1:1")
	opts.OnMigrate = func(st contract.MigrateState) {
		if st.Phase != contract.MigrateCopying || st.Current == "" {
			return
		}
		once.Do(func() {
			_, secondErr = migM.RunMigration(context.Background(), contract.MigrateRequest{Target: target})
		})
	}
	migM = newRawManager(t, opts)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := migM.RunMigration(ctx, contract.MigrateRequest{Target: target}); err != nil {
		t.Fatalf("run: %v", err)
	}
	migM.AwaitMigration(ctx)
	if !apierr.HasCode(secondErr, apierr.CodeModelBusy) {
		t.Fatalf("second RunMigration error = %v, want %s", secondErr, apierr.CodeModelBusy)
	}
}

func TestCopyTreeSkipsTrashAndStaging(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	dst := filepath.Join(t.TempDir(), "dst")
	writeFile(t, filepath.Join(src, "models", "a@1.0.0", "f.bin"), []byte("keep"))
	writeFile(t, filepath.Join(src, DirTrashPrefix+"old-x", "f.bin"), []byte("drop"))
	writeFile(t, filepath.Join(src, DirStagingPrefix+"dead", "f.bin"), []byte("drop"))

	hashes, _, err := copyTree(context.Background(), src, dst, nil)
	if err != nil {
		t.Fatalf("copyTree: %v", err)
	}
	if len(hashes) != 1 {
		t.Fatalf("copied %d files, want 1 (%v)", len(hashes), hashes)
	}
	if _, err := os.Stat(filepath.Join(dst, "models", "a@1.0.0", "f.bin")); err != nil {
		t.Fatalf("expected file missing: %v", err)
	}
	entries, _ := os.ReadDir(dst)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), DirTrashPrefix) || strings.HasPrefix(e.Name(), DirStagingPrefix) {
			t.Fatalf("temporary dir copied: %s", e.Name())
		}
	}
}

func TestCopyTreeHonoursCancellation(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	writeFile(t, filepath.Join(src, "a.bin"), payload(1, 8))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := copyTree(ctx, src, filepath.Join(t.TempDir(), "dst"), nil); err == nil {
		t.Fatal("copyTree must fail on a canceled context")
	}
}

func TestIsNetworkPath(t *testing.T) {
	cases := map[string]bool{
		`\\server\share\models`: true,
		"//server/share/models": true,
		`C:\models`:             false,
		"/home/user/models":     false,
		"":                      false,
	}
	for in, want := range cases {
		if got := isNetworkPath(in); got != want {
			t.Errorf("isNetworkPath(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestSamePath(t *testing.T) {
	dir := t.TempDir()
	if !samePath(dir, dir) {
		t.Fatal("same path not detected")
	}
	if samePath(dir, filepath.Join(dir, "sub")) {
		t.Fatal("different paths reported as same")
	}
	if !samePath(dir+string(os.PathSeparator), dir) {
		t.Fatal("trailing separator not normalized")
	}
}
