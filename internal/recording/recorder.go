package recording

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/audioflac"
	"vip.oberon.mandaryn/internal/capture"
	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/paths"
	"vip.oberon.mandaryn/internal/storage"
	"vip.oberon.mandaryn/internal/utils/logx"
)

// Options 是 Recorder 的依赖注入点。
//
// 全部用函数字段而不是具体类型，是为了让本包：
//   - 不依赖 paths.Resolver 的具体实现（只要一个「给我 dataRoot」的函数）；
//   - 能在测试里注入假通道 / 假时钟 / 临时目录，**不碰真麦克风与真设备**。
type Options struct {
	// Store 持久层端口（必填）。
	Store Store
	// DataRoot 返回当前生效的数据根目录（持久化录音必填；内存直传模式不调用）。
	DataRoot func() (string, error)
	// OpenChannel 打开捕获通道，默认 capture.Open。
	OpenChannel func(sourceID string) (capture.Channel, error)
	// ResolveSource 解析源的展示名与类型，默认按源 ID 前缀推导
	// （不探测设备存在性：可用性由 OpenChannel 的真实失败来回答）。
	ResolveSource func(sourceID string) (name, kind string, err error)
	// DefaultModelSource 返回 app_settings 里的当前模型来源（local|online）；
	// 为空时按 local 处理。
	DefaultModelSource func() string
	// Now 墙钟，仅用于 sessions.started_at/ended_at 的**显示时间**；
	// 时间轴一律用帧计数（见 doc.go 铁律 1）。默认 time.Now。
	Now func() time.Time
	// ReadBufferSamples 每次从通道读取的样本数（含声道展开），默认 4096。
	ReadBufferSamples int
	// StopTimeout 收尾时等待采集/落盘 goroutine 的上限，默认 3s。
	StopTimeout time.Duration

	// LiveEnabled 报告「边录边转」开关当前是否开启（FR-03-10，默认 **false**）。
	//
	// 装配层传 `appsettings` 快照里的 `live_transcribe`。nil = 永远关闭，
	// 此时录制路径与未实现该功能前完全一致（见 live.go 的不变量 1）。
	// 开关**只在下一个会话生效**：这里只在 Start 时读一次，录制中不重读。
	LiveEnabled func() bool
	// NewLiveSink 为一个会话创建音频旁路接收端；返回 nil 表示本次不旁路。
	NewLiveSink func(sessionID string) LiveSink
	// NewMemorySink 为在线内存直传创建接收端。创建失败时会话不会进入 recording，
	// 且不会创建音频文件；错误由调用方映射为启动失败。
	NewMemorySink func(sessionID string) (MemorySink, error)
}

// Handlers 是向 L3 薄壳暴露的回调集合。
//
// 用注册式回调而不是直接 import events：events 依赖 Wails，
// 而本包是 L2 领域层（禁止 import Wails）。
//
// 回调会被采集/落盘 goroutine 调用，**必须非阻塞**（只做类型转换 + emit）。
type Handlers struct {
	// OnState 状态每次转换后触发（EV-01 recording:state）。
	OnState func(contract.RecordingState)
	// OnMetrics 指标上报，250ms 节流（EV-02 recording:level / EV-03 recording:tick）。
	OnMetrics func(Metrics)
	// OnNoAudio 连续静音 ≥ 10s 首次触发（FR-03-7）。只提示，不改状态。
	OnNoAudio func(Metrics)
}

// Recorder 是录制编排器：状态机 + 采集 goroutine + 落盘 goroutine + 封存。
//
// 职责边界：
//   - **只做采集与封存**。Stop 不在收尾里跑任何推理（FR-03 铁律）；
//     转写由 transcriptsvc 在会话封存后独立发起（transcription_status 是另一条轴）。
//   - 不写 `degraded` / `throttled` / `suspended` 三个正交标记（那是引擎与预算域的事），
//     这里只在 DTO 里原样带出（当前恒为 false）。
type Recorder struct {
	opts Options
	sm   *StateMachine

	mu sync.Mutex
	// run 是当前（或最近一次）会话的运行时。
	run *run
	// lastErr 最近一次错误，供 RecordingState.LastError 展示。
	lastErr *apierr.Err
	// 预算域叠加的正交标记（NFR-08）：由装配层在档位变化时写入。
	budgetLevel     string
	budgetThrottled bool
	budgetSuspended bool

	hMu sync.RWMutex
	h   Handlers
}

// New 构造录制编排器。
func New(opts Options) (*Recorder, error) {
	if opts.Store == nil {
		return nil, apierr.New(apierr.CodeInvalidArgument).WithDetail("录制器缺少 Store 依赖")
	}

	if opts.OpenChannel == nil {
		opts.OpenChannel = capture.Open
	}
	if opts.ResolveSource == nil {
		opts.ResolveSource = defaultResolveSource
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.ReadBufferSamples <= 0 {
		opts.ReadBufferSamples = 4096
	}
	if opts.StopTimeout <= 0 {
		opts.StopTimeout = 3 * time.Second
	}
	return &Recorder{opts: opts, sm: NewStateMachine()}, nil
}

// SetHandlers 注册回调（由 L3 薄壳在装配期调用）。
func (r *Recorder) SetHandlers(h Handlers) {
	r.hMu.Lock()
	r.h = h
	r.hMu.Unlock()
}

// State 返回当前状态快照（API-2.5 的幂等查询）。
func (r *Recorder) State() contract.RecordingState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stateLocked()
}

// Metrics 最近一次指标快照（不等节拍）。
func (r *Recorder) Metrics() Metrics {
	r.mu.Lock()
	rn := r.run
	r.mu.Unlock()
	if rn == nil {
		return Metrics{}
	}
	return rn.monitor.Snapshot(rn.writer.SizeBytes())
}

// LiveCharCount 返回当前会话边录边转已定稿的字符数（FR-03-9）。
//
// 未开启边录边转（sink 为 nil）时恒为 0，与「默认关闭」的行为一致。
// 只读一个原子计数，不阻塞落盘与收尾。
func (r *Recorder) LiveCharCount() int64 {
	r.mu.Lock()
	rn := r.run
	active := rn != nil && !rn.finished
	var sink LiveSink
	if active {
		sink = rn.sink
	}
	r.mu.Unlock()
	if sink == nil {
		return 0
	}
	return sink.CharCount()
}

