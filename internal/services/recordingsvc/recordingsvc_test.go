package recordingsvc

import (
	"context"
	"io"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/capture"
	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/recording"
	"vip.oberon.mandaryn/internal/storage"
)

// fakeChannel 是合成捕获通道（与 L2 测试同构，但这里只关心薄壳的转发行为）。
type fakeChannel struct {
	format capture.AudioFormat
	fill   func(dst []float32)
	delay  time.Duration
	closed chan struct{}
	once   sync.Once
	errCh  chan error
}

func newFakeChannel(format capture.AudioFormat, fill func(dst []float32)) *fakeChannel {
	return &fakeChannel{format: format, fill: fill, closed: make(chan struct{}), errCh: make(chan error, 1)}
}

func (c *fakeChannel) ReadPCM(dst []float32) (int, error) {
	select {
	case <-c.closed:
		// 与 capture.Channel 契约一致：关闭后返回 io.EOF（不是自定义错误），
		// 否则上层会把「主动停止」误判成「设备失效」。
		return 0, io.EOF
	default:
	}
	if c.delay > 0 {
		select {
		case <-time.After(c.delay):
		case <-c.closed:
			return 0, io.EOF
		}
	}
	ch := c.format.Channels
	if ch <= 0 {
		ch = 1
	}
	n := len(dst) - len(dst)%ch
	c.fill(dst[:n])
	return n, nil
}

func (c *fakeChannel) Format() capture.AudioFormat { return c.format }
func (c *fakeChannel) Err() <-chan error           { return c.errCh }
func (c *fakeChannel) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

// TestServiceLifecycleAndEvents 走一遍 API-2.1/2.2/2.3/2.4/2.5，断言状态与事件转发。
func TestServiceLifecycleAndEvents(t *testing.T) {
	var mu sync.Mutex
	var states []string
	var levels int
	var ticks int
	var tickElapsed []int64

	svc := newTestService(t, func(o *recording.Options) {
		o.OpenChannel = func(string) (capture.Channel, error) {
			ch := newFakeChannel(capture.AudioFormat{SampleRate: 16000, Channels: 1}, fillConst(0.3))
			ch.delay = 200 * time.Microsecond
			return ch, nil
		}
	}, Emit{
		State: func(st contract.RecordingState) {
			mu.Lock()
			states = append(states, st.State)
			mu.Unlock()
		},
		Level: func(contract.RecordingLevel) {
			mu.Lock()
			levels++
			mu.Unlock()
		},
		Tick: func(tk contract.RecordingTick) {
			mu.Lock()
			ticks++
			tickElapsed = append(tickElapsed, tk.ElapsedMs)
			mu.Unlock()
		},
	})

	ctx := context.Background()
	st, err := svc.Start(ctx, contract.StartRequest{SourceID: "dev:a"})
	if err != nil {
		t.Fatalf("Start 失败：%v", err)
	}
	if st.State != contract.StateRecording || st.SessionID == "" {
		t.Fatalf("Start 后状态异常：%+v", st)
	}

	waitUntil(t, 5*time.Second, "录到 1.3s", func() bool { return svc.GetStateQuiet().ElapsedMs >= 1300 })

	if _, err := svc.Pause(ctx); err != nil {
		t.Fatalf("Pause 失败：%v", err)
	}
	if got := svc.GetStateQuiet().State; got != contract.StatePaused {
		t.Fatalf("Pause 后状态应为 paused，实际 %s", got)
	}
	if _, err := svc.Resume(ctx); err != nil {
		t.Fatalf("Resume 失败：%v", err)
	}
	if got := svc.GetStateQuiet().State; got != contract.StateRecording {
		t.Fatalf("Resume 后状态应为 recording，实际 %s", got)
	}
	st, err = svc.Stop(ctx)
	if err != nil {
		t.Fatalf("Stop 失败：%v", err)
	}
	if st.State != contract.StateCompleted {
		t.Fatalf("Stop 后状态应为 completed，实际 %s（lastError=%+v）", st.State, st.LastError)
	}

	mu.Lock()
	defer mu.Unlock()
	want := []string{
		contract.StatePreparing, contract.StateRecording,
		contract.StatePaused, contract.StateRecording, contract.StateFinalizing, contract.StateCompleted,
	}
	if len(states) != len(want) {
		t.Fatalf("状态事件序列长度 %d，期望 %d：%v", len(states), len(want), states)
	}
	for i := range want {
		if states[i] != want[i] {
			t.Fatalf("第 %d 个状态事件 %s，期望 %s（完整序列 %v）", i, states[i], want[i], states)
		}
	}
	if levels == 0 {
		t.Fatal("应转发 EV-02 电平事件")
	}
	if ticks == 0 {
		t.Fatal("应转发 EV-03 节拍事件")
	}
	// tick 1Hz 节流：间隔不小于 1000ms（除非最后一次是收尾）。
	for i := 1; i < len(tickElapsed); i++ {
		if d := tickElapsed[i] - tickElapsed[i-1]; d < TickIntervalMs-1 {
			t.Fatalf("tick 间隔 %dms 小于节流门槛 %dms：%v", d, TickIntervalMs, tickElapsed)
		}
	}
}

