// PrivacyService：隐私边界（FR-08）的前端封装。
//
// 后端归属：internal/services/privacysvc + internal/privacy。
//   GetStatus       离线状态与来源（cli/env/user）+ 最近 24h 出站汇总
//   SetOffline      切用户层离线开关（env 层接管时无法在此解除）
//   ListOutbound    分页读最近出站记录（新的在前）
//   ClearOutbound   清空出站留痕
//   RunAudit        零出站自检（结论 + 违例明细）
//   ExportDiagnostics 生成脱敏诊断包（不含音频 / 正文 / 声纹向量 / 凭据）
//   GetDebugLogStatus / ReadDebugLog / ClearDebugLog / ReportFrontendLog
//                   调试模式的状态、查看器、清空与前端上报（开关本身走 AppSettingsService.setDebugMode）
//
// 铁律：诊断包的落盘必须由前端经 NativeDialogs 的**原生「另存为」**拿到路径，
// 后端不弹 UI、也从不自行落盘。本服务只负责生成产物与（在可用时）写入目标路径。
import { Service as Binding } from '@bindings/vip.oberon.mandaryn/internal/services/privacysvc';
import type {
  AuditFilter,
  AuditReport,
  ClearResult,
  DebugLogPage,
  DebugLogQuery,
  DebugLogStatus,
  DiagnosticsArtifact,
  FrontendLogEntry,
  OutboundPage,
  PrivacyStatus,
} from '@bindings/vip.oberon.mandaryn/internal/contract/models';
import { toAppError } from '../errors/AppError';

// 契约类型原样 re-export（页面不手写重复类型）。
export type {
  AuditFilter,
  AuditFinding,
  AuditReport,
  ClearResult,
  DebugLogEntry,
  DebugLogFile,
  DebugLogPage,
  DebugLogQuery,
  DebugLogStatus,
  DebugModeState,
  DiagnosticsArtifact,
  FrontendLogEntry,
  OutboundPage,
  OutboundRecord,
  PrivacyStatus,
} from '@bindings/vip.oberon.mandaryn/internal/contract/models';

// 调试日志查看器的筛选取值（与后端 DebugLogQuery 对齐；'' = 不过滤）。
export type DebugLogLevel = '' | 'DEBUG' | 'INFO' | 'WARN' | 'ERROR';
export type DebugLogOrigin = '' | 'backend' | 'frontend';

// 查看器一次读取的条数（后端上限 500）。
export const DEBUG_LOG_PAGE_SIZE = 200;

// 离线生效来源：'' 未开 / 'env' 环境变量 / 'user' 用户开关 / 'env+user' 两者。
export type OfflineSource = '' | 'env' | 'user' | 'env+user';

// envForced 表示「离线由环境变量强制」——此时界面不得假装能改（FR-08）。
export const isOfflineEnvForced = (status: PrivacyStatus | null): boolean =>
  status !== null && status.envOffline;

// 出站记录分页尺寸（设置页表格一页 20 条）。
export const OUTBOUND_PAGE_SIZE = 20;

// 自检默认窗口：最近 24h（与出站留痕保留策略一致）。
export const DEFAULT_AUDIT_WINDOW_MS = 24 * 60 * 60 * 1000;

// base64 → 字节长度（不解码，只估算传输体积用于展示）。
const base64ByteLength = (b64: string): number => {
  const clean = b64.replace(/\s+/g, '');
  if (clean.length === 0) return 0;
  const padding = clean.endsWith('==') ? 2 : clean.endsWith('=') ? 1 : 0;
  return Math.max(0, Math.floor((clean.length * 3) / 4) - padding);
};

// 诊断包保存结果。
//   ok=true  —— 已写入目标路径；
//   ok=false —— 当前构建的后端未暴露「写入任意字节到指定路径」的接口
//              （exportsvc 只接受 .csv / .xlsx，privacysvc 只生成产物）。
//               这一分支不改错误码约定（错误码只由后端登记），由 UI 用 i18n 说明。
export type SaveArtifactResult = { ok: true; path: string } | { ok: false; reason: 'unsupported' };