// Start 开始录制：ST-01 → ST-02 → ST-03（API-2.1）。
//
// 失败路径统一走 ST-02 → ST-09（failed），并把会话标记为 failed：
// 不允许「调用失败但状态停在 preparing」——UI 会永远显示转圈。
func (r *Recorder) Start(req contract.StartRequest) (contract.RecordingState, error) {
	return r.StartWithAutoName(req, true)
}

// StartWithAutoName 应用命名策略；设置读取留给调用端，不改变采集/封存链路。
func (r *Recorder) StartWithAutoName(req contract.StartRequest, autoName bool) (contract.RecordingState, error) {
	return r.StartWithSettings(req, autoName, AudioFormatWAV, contract.SensitivityStandard)
}

// StartWithSettings snapshots the recording options for one new session.
func (r *Recorder) StartWithSettings(req contract.StartRequest, autoName bool, audioFormat, sensitivity string) (contract.RecordingState, error) {
	return r.StartWithPolicy(req, autoName, audioFormat, sensitivity, "", false)
}

// StartWithPolicy starts a session using an explicit model/privacy policy snapshot.
// noAudioPersist is honored only for online model sources.
func (r *Recorder) StartWithPolicy(req contract.StartRequest, autoName bool, audioFormat, sensitivity, modelSource string, noAudioPersist bool) (contract.RecordingState, error) {
	audioFormat = strings.ToLower(strings.TrimSpace(audioFormat))
	if audioFormat == "" {
		audioFormat = AudioFormatWAV
	}
	if audioFormat != AudioFormatWAV && audioFormat != AudioFormatFLAC {
		return r.State(), apierr.New(apierr.CodeInvalidArgument).WithDetail("不支持的录音格式")
	}
	sourceID := strings.TrimSpace(req.SourceID)
	if sourceID == "" {
		return r.State(), apierr.New(apierr.CodeInvalidArgument).WithDetail("sourceId 不能为空")
	}

	r.mu.Lock()
	if err := r.sm.To(contract.StatePreparing); err != nil {
		st := r.stateLocked()
		r.mu.Unlock()
		return st, err
	}
	r.lastErr = nil
	preparing := r.stateLocked()
	r.mu.Unlock()
	r.emitState(preparing)

	// fail 把「准备阶段的任何失败」收敛成 ST-09 + ERR。
	fail := func(cause *apierr.Err, rn *run) (contract.RecordingState, error) {
		r.mu.Lock()
		if rn != nil {
			rn.finished = true
			rn.closeChannel()
		}
		r.lastErr = cause
		// preparing → failed 是合法转换；若已不在 preparing（理论上不会）则强制对齐。
		if err := r.sm.To(contract.StateFailed); err != nil {
			_ = r.sm.Force(contract.StateFailed)
		}
		if rn != nil && rn.writer != nil {
			_ = rn.writer.Seal()
			_ = rn.writer.Close()
			r.markSessionFailedLocked(rn, cause)
		}
		// 准备失败或异常终止不得让未上传音频继续排队。
		if rn != nil && rn.sink != nil {
			if rn.noAudioPersist {
				if sink, ok := rn.sink.(MemorySink); ok {
					sink.Abort()
				} else {
					rn.sink.Close()
				}
			} else {
				rn.sink.Close()
			}
		}
		st := r.stateLocked()
		r.mu.Unlock()
		r.emitState(st)
		return st, cause
	}

	name, kind, err := r.opts.ResolveSource(sourceID)
	if err != nil {
		return fail(apierr.From(err, apierr.CodeSourceNotFound), nil)
	}

	modelSource = strings.TrimSpace(modelSource)
	if modelSource == "" && r.opts.DefaultModelSource != nil {
		modelSource = strings.TrimSpace(r.opts.DefaultModelSource())
	}
	if modelSource == "" {
		modelSource = contract.ModelSourceLocal
	}
	noDisk := modelSource == contract.ModelSourceOnline && noAudioPersist

	var dataRoot string
	if !noDisk {
		if r.opts.DataRoot == nil {
			return fail(apierr.New(apierr.CodePathUnwritable).WithDetail("数据目录不可用"), nil)
		}
		dataRoot, err = r.opts.DataRoot()
		if err != nil || strings.TrimSpace(dataRoot) == "" {
			return fail(apierr.New(apierr.CodePathUnwritable).
				WithDetail("数据目录不可用：%v", err), nil)
		}
	}

	ch, err := r.opts.OpenChannel(sourceID)
	if err != nil {
		return fail(openChannelError(err), nil)
	}
	format := ch.Format()
	pipeline, err := NewPipeline(format)
	if err != nil {
		_ = ch.Close()
		return fail(apierr.From(err, apierr.CodeCaptureUnavailable), nil)
	}

	id := newSessionID()
	relPath := ""
	absPath := ""
	var writer audioWriter
	if noDisk {
		writer = newMemoryWriter()
	} else {
		relPath = path.Join(paths.SessionsDirName, id, "audio."+audioFormat)
		absPath = filepath.Join(dataRoot, filepath.FromSlash(relPath))
		if audioFormat == AudioFormatFLAC {
			writer, err = audioflac.Create(absPath)
		} else {
			writer, err = CreateWriter(absPath)
		}
		if err != nil {
			_ = ch.Close()
			return fail(apierr.From(err, apierr.CodeDiskFull), nil)
		}
	}

	// 边录边转（FR-03-10）：开关只在**会话开始时**读一次，录制中不重读
	// （语义是「只在下一个会话生效」，与模型来源切换同规则）。
	liveOn := noDisk || (r.opts.LiveEnabled != nil && r.opts.LiveEnabled())

	now := r.opts.Now()
	title := strings.TrimSpace(req.Title)
	// 每次新建会话读取设置；显式标题不受影响，关闭时保留空标题供界面兜底。
	if title == "" && autoName {
		title = defaultTitle(now, name)
	}

	rn := &run{
		id:             id,
		title:          title,
		sourceID:       sourceID,
		sourceName:     name,
		sourceKind:     kind,
		modelSource:    modelSource,
		noAudioPersist: noDisk,
		audioPath:      relPath,
		audioFormat:    audioFormat,
		absPath:        absPath,
		startedAt:      now,
		ch:             ch,
		pipeline:       pipeline,
		writer:         writer,
		monitor:        NewMonitor(),
		notifyCh:       make(chan struct{}, 1),
		ringClosed:     make(chan struct{}),
		writerDone:     make(chan struct{}),
	}
	rn.readerDone = make(chan struct{})
	close(rn.readerDone) // 尚未启动 reader：视为「当前没有活着的 reader」

	row := &storage.Session{
		ID:          id,
		Title:       title,
		SourceID:    sourceID,
		SourceName:  name,
		SourceKind:  kind,
		ModelSource: modelSource,
		Status:      contract.StateRecording,
		StartedAt:   now.UnixMilli(),
		AudioPath:   relPath,
		AudioFormat: func() string {
			if noDisk {
				return ""
			}
			return audioFormat
		}(),
		VadPreset:           contract.SnapshotVadPreset(sensitivity),
		TranscriptionStatus: contract.TranscriptionNone,
		LiveTranscribed:     liveOn,
	}
	// 会话行必须在开始采集**之前**写入：晚写会让「起录成功但库中没有行」的窗口
	// 变长，崩溃恢复扫不到这个会话（音频就成孤儿文件了）。
	if err := r.opts.Store.CreateSession(context.Background(), row); err != nil {
		_ = ch.Close()
		_ = writer.Seal()
		_ = writer.Close()
		return fail(apierr.From(err, apierr.CodeDBWrite), nil)
	}

	// 内存直传 sink 必须在会话行写入后、录制状态可见前准备成功。
	// 这样失败不会暴露一个看似正在录制、却无法上传的无盘会话。
	if noDisk {
		if r.opts.NewMemorySink == nil {
			return fail(apierr.New(apierr.CodeEngineNotReady).WithDetail("在线内存转写接收端不可用"), rn)
		}
		rn.sink, err = r.opts.NewMemorySink(id)
		if err != nil || rn.sink == nil {
			if err == nil {
				err = errors.New("在线内存转写接收端为空")
			}
			return fail(apierr.From(err, apierr.CodeEngineNotReady), rn)
		}
	} else if liveOn && r.opts.NewLiveSink != nil {
		rn.sink = r.opts.NewLiveSink(id)
	}

	r.mu.Lock()
	r.run = rn
	if err := r.sm.To(contract.StateRecording); err != nil {
		r.mu.Unlock()
		_ = ch.Close()
		return fail(err.(*apierr.Err), rn)
	}
	st := r.stateLocked()
	r.mu.Unlock()
	r.emitState(st)

	logx.For("recording").Info("开始录制",
		"session", id, "source", sourceID, "format", pipeline.String(), "modelSource", modelSource,
		"noAudioPersist", noDisk)

	// 落盘 goroutine 先起：reader 一推数据就要有人取，否则环形缓冲白转。
	go r.writeLoop(rn)
	r.startReader(rn, ch)
	return st, nil
}