// TestServicePauseStopRejectedWhenIdle 断言未录制时的操作返回 ERR_STATE_FORBIDDEN。
func TestServicePauseStopRejectedWhenIdle(t *testing.T) {
	svc := newTestService(t, nil, Emit{})
	ctx := context.Background()

	if _, err := svc.Pause(ctx); !apierr.HasCode(err, apierr.CodeStateForbidden) {
		t.Fatalf("Pause 应返回 ERR_STATE_FORBIDDEN，实际 %v", err)
	}
	if _, err := svc.Stop(ctx); !apierr.HasCode(err, apierr.CodeStateForbidden) {
		t.Fatalf("Stop 应返回 ERR_STATE_FORBIDDEN，实际 %v", err)
	}
	if _, err := svc.Resume(ctx); !apierr.HasCode(err, apierr.CodeStateForbidden) {
		t.Fatalf("Resume 应返回 ERR_STATE_FORBIDDEN，实际 %v", err)
	}
	st, err := svc.GetState(ctx)
	if err != nil {
		t.Fatalf("GetState 不应返回错误：%v", err)
	}
	if st.State != contract.StateIdle {
		t.Fatalf("初始状态应为 idle，实际 %s", st.State)
	}
}

// TestServiceStartValidation 断言 Start 的参数校验与源不可用映射。
func TestServiceStartValidation(t *testing.T) {
	svc := newTestService(t, nil, Emit{})
	ctx := context.Background()
	if _, err := svc.Start(ctx, contract.StartRequest{}); !apierr.HasCode(err, apierr.CodeInvalidArgument) {
		t.Fatalf("空源应返回 ERR_INVALID_ARGUMENT，实际 %v", err)
	}

	svc2 := newTestService(t, func(o *recording.Options) {
		o.OpenChannel = func(string) (capture.Channel, error) {
			return nil, apierr.New(apierr.CodeCaptureUnavailable).WithDetail("平台不支持该源")
		}
	}, Emit{})
	if _, err := svc2.Start(ctx, contract.StartRequest{SourceID: "app:kook.exe"}); !apierr.HasCode(err, apierr.CodeCaptureUnavailable) {
		t.Fatalf("源不可用应返回 ERR_CAPTURE_UNAVAILABLE，实际 %v", err)
	}
}

// TestServiceRecoverAndExport 覆盖 API-2.6 / API-2.7 的转发与参数校验。
func TestServiceRecoverAndExport(t *testing.T) {
	svc := newTestService(t, func(o *recording.Options) {
		o.OpenChannel = func(string) (capture.Channel, error) {
			ch := newFakeChannel(capture.AudioFormat{SampleRate: 16000, Channels: 1}, fillConst(0.3))
			ch.delay = 200 * time.Microsecond
			return ch, nil
		}
	}, Emit{})
	ctx := context.Background()

	if _, err := svc.RecoverSession(ctx, "s_notexist"); !apierr.HasCode(err, apierr.CodeNotFound) {
		t.Fatalf("不存在的会话应返回 ERR_NOT_FOUND，实际 %v", err)
	}
	if _, err := svc.RecoverSession(ctx, ""); !apierr.HasCode(err, apierr.CodeInvalidArgument) {
		t.Fatalf("空 sessionId 应返回 ERR_INVALID_ARGUMENT，实际 %v", err)
	}

	if _, err := svc.Start(ctx, contract.StartRequest{SourceID: "dev:a"}); err != nil {
		t.Fatalf("Start 失败：%v", err)
	}
	waitUntil(t, 5*time.Second, "录到 1.3s", func() bool { return svc.GetStateQuiet().ElapsedMs >= 1300 })
	st, err := svc.Stop(ctx)
	if err != nil {
		t.Fatalf("Stop 失败：%v", err)
	}

	if _, err := svc.ExportAudio(ctx, st.SessionID, ""); !apierr.HasCode(err, apierr.CodeInvalidArgument) {
		t.Fatalf("空 targetPath 应返回 ERR_INVALID_ARGUMENT，实际 %v", err)
	}
	target := filepath.Join(t.TempDir(), "out.wav")
	got, err := svc.ExportAudio(ctx, st.SessionID, target)
	if err != nil {
		t.Fatalf("导出失败：%v", err)
	}
	if got != target {
		t.Fatalf("应返回导出路径 %q，实际 %q", target, got)
	}

	// 修复已完成的会话是幂等的：返回同一会话，不改变状态。
	sess, err := svc.RecoverSession(ctx, st.SessionID)
	if err != nil {
		t.Fatalf("对已完成会话调用 RecoverSession 不应失败：%v", err)
	}
	if sess.ID != st.SessionID {
		t.Fatalf("应返回同一会话，实际 %s", sess.ID)
	}
}

