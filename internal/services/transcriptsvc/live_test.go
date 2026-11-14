package transcriptsvc

import (
	"context"
	"testing"
	"time"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/asr"
	"vip.oberon.mandaryn/internal/transcribe"
)

// 边录边转的**薄壳**用例（FR-03-10）：
//   - 没有流式能力时必须明确报不可用（错误码 + 原因），而不是假装可用；
//   - 开启过边录边转的会话不再参与「空闲自动转写」，但仍可手动重转。

// TestLiveAvailabilityWithoutStreaming 断言默认引擎（只有 Finalize）下不可用。
func TestLiveAvailabilityWithoutStreaming(t *testing.T) {
	f := newFixture(t, true)

	got, err := f.svc.LiveAvailability(context.Background())
	if err != nil {
		t.Fatalf("LiveAvailability 不应返回错误：%v", err)
	}
	if got.Available {
		t.Fatalf("假引擎没有流式能力，不应判定可用：%+v", got)
	}
	if got.ErrorCode != apierr.CodeLocalUnavailable {
		t.Fatalf("错误码 = %q，期望 %q", got.ErrorCode, apierr.CodeLocalUnavailable)
	}
	if got.Reason == "" {
		t.Fatal("不可用时必须给出原因（前端要显示「缺流式模型包」）")
	}
}

// TestNewLiveSinkUnavailable 断言没有流式能力时建旁路会明确失败（调用方忽略即可）。
func TestNewLiveSinkUnavailable(t *testing.T) {
	f := newFixture(t, true)

	runner, err := f.svc.NewLiveSink(context.Background(), "s_audio01")
	if err == nil {
		t.Fatalf("没有流式能力时不应返回 runner：%v", runner)
	}
	if !apierr.HasCode(err, apierr.CodeLocalUnavailable) {
		t.Fatalf("错误码应为 ERR_LOCAL_UNAVAILABLE，实际 %v", err)
	}
}

// TestNewLiveSinkHasStreaming 断言有流式能力时能建出旁路，且不碰录制主状态。
func TestNewLiveSinkHasStreaming(t *testing.T) {
	f := newFixture(t, true)
	f.svc.engine = &streamingFakeEngine{
		fakeEngine: fakeEngine{id: "zipformer-stream@1"},
		streamID:   "zipformer-stream@1",
	}

	avail, err := f.svc.LiveAvailability(context.Background())
	if err != nil {
		t.Fatalf("LiveAvailability: %v", err)
	}
	if !avail.Available {
		t.Fatalf("有流式引擎时应可用：%+v", avail)
	}

	runner, err := f.svc.NewLiveSink(context.Background(), "s_audio01")
	if err != nil {
		t.Fatalf("NewLiveSink: %v", err)
	}
	defer runner.Close()
	if runner.SessionID() != "s_audio01" {
		t.Fatalf("会话 ID = %q", runner.SessionID())
	}
	// 录制主状态不得被边录边转改写（转写轴与录制轴正交）。
	if row := f.session(t, "s_audio01"); row.Status != "completed" {
		t.Fatalf("录制主状态被改写：%q", row.Status)
	}
}

// TestNewLiveSinkDoesNotLoadEngine 断言 `NewLiveSink` **不加载**流式模型。
//
// 为什么这条是硬要求：`NewLiveSink` 在 `recording.Start` 的同步路径上被调用，
// 而那时 `OpenChannel` 已经把采集管道打开了。本地流式模型加载要数秒，
// 在这里同步加载会把会话开头的音频顶掉 —— 等于「推理阻塞了采集」，
// 违反「边录边转绝不阻塞落盘/采集」。
//
// 所以服务层只做便宜探测（`StreamingAvailable`），实例用 asr.LazyStreaming 包一层，
// 真正加载推迟到推理 goroutine 第一次 Accept。
func TestNewLiveSinkDoesNotLoadEngine(t *testing.T) {
	f := newFixture(t, true)
	fake := &streamingFakeEngine{
		fakeEngine: fakeEngine{id: "zipformer-stream@1"},
		streamID:   "zipformer-stream@1",
	}
	f.svc.engine = fake

	runner, err := f.svc.NewLiveSink(context.Background(), "s_audio01")
	if err != nil {
		t.Fatalf("NewLiveSink: %v", err)
	}
	defer runner.Close()

	if fake.loads != 0 {
		t.Fatalf("NewLiveSink 触发了 %d 次流式引擎加载（会阻塞会话开头音频）", fake.loads)
	}
	// 便宜探测仍应问答正确（前端靠它区分「缺模型包」与「开关没开」）。
	avail, err := f.svc.LiveAvailability(context.Background())
	if err != nil || !avail.Available {
		t.Fatalf("LiveAvailability = %+v, %v", avail, err)
	}
}

