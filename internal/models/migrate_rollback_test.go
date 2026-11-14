package models

// 迁移回滚与一致性的**逐阶段**验证（FR-07-3/4/5、ARCH §4.6.3）。
//
// 本文件刻意与 migrate_test.go 分工：
//   - migrate_test.go 管主流程（阶段顺序、原子切换、互斥）；
//   - 这里管「坏路径」：preflight / copying / verifying / switching 各自失败、
//     各阶段用户取消、以及最要紧的一条 —— **失败路径绝不清旧目录**。
//
// 判定口径统一用 snapshotTree：迁移前后对整个模型根做一次「相对路径 → 内容摘要」
// 快照，只要有一个文件被删 / 被改 / 被移走，断言立刻失败。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/storage"
)

// snapshotTree 返回目录的「相对路径 → sha256」快照（跳过临时区 / 待删区）。
func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := make(map[string]string)
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != root && shouldSkipEntry(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		sum, _, herr := sha256File(p)
		if herr != nil {
			return herr
		}
		out[filepath.ToSlash(rel)] = sum
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %s: %v", root, err)
	}
	return out
}

// assertSnapshotEqual 断言目录内容与快照完全一致（多一个少一个都不算）。
func assertSnapshotEqual(t *testing.T, root string, want map[string]string) {
	t.Helper()
	got := snapshotTree(t, root)
	if len(got) != len(want) {
		t.Fatalf("%s 文件数变了：got %d, want %d（got=%v）", root, len(got), len(want), sortedKeys(got))
	}
	for rel, wantSum := range want {
		gotSum, ok := got[rel]
		if !ok {
			t.Fatalf("%s 丢了文件 %s", root, rel)
		}
		if gotSum != wantSum {
			t.Fatalf("%s 的 %s 内容变了", root, rel)
		}
	}
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// stagingDirs 返回目录下所有 `.staging-*` 残留。
func stagingDirs(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read dir %s: %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), DirStagingPrefix) {
			out = append(out, e.Name())
		}
	}
	return out
}

// assertNoStagingIn 断言没有迁移临时区残留（「新目录不留半成品」）。
func assertNoStagingIn(t *testing.T, dir string) {
	t.Helper()
	if dirs := stagingDirs(t, dir); len(dirs) > 0 {
		t.Fatalf("%s 残留临时区：%v", dir, dirs)
	}
}

// openMigrateDB 打开临时库并把 app_settings.model_root 预置成 root。
//
// 预置的意义：回滚断言可以直接读「库里到底指哪儿」，而不是只看内存里的
// rootOverride —— 二者不一致正是「一半新一半旧」的典型形态。
func openMigrateDB(t *testing.T, root string) (*storage.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mandaryn.db")
	db, err := storage.Open(path)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	// storage.Open 已经建好 id=1 的默认行，这里改成「用户已选过根目录」的状态。
	if err := db.GORM.Model(&storage.AppSettings{}).Where("id = ?", 1).
		Update("model_root", root).Error; err != nil {
		t.Fatalf("seed app_settings: %v", err)
	}
	return db, path
}

// readPersistedRoot 读 app_settings.model_root。
func readPersistedRoot(t *testing.T, db *storage.DB) string {
	t.Helper()
	var row storage.AppSettings
	if err := db.GORM.First(&row, 1).Error; err != nil {
		t.Fatalf("read app_settings: %v", err)
	}
	return row.ModelRoot
}

// runWithCancelAt 在指定阶段注入「用户取消」，其余逻辑照常执行。
func runWithCancelAt(t *testing.T, root, target, phase string) (*Manager, contract.MigrateState) {
	t.Helper()
	var (
		once    sync.Once
		cancelM *Manager
	)
	opts := testOptions(root, "http://127.0.0.1:1")
	opts.OnMigrate = func(st contract.MigrateState) {
		if st.Phase != phase {
			return
		}
		once.Do(func() { _ = cancelM.CancelMigration(context.Background()) })
	}
	cancelM = newRawManager(t, opts)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := cancelM.RunMigration(ctx, contract.MigrateRequest{Target: target}); err != nil {
		t.Fatalf("run migration: %v", err)
	}
	return cancelM, cancelM.AwaitMigration(ctx)
}

// —— preflight 失败 ——

