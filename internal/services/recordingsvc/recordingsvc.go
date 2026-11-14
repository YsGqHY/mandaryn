// Package recordingsvc 是「录制与录音实例」的 Wails 薄壳（API-2.1 ~ API-2.7 / FR-03）。
//
// 职责边界（CLAUDE.md 铁律）：
//   - 本层**只做**类型转换、错误码映射与事件转发素材的编排；
//   - 状态机、音频管线、落盘、崩溃恢复全在 internal/recording（L2 领域包）；
//   - **不跑转写**：Stop 封存后转写由 transcriptsvc 独立发起（FR-03 录制与转写解耦）。
//
// 为什么事件通过回调注入而不是直接 import events：
// events 依赖 Wails，而这层要保持可单测（沿用 capturesvc 的做法）。
package recordingsvc

import (
	"context"
	"strings"
	"sync"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/recording"
	"vip.oberon.mandaryn/internal/storage"
	"vip.oberon.mandaryn/internal/utils/logx"
)

// TickIntervalMs 采集节拍（EV-03）的下限间隔：1Hz。
//
// 指标本身是 4Hz（EV-02 电平），但「已录时长 + 已存字节」这类数字 1Hz 足够，
// 再快只会让 UI 每帧重排（NFR-01 的 60fps 预算被无关重渲染吃掉）。
const TickIntervalMs = 1000

// Emit 是事件转发素材（由装配层桥接到 app.Event.Emit）。
//
// 三个事件分别对应 DOC-03 §3 的 EV-01 / EV-02 / EV-03。
type Emit struct {
	// State EV-01 recording:state（每次状态转换）。
	State func(contract.RecordingState)
	// Level EV-02 recording:level（4Hz 节流）。
	Level func(contract.RecordingLevel)
	// Tick EV-03 recording:tick（1Hz 节流）。
	Tick func(contract.RecordingTick)
}

// Service 暴露给前端的录制服务。
type Service struct {
	rec    *recording.Recorder
	holder *storage.Holder
	emit   Emit

	// lastTickMs 上一次转发 tick 时的逻辑时长（1Hz 节流状态）。
	lastTickMs int64

	// sessMu / sessionID 缓存「当前会话 ID」，来源是 EV-01 的状态快照。
	//
	// 为什么缓存而不是在指标回调里调 rec.State()：指标回调由落盘 goroutine 触发，
	// 而 Stop 的收尾会**持有 L2 的锁等这个 goroutine 结束**；回调里再去抢锁
	// 就形成「收尾等回调、回调等收尾」的相互等待（要到超时才解开）。
	// 缓存是唯一不引入锁序问题的做法。
	sessMu    sync.RWMutex
	sessionID string
}

// New 构造服务。
//
// rec 是 L2 编排器（由装配层用 storage.Holder / paths.Resolver / capture.Open 构造），
// 本层只把它的状态与指标搬到前端 DTO 上。
//
// 传 nil 的 emit 字段会在测试与启动早期被安全跳过。
func New(rec *recording.Recorder, holder *storage.Holder, emit Emit) *Service {
	s := &Service{rec: rec, holder: holder, emit: emit}
	// 把 L2 的回调注册进来：L2 不 import Wails，事件桥接只在这里发生。
	rec.SetHandlers(recording.Handlers{
		OnState:   s.onState,
		OnMetrics: s.onMetrics,
		OnNoAudio: s.onNoAudio,
	})
	return s
}

// Start 开始录制（API-2.1）：进入 ST-02 → ST-03。
//
// 可能错误：ERR_STATE_FORBIDDEN / ERR_CAPTURE_UNAVAILABLE / ERR_SOURCE_NOT_FOUND /
// ERR_DISK_FULL / ERR_PATH_UNWRITABLE。
//
// 注意 modelSource 的落库口径：本层只把「用户/设置里的当前值」写进会话快照
// （FR-08-3 要求导出保留来源），**不在这里预热引擎** —— 预热与降级属于
// enginesvc（API-5.x），录制不应依赖推理引擎是否就绪（默认延后转写）。
func (s *Service) Start(_ context.Context, req contract.StartRequest) (contract.RecordingState, error) {
	id := strings.TrimSpace(req.SourceID)
	if id == "" {
		return s.rec.State(), apierr.New(apierr.CodeInvalidArgument).
			WithDetail("sourceId 不能为空")
	}
	req.SourceID = id
	st, err := s.rec.Start(req)
	if err != nil {
		return st, apierr.From(err, mapStartFallback(err))
	}
	return st, nil
}

