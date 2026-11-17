import type { Messages } from '@/i18n';

// RecordPage 中文文案。挂在 record.* 命名空间下。
// 由 src/pages/RecordPage/lang/index.ts 通过 localeRegistry.extend('zh-CN', ...) 注入。
export const recordPageZhCN: Messages = {
  record: {
    eyebrow: '录制',
    title: '选择要转写的音频源',
    subtitle: '按应用分路捕获正在播放的中文语音。录制只做采集与落盘，转写是会话库里的独立任务，可稍后再发起。',

    caps: {
      title: '平台捕获能力',
      loading: '正在读取平台能力…',
      processOn: '支持按应用分路捕获：只录制选中应用的声音。',
      processOff: '当前平台或系统版本不支持按应用分路捕获，只能录制整机混音。',
      reasonLabel: '原因',
      deviceOn: '设备源为回环录制，会包含所有应用的声音。',
      deviceOff: '当前平台没有可用的回环设备。',
      permission: '需要先授予系统音频权限，才能捕获应用声音。',
      platformLabel: '平台',
    },

    // 录制主状态（ST-01~ST-09 中会出现在 state 字段上的取值）。
    // ST-07（degraded）不在此列：它是正交布尔标记，在「模型来源」区以 chip 展示。
    state: {
      idle: '空闲',
      preparing: '准备中',
      recording: '录制中',
      paused: '已暂停',
      finalizing: '封存中',
      completed: '已完成',
      interrupted: '异常中断',
      failed: '已失败',
    },

    controls: {
      title: '录制控制',
      hint: '按钮可用性跟随录制状态机：空闲只能开始，录制中可暂停 / 停止，暂停后可继续或停止。',
      start: '开始录制',
      startHint: '先在上方选择一个可用的音频源，再开始录制。',
      pause: '暂停',
      resume: '继续',
      stop: '停止',
      exportAudio: '另存录音文件',
      recover: '修补会话',
    },

    metrics: {
      levelLabel: '电平',
      levelAria: '当前采集电平',
      peakLabel: '峰值 {{percent}}%',
      elapsedLabel: '已录时长',
      bytesLabel: '已写入',
      sourceLabel: '录制源',
      // 连续静音超过 10s（后端 silentMs），只提示不改按钮状态。
      silentHint: '已连续 {{seconds}} 秒未检测到音频，请确认该应用正在发声。',
    },

    model: {
      title: '模型来源',
      hint: '录制前可切换；录制中锁定，切换后端会拒绝。',
      lockedHint: '录制进行中不能切换模型来源，请先停止录制。',
      local: '本地',
      online: '在线',
      aria: '选择模型来源',
      loading: '正在读取引擎状态…',
      degraded: '已降级到本地',
      throttled: '已降档 {{level}}',
      suspended: '推理已暂停',
      lagging: '转写落后',
      failedHint: '推理引擎加载失败，暂时无法转写；录制不受影响。',
      state: {
        loading: '引擎加载中',
        ready: '引擎就绪',
        failed: '引擎不可用',
        degraded: '引擎降级',
        paused: '引擎已暂停',
      },
    },

    sources: {
      title: '音频源',
      refresh: '刷新源列表',
      refreshing: '正在刷新音频源…',
      groupApplications: '应用',
      groupDevices: '设备',
      emptyApplications: '当前没有正在播放声音的应用。',
      emptyDevices: '没有可用的回环设备。',
      present: '可用',
      absent: '未运行',
      producing: '正在发声',
      childCount: '含 {{count}} 个子进程',
      defaultDevice: '默认设备',
      absentTooltip: '该源当前不可用（进程已退出或设备已断开），无法选择。',
      selected: '已选择',
      selectAria: '选择音频源：{{name}}',
      unavailableBanner: '当前选择的源「{{name}}」已不可用，请重新选择。',
      unknownUnavailable: '上次选择的源已不可用，请重新选择。',
      loadFailed: '读取音频源失败。',
      warn: {
        'exclusive-mode': '该源可能被独占模式占用，录制时听不到声音。',
        'process-loopback-unavailable': '本机不支持按应用分路捕获，该源无法录制；请改用设备源（会包含全部应用声音）。',
      },
      lockedHint: '录制进行中不能切换音频源。',
      lockedTooltip: '录制进行中，音频源已绑定到当前会话；如需更换请先停止录制。',
    },

    export: {
      dialogTitle: '另存录音文件',
      fileBaseName: 'recording',
      filterWav: 'WAV 音频 (*.wav)',
      doneTitle: '已另存录音文件',
      doneMessage: '录音已保存到：\n{{path}}',
    },

    // 边录边转（FR-03-10，**默认关闭**）：开关关闭时整块不渲染。
    live: {
      title: '边录边转',
      stateOn: '实时出字中',
      stateUnavailable: '不可用',
      idleHint: '开始录制后这里会实时显示识别出的文字。',
      waiting: '正在等待语音…',
      capErrors: {
        ERR_LOCAL_UNAVAILABLE: '本构建不含流式引擎，边录边转不可用。',
        ERR_MODEL_NOT_FOUND: '缺少流式模型包：请先在「模型」页下载流式中文模型。',
        ERR_MODEL_CORRUPT: '流式模型包已损坏，请重新下载。',
        ERR_ENGINE_LOAD: '流式引擎加载失败，本次录制不会实时出字（录音不受影响）。',
      },
    },

    // 错误文案按后端错误码（internal/apierr）映射；命中不到时回落 errors.unknown。
    errors: {
      unknown: '操作失败：{{message}}',
      dismiss: '关闭提示',
      ERR_CAPTURE_PERMISSION: '缺少系统音频权限，请在系统设置中授予本应用录音权限后重试。',
      ERR_CAPTURE_UNAVAILABLE: '当前系统的音频捕获服务不可用，请检查系统音频设备后重试。',
      ERR_CAPTURE_DEVICE_LOST: '音频设备已断开，请连接设备后刷新列表。',
      ERR_SOURCE_NOT_FOUND: '该音频源已不存在，请刷新列表后重新选择。',
      ERR_INVALID_ARGUMENT: '请求参数不合法，请刷新列表后重试。',
      ERR_OFFLINE_ENFORCED: '已开启离线模式，该操作不会发起任何网络请求。',
      ERR_PATH_UNWRITABLE: '目标路径不可写，请选择其他位置。',
      ERR_STATE_FORBIDDEN: '当前状态下不允许该操作，请检查录制状态后重试。',
      emptyRecording: '这次没有录到声音（时长不足 1 秒或全程静音），会话已保留在会话库。请确认所选音频源正在发声后重新录制。',
      ERR_DISK_FULL: '磁盘空间不足，录制已停止。请清理空间后重新开始。',
      ERR_DB_WRITE: '写入数据库失败，请稍后重试。',
      ERR_NOT_FOUND: '找不到对应的会话，可能已被删除。',
      ERR_ENGINE_NOT_READY: '推理引擎尚未就绪，请稍后重试。',
      ERR_MODEL_NOT_FOUND: '当前模型不可用，请到「模型」页下载或切换。',
    },
  },
};