// Pause 暂停采集：ST-03 → ST-04（API-2.2）。
//
// 暂停时**不关闭捕获通道**，只是丢弃后续样本：时间轴由帧计数驱动，
// 丢弃即「不推进时间轴」，恢复后同一文件时间轴连续（DOC-02 §6.2 暂停/继续语义）。
func (r *Recorder) Pause() (contract.RecordingState, error) {
	r.mu.Lock()
	rn := r.run
	if rn == nil {
		st := r.stateLocked()
		r.mu.Unlock()
		return st, apierr.New(apierr.CodeStateForbidden).
			WithDetail("非法状态转换：%s → %s（无活动会话）", r.sm.Current(), contract.StatePaused)
	}
	if err := r.sm.To(contract.StatePaused); err != nil {
		st := r.stateLocked()
		r.mu.Unlock()
		return st, err
	}
	rn.setPaused(true)
	atMs := rn.writer.DurationMs()
	st := r.stateLocked()
	r.mu.Unlock()
	r.emitState(st)

	// 标记位写库放在锁外：它是「回放可见性」的附加信息，不该拖慢状态切换。
	r.appendMarker(rn, atMs, contract.MarkerPaused, "用户暂停")
	logx.For("recording").Info("暂停录制", "session", rn.id, "atMs", atMs)
	return st, nil
}

// Resume 继续采集：ST-04 → ST-03（API-2.3）。
//
// 若通道在暂停期间失效（设备拔出 / 应用退出），这里会尝试重连同一个源；
// 重连失败返回 ERR_CAPTURE_UNAVAILABLE 并**停留在 paused**（会话不丢，音频保留）。
func (r *Recorder) Resume() (contract.RecordingState, error) {
	r.mu.Lock()
	rn := r.run
	if rn == nil {
		st := r.stateLocked()
		r.mu.Unlock()
		return st, apierr.New(apierr.CodeStateForbidden).
			WithDetail("非法状态转换：%s → %s（无活动会话）", r.sm.Current(), contract.StateRecording)
	}
	if err := r.sm.To(contract.StateRecording); err != nil {
		st := r.stateLocked()
		r.mu.Unlock()
		return st, err
	}
	rn.setPaused(false)
	needReopen := rn.channelDead()
	r.mu.Unlock()

	if needReopen {
		ch, err := r.opts.OpenChannel(rn.sourceID)
		if err != nil {
			cause := openChannelError(err)
			r.mu.Lock()
			rn.setPaused(true)
			// 回到 paused（recording → paused 合法）：连接不上就不能假装在录。
			if terr := r.sm.To(contract.StatePaused); terr != nil {
				_ = r.sm.Force(contract.StatePaused)
			}
			r.lastErr = cause
			st := r.stateLocked()
			r.mu.Unlock()
			r.emitState(st)
			r.appendMarker(rn, rn.writer.DurationMs(), contract.MarkerSourceLost,
				"继续失败：源不可用")
			logx.For("recording").Warn("继续录制失败：源不可用",
				"session", rn.id, "code", cause.Code)
			return st, cause
		}
		rn.setChannel(ch)
		r.startReader(rn, ch)
	}

	atMs := rn.writer.DurationMs()
	st := r.State()
	r.emitState(st)
	r.appendMarker(rn, atMs, contract.MarkerResumed, "用户继续")
	logx.For("recording").Info("继续录制", "session", rn.id, "atMs", atMs)
	return st, nil
}