func TestMigratePreflightFailureKeepsOldRootAndReportsFailed(t *testing.T) {
	root, _ := seedModelRoot(t)
	before := snapshotTree(t, root)
	parent := t.TempDir()
	// 目标路径上已经存在一个**同名文件**：建目录必然失败 → preflight 不通过。
	blocked := filepath.Join(parent, "not-a-dir")
	if err := os.WriteFile(blocked, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed blocker file: %v", err)
	}

	m := newRawManager(t, testOptions(root, "http://127.0.0.1:1"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := m.RunMigration(ctx, contract.MigrateRequest{Target: blocked})
	if !apierr.HasCode(err, apierr.CodeMigratePrecheck) {
		t.Fatalf("preflight error = %v, want %s", err, apierr.CodeMigratePrecheck)
	}

	// 对外状态必须收敛到 failed 并带原因：否则 GetMigrationState 会返回上一次的
	// done/idle，前端会把一次失败的迁移显示成「已完成」。
	st := m.MigrationState(ctx)
	if st.Phase != contract.MigrateFailed {
		t.Fatalf("state phase = %s, want failed", st.Phase)
	}
	if st.LastError == nil || st.LastError.Code != apierr.CodeMigratePrecheck {
		t.Fatalf("state lastError = %+v, want code %s", st.LastError, apierr.CodeMigratePrecheck)
	}

	// 旧目录必须原样可用，且没有任何临时区 / 半成品。
	assertSnapshotEqual(t, root, before)
	assertNoStagingIn(t, root)
	assertNoStagingIn(t, parent)
	if got, _ := m.Root(); !samePath(got, root) {
		t.Fatalf("manager root = %s, want %s", got, root)
	}

	// 预检失败后互斥必须释放（否则整个模型仓库会一直 ERR_MODEL_BUSY）。
	if _, lerr := m.ListInstalled(ctx); apierr.HasCode(lerr, apierr.CodeModelBusy) {
		t.Fatalf("manager still busy after preflight failure: %v", lerr)
	}
}

func TestMigratePreflightRejectsNeverTouchSource(t *testing.T) {
	root, _ := seedModelRoot(t)
	before := snapshotTree(t, root)
	m := newRawManager(t, testOptions(root, "http://127.0.0.1:1"))
	ctx := context.Background()

	inside := filepath.Join(root, "nested")
	cases := map[string]string{
		"empty":  "",
		"same":   root,
		"nested": inside,
		"parent": filepath.Dir(root),
	}
	for name, target := range cases {
		if _, err := m.RunMigration(ctx, contract.MigrateRequest{Target: target}); !apierr.HasCode(err, apierr.CodeMigratePrecheck) {
			t.Fatalf("%s: error = %v, want %s", name, err, apierr.CodeMigratePrecheck)
		}
		assertSnapshotEqual(t, root, before)
		assertNoStagingIn(t, root)
	}
	// 被拒绝的目标不能因为一次预检就被创建出来。
	if _, err := os.Stat(inside); !os.IsNotExist(err) {
		t.Fatalf("rejected target was created: %v", err)
	}
}

// —— copying 失败 ——

func TestMigrateCopyFailureRollsBackAndKeepsSource(t *testing.T) {
	root, payloads := seedModelRoot(t)
	before := snapshotTree(t, root)
	target := filepath.Join(t.TempDir(), "target")

	db, _ := openMigrateDB(t, root)

	// 注入：复制到第二个文件时把源文件换成同名目录（读目录必然失败）。
	victim := filepath.Join(root, filepath.FromSlash("models/sensevoice-small@1.2.0/tokens.txt"))
	var once sync.Once
	opts := testOptions(root, "http://127.0.0.1:1")
	opts.Holder = storage.NewHolder(db)
	opts.OnMigrate = func(st contract.MigrateState) {
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
		t.Fatalf("final phase = %s (err=%+v)", st.Phase, st.LastError)
	}
	if st.LastError == nil {
		t.Fatal("故障回滚必须带 lastError")
	}

	// 除被测试主动破坏的那个文件以外，其余内容一字未动。
	for rel, want := range payloads {
		if rel == "models/sensevoice-small@1.2.0/tokens.txt" {
			continue
		}
		got, rerr := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if rerr != nil {
			t.Fatalf("旧目录在回滚后不可用（%s）：%v", rel, rerr)
		}
		if string(got) != string(want) {
			t.Fatalf("旧目录内容被改动（%s）", rel)
		}
	}
	assertNoStagingIn(t, target)
	if got, _ := m.Root(); !samePath(got, root) {
		t.Fatalf("manager root = %s, want %s", got, root)
	}
	if persisted := readPersistedRoot(t, db); !samePath(persisted, root) {
		t.Fatalf("app_settings.model_root = %s, want %s", persisted, root)
	}
	_ = before
}

// —— verifying 失败 ——

func TestMigrateVerifyFailureRollsBackAndCleansTarget(t *testing.T) {
	root, _ := seedModelRoot(t)
	before := snapshotTree(t, root)
	target := filepath.Join(t.TempDir(), "target")
	db, _ := openMigrateDB(t, root)

	// 注入：copy 全部成功、校验开始前改坏 staging 里的一个已复制文件，
	// 使目标摘要与源摘要不一致（模拟介质错误 / 传输篡改）。
	var once sync.Once
	opts := testOptions(root, "http://127.0.0.1:1")
	opts.Holder = storage.NewHolder(db)
	opts.OnMigrate = func(st contract.MigrateState) {
		if st.Phase != contract.MigrateVerifying {
			return
		}
		once.Do(func() {
			dirs := stagingDirs(t, target)
			if len(dirs) == 0 {
				t.Errorf("verifying 阶段找不到临时区")
				return
			}
			corrupt := filepath.Join(target, dirs[0], DirModels, "sensevoice-small@1.2.0", "model.onnx")
			f, oerr := os.OpenFile(corrupt, os.O_WRONLY|os.O_APPEND, 0o644)
			if oerr != nil {
				t.Errorf("open for corrupt: %v", oerr)
				return
			}
			_, _ = f.Write([]byte("corrupt"))
			_ = f.Close()
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
	if st.LastError == nil || st.LastError.Code != apierr.CodeMigrateVerify {
		t.Fatalf("lastError = %+v, want code %s", st.LastError, apierr.CodeMigrateVerify)
	}

	// 关键：校验失败绝不能动旧目录，也绝不能在新目录留下半成品。
	assertSnapshotEqual(t, root, before)
	assertNoStagingIn(t, target)
	if persisted := readPersistedRoot(t, db); !samePath(persisted, root) {
		t.Fatalf("app_settings.model_root = %s, want %s", persisted, root)
	}
	if got, _ := m.Root(); !samePath(got, root) {
		t.Fatalf("manager root = %s, want %s", got, root)
	}
	// 新目录里不该出现可用的 models 内容（staging 已被清掉）。
	if entries, err := os.ReadDir(filepath.Join(target, DirModels)); err == nil && len(entries) > 0 {
		t.Fatalf("目标目录残留已切换内容：%v", entries)
	}
}

// —— switching 失败 ——

func TestMigrateSwitchFailureRollsBackToOldRoot(t *testing.T) {
	root, _ := seedModelRoot(t)
	before := snapshotTree(t, root)
	target := filepath.Join(t.TempDir(), "target")
	db, dbPath := openMigrateDB(t, root)

	// 注入：进入 switching 时关闭数据库 —— 落库这一步必然失败，
	// 于是「先写库再 rename」的第一枪就打不响（现实里等价于库被锁 / 盘被拔）。
	var once sync.Once
	opts := testOptions(root, "http://127.0.0.1:1")
	opts.Holder = storage.NewHolder(db)
	opts.OnMigrate = func(st contract.MigrateState) {
		if st.Phase != contract.MigrateSwitching {
			return
		}
		once.Do(func() { _ = db.Close() })
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
	if st.LastError == nil || st.LastError.Code != apierr.CodeMigrateSwitch {
		t.Fatalf("lastError = %+v, want code %s", st.LastError, apierr.CodeMigrateSwitch)
	}

	// 旧目录完整；新目录既没有半成品也没有 sqlite 指向它。
	assertSnapshotEqual(t, root, before)
	assertNoStagingIn(t, target)
	if got, _ := m.Root(); !samePath(got, root) {
		t.Fatalf("manager root = %s, want %s", got, root)
	}
	// 重新开一次库确认落盘值仍指旧根（切换阶段的写库没有隐式成功）。
	fresh, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer func() { _ = fresh.Close() }()
	if persisted := readPersistedRoot(t, fresh); !samePath(persisted, root) {
		t.Fatalf("app_settings.model_root = %s, want %s", persisted, root)
	}
}

// —— 用户取消（各阶段各一条断言）——

func TestMigrateCancelDuringCopyingKeepsOldRoot(t *testing.T) {
	root, _ := seedModelRoot(t)
	before := snapshotTree(t, root)
	target := filepath.Join(t.TempDir(), "target")

	m, st := runWithCancelAt(t, root, target, contract.MigrateCopying)
	if st.Phase != contract.MigrateRollback {
		t.Fatalf("final phase = %s, want rollback", st.Phase)
	}
	// 用户取消不是故障：LastError 必须为空，前端才画「已取消」而不是「失败」。
	if st.LastError != nil {
		t.Fatalf("取消回滚不应带 lastError：%+v", st.LastError)
	}
	assertSnapshotEqual(t, root, before)
	assertNoStagingIn(t, target)
	if got, _ := m.Root(); !samePath(got, root) {
		t.Fatalf("manager root = %s, want %s", got, root)
	}
}

func TestMigrateCancelDuringVerifyingRollsBack(t *testing.T) {
	root, _ := seedModelRoot(t)
	before := snapshotTree(t, root)
	target := filepath.Join(t.TempDir(), "target")

	// 校验阶段是「最后一个安全取消点」：此时源目录完好、还没切换，
	// 必须尊重取消而不是把目录切走。
	m, st := runWithCancelAt(t, root, target, contract.MigrateVerifying)
	if st.Phase != contract.MigrateRollback {
		t.Fatalf("final phase = %s, want rollback", st.Phase)
	}
	if st.LastError != nil {
		t.Fatalf("取消回滚不应带 lastError：%+v", st.LastError)
	}
	assertSnapshotEqual(t, root, before)
	assertNoStagingIn(t, target)
	if got, _ := m.Root(); !samePath(got, root) {
		t.Fatalf("manager root = %s, want %s", got, root)
	}
}

func TestMigrateCancelDuringCleaningCompletesButKeepsSource(t *testing.T) {
	root, payloads := seedModelRoot(t)
	sourceBefore := snapshotTree(t, root)
	target := filepath.Join(t.TempDir(), "target")

	// 已切换成功后再取消：不回滚（搬回去比留着风险大），但**保留旧目录**，
	// 让用户自己决定删哪一份 —— 绝不能删掉仍是唯一可用副本的数据。
	m, st := runWithCancelAt(t, root, target, contract.MigrateCleaning)
	if st.Phase != contract.MigrateDone {
		t.Fatalf("final phase = %s, want done", st.Phase)
	}
	assertSnapshotEqual(t, root, sourceBefore)
	assertNoStagingIn(t, target)
	for rel, want := range payloads {
		got, rerr := os.ReadFile(filepath.Join(target, filepath.FromSlash(rel)))
		if rerr != nil {
			t.Fatalf("新目录缺少 %s：%v", rel, rerr)
		}
		if string(got) != string(want) {
			t.Fatalf("新目录 %s 内容不一致", rel)
		}
	}
	if got, _ := m.Root(); !samePath(got, target) {
		t.Fatalf("manager root = %s, want %s", got, target)
	}
}

func TestMigrateCancelAfterSwitchKeepsSourceDir(t *testing.T) {
	root, payloads := seedModelRoot(t)
	target := filepath.Join(t.TempDir(), "target")

	// 在 switching 阶段取消：切换本身不可中断，收尾时据此保留源目录。
	m, st := runWithCancelAt(t, root, target, contract.MigrateSwitching)
	if st.Phase != contract.MigrateDone {
		t.Fatalf("final phase = %s, want done", st.Phase)
	}
	if _, err := os.Stat(filepath.Join(root, "manifest.cache.json")); err != nil {
		t.Fatalf("取消后源目录被删：%v", err)
	}
	for rel := range payloads {
		if _, err := os.Stat(filepath.Join(target, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("新目录缺少 %s：%v", rel, err)
		}
	}
	assertNoStagingIn(t, target)
	if got, _ := m.Root(); !samePath(got, target) {
		t.Fatalf("manager root = %s, want %s", got, target)
	}
}

// —— 复制模式 ——

func TestMigrateCopyOnlyKeepsSource(t *testing.T) {
	root, payloads := seedModelRoot(t)
	before := snapshotTree(t, root)
	target := filepath.Join(t.TempDir(), "target")

	m := newRawManager(t, testOptions(root, "http://127.0.0.1:1"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := m.RunMigration(ctx, contract.MigrateRequest{Target: target, CopyOnly: true}); err != nil {
		t.Fatalf("run migration: %v", err)
	}
	if st := m.AwaitMigration(ctx); st.Phase != contract.MigrateDone {
		t.Fatalf("final phase = %s (err=%+v)", st.Phase, st.LastError)
	}

	// 复制模式：两份共存，源目录一字不动。
	assertSnapshotEqual(t, root, before)
	for rel, want := range payloads {
		got, rerr := os.ReadFile(filepath.Join(target, filepath.FromSlash(rel)))
		if rerr != nil {
			t.Fatalf("新目录缺少 %s：%v", rel, rerr)
		}
		if string(got) != string(want) {
			t.Fatalf("新目录 %s 内容不一致", rel)
		}
	}
	assertNoStagingIn(t, target)
}

// —— 注册表（FR-07-5 的 bundles 表）——

// seedBundleWithManifest 造一个「像真的」已装包：目录内含 manifest.json。
func seedBundleWithManifest(t *testing.T, root string) ManifestBundle {
	t.Helper()
	dir := filepath.Join(root, DirModels, "sensevoice-small@1.2.0")
	files := map[string][]byte{
		"model.onnx": payload(1, 64),
		"tokens.txt": payload(2, 16),
	}
	b := ManifestBundle{
		BundleID:  "sensevoice-small",
		Name:      "SenseVoice Small",
		Version:   "1.2.0",
		Roles:     []string{RoleOffline},
		Languages: []string{"zh"},
		License:   "MIT",
		RAMHintMB: 512,
		SHA256:    sha256Hex(payload(1, 64)),
	}
	var total int64
	for name, data := range files {
		writeFile(t, filepath.Join(dir, name), data)
		b.Files = append(b.Files, ManifestFile{Path: name, SHA256: sha256Hex(data), Size: int64(len(data))})
		total += int64(len(data))
	}
	b.SizeBytes = total
	buf, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	writeFile(t, filepath.Join(dir, FileBundleManifest), buf)
	writeFile(t, filepath.Join(root, FileManifestCache), []byte(`{"schemaVersion":1,"bundles":[]}`))
	return b
}

func TestMigrateRegistryConsistentAcrossMigration(t *testing.T) {
	root := filepath.Join(t.TempDir(), "modelroot")
	bundle := seedBundleWithManifest(t, root)
	sourceBefore := snapshotTree(t, root)
	target := filepath.Join(t.TempDir(), "target")

	db, _ := openMigrateDB(t, root)
	opts := testOptions(root, "http://127.0.0.1:1")
	opts.Holder = storage.NewHolder(db)
	m := newRawManager(t, opts)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 迁移前：注册表里有一行，且扫描出来的包状态是 installed。
	if err := db.GORM.Create(&storage.Bundle{
		ID: bundleDirName(bundle.BundleID, bundle.Version), BundleID: bundle.BundleID,
		Version: bundle.Version, Name: bundle.Name, Path: DirModels + "/sensevoice-small@1.2.0",
		SizeBytes: bundle.SizeBytes, SHA256: bundle.SHA256, VerifiedAt: 1700000000000,
		State: contract.BundleInstalled, Roles: `["offline"]`, Languages: `["zh"]`,
		License: bundle.License, RAMHintMB: bundle.RAMHintMB,
	}).Error; err != nil {
		t.Fatalf("seed bundle row: %v", err)
	}
	rowsBefore := readBundleRows(t, db)
	if len(rowsBefore) != 1 {
		t.Fatalf("bundles 行数 = %d, want 1", len(rowsBefore))
	}
	for _, key := range []string{"id", "bundle_id", "version", "sha256", "size_bytes", "state", "path"} {
		if strings.TrimSpace(fmt.Sprint(rowsBefore[0][key])) == "" {
			t.Fatalf("注册表缺字段 %s：%+v", key, rowsBefore[0])
		}
	}
	installedBefore, err := m.ListInstalled(ctx)
	if err != nil {
		t.Fatalf("list installed: %v", err)
	}
	if len(installedBefore) != 1 || installedBefore[0].State != contract.BundleInstalled {
		t.Fatalf("迁移前已装列表 = %+v", installedBefore)
	}
	statsBefore, err := m.Stats(ctx)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}

	// 迁移。
	if _, err := m.RunMigration(ctx, contract.MigrateRequest{Target: target}); err != nil {
		t.Fatalf("run migration: %v", err)
	}
	if st := m.AwaitMigration(ctx); st.Phase != contract.MigrateDone {
		t.Fatalf("final phase = %s (err=%+v)", st.Phase, st.LastError)
	}

	// 迁移后：注册表行逐字段一致（path 是相对 modelRoot，因此不该变）。
	rowsAfter := readBundleRows(t, db)
	if len(rowsAfter) != len(rowsBefore) {
		t.Fatalf("迁移改了注册表行数：%d → %d", len(rowsBefore), len(rowsAfter))
	}
	for i := range rowsBefore {
		for key, want := range rowsBefore[i] {
			got := rowsAfter[i][key]
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("注册表字段 %s 变了：%v → %v", key, want, got)
			}
		}
	}

	// 迁移后还能从新根解析出同一个包，占用体积一致（迁移不丢不重）。
	installedAfter, err := m.ListInstalled(ctx)
	if err != nil {
		t.Fatalf("list installed after: %v", err)
	}
	if len(installedAfter) != len(installedBefore) {
		t.Fatalf("迁移后已装列表 = %+v", installedAfter)
	}
	if installedAfter[0].BundleID != installedBefore[0].BundleID ||
		installedAfter[0].Version != installedBefore[0].Version ||
		installedAfter[0].SHA256 != installedBefore[0].SHA256 {
		t.Fatalf("迁移后包标识 / 版本 / 校验信息变了：%+v → %+v", installedBefore[0], installedAfter[0])
	}
	if installedAfter[0].State != contract.BundleInstalled {
		t.Fatalf("迁移后状态 = %s, want installed", installedAfter[0].State)
	}
	if installedAfter[0].SizeBytes != installedBefore[0].SizeBytes {
		t.Fatalf("迁移后体积变了：%d → %d", installedBefore[0].SizeBytes, installedAfter[0].SizeBytes)
	}
	statsAfter, err := m.Stats(ctx)
	if err != nil {
		t.Fatalf("stats after: %v", err)
	}
	if !samePath(statsAfter.Root, target) {
		t.Fatalf("StoreStats.root = %s, want %s", statsAfter.Root, target)
	}
	if statsAfter.TotalBytes != statsBefore.TotalBytes {
		t.Fatalf("仓库总占用变了：%d → %d", statsBefore.TotalBytes, statsAfter.TotalBytes)
	}

	// 引擎定位仍能命中（迁移后模型立即可用，FR-07-3）。
	dir, id, lerr := m.LocateBundle(RoleOffline)
	if lerr != nil {
		t.Fatalf("locate bundle after migration: %v", lerr)
	}
	if id != bundle.BundleID || !strings.HasPrefix(filepath.ToSlash(dir), filepath.ToSlash(target)) {
		t.Fatalf("locate = (%s, %s), want bundle %s under %s", dir, id, bundle.BundleID, target)
	}
	// 源目录按策略清理；新目录内容与迁移前逐字节一致。
	if _, serr := os.Stat(root); !os.IsNotExist(serr) {
		t.Fatalf("源目录应已被清理，stat err = %v", serr)
	}
	assertSnapshotEqual(t, target, sourceBefore)
}

// readBundleRows 把 bundles 表读成 map 列表（便于逐字段比对）。
func readBundleRows(t *testing.T, db *storage.DB) []map[string]any {
	t.Helper()
	var rows []storage.Bundle
	if err := db.GORM.Order("id").Find(&rows).Error; err != nil {
		t.Fatalf("read bundles: %v", err)
	}
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, map[string]any{
			"id":          r.ID,
			"bundle_id":   r.BundleID,
			"version":     r.Version,
			"name":        r.Name,
			"path":        r.Path,
			"size_bytes":  r.SizeBytes,
			"sha256":      r.SHA256,
			"verified_at": r.VerifiedAt,
			"state":       r.State,
			"roles":       r.Roles,
			"languages":   r.Languages,
			"license":     r.License,
			"ram_hint_mb": r.RAMHintMB,
		})
	}
	return out
}
