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

// ProxyType 是代理类型（与后端 contract.ProxyConfig.type 同一取值集）。
export type ProxyType = 'none' | 'http' | 'https' | 'socks5';

// ProxyConfigDTO 是代理表单的读写形状（与后端 contract.ProxyConfig 对齐）。
export interface ProxyConfigDTO {
  type: string;
  host: string;
  port: number;
  username: string;
  // password 读出时是**掩码**；写入时掩码或空串表示「保持原值」。
  password: string;
  useForDownload: boolean;
  useForOnline: boolean;
  noProxy: string;
}

// ProxyTestResult 是代理连通性测试结果（API-9.4 / contract.TestResult）。
export interface ProxyTestResult {
  ok: boolean;
  latencyMs: number;
  // category 稳定取值：auth | rate-limit | tls | timeout | network | proxy | unknown。
  category: string;
  // note 后端给的**已脱敏**说明（不含密码与完整 URL）。
  note: string;
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

  // —— 代理（API-9.3 / API-9.4 / FR-05-5）——

  // 读取代理配置（密码为掩码，**不是**明文）。
  async getProxy(): Promise<ProxyConfigDTO> {
    try {
      const snap = await Binding.Get();
      const p = (snap as unknown as { proxy?: Partial<ProxyConfigDTO> }).proxy;
      return {
        type: p?.type ?? 'none',
        host: p?.host ?? '',
        port: p?.port ?? 0,
        username: p?.username ?? '',
        password: p?.password ?? '',
        useForDownload: p?.useForDownload ?? true,
        useForOnline: p?.useForOnline ?? true,
        noProxy: p?.noProxy ?? '',
      };
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 保存代理配置（API-9.3）。
  //
  // 密码语义：传掩码（如 `ab***yz`）或空串表示「保持库中已存的那份」——
  // 因此界面回显掩码、用户不动它就能安全地只改其它字段。
  async setProxy(p: ProxyConfigDTO): Promise<void> {
    try {
      await Binding.SetProxy(p as never);
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 清空代理配置（等价于保存 type=none，后端会连带清空主机与凭据）。
  async clearProxy(): Promise<void> {
    await AppSettingsService.setProxy({
      type: 'none',
      host: '',
      port: 0,
      username: '',
      password: '',
      useForDownload: true,
      useForOnline: true,
      noProxy: '',
    });
  },

  // 读取「代理是否被环境变量接管」的状态（FR-05-5）。
  //
  // 返回接管变量名（如 MANDARYN_HTTP_PROXY）；未接管返回空串。
  // 变量名由后端给（不前端硬编码）：改名时提示才不会变成谎话。
  async getProxyEnvOverride(): Promise<string> {
    try {
      const snap = await Binding.Get();
      return (snap as unknown as { proxyEnvVar?: string }).proxyEnvVar ?? '';
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 测试代理连通（API-9.4）。**不落库**；只测「能否经该代理拿到目标响应」。
  //
  // 返回的 TestResult.Category 是稳定取值（auth / rate-limit / tls / timeout /
  // network / proxy / unknown），文案由 View 按 i18n 渲染，VM 不拼人类可见字符串。
  async testProxy(p: ProxyConfigDTO): Promise<ProxyTestResult> {
    try {
      const result = await Binding.TestProxy(p as never);
      return {
        ok: result?.ok ?? false,
        latencyMs: result?.latencyMs ?? 0,
        category: result?.category ?? '',
        note: result?.note ?? '',
      };
    } catch (err) {
      // 配置本身非法（未知 scheme / 缺主机）时后端返回 error，这里统一成 AppError，
      // 由 View 显示在表单下方（与「测不通」区分：那是配置错，不是网络问题）。
      throw toAppError(err);
    }
  },
} as const;
