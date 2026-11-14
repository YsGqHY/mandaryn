package appsettings

import (
	"context"
	"errors"
	"testing"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
)

// TestDebugModeDefaultOff：调试模式默认关闭（log_level 默认 info）。
func TestDebugModeDefaultOff(t *testing.T) {
	snap, err := newTestService(t).Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snap.Privacy.LogLevel != "info" {
		t.Fatalf("log_level 默认应为 info，实际 %q", snap.Privacy.LogLevel)
	}
}

// TestSetDebugModePersistsAndApplies：落库为 debug/info，并把用户层的值交给运行时落实。
func TestSetDebugModePersistsAndApplies(t *testing.T) {
	var applied []bool
	svc := newTestService(t, WithDebugMode(func(on bool) (contract.DebugModeState, error) {
		applied = append(applied, on)
		return contract.DebugModeState{Enabled: on, UserEnabled: on, Source: contract.DebugModeSourceUser}, nil
	}))
	ctx := context.Background()

	st, err := svc.SetDebugMode(ctx, true)
	if err != nil || !st.Enabled {
		t.Fatalf("开启失败：%+v %v", st, err)
	}
	if snap, _ := svc.Get(ctx); snap.Privacy.LogLevel != "debug" {
		t.Fatalf("开启后 log_level 应为 debug，实际 %q", snap.Privacy.LogLevel)
	}
	if _, err := svc.SetDebugMode(ctx, false); err != nil {
		t.Fatal(err)
	}
	if snap, _ := svc.Get(ctx); snap.Privacy.LogLevel != "info" {
		t.Fatalf("关闭后 log_level 应为 info，实际 %q", snap.Privacy.LogLevel)
	}
	// SetPrivacy 写 log_level 时同样同步运行时。
	if err := svc.SetPrivacy(ctx, contract.PrivacySettings{LogLevel: "debug"}); err != nil {
		t.Fatal(err)
	}
	if len(applied) != 3 || !applied[0] || applied[1] || !applied[2] {
		t.Fatalf("落实顺序应为 [true false true]，实际 %v", applied)
	}
}

// TestSetDebugModeEnvManagedKeepsUserValue：环境变量接管时仍保存用户层的值，返回以环境变量为准。
func TestSetDebugModeEnvManagedKeepsUserValue(t *testing.T) {
	svc := newTestService(t, WithDebugMode(func(on bool) (contract.DebugModeState, error) {
		return contract.DebugModeState{Enabled: true, UserEnabled: on, Source: contract.DebugModeSourceEnv, EnvVar: "MANDARYN_LOG_LEVEL"}, nil
	}))
	ctx := context.Background()
	st, err := svc.SetDebugMode(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Enabled || st.UserEnabled || st.Source != contract.DebugModeSourceEnv {
		t.Fatalf("接管状态不对：%+v", st)
	}
	if snap, _ := svc.Get(ctx); snap.Privacy.LogLevel != "info" {
		t.Fatalf("用户层的值仍应落库，实际 %q", snap.Privacy.LogLevel)
	}
}

// TestSetDebugModeApplyFailure：日志文件打不开时报 ERR_PATH_UNWRITABLE，生效值为关。
func TestSetDebugModeApplyFailure(t *testing.T) {
	svc := newTestService(t, WithDebugMode(func(on bool) (contract.DebugModeState, error) {
		return contract.DebugModeState{UserEnabled: on, Source: contract.DebugModeSourceUser}, errors.New("disk full")
	}))
	st, err := svc.SetDebugMode(context.Background(), true)
	if !apierr.HasCode(err, apierr.CodePathUnwritable) || st.Enabled {
		t.Fatalf("应报 ERR_PATH_UNWRITABLE 且未开启：%+v %v", st, err)
	}
}
