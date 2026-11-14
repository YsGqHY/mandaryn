package modelssvc

// 薄壳冒烟测试：只验证装配、类型转换与错误码映射。
// 业务行为（分片 / 迁移 / 校验）在 internal/models 里覆盖，这里不重复。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/models"
	"vip.oberon.mandaryn/internal/paths"
	"vip.oberon.mandaryn/internal/storage"
)

// newService 构造一个只依赖临时目录与假清单的服务。
func newService(t *testing.T, catalogURL string) (*Service, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "modelroot")
	db, err := storage.Open(filepath.Join(t.TempDir(), "mandaryn.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	resolver := paths.New(paths.Options{
		LookupEnv:        func(string) (string, bool) { return "", false },
		DefaultModelRoot: func() (string, error) { return root, nil },
		DefaultDataRoot:  func() (string, error) { return t.TempDir(), nil },
	})
	t.Setenv(EnvCatalogURL, catalogURL)
	t.Setenv(EnvCatalogMirrors, "")

	return New(storage.NewHolder(db), resolver, nil), root
}

// fakeCatalog 起一个只提供清单的假服务。
func fakeCatalog(t *testing.T, bundles []map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/"+models.FileCatalog {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schemaVersion": 1,
			"bundles":       bundles,
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestServiceListAvailable(t *testing.T) {
	srv := fakeCatalog(t, []map[string]any{{
		"bundleId":  "sensevoice-small",
		"name":      "SenseVoice Small",
		"version":   "1.2.0",
		"sizeBytes": 1024,
		"roles":     []string{"offline"},
		"files": []map[string]any{{
			"path":   "model.onnx",
			"sha256": "0000000000000000000000000000000000000000000000000000000000000000",
			"size":   1024,
			"url":    "model.onnx",
		}},
	}})
	svc, _ := newService(t, srv.URL)

	list, err := svc.ListAvailable(context.Background())
	if err != nil {
		t.Fatalf("ListAvailable: %v", err)
	}
	if len(list) != 1 || list[0].BundleID != "sensevoice-small" {
		t.Fatalf("list = %+v", list)
	}
	if list[0].SizeBytes != 1024 || list[0].Installed {
		t.Fatalf("entry = %+v", list[0])
	}
}

func TestServiceErrorMapping(t *testing.T) {
	srv := fakeCatalog(t, nil)
	svc, _ := newService(t, srv.URL)
	ctx := context.Background()

	// 空 bundleId → 参数错误。
	if _, err := svc.Download(ctx, "  "); !apierr.HasCode(err, apierr.CodeInvalidArgument) {
		t.Fatalf("empty bundleId error = %v", err)
	}
	// 未安装的包 → MODEL_NOT_FOUND。
	if _, err := svc.Verify(ctx, "nope"); !apierr.HasCode(err, apierr.CodeModelNotFound) {
		t.Fatalf("verify error = %v", err)
	}
	if err := svc.Remove(ctx, "nope"); !apierr.HasCode(err, apierr.CodeModelNotFound) {
		t.Fatalf("remove error = %v", err)
	}
	// 暂停一个不存在的下载 → NOT_FOUND。
	if err := svc.PauseDownload(ctx, "nope"); !apierr.HasCode(err, apierr.CodeNotFound) {
		t.Fatalf("pause error = %v", err)
	}
	// 离线导入：空路径 / 不存在的文件。
	if _, err := svc.ImportOffline(ctx, " "); !apierr.HasCode(err, apierr.CodeInvalidArgument) {
		t.Fatalf("empty archive error = %v", err)
	}
	if _, err := svc.ImportOffline(ctx, filepath.Join(t.TempDir(), "missing.zip")); !apierr.HasCode(err, apierr.CodeNotFound) {
		t.Fatalf("missing archive error = %v", err)
	}
	// 没有迁移在跑时取消 → 状态错误。
	if err := svc.CancelMigration(ctx); err == nil {
		t.Fatal("cancel without migration must fail")
	}
}

func TestServiceStoreStatsAndPlan(t *testing.T) {
	srv := fakeCatalog(t, nil)
	svc, root := newService(t, srv.URL)
	ctx := context.Background()

	stats, err := svc.GetStoreStats(ctx)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.Root != root {
		t.Fatalf("stats root = %s, want %s", stats.Root, root)
	}

	target := filepath.Join(t.TempDir(), "newroot")
	plan, err := svc.SetModelRoot(ctx, target)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if !plan.Feasible || plan.To != target {
		t.Fatalf("plan = %+v", plan)
	}

	// 同路径应被预检拒绝。
	if _, err := svc.SetModelRoot(ctx, root); !apierr.HasCode(err, apierr.CodeMigratePrecheck) {
		t.Fatalf("same-path plan error = %v", err)
	}

	// 迁移状态初始为 idle。
	st, err := svc.GetMigrationState(ctx)
	if err != nil {
		t.Fatalf("migration state: %v", err)
	}
	if st.Phase != contract.MigrateIdle {
		t.Fatalf("phase = %s, want idle", st.Phase)
	}
}

func TestServiceCatalogHelpers(t *testing.T) {
	t.Setenv(EnvCatalogURL, "https://example.com/dir")
	if got := catalogURL(); got != "https://example.com/dir" {
		t.Fatalf("catalogURL = %s", got)
	}
	t.Setenv(EnvCatalogMirrors, " https://a , , https://b ")
	got := catalogMirrors()
	if len(got) != 2 || got[0] != "https://a" || got[1] != "https://b" {
		t.Fatalf("catalogMirrors = %v", got)
	}

	// 未设环境变量时回落到内置默认值。
	t.Setenv(EnvCatalogURL, "")
	if got := catalogURL(); got != defaultCatalogURL {
		t.Fatalf("default catalogURL = %s", got)
	}
	t.Setenv(EnvCatalogMirrors, "")
	if got := catalogMirrors(); len(got) != 1 || got[0] != defaultCatalogMirror {
		t.Fatalf("default mirrors = %v", got)
	}
}

func TestServiceValueHelpers(t *testing.T) {
	if _, err := requireBundleID(""); !apierr.HasCode(err, apierr.CodeInvalidArgument) {
		t.Fatalf("requireBundleID(\"\") = %v", err)
	}
	if id, err := requireBundleID(" abc "); err != nil || id != "abc" {
		t.Fatalf("requireBundleID = %q, %v", id, err)
	}
	if e := mapErr(os.ErrNotExist, apierr.CodeNotFound); !apierr.HasCode(e, apierr.CodeNotFound) {
		t.Fatalf("mapErr = %v", e)
	}
	if e := mapErr(apierr.New(apierr.CodeModelBusy), apierr.CodeNotFound); !apierr.HasCode(e, apierr.CodeModelBusy) {
		t.Fatalf("mapErr must preserve existing codes, got %v", e)
	}
}

// TestServiceMigratePreflightFailureVisibleInState 锁定一处真实缺陷的回归：
// 预检失败时 L2 曾只返回错误对象、不更新对外状态，导致 GetMigrationState
// 一直返回上一次的 done/idle，前端把失败的迁移显示成「已完成」。
func TestServiceMigratePreflightFailureVisibleInState(t *testing.T) {
	srv := fakeCatalog(t, nil)
	svc, root := newService(t, srv.URL)
	ctx := context.Background()

	// 目标路径被一个同名文件占住 → preflight 必然失败。
	blocked := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(blocked, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed blocker: %v", err)
	}
	if _, err := svc.MigrateModelRoot(ctx, contract.MigrateRequest{Target: blocked}); !apierr.HasCode(err, apierr.CodeMigratePrecheck) {
		t.Fatalf("MigrateModelRoot error = %v, want %s", err, apierr.CodeMigratePrecheck)
	}

	st, err := svc.GetMigrationState(ctx)
	if err != nil {
		t.Fatalf("GetMigrationState: %v", err)
	}
	if st.Phase != contract.MigrateFailed {
		t.Fatalf("phase = %s, want failed", st.Phase)
	}
	if st.LastError == nil || st.LastError.Code != apierr.CodeMigratePrecheck {
		t.Fatalf("lastError = %+v, want code %s", st.LastError, apierr.CodeMigratePrecheck)
	}
	// 旧根仍可用：包列表读得到（空列表也算成功）。
	if _, lerr := svc.ListInstalled(ctx); lerr != nil {
		t.Fatalf("ListInstalled after failed preflight: %v", lerr)
	}
	if stats, serr := svc.GetStoreStats(ctx); serr != nil || stats.Root != root {
		t.Fatalf("stats = %+v, err = %v", stats, serr)
	}
}

// TestServiceMigrateEndToEnd 走一遍薄壳层的完整迁移：预检 → 执行 → 状态收敛 → 新根生效。
func TestServiceMigrateEndToEnd(t *testing.T) {
	srv := fakeCatalog(t, nil)
	svc, root := newService(t, srv.URL)
	ctx := context.Background()

	// 造一个已装包（含 manifest.json），迁移后应仍被识别。
	dir := filepath.Join(root, models.DirModels, "sensevoice-small@1.2.0")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir bundle: %v", err)
	}
	body := []byte("weights")
	if err := os.WriteFile(filepath.Join(dir, "model.onnx"), body, 0o644); err != nil {
		t.Fatalf("write weights: %v", err)
	}
	manifest := `{"bundleId":"sensevoice-small","name":"SenseVoice Small","version":"1.2.0",` +
		`"roles":["offline"],"sha256":"","files":[{"path":"model.onnx","sha256":""}]}`
	if err := os.WriteFile(filepath.Join(dir, models.FileBundleManifest), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	target := filepath.Join(t.TempDir(), "newroot")
	plan, err := svc.SetModelRoot(ctx, target)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if !plan.Feasible || plan.BundleCount != 1 {
		t.Fatalf("plan = %+v", plan)
	}
	if _, err := svc.MigrateModelRoot(ctx, contract.MigrateRequest{Target: target, CopyOnly: true}); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// 轮询兜底接口（API-6.12）直到终态。
	deadline := time.Now().Add(30 * time.Second)
	var st contract.MigrateState
	for time.Now().Before(deadline) {
		st, err = svc.GetMigrationState(ctx)
		if err != nil {
			t.Fatalf("state: %v", err)
		}
		if st.Phase == contract.MigrateDone || st.Phase == contract.MigrateRollback || st.Phase == contract.MigrateFailed {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if st.Phase != contract.MigrateDone {
		t.Fatalf("final phase = %s (err=%+v)", st.Phase, st.LastError)
	}

	stats, err := svc.GetStoreStats(ctx)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.Root != target {
		t.Fatalf("stats root = %s, want %s", stats.Root, target)
	}
	if len(stats.Bundles) != 1 || stats.Bundles[0].BundleID != "sensevoice-small" {
		t.Fatalf("bundles = %+v", stats.Bundles)
	}
	// 占用体积按整目录统计（含 manifest.json），这里只要求它覆盖到权重文件。
	if stats.Bundles[0].SizeBytes < int64(len(body)) {
		t.Fatalf("bundle size = %d, want >= %d", stats.Bundles[0].SizeBytes, len(body))
	}
}
