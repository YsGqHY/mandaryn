// LiveTranscriptService：边录边转（FR-03-10，**默认关闭**）的前端封装。
//
// 后端：internal/services/transcriptsvc
//   API-4.8 LiveAvailability —— 能力探测（**便宜**：不加载模型）
//   事件 EV-04 transcript:partial（partial，不落库）
//        EV-05 transcript:segment（定稿）
//
// 三个概念要分清，页面才能给出正确提示：
//   1. **开关**（app_settings.live_transcribe）：用户的意愿，默认 false；
//   2. **能力**（LiveAvailability）：本机有没有流式模型包 / 本构建含不含流式引擎；
//   3. **状态**（recording:state）：开关与能力都满足、且正在录制时才会真正出字。
//
// 因此「开关开着但没字」有三种原因：没在录、缺流式模型包、录制中切换被拒。
// 页面必须把它们分开表达，不能笼统说「未开启」。
import { Events } from '@wailsio/runtime';
import { Service as Binding } from '@bindings/vip.oberon.mandaryn/internal/services/transcriptsvc';
import type { Segment } from '@bindings/vip.oberon.mandaryn/internal/contract/models';
import { AppEvents } from '@/shared/events';
import { toAppError } from '../errors/AppError';

// 契约类型原样 re-export（页面不手写重复类型）。
export type { Segment } from '@bindings/vip.oberon.mandaryn/internal/contract/models';

// LiveAvailability 对应 internal/contract/live.go 的 contract.LiveAvailability。
//
// 手写类型而不是等绑定：该类型新增于 Wave F-2，绑定重新生成前也必须有明确结构，
// 否则页面的「不可用原因」会退化成 any。
export interface LiveAvailability {
  // available 能力当前可用（与开关值无关）。
  available: boolean;
  // engineId 实际会用哪个流式引擎（未加载时为空）。
  engineId: string;
  // errorCode 不可用原因（ERR_LOCAL_UNAVAILABLE / ERR_MODEL_NOT_FOUND / ...）。
  errorCode: string;
  // reason 面向用户的中文短说明。
  reason: string;
}

// 能力不可用时的兜底（后端方法缺失 / 调用失败）。
const UNAVAILABLE: LiveAvailability = {
  available: false,
  engineId: '',
  errorCode: 'ERR_LOCAL_UNAVAILABLE',
  reason: '',
};

export const LiveTranscriptService = {
  // 查询边录边转是否可用（不加载模型，可随页面挂载调用）。
  async getAvailability(): Promise<LiveAvailability> {
    const ext = Binding as unknown as {
      LiveAvailability?: () => Promise<LiveAvailability>;
    };
    if (typeof ext.LiveAvailability !== 'function') return UNAVAILABLE;
    try {
      const got = await ext.LiveAvailability();
      return {
        available: got?.available ?? false,
        engineId: got?.engineId ?? '',
        errorCode: got?.errorCode ?? '',
        reason: got?.reason ?? '',
      };
    } catch (err) {
      // 探测失败按「不可用」处理：它只影响一个可选功能，不该把录制页打成错误态。
      throw toAppError(err);
    }
  },

  // 订阅流式 partial（EV-04）。返回取消订阅函数。
  //
  // partial 是**累积文本**（同一句会被反复覆盖），页面直接整体替换显示即可，
  // 不要追加 —— 那会显示成「同一句重复很多遍」。
  onPartial(callback: (segment: Segment) => void): () => void {
    const cancel = Events.On(AppEvents.TranscriptPartial, (event) => {
      const payload = event.data as Segment | null;
      if (payload) callback(payload);
    });
    return () => {
      if (typeof cancel === 'function') cancel();
    };
  },

  // 订阅逐句定稿（EV-05）：边录边转与延后转写都会发。
  onSegment(callback: (segment: Segment) => void): () => void {
    const cancel = Events.On(AppEvents.TranscriptSegment, (event) => {
      const payload = event.data as Segment | null;
      if (payload) callback(payload);
    });
    return () => {
      if (typeof cancel === 'function') cancel();
    };
  },
} as const;