// Pause 暂停（API-2.2）：ST-03 → ST-04。
func (s *Service) Pause(_ context.Context) (contract.RecordingState, error) {
	st, err := s.rec.Pause()
	if err != nil {
		return st, apierr.From(err, apierr.CodeStateForbidden)
	}
	return st, nil
}

// Resume 继续（API-2.3）：ST-04 → ST-03。
//
// 可能错误：ERR_STATE_FORBIDDEN / ERR_CAPTURE_UNAVAILABLE（源在暂停期间失效）。
func (s *Service) Resume(_ context.Context) (contract.RecordingState, error) {
	st, err := s.rec.Resume()
	if err != nil {
		return st, apierr.From(err, apierr.CodeCaptureUnavailable)
	}
	return st, nil
}

// Stop 停止并封存录音实例（API-2.4）：→ ST-05 → ST-06 / ST-08 / ST-09。
//
// **只封存，不转写**（FR-03 铁律）。转写由 transcriptsvc 在会话可回放后独立发起。
//
// 可能错误：ERR_STATE_FORBIDDEN / ERR_DB_WRITE。
func (s *Service) Stop(_ context.Context) (contract.RecordingState, error) {
	st, err := s.rec.Stop()
	if err != nil {
		return st, apierr.From(err, apierr.CodeDBWrite)
	}
	return st, nil
}

// GetState 幂等查询当前录制状态（API-2.5，窗口重新打开时用）。
func (s *Service) GetState(_ context.Context) (contract.RecordingState, error) {
	return s.rec.State(), nil
}

// RecoverSession 把未正常结束的会话修补为可回放（API-2.6 / UC-07）。
//
// 可能错误：ERR_NOT_FOUND（会话不存在）/ ERR_DB_WRITE。
func (s *Service) RecoverSession(ctx context.Context, sessionID string) (contract.Session, error) {
	if strings.TrimSpace(sessionID) == "" {
		return contract.Session{}, apierr.New(apierr.CodeInvalidArgument).
			WithDetail("sessionId 不能为空")
	}
	sess, err := s.rec.RecoverSession(ctx, sessionID)
	if err != nil {
		return contract.Session{}, apierr.From(err, apierr.CodeDBWrite)
	}
	return sess, nil
}

// ExportAudio 另存录音实例为单个音频文件（API-2.7）。
//
// **不改库**（FR-03-11）：导出后会话仍在库里、仍可回放。
// targetPath 为空时返回 ERR_INVALID_ARGUMENT —— 原生「另存为」对话框由前端
// NativeDialogs 打开，后端不弹 UI（CLAUDE.md 铁律）。
//
// 可能错误：ERR_NOT_FOUND / ERR_DISK_FULL / ERR_INVALID_ARGUMENT。
func (s *Service) ExportAudio(ctx context.Context, sessionID, targetPath string) (string, error) {
	path, err := s.rec.ExportAudio(ctx, sessionID, targetPath)
	if err != nil {
		return "", apierr.From(err, apierr.CodeInvalidArgument)
	}
	return path, nil
}

// RecoverUnfinished 启动自检：修补上次异常结束的会话（供装配层在启动时调用）。
//
// 不是前端 API（无对应 API-x 编号）：它是启动流程的一部分，
// 放在服务上只是为了复用同一份 L2 依赖。
func (s *Service) RecoverUnfinished(ctx context.Context) ([]recording.RecoverResult, error) {
	results, err := s.rec.RecoverUnfinished(ctx)
	if err != nil {
		return nil, apierr.From(err, apierr.CodeDBWrite)
	}
	if len(results) > 0 {
		logx.For("recordingsvc").Info("启动自检修复了未完成会话", "count", len(results))
	}
	return results, nil
}

