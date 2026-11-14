package recording

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/capture"
	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/storage"
)

// TestRecorderStartStopSealsSession 走完整链路：起录 → 采集 → 停止 → 封存。
//
// 断言重点：
//   - 状态落到 completed；
//   - 库里 status/duration_ms/size_bytes/audio_path 都已回填，transcription_status 仍是 none；
//   - WAV 头声明的帧数与 duration_ms 严格一致（帧计数是唯一时间基）。
func TestRecorderStartStopSealsSession(t *testing.T) {
	fake := newFakeChannel(capture.AudioFormat{SampleRate: 48000, Channels: 2}, fillConst(0.3))
	fake.delay = 500 * time.Microsecond

	rec := newTestRecorder(t, func(o *Options) {
		o.OpenChannel = func(string) (capture.Channel, error) { return fake, nil }
	})

	st, err := rec.Start(contract.StartRequest{SourceID: "dev:test", ModelSource: contract.ModelSourceLocal})
	if err != nil {
		t.Fatalf("开始录制失败：%v", err)
	}
	if st.State != contract.StateRecording {
		t.Fatalf("状态应为 recording，实际 %s", st.State)
	}
	if st.SessionID == "" {
		t.Fatal("会话 ID 不能为空")
	}

	// 录够 1.1 秒音频（空录音门槛是 1s，低于它会走 failed 分支）。
	waitFor(t, 10*time.Second, "采集到至少 1.1s 音频", func() bool {
		return rec.State().ElapsedMs >= 1100
	})

	st, err = rec.Stop()
	if err != nil {
		t.Fatalf("停止录制失败：%v", err)
	}
	if st.State != contract.StateCompleted {
		t.Fatalf("状态应为 completed，实际 %s（lastError=%+v）", st.State, st.LastError)
	}
	if st.ElapsedMs < 1000 {
		t.Fatalf("时长应 ≥1000ms，实际 %d", st.ElapsedMs)
	}

	row := getSession(t, rec, st.SessionID)
	if row.Status != contract.StateCompleted {
		t.Fatalf("库中状态应为 completed，实际 %s", row.Status)
	}
	if row.TranscriptionStatus != contract.TranscriptionNone {
		t.Fatalf("转写状态应保持 none（Stop 不跑转写），实际 %s", row.TranscriptionStatus)
	}
	if row.DurationMs != st.ElapsedMs {
		t.Fatalf("库中时长 %d 与状态快照 %d 不一致", row.DurationMs, st.ElapsedMs)
	}
	if row.AudioPath == "" || row.AudioFormat != AudioFormatWAV {
		t.Fatalf("音频元数据未回填：path=%q format=%q", row.AudioPath, row.AudioFormat)
	}
	if row.EndedAt == 0 {
		t.Fatal("ended_at 未回填")
	}
	if row.ModelSource != contract.ModelSourceLocal {
		t.Fatalf("model_source 应为 local，实际 %s", row.ModelSource)
	}

	// 文件层面：帧数、时长、字节数三者必须自洽。
	info, err := InspectWAV(mustAudioPath(t, rec, row.AudioPath))
	if err != nil {
		t.Fatalf("读取落盘文件失败：%v", err)
	}
	if info.SampleRate != InternalSampleRate || info.Channels != 1 || info.BitsPerSample != 16 {
		t.Fatalf("落盘格式不符：%dHz/%dch/%dbit", info.SampleRate, info.Channels, info.BitsPerSample)
	}
	if info.DurationMs() != row.DurationMs {
		t.Fatalf("头换算时长 %dms 与库中 duration_ms %d 不一致", info.DurationMs(), row.DurationMs)
	}
	if row.SizeBytes != info.FileBytes {
		t.Fatalf("库中 size_bytes %d 与文件长度 %d 不一致", row.SizeBytes, info.FileBytes)
	}
}

