package enginesvc

import (
	"context"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/asr"
	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/paths"
	"vip.oberon.mandaryn/internal/services/appsettings"
	"vip.oberon.mandaryn/internal/storage"
)

// stubEngine 是最小引擎替身（enginesvc 不关心转写细节，只要能 Warm/Health）。
type stubEngine struct {
	id      string
	warmErr error
	closed  atomic.Int32
}

func (s *stubEngine) ID() string { return s.id }
func (s *stubEngine) Warm(_ context.Context) error {
	return s.warmErr
}
func (s *stubEngine) Feed(_ []float32) (<-chan asr.Partial, error) {
	ch := make(chan asr.Partial)
	close(ch)
	return ch, nil
}
func (s *stubEngine) Finalize(_ asr.AudioSegment) (asr.FinalResult, error) {
	return asr.FinalResult{Text: "ok", EngineID: s.id}, nil
}
func (s *stubEngine) Health(_ context.Context) asr.HealthStatus {
	state := contract.EngineStateReady
	if s.warmErr != nil {
		state = contract.EngineStateFailed
	}
	return asr.HealthStatus{State: state, EngineID: s.id}
}
func (s *stubEngine) Close() error {
	s.closed.Add(1)
	return nil
}

// setup 建一个临时库 + appsettings + router（本地引擎用替身）。
func setup(t *testing.T) (*Service, *appsettings.Service, *asr.Router, *storage.Holder) {
	t.Helper()
	// cryptox 的密钥文件落在数据根目录：测试必须指到临时目录，
	// 否则在无 APPDATA 的 shell 里会失败（见 docs/DEV-ISSUES-S3.md）。
	t.Setenv(paths.EnvDataRoot, t.TempDir())

	path := filepath.Join(t.TempDir(), "test.db")
	db, err := storage.Open(path)
	if err != nil {
		t.Fatalf("打开库失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	holder := storage.NewHolder(db)
	settings := appsettings.New(holder, nil, nil)

	router := asr.NewRouter(asr.RouterOptions{
		Source: contract.ModelSourceLocal,
		Local: func() (asr.Engine, error) {
			return &stubEngine{id: "stub-local"}, nil
		},
		// 故意不给在线工厂：装配时「在线端点尚未配置」才是真实初始状态，
		// 只有 SetOnlineConfig 成功后引擎才可能被构造出来。
		Policy: contract.FallbackPolicy{FallbackToLocal: true},
	})
	t.Cleanup(func() { _ = router.Close() })

	svc, err := New(holder, settings, router, nil, func() bool { return false })
	if err != nil {
		t.Fatalf("构造 enginesvc 失败: %v", err)
	}
	return svc, settings, router, holder
}

func TestGetStatus(t *testing.T) {
	svc, _, _, _ := setup(t)
	st, err := svc.GetStatus(context.Background())
	if err != nil {
		t.Fatalf("GetStatus 失败: %v", err)
	}
	if st.Source != contract.ModelSourceLocal {
		t.Fatalf("Source = %q", st.Source)
	}
}

func TestSetSource_InvalidArgument(t *testing.T) {
	svc, _, _, _ := setup(t)
	_, err := svc.SetSource(context.Background(), "cloud")
	if !apierr.HasCode(err, apierr.CodeInvalidArgument) {
		t.Fatalf("期望 ERR_INVALID_ARGUMENT，实际 %v", err)
	}
}

func TestSetSource_LocalSuccessPersists(t *testing.T) {
	svc, settings, router, _ := setup(t)
	st, err := svc.SetSource(context.Background(), contract.ModelSourceLocal)
	if err != nil {
		t.Fatalf("切本地失败: %v", err)
	}
	if st.State != contract.EngineStateReady {
		t.Fatalf("State = %q, 期望 ready", st.State)
	}
	if router.ID() != "stub-local" {
		t.Fatalf("EngineID = %q", router.ID())
	}
	snap, err := settings.Get(context.Background())
	if err != nil {
		t.Fatalf("读设置失败: %v", err)
	}
	if snap.ModelSource != contract.ModelSourceLocal {
		t.Fatalf("库里的 modelSource = %q", snap.ModelSource)
	}
}

func TestSetSource_OnlineRequiresEndpointAndConsent(t *testing.T) {
	svc, settings, _, _ := setup(t)
	ctx := context.Background()

	// 1) 没配端点 → 参数错误。
	if _, err := svc.SetSource(ctx, contract.ModelSourceOnline); !apierr.HasCode(err, apierr.CodeInvalidArgument) {
		t.Fatalf("期望 ERR_INVALID_ARGUMENT，实际 %v", err)
	}

	// 2) 配了端点但没做隐私确认 → 需要确认。
	const endpoint = "https://127.0.0.1:1"
	if err := settings.SetConsentOnline(ctx, endpoint); err != nil {
		t.Fatalf("记录确认失败: %v", err)
	}
	// 先把 consent 清掉：用一个不同域名的确认来模拟「域名变了」。
	if err := settings.SetConsentOnline(ctx, "https://other.example.com"); err != nil {
		t.Fatalf("记录确认失败: %v", err)
	}
	if err := settings.SetOnlineConfig(ctx, contract.OnlineConfig{
		Endpoint: endpoint,
		Model:    "m",
		APIKey:   "sk-abcdefgh",
	}); !apierr.HasCode(err, apierr.CodeSettingsConsentRequired) {
		t.Fatalf("期望 consent 错误，实际 %v", err)
	}

	// 3) 对当前域名确认后可通过 svc 保存（同时会装上真实的在线引擎工厂）。
	if err := settings.SetConsentOnline(ctx, endpoint); err != nil {
		t.Fatalf("记录确认失败: %v", err)
	}
	if err := svc.SetOnlineConfig(ctx, contract.OnlineConfig{
		Endpoint: endpoint,
		Model:    "m",
		APIKey:   "sk-abcdefgh",
	}); err != nil {
		t.Fatalf("保存在线配置失败: %v", err)
	}

	// 4) 切在线：端点连不上 → 引擎加载（网络）失败，且**不得**把来源落库成 online。
	_, err := svc.SetSource(ctx, contract.ModelSourceOnline)
	if err == nil {
		t.Fatal("端点不可达时应返回错误")
	}
	snap, serr := settings.Get(ctx)
	if serr != nil {
		t.Fatalf("读设置失败: %v", serr)
	}
	if snap.ModelSource == contract.ModelSourceOnline {
		t.Fatal("切换失败时不得把 modelSource 落库为 online")
	}
}

func TestSetSource_RejectedWhileRecording(t *testing.T) {
	t.Setenv(paths.EnvDataRoot, t.TempDir())
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := storage.Open(path)
	if err != nil {
		t.Fatalf("打开库失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	holder := storage.NewHolder(db)
	settings := appsettings.New(holder, nil, nil)
	active := true
	router := asr.NewRouter(asr.RouterOptions{
		Source:        contract.ModelSourceLocal,
		Local:         func() (asr.Engine, error) { return &stubEngine{id: "stub-local"}, nil },
		ActiveSession: func() bool { return active },
	})
	svc, err := New(holder, settings, router, nil, func() bool { return active })
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}

	if _, err := svc.SetSource(context.Background(), contract.ModelSourceLocal); !apierr.HasCode(err, apierr.CodeStateForbidden) {
		t.Fatalf("录制中应拒绝切换，实际 %v", err)
	}
}

func TestReload(t *testing.T) {
	svc, _, router, _ := setup(t)
	if _, err := svc.Reload(context.Background()); err != nil {
		t.Fatalf("Reload 失败: %v", err)
	}
	if router.Status().State != contract.EngineStateReady {
		t.Fatalf("State = %q", router.Status().State)
	}
}

func TestSetFallbackPolicy(t *testing.T) {
	svc, settings, router, _ := setup(t)
	ctx := context.Background()

	if err := svc.SetFallbackPolicy(ctx, contract.FallbackPolicy{MaxRetries: 99}); !apierr.HasCode(err, apierr.CodeInvalidArgument) {
		t.Fatalf("期望 ERR_INVALID_ARGUMENT，实际 %v", err)
	}

	want := contract.FallbackPolicy{FallbackToLocal: false, AutoSwitchBack: true, MaxRetries: 1}
	if err := svc.SetFallbackPolicy(ctx, want); err != nil {
		t.Fatalf("保存策略失败: %v", err)
	}
	if got := router.Policy(); got.FallbackToLocal != want.FallbackToLocal ||
		got.AutoSwitchBack != want.AutoSwitchBack || got.MaxRetries != want.MaxRetries {
		t.Fatalf("router 策略 = %+v", got)
	}
	// 两个布尔列应已落库（maxRetries 暂无列，见 DEV-ISSUES-S3）。
	snap, err := settings.Get(ctx)
	if err != nil {
		t.Fatalf("读设置失败: %v", err)
	}
	if snap.FallbackToLocal != want.FallbackToLocal || snap.AutoSwitchBack != want.AutoSwitchBack {
		t.Fatalf("库里策略 = %+v", snap)
	}
}

func TestGetOnlineConfigMasksAPIKey(t *testing.T) {
	svc, settings, _, _ := setup(t)
	ctx := context.Background()

	const endpoint = "https://api.example.com"
	if err := settings.SetConsentOnline(ctx, endpoint); err != nil {
		t.Fatalf("记录确认失败: %v", err)
	}
	if err := svc.SetOnlineConfig(ctx, contract.OnlineConfig{
		Endpoint: endpoint,
		Model:    "whisper-1",
		Language: "zh",
		APIKey:   "sk-abcdefghijkl",
	}); err != nil {
		t.Fatalf("保存失败: %v", err)
	}

	cfg, err := svc.GetOnlineConfig(ctx)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if cfg.Endpoint != endpoint || cfg.Model != "whisper-1" {
		t.Fatalf("配置回读不正确: %+v", cfg)
	}
	if !strings.Contains(cfg.APIKey, "***") {
		t.Fatalf("APIKey 未掩码: %q", cfg.APIKey)
	}
	if cfg.TimeoutMs != defaultOnlineTimeoutMs {
		t.Fatalf("TimeoutMs = %d", cfg.TimeoutMs)
	}
}

func TestTestOnline_OfflineEnforced(t *testing.T) {
	svc, _, _, _ := setup(t)
	t.Setenv("MANDARYN_OFFLINE", "1")

	res, err := svc.TestOnline(context.Background(), contract.OnlineConfig{Endpoint: "https://api.example.com", Model: "m"})
	if !apierr.HasCode(err, apierr.CodeOfflineEnforced) {
		t.Fatalf("期望 ERR_OFFLINE_ENFORCED，实际 %v", err)
	}
	if res.OK {
		t.Fatal("离线时不应报告成功")
	}
}

func TestNew_RejectsMissingDependencies(t *testing.T) {
	if _, err := New(nil, nil, nil, nil, nil); err == nil {
		t.Fatal("缺依赖时应报错")
	}
}
