// HotkeysService：快捷键体系（FR-09）的前端封装。
//
// 后端归属：internal/services/hotkeysvc（薄壳）+ internal/hotkeys（L2 领域）。
//   Available / List / ListActions / Set / Disable / Reset / ResetAll /
//   DetectConflicts / GetLeanState / SetLeanMode
//
// 语义提醒（DOC-05 §2.3）：
//   - 加速键的传输形态是**显示形**（macOS 为 ⌘⌥R，其它平台 Ctrl+Alt+R）；
//     Set 接受同一形态，后端 Parse 会把字形还原成规范形；
//   - 冲突必须在保存前判完：Set 命中冲突返回 ERR_HOTKEY_CONFLICT（软件内重复 /
//     系统保留），命中系统占用返回 ERR_HOTKEY_REGISTER（已回滚），前端不得静默；
//   - 绑定存 null = 该动作被禁用（DOC-04 TBL-01.1）。
import { Events } from '@wailsio/runtime';
import { Service as HotkeysBinding } from '@bindings/vip.oberon.mandaryn/internal/services/hotkeysvc';
import type {
  Binding,
  ConflictReport,
} from '@bindings/vip.oberon.mandaryn/internal/contract/models';
import type {
  ActionInfo,
  LeanState,
} from '@bindings/vip.oberon.mandaryn/internal/hotkeys/models';
import { AppEvents } from '@/shared/events';
import { toAppError } from '../errors/AppError';

// 契约类型原样 re-export（页面不手写重复类型）。
export type {
  Binding,
  ConflictReport,
  HotkeyConflict,
} from '@bindings/vip.oberon.mandaryn/internal/contract/models';
export type { ActionInfo, LeanState } from '@bindings/vip.oberon.mandaryn/internal/hotkeys/models';

// 冲突原因键（contract.HotkeyConflict.reason）：system 系统保留 / duplicate 与其它动作重复。
export type ConflictReason = 'system' | 'duplicate';

// 动作分组顺序（DOC-05 §2.2 的场景顺序），未知分组排在最后。
export const HOTKEY_GROUP_ORDER = [
  'recording',
  'playback',
  'navigation',
  'search',
  'editing',
  'model',
  'ux',
] as const;

// 作用域取值：global 软件在后台也响应 / app 仅应用内。
export type HotkeyScope = 'global' | 'app';

export const HotkeysService = {
  // 平台注册能力是否可用（false 时 UI 应整块提示「当前平台不支持系统级快捷键」）。
  async available(): Promise<boolean> {
    try {
      return await HotkeysBinding.Available();
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 全部动作的当前绑定快照（accelerator / default 均为显示形）。
  async list(): Promise<Binding[]> {
    try {
      return await HotkeysBinding.List();
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 动作元数据（i18n 键 / 分组 / 作用域 / 默认值 / 是否已接线）。
  async listActions(): Promise<ActionInfo[]> {
    try {
      return await HotkeysBinding.ListActions();
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 设置绑定（先校验格式、再查冲突、再预注册，最后落库）。
  async set(action: string, accelerator: string): Promise<ConflictReport> {
    try {
      return await HotkeysBinding.Set(action, accelerator);
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 禁用单个动作（绑定存 null）。
  async disable(action: string): Promise<void> {
    try {
      await HotkeysBinding.Disable(action);
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 恢复单个动作的默认绑定。
  async reset(action: string): Promise<void> {
    try {
      await HotkeysBinding.Reset(action);
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 全部恢复默认。
  async resetAll(): Promise<void> {
    try {
      await HotkeysBinding.ResetAll();
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 单独探测某组合的冲突（软件内重复 + 系统保留），不做真实注册探测。
  async detectConflicts(accelerator: string): Promise<ConflictReport> {
    try {
      return await HotkeysBinding.DetectConflicts(accelerator);
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 精简模式状态 + 取舍清单（清单恒定返回，与 enabled 无关）。
  async getLeanState(): Promise<LeanState> {
    try {
      return await HotkeysBinding.GetLeanState();
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 开关精简模式（幂等；后端立刻持久化并广播）。
  async setLeanMode(enabled: boolean): Promise<LeanState> {
    try {
      return await HotkeysBinding.SetLeanMode(enabled);
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 订阅「绑定已变更」（EV-18 hotkeys:changed，含注册失败原因）。返回取消订阅函数。
  onChanged(callback: (binding: Binding) => void): () => void {
    const cancel = Events.On(AppEvents.HotkeysChanged, (event) => {
      const payload = event.data as Binding | null;
      if (payload) callback(payload);
    });
    return () => {
      if (typeof cancel === 'function') cancel();
    };
  },
} as const;