// TestRecorderStopIsNotIdempotentButRejects 断言停止后再次停止被拒（不静默重复封存）。
func TestRecorderStopRejectsWhenIdle(t *testing.T) {
	rec := newTestRecorder(t, nil)
	if _, err := rec.Stop(); !apierr.HasCode(err, apierr.CodeStateForbidden) {
		t.Fatalf("空闲时 Stop 应返回 ERR_STATE_FORBIDDEN，实际 %v", err)
	}
	if _, err := rec.Pause(); !apierr.HasCode(err, apierr.CodeStateForbidden) {
		t.Fatalf("空闲时 Pause 应返回 ERR_STATE_FORBIDDEN，实际 %v", err)
	}
	if _, err := rec.Resume(); !apierr.HasCode(err, apierr.CodeStateForbidden) {
		t.Fatalf("空闲时 Resume 应返回 ERR_STATE_FORBIDDEN，实际 %v", err)
	}
}

// TestRecorderRejectsDuplicateStart 断言重复 Start 被拒（recording → recording 非法）。
func TestRecorderRejectsDuplicateStart(t *testing.T) {
	fake := newFakeChannel(capture.AudioFormat{SampleRate: 48000, Channels: 2}, fillConst(0.2))
	fake.delay = 5 * time.Millisecond
	rec := newTestRecorder(t, func(o *Options) {
		o.OpenChannel = func(string) (capture.Channel, error) { return fake, nil }
	})
	if _, err := rec.Start(contract.StartRequest{SourceID: "dev:a"}); err != nil {
		t.Fatalf("首次开始失败：%v", err)
	}
	t.Cleanup(func() { _, _ = rec.Stop() })

	_, err := rec.Start(contract.StartRequest{SourceID: "dev:a"})
	if !apierr.HasCode(err, apierr.CodeStateForbidden) {
		t.Fatalf("重复开始应返回 ERR_STATE_FORBIDDEN，实际 %v", err)
	}
}

// TestRecorderPauseResumeKeepsTimelineContinuous 断言暂停期间时间轴不推进、恢复后连续。
func TestRecorderPauseResumeKeepsTimelineContinuous(t *testing.T) {
	fake := newFakeChannel(capture.AudioFormat{SampleRate: 16000, Channels: 1}, fillConst(0.3))
	fake.delay = 200 * time.Microsecond
	rec := newTestRecorder(t, func(o *Options) {
		o.OpenChannel = func(string) (capture.Channel, error) { return fake, nil }
	})

	if _, err := rec.Start(contract.StartRequest{SourceID: "dev:a"}); err != nil {
		t.Fatalf("开始录制失败：%v", err)
	}
	waitFor(t, 5*time.Second, "录到 600ms", func() bool { return rec.State().ElapsedMs >= 600 })
	pausedAt := rec.State().ElapsedMs

	st, err := rec.Pause()
	if err != nil {
		t.Fatalf("暂停失败：%v", err)
	}
	if st.State != contract.StatePaused {
		t.Fatalf("状态应为 paused，实际 %s", st.State)
	}

	// 暂停后要先把「暂停前已采到但还在缓冲里」的音频写完（这是真实采集到的音频，
	// 不该丢），因此时长会再涨一小段；等它稳定后才断言不再推进。
	//
	// 注意合成源比实时快几百倍，缓冲里可能积了上秒的音频；真机上是几十毫秒量级。
	stableAt := waitStableElapsed(t, rec, pausedAt)
	if got := rec.State().ElapsedMs; got != stableAt {
		t.Fatalf("暂停后时长应在 %dms 稳定，实际 %dms", stableAt, got)
	}

	if _, err := rec.Resume(); err != nil {
		t.Fatalf("继续失败：%v", err)
	}
	waitFor(t, 5*time.Second, "恢复后继续录到 +600ms", func() bool {
		return rec.State().ElapsedMs >= stableAt+600
	})

	st, err = rec.Stop()
	if err != nil {
		t.Fatalf("停止失败：%v", err)
	}
	if st.State != contract.StateCompleted {
		t.Fatalf("状态应为 completed，实际 %s（lastError=%+v）", st.State, st.LastError)
	}

	// 时间轴标记：暂停 + 继续各一条（TBL-10，回放可见）。
	row := getSession(t, rec, st.SessionID)
	info, err := InspectWAV(mustAudioPath(t, rec, row.AudioPath))
	if err != nil {
		t.Fatalf("读取落盘文件失败：%v", err)
	}
	if info.DurationMs() != st.ElapsedMs {
		t.Fatalf("落盘时长 %dms 与状态时长 %dms 不一致", info.DurationMs(), st.ElapsedMs)
	}
	t.Logf("暂停点 %dms，缓冲排空后稳定在 %dms，总时长 %dms（暂停期间不产生新帧）",
		pausedAt, stableAt, st.ElapsedMs)
}

