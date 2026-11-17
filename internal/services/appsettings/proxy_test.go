package appsettings

import (
	"context"
	"os"
	"strings"
	"testing"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
)

// TestTestProxyWithoutTester 未接入模型域时必须明确报错，而不是假装测通。
func TestTestProxyWithoutTester(t *testing.T) {
	svc := newTestService(t)
	_, err := svc.TestProxy(context.Background(), contract.ProxyConfig{Type: contract.ProxyNone})
	if !apierr.HasCode(err, apierr.CodeInvalidArgument) {
		t.Fatalf("未注入 proxyTester 时应返回 ERR_INVALID_ARGUMENT，实际 %v", err)
	}
}

// TestTestProxyDelegates 注入后应把配置原样转交（L3 只做转发，不自己发请求）。
func TestTestProxyDelegates(t *testing.T) {
	var got contract.ProxyConfig
	svc := newTestService(t, WithProxyTester(
		func(_ context.Context, p contract.ProxyConfig) (contract.TestResult, error) {
			got = p
			return contract.TestResult{OK: true, LatencyMs: 12}, nil
		}))

	res, err := svc.TestProxy(context.Background(), contract.ProxyConfig{
		Type: contract.ProxyHTTP, Host: "127.0.0.1", Port: 7890, Username: "u",
	})
	if err != nil {
		t.Fatalf("TestProxy: %v", err)
	}
	if !res.OK || res.LatencyMs != 12 {
		t.Fatalf("结果未原样返回：%+v", res)
	}
	if got.Type != contract.ProxyHTTP || got.Host != "127.0.0.1" || got.Port != 7890 {
		t.Fatalf("配置未原样转交：%+v", got)
	}
}

// TestTestProxyUsesStoredPasswordWhenMasked 是本用例的重点：
//
// 界面回显的是**掩码**（如 `ab***yz`）。若把掩码字面量当密码去认证，必然 407，
// 用户会以为「密码明明对的却测不通」。此处断言 L3 会把掩码替换成库里已存的明文。
func TestTestProxyUsesStoredPasswordWhenMasked(t *testing.T) {
	ctx := context.Background()
	var got contract.ProxyConfig
	svc := newTestService(t, WithProxyTester(
		func(_ context.Context, p contract.ProxyConfig) (contract.TestResult, error) {
			got = p
			return contract.TestResult{OK: true}, nil
		}))

	// 先保存一份真实密码（SetProxy 会加密落库）。
	if err := svc.SetProxy(ctx, contract.ProxyConfig{
		Type: contract.ProxyHTTP, Host: "127.0.0.1", Port: 7890,
		Username: "alice", Password: "real-secret",
	}); err != nil {
		t.Fatalf("SetProxy: %v", err)
	}

	// 模拟前端把快照里的掩码原样回传。
	masked := (svc.mustSnapshot(ctx)).Proxy.Password
	if !isMasked(masked) {
		t.Fatalf("快照里的密码应是掩码，实际 %q", masked)
	}
	if _, err := svc.TestProxy(ctx, contract.ProxyConfig{
		Type: contract.ProxyHTTP, Host: "127.0.0.1", Port: 7890,
		Username: "alice", Password: masked,
	}); err != nil {
		t.Fatalf("TestProxy: %v", err)
	}
	if got.Password != "real-secret" {
		t.Fatalf("掩码应被替换成库中明文，实际 %q", got.Password)
	}
}

// TestTestProxyExplicitPasswordWins 用户真的输入了新密码时，以输入为准。
func TestTestProxyExplicitPasswordWins(t *testing.T) {
	ctx := context.Background()
	var got contract.ProxyConfig
	svc := newTestService(t, WithProxyTester(
		func(_ context.Context, p contract.ProxyConfig) (contract.TestResult, error) {
			got = p
			return contract.TestResult{OK: true}, nil
		}))

	if err := svc.SetProxy(ctx, contract.ProxyConfig{
		Type: contract.ProxyHTTP, Host: "h", Port: 1, Username: "alice", Password: "old",
	}); err != nil {
		t.Fatalf("SetProxy: %v", err)
	}
	if _, err := svc.TestProxy(ctx, contract.ProxyConfig{
		Type: contract.ProxyHTTP, Host: "h", Port: 1, Username: "alice", Password: "brand-new",
	}); err != nil {
		t.Fatalf("TestProxy: %v", err)
	}
	if got.Password != "brand-new" {
		t.Fatalf("用户显式输入的密码应优先，实际 %q", got.Password)
	}
}

// TestSnapshotProxyEnvVar 断言环境变量接管状态只在真的设了值时暴露。
//
// 前端据此渲染「已由 MANDARYN_HTTP_PROXY 接管、本页不生效」的提示；
// 若空串也报接管，用户会被误导去排查一个并不存在的覆盖。
func TestSnapshotProxyEnvVar(t *testing.T) {
	ctx := context.Background()

	t.Run("未设置时为空", func(t *testing.T) {
		// 显式清空，避免宿主环境里已有该变量造成假阳性。
		t.Setenv("MANDARYN_HTTP_PROXY", "")
		snap := newTestService(t).mustSnapshot(ctx)
		if snap.ProxyEnvVar != "" {
			t.Fatalf("未设置环境变量时不应报接管，实际 %q", snap.ProxyEnvVar)
		}
	})

	t.Run("设置后暴露变量名", func(t *testing.T) {
		t.Setenv("MANDARYN_HTTP_PROXY", "http://127.0.0.1:7890")
		snap := newTestService(t).mustSnapshot(ctx)
		if snap.ProxyEnvVar != "MANDARYN_HTTP_PROXY" {
			t.Fatalf("应暴露接管变量名，实际 %q", snap.ProxyEnvVar)
		}
	})

	t.Run("空白值不算接管", func(t *testing.T) {
		// 只有空白字符时等价于未配置（与 models.globalProxyFromEnv 同口径）。
		t.Setenv("MANDARYN_HTTP_PROXY", "   ")
		if got := strings.TrimSpace(os.Getenv("MANDARYN_HTTP_PROXY")); got != "" {
			t.Fatalf("测试前提不成立：%q", got)
		}
		snap := newTestService(t).mustSnapshot(ctx)
		if snap.ProxyEnvVar != "" {
			t.Fatalf("空白值不应算接管，实际 %q", snap.ProxyEnvVar)
		}
	})
}

// mustSnapshot 是测试用的「取快照并在失败时立即终止」。
func (s *Service) mustSnapshot(ctx context.Context) Snapshot {
	snap, err := s.Get(ctx)
	if err != nil {
		panic(err)
	}
	return snap
}
