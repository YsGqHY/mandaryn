import type { Messages } from '@/i18n';

// 音频子页面文案（中文）。namespace: settings.audio.*
export const audioZhCN: Messages = {
  settings: {
    audio: {
      title: '音频与录制',
      hint: '选择默认来源、说话人分析上限与新会话命名方式。录制默认只采集并落盘，转写是独立任务。',
      defaults: {
        source: '默认音频源',
        sourceHint: '录制页选择与录制快捷键共用此来源；当前录制不会切换来源。',
        noSource: '请选择音频源',
        clearSource: '清空默认音频源',
        noSources: '没有可用音频源',
        unavailableOption: '已保存的音频源（暂不可用）',
        unavailable: '默认音频源当前不可用，请重新选择；不会自动改录其它来源。',
        sourcesFailed: '无法读取音频源，请刷新重试。',
        refresh: '刷新音频源',
        sensitivity: '分句灵敏度',
        sensitivityHint: '只对下次录制生效。嘈杂模式要求更高的语音电平，可能漏掉小声说话；两种模式都不改原始录音。',
        standard: '标准',
        noisy: '嘈杂',
        maxSpeakers: '说话人上限',
        maxSpeakersHint: '对下一次发起的说话人分析生效，不改动已有识别结果。',
        auto: '自动',
        speakers: '{{count}} 人',
        autoName: '会话自动命名',
        autoNameHint: '新会话使用“日期 时间 · 来源应用”。关闭后新会话保持未命名；已有名称不变。',
        format: '录制保存格式',
        formatHint: 'WAV 兼容性更广，FLAC 无损压缩。设置只对下次录制生效。',
      },
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
        saved: '已保存。',
        ERR_STATE_FORBIDDEN: '录制中不能切换边录边转。请先停止录制，再设置（只对下一个会话生效）。',
      },
      errors: {
        ERR_DB_WRITE: '保存失败：数据库写入出错。',
      },
    },
  },
};