// TestRecorderEmptyRecordingMarkedFailed 断言「时长 < 1s」不落成 completed。
//
// 策略见 recorder.go 的注释：保留音频与行、状态标 failed 并写明原因 ——
// 静默丢弃会让用户以为软件没反应（FR-03-7 不产生空录音实例）。
//
// 注意 Stop 本身**不返回错误**：停止与封存都成功了，空录音只是结果状态，
// 原因通过 state.LastError 与 degrade_reason 传给 UI。
func TestRecorderEmptyRecordingMarkedFailed(t *testing.T) {
	fake := newFakeChannel(capture.AudioFormat{SampleRate: 48000, Channels: 2}, fillConst(0.3))
	fake.delay = time.Millisecond
	rec := newTestRecorder(t, func(o *Options) {
		o.OpenChannel = func(string) (capture.Channel, error) { return fake, nil }
	})

	st, err := rec.Start(contract.StartRequest{SourceID: "dev:a"})
	if err != nil {
		t.Fatalf("开始失败：%v", err)
	}
	st, err = rec.Stop() // 立刻停止：时长必然 < 1s
	if err != nil {
		t.Fatalf("空录音不是操作失败，Stop 不应返回错误：%v", err)
	}
	if st.State != contract.StateFailed {
		t.Fatalf("空录音状态应为 failed，实际 %s", st.State)
	}
	if st.LastError == nil {
		t.Fatal("空录音应通过 LastError 说明原因")
	}
	if !contains(st.LastError.Detail, "空录音") {
		t.Fatalf("LastError.Detail 应说明是空录音，实际 %q（session 必须保留以便用户自查）", st.LastError.Detail)
	}
	// 前端靠动作键把空录音与真正的状态冲突区分开（两者同为 ERR_STATE_FORBIDDEN）。
	if st.LastError.Action != EmptyRecordingAction {
		t.Fatalf("空录音的动作键应为 %q，实际 %q", EmptyRecordingAction, st.LastError.Action)
	}
	row := getSession(t, rec, st.SessionID)
	if row.Status != contract.StateFailed {
		t.Fatalf("库中状态应为 failed，实际 %s", row.Status)
	}
	if row.DegradeReason == "" {
		t.Fatal("空录音应写明原因（degrade_reason）")
	}
	// 音频文件仍保留（用户可自查或删除），不是零字节的孤儿。
	if _, statErr := os.Stat(mustAudioPath(t, rec, row.AudioPath)); statErr != nil {
		t.Fatalf("空录音的音频文件应保留：%v", statErr)
	}
}

