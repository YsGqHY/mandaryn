// PlaybackService：会话回放（FR-04 / API-4.x）的前端封装。
//
// 后端：
//   internal/services/transcriptsvc
//     GetTimelinePage(sessionId, offset, limit) → TimelinePage{ sessionId, total, offset, limit, segments[] }
//     GetTimeline(sessionId) → Timeline{ sessionId, durationMs, segments[], markers[] }
//   internal/services/sessionsvc
//     GetDetail(sessionId) → SessionDetail{ session, audioExists }
//   internal/services/speakersvc
//     ListRoles() → Role[]（含 colorToken，用于说话人着色）
//   音频：同源 HTTP 流 GET /audio/{sessionId}（后端已支持 Range，可 seek）
//
// 语义提醒：
//   - 音频**不**读 base64、**不**整段 fetch 进内存：直接用原生 <audio> 的 src 指向
//     同源 URL，由浏览器自己按 Range 拉流并解码。这里只负责拼 URL。
//   - audioExists 为 false 时不渲染播放器（后端已找不到音频文件，播放器必然报错）。
//   - 片段按 startMs 单调不减（后端保证），所以「当前片段」可以用二分查找定位。
//   - 落在两句之间的间隙时**保持上一句为当前句**（UX-SPEC §5.5），不要返回 -1 抖动。
//   - **分页优先**（E-06）：片段的常规读取走 getTimelinePage（首屏一页，滚动增量补齐）。
//     TimelinePage 自带 durationMs 与 markers（offset==0 时下发），因此大会话**不需要**
//     再发一次整段请求；getTimeline 只用于小会话（≤1500 句）一次性取全。
//     细节见 pagedTimeline.ts 顶部说明。
import { Service as SessionsBinding } from '@bindings/vip.oberon.mandaryn/internal/services/sessionsvc';
import { Service as TranscriptsBinding } from '@bindings/vip.oberon.mandaryn/internal/services/transcriptsvc';
import type {
  Marker,
  Segment,
  SessionDetail,
  Timeline,
  TimelinePage,
} from '@bindings/vip.oberon.mandaryn/internal/contract/models';
import { SpeakerService, type Role } from '../speaker/SpeakerService';
import { toAppError } from '../errors/AppError';

// 契约类型原样 re-export（页面不手写重复类型）。
export type { Marker, Role, Segment, SessionDetail, Timeline, TimelinePage };

// 一次「后退 / 前进」的步长（FR-04-1：前后 5 秒）。
export const SEEK_STEP_MS = 5000;

// 倍速档位（FR-04-1：0.5/1/1.5/2；额外提供 0.75/1.25 便于对听力材料）。
export const PLAYBACK_RATES = [0.5, 0.75, 1, 1.25, 1.5, 2] as const;

// 音量步进（键盘 / 按钮微调用）。
export const VOLUME_STEP = 0.05;

// 时钟 state 的更新节流窗口：rAF 每帧都读 audio.currentTime 并二分定位当前片段
// （切换延迟 ≤ 1 帧 ≈ 17ms，远小于 FR-04-2 的 80ms 上限），但「播放位置」这个
// state 每帧都 setState 会让大列表白重渲染，所以时钟单独按 60ms 节流。
export const CLOCK_THROTTLE_MS = 60;

// 自动居中滚动后，用户多久没有手动滚动才恢复自动居中不算数 —— 这里不用定时器，
// 用户滚一下就永久暂停自动居中，直到点「回到当前句」或播放器 seek 归位。

// 时间轴刻度：marker 种类 → i18n key 后缀（`library.playback.markers.<kind>`）。
// 后端 kind 取值见 contract.Marker；未列出的种类走 generic 兜底。
export const MARKER_KINDS = [
  'degrade',
  'source-lost',
  'device-switch',
  'paused',
  'resumed',
  'perf-throttle',
  'perf-suspend',
  'perf-resume',
] as const;

export type MarkerKind = (typeof MARKER_KINDS)[number];

const MARKER_KIND_SET = new Set<string>(MARKER_KINDS);

