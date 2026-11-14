// SessionService：会话库（列表 / 详情 / 改名 / 删除 / 导出）与转写任务
// （发起 / 取消 / 查询 / 时间轴）的前端封装。
//
// 后端：
//   internal/services/sessionsvc（API-3.1~3.5）
//     List / Get / GetDetail / Rename / Delete / DeleteDetailed / Export
//   internal/services/transcriptsvc
//     StartTranscription / CancelTranscription / GetTranscription /
//     GetTimeline / GetTimelinePage / SearchSegments
//
// 语义提醒：
//   - 转写状态（none|queued|running|done|failed）是**独立轴**，与会话录制状态
//     status（completed|interrupted|failed|degraded）正交，不要混为一谈；
//   - 导出 / 另存都不弹后端 UI：targetPath 必须由前端 NativeDialogs 提供；
//   - 删除默认连音频一起删，音频删不掉的会话会出现在 DeleteResult.audioFailed。
import { Events } from '@wailsio/runtime';
import { Service as SessionsBinding } from '@bindings/vip.oberon.mandaryn/internal/services/sessionsvc';
import { Service as TranscriptsBinding } from '@bindings/vip.oberon.mandaryn/internal/services/transcriptsvc';
import type {
  DeleteResult,
  ExportRequest,
  ListQuery,
  ListResult,
  Segment,
  SegmentQuery,
  Session,
  SessionDetail,
  Timeline,
  TimelinePage,
  TranscriptionState,
} from '@bindings/vip.oberon.mandaryn/internal/contract/models';
import { AppEvents } from '@/shared/events';
import { toAppError } from '../errors/AppError';

// 契约类型原样 re-export（页面不手写重复类型）。
export type {
  DeleteResult,
  ExportRequest,
  ListQuery,
  ListResult,
  Segment,
  SegmentQuery,
  Session,
  SessionDetail,
  Timeline,
  TimelinePage,
  TranscriptionState,
} from '@bindings/vip.oberon.mandaryn/internal/contract/models';

// 导出格式：contract.ExportRequest.format 的四个取值。
export type ExportFormat = 'txt' | 'srt' | 'csv' | 'xlsx';

// 转写状态取值（contract.TranscriptionState.status）。
export const TranscriptionStatuses = ['none', 'queued', 'running', 'done', 'failed'] as const;
export type TranscriptionStatusValue = (typeof TranscriptionStatuses)[number];

// 会话录制状态取值（contract.Session.status）。
export const SessionStatuses = ['completed', 'interrupted', 'failed', 'degraded'] as const;

// 转写中（queued 已入队也算「进行中」，UI 需要显示可取消）。
export const isTranscribing = (status: string): boolean =>
  status === 'queued' || status === 'running';

// 可以（重新）发起转写：未转 / 失败 / 已完成（整段重转）。
export const canStartTranscription = (status: string): boolean => !isTranscribing(status);

export const SessionService = {
  // 分页 + 关键词 + 筛选（API-3.1）。
  async list(query: ListQuery): Promise<ListResult> {
    try {
      return await SessionsBinding.List(query);
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 会话详情（API-3.2）。
  async get(sessionId: string): Promise<Session> {
    try {
      return await SessionsBinding.Get(sessionId);
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 详情 + 音频是否还在（API-3.2 扩展）：回放 / 导出按钮是否可用靠它。
  async getDetail(sessionId: string): Promise<SessionDetail> {
    try {
      return await SessionsBinding.GetDetail(sessionId);
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 改标题（API-3.3）。
  async rename(sessionId: string, title: string): Promise<void> {
    try {
      await SessionsBinding.Rename(sessionId, title);
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 批量删除（API-3.4）。音频删不掉不报错；需要逐项结果用 deleteDetailed。
  async delete(sessionIds: string[], deleteAudio: boolean): Promise<void> {
    try {
      await SessionsBinding.Delete(sessionIds, deleteAudio);
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 批量删除 + 逐项结果（API-3.4 扩展），用于提示「记录已删、音频仍在磁盘」。
  async deleteDetailed(sessionIds: string[], deleteAudio: boolean): Promise<DeleteResult> {
    try {
      return await SessionsBinding.DeleteDetailed(sessionIds, deleteAudio);
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 导出会话文本（API-3.5），返回实际落盘路径。
  async export(request: ExportRequest): Promise<string> {
    try {
      return await SessionsBinding.Export(request);
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 发起转写（手动）。已 done 时等价整段重转。
  async startTranscription(sessionId: string): Promise<TranscriptionState> {
    try {
      return await TranscriptsBinding.StartTranscription(sessionId);
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 取消排队 / 运行中的转写；已转句子保留，状态回 none。
  async cancelTranscription(sessionId: string): Promise<TranscriptionState> {
    try {
      return await TranscriptsBinding.CancelTranscription(sessionId);
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 幂等查询转写进度（窗口重开 / 事件丢失时兜底）。
  async getTranscription(sessionId: string): Promise<TranscriptionState> {
    try {
      return await TranscriptsBinding.GetTranscription(sessionId);
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 一次性取整段时间轴（≤2 万条）。
  async getTimeline(sessionId: string): Promise<Timeline> {
    try {
      return await TranscriptsBinding.GetTimeline(sessionId);
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 超大通话的时间轴分页。
  async getTimelinePage(sessionId: string, offset: number, limit: number): Promise<TimelinePage> {
    try {
      return await TranscriptsBinding.GetTimelinePage(sessionId, offset, limit);
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 片段级检索（带跳转时间）。
  async searchSegments(query: SegmentQuery): Promise<Segment[]> {
    try {
      return await TranscriptsBinding.SearchSegments(query);
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 订阅转写任务状态（EV-16 transcription:state）。返回取消订阅函数。
  onTranscriptionState(callback: (state: TranscriptionState) => void): () => void {
    const cancel = Events.On(AppEvents.TranscriptionState, (event) => {
      const payload = event.data as TranscriptionState | null;
      if (payload) callback(payload);
    });
    return () => {
      if (typeof cancel === 'function') cancel();
    };
  },
} as const;
