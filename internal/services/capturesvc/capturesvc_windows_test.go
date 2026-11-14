//go:build windows

package capturesvc

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/capture"
	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/storage"
)

// newTestService 构造一个挂在临时库上的服务。
//
// 用 storage.Open 而不是 mock：preferences 的读写是 gorm + upsert 语义，
// 只有跑在真 SQLite 上才能验证「重复写入不会插出两行」这类行为。
func newTestService(t *testing.T) *Service {
	t.Helper()
	dir := t.TempDir()
	db, err := storage.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("打开测试库失败：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return New(storage.NewHolder(db), nil)
}

// TestListSources 与 TestRefreshSources 走同一条枚举路径，只验证不报错、不为 nil。
func TestListSources(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	sources, err := svc.ListSources(ctx)
	if err != nil {
		t.Fatalf("ListSources 失败：%v", err)
	}
	if sources == nil {
		t.Fatal("ListSources 不应返回 nil（前端无需判空）")
	}
	t.Logf("ListSources 返回 %d 个源", len(sources))

	refreshed, err := svc.RefreshSources(ctx)
	if err != nil {
		t.Fatalf("RefreshSources 失败：%v", err)
	}
	if len(refreshed) != len(sources) {
		t.Logf("两次枚举数量不同（%d vs %d），可能是期间有应用启停，属正常",
			len(sources), len(refreshed))
	}

	for _, s := range sources {
		if s.Kind != contract.SourceKindDevice && s.Kind != contract.SourceKindApplication {
			t.Errorf("源类型非法：%q", s.Kind)
		}
		if s.ID == "" || s.DisplayName == "" {
			t.Errorf("源缺少 ID / DisplayName：%+v", s)
		}
	}
}

// TestGetCapabilities 验证薄壳忠实透传领域层能力矩阵。
func TestGetCapabilities(t *testing.T) {
	svc := newTestService(t)
	caps, err := svc.GetCapabilities(context.Background())
	if err != nil {
		t.Fatalf("GetCapabilities 失败：%v", err)
	}
	if caps.Platform != "windows" {
		t.Fatalf("Platform 应为 windows，实际 %q", caps.Platform)
	}
	if !caps.ProcessLoopback && caps.Reason == "" {
		t.Fatal("不支持进程级捕获时必须给出 Reason")
	}
	t.Logf("Capabilities = %+v", caps)
}

// TestProbeSource 覆盖参数校验与「源不存在」两条分支。
func TestProbeSource(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	// 空 ID → 参数错误。
	if _, err := svc.ProbeSource(ctx, "  "); !apierr.HasCode(err, apierr.CodeInvalidArgument) {
		t.Fatalf("空 ID 应返回 %s，实际 %v", apierr.CodeInvalidArgument, err)
	}

	// 非 dev:/app: 前缀 → 参数错误。
	if _, err := svc.ProbeSource(ctx, "kook.exe"); !apierr.HasCode(err, apierr.CodeInvalidArgument) {
		t.Fatalf("非法前缀应返回 %s，实际 %v", apierr.CodeInvalidArgument, err)
	}

	// 不存在的设备 → 源找不到。
	_, err := svc.ProbeSource(ctx, "dev:{0.0.0.00000000}.{00000000-0000-0000-0000-000000000000}")
	if !apierr.HasCode(err, apierr.CodeSourceNotFound) &&
		!apierr.HasCode(err, apierr.CodeCaptureUnavailable) {
		t.Fatalf("不存在的设备应返回 %s，实际 %v", apierr.CodeSourceNotFound, err)
	}

	// 存在的设备 → 成功且 ID 回显一致。
	deviceID := firstDeviceID(t)
	if deviceID == "" {
		t.Skip("本机没有活动输出设备，跳过成功分支")
	}
	src, err := svc.ProbeSource(ctx, deviceID)
	if err != nil {
		t.Fatalf("探测真实设备失败：%v", err)
	}
	if src.ID != deviceID {
		t.Fatalf("探测返回 ID 不符：%s vs %s", src.ID, deviceID)
	}
}

// TestPreferredSource 覆盖「记住上次选择的源」的读写与校验（FR-02-5）。
func TestPreferredSource(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	// 未设置过 → 空字符串，不报错。
	got, err := svc.GetPreferredSource(ctx)
	if err != nil {
		t.Fatalf("GetPreferredSource 失败：%v", err)
	}
	if got != "" {
		t.Fatalf("初始应为空，实际 %q", got)
	}

	// 空 ID 拒绝。
	if err := svc.SetPreferredSource(ctx, ""); !apierr.HasCode(err, apierr.CodeInvalidArgument) {
		t.Fatalf("空 ID 应返回 %s，实际 %v", apierr.CodeInvalidArgument, err)
	}

	// 不存在的源拒绝，且不得污染已存值。
	if err := svc.SetPreferredSource(ctx, "dev:不存在的设备"); err == nil {
		t.Fatal("不存在的源应被拒绝")
	}
	if got, _ := svc.GetPreferredSource(ctx); got != "" {
		t.Fatalf("失败的写入不应落库，实际 %q", got)
	}

	// 真实设备 → 写入成功并可读回。
	deviceID := firstDeviceID(t)
	if deviceID == "" {
		t.Skip("本机没有活动输出设备，跳过成功分支")
	}
	if err := svc.SetPreferredSource(ctx, deviceID); err != nil {
		t.Fatalf("写入首选源失败：%v", err)
	}
	got, err = svc.GetPreferredSource(ctx)
	if err != nil {
		t.Fatalf("读取首选源失败：%v", err)
	}
	if got != deviceID {
		t.Fatalf("首选源不符：%q vs %q", got, deviceID)
	}

	// 重复写入同一个键：upsert，不得插出第二行。
	if err := svc.SetPreferredSource(ctx, deviceID); err != nil {
		t.Fatalf("重复写入失败：%v", err)
	}
	var count int64
	if err := svc.holder.Current().GORM.
		Model(&storage.Preference{}).
		Where("key = ?", prefKeyPreferredSource).
		Count(&count).Error; err != nil {
		t.Fatalf("统计失败：%v", err)
	}
	if count != 1 {
		t.Fatalf("preferences 中应只有 1 行，实际 %d", count)
	}

	// 键名是对外契约的一部分（前端可能直接读 preferences），锁死防止手滑改名。
	if prefKeyPreferredSource != "capture.preferredSource" {
		t.Fatalf("首选源键名被改动：%s", prefKeyPreferredSource)
	}
}

// TestSourcesChangedHandler 验证构造期注入回调后能启动轮询、Shutdown 能停掉且幂等。
//
// 注：早期版本用导出的 SetSourcesChangedHandler(fn func(...)) 装配，但 func 参数会
// 触发 Wails 绑定生成器警告并生成畸形前端方法，故改为构造注入 + ServiceShutdown 生命周期钩子。
func TestSourcesChangedHandler(t *testing.T) {
	dir := t.TempDir()
	db, err := storage.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("打开测试库失败：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	svc := New(storage.NewHolder(db), func([]contract.Source) {})
	if svc.monitor == nil {
		t.Fatal("注入回调后应启动轮询器")
	}
	if err := svc.ServiceShutdown(); err != nil {
		t.Fatalf("Shutdown 报错：%v", err)
	}
	// 幂等：重复 Shutdown 也不能 panic。
	if err := svc.ServiceShutdown(); err != nil {
		t.Fatalf("重复 Shutdown 报错：%v", err)
	}

	// 不注入回调则不启动轮询（退化为「只在 UI 请求时枚举」）。
	plain := New(storage.NewHolder(db), nil)
	if plain.monitor != nil {
		t.Fatal("未注入回调时不应启动轮询器")
	}
}

// firstDeviceID 取当前枚举结果里第一个设备源的 ID。
func firstDeviceID(t *testing.T) string {
	t.Helper()
	sources, err := capture.Enumerate()
	if err != nil {
		t.Logf("枚举失败，按无设备处理：%v", err)
		return ""
	}
	for _, s := range sources {
		if s.Kind == contract.SourceKindDevice && strings.HasPrefix(s.ID, capture.DeviceIDPrefix) {
			return s.ID
		}
	}
	return ""
}