// Stop 停止录制并封存录音实例：ST-03/ST-04 → ST-05 → ST-06（API-2.4）。
//
// **只做封存，绝不同步跑转写**（FR-03 铁律）：转写由 transcriptsvc 在封存后
// 独立发起。收尾越短，崩溃时丢音频的窗口越小。
func (r *Recorder) Stop() (contract.RecordingState, error) {
	r.mu.Lock()
	rn := r.run
	if rn == nil {
		st := r.stateLocked()
		r.mu.Unlock()
		return st, apierr.New(apierr.CodeStateForbidden).
			WithDetail("非法状态转换：%s → %s（无活动会话）", r.sm.Current(), contract.StateFinalizing)
	}
	if err := r.sm.To(contract.StateFinalizing); err != nil {
		st := r.stateLocked()
		r.mu.Unlock()
		return st, err
	}
	finalizing := r.stateLocked()
	r.mu.Unlock()
	r.emitState(finalizing)

	r.mu.Lock()
	st, err := r.finalizeLocked(rn, contract.StateCompleted, nil)
	r.mu.Unlock()
	r.emitState(st)
	return st, err
}

// Shutdown 应用退出时的收尾（L3 的 ServiceShutdown 调用）。
//
// 正在录制时**走正常 Stop**，把音频封口并落库：直接退出会留下
// status=recording 的残留行，只能等下个行程由 Recover 修补（少 200ms 且
// 用户会看到「上次异常中断」的惊吓）。
func (r *Recorder) Shutdown() error {
	// 应用退出不得继续排空在线内存音频：先发 Abort，再让录制主链路正常停采集。
	r.mu.Lock()
	if rn := r.run; rn != nil && !rn.finished && rn.noAudioPersist && rn.sink != nil {
		rn.setShutdownAbort(true)
		if sink, ok := rn.sink.(MemorySink); ok {
			sink.Abort()
		}
	}
	r.mu.Unlock()

	state := r.sm.Current()
	switch state {
	case contract.StateRecording, contract.StatePaused:
		if _, err := r.Stop(); err != nil {
			logx.For("recording").Warn("退出前封存录制失败", "state", state, "err", err.Error())
			return err
		}
		return nil
	case contract.StatePreparing, contract.StateFinalizing:
		logx.For("recording").Warn("退出时仍有录制在准备/收尾，交由下次启动恢复", "state", state)
	}
	return nil
}

// finalizeLocked 执行封存（r.mu 已持有）。outcome 是期望的终态。
//
// 顺序是有讲究的（每一步都在缩小「崩溃丢音频」的窗口）：
//  1. 停采集（关通道 → reader 退出）；
//  2. 排空重采样器与环形缓冲（把最后几百毫秒写完）；
//  3. 等落盘 goroutine 写完并 Seal（回填 WAV 头长度）；
//  4. 再写数据库元数据（库写失败时音频已可用，标 interrupted 而不是 failed）。
func (r *Recorder) finalizeLocked(rn *run, outcome string, cause *apierr.Err) (contract.RecordingState, error) {
	if rn.finished {
		return r.stateLocked(), nil
	}
	rn.finished = true
	rn.setStopping(true)
	if cause != nil {
		r.lastErr = cause
	}

	// 1) 停采集：关闭通道让阻塞中的 ReadPCM 返回 io.EOF。
	rn.closeChannel()
	r.waitDone(rn.readerDoneCh(), r.opts.StopTimeout, rn.id, "采集")

	// 2) 冲刷管线尾段。
	rn.pipeline.Flush()
	rn.notify()

	// 3) 封口落盘（writer 自己在退出时 Seal，这里再兜一次，幂等）。
	rn.closeRing()
	r.waitDone(rn.writerDone, r.opts.StopTimeout, rn.id, "落盘")
	sealErr := rn.writer.Seal()
	if sealErr != nil {
		logx.For("recording").Warn("封存音频失败", "session", rn.id, "err", sealErr.Error())
	}
	// 必须显式关闭文件句柄：不关会让文件被本进程占住，
	// 用户在资源管理器里删不掉、Windows 上「删除会话」也会失败（DEV-ISSUES-S2 S2-03）。
	if err := rn.writer.Close(); err != nil {
		logx.For("recording").Warn("关闭音频文件失败", "session", rn.id, "err", err.Error())
	}

	frames := rn.writer.Frames()
	durationMs := FramesToMs(frames)
	sizeBytes := rn.writer.SizeBytes()

	if sealErr != nil {
		// Leave the non-final database row eligible for startup recovery.
		// A damaged stream header must never be advertised as a completed file.
		if !rn.isShutdownAbort() {
			if sink, ok := rn.sink.(MemorySink); ok {
				sink.Abort()
			} else if rn.sink != nil {
				rn.sink.Close()
			}
		}
		r.lastErr = apierr.From(wrapFSError(sealErr, "封存音频失败，已保留录音供恢复"), apierr.CodePathUnwritable)
		_ = r.sm.Force(contract.StateInterrupted)
		return r.stateLocked(), r.lastErr
	}

	// 空录音策略（FR-03-7「不产生空录音实例」）：
	//   时长 < 1s 或全程峰值低于 -60dBFS → 不落成 completed，改标 failed 并写明原因。
	//   为什么是 failed 而不是删除：音频与行都留着，用户能在库里看到「这次没录到声音」
	//   并能自己删；静默删掉会让用户以为软件没反应。见 DEV-ISSUES-S2 S2-05。
	//
	// 注意它**不是操作失败**：停止与封存都成功了，因此不回错误码，
	// 只把原因放进 state.lastError 让 UI 提示（API-2.4 的错误码只有
	// ERR_STATE_FORBIDDEN / ERR_DB_WRITE，空录音不在其列）。
	emptyNote := ""
	if outcome == contract.StateCompleted {
		switch {
		case durationMs < MinDurationMs:
			emptyNote = fmt.Sprintf("空录音：时长仅 %dms（< %dms）", durationMs, MinDurationMs)
		case rn.monitor.MaxPeak() < SilentPeakThreshold:
			emptyNote = fmt.Sprintf("空录音：全程未检测到音频（峰值 %.5f）", rn.monitor.MaxPeak())
		}
		if emptyNote != "" {
			outcome = contract.StateFailed
			r.lastErr = apierr.New(apierr.CodeStateForbidden).
				WithAction(EmptyRecordingAction).
				WithDetail("%s", emptyNote)
		}
	}

	// opErr 只装「操作本身失败」的错误（写库失败）；空录音不算。
	var opErr *apierr.Err

	// 4) 写元数据（字段 map：零值也要写进去，见 Store.UpdateSession 注释）。
	now := r.opts.Now().UnixMilli()
	fields := map[string]any{
		"status":       outcome,
		"ended_at":     now,
		"duration_ms":  durationMs,
		"size_bytes":   sizeBytes,
		"audio_path":   rn.audioPath,
		"audio_format": rn.audioFormat,
	}
	if rn.noAudioPersist {
		fields["audio_path"] = ""
		fields["audio_format"] = ""
		fields["size_bytes"] = int64(0)
	}
	if emptyNote != "" {
		fields["degrade_reason"] = emptyNote
	}
	if derr := r.opts.Store.UpdateSession(context.Background(), rn.id, fields); derr != nil {
		// 库写失败：音频已经可用，按 ST-08（interrupted）处理而不是 failed ——
		// 语义是「音频保住了，元数据可修」，RecoverSession 之后能补齐。
		logx.For("recording").Error("封存会话元数据失败", "session", rn.id,
			"code", apierr.From(derr, apierr.CodeDBWrite).Code)
		outcome = contract.StateInterrupted
		opErr = apierr.From(derr, apierr.CodeDBWrite)
		r.lastErr = opErr
	}

	abortSink := rn.pipeline.Dropped() > 0 || outcome != contract.StateCompleted || opErr != nil || cause != nil || rn.isShutdownAbort()
	if rn.sink != nil {
		if abortSink {
			if !rn.isShutdownAbort() {
				if sink, ok := rn.sink.(MemorySink); ok {
					sink.Abort()
				} else {
					rn.sink.Close()
				}
			}
		} else {
			rn.sink.Close()
		}
	}

	if err := r.sm.To(outcome); err != nil {
		_ = r.sm.Force(outcome)
	}
	st := r.stateLocked()
	logx.For("recording").Info("录制已封存",
		"session", rn.id, "status", outcome, "durationMs", durationMs,
		"frames", frames, "bytes", sizeBytes, "dropped", rn.pipeline.Dropped())
	// 注意：opErr 是 *apierr.Err，直接 `return st, opErr` 会把 nil 指针装进
	// error 接口，得到「非 nil 的 nil error」——调用方 if err != nil 会误判失败
	// （DEV-ISSUES-S2 S2-09）。必须显式分支。
	if opErr == nil {
		return st, nil
	}
	return st, opErr
}