// TestRecorderNoAudioWarnDoesNotChangeState 断言全零输入 10 秒：
// 回调被触发一次、状态仍是 recording、不产生空录音实例。
func TestRecorderNoAudioWarnDoesNotChangeState(t *testing.T) {
	// 不设延迟：让「10 秒音频」在毫秒级内被喂完（否则测试要跑 10 秒）。
	// 用 16k 单声道走重采样直通路径，进一步把这段时间压到几十毫秒。
	fake := newFakeChannel(capture.AudioFormat{SampleRate: 16000, Channels: 1}, fillZero())
	fake.delay = 200 * time.Microsecond

	var mu sync.Mutex
	var noAudioCount int
	var lastSilent int64
	rec := newTestRecorder(t, func(o *Options) {
		o.OpenChannel = func(string) (capture.Channel, error) { return fake, nil }
	})
	rec.SetHandlers(Handlers{
		OnNoAudio: func(m Metrics) {
			mu.Lock()
			noAudioCount++
			lastSilent = m.ZeroInputMs
			mu.Unlock()
		},
	})

	if _, err := rec.Start(contract.StartRequest{SourceID: "dev:a"}); err != nil {
		t.Fatalf("开始失败：%v", err)
	}
	waitFor(t, 10*time.Second, "喂入 ≥10.5 秒静音", func() bool {
		return rec.State().ElapsedMs >= 10500
	})

	mu.Lock()
	count := noAudioCount
	silent := lastSilent
	mu.Unlock()
	if count != 1 {
		t.Fatalf("「未检测到音频」应只上报 1 次，实际 %d 次", count)
	}
	if silent < SilenceWarnMs {
		t.Fatalf("静音时长应 ≥ %dms，实际 %d", SilenceWarnMs, silent)
	}
	if got := rec.State().State; got != contract.StateRecording {
		t.Fatalf("静音提示不得改变状态，当前 %s", got)
	}

	// 停止后：全零录音按空录音处理（failed），而不是留一个 completed 的空会话。
	// Stop 本身成功（封存完成），因此不返回错误，原因在 LastError 里。
	st, err := rec.Stop()
	if err != nil {
		t.Fatalf("全零录音的 Stop 不应返回操作错误：%v", err)
	}
	if st.State != contract.StateFailed {
		t.Fatalf("全零录音状态应为 failed，实际 %s", st.State)
	}
	if st.LastError == nil || !contains(st.LastError.Detail, "空录音") {
		t.Fatalf("应通过 LastError 说明空录音原因，实际 %+v", st.LastError)
	}
}

// TestRecorderDeviceLostPausesSession 断言采集通道失效 → 转 paused（会话不丢）。
func TestRecorderDeviceLostPausesSession(t *testing.T) {
	fake := newFakeChannel(capture.AudioFormat{SampleRate: 16000, Channels: 1}, fillConst(0.3))
	fake.delay = 200 * time.Microsecond
	fake.failAfter = 120 // 约 2 秒音频后失效（保证时长 > 1s）
	fake.failErr = errBoom
	fake.pushErr = true

	rec := newTestRecorder(t, func(o *Options) {
		o.OpenChannel = func(string) (capture.Channel, error) { return fake, nil }
	})
	if _, err := rec.Start(contract.StartRequest{SourceID: "dev:a"}); err != nil {
		t.Fatalf("开始失败：%v", err)
	}

	waitFor(t, 10*time.Second, "通道失效后转为 paused", func() bool {
		return rec.State().State == contract.StatePaused
	})
	st := rec.State()
	if st.LastError == nil {
		t.Fatal("暂停时应带出错误原因（设备丢失）")
	}
	if st.LastError.Code != apierr.CodeCaptureDeviceLost {
		t.Fatalf("错误码应为 ERR_CAPTURE_DEVICE_LOST，实际 %s", st.LastError.Code)
	}

	// 会话仍在：可以停止并封存已录音频。
	stopState, err := rec.Stop()
	if err != nil {
		t.Fatalf("设备丢失后仍应能停止并封存：%v", err)
	}
	if stopState.State != contract.StateCompleted && stopState.State != contract.StateFailed {
		t.Fatalf("停止后状态异常：%s", stopState.State)
	}
	row := getSession(t, rec, stopState.SessionID)
	if row.AudioPath == "" {
		t.Fatal("音频路径应保留")
	}
	// 设备丢失是「暂停」而不是「结束」：已录音频必须可回放（时长 > 0）。
	if row.DurationMs == 0 {
		t.Fatal("设备丢失前录到的音频不应丢失")
	}
}

