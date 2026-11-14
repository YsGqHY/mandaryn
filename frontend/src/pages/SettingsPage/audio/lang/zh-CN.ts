import type { Messages } from '@/i18n';

// 音频子页面文案（中文）。namespace: settings.audio.*
export const audioZhCN: Messages = {
  settings: {
    audio: {
      title: '音频与录制',
      hint: '录制默认只做采集与落盘，转写是录制结束后的独立任务。这里的开关会让录制中额外跑一遍推理，因此默认关闭。',
      live: {
        title: '边录边转',
        desc: '录制过程中实时出字：partial 会被不断覆盖重写，一句话结束后定稿并不再变化。',
        stateOn: '已开启',
        stateOff: '已关闭',
        switchLabel: '开启边录边转（录制中实时出字）',
        noteDefault: '默认关闭：不开也能正常录制，录完再从会话库发起转写。',
        noteNextSession: '只在下一个会话生效：录制中无法切换，请先停止录制。',
        noteDeferred: '会额外占用 CPU：边录边转需要常主流式模型，整机忙碌时可能自动降级（关闭二次校正）。',
        capLabel: '流式能力',
        capReady: '可用（{{engine}}）',
        capReadyUnknown: '流式模型',
        capChecking: '正在检查…',
        capUnavailable: '不可用',
        capErrors: {
          ERR_LOCAL_UNAVAILABLE: '本构建不含流式引擎，边录边转不可用。',
          ERR_MODEL_NOT_FOUND: '缺少流式模型包：请先在「模型」页下载流式中文模型。',
          ERR_MODEL_CORRUPT: '流式模型包已损坏，请重新下载。',
          ERR_ENGINE_LOAD: '流式引擎加载失败，请查看诊断信息。',
        },
        enabledButUnavailable: '开关已打开，但当前没有可用的流式模型，录制的这一场不会实时出字。',
      },
      feedback: {
        saving: '正在保存…',
        saved: '已保存，将在下一个会话生效。',
        ERR_STATE_FORBIDDEN: '录制中不能切换边录边转。请先停止录制，再设置（只对下一个会话生效）。',
      },
      errors: {
        ERR_DB_WRITE: '保存失败：数据库写入出错。',
      },
    },
  },
};
