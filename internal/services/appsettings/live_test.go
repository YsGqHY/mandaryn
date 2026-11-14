package appsettings

import (
	"context"
	"path/filepath"
	"testing"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/storage"
)

// newTestService 在临时库上构造服务（app_settings 单行由 storage.Open 自动补齐）。
func newTestService(t *testing.T, opts ...Option) *Service {
	t.Helper()
	dir := t.TempDir()
	db, err := storage.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("打开测试库失败：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return New(storage.NewHolder(db), nil, nil, opts...)
}

// TestLiveTranscribeDefaultOff 断言默认是**关**（FR-03-10 的第一条铁律）。
func TestLiveTranscribeDefaultOff(t *testing.T) {
	svc := newTestService(t)
	snap, err := svc.Get(context.Background())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if snap.LiveTranscribe {
		t.Fatal("边录边转默认必须关闭")
	}
}

// TestSetLiveTranscribePersists 断言开关能落库并在快照里读回。
func TestSetLiveTranscribePersists(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	if err := svc.SetLiveTranscribe(ctx, true); err != nil {
		t.Fatalf("SetLiveTranscribe(true): %v", err)
	}
	snap, err := svc.Get(ctx)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !snap.LiveTranscribe {
		t.Fatal("开启后快照应读回 true")
	}

	if err := svc.SetLiveTranscribe(ctx, false); err != nil {
		t.Fatalf("SetLiveTranscribe(false): %v", err)
	}
	snap, _ = svc.Get(ctx)
	if snap.LiveTranscribe {
		t.Fatal("关闭后快照应读回 false")
	}
}

// TestSetLiveTranscribeRejectedWhileRecording 断言录制中切换被拒绝。
//
// 语义理由：开关只在**下一个会话**生效，录制期间接受只会让用户以为
// 「开了但没生效」。与模型来源切换同规则。
func TestSetLiveTranscribeRejectedWhileRecording(t *testing.T) {
	recording := true
	svc := newTestService(t, WithActiveSession(func() bool { return recording }))
	ctx := context.Background()

	err := svc.SetLiveTranscribe(ctx, true)
	if !apierr.HasCode(err, apierr.CodeStateForbidden) {
		t.Fatalf("录制中开启应返回 ERR_STATE_FORBIDDEN，实际 %v", err)
	}
	snap, _ := svc.Get(ctx)
	if snap.LiveTranscribe {
		t.Fatal("被拒绝的写入不得改库")
	}

	// 停止录制后可以切。
	recording = false
	if err := svc.SetLiveTranscribe(ctx, true); err != nil {
		t.Fatalf("空闲时应允许：%v", err)
	}
	if snap, _ := svc.Get(ctx); !snap.LiveTranscribe {
		t.Fatal("空闲时的写入应生效")
	}

	// 关闭方向**不**受限：录制中关掉它也是安全的（本会话已按开启启动，
	// 关掉只影响下一个会话；这里按「两个方向都拒」实现更简单也更可预期，
	// 因此再确认一次录制中的关闭同样被拒）。
	recording = true
	if err := svc.SetLiveTranscribe(ctx, false); !apierr.HasCode(err, apierr.CodeStateForbidden) {
		t.Fatalf("录制中关闭也应返回 ERR_STATE_FORBIDDEN，实际 %v", err)
	}
}
