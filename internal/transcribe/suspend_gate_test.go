package transcribe

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"sync/atomic"
	"testing"

	"vip.oberon.mandaryn/internal/apierr"
)

// TestRunRejectedWhenInferenceSuspended 断言 P 档（预算耗尽）下转写被拒且**零副作用**：
// 不产生片段、不改转写状态、不动音频、不调用引擎。
//
// 为什么「不置 failed」是要紧的：置 failed 会让库里的会话看起来像「转写出错」，
// 用户会去查日志；而真实原因是「整机忙，等会儿再来」，重试即可成功。
func TestRunRejectedWhenInferenceSuspended(t *testing.T) {
	st, path, _ := twoSentenceStore(t)
	eng := newFakeEngine()

	before := fileDigest(t, path)
	var calls atomic.Int32
	runner := mustRunner(t, st, eng, openerFor(path), Options{
		Allowed: func() error {
			calls.Add(1)
			return apierr.New(apierr.CodePerfSuspended)
		},
	})

	res, err := runner.Run(context.Background(), TaskRequest{SessionID: st.session.ID, Trigger: "manual"})
	if err == nil {
		t.Fatalf("P 档下应拒绝转写，实际成功：%+v", res)
	}
	if !apierr.HasCode(err, apierr.CodePerfSuspended) {
		t.Fatalf("应返回 ERR_PERF_SUSPENDED，实际 %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("闸门应被调用一次，实际 %d 次", calls.Load())
	}

	if got := eng.Calls(); got != 0 {
		t.Fatalf("被拒绝的任务不得调用引擎，实际 %d 次", got)
	}
	session, segs := st.snapshot()
	if len(segs) != 0 {
		t.Fatalf("被拒绝的任务不得产生片段，实际 %d 条", len(segs))
	}
	if session.TranscriptionStatus != StatusNone {
		t.Fatalf("被拒绝的任务不得改转写状态（应保持 none 以便重试），实际 %s", session.TranscriptionStatus)
	}
	if session.TranscribeProgressMs != 0 {
		t.Fatalf("被拒绝的任务不得改进度，实际 %d", session.TranscribeProgressMs)
	}
	if after := fileDigest(t, path); after != before {
		t.Fatal("被拒绝的任务不得改写音频文件")
	}
}

// TestRunAllowedNilKeepsLegacyBehavior 断言不注入闸门时行为不变（既有路径必须原样通过）。
func TestRunAllowedNilKeepsLegacyBehavior(t *testing.T) {
	st, path, _ := twoSentenceStore(t)
	eng := newFakeEngine()
	runner := mustRunner(t, st, eng, openerFor(path), Options{})

	res, err := runner.Run(context.Background(), TaskRequest{SessionID: st.session.ID, Trigger: "manual"})
	if err != nil {
		t.Fatalf("未注入闸门时应正常执行，实际 %v", err)
	}
	if res.Outcome != OutcomeDone {
		t.Fatalf("outcome = %q，期望 done", res.Outcome)
	}
	if eng.Calls() != 2 {
		t.Fatalf("引擎应被调用 2 次，实际 %d", eng.Calls())
	}
}

// TestRunAllowedReadPerRun 断言闸门每次 Run 都重新读（档位随时可能变化），
// 且档位恢复后同一个 Runner 能继续成功——不注入就退回旧的「一次性」语义是不行的。
func TestRunAllowedReadPerRun(t *testing.T) {
	st, path, _ := twoSentenceStore(t)
	eng := newFakeEngine()

	var suspended atomic.Bool
	suspended.Store(true)
	var reads atomic.Int32
	runner := mustRunner(t, st, eng, openerFor(path), Options{
		Allowed: func() error {
			reads.Add(1)
			if suspended.Load() {
				return apierr.New(apierr.CodePerfSuspended)
			}
			return nil
		},
	})

	ctx := context.Background()
	if _, err := runner.Run(ctx, TaskRequest{SessionID: st.session.ID, Trigger: "manual"}); !apierr.HasCode(err, apierr.CodePerfSuspended) {
		t.Fatalf("P 档应拒绝，实际 %v", err)
	}

	// 档位恢复（P → L3）：同一个 Runner 再跑一次必须成功。
	suspended.Store(false)
	res, err := runner.Run(ctx, TaskRequest{SessionID: st.session.ID, Trigger: "manual"})
	if err != nil {
		t.Fatalf("恢复后应能转写，实际 %v", err)
	}
	if res.Outcome != OutcomeDone {
		t.Fatalf("outcome = %q，期望 done", res.Outcome)
	}
	if reads.Load() != 2 {
		t.Fatalf("闸门应每次 Run 读一次，共 2 次，实际 %d 次", reads.Load())
	}
	_, segs := st.snapshot()
	if len(segs) != 2 {
		t.Fatalf("恢复后应正常落 2 句，实际 %d 句", len(segs))
	}
}

// TestSetAllowedAfterConstruction 断言后置接线（装配层迟到拿到 perf 服务时）同样生效。
func TestSetAllowedAfterConstruction(t *testing.T) {
	st, path, _ := twoSentenceStore(t)
	runner := mustRunner(t, st, newFakeEngine(), openerFor(path), Options{})

	runner.SetAllowed(func() error { return apierr.New(apierr.CodePerfSuspended) })
	_, err := runner.Run(context.Background(), TaskRequest{SessionID: st.session.ID, Trigger: "manual"})
	if !apierr.HasCode(err, apierr.CodePerfSuspended) {
		t.Fatalf("后置接线的闸门也应生效，实际 %v", err)
	}
}

// fileDigest 返回文件内容的摘要（用于断言「音频没被改写」）。
func fileDigest(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取音频失败：%v", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
