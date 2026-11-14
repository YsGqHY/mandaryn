// Package app 封装 Wails 应用的启动逻辑。
// main.go 只保留 embed.FS 与 Run 调用，业务装配集中在这里。
package app

import (
	"context"
	"embed"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"vip.oberon.mandaryn/internal/app/audioroute"
	"vip.oberon.mandaryn/internal/asr"
	asrlocal "vip.oberon.mandaryn/internal/asr/local"
	"vip.oberon.mandaryn/internal/capture"
	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/events"
	"vip.oberon.mandaryn/internal/hotkeys"
	"vip.oberon.mandaryn/internal/outbound"
	"vip.oberon.mandaryn/internal/paths"
	"vip.oberon.mandaryn/internal/perf"
	"vip.oberon.mandaryn/internal/privacy"
	"vip.oberon.mandaryn/internal/recording"
	"vip.oberon.mandaryn/internal/services/appsettings"
	"vip.oberon.mandaryn/internal/services/capturesvc"
	"vip.oberon.mandaryn/internal/services/enginesvc"
	"vip.oberon.mandaryn/internal/services/exportsvc"
	"vip.oberon.mandaryn/internal/services/hotkeysvc"
	"vip.oberon.mandaryn/internal/services/modelssvc"
	"vip.oberon.mandaryn/internal/services/pathsvc"
	"vip.oberon.mandaryn/internal/services/preferences"
	"vip.oberon.mandaryn/internal/services/privacysvc"
	"vip.oberon.mandaryn/internal/services/recordingsvc"
	"vip.oberon.mandaryn/internal/services/sessionsvc"
	"vip.oberon.mandaryn/internal/services/speakersvc"
	"vip.oberon.mandaryn/internal/services/storagesvc"
	"vip.oberon.mandaryn/internal/services/subprocess"
	"vip.oberon.mandaryn/internal/services/transcriptsvc"
	"vip.oberon.mandaryn/internal/speaker/sherpa"
	"vip.oberon.mandaryn/internal/storage"
	"vip.oberon.mandaryn/internal/transcribe"
	"vip.oberon.mandaryn/internal/utils/cryptox"
	"vip.oberon.mandaryn/internal/utils/httpx"
	"vip.oberon.mandaryn/internal/utils/logx"

	"vip.oberon.mandaryn/internal/apierr"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// 应用标识（ARCH §10.1：与 Taskfile / build/config.yml / 各平台窗口标题一致）。
const (
	AppName        = "Mandaryn"
	AppDescription = "按应用分路录制并转写中文语音，默认不联网"
)

// Run 创建 Wails 应用、注册服务、打开主窗口并启动事件循环。
// assets 由调用方（main.go）通过 //go:embed 注入。
func Run(assets embed.FS, trayIcon []byte) error {
	// —— 0) 路径解析（必须在 cryptox.Init 与 storage.Open 之前完成）——
	//
	// ADR-02：dataRoot / modelRoot 由单一解析器裁决，禁止各模块自己读环境变量。
	// 解析顺序：命令行 > 环境变量 > 用户设置 > 默认。
	cliPaths := parsePathFlags(os.Args)

	cfgMgr, err := storage.LoadConfig()
	if err != nil {
		return fmt.Errorf("load storage config: %w", err)
	}

	// holder 在数据库打开后才赋值；modelRoot 的 user/pending 层存放在 DB 里，
	// 因此这些闭包只能在此之后被调用（Resolve(dataRoot) 不需要它们）。
	var holder *storage.Holder

	resolver := paths.New(paths.Options{
		CLI: cliPaths,
		UserValue: func(key paths.Key) (string, error) {
			switch key {
			case paths.KeyDataRoot:
				// dataRoot 的用户层落在 storage.json（鸡生蛋：不能存进它自己配置的库）。
				return cfgMgr.Get().DataPath, nil
			case paths.KeyModelRoot:
				if holder == nil {
					return "", nil
				}
				var row storage.AppSettings
				if err := holder.Current().GORM.First(&row, "id = ?", 1).Error; err != nil {
					return "", nil
				}
				return row.ModelRoot, nil
			}
			return "", nil
		},
		PendingValue: func(key paths.Key) (string, error) {
			if key != paths.KeyModelRoot || holder == nil {
				return "", nil
			}
			var row storage.AppSettings
			if err := holder.Current().GORM.First(&row, "id = ?", 1).Error; err != nil {
				return "", nil
			}
			return row.ModelRootPending, nil
		},
		SetUserValue: func(key paths.Key, v string) error {
			switch key {
			case paths.KeyDataRoot:
				return cfgMgr.SetDataPath(v)
			case paths.KeyModelRoot:
				if holder == nil {
					return fmt.Errorf("model root store not ready")
				}
				return holder.Current().GORM.Model(&storage.AppSettings{}).
					Where("id = ?", 1).Update("model_root", v).Error
			}
			return fmt.Errorf("unsupported path key: %s", key)
		},
		SetPendingValue: func(key paths.Key, v string) error {
			if key != paths.KeyModelRoot || holder == nil {
				return fmt.Errorf("pending store not ready")
			}
			return holder.Current().GORM.Model(&storage.AppSettings{}).
				Where("id = ?", 1).Update("model_root_pending", v).Error
		},
	})

	dataRoot, err := resolver.Effective(paths.KeyDataRoot)
	if err != nil {
		return fmt.Errorf("resolve data root: %w", err)
	}

	// —— 1) 工具层就绪：日志 → HTTP UA → 后续模块都能直接用 ——
	// 日志目录显式取自解析结果，保证「库 / 密钥 / 日志」必然同根。
	if err := logx.Init(logx.Config{
		Level:          slog.LevelInfo,
		Dir:            joinPath(dataRoot, paths.LogsDirName),
		ConsoleEnabled: true,
	}); err != nil {
		// 日志初始化失败不应阻断启动；降级到默认 stderr logger（logx.For 内部已兜底）
		fmt.Println("warn: logx init failed:", err)
	}
	// 调试模式（设置 → 隐私边界）：脱敏器必须先于开关就位（logx 未注入时拒绝开启）。
	// 环境变量 MANDARYN_LOG_LEVEL=debug 在这里就生效，能记下启动阶段；
	// 设置页的用户层要等数据库打开后才读得到（见下方 storage.Open 之后）。
	debugMode := privacy.NewDebugMode(nil)
	logx.SetDebugSanitizer(privacy.SanitizeDebugAttr)
	if bad := debugMode.InvalidEnv(); bad != "" {
		logx.For("app").Warn("忽略无法识别的日志级别环境变量", "env", privacy.EnvLogLevel, "value", bad)
	}
	if err := debugMode.Apply(); err != nil {
		logx.For("app").Warn("调试日志无法开启", "err", err.Error())
	}
	// 主密钥与数据库同根：cryptox 不再自己读环境变量（否则会出现「密钥在旧目录」）。
	if err := cryptox.SetBaseDir(dataRoot); err != nil {
		logx.For("app").Warn("cryptox base dir rejected", "err", err.Error())
	}
	httpx.SetUserAgent(AppName + "/0.1 (+wails-v3)")

	events.Register()

	// —— 2) 持久化层 ——
	dbPath := storage.DBPathIn(dataRoot)
	db, err := storage.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	holder = storage.NewHolder(db)
	logx.For("app").Info("storage opened", "path", dbPath)

	// 调试模式的用户层：app_settings.log_level（debug = 开）。读到即生效，无需重启。
	var logLevelRow storage.AppSettings
	if err := holder.Current().GORM.Select("log_level").First(&logLevelRow, "id = ?", 1).Error; err != nil {
		logx.For("app").Warn("读取调试模式设置失败，按关闭处理", "err", err.Error())
	} else if st, err := debugMode.SetUserLevel(logLevelRow.LogLevel); err != nil {
		logx.For("app").Warn("调试日志无法开启", "err", err.Error())
	} else if st.Enabled {
		logx.For("app").Info("调试模式已开启", "source", st.Source, "dir", logx.DebugLogDir())
	}

	// 暂存路径：解除接管后自动应用（FR-07-8）。必须在 DB 打开后执行。
	if applied, err := resolver.ApplyPending(); err != nil {
		logx.For("app").Warn("apply pending paths failed", "err", err.Error())
	} else if len(applied) > 0 {
		logx.For("app").Info("pending path settings applied", "keys", strings.Join(applied, ","))
	}

	// —— 3) 出站留痕：清理 24h 前记录 ——
	outRecorder := outbound.NewRecorder(holder)
	if err := outRecorder.Purge(); err != nil {
		logx.For("app").Warn("purge outbound log failed", "err", err.Error())
	}

	// —— 4) 服务装配 ——
	// subprocess service 需要 *application.App 才能向前端 Emit 事件。
	// 但 application.New 又需要 services 列表 —— 用预创建 + Attach 模式。
	subSvc := subprocess.New(nil) // app 暂为 nil
	childWinSvc := NewChildWindowService()
	// 快捷键薄壳同样是「先占位、后挂载」：领域对象要 app 才能注册热键。
	hkShell := hotkeysvc.New()
	prefsSvc := preferences.New(holder)
	systraySvc := NewSystrayService(prefsSvc, trayIcon)

	// 数据目录切换后让解析器缓存失效，避免 UI 继续显示旧生效路径。
	storageSvc := storagesvc.New(holder, cfgMgr, resolver.Invalidate)

	// —— Wave D：整机性能预算（NFR-08）——
	//
	// 口径是「整机份额 N」而非「本应用上限」：headRoom = N − otherPct。
	// 构造顺序看着别扭但不成环：设置域要注入 perf 的读数来源，perf 又要读设置的 N ——
	// 两边都只是闭包，构造期不互相调用。
	var (
		settingsSvc *appsettings.Service
		recorder    *recording.Recorder
		// prevBudgetLevel 上一次档位：OnBudget 只在变化时触发，用它区分「降档」与「恢复」。
		prevBudgetLevel perf.Level
	)
	perfSvc := perf.New(perf.Options{
		// N 每拍读库（改设置立即生效），不在启动时快照。
		Cap: func() int {
			if settingsSvc == nil {
				return 80
			}
			snap, err := settingsSvc.Get(context.Background())
			if err != nil {
				// 读不到就退回默认：预算限流是软约束，不能因读库失败把档位判成 P。
				return 80
			}
			return snap.ResourceCapPct
		},
		OnUsage: func(u contract.PerfUsage) {
			if app := application.Get(); app != nil {
				app.Event.Emit(events.PerfUsage, u)
			}
		},
		OnBudget: func(b perf.BudgetState) {
			// 预算**只让推理让步**：叠加到录制状态快照后补发一次 EV-01，
			// 绝不停录制、不丢已采音频（NFR-08 铁律）。
			if recorder != nil {
				recorder.SetBudgetOverlay(string(b.Level), b.Throttled, b.Suspended)
				// 档位变化写进会话时间轴（TBL-10），让回放时能解释「当时为什么慢」。
				if kind := budgetMarkerKind(prevBudgetLevel, b); kind != "" {
					if err := recorder.MarkPerf(kind, budgetMarkerNote(b)); err != nil {
						logx.For("app").Warn("写入预算标记失败", "kind", kind, "err", err.Error())
					}
				}
			}
			prevBudgetLevel = b.Level
			logx.For("app").Info("预算档位变化", "level", b.Level, "reason", b.Reason,
				"suspended", b.Suspended, "capPct", b.CapPct,
				"totalCpuPct", b.TotalCpuPct, "selfCpuPct", b.SelfCpuPct, "otherPct", b.OtherPct)
		},
	})
	perfCtx, stopPerf := context.WithCancel(context.Background())
	defer stopPerf()
	go perfSvc.Start(perfCtx)

	// —— Wave B：捕获 / 引擎 / 模型仓库 ——
	// API-9.9 返回真实三分量读数（与 EV-15 同一口径，设置域不另算 CPU）。
	//
	// 晚绑定 activeSessionFn：settingsSvc 需要「是否有活跃录制会话」来拒绝录制中切换设置，
	// 而 recSvc 的构造依赖 recorder 依赖 transSvc……顺序上只能先留空、由下面 recSvc 就绪后回填。
	// 不回填时判定为「无活跃会话」（等价于装配未完成，设置域各开关仍是可改的，不会卡死）。
	var activeSessionFn func() bool
	settingsSvc = appsettings.New(holder, outRecorder, perfSvc.Usage,
		appsettings.WithActiveSession(func() bool {
			return activeSessionFn != nil && activeSessionFn()
		}),
		// 设置页切换调试模式 → 立即落实到 logx（环境变量接管时以环境变量为准）。
		appsettings.WithDebugMode(debugMode.SetUser))

	// 源可用性变化 → EV-02 capture:sources。L3 只转事件，轮询与差异比较在 L2。
	// 载荷必须是注册类型 CaptureSourcesChanged（events.Register）：直接发 []Source 会被
	// Wails 判为类型不符而取消，日志刷「does not match registered data type」，前端永远收不到。
	captureSvc := capturesvc.New(holder, func(srcs []contract.Source) {
		if app := application.Get(); app != nil {
			app.Event.Emit(events.CaptureSources, contract.CaptureSourcesChanged{Sources: srcs})
		}
	})

	// 模型仓库：清单 / 下载 / 校验 / 迁移。启动时清理残留（.staging、卡住的断点）。
	modelsSvc := modelssvc.New(holder, resolver, outRecorder)
	modelsSvc.Cleanup(context.Background())

	// —— Wave D：隐私边界（FR-08）——
	// Gate 为 nil 时由 privacysvc 自建（环境变量 MANDARYN_OFFLINE + preferences 用户开关），
	// 并在构造期 InstallGlobal：既有出站调用方（asr / models / enginesvc）无需改动即遵守开关。
	privSvc, err := privacysvc.New(privacysvc.Deps{
		Holder: holder,
		// 诊断包只带元数据：版本 + 模块清单；音频 / 正文 / 声纹 / 凭据一概不进。
		Version: "0.1",
		Modules: func() []string {
			return []string{
				"capture", "recording", "transcribe", "sessions", "asr", "models",
				"speaker", "perf", "privacy", "hotkeys",
			}
		},
		// 诊断包附带 app.log 与调试日志的尾部（进包前统一脱敏）。此前没有接线，诊断包里没有日志。
		LogDir: func() string { return joinPath(dataRoot, paths.LogsDirName) },
		Debug:  debugMode,
	})
	if err != nil {
		return fmt.Errorf("wire privacysvc: %w", err)
	}

	// —— Wave C：录制（先建 Recorder，它决定活跃会话判定）——
	//
	// 边录边转（FR-03-10，默认关）的两个注入点都必须**晚绑定**：
	//   - LiveEnabled 读设置快照（settingsSvc 已就绪，可直接闭包）；
	//   - NewLiveSink 需要 transcriptsvc（它反过来依赖 recSvc 的 activeSession），
	//     所以这里只留一个间接调用，等 transSvc 构造完成后再回填 newLiveSink。
	// 任一为 nil 即「不旁路」，与未实现该功能时的行为逐字一致。
	var newLiveSink func(sessionID string) recording.LiveSink
	recorder, err = recording.New(recording.Options{
		Store:    recording.NewGORMStore(holder),
		DataRoot: func() (string, error) { return dataRoot, nil },
		// 自动命名用设备友好名（与录制页列表一致），探测失败回落到短 ID（G-E2E-02）。
		ResolveSource: recording.ResolveSourceWith(capture.Probe),
		LiveEnabled: func() bool {
			snap, gerr := settingsSvc.Get(context.Background())
			if gerr != nil {
				return false // 读不到设置就当作关闭：宁可不出字，也不要意外占 CPU
			}
			return snap.LiveTranscribe
		},
		NewLiveSink: func(sessionID string) recording.LiveSink {
			if newLiveSink == nil {
				return nil // 装配尚未完成：本会话不旁路
			}
			return newLiveSink(sessionID)
		},
	})
	if err != nil {
		return fmt.Errorf("wire recorder: %w", err)
	}
	recorder.SetHandlers(recording.Handlers{
		OnState: func(st contract.RecordingState) {
			if app := application.Get(); app != nil {
				app.Event.Emit(events.RecordingState, st)
			}
		},
		OnMetrics: func(m recording.Metrics) {
			// 电平走 EV-02（4Hz 节流在 L2）；时长/字节走 EV-03 tick。
			if app := application.Get(); app != nil {
				app.Event.Emit(events.RecordingLevel, contract.RecordingLevel{
					RMS:      m.LevelRMS,
					Peak:     m.LevelPeak,
					SilentMs: m.ZeroInputMs,
				})
			}
		},
		OnNoAudio: func(recording.Metrics) {
			// 「未检测到音频」只提示不改状态（FR-03-7）；前端据 EV-02 的
			// silentMs 渲染提示，这里无需额外事件。
		},
	})
	recSvc := recordingsvc.New(recorder, holder, recordingsvc.Emit{
		State: func(st contract.RecordingState) {
			if app := application.Get(); app != nil {
				app.Event.Emit(events.RecordingState, st)
			}
		},
		Level: func(lv contract.RecordingLevel) {
			if app := application.Get(); app != nil {
				app.Event.Emit(events.RecordingLevel, lv)
			}
		},
		Tick: func(tk contract.RecordingTick) {
			if app := application.Get(); app != nil {
				app.Event.Emit(events.RecordingTick, tk)
			}
		},
	})

	// 崩溃自检：把上次异常退出遗留的会话修补成可用状态。
	//
	// 为什么必须在这里调：`RecoverUnfinished` 与 `RepairWAV` 早已实现并有单测，
	// 但**从来没有调用点** —— 也就是说「崩溃后音频可回放」这条承诺在生产路径上
	// 根本没生效（库里的 status 会永远停在 recording、WAV 头的 data 长度是 0，
	// 能播只是 Chromium 按文件长度兜底，不是头被修好）。见 DEV-ISSUES-G-e2e.md G-E2E-01。
	//
	// 与 modelsSvc.Cleanup 同位置：都在装配完成、窗口打开之前，且失败**不阻断启动**
	// —— 自检失败最多是「旧会话状态不准」，不该让用户开不了应用。
	if results, rerr := recorder.RecoverUnfinished(context.Background()); rerr != nil {
		logx.For("recording").Warn("启动自检：崩溃会话恢复失败", "err", rerr.Error())
	} else {
		for _, r := range results {
			// 只写元数据：会话号、恢复后状态、帧数与时长，不含正文与音频。
			logx.For("recording").Info("启动自检：已恢复中断会话",
				"session", r.SessionID, "status", r.Status,
				"frames", r.Frames, "durationMs", r.DurationMs, "bytes", r.SizeBytes)
		}
	}

	// 活跃会话判定：录制中/暂停中都算「有会话」，引擎与设置域据此拒绝切换。
	activeSession := func() bool {
		st, err := recSvc.GetState(context.Background())
		if err != nil {
			return false
		}
		return st.State == contract.StateRecording || st.State == contract.StatePaused
	}
	// 回填给 settingsSvc（见上方晚绑定说明）：设置域与引擎域共用同一个判定，
	// 避免两处各写一份「什么算录制中」而慢慢分叉。
	activeSessionFn = activeSession

	// 本地引擎按角色向模型仓库取目录；在线工厂由 enginesvc 在构造时注入
	// （端点与密钥都在 appsettings，只有它读得到）。
	//
	// 初始来源取持久化设置：不传时 Router 一律按 local 起，用户选过「在线」的话
	// 重启后录制页显示的来源会与设置不一致，转写也会走错引擎。
	initialSource := contract.ModelSourceLocal
	if snap, serr := settingsSvc.Get(context.Background()); serr == nil && snap.ModelSource != "" {
		initialSource = snap.ModelSource
	}
	asrRouter := asr.NewRouter(asr.RouterOptions{
		Source: initialSource,
		// 线程数按当前预算档位取（NFR-08）：只在**引擎加载时**生效，
		// 改档位后要等下次加载（热切换/重启），不是热生效。
		Local:       asrlocal.NewFactoryWithThreads(modelsSvc.Manager(), perfSvc.Threads),
		LoadThreads: perfSvc.Threads,
		// 流式引擎（边录边转）：与上面的离线引擎**互不参与**降级矩阵，
		// 缺模型包时 `Router.Streaming` 直接返回不可用，不会拿离线引擎冒充。
		Streaming: asrlocal.NewStreamingFactoryWithThreads(modelsSvc.Manager(), perfSvc.Threads),
		// 便宜探测：只查「流式模型包装没装」，不加载模型（加载要 ~1s，不能堵在状态查询上）。
		StreamingProbe: func() error {
			_, _, lerr := modelsSvc.Manager().LocateBundle(asr.RoleStreaming)
			return lerr
		},
		ActiveSession: activeSession,
	})
	engineSvc, err := enginesvc.New(holder, settingsSvc, asrRouter, outRecorder, activeSession)
	if err != nil {
		return fmt.Errorf("wire enginesvc: %w", err)
	}

	// —— Wave C：转写任务与会话库 ——
	transSvc, err := transcriptsvc.New(transcriptsvc.Deps{
		Holder:   holder,
		Engine:   asrRouter,
		Resolver: resolver,
		OnState: func(st contract.TranscriptionState) {
			if app := application.Get(); app != nil {
				app.Event.Emit(events.TranscriptionState, st)
			}
		},
		// 整机空闲判定（NFR-08）：忙时只让「空闲自动」任务让位，
		// 用户手动发起的转写照常跑（FR-03-4）。
		Idle:    perfSvc.Idle,
		IdleFor: perfSvc.IdleFor,
		// 预算耗尽（P 档）连手动任务也拒绝，返回可重试的 ERR_PERF_SUSPENDED。
		Allowed: perfSvc.CheckInferenceAllowed,
		// 边录边转的事件出口（FR-03-10）：partial 不落库、定稿走既有片段事件。
		Live: transcriptsvc.LiveDeps{
			OnPartial: func(seg contract.Segment) {
				if app := application.Get(); app != nil {
					app.Event.Emit(events.TranscriptPartial, seg)
				}
			},
			OnSegment: func(seg contract.Segment) {
				if app := application.Get(); app != nil {
					app.Event.Emit(events.TranscriptSegment, seg)
				}
			},
			OnState: func(st transcribe.LiveState) {
				// 只写元数据：会话号 / 状态 / 错误码 / 句数 / 丢弃时长，不含正文与音频。
				logx.For("transcribe").Info("边录边转状态",
					"session", st.SessionID, "active", st.Active,
					"code", st.ErrorCode, "segments", st.Segments, "droppedMs", st.DroppedMs)
			},
			// 第二遍离线校正（ARCH §2.3）：只在预算允许时做。
			// Refine 为 nil 时 LiveRunner 只保留流式结果（等价 L1 形态），这里先不接第二遍 ——
			// 流式引擎本身尚未落地（见 docs/DEV-ISSUES-F-live.md FL-04），
			// 提前接校正会把「未实现」伪装成「已实现」。
			RefineAllowed: func() bool {
				return perf.RefinementAllowed(perfSvc.State().Level)
			},
		},
	})
	if err != nil {
		return fmt.Errorf("wire transcriptsvc: %w", err)
	}
	// 回填边录边转的旁路工厂（见上方 recording.Options 的晚绑定说明）。
	// 能力缺失时返回 nil：本会话只录音、不出实时字，录制不受影响（FR-03-6）。
	newLiveSink = func(sessionID string) recording.LiveSink {
		sink, serr := transSvc.NewLiveSink(context.Background(), sessionID)
		if serr != nil {
			logx.For("transcribe").Warn("边录边转不可用，本会话仅录音",
				"session", sessionID, "code", apierr.From(serr, apierr.CodeLocalUnavailable).Code)
			return nil
		}
		return sink
	}
	sessionSvc, err := sessionsvc.New(holder, resolver)
	if err != nil {
		return fmt.Errorf("wire sessionsvc: %w", err)
	}

	// —— Wave D：说话人分离与角色命名（FR-06）——
	// 声纹提取器按需构造：定位器每次查模型目录，模型运行期下载完即可用；
	// 本机未装声纹模型时构造失败不影响角色管理（降级为 ERR_MODEL_NOT_FOUND）。
	speakerSvc, err := speakersvc.New(speakersvc.Deps{
		Holder:   holder,
		Resolver: resolver,
		Locator:  modelsSvc.Manager(),
		Factory:  sherpa.NewSherpaExtractor,
		MaxSpeakers: func(ctx context.Context) (int, error) {
			snap, err := settingsSvc.Get(ctx)
			if err != nil {
				return 0, err
			}
			return snap.Audio.MaxSpeakers, nil
		},
		OnRolesChanged: func(ev contract.RoleChanged) {
			if app := application.Get(); app != nil {
				app.Event.Emit(events.RolesChanged, ev)
			}
		},
	})
	if err != nil {
		return fmt.Errorf("wire speakersvc: %w", err)
	}

	app := application.New(application.Options{
		Name:        AppName,
		Description: AppDescription,
		Services: []application.Service{
			application.NewService(prefsSvc),
			// 有 ERR_* 错误码的服务统一挂 MarshalError：
			// 前端 Promise reject 会拿到 {code,message,action,detail,retryable} 结构。
			application.NewServiceWithOptions(settingsSvc, serviceOptions("appsettings")),
			application.NewServiceWithOptions(storageSvc, serviceOptions("storagesvc")),
			application.NewServiceWithOptions(exportsvc.New(), serviceOptions("exportsvc")),
			application.NewServiceWithOptions(pathsvc.New(resolver), serviceOptions("pathsvc")),
			application.NewServiceWithOptions(captureSvc, serviceOptions("capturesvc")),
			application.NewServiceWithOptions(modelsSvc, serviceOptions("modelssvc")),
			application.NewServiceWithOptions(engineSvc, serviceOptions("enginesvc")),
			application.NewServiceWithOptions(recSvc, serviceOptions("recordingsvc")),
			application.NewServiceWithOptions(transSvc, serviceOptions("transcriptsvc")),
			application.NewServiceWithOptions(sessionSvc, serviceOptions("sessionsvc")),
			application.NewServiceWithOptions(speakerSvc, serviceOptions("speakersvc")),
			application.NewServiceWithOptions(privSvc, serviceOptions("privacysvc")),
			application.NewServiceWithOptions(hkShell, serviceOptions("hotkeysvc")),
			application.NewService(subSvc),
			application.NewService(childWinSvc),
			application.NewService(systraySvc),
		},
		Assets: application.AssetOptions{
			// 在 SPA 资源处理器上叠加 /audio/<sessionId> 流式路由（支持 Range，回放可 seek）。
			// 放在 audioroute 子包而不是本包：本包链接 sherpa（CGO），而该路由是纯 HTTP，
			// 下沉后它的测试不依赖 sherpa 运行时 DLL，能直接 `go test ./internal/...`。
			Handler: audioroute.NewHandler(assets, holder, resolver),
		},
		Mac: application.MacOptions{
			ApplicationShouldTerminateAfterLastWindowClosed: true,
		},
		Windows: application.WindowsOptions{
			// WebView2 的数据目录**必须显式指定**，不能依赖 Wails 的兜底。
			//
			// Wails 在未设置时的兜底是 `filepath.Join(os.Getenv("AppData"), <exe 文件名>)`：
			// 一旦 `AppData` 取不到（精简环境、服务账户、CI、从某些终端启动），
			// Join 的左侧为空 → 得到**相对路径** `mandaryn.exe`；若当前工作目录是 bin/，
			// 就会被解析成 `bin\mandaryn.exe` —— 那是 exe 文件本身，WebView2 无法在其中
			// 创建数据目录，弹「无法读取和写入其数据目录 ...\mandaryn.exe\EBWebView」。
			//
			// 固定到 dataRoot 下的 webview/：既消除对环境变量的隐式依赖，
			// 也符合 ADR-02「所有落盘路径经 Resolver 取得、单一数据根」的约定。
			WebviewUserDataPath: joinPath(dataRoot, "webview"),
		},
	})
	subSvc.AttachApp(app)
	childWinSvc.AttachApp(app)

	mainWindow := app.Window.NewWithOptions(windowOptions())

	// —— Wave D：快捷键与精简 UX（FR-09）——
	// 构造时机：必须在 app.Run 之前（Wails 会把 Run 前的注册排队），
	// 且必须在 window 创建之后（window.show 要拿窗口）。
	hkRegistry := hotkeys.DefaultRegistry()
	hkSvc := hotkeys.New(hkRegistry, newWailsRegistrar(app),
		hotkeys.NewPreferenceStore(holder), hotkeys.Emit{
			Changed: func(b contract.Binding) {
				app.Event.Emit(events.HotkeysChanged, b)
			},
			// Lean 不单独发事件：精简开关的调用方（设置页 / 热键触发后的前端）
			// 都能从返回值或 EV-17 得知结果，多一条事件只会多一套真相。
		})
	hkShell.Attach(hkSvc)
	wireHotkeyHandlers(hkRegistry, recSvc, settingsSvc, mainWindow, app)

	// 系统托盘默认禁用：由前端 SystrayService.enable() 开启，开关持久化在 preferences 表，
	// 这里按上次会话的状态恢复（app.Run 之前创建的托盘 / 窗口会延迟到事件循环启动后生效）。
	systraySvc.attach(app, mainWindow)
	systraySvc.restore(context.Background())

	// 启动预热（ARCH §4.2.2「启动时不阻塞 UI：引擎在后台 Warm」）。
	//
	// Router 构造时状态是 loading，此前没有任何调用点去预热它：录制页会永远显示
	// 「引擎加载中」，即使已装好模型，「发起转写」也一直报 ERR_ENGINE_NOT_READY。
	// 放在 application.New 之后：预热结果经 enginesvc 的回调转成 EV engine:status 推给前端。
	// 失败（典型是没装模型）只是把状态置 failed 并带上原因，不影响启动与录制；
	// 本地失败不会回退在线（隐私默认，由 Router 保证）。
	go func() {
		if werr := asrRouter.Warm(context.Background()); werr != nil {
			logx.For("asr").Info("启动预热未就绪",
				"source", initialSource, "code", apierr.From(werr, apierr.CodeEngineLoad).Code)
		}
	}()

	go emitTimeLoop(app)

	defer logx.Close()
	return app.Run()
}

// serviceOptions 为服务挂上统一错误序列化与可读名称。
func serviceOptions(name string) application.ServiceOptions {
	return application.ServiceOptions{
		Name:         name,
		MarshalError: apierr.Marshal,
	}
}

// parsePathFlags 从 argv 中提取 --data-dir / --model-dir（最高优先级层）。
//
// 刻意手写而非 flag.Parse：Wails 自身的命令行参数（以及 `wails3 dev` 注入的参数）
// 会与 flag 包冲突并触发 "flag provided but not defined" 退出。
// 支持 `--key=value` 与 `--key value` 两种写法。
func parsePathFlags(args []string) map[paths.Key]string {
	out := map[paths.Key]string{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		name, value, hasValue := strings.Cut(arg, "=")
		switch name {
		case paths.FlagDataDir, paths.FlagModelDir:
		default:
			continue
		}
		if !hasValue {
			if i+1 >= len(args) {
				continue
			}
			value = args[i+1]
			i++
		}
		key := paths.KeyDataRoot
		if name == paths.FlagModelDir {
			key = paths.KeyModelRoot
		}
		out[key] = value
	}
	return out
}

// joinPath 是 filepath.Join 的一行别名：调用点语义更清楚（拼 dataRoot 子目录）。
func joinPath(base, elem string) string { return filepath.Join(base, elem) }

// emitTimeLoop 演示后端主动推事件给前端的链路。
// 真实业务可参考此结构注册自己的后台 goroutine。
func emitTimeLoop(app *application.App) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for range ticker.C {
		app.Event.Emit(events.Time, time.Now().Format(time.RFC1123))
	}
}