// markSessionFailedLocked 准备阶段失败时把会话行标 failed（尽力而为）。
func (r *Recorder) markSessionFailedLocked(rn *run, cause *apierr.Err) {
	if rn.id == "" {
		return
	}
	fields := map[string]any{
		"status":         contract.StateFailed,
		"ended_at":       r.opts.Now().UnixMilli(),
		"degrade_reason": truncateNote(cause.Message),
	}
	if err := r.opts.Store.UpdateSession(context.Background(), rn.id, fields); err != nil {
		logx.For("recording").Warn("标记会话失败时写库失败", "session", rn.id, "err", err.Error())
	}
}

// startReader 启动一个采集 epoch。
//
// 为什么允许有多个 epoch：暂停期间源可能失效，Resume 会重连并起新的 reader；
// 落盘 goroutine 与 WAV 文件贯穿整个会话，所以时间轴连续。
func (r *Recorder) startReader(rn *run, ch capture.Channel) {
	done := make(chan struct{})
	rn.setReaderDone(done)
	go r.readLoop(rn, ch, done)
}

// readLoop 采集循环：读原始 PCM → 管线（混音/重采样）→ 环形缓冲。
func (r *Recorder) readLoop(rn *run, ch capture.Channel, done chan struct{}) {
	var lostErr error
	defer func() {
		close(done)
		// 异步处理「通道结束」：绝不能在当前 goroutine 里抢 r.mu ——
		// Stop 会持锁等 done，同步抢锁就是死锁。
		go r.handleChannelEnd(rn, lostErr)
	}()

	// 通道的被动错误（设备失效 / 应用退出）会先于 ReadPCM 的 EOF 到达。
	go func() {
		errCh := ch.Err()
		if errCh == nil {
			return
		}
		select {
		case err, ok := <-errCh:
			if ok && err != nil {
				rn.setLostErr(err)
				_ = ch.Close() // 让阻塞中的 ReadPCM 返回
			}
		case <-done:
		}
	}()

	buf := make([]float32, r.opts.ReadBufferSamples)
	for {
		if rn.isStopping() {
			lostErr = nil
			return
		}
		n, err := ch.ReadPCM(buf)
		if n > 0 && !rn.isPaused() {
			// 暂停：丢弃样本（不推进时间轴），但保持通道打开以便无缝继续。
			rn.pipeline.Push(buf[:n])
			rn.notify()
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				lostErr = err
			}
			if lost := rn.lost(); lost != nil {
				lostErr = lost
			}
			return
		}
	}
}

// writeLoop 落盘循环：环形缓冲 → WAV + 指标。
func (r *Recorder) writeLoop(rn *run) {
	defer func() {
		// 无论正常还是出错退出都 Seal：绝不留一个 data 长度为 0 的 WAV。
		if err := rn.writer.Seal(); err != nil {
			logx.For("recording").Warn("落盘退出时封口失败", "session", rn.id, "err", err.Error())
		}
		close(rn.writerDone)
	}()

	buf := make([]float32, ReportWindowSamples)
	for {
		n := rn.pipeline.Read(buf)
		if n > 0 {
			// 旁路取「即将写进文件的这一批样本」：atFrame = 已落盘帧数，
			// 因此 sink 侧时间轴与 WAV 严格一致（暂停期间不推，时间轴自然对齐）。
			atFrame := rn.writer.Frames()
			if err := rn.writer.Write(buf[:n]); err != nil {
				rn.setWriteErr(err)
				// 写不进去就别再采集：继续读只会让环形缓冲一直丢最旧，白耗 CPU。
				rn.closeChannel()
				rn.closeRing()
				go r.handleWriteError(rn, err)
				return
			}
			report, noAudio := rn.monitor.Feed(buf[:n], rn.writer.SizeBytes())
			// 落盘成功之后才推给旁路：推理侧永远拿不到「没进文件」的音频。
			// sink 是有界队列、非阻塞（见 LiveSink 契约），这里绝不等待推理。
			if sink, ok := rn.sink.(MemorySink); rn.noAudioPersist && ok && rn.pipeline.Dropped() != 0 {
				sink.Abort()
			} else if rn.sink != nil {
				rn.sink.Push(buf[:n], atFrame)
			}
			if noAudio {
				r.notifyNoAudio(rn)
			}
			if report {
				r.notifyMetrics(rn)
			}
			continue
		}
		select {
		case <-rn.ringClosed:
			if rn.pipeline.Buffered() == 0 {
				return
			}
		case <-rn.notifyCh:
		}
	}
}