// TestRecorderResumeRekeysChannelAfterLoss 断言设备丢失后 Resume 会重连同一个源。
func TestRecorderResumeRekeysChannelAfterLoss(t *testing.T) {
	first := newFakeChannel(capture.AudioFormat{SampleRate: 16000, Channels: 1}, fillConst(0.3))
	first.delay = 200 * time.Microsecond
	first.failAfter = 60
	first.pushErr = true

	second := newFakeChannel(capture.AudioFormat{SampleRate: 16000, Channels: 1}, fillConst(0.3))
	second.delay = 200 * time.Microsecond

	var mu sync.Mutex
	opens := 0
	rec := newTestRecorder(t, func(o *Options) {
		o.OpenChannel = func(string) (capture.Channel, error) {
			mu.Lock()
			defer mu.Unlock()
			opens++
			if opens == 1 {
				return first, nil
			}
			return second, nil
		}
	})

	if _, err := rec.Start(contract.StartRequest{SourceID: "dev:a"}); err != nil {
		t.Fatalf("开始失败：%v", err)
	}
	waitFor(t, 10*time.Second, "第一次通道失效进入 paused", func() bool {
		return rec.State().State == contract.StatePaused
	})

	if _, err := rec.Resume(); err != nil {
		t.Fatalf("继续失败：%v", err)
	}
	mu.Lock()
	gotOpens := opens
	mu.Unlock()
	if gotOpens < 2 {
		t.Fatalf("继续时应重连源（OpenChannel 应被调用 ≥2 次），实际 %d 次", gotOpens)
	}
	waitFor(t, 5*time.Second, "重连后继续采集", func() bool {
		return rec.State().ElapsedMs >= 1200 && rec.State().State == contract.StateRecording
	})
	if _, err := rec.Stop(); err != nil {
		t.Fatalf("停止失败：%v", err)
	}
}

// TestRecorderResumeFailsWhenSourceGone 断言重连失败时停留在 paused 并返回
// ERR_CAPTURE_UNAVAILABLE（会话与音频都保留）。
func TestRecorderResumeFailsWhenSourceGone(t *testing.T) {
	first := newFakeChannel(capture.AudioFormat{SampleRate: 16000, Channels: 1}, fillConst(0.3))
	first.delay = 200 * time.Microsecond
	first.failAfter = 60
	first.pushErr = true

	var mu sync.Mutex
	opens := 0
	rec := newTestRecorder(t, func(o *Options) {
		o.OpenChannel = func(string) (capture.Channel, error) {
			mu.Lock()
			defer mu.Unlock()
			opens++
			if opens == 1 {
				return first, nil
			}
			return nil, apierr.New(apierr.CodeSourceNotFound).WithDetail("设备已拔出")
		}
	})

	if _, err := rec.Start(contract.StartRequest{SourceID: "dev:a"}); err != nil {
		t.Fatalf("开始失败：%v", err)
	}
	waitFor(t, 10*time.Second, "进入 paused", func() bool {
		return rec.State().State == contract.StatePaused
	})

	st, err := rec.Resume()
	if !apierr.HasCode(err, apierr.CodeSourceNotFound) {
		t.Fatalf("重连失败应返回 ERR_SOURCE_NOT_FOUND，实际 %v", err)
	}
	if st.State != contract.StatePaused {
		t.Fatalf("重连失败后应停留在 paused，实际 %s", st.State)
	}
	if _, stopErr := rec.Stop(); stopErr != nil {
		t.Fatalf("仍应能停止：%v", stopErr)
	}
}