// 后端是否已提供「写诊断包到指定路径」的接口。
// 后端补上 WriteDiagnostics 后：把它改为 true，并在 saveDiagnostics 里调用该方法。
// UI 据此在用户点「另存为」**之前**就把限制说清楚，避免选完路径才失败。
export const SAVE_DIAGNOSTICS_SUPPORTED = true;

export const PrivacyService = {
  // 离线状态 + 来源 + 最近 24h 出站汇总。
  async getStatus(): Promise<PrivacyStatus> {
    try {
      return await Binding.GetStatus();
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 切用户层离线开关。返回切换后的状态（env 层仍在生效时 offline 仍为 true）。
  async setOffline(enabled: boolean): Promise<PrivacyStatus> {
    try {
      return await Binding.SetOffline(enabled);
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 分页读取出站记录（新的在前）。
  async listOutbound(limit: number = OUTBOUND_PAGE_SIZE, offset = 0): Promise<OutboundPage> {
    try {
      return await Binding.ListOutbound(limit, offset);
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 清空出站留痕（用户主动「清空留痕」）。
  async clearOutbound(): Promise<ClearResult> {
    try {
      return await Binding.ClearOutbound();
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 零出站自检（FR-08：支撑「近期无外连」的自证）。
  async runAudit(filter?: Partial<AuditFilter>): Promise<AuditReport> {
    const now = Date.now();
    const request: AuditFilter = {
      fromMs: filter?.fromMs ?? now - DEFAULT_AUDIT_WINDOW_MS,
      toMs: filter?.toMs ?? now,
      kinds: filter?.kinds ?? [],
      limit: filter?.limit ?? 0,
    };
    try {
      return await Binding.RunAudit(request);
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 生成脱敏诊断包（字节为 base64；后端从不落盘、也从不下音频）。
  async exportDiagnostics(): Promise<DiagnosticsArtifact> {
    try {
      return await Binding.ExportDiagnostics();
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 诊断包字节的传输体积（展示用）。
  artifactByteLength(artifact: DiagnosticsArtifact): number {
    return base64ByteLength(artifact.bytes ?? '');
  },

  // 把诊断包写入用户选定的路径。
  // path 必须来自前端 NativeDialogs.saveFile（后端不弹 UI）。
  //
  // 实现说明：后端目前**没有**「写任意字节到指定路径」的服务方法，
  // 因此这里只能回报 unsupported。等后端补上 WriteDiagnostics 后，仅改本函数即可。
  async saveDiagnostics(path: string, _artifact: DiagnosticsArtifact): Promise<SaveArtifactResult> {
    // 由后端重新生成并原子落盘，而不是把前端手里的 base64 再回传一遍：
    // ① 少一次跨桥大字节传输；② 落盘内容与「自检通过的产物」严格同源。
    const written = await Binding.WriteDiagnostics(path);
    return { ok: true, path: written };
  },

  // —— 调试模式（API-10.8 ~ 10.11）——

  // 调试模式生效状态 + 日志目录、文件、占用与保留策略。
  async getDebugLogStatus(): Promise<DebugLogStatus> {
    try {
      return await Binding.GetDebugLogStatus();
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 读取调试日志（新的在前）。调试模式关闭时同样可读。
  async readDebugLog(query: Partial<DebugLogQuery> = {}): Promise<DebugLogPage> {
    const request: DebugLogQuery = {
      limit: query.limit ?? DEBUG_LOG_PAGE_SIZE,
      minLevel: query.minLevel ?? '',
      origin: query.origin ?? '',
      component: query.component ?? '',
    };
    try {
      return await Binding.ReadDebugLog(request);
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 删除全部调试日志文件，返回清空后的状态。
  async clearDebugLog(): Promise<DebugLogStatus> {
    try {
      return await Binding.ClearDebugLog();
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 上报一条前端错误 / 警告（只在调试模式开启时调用；后端关闭时也会直接忽略）。
  async reportFrontendLog(entry: FrontendLogEntry): Promise<void> {
    await Binding.ReportFrontendLog(entry);
  },
} as const;