// handleChannelEnd 处理采集通道结束（设备失效 / 源退出）：
// 按 DOC-02 §6.2「recording → paused（保留会话，提示插入设备）」降级，
// 而不是直接结束会话 —— 设备抖动不该丢掉已经录下的音频。
func (r *Recorder) handleChannelEnd(rn *run, cause error) {
	if rn.isStopping() || cause == nil {
		return
	}
	r.mu.Lock()
	if r.run != rn || rn.finished {
		r.mu.Unlock()
		return
	}
	cur := r.sm.Current()
	if cur != contract.StateRecording {
		r.mu.Unlock()
		return
	}
	if err := r.sm.To(contract.StatePaused); err != nil {
		r.mu.Unlock()
		return
	}
	rn.setPaused(true)
	causeErr := captureLostError(cause)
	r.lastErr = causeErr
	atMs := rn.writer.DurationMs()
	st := r.stateLocked()
	r.mu.Unlock()

	r.emitState(st)
	r.appendMarker(rn, atMs, contract.MarkerSourceLost, "采集中断，已暂停（可继续或停止）")
	logx.For("recording").Warn("采集通道中断，会话转为暂停",
		"session", rn.id, "atMs", atMs, "err", cause.Error())
}

// handleWriteError 处理落盘失败（磁盘满等）：ST-03/ST-04 → ST-09。
//
// 策略说明（DEV-ISSUES-S2 S2-06）：磁盘满时停采集并标 failed，保留已写入的
// 音频（Seal 后仍可回放）。不选「无限重试」是因为磁盘满是用户侧动作才能解的
// 阻塞，无限重试只会让会话一直占着设备且日志刷屏。
func (r *Recorder) handleWriteError(rn *run, cause error) {
	r.mu.Lock()
	if r.run != rn || rn.finished {
		r.mu.Unlock()
		return
	}
	apiErr := apierr.From(cause, apierr.CodeDiskFull)
	r.lastErr = apiErr
	st, _ := r.finalizeLocked(rn, contract.StateFailed, apiErr)
	r.mu.Unlock()

	r.emitState(st)
	logx.For("recording").Error("落盘失败，录制停止",
		"session", rn.id, "code", apiErr.Code, "durationMs", st.ElapsedMs)
}

// waitDone 等待某个 goroutine 结束，超时只告警不阻塞用户（收尾不能卡死）。
func (r *Recorder) waitDone(done <-chan struct{}, timeout time.Duration, sessionID, what string) bool {
	if done == nil {
		return true
	}
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		logx.For("recording").Warn("等待"+what+"超时", "session", sessionID, "timeout", timeout.String())
		return false
	}
}

// stateLocked 构造状态快照（r.mu 已持有）。
func (r *Recorder) stateLocked() contract.RecordingState {
	state := StateToContract(r.sm.Current())
	st := contract.RecordingState{State: state}
	if rn := r.run; rn != nil {
		st.SessionID = rn.id
		st.AudioFormat = rn.audioFormat
		st.NoAudioPersist = rn.noAudioPersist
		st.SourceID = rn.sourceID
		st.ModelSource = rn.modelSource
		st.StartedAt = rn.startedAt.Format(time.RFC3339Nano)
		st.ElapsedMs = rn.writer.DurationMs()
	}
	if r.lastErr != nil {
		st.LastError = &contract.ErrorDTO{
			Code:      r.lastErr.Code,
			Message:   r.lastErr.Message,
			Action:    r.lastErr.Action,
			Detail:    r.lastErr.Detail,
			Retryable: r.lastErr.Retryable,
		}
	}
	// degraded / throttled / suspended 由引擎域与预算域决定，但**状态快照只有这一份**：
	// 叠加在这里才不会出现「事件里有、GetState 里没有」的不一致（见 SetBudgetOverlay）。
	st.ThrottleLevel = r.budgetLevel
	st.Throttled = r.budgetThrottled
	st.Suspended = r.budgetSuspended
	return st
}

// appendMarker 写一条时间轴标记（TBL-10）。失败只记日志：标记是附加信息。
func (r *Recorder) appendMarker(rn *run, atMs int64, kind, note string) {
	if err := r.writeMarker(rn, atMs, kind, note); err != nil {
		logx.For("recording").Warn("写入时间轴标记失败", "session", rn.id, "kind", kind,
			"code", apierr.From(err, apierr.CodeDBWrite).Code)
	}
}

// writeMarker 是 appendMarker 的返回错误版本（MarkPerf 要把失败交给调用方）。
func (r *Recorder) writeMarker(rn *run, atMs int64, kind, note string) error {
	if rn == nil || rn.id == "" {
		return nil
	}
	ev := &storage.SessionEvent{
		SessionID: rn.id,
		AtMs:      atMs,
		Kind:      kind,
		Note:      truncateNote(note),
	}
	if err := r.opts.Store.AppendEvent(context.Background(), ev); err != nil {
		return apierr.From(err, apierr.CodeDBWrite)
	}
	return nil
}

// perfMarkerKinds 是允许写入时间轴的预算标记集合（TBL-10 / DOC-04）。
//
// 白名单而不是「任意字符串」：`session_events.kind` 是前端回放与 i18n 的取值域，
// 多一个拼错的 kind 就会变成回放里一个没人认识的空白点，且不会有任何报错。
var perfMarkerKinds = map[string]struct{}{
	contract.MarkerPerfThrottle: {},
	contract.MarkerPerfSuspend:  {},
	contract.MarkerPerfResume:   {},
}

