// SpeakerService：说话人分离 / 角色管理的后端封装（FR-06 界面侧）。
//
// 后端：
//   internal/services/speakersvc（API-7.x）
//     HasDiarization / AnalyzeSession / ListSessionSpeakers / ListRoles /
//     RenameRole / MergeSpeakers / SplitSpeaker / AssignSpeaker
//
// 语义提醒（与后端 internal/speaker 保持一致）：
//   - 角色名**不冗余存在片段里**，由 roles JOIN 得到；改名是单行 UPDATE，历史会话自动同步。
//     所以前端收到 roles:changed 后必须**重新拉取**，不要拿旧名字做局部替换。
//   - 「未识别」是内置角色（contract.RoleUnknownID = rl_unknown），不是普通角色：
//     它的 sessionSpeaker 行 speakerId 为空（片段本来就没有声纹身份），
//     因此**不能**对它指派角色 / 拆分，只能重跑分离。
//   - 合并 / 指派 / 拆分操作的粒度是**声纹身份（speakerId）**，身份是跨会话的：
//     合并后不可自动还原（FR-06-5），UI 必须二次确认。
//   - HasDiarization 为 false 不是错误：本机没装声纹模型时角色管理照常可用，
//     只有「发起分离」不可用（后端会回 ERR_LOCAL_UNAVAILABLE / ERR_MODEL_NOT_FOUND）。
import { Events } from '@wailsio/runtime';
import { Service as Binding } from '@bindings/vip.oberon.mandaryn/internal/services/speakersvc';
import type {
  DiarizeResult,
  Role,
  RoleChanged,
  SessionSpeaker,
} from '@bindings/vip.oberon.mandaryn/internal/contract/models';
import { AppEvents } from '@/shared/events';
import { toAppError } from '../errors/AppError';

// 契约类型原样 re-export（页面不手写重复类型）。
export type {
  DiarizeResult,
  Role,
  RoleChanged,
  SessionSpeaker,
} from '@bindings/vip.oberon.mandaryn/internal/contract/models';

// 内置「未识别」角色 ID（= internal/contract.RoleUnknownID）。
// 判定统一走 roleIsUnidentified()，调用方不要各自硬编码字符串。
export const ROLE_UNKNOWN_ID = 'rl_unknown';

// 角色是否为内置「未识别」角色（后端 contract.RoleIsUnidentified 的前端镜像）。
export const roleIsUnidentified = (roleId: string): boolean => roleId === ROLE_UNKNOWN_ID;

// 角色配色令牌 → 该令牌是否是前端可识别的主题令牌。
// 后端只存令牌（role-1..role-8 / neutral / accent），颜色由样式层映射到 --fd-*，
// 这里只用于给未知令牌兜底，**不**参与取色（禁止在前端拼色值）。
export const isKnownRoleColorToken = (token: string): boolean =>
  token === 'accent' || token === 'neutral' || /^role-[1-8]$/.test(token);

export const SpeakerService = {
  // 本机是否具备声纹分离能力（未装声纹模型时为 false，不是错误）。
  async hasDiarization(): Promise<boolean> {
    try {
      return await Binding.HasDiarization();
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 对会话跑一次说话人分离（重跑幂等覆盖）。同一会话同时只允许一个分离任务
  // （并发时后端回 ERR_STATE_FORBIDDEN）。
  async analyzeSession(sessionId: string): Promise<DiarizeResult> {
    try {
      return await Binding.AnalyzeSession(sessionId);
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 会话内说话人 → 角色映射；未识别片段作为合成行附在末尾（speakerId 为空）。
  async listSessionSpeakers(sessionId: string): Promise<SessionSpeaker[]> {
    try {
      return await Binding.ListSessionSpeakers(sessionId);
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 全局角色列表（含颜色令牌 / 覆盖的声纹身份 / 命中会话数）。
  async listRoles(): Promise<Role[]> {
    try {
      return await Binding.ListRoles();
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 角色改名，**全局立即生效**（历史会话靠 JOIN 同步）。
  async renameRole(roleId: string, name: string): Promise<void> {
    try {
      await Binding.RenameRole(roleId, name);
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 合并两个声纹身份：片段改指向、质心融合、冗余计数重算。**不可自动还原**。
  async mergeSpeakers(fromSpeakerId: string, toSpeakerId: string): Promise<void> {
    try {
      await Binding.MergeSpeakers(fromSpeakerId, toSpeakerId);
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 从某说话人拆出指定片段，生成新说话人与新角色；返回新建的角色。
  async splitSpeaker(sessionId: string, speakerId: string, segmentIds: number[]): Promise<Role> {
    try {
      return await Binding.SplitSpeaker(sessionId, speakerId, segmentIds);
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 把某个声纹身份指派到指定角色（只改会话内映射，不动声纹身份与片段归属）。
  async assignSpeaker(sessionId: string, speakerId: string, roleId: string): Promise<void> {
    try {
      await Binding.AssignSpeaker(sessionId, speakerId, roleId);
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 订阅角色变更（EV-07 roles:changed）。返回取消订阅函数，调用方必须在 unmount 时调用。
  onRolesChanged(callback: (changed: RoleChanged) => void): () => void {
    const cancel = Events.On(AppEvents.RolesChanged, (event) => {
      const payload = event.data as RoleChanged | null;
      if (payload) callback(payload);
    });
    return () => {
      if (typeof cancel === 'function') cancel();
    };
  },
} as const;
