// EngineService：模型来源（本地 / 在线）切换、热重载、端点配置与连通性测试的前端封装。
//
// 后端 internal/services/enginesvc（API-5.1~5.7）：
//   GetStatus / SetSource / Reload / TestOnline / GetOnlineConfig /
//   SetOnlineConfig / SetFallbackPolicy
//
// 铁律提醒（FR-05 / ARCH §4.3）：在线故障可自动回退本地（降级）；
// 本地故障**不得**自动回退在线 —— 因此本页只暴露能力，不做任何隐式切换。
import { Events } from '@wailsio/runtime';
import { Service as SettingsBinding } from '@bindings/vip.oberon.mandaryn/internal/services/appsettings';
import { Service as Binding } from '@bindings/vip.oberon.mandaryn/internal/services/enginesvc';
import type {
  EngineStatus,
  FallbackPolicy,
  OnlineConfig,
  TestResult,
} from '@bindings/vip.oberon.mandaryn/internal/contract/models';
import { AppEvents } from '@/shared/events';
import { toAppError } from '../errors/AppError';

// 契约类型原样 re-export（页面不手写重复类型）。
export type {
  EngineStatus,
  FallbackPolicy,
  OnlineConfig,
  TestResult,
} from '@bindings/vip.oberon.mandaryn/internal/contract/models';

// ModelSource 是 contract.EngineStatus.source 的两个取值。
export type ModelSource = 'local' | 'online';

export const EngineService = {
  // 引擎状态（ready / loading / failed / degraded + 详情）。
  async getStatus(): Promise<EngineStatus> {
    try {
      return await Binding.GetStatus();
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 切换来源（local | online）。后端先热切换成功才落库，失败时来源保持不变。
  async setSource(source: ModelSource): Promise<EngineStatus> {
    try {
      return await Binding.SetSource(source);
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 强制重载当前引擎（换模型包 / 改端点后用）。
  async reload(): Promise<EngineStatus> {
    try {
      return await Binding.Reload();
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 读取在线端点配置（apiKey 为掩码）。
  async getOnlineConfig(): Promise<OnlineConfig> {
    try {
      return await Binding.GetOnlineConfig();
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 读持久化的降级策略（包括界面未编辑的重试次数）和端点授权状态。
  async getOnlineSettings() {
    try {
      const snap = await SettingsBinding.Get();
      return {
        consentAt: snap.onlineConsentAt,
        consentHost: snap.onlineConsentHost,
        policy: {
          fallbackToLocal: snap.fallbackToLocal,
          autoSwitchBack: snap.autoSwitchBack,
          maxRetries: snap.maxRetries,
        } satisfies FallbackPolicy,
      };
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 仅在用户显式同意当前端点后记录授权；此 API 不发出站请求。
  async setConsentOnline(endpoint: string): Promise<void> {
    try {
      await SettingsBinding.SetConsentOnline(endpoint);
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 保存在线端点（需已对当前域名做过隐私确认）。
  async setOnlineConfig(config: OnlineConfig): Promise<void> {
    try {
      await Binding.SetOnlineConfig(config);
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 连通性 + 延迟 + 错误分类测试；MANDARYN_OFFLINE=1 时返回 ERR_OFFLINE_ENFORCED。
  async testOnline(config: OnlineConfig): Promise<TestResult> {
    try {
      return await Binding.TestOnline(config);
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 保存降级策略并同步给运行中的路由；重试次数也会持久化。
  async setFallbackPolicy(policy: FallbackPolicy): Promise<void> {
    try {
      await Binding.SetFallbackPolicy(policy);
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 订阅引擎状态变化（EV-08 engine:status）。返回取消订阅函数。
  onStatusChanged(callback: (status: EngineStatus) => void): () => void {
    const cancel = Events.On(AppEvents.EngineStatus, (event) => {
      const payload = event.data as EngineStatus | null;
      if (payload) callback(payload);
    });
    return () => {
      if (typeof cancel === 'function') cancel();
    };
  },
} as const;