// MarkPerf 在活跃会话的时间轴上写一条**预算标记**（NFR-08，TBL-10）。
//
// kind 只接受 contract.MarkerPerfThrottle / MarkerPerfSuspend / MarkerPerfResume，
// 其它值返回 ERR_INVALID_ARGUMENT（见 perfMarkerKinds 的理由）。
//
// atMs 取当前会话**已录时长**（`frameCount / 16000 × 1000`，由 writer.DurationMs 换算），
// 与 Pause/Resume 等既有标记同一时间基。**不要**改用墙钟：暂停期间时间轴不推进，
// 墙钟会让标记漂到暂停时段里去。
//
// 不变量：
//   - 无活跃会话（未录制、已封存）→ 返回 nil 且**不写库、不报错**。
//     预算变化与是否在录无关（空闲时档位照样在变），调用方不该为此写 if。
//   - 只写标记，**不碰**音频、状态机与 `sessions` 行。暂停推理绝不影响录制（NFR-08 铁律）。
//
// 与 SetBudgetOverlay 的分工（都由装配层调用，本包不自己监听档位）：
//   - SetBudgetOverlay 负责**状态快照**（GetState / EV-01 里能看到的 throttled/suspended）；
//   - MarkPerf 负责**时间轴留痕**（回放里能看到的「第 42 秒降到 L2」）。
//
// 两者不合并：快照要「当前值」，标记要「变化点」，语义与生命周期都不同。
func (r *Recorder) MarkPerf(kind, note string) error {
	if _, ok := perfMarkerKinds[kind]; !ok {
		return apierr.Newf(apierr.CodeInvalidArgument, "未知的预算标记类型：%q", kind)
	}

	r.mu.Lock()
	rn := r.run
	active := rn != nil && !rn.finished
	var atMs int64
	if active {
		atMs = rn.writer.DurationMs()
	}
	r.mu.Unlock()

	if !active {
		// 未在录制：静默跳过（预算档位与录制无关，不该把「没在录」当错误）。
		return nil
	}
	if err := r.writeMarker(rn, atMs, kind, note); err != nil {
		logx.For("recording").Warn("写入预算标记失败",
			"session", rn.id, "kind", kind, "code", apierr.From(err, apierr.CodeDBWrite).Code)
		return err
	}
	logx.For("recording").Info("预算标记已写入时间轴",
		"session", rn.id, "kind", kind, "atMs", atMs)
	return nil
}

// SetBudgetOverlay 由装配层在预算档位变化时调用（NFR-08）。
//
// 为什么叠加在录制域而不是让前端/薄壳自己拼：`RecordingState` 的唯一构造点是
// 本包的 `stateLocked`，若在别处拼装，会出现「EV-01 事件里没有标记、GetState 里有」
// 的不一致。这里只在**变化时**补发一次状态，不打断录制（FR/NFR 铁律：预算只让推理让步）。
func (r *Recorder) SetBudgetOverlay(level string, throttled, suspended bool) {
	r.mu.Lock()
	changed := r.budgetLevel != level || r.budgetThrottled != throttled || r.budgetSuspended != suspended
	r.budgetLevel, r.budgetThrottled, r.budgetSuspended = level, throttled, suspended
	var st contract.RecordingState
	if changed {
		st = r.stateLocked()
	}
	r.mu.Unlock()
	if changed {
		r.emitState(st)
	}
}

func (r *Recorder) emitState(st contract.RecordingState) {
	r.hMu.RLock()
	fn := r.h.OnState
	r.hMu.RUnlock()
	if fn != nil {
		fn(st)
	}
}

func (r *Recorder) notifyMetrics(rn *run) {
	r.hMu.RLock()
	fn := r.h.OnMetrics
	r.hMu.RUnlock()
	if fn == nil {
		return
	}
	m := rn.monitor.Metrics()
	// 用 writer 的实际字节数（monitor 里存的是写入时刻的值，可能落后一拍）。
	m.BytesWritten = rn.writer.SizeBytes()
	// 溢出只在首次发生时告警一次：环形缓冲持续溢出时每 250ms 打一条会把日志刷满，
	// 真正有用的是「有没有溢出 + 总量」，最后一次封存日志会带 dropped 总数。
	if d := rn.pipeline.Dropped(); d > 0 && !rn.dropsLogged {
		rn.dropsLogged = true
		logx.For("recording").Warn("环形缓冲溢出，开始丢弃最旧音频",
			"session", rn.id, "dropped", d)
	}
	fn(m)
}

func (r *Recorder) notifyNoAudio(rn *run) {
	r.hMu.RLock()
	fn := r.h.OnNoAudio
	r.hMu.RUnlock()
	if fn == nil {
		return
	}
	// 只上报元数据（静音时长/峰值），不含任何音频内容。
	logx.For("recording").Warn("未检测到音频", "session", rn.id,
		"silentMs", rn.monitor.SilentMs(), "peak", rn.monitor.MaxPeak())
	fn(rn.monitor.Snapshot(rn.writer.SizeBytes()))
}

// defaultResolveSource 按源 ID 前缀推导展示名与类型。
//
// 刻意**不探测**设备存在性：探测属于 capturesvc 的语义（会在 UI 给出可用性），
// 而这里只需要「写进 sessions 的展示名」；真正的可用性由 OpenChannel 的
// 失败来回答（那时返回 ERR_CAPTURE_UNAVAILABLE，UI 直接引导用户换源）。
// 装配层想要设备友好名时用 ResolveSourceWith 包一层（见下）。
func defaultResolveSource(sourceID string) (name, kind string, err error) {
	kind, value, err := capture.ParseSourceID(sourceID)
	if err != nil {
		return "", "", err
	}
	switch kind {
	case contract.SourceKindApplication:
		return value, kind, nil
	case contract.SourceKindDevice:
		return deviceFallbackName(value), kind, nil
	default:
		return value, kind, nil
	}
}

// deviceFallbackName 在拿不到设备友好名时，从端点 ID 截一段可读的短标识。
//
// Windows 端点 ID 形如 `{0.0.0.00000000}.{12931ccb-…-f49efe5854bb}`：
// 直接截末 8 个字符会带上右花括号（G-E2E-02：标题变成「音频设备 e5854bb}」），
// 所以先去掉 `{}` 包裹再取末段。
func deviceFallbackName(deviceID string) string {
	if len(deviceID) <= 16 {
		return deviceID
	}
	clean := strings.Trim(deviceID, "{}")
	if i := strings.LastIndexAny(clean, "{}.-"); i >= 0 && i < len(clean)-1 {
		clean = clean[i+1:]
	}
	if len(clean) > 8 {
		clean = clean[len(clean)-8:]
	}
	return "音频设备 " + clean
}

// ResolveSourceWith 返回一个 ResolveSource：设备源优先用 lookup 给出的友好名
// （如「扬声器 (HECATE G2000)」，与录制页列表里用户看到的一致），
// lookup 失败或给空名时回落到 defaultResolveSource。
//
// lookup 只用于取**展示名**：它失败不会阻止开录，可用性仍由 OpenChannel 回答。
// 装配层传 capture.Probe；单测用默认实现，不碰真实音频设备。
func ResolveSourceWith(lookup func(sourceID string) (contract.Source, error)) func(string) (string, string, error) {
	return func(sourceID string) (string, string, error) {
		name, kind, err := defaultResolveSource(sourceID)
		if err != nil || kind != contract.SourceKindDevice || lookup == nil {
			return name, kind, err
		}
		if src, lerr := lookup(sourceID); lerr == nil {
			if display := strings.TrimSpace(src.DisplayName); display != "" {
				return display, kind, nil
			}
		}
		return name, kind, nil
	}
}