// TestRecorderStartFailurePaths 断言准备阶段的失败都落到 failed 且带错误码。
func TestRecorderStartFailurePaths(t *testing.T) {
	t.Run("源不可用", func(t *testing.T) {
		rec := newTestRecorder(t, func(o *Options) {
			o.OpenChannel = func(string) (capture.Channel, error) {
				return nil, apierr.New(apierr.CodeCaptureUnavailable).WithDetail("平台不支持")
			}
		})
		st, err := rec.Start(contract.StartRequest{SourceID: "app:kook.exe"})
		if !apierr.HasCode(err, apierr.CodeCaptureUnavailable) {
			t.Fatalf("应返回 ERR_CAPTURE_UNAVAILABLE，实际 %v", err)
		}
		if st.State != contract.StateFailed {
			t.Fatalf("状态应为 failed，实际 %s", st.State)
		}
	})

	t.Run("源 ID 非法", func(t *testing.T) {
		rec := newTestRecorder(t, nil)
		if _, err := rec.Start(contract.StartRequest{SourceID: "kook.exe"}); !apierr.HasCode(err, apierr.CodeInvalidArgument) {
			t.Fatalf("非法源 ID 应返回 ERR_INVALID_ARGUMENT，实际 %v", err)
		}
	})

	t.Run("空源 ID", func(t *testing.T) {
		rec := newTestRecorder(t, nil)
		if _, err := rec.Start(contract.StartRequest{}); !apierr.HasCode(err, apierr.CodeInvalidArgument) {
			t.Fatalf("空源 ID 应返回 ERR_INVALID_ARGUMENT，实际 %v", err)
		}
	})

	t.Run("数据目录不可用", func(t *testing.T) {
		fake := newFakeChannel(capture.AudioFormat{SampleRate: 48000, Channels: 2}, fillConst(0.1))
		rec := newTestRecorder(t, func(o *Options) {
			o.DataRoot = func() (string, error) { return "", os.ErrPermission }
			o.OpenChannel = func(string) (capture.Channel, error) { return fake, nil }
		})
		if st, err := rec.Start(contract.StartRequest{SourceID: "dev:a"}); !apierr.HasCode(err, apierr.CodePathUnwritable) {
			t.Fatalf("应返回 ERR_PATH_UNWRITABLE，实际 %v（state=%s）", err, st.State)
		}
	})
}

// TestRecorderConcurrentPauseStop 并发 Pause/Resume/Stop：不 panic、不产生非法状态。
func TestRecorderConcurrentPauseStop(t *testing.T) {
	fake := newFakeChannel(capture.AudioFormat{SampleRate: 16000, Channels: 1}, fillConst(0.3))
	fake.delay = 200 * time.Microsecond
	rec := newTestRecorder(t, func(o *Options) {
		o.OpenChannel = func(string) (capture.Channel, error) { return fake, nil }
	})
	if _, err := rec.Start(contract.StartRequest{SourceID: "dev:a"}); err != nil {
		t.Fatalf("开始失败：%v", err)
	}
	waitFor(t, 5*time.Second, "先录到 1.2s，避开空录音分支", func() bool {
		return rec.State().ElapsedMs >= 1200
	})

	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				var err error
				switch i % 3 {
				case 0:
					_, err = rec.Pause()
				case 1:
					_, err = rec.Resume()
				default:
					_, err = rec.Stop()
				}
				if err != nil && !apierr.HasCode(err, apierr.CodeStateForbidden) {
					errs <- err
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("并发调用出现非 ERR_STATE_FORBIDDEN 错误：%v", err)
	}

	// 收敛到终态：要么已完成封存，要么还停着（状态必须已知）。
	final := rec.State().State
	if !IsKnownState(final) {
		t.Fatalf("并发后出现未知状态：%q", final)
	}
	if final == contract.StateRecording || final == contract.StatePaused || final == contract.StateFinalizing {
		if _, err := rec.Stop(); err != nil {
			t.Fatalf("收尾停止失败：%v", err)
		}
	}
	if st := rec.State().State; st != contract.StateCompleted && st != contract.StateFailed && st != contract.StateInterrupted {
		t.Fatalf("收尾后状态异常：%s", st)
	}
}

