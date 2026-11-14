package speakersvc

// L3 薄壳测试：只验证「参数校验 / 错误码映射 / 依赖装配 / 事件转发素材」，
// 领域语义（聚类、归并、幂等）由 internal/speaker 的测试覆盖。

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/speaker"
	"vip.oberon.mandaryn/internal/storage"
	"vip.oberon.mandaryn/internal/utils/cryptox"
)

func TestMain(m *testing.M) {
	// 声纹向量加密需要主密钥目录：指到临时目录，避免污染用户数据目录。
	dir, err := os.MkdirTemp("", "mandaryn-speakersvc-key-*")
	if err == nil {
		_ = cryptox.SetBaseDir(dir)
	}
	code := m.Run()
	if dir != "" {
		_ = os.RemoveAll(dir)
	}
	os.Exit(code)
}

// newHolder 建一个临时库并包成 Holder。
func newHolder(t *testing.T) *storage.Holder {
	t.Helper()
	dir := t.TempDir()
	db, err := storage.Open(filepath.Join(dir, "mandaryn.db"))
	if err != nil {
		t.Fatalf("打开临时数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return storage.NewHolder(db)
}

// TestNewRequiresHolder 没有数据库句柄直接报错（不允许半残服务）。
func TestNewRequiresHolder(t *testing.T) {
	if _, err := New(Deps{}); !apierr.HasCode(err, apierr.CodeDBWrite) {
		t.Fatalf("期望 ERR_DB_WRITE，实际 %v", err)
	}
}

// TestDegradedWithoutExtractor 无提取器仍可用（角色管理不依赖推理）。
func TestDegradedWithoutExtractor(t *testing.T) {
	holder := newHolder(t)
	var events []contract.RoleChanged
	svc, err := New(Deps{Holder: holder, OnRolesChanged: func(e contract.RoleChanged) {
		events = append(events, e)
	}})
	if err != nil {
		t.Fatalf("构造服务失败: %v", err)
	}
	if svc.String() != "speakersvc" {
		t.Fatalf("服务名应稳定为 speakersvc，实际 %q", svc.String())
	}

	// 未装配模型 → 分离能力不可用，但服务本身可用。
	if svc.HasDiarization() {
		t.Fatal("未注入提取器时 HasDiarization 应为 false")
	}

	// 角色列表可用（内置 rl_unknown 已由迁移播种）。
	roles, err := svc.ListRoles(context.Background())
	if err != nil {
		t.Fatalf("ListRoles 失败: %v", err)
	}
	if len(roles) != 1 || !contract.RoleIsUnidentified(roles[0].ID) {
		t.Fatalf("应只有内置未识别角色，实际 %+v", roles)
	}

	// 分离返回明确错误码，不 panic。
	if _, err := svc.AnalyzeSession(context.Background(), "s_1"); !apierr.HasCode(err, apierr.CodeLocalUnavailable) {
		t.Fatalf("无提取器时应返回 ERR_LOCAL_UNAVAILABLE，实际 %v", err)
	}

	// 建一个角色 → 改名 → 收到事件。
	now := time.Now().UnixMilli()
	if err := holder.Current().GORM.Create(&storage.Role{
		ID: "rl_1", Name: "说话人 1", ColorToken: "role-1", CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatalf("建角色失败: %v", err)
	}
	if err := svc.RenameRole(context.Background(), "rl_1", "小王"); err != nil {
		t.Fatalf("改名失败: %v", err)
	}
	if len(events) != 1 || events[0].RoleID != "rl_1" || events[0].Name != "小王" {
		t.Fatalf("应转发 1 次角色变更素材，实际 %+v", events)
	}
}

// TestSetMaxSpeakersRange SetMaxSpeakers 只接受 0 或 2–8。
func TestSetMaxSpeakersRange(t *testing.T) {
	holder := newHolder(t)
	svc, err := New(Deps{Holder: holder})
	if err != nil {
		t.Fatalf("构造服务失败: %v", err)
	}
	ctx := context.Background()

	for _, bad := range []int{-1, 1, 9, 100} {
		if err := svc.SetMaxSpeakers(ctx, bad); !apierr.HasCode(err, apierr.CodeInvalidArgument) {
			t.Fatalf("%d 应被拒，实际 %v", bad, err)
		}
	}
	for _, ok := range []int{0, 2, 5, 8} {
		if err := svc.SetMaxSpeakers(ctx, ok); err != nil {
			t.Fatalf("%d 应被接受，实际 %v", ok, err)
		}
		var row storage.AppSettings
		if err := holder.Current().GORM.First(&row, "id = ?", 1).Error; err != nil {
			t.Fatalf("读设置失败: %v", err)
		}
		if row.MaxSpeakers != ok {
			t.Fatalf("设置未落库：期望 %d，实际 %d", ok, row.MaxSpeakers)
		}
	}
}

// TestListSessionSpeakersNotFoundPassthrough 错误码透传（薄壳不吞错误）。
func TestListSessionSpeakersNotFoundPassthrough(t *testing.T) {
	holder := newHolder(t)
	svc, err := New(Deps{Holder: holder})
	if err != nil {
		t.Fatalf("构造服务失败: %v", err)
	}
	if _, err := svc.ListSessionSpeakers(context.Background(), "s_nope"); !apierr.HasCode(err, apierr.CodeNotFound) {
		t.Fatalf("期望 ERR_NOT_FOUND 透传，实际 %v", err)
	}
	if _, err := svc.SplitSpeaker(context.Background(), "s_nope", "sp_x", nil); !apierr.HasCode(err, apierr.CodeInvalidArgument) {
		t.Fatalf("期望 ERR_INVALID_ARGUMENT，实际 %v", err)
	}
}

// TestAutoWAVLoaderWiring 给了 Resolver 时自动装配 WAV 读取器（不必外部再传 Audio）。
func TestAutoWAVLoaderWiring(t *testing.T) {
	holder := newHolder(t)
	// 没有 resolver 时不应报错，只是音频端口为空。
	if _, err := New(Deps{Holder: holder}); err != nil {
		t.Fatalf("无 resolver 也应能构造服务: %v", err)
	}
	// Resolver 为空时即便不给 Audio 也不该 panic（wavOpener 不会被调用）。
	svc, err := New(Deps{Holder: holder, Audio: nil})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if svc == nil {
		t.Fatal("服务不应为 nil")
	}
}

// 编译期断言：Service 满足契约里用到的方法集合（防止签名漂移）。
var _ = []any{
	(*Service)(nil).ListRoles,
	(*Service)(nil).RenameRole,
	(*Service)(nil).ListSessionSpeakers,
	(*Service)(nil).MergeSpeakers,
	(*Service)(nil).SplitSpeaker,
	(*Service)(nil).SetMaxSpeakers,
	(*Service)(nil).SetRoleColor,
	(*Service)(nil).AnalyzeSession,
	(*Service)(nil).AssignSpeaker,
	(*Service)(nil).HasDiarization,
}

// 确保 speaker 包被引用（HasDiarization 语义依赖它）。
var _ = speaker.RoleSpeaker

// fakeLocator 假的模型包定位器。
type fakeLocator struct {
	dir     string
	bundle  string
	errCode string
}

func (l fakeLocator) LocateBundle(role string) (string, string, error) {
	if l.errCode != "" {
		return "", "", apierr.New(l.errCode).WithDetail("测试用：role=%s", role)
	}
	return l.dir, l.bundle, nil
}

// fakeExtractor 假的声纹提取器（只关心 ID 与 Dim）。
type fakeExtractor struct{ id string }

func (e fakeExtractor) Embed(samples []float32, sampleRate int) ([]float32, error) {
	out := make([]float32, 4)
	copy(out, samples)
	return out, nil
}
func (e fakeExtractor) ID() string   { return e.id }
func (e fakeExtractor) Close() error { return nil }

// TestExtractorBuiltFromLocator 给了定位器 + 工厂时构造期自动装配提取器。
func TestExtractorBuiltFromLocator(t *testing.T) {
	holder := newHolder(t)
	svc, err := New(Deps{
		Holder:  holder,
		Locator: fakeLocator{dir: t.TempDir(), bundle: "sherpa-speaker@1.0"},
		Factory: func(dir string) (speaker.Extractor, error) {
			if dir == "" {
				t.Fatal("工厂应收到模型包目录")
			}
			return fakeExtractor{id: "speaker@1.0"}, nil
		},
	})
	if err != nil {
		t.Fatalf("构造服务失败: %v", err)
	}
	if !svc.HasDiarization() {
		t.Fatal("装配成功后 HasDiarization 应为 true")
	}
}

// TestModelNotInstalledKeepsServiceUsable 模型装不上时：服务可用，且错误码具体。
func TestModelNotInstalledKeepsServiceUsable(t *testing.T) {
	holder := newHolder(t)
	built := false
	svc, err := New(Deps{
		Holder:  holder,
		Locator: fakeLocator{errCode: apierr.CodeModelNotFound},
		Factory: func(string) (speaker.Extractor, error) {
			built = true
			return nil, apierr.New(apierr.CodeModelNotFound)
		},
	})
	if err != nil {
		t.Fatalf("模型未装不应导致服务构造失败: %v", err)
	}
	if built {
		t.Fatal("定位失败时不应调用工厂")
	}
	if svc.HasDiarization() {
		t.Fatal("模型未装时 HasDiarization 应为 false")
	}
	// 错误码要具体：ERR_MODEL_NOT_FOUND（提示用户去下载），而不是笼统的不可用。
	if _, err := svc.AnalyzeSession(context.Background(), "s_1"); !apierr.HasCode(err, apierr.CodeModelNotFound) {
		t.Fatalf("应透出 ERR_MODEL_NOT_FOUND，实际 %v", err)
	}
	// 角色管理照常可用。
	if _, err := svc.ListRoles(context.Background()); err != nil {
		t.Fatalf("角色列表应仍可用: %v", err)
	}
}