// 该 marker 种类是否在前端有专门文案（否则 UI 用 generic + note 兜底）。
export const isKnownMarkerKind = (kind: string): kind is MarkerKind => MARKER_KIND_SET.has(kind);

export const PlaybackService = {
  // 取一页片段。offset/limit 由调用方按页算好（后端把 limit 钳到 [1,2000]、offset 越界钳到合法值）。
  // 返回的 `total` 是**整段**的句数，用来算页数与总高度。
  async getTimelinePage(sessionId: string, offset: number, limit: number): Promise<TimelinePage> {
    try {
      return await TranscriptsBinding.GetTimelinePage(sessionId, offset, limit);
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 一次性取整段（≤2 万条），含 markers / durationMs。仅小会话一次性取全时用。
  async getTimeline(sessionId: string): Promise<Timeline> {
    try {
      return await TranscriptsBinding.GetTimeline(sessionId);
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 会话详情：audioExists 决定播放器是否可渲染。
  async getDetail(sessionId: string): Promise<SessionDetail> {
    try {
      return await SessionsBinding.GetDetail(sessionId);
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 全局角色列表：只为拿 roleId → colorToken（说话人着色）与最新角色名。
  async listRoles(): Promise<Role[]> {
    return SpeakerService.listRoles();
  },

  // 同源音频流地址（后端已支持 Range，<audio> 自己会按需拉流并 seek）。
  // encodeURIComponent：sessionId 里可能出现 ':' 等 URL 保留字符。
  audioSrc(sessionId: string): string {
    return `/audio/${encodeURIComponent(sessionId)}`;
  },
} as const;

// —— 纯函数工具（无 React / 无 DOM 依赖，便于复用与单测） ——

// 二分查找「当前位置对应的片段下标」。
//   - 取 startMs <= positionMs 的**最后**一个片段：落在两句间隙时保持上一句（不抖动）；
//   - 位置在第一句之前返回 -1（UI 显示「尚未开始」而非乱高亮）。
// segments 由后端保证按 startMs 单调不减；若调用方传入未排序数据，结果不保证正确。
export const findActiveIndex = (segments: readonly Segment[], positionMs: number): number => {
  if (segments.length === 0) return -1;
  const first = segments[0];
  if (first === undefined) return -1;
  if (positionMs < first.startMs) return -1;

  let lo = 0;
  let hi = segments.length - 1;
  let found = 0;
  while (lo <= hi) {
    const mid = (lo + hi) >> 1;
    const seg = segments[mid];
    if (seg === undefined) break;
    if (seg.startMs <= positionMs) {
      found = mid;
      lo = mid + 1;
    } else {
      hi = mid - 1;
    }
  }
  return found;
};

// 把目标时间夹到 [0, durationMs]；durationMs 未知（<=0）时只保证非负。
export const clampSeek = (positionMs: number, durationMs: number): number => {
  if (!Number.isFinite(positionMs)) return 0;
  const nonNegative = Math.max(0, positionMs);
  if (!Number.isFinite(durationMs) || durationMs <= 0) return nonNegative;
  return Math.min(nonNegative, durationMs);
};

// 毫秒 → "mm:ss" / "h:mm:ss"（片段时间戳与播放器时间共用）。
export const formatTime = (ms: number): string => {
  const totalSeconds = Math.max(0, Math.floor((Number.isFinite(ms) ? ms : 0) / 1000));
  const hours = Math.floor(totalSeconds / 3600);
  const minutes = Math.floor((totalSeconds % 3600) / 60);
  const seconds = totalSeconds % 60;
  const pad = (value: number): string => (value < 10 ? `0${value}` : String(value));
  return hours > 0 ? `${hours}:${pad(minutes)}:${pad(seconds)}` : `${pad(minutes)}:${pad(seconds)}`;
};

// 秒 → 毫秒（<audio>.currentTime / duration 都是秒）。
export const secondsToMs = (seconds: number): number =>
  Number.isFinite(seconds) ? Math.max(0, seconds * 1000) : 0;
