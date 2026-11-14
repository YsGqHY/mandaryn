// 应用启动后由后端定时推送的事件名集合，
// 与 internal/events/events.go 中的常量保持同步。
//
// bindings 未生成 internal/events 目录（后端只用 application.RegisterEvent 注册强类型载荷），
// 因此这里按 internal/events/events.go 的常量值手写字符串 —— 改后端常量必须同步这里。
export const AppEvents = {
  Time: 'time',

  // EV-12 源列表变化（应用启动/退出、设备增减）。
  CaptureSources: 'capture:sources',
  // EV-13 源失效 / 重连中。
  CaptureInvalid: 'capture:invalid',

  // EV-01 录制主状态机每次转换（ST-01~ST-09）。
  RecordingState: 'recording:state',
  // EV-02 采集电平，10Hz 节流（页面侧再做节流展示）。
  RecordingLevel: 'recording:level',
  // EV-03 采集节拍，1Hz（计时与已写字节）。
  RecordingTick: 'recording:tick',
  // EV-16 转写任务状态（入队 / 开跑 / 完成 / 失败 / 被抢占）。
  TranscriptionState: 'transcription:state',
  // EV-04 流式出字（**仅边录边转开启时**，FR-03-10）：
  //   载荷是 finalized=false 的 Segment，只用于「正在说的话」，**不落库**。
  TranscriptPartial: 'transcript:partial',
  // EV-05 逐句定稿（延后转写与边录边转两条路径都会发）。
  TranscriptSegment: 'transcript:segment',
  // EV-07 角色变更（改名 / 换色 / 合并 / 拆分 / 指派后广播，载荷 contract.RoleChanged）。
  // 片段上的角色名由后端 roles JOIN 得到，前端收到事件后**重新拉取**列表即可，
  // 不要在本地做名字替换（那会与历史会话的真实数据分叉）。
  RolesChanged: 'roles:changed',

  // EV-08 引擎预热 / 降级 / 恢复 / 失败。
  EngineStatus: 'engine:status',

  // EV-09 下载进度，4Hz 节流。
  ModelsDownload: 'models:download',
  // EV-10 仓库变化（安装 / 删除 / 校验完成）。
  ModelsChanged: 'models:changed',
  // EV-11 迁移进度。
  ModelsMigrate: 'models:migrate',

  // EV-15 性能预算采样，1Hz（NFR-08 三分量读数 + 档位）。
  PerfUsage: 'perf:usage',
  // EV-18 快捷键绑定变化（单个动作，含注册失败原因）。
  HotkeysChanged: 'hotkeys:changed',
} as const;

export type AppEventName = (typeof AppEvents)[keyof typeof AppEvents];
