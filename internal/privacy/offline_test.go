package privacy

// 离线闸门测试：重点是「离线时零真实请求」——用 httptest 计数断言
// handler 从未被调用，而不是只看返回了错误码。

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/storage"
	"vip.oberon.mandaryn/internal/utils/httpx"
)

// newHolder 建一个临时库并包成 Holder（AutoMigrate 已含 outbound_log）。
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

// boolPtr 便于构造注入函数。
func boolPtr(v bool) func() bool { return func() bool { return v } }

// TestGateEnvOffline 环境变量开启即离线（Env 注入为 nil 时走 MANDARYN_OFFLINE）。
func TestGateEnvOffline(t *testing.T) {
	t.Setenv(EnvOffline, "1")
	g := NewGate(GateOptions{})
	if !g.IsOffline() {
		t.Fatal("MANDARYN_OFFLINE=1 时闸门必须处于离线")
	}
	st := g.State()
	if !st.Env || st.User {
		t.Fatalf("来源判定错误: %+v", st)
	}
	if st.Source != SourceEnv {
		t.Fatalf("Source 期望 env，实际 %q", st.Source)
	}
}

// TestGateUserOffline 用户开关单独也能开启离线。
func TestGateUserOffline(t *testing.T) {
	g := NewGate(GateOptions{Env: boolPtr(false), User: boolPtr(true)})
	st := g.State()
	if !st.Offline || st.Source != SourceUser {
		t.Fatalf("期望 user 来源离线，实际 %+v", st)
	}
}

// TestGateEnvOrUser 两层取或，且来源可区分。
func TestGateEnvOrUser(t *testing.T) {
	cases := []struct {
		env, user bool
		want      OfflineSource
	}{
		{false, false, SourceNone},
		{true, false, SourceEnv},
		{false, true, SourceUser},
		{true, true, SourceEnvAndUser},
	}
	for _, c := range cases {
		g := NewGate(GateOptions{Env: boolPtr(c.env), User: boolPtr(c.user)})
		st := g.State()
		if st.Source != c.want {
			t.Fatalf("env=%v user=%v 期望 %q，实际 %q", c.env, c.user, c.want, st.Source)
		}
		if st.Offline != (c.env || c.user) {
			t.Fatalf("env=%v user=%v 取或结果错误", c.env, c.user)
		}
	}
}

// TestGateCheckRejectsWithErrorCode 离线时返回 ERR_OFFLINE_ENFORCED，
// 且 detail 里只有 host，不带 query 里的凭据。
func TestGateCheckRejectsWithErrorCode(t *testing.T) {
	g := NewGate(GateOptions{Env: boolPtr(true)})
	err := g.Check("online-asr", "https://api.example.com/v1/audio/transcriptions?token=sk-super-secret-value-123456")
	if err == nil {
		t.Fatal("离线时 Check 必须报错")
	}
	if !apierr.HasCode(err, apierr.CodeOfflineEnforced) {
		t.Fatalf("期望 ERR_OFFLINE_ENFORCED，实际 %v", err)
	}
	var e *apierr.Err
	if !errors.As(err, &e) {
		t.Fatalf("期望 *apierr.Err，实际 %T", err)
	}
	if e.Action != "disable-offline" {
		t.Fatalf("action 期望 disable-offline，实际 %q", e.Action)
	}
	if strings.Contains(e.Detail, "sk-super-secret-value-123456") || strings.Contains(e.Detail, "token=") {
		t.Fatalf("detail 泄漏了 query 凭据: %q", e.Detail)
	}
	if !strings.Contains(e.Detail, "api.example.com") {
		t.Fatalf("detail 应保留目标 host 便于排查: %q", e.Detail)
	}
}

// TestGateAllowsOnline 在线时 Check 放行（返回 nil）。
func TestGateAllowsOnline(t *testing.T) {
	g := NewGate(GateOptions{Env: boolPtr(false), User: boolPtr(false)})
	if err := g.Check("online-asr", "https://api.example.com/v1/audio/transcriptions"); err != nil {
		t.Fatalf("在线时不应拦截: %v", err)
	}
}

// TestPipeOfflineMakesZeroRequests 离线时 handler 计数必须为 0：
// 这是「不允许先发请求再报错」的断言。
func TestPipeOfflineMakesZeroRequests(t *testing.T) {
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&hits, 1)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	rec := NewRecorder(newHolder(t))
	pipe := NewPipe(NewGate(GateOptions{Env: boolPtr(true)}), rec)

	err := pipe.DoRecorded(context.Background(), Call{
		Kind: "online-asr", Purpose: "finalize", Target: srv.URL + "/v1/audio/transcriptions", Audio: true,
	}, func(ctx context.Context) (Outcome, error) {
		req, cancel, rerr := httpx.NewRequest(ctx, http.MethodGet, srv.URL, nil)
		if rerr != nil {
			return Outcome{}, rerr
		}
		defer cancel()
		resp, derr := httpx.Do(req)
		if resp != nil {
			_ = resp.Body.Close()
		}
		return Outcome{Status: http.StatusOK}, derr
	})

	if !apierr.HasCode(err, apierr.CodeOfflineEnforced) {
		t.Fatalf("期望 ERR_OFFLINE_ENFORCED，实际 %v", err)
	}
	if got := atomic.LoadInt64(&hits); got != 0 {
		t.Fatalf("离线模式下发生了 %d 次真实请求（必须为 0）", got)
	}

	// 被拦下的尝试也要留痕（证明「拦住了」，而不是「没发生过」）。
	rows, total, lerr := rec.List(ListOptions{Limit: 10})
	if lerr != nil {
		t.Fatalf("读取出站记录失败: %v", lerr)
	}
	if total != 1 || len(rows) != 1 {
		t.Fatalf("期望 1 条 blocked 记录，实际 total=%d rows=%d", total, len(rows))
	}
	if !rows[0].Blocked || rows[0].Result != ResultBlocked {
		t.Fatalf("期望 blocked 记录，实际 %+v", rows[0])
	}
	if rows[0].Kind != "online-asr" {
		t.Fatalf("kind 未正确记录: %+v", rows[0])
	}
	if !rows[0].Audio {
		t.Fatal("audio 标记必须保留（出站可见要能区分是否含音频）")
	}
}
