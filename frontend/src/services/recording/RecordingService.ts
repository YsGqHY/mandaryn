// RecordingService：录制状态机（开始 / 暂停 / 继续 / 停止）、电平与节拍事件、
// 崩溃会话修补、录音另存的前端封装。
//
// 后端 internal/services/recordingsvc（API-2.1~2.7）：
//   Start / Pause / Resume / Stop / GetState / RecoverSession / ExportAudio
//
// 语义提醒（FR-03 铁律）：
//   - Stop 只封存音频与元数据，**不做转写**；转写是 transcriptsvc 的独立任务；
//   - 录制中切换模型来源会被后端拒绝（ERR_STATE_FORBIDDEN），前端负责禁用控件；
//   - 状态只有 `state`（ST-01~ST-09 会出现在该字段上的取值），degraded / throttled /
//     suspended 是正交布尔标记，**不是**状态值（禁止出现 ST-07 这类字符串）。
import { Events } from '@wailsio/runtime';
import { Service as Binding } from '@bindings/vip.oberon.mandaryn/internal/services/recordingsvc';
import type {
  RecordingLevel,
  RecordingState,
  RecordingTick,
  Session,
  StartRequest,
} from '@bindings/vip.oberon.mandaryn/internal/contract/models';
import { AppEvents } from '@/shared/events';
import { toAppError } from '../errors/AppError';

// 契约类型原样 re-export（页面不手写重复类型）。
export type {
  RecordingLevel,
  RecordingState,
  RecordingTick,
  Session,
  StartRequest,
} from '@bindings/vip.oberon.mandaryn/internal/contract/models';

// 录制主状态取值：与 internal/contract/recording.go 的常量一一对应。
export const RecordingStates = {
  Idle: 'idle', // ST-01
  Preparing: 'preparing', // ST-02
  Recording: 'recording', // ST-03
  Paused: 'paused', // ST-04
  Finalizing: 'finalizing', // ST-05
  Completed: 'completed', // ST-06
  Interrupted: 'interrupted', // ST-08
  Failed: 'failed', // ST-09
} as const;

export type RecordingStateValue = (typeof RecordingStates)[keyof typeof RecordingStates];

// 有活动会话（含准备与收尾）：此时禁止切换模型来源 / 音频源。
export const isRecordingActive = (state: string): boolean =>
  state === RecordingStates.Preparing ||
  state === RecordingStates.Recording ||
  state === RecordingStates.Paused ||
  state === RecordingStates.Finalizing;

// 收尾已经结束、会话已落库：此时才能另存录音文件 / 发起转写。
export const isRecordingSettled = (state: string): boolean =>
  state === RecordingStates.Completed ||
  state === RecordingStates.Interrupted ||
  state === RecordingStates.Failed;

// 静音阈值（FR-03-7）：后端 silentMs 连续超过该值即提示「未检测到音频」。
export const SILENCE_WARN_MS = 10_000;

export const RecordingService = {
  // 开始录制（API-2.1）：ST-01 → ST-02 → ST-03。
  async start(request: StartRequest): Promise<RecordingState> {
    try {
      return await Binding.Start(request);
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 暂停（API-2.2）：ST-03 → ST-04。
  async pause(): Promise<RecordingState> {
    try {
      return await Binding.Pause();
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 继续（API-2.3）：ST-04 → ST-03。
  async resume(): Promise<RecordingState> {
    try {
      return await Binding.Resume();
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 停止并封存（API-2.4）：→ ST-05 → ST-06 / ST-08 / ST-09。只封存、不转写。
  async stop(): Promise<RecordingState> {
    try {
      return await Binding.Stop();
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 幂等查询当前状态（API-2.5）：窗口重新打开后靠它恢复 UI。
  async getState(): Promise<RecordingState> {
    try {
      return await Binding.GetState();
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 把未正常结束的会话修补为可回放（API-2.6 / UC-07）。
  async recoverSession(sessionId: string): Promise<Session> {
    try {
      return await Binding.RecoverSession(sessionId);
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 另存录音实例为单个音频文件（API-2.7）。**不改库**，会话仍在。
  // targetPath 必须来自前端 NativeDialogs 的原生「另存为」（后端不弹 UI）。
  async exportAudio(sessionId: string, targetPath: string): Promise<string> {
    try {
      return await Binding.ExportAudio(sessionId, targetPath);
    } catch (err) {
      throw toAppError(err);
    }
  },

  // 订阅状态机转换（EV-01 recording:state）。返回取消订阅函数。
  onState(callback: (state: RecordingState) => void): () => void {
    const cancel = Events.On(AppEvents.RecordingState, (event) => {
      const payload = event.data as RecordingState | null;
      if (payload) callback(payload);
    });
    return () => {
      if (typeof cancel === 'function') cancel();
    };
  },

  // 订阅采集电平（EV-02 recording:level，后端 10Hz 节流）。返回取消订阅函数。
  onLevel(callback: (level: RecordingLevel) => void): () => void {
    const cancel = Events.On(AppEvents.RecordingLevel, (event) => {
      const payload = event.data as RecordingLevel | null;
      if (payload) callback(payload);
    });
    return () => {
      if (typeof cancel === 'function') cancel();
    };
  },

  // 订阅采集节拍（EV-03 recording:tick，1Hz）。返回取消订阅函数。
  onTick(callback: (tick: RecordingTick) => void): () => void {
    const cancel = Events.On(AppEvents.RecordingTick, (event) => {
      const payload = event.data as RecordingTick | null;
      if (payload) callback(payload);
    });
    return () => {
      if (typeof cancel === 'function') cancel();
    };
  },
} as const;