// TestServiceNoAudioHint 断言静音提示通过 EV-02 的 silentMs 上报，且不改状态。
func TestServiceNoAudioHint(t *testing.T) {
	var mu sync.Mutex
	var maxSilent int64

	svc := newTestService(t, func(o *recording.Options) {
		o.OpenChannel = func(string) (capture.Channel, error) {
			ch := newFakeChannel(capture.AudioFormat{SampleRate: 16000, Channels: 1}, fillConst(0))
			ch.delay = 100 * time.Microsecond
			return ch, nil
		}
	}, Emit{
		Level: func(l contract.RecordingLevel) {
			mu.Lock()
			if l.SilentMs > maxSilent {
				maxSilent = l.SilentMs
			}
			mu.Unlock()
		},
	})
	ctx := context.Background()
	if _, err := svc.Start(ctx, contract.StartRequest{SourceID: "dev:a"}); err != nil {
		t.Fatalf("Start 失败：%v", err)
	}
	waitUntil(t, 20*time.Second, "静音累计到 10.5s", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return maxSilent >= 10500
	})
	if got := svc.GetStateQuiet().State; got != contract.StateRecording {
		t.Fatalf("静音提示不得改变状态，当前 %s", got)
	}
	if _, err := svc.Stop(ctx); err != nil {
		t.Fatalf("Stop 失败：%v", err)
	}
}

// TestServiceShutdownStopsRecording 断言退出钩子会封存正在录制的会话。
func TestServiceShutdownStopsRecording(t *testing.T) {
	svc := newTestService(t, func(o *recording.Options) {
		o.OpenChannel = func(string) (capture.Channel, error) {
			ch := newFakeChannel(capture.AudioFormat{SampleRate: 16000, Channels: 1}, fillConst(0.3))
			ch.delay = 200 * time.Microsecond
			return ch, nil
		}
	}, Emit{})
	ctx := context.Background()
	st, err := svc.Start(ctx, contract.StartRequest{SourceID: "dev:a"})
	if err != nil {
		t.Fatalf("Start 失败：%v", err)
	}
	waitUntil(t, 5*time.Second, "录到 1.3s", func() bool { return svc.GetStateQuiet().ElapsedMs >= 1300 })

	if err := svc.ServiceShutdown(); err != nil {
		t.Fatalf("退出收尾失败：%v", err)
	}
	var row storage.Session
	if err := svc.holder.Current().GORM.Where("id = ?", st.SessionID).Take(&row).Error; err != nil {
		t.Fatalf("读取会话失败：%v", err)
	}
	if row.Status != contract.StateCompleted && row.Status != contract.StateFailed {
		t.Fatalf("退出后会话应已封存，实际状态 %s", row.Status)
	}
	if row.DurationMs == 0 {
		t.Fatal("退出封存应写入时长")
	}
}

// —— 测试装配 ——

// newTestService 在临时库上构造服务；opts 用于注入假通道等。
func newTestService(t *testing.T, opts func(*recording.Options), emit Emit) *Service {
	t.Helper()
	dir := t.TempDir()
	db, err := storage.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("打开测试库失败：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	holder := storage.NewHolder(db)
	root := t.TempDir()

	rec, err := recording.New(withOpts(recording.Options{
		Store:    recording.NewGORMStore(holder),
		DataRoot: func() (string, error) { return root, nil },
		Now:      func() time.Time { return time.UnixMilli(1_700_000_000_000) },
	}, opts))
	if err != nil {
		t.Fatalf("构造录制器失败：%v", err)
	}
	svc := New(rec, holder, emit)
	t.Cleanup(func() { _ = svc.ServiceShutdown() })
	return svc
}

func withOpts(base recording.Options, opts func(*recording.Options)) recording.Options {
	if opts != nil {
		opts(&base)
	}
	return base
}

// GetStateQuiet 是测试便捷方法（忽略 error，GetState 恒不报错）。
func (s *Service) GetStateQuiet() contract.RecordingState {
	st, _ := s.GetState(context.Background())
	return st
}

func waitUntil(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("等待超时（%s）：%s", timeout, what)
}

func fillConst(v float32) func(dst []float32) {
	return func(dst []float32) {
		for i := range dst {
			dst[i] = v
		}
	}
}