// TestRecorderStartAfterCompleted 断言 completed 之后可以开始新会话（「开始」按钮可用）。
func TestRecorderStartAfterCompleted(t *testing.T) {
	// 每次 OpenChannel 都给新通道：真实场景下 Stop 会关闭通道，
	// 复用同一个 fake 实例会让第二场读到已关闭的通道（测试假故障）。
	rec := newTestRecorder(t, func(o *Options) {
		o.OpenChannel = func(string) (capture.Channel, error) {
			ch := newFakeChannel(capture.AudioFormat{SampleRate: 16000, Channels: 1}, fillConst(0.3))
			ch.delay = 200 * time.Microsecond
			return ch, nil
		}
	})

	if _, err := rec.Start(contract.StartRequest{SourceID: "dev:a", Title: "第一场"}); err != nil {
		t.Fatalf("第一场开始失败：%v", err)
	}
	waitFor(t, 5*time.Second, "第一场录到 1.2s", func() bool { return rec.State().ElapsedMs >= 1200 })
	first, err := rec.Stop()
	if err != nil {
		t.Fatalf("第一场停止失败：%v", err)
	}
	if first.State != contract.StateCompleted {
		t.Fatalf("第一场应完成，实际 %s", first.State)
	}

	second, err := rec.Start(contract.StartRequest{SourceID: "dev:a", Title: "第二场"})
	if err != nil {
		t.Fatalf("第二场开始失败：%v", err)
	}
	if second.SessionID == first.SessionID {
		t.Fatal("新会话应有不同的 ID")
	}
	waitFor(t, 5*time.Second, "第二场录到 1.2s", func() bool { return rec.State().ElapsedMs >= 1200 })
	if _, err := rec.Stop(); err != nil {
		t.Fatalf("第二场停止失败：%v", err)
	}
}

// TestRecorderShutdownSealsActiveSession 断言退出时正在录制会走正常封存
// （而不是留下 status=recording 的残留行让下次启动去修）。
func TestRecorderShutdownSealsActiveSession(t *testing.T) {
	fake := newFakeChannel(capture.AudioFormat{SampleRate: 16000, Channels: 1}, fillConst(0.3))
	fake.delay = 200 * time.Microsecond
	rec := newTestRecorder(t, func(o *Options) {
		o.OpenChannel = func(string) (capture.Channel, error) { return fake, nil }
	})
	st, err := rec.Start(contract.StartRequest{SourceID: "dev:a"})
	if err != nil {
		t.Fatalf("开始失败：%v", err)
	}
	waitFor(t, 5*time.Second, "录到 1.2s", func() bool { return rec.State().ElapsedMs >= 1200 })

	if err := rec.Shutdown(); err != nil {
		t.Fatalf("退出收尾失败：%v", err)
	}
	row := getSession(t, rec, st.SessionID)
	if row.Status == contract.StateRecording || row.Status == contract.StateFinalizing {
		t.Fatalf("退出后不应残留中间状态：%s", row.Status)
	}
	if row.DurationMs == 0 {
		t.Fatal("退出封存应写入时长")
	}
}

// —— 测试辅助 ——

func getSession(t *testing.T, rec *Recorder, id string) *storage.Session {
	t.Helper()
	row, err := rec.opts.Store.GetSession(context.Background(), id)
	if err != nil {
		t.Fatalf("读取会话失败：%v", err)
	}
	return row
}

func mustAudioPath(t *testing.T, rec *Recorder, rel string) string {
	t.Helper()
	abs, err := rec.audioAbsPath(rel)
	if err != nil {
		t.Fatalf("解析音频路径失败：%v", err)
	}
	return abs
}

// waitStableElapsed 等待「已录时长」停止增长并返回稳定值。
//
// 为什么需要它：暂停只停止**采集**，缓冲里已经采到的音频仍会继续落盘
// （那是真实音频，不该丢）。合成源比实时快几百倍，缓冲里可能积了上秒音频，
// 所以「暂停后时长立刻不变」这种断言只有在真实节奏下才成立。
func waitStableElapsed(t *testing.T, rec *Recorder, from int64) int64 {
	t.Helper()
	last := from
	same := 0
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		cur := rec.State().ElapsedMs
		if cur == last {
			same++
			if same >= 5 { // 连续 5 次（约 25ms）不变即视为稳定
				return cur
			}
		} else {
			last = cur
			same = 0
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待时长稳定超时，最后值 %dms", last)
	return last
}