// openChannelError 把捕获层错误映射成 API-2.1 的错误码。
func openChannelError(err error) *apierr.Err {
	e := apierr.From(err, apierr.CodeCaptureUnavailable)
	switch e.Code {
	case apierr.CodeCaptureUnavailable, apierr.CodeCapturePermission, apierr.CodeSourceNotFound:
		return e
	}
	return apierr.New(apierr.CodeCaptureUnavailable).WithCause(err).
		WithDetail("打开音频源失败：%v", err)
}

// captureLostError 把通道被动错误映射成「设备丢失」类错误。
func captureLostError(err error) *apierr.Err {
	if err == nil {
		return nil
	}
	return apierr.New(apierr.CodeCaptureDeviceLost).WithCause(err).
		WithDetail("采集通道中断：%v", err)
}

// defaultTitle 生成会话默认标题：`YYYY-MM-DD HH:mm · 源名`（DATA-MODEL TBL-03）。
func defaultTitle(now time.Time, sourceName string) string {
	name := strings.TrimSpace(sourceName)
	if name == "" {
		name = "未知来源"
	}
	return fmt.Sprintf("%s · %s", now.Format("2006-01-02 15:04"), name)
}

// truncateNote 把备注裁到 sessions.degrade_reason / session_events.note 的长度上限。
func truncateNote(s string) string {
	const maxLen = 255
	if len(s) <= maxLen {
		return s
	}
	// 按 rune 截断，避免把多字节中文切成半个字符。
	runes := []rune(s)
	if len(runes) <= 128 {
		return s
	}
	return string(runes[:128])
}

// run 是一场录制的运行时（采集 epoch + 落盘 goroutine + 计数）。
type audioWriter interface {
	Write([]float32) error
	Seal() error
	Close() error
	Frames() int64
	DurationMs() int64
	SizeBytes() int64
}
type run struct {
	id             string
	title          string
	sourceID       string
	sourceName     string
	sourceKind     string
	modelSource    string
	noAudioPersist bool
	audioPath      string // 相对 dataRoot（正斜杠）
	audioFormat    string
	absPath        string

	startedAt time.Time

	pipeline *Pipeline
	writer   audioWriter
	monitor  *Monitor

	// sink 是边录边转的音频旁路（FR-03-10）；nil = 未开启（默认）。
	//
	// 只在 Start 里赋值一次（goroutine 启动前），之后只读，因此不需要加锁。
	sink LiveSink

	mu         sync.Mutex
	ch         capture.Channel
	paused     bool
	stopping   bool
	lostErr    error
	writeErr   error
	readerDone chan struct{}

	notifyCh     chan struct{}
	ringClosed   chan struct{}
	ringCloseOne sync.Once
	writerDone   chan struct{}

	// finished 由 Recorder 的锁保护，标记已走完收尾。
	finished bool
	// shutdownAbort 由应用退出设置，阻止内存直传在 Stop 后继续排空。
	shutdownAbort bool
	// dropsLogged 环形缓冲溢出只告警一次（避免刷屏）。
	dropsLogged bool
}

func (rn *run) channel() capture.Channel {
	rn.mu.Lock()
	defer rn.mu.Unlock()
	return rn.ch
}

func (rn *run) setChannel(ch capture.Channel) {
	rn.mu.Lock()
	old := rn.ch
	rn.ch = ch
	rn.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
}

func (rn *run) channelDead() bool {
	rn.mu.Lock()
	ch := rn.ch
	rn.mu.Unlock()
	if ch == nil {
		return true
	}
	// 已收到被动错误或读循环已退出 → 通道不可用。
	if rn.lostErr != nil {
		return true
	}
	select {
	case <-rn.readerDoneCh():
		return true
	default:
		return false
	}
}

func (rn *run) closeChannel() {
	rn.mu.Lock()
	ch := rn.ch
	rn.mu.Unlock()
	if ch != nil {
		_ = ch.Close()
	}
}

func (rn *run) readerDoneCh() chan struct{} {
	rn.mu.Lock()
	defer rn.mu.Unlock()
	return rn.readerDone
}

func (rn *run) setReaderDone(done chan struct{}) {
	rn.mu.Lock()
	rn.readerDone = done
	rn.lostErr = nil
	rn.mu.Unlock()
}

func (rn *run) setPaused(v bool) {
	rn.mu.Lock()
	rn.paused = v
	rn.mu.Unlock()
}

func (rn *run) isPaused() bool {
	rn.mu.Lock()
	defer rn.mu.Unlock()
	return rn.paused
}

func (rn *run) setStopping(v bool) {
	rn.mu.Lock()
	rn.stopping = v
	rn.mu.Unlock()
}

func (rn *run) isStopping() bool {
	rn.mu.Lock()
	defer rn.mu.Unlock()
	return rn.stopping
}

func (rn *run) setShutdownAbort(v bool) {
	rn.mu.Lock()
	rn.shutdownAbort = v
	rn.mu.Unlock()
}

func (rn *run) isShutdownAbort() bool {
	rn.mu.Lock()
	defer rn.mu.Unlock()
	return rn.shutdownAbort
}

func (rn *run) setLostErr(err error) {
	rn.mu.Lock()
	if rn.lostErr == nil {
		rn.lostErr = err
	}
	rn.mu.Unlock()
}

func (rn *run) lost() error {
	rn.mu.Lock()
	defer rn.mu.Unlock()
	return rn.lostErr
}

func (rn *run) setWriteErr(err error) {
	rn.mu.Lock()
	if rn.writeErr == nil {
		rn.writeErr = err
	}
	rn.mu.Unlock()
}

func (rn *run) notify() {
	select {
	case rn.notifyCh <- struct{}{}:
	default:
	}
}

func (rn *run) closeRing() {
	rn.ringCloseOne.Do(func() { close(rn.ringClosed) })
}

// absAudioPath 返回会话音频的绝对路径。
func (rn *run) absAudioPath() string { return rn.absPath }