// TestShouldAutoTranscribeSkipsLive 断言「开过边录边转的会话不再自动整段转写」。
//
// 这是「Stop 之后不再自动触发整段转写」在服务层的判定：
// 会话已 live_transcribed → 自动排队被跳过；手动重转仍然可以（幂等覆盖）。
func TestShouldAutoTranscribeSkipsLive(t *testing.T) {
	f := newFixture(t, true)
	ctx := context.Background()

	plain, err := f.svc.ShouldAutoTranscribe(ctx, "s_audio01")
	if err != nil {
		t.Fatalf("ShouldAutoTranscribe: %v", err)
	}
	if !plain {
		t.Fatal("未开边录边转的会话应参与空闲自动转写")
	}

	// 标记为「录制时开了边录边转」。
	if err := f.db.GORM.Model(&struct{}{}).Table("sessions").
		Where("id = ?", "s_audio01").Update("live_transcribed", true).Error; err != nil {
		t.Fatalf("更新标记失败：%v", err)
	}
	live, err := f.svc.ShouldAutoTranscribe(ctx, "s_audio01")
	if err != nil {
		t.Fatalf("ShouldAutoTranscribe: %v", err)
	}
	if live {
		t.Fatal("开过边录边转的会话不应再被自动整段转写（避免重复推理）")
	}

	// 手动重转仍然可用：这是用户显式动作，幂等覆盖旧片段。
	if _, err := f.svc.StartTranscription(ctx, "s_audio01"); err != nil {
		t.Fatalf("手动重转应可用：%v", err)
	}
	row := f.waitDone(t, "s_audio01")
	if row.TranscriptionStatus != transcribe.StatusDone {
		t.Fatalf("手动重转后状态 = %q", row.TranscriptionStatus)
	}
	if n := len(f.segments(t, "s_audio01")); n == 0 {
		t.Fatal("手动重转应产出片段")
	}
}

// TestShouldAutoTranscribeNotFound 断言不存在的会话返回 ERR_NOT_FOUND。
func TestShouldAutoTranscribeNotFound(t *testing.T) {
	f := newFixture(t, true)
	if _, err := f.svc.ShouldAutoTranscribe(context.Background(), "s_missing"); !apierr.HasCode(err, apierr.CodeNotFound) {
		t.Fatalf("应返回 ERR_NOT_FOUND，实际 %v", err)
	}
}

// streamingFakeEngine 是可选的流式引擎（同时满足 Recognizer 与 StreamingEngine）。
type streamingFakeEngine struct {
	fakeEngine
	streamID string
	resets   int
	accepts  int
	// loads 统计 Streaming() 被调用的次数（= 真实装配里「构造 + 预热」的次数）。
	// 非 0 即说明有人在录制 Start 路径上把数秒的模型加载挡在了采集前面。
	loads int
}

func (s *streamingFakeEngine) Accept(samples []float32) (string, error) {
	s.accepts++
	if len(samples) == 0 {
		return "", nil
	}
	return "字", nil
}

func (s *streamingFakeEngine) Reset() error { s.resets++; return nil }

func (s *streamingFakeEngine) Warm(context.Context) error { return nil }

func (s *streamingFakeEngine) Feed([]float32) (<-chan asr.Partial, error) {
	ch := make(chan asr.Partial)
	close(ch)
	return ch, nil
}

func (s *streamingFakeEngine) Health(context.Context) asr.HealthStatus {
	return asr.HealthStatus{State: "ready", EngineID: s.streamID}
}

func (s *streamingFakeEngine) Close() error { return nil }

// —— streamingRouter 的三个入口（与 asr.Router 同形）——
//
// 本域靠类型断言拿流式能力，因此测试替身必须同时扮演「路由器」与「流式引擎」，
// 这样才与真实装配（Router 自己既是 Recognizer 又是 Streaming 提供方）一致。

func (s *streamingFakeEngine) Streaming() (asr.StreamingEngine, error) {
	s.loads++
	return s, nil
}
func (s *streamingFakeEngine) StreamingAvailable() error { return nil }
func (s *streamingFakeEngine) StreamingEngineID() string { return s.streamID }

// 编译期断言：测试替身必须同时满足两条契约。
var (
	_ transcribe.Recognizer        = (*streamingFakeEngine)(nil)
	_ asr.StreamingEngine          = (*streamingFakeEngine)(nil)
	_ = time.Millisecond
)
