// PerfService：性能预算（NFR-08）的前端封装。
//
// 后端归属：预算值 `resource_cap_pct` 存在 app_settings 单行表，
// 写入入口是 appsettings.SetResourceCap（API-9.9）；实时读数由预算域
// 以 1Hz 广播 `perf:usage`（EV-15）。本服务把这两条合到同一个域里，
// 页面只需要认 PerfService，不必知道读数和开关分属两个后端服务。
//
// 口径铁律（NFR-08 注 2）：capPct 是**整机份额**预算，不是「本应用上限」：
//   otherPct = totalCpuPct − selfCpuPct
//   headroomPct = capPct − otherPct
// 因此这里原样透传后端三个分量，**不在前端另算一套 CPU 口径**。
import { Events } from '@wailsio/runtime';
import { Service as Binding } from '@bindings/vip.oberon.mandaryn/internal/services/appsettings';
import type { PerfUsage } from '@bindings/vip.oberon.mandaryn/internal/contract/models';
import { AppEvents } from '@/shared/events';
import { toAppError } from '../errors/AppError';

// 契约类型原样 re-export（页面不手写重复类型）。
export type { PerfUsage } from '@bindings/vip.oberon.mandaryn/internal/contract/models';

// 预算取值约束（与后端 SetResourceCap 的校验一致：10–100 且为 5 的倍数）。
export const RESOURCE_CAP_MIN = 10;
export const RESOURCE_CAP_MAX = 100;
export const RESOURCE_CAP_STEP = 5;

// 降档档位：L0 全速 / L1~L3 逐级降档 / P 暂停推理。
export const PERF_LEVELS = ['L0', 'L1', 'L2', 'L3', 'P'] as const;
export type PerfLevel = (typeof PERF_LEVELS)[number];

// 把后端 level 字符串收敛成已知档位（未知值返回 null，UI 只显示原文）。
export const perfLevelOf = (level: string): PerfLevel | null =>
  (PERF_LEVELS as readonly string[]).includes(level) ? (level as PerfLevel) : null;

// 档位 P（或 suspended）表示推理已暂停 —— 录制与落盘不受影响（NFR-08）。
export const isInferencePaused = (usage: PerfUsage | null): boolean =>
  usage !== null && (usage.level === 'P' || usage.suspended);

export const PerfService = {
  // 设置整机预算 N（API-9.9）。越界返回 ERR_PERF_CAP_RANGE，UI 按 code 提示「10–100，步进 5」。
  // 成功时返回当前读数（后端已接入预算域时是真实三分量）。
  async setCap(capPct: number): Promise<PerfUsage> {
    try {
      return await Binding.SetResourceCap(capPct);
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 订阅性能采样（EV-15 perf:usage，1Hz）。返回取消订阅函数。
  onUsage(callback: (usage: PerfUsage) => void): () => void {
    const cancel = Events.On(AppEvents.PerfUsage, (event) => {
      const payload = event.data as PerfUsage | null;
      if (payload) callback(payload);
    });
    return () => {
      if (typeof cancel === 'function') cancel();
    };
  },
} as const;
