package hotkeys

import (
	"context"
	"testing"
)

// TestLeanDefaultState 断言精简模式默认关闭且清单恒定返回。
func TestLeanDefaultState(t *testing.T) {
	svc := New(nil, newFakeRegistrar(), NewMemoryStore(), Emit{})
	st, err := svc.GetLeanState(context.Background())
	if err != nil {
		t.Fatalf("GetLeanState: %v", err)
	}
	if st.Enabled {
		t.Error("默认应为关闭（P1：默认值即最佳实践，首次安装要看得见完整能力）")
	}
	if len(st.VisibleRegions) == 0 || len(st.HiddenRegions) == 0 || len(st.HiddenControls) == 0 {
		t.Fatalf("取舍清单不该为空: %+v", st)
	}
	// 开关关闭时清单仍要返回同一份，前端才能预先知道要隐什么。
	if !contains(copyJoin(st.HiddenRegions), RegionSettingsAdvanced) {
		t.Errorf("隐去区域应包含 %s", RegionSettingsAdvanced)
	}
	if !contains(copyJoin(st.VisibleRegions), RegionStatus) {
		t.Errorf("保留区域应包含 %s（L1 状态常驻）", RegionStatus)
	}
}

// TestLeanTogglePersists 断言开关往返持久化（模拟重启）。
func TestLeanTogglePersists(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	svc := New(nil, newFakeRegistrar(), store, Emit{})

	st, err := svc.SetLeanMode(ctx, true)
	if err != nil {
		t.Fatalf("SetLeanMode: %v", err)
	}
	if !st.Enabled {
		t.Error("返回值应反映已开启")
	}
	raw, err := store.Get(ctx, KeyLeanMode)
	if err != nil || raw == "" {
		t.Fatalf("精简模式未落库: raw=%q err=%v", raw, err)
	}

	// 幂等：重复设置同一值不报错
	if _, err := svc.SetLeanMode(ctx, true); err != nil {
		t.Fatalf("重复 SetLeanMode: %v", err)
	}

	// 模拟重启
	svc2 := New(nil, newFakeRegistrar(), store, Emit{})
	st2, err := svc2.GetLeanState(ctx)
	if err != nil {
		t.Fatalf("GetLeanState: %v", err)
	}
	if !st2.Enabled {
		t.Error("重启后精简模式应保持开启")
	}

	// 关闭后重启仍是关闭
	if _, err := svc2.SetLeanMode(ctx, false); err != nil {
		t.Fatalf("SetLeanMode(false): %v", err)
	}
	svc3 := New(nil, newFakeRegistrar(), store, Emit{})
	st3, _ := svc3.GetLeanState(ctx)
	if st3.Enabled {
		t.Error("关闭后重启应保持关闭")
	}
}

// TestLeanSnapshotIsolated 断言调用方改返回切片不影响后续调用（别名泄漏防御）。
func TestLeanSnapshotIsolated(t *testing.T) {
	svc := New(nil, newFakeRegistrar(), NewMemoryStore(), Emit{})
	ctx := context.Background()

	st, _ := svc.GetLeanState(ctx)
	st.HiddenRegions[0] = "tampered"

	again, _ := svc.GetLeanState(ctx)
	if again.HiddenRegions[0] == "tampered" {
		t.Error("返回的清单与包级常量共享底层数组，调用方修改污染了其它调用")
	}
}

// TestLeanUnavailableWithoutStore 断言没有持久化时开关仍可用（只是不落库）。
func TestLeanUnavailableWithoutStore(t *testing.T) {
	ctx := context.Background()
	svc := New(nil, nil, nil, Emit{})
	st, err := svc.SetLeanMode(ctx, true)
	if err != nil {
		t.Fatalf("无 store 时 SetLeanMode 应成功: %v", err)
	}
	if !st.Enabled {
		t.Error("内存态应已开启")
	}
}

// copyJoin 把切片拼成一个便于 contains 判断的字符串。
func copyJoin(in []string) string {
	out := ""
	for i, s := range in {
		if i > 0 {
			out += "|"
		}
		out += s
	}
	return out
}