// ServiceShutdown 是 Wails 的服务生命周期钩子（不是前端 API，绑定生成器会跳过）。
//
// 正在录制时走正常 Stop 把音频封口：直接退出会留下 status=recording 的残留行。
func (s *Service) ServiceShutdown() error { return s.rec.Shutdown() }

// onState 把 L2 状态快照原样转发为 EV-01（DTO 已在 L2 侧构造成 contract 类型）。
func (s *Service) onState(st contract.RecordingState) {
	s.sessMu.Lock()
	if st.SessionID != s.sessionID {
		// 新会话：重置 tick 节流状态，避免沿用上一场的时间轴。
		s.sessionID = st.SessionID
		s.lastTickMs = 0
	}
	s.sessMu.Unlock()
	if s.emit.State != nil {
		s.emit.State(st)
	}
}

// onMetrics 把 4Hz 指标拆成 EV-02（电平）与 1Hz EV-03（节拍）。
//
// 拆两个事件而不是塞一个：电平条要 4Hz 才不卡顿，而「已录时长/已存字节」
// 要 1Hz 才不刷屏；合并会强迫其中一个用不属于自己的频率。
func (s *Service) onMetrics(m recording.Metrics) {
	sid := s.cachedSessionID()
	if s.emit.Level != nil {
		s.emit.Level(contract.RecordingLevel{
			SessionID: sid,
			RMS:       m.LevelRMS,
			Peak:      m.LevelPeak,
			SilentMs:  m.ZeroInputMs,
		})
	}
	if s.emit.Tick == nil {
		return
	}
	// tick 按逻辑时长（帧计数）做 1Hz 节流，不依赖时钟：
	// 指标本身 4Hz，若原样转发，UI 的「已录时长」每秒重排四次纯属浪费。
	s.sessMu.Lock()
	due := m.RecordedMs-s.lastTickMs >= TickIntervalMs || m.RecordedMs == 0
	if due {
		s.lastTickMs = m.RecordedMs
	}
	s.sessMu.Unlock()
	if !due {
		return
	}
	s.emit.Tick(contract.RecordingTick{
		SessionID:    sid,
		ElapsedMs:    m.RecordedMs,
		BytesWritten: m.BytesWritten,
		// CharCount 只在「边录边转」开启时有意义（FR-03-9）：未开启时恒为 0。
		CharCount: int(s.rec.LiveCharCount()),
	})
}

// onNoAudio 转发「未检测到音频」（FR-03-7）。
//
// 它**不改状态、不产生空录音实例**，只作为非阻塞提示素材：
// 因此这里不构造新的事件名（DOC-03 §3 无对应事件），而是复用 EV-02 的
// SilentMs 字段（前端按 silentMs ≥ 10000 展示提示），并打一条元数据日志。
func (s *Service) onNoAudio(m recording.Metrics) {
	sid := s.cachedSessionID()
	if s.emit.Level != nil {
		s.emit.Level(contract.RecordingLevel{
			SessionID: sid,
			RMS:       m.LevelRMS,
			Peak:      m.LevelPeak,
			SilentMs:  m.ZeroInputMs,
		})
	}
	logx.For("recordingsvc").Info("未检测到音频（仅提示，不改变录制状态）",
		"session", sid, "silentMs", m.ZeroInputMs)
}

// cachedSessionID 当前会话 ID（来自最近一次 EV-01 快照；无会话时为空）。
func (s *Service) cachedSessionID() string {
	s.sessMu.RLock()
	defer s.sessMu.RUnlock()
	return s.sessionID
}

// mapStartFallback 决定 Start 失败的兜底错误码：
// 已经有码的错误原样保留，未登记的按「源不可用」处理（录制起不来最常见的原因是源）。
func mapStartFallback(err error) string {
	if apierr.HasCode(err, apierr.CodeDiskFull) {
		return apierr.CodeDiskFull
	}
	return apierr.CodeCaptureUnavailable
}
