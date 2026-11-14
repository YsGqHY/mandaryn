// AppSettingsService：单行表 app_settings 的前端封装。
//
// 后端字段：themeChoice / customTheme(JSON 字符串) / localeChoice。
// customTheme 在前端 stringify / parse；其余按字符串透传。
import { Events } from '@wailsio/runtime';
import { Service as Binding } from '@bindings/vip.oberon.mandaryn/internal/services/appsettings';
import type { DebugModeState } from '@bindings/vip.oberon.mandaryn/internal/contract/models';
import { toAppError } from '../errors/AppError';

export type AppSettingsField =
  | 'themeChoice'
  | 'customTheme'
  | 'localeChoice'
  | 'liveTranscribe'
  | 'privacy'
  | 'debugMode';

export interface AppSettingsSnapshot {
  themeChoice: string;
  customTheme: string; // JSON: { mode, palette }；空字符串=未设置
  localeChoice: string;
  // 整机性能预算 N（NFR-08；10–100，步进 5）。只读快照，写入走 PerfService.setCap。
  resourceCapPct: number;
  // 边录边转开关（FR-03-10，**默认 false**）。写入走 setLiveTranscribe。
  liveTranscribe: boolean;
}

export const AppSettingsService = {
  async get(): Promise<AppSettingsSnapshot> {
    const snap = await Binding.Get();
    // `liveTranscribe` 在绑定重新生成后才会出现在类型里；这里按结构取值，
    // 保证「后端已加字段、绑定未重生成」的过渡期也能正确读到 false。
    const extended = snap as unknown as { liveTranscribe?: boolean };
    return {
      themeChoice: snap.themeChoice ?? '',
      customTheme: snap.customTheme ?? '',
      localeChoice: snap.localeChoice ?? '',
      resourceCapPct: snap.resourceCapPct ?? 0,
      liveTranscribe: extended.liveTranscribe ?? false,
    };
  },

  async setThemeChoice(choice: string): Promise<void> {
    await Binding.SetThemeChoice(choice);
  },

  // value 传 null/undefined 等价于清空。
  async setCustomTheme(value: unknown): Promise<void> {
    if (value === null || value === undefined) {
      await Binding.SetCustomTheme('');
      return;
    }
    await Binding.SetCustomTheme(JSON.stringify(value));
  },

  async resetCustomTheme(): Promise<void> {
    await Binding.ResetCustomTheme();
  },

  async setLocaleChoice(choice: string): Promise<void> {
    await Binding.SetLocaleChoice(choice);
  },

  // 切换「边录边转」（API-9.10 / FR-03-10）。
  //
  // 语义：只在**下一个会话**生效；录制中后端返回 ERR_STATE_FORBIDDEN
  // （前端同时应禁用控件，二者不冲突：后端是真相，前端是体验）。
  // 关掉它不影响「录完再转」——延后转写是独立任务。
  async setLiveTranscribe(enabled: boolean): Promise<void> {
    // 该方法在绑定重新生成后才出现在类型里；用结构断言保证类型检查能过，
    // 缺失时给出明确错误而不是静默失败（避免「开关点了没反应」）。
    const ext = Binding as unknown as {
      SetLiveTranscribe?: (value: boolean) => Promise<void>;
    };
    if (typeof ext.SetLiveTranscribe !== 'function') {
      throw new Error('后端绑定缺少 SetLiveTranscribe（需要重新生成 bindings）');
    }
    await ext.SetLiveTranscribe(enabled);
  },

  // 切换调试模式（API-9.16）：立即生效、无需重启，持久化在 app_settings.log_level。
  // 被环境变量 MANDARYN_LOG_LEVEL 接管时仍保存用户层的值，返回的生效状态以环境变量为准。
  async setDebugMode(enabled: boolean): Promise<DebugModeState> {
    try {
      return await Binding.SetDebugMode(enabled);
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 订阅"设置已变更"广播（后端任一 setter 写成功后对所有窗口 Emit appsettings:changed）。
  // 托盘面板 / 子窗口 / 主窗口据此重新 get()，实现主题、语言跨窗口实时同步。返回取消订阅函数。
  onChanged(callback: (field: AppSettingsField) => void): () => void {
    return Events.On('appsettings:changed', (event) => {
      callback(event.data as AppSettingsField);
    });
  },
} as const;
