// CaptureService：音频源（应用 / 设备）枚举、能力矩阵与「上次选择」的前端封装。
//
// 后端 internal/services/capturesvc（API-1.1~1.6）：
//   ListSources / RefreshSources / ProbeSource / GetPreferredSource /
//   SetPreferredSource / GetCapabilities
//
// View 与 ViewModel 只经本文件访问后端，禁止直接 import @bindings/*。
import { Events } from '@wailsio/runtime';
import { Service as Binding } from '@bindings/vip.oberon.mandaryn/internal/services/capturesvc';
import type {
  CaptureCaps,
  CaptureSourcesChanged,
  Source,
} from '@bindings/vip.oberon.mandaryn/internal/contract/models';
import { AppEvents } from '@/shared/events';
import { toAppError } from '../errors/AppError';

// 契约类型直接 re-export（不手写重复类型）：页面从 '@/services' 拿到的就是 bindings 的真实形状。
export type { CaptureCaps, CaptureSourcesChanged, Source } from '@bindings/vip.oberon.mandaryn/internal/contract/models';

// SourceKind 是 contract.Source.kind 的两个取值。
export type SourceKind = 'application' | 'device';

// 判断源属于「应用」还是「设备」——UI 分组用。
// 后端 kind 为 'application' | 'device'；未知值按设备处理（设备分组永远可展示）。
export const sourceKindOf = (source: Source): SourceKind =>
  source.kind === 'application' ? 'application' : 'device';

export const CaptureService = {
  // 枚举源（API-1.1）。Present / Producing 是后端实时值。
  async listSources(): Promise<Source[]> {
    try {
      return await Binding.ListSources();
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 强制重新枚举（API-1.2，UI「刷新」按钮）。
  async refreshSources(): Promise<Source[]> {
    try {
      return await Binding.RefreshSources();
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 探测单个源当前是否可用 / 有声（API-1.3）。
  async probeSource(sourceId: string): Promise<Source> {
    try {
      return await Binding.ProbeSource(sourceId);
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 上次选择的源 ID（API-1.5）；未设置过返回空字符串。
  async getPreferredSource(): Promise<string> {
    try {
      return await Binding.GetPreferredSource();
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 记住选择（API-1.4）。后端会校验源存在性，不存在时抛 ERR_SOURCE_NOT_FOUND。
  async setPreferredSource(sourceId: string): Promise<void> {
    try {
      await Binding.SetPreferredSource(sourceId);
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 平台能力矩阵（API-1.6）——UI 决定「是否展示按应用分路」的唯一依据。
  async getCapabilities(): Promise<CaptureCaps> {
    try {
      return await Binding.GetCapabilities();
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 订阅源列表变化（EV-12 capture:sources）。返回取消订阅函数。
  onSourcesChanged(callback: (sources: Source[]) => void): () => void {
    const cancel = Events.On(AppEvents.CaptureSources, (event) => {
      const payload = event.data as CaptureSourcesChanged | Source[] | null;
      if (Array.isArray(payload)) {
        callback(payload);
        return;
      }
      callback(payload?.sources ?? []);
    });
    return () => {
      if (typeof cancel === 'function') cancel();
    };
  },
} as const;
