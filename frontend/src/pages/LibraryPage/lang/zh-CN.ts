import type { Messages } from '@/i18n';

// LibraryPage 中文文案。挂在 library.* 命名空间下。
// 由 src/pages/LibraryPage/lang/index.ts 通过 localeRegistry.extend('zh-CN', ...) 注入。
export const libraryPageZhCN: Messages = {
  library: {
    eyebrow: '会话库',
    title: '已录制的会话',
    subtitle: '按应用分好的录音实例都在这里：可搜索、改名、导出文本，也可以对还没转写的录音发起转写。',

    search: {
      title: '搜索与筛选',
      reset: '重置条件',
      refresh: '刷新列表',
      keywordLabel: '关键词',
      keywordPlaceholder: '搜索标题或转写正文',
      keywordAria: '按关键词搜索会话',
      fromLabel: '起始日期',
      fromAria: '筛选开始日期不早于',
      toLabel: '结束日期',
      toAria: '筛选开始日期不晚于',
      sourceLabel: '来源应用',
      sourcePlaceholder: '全部应用',
      sourceAria: '按来源应用筛选',
      statusLabel: '转写状态',
      statusPlaceholder: '全部状态',
      statusAria: '按转写状态筛选',
      minDurationLabel: '最短时长',
      minDurationAria: '筛选时长不短于（秒）',
      maxDurationLabel: '最长时长',
      maxDurationAria: '筛选时长不长于（秒）',
      secondsSuffix: '秒',
      // 后端 ListQuery 没有时长条件：如实说明只作用于当前页。
      durationHint: '时长区间只在当前页内筛选（列表接口暂不支持按时长查询）。',
    },

    list: {
      title: '会话列表',
      countLabel: '共 {{count}} 条',
      durationFiltered: '本页命中 {{shown}} 条',
      pageSizeAria: '每页条数',
      pageSizeOption: '每页 {{count}} 条',
      emptyTitle: '还没有录制过会话',
      emptyDescription: '到「录制」页选择一个应用音频源开始录制，会话结束后会出现在这里。',
      emptyFilteredTitle: '没有符合条件的会话',
      emptyFilteredDescription: '试试放宽时间范围、缩短关键词，或重置筛选条件。',
      degraded: '已降级',
      online: '在线模型',
      sourceLabel: '来源',
      segmentLabel: '{{count}} 句',
      progressAria: '转写进度',
      progressText: '已转写 {{percent}}%',
      startTranscription: '发起转写',
      retryTranscription: '重试转写',
      cancelTranscription: '取消转写',
      export: '导出文本',
      rename: '重命名',
      delete: '删除会话',
      speakers: '说话人与角色',
      playback: '回放',
      openTranscript: '查看转写文本',
      noAudio: '未保存音频',
      noAudioRetry: '未保存音频，无法转写或重试',
    },

    // 转写状态（独立轴，与会话录制状态正交）
    transcription: {
      none: '未转写',
      queued: '排队中',
      running: '转写中',
      done: '已转写',
      failed: '转写失败',
    },

    rename: {
      title: '重命名会话',
      label: '标题',
      placeholder: '输入新的会话标题',
      cancel: '取消',
      submit: '保存',
    },

    delete: {
      title: '删除会话',
      message: '将删除会话「{{title}}」及其转写文本，此操作不可撤销。',
      alsoAudio: '同时删除音频文件',
      alsoAudioHint: '取消勾选则只删记录，音频文件保留在磁盘上。',
      cancel: '取消',
      submit: '删除',
      confirmTitle: '确认删除',
      confirmMessage: '删除后无法恢复，确定要删除「{{title}}」吗？',
      confirmOk: '删除',
      confirmCancel: '取消',
      audioLeftTitle: '记录已删除',
      audioLeftMessage: '有 {{count}} 个音频文件未能删除，仍留在磁盘上，可稍后手动清理。',
      doneTitle: '已删除',
      doneMessage: '已删除 {{count}} 个会话，其中 {{audio}} 个音频文件已一并删除。',
    },

    export: {
      dialogTitle: '导出会话文本',
      filterTxt: '纯文本 (*.txt)',
      filterSrt: '字幕 (*.srt)',
      filterCsv: 'CSV (*.csv)',
      filterXlsx: 'Excel (*.xlsx)',
      formatTxt: '导出为纯文本 (.txt)',
      formatSrt: '导出为字幕 (.srt)',
      formatCsv: '导出为 CSV (.csv)',
      formatXlsx: '导出为 Excel (.xlsx)',
      doneTitle: '导出完成',
      doneMessage: '文本已导出到：\n{{path}}',
    },

    // 会话详情的「说话人 / 角色」侧栏（FR-06 界面侧）
    speakers: {
      title: '说话人 / 角色',
      subtitle: '角色改名对所有会话生效；合并与拆分动的是声纹身份本身。',
      close: '关闭说话人侧栏',
      refresh: '刷新说话人',
      loading: '正在读取说话人…',

      analyze: {
        title: '说话人分离',
        hint: '按声纹把片段归到不同说话人，可重复执行（覆盖上一次结果）。',
        run: '发起分离',
        rerun: '重新分离',
        running: '正在分离…',
        noModelTitle: '无法发起分离',
        noModel: '本机未安装声纹模型，请先到「模型」页下载后再发起分离。角色改名与指派仍然可用。',
        resultTitle: '分离完成',
        resultDismiss: '收起分离结果',
        resultSummary: '{{speakers}} 个说话人 · {{assigned}} 句已归属 · {{unidentified}} 句未识别',
        resultDetail: '共 {{segments}} 句参与；过短跳过 {{tooShort}} 句，读取失败 {{failed}} 句。',
        modelLabel: '声纹模型：{{model}}',
      },

      identified: {
        title: '已识别说话人',
        empty: '这个会话还没有分出说话人。先转写录音，再发起分离。',
      },

      rows: {
        unnamed: '未命名角色',
        count: '{{count}} 句',
        speech: '语音 {{duration}}',
        rename: '改角色名',
        split: '拆出片段',
        assign: '指派到角色',
        merge: '合并说话人',
        mergeInto: '把此人合并到…',
        nameLabel: '角色名',
        namePlaceholder: '给这个说话人起个名字',
        save: '保存',
        cancel: '取消',
      },

      unidentified: {
        title: '未识别语音',
        description:
          '这些片段没有可用的声纹身份（重叠语音、过短或提取失败），因此不能指派或拆分；需要改善请重新发起分离。',
      },

      merge: {
        title: '合并两个说话人',
        description:
          '合并后：源说话人的所有片段会归属到目标说话人，声纹向量按权重融合，源声纹身份被删除。',
        irreversible: '合并后不可自动还原（只能靠重新指派或重新分离找回）。',
        fromLabel: '被合并的说话人',
        toLabel: '合并到',
        placeholder: '选择说话人',
        needTwo: '至少需要两个已识别的说话人才能合并。',
        cancel: '取消',
        submit: '合并',
        confirmTitle: '确认合并说话人',
        confirmMessage: '将把「{{from}}」合并到「{{to}}」，源说话人会被永久删除，此操作不可自动还原。确定继续吗？',
        confirmOk: '合并',
        confirmCancel: '取消',
        doneTitle: '已合并',
        doneMessage: '说话人已合并，相关片段与历史会话已同步。',
      },

      split: {
        title: '从「{{name}}」拆出片段',
        description: '勾选要拆到新说话人的片段；新说话人会自动获得一个新角色，其余片段留在原说话人。',
        note: '拆分只影响本会话，且不保留片段的声纹向量 —— 下次重新分离时它们可能再次被聚到一起。',
        loading: '正在读取片段…',
        empty: '这个说话人在本会话里没有可拆分的片段。',
        emptyText: '（无文本）',
        segmentAria: '选择第 {{seq}} 句',
        selected: '已选 {{count}} 句',
        cancel: '取消',
        submit: '拆分',
      },

      assign: {
        title: '指派「{{name}}」到角色',
        description: '只改这个会话里该说话人的角色映射，声纹身份与片段归属都不变。',
        roleLabel: '目标角色',
        placeholder: '选择已有角色',
        empty: '还没有可指派的角色，先把某个说话人改名成一个角色吧。',
        cancel: '取消',
        submit: '指派',
      },
    },

    // 会话回放与歌词式同步（FR-04 界面侧）
    playback: {
      close: '关闭回放',
      closeTranscript: '关闭转写文本',
      loading: '正在读取时间轴…',
      retry: '重试',
      titleMeta: '{{time}} · {{source}} · {{count}} 句',
      lyricsAria: '转写片段列表',

      // —— 播放器控件 ——
      play: '播放',
      pause: '暂停',
      back5: '后退 5 秒',
      forward5: '前进 5 秒',
      prevSegment: '上一句',
      nextSegment: '下一句',
      progressAria: '播放进度，可拖动定位',
      positionAria: '播放位置 {{position}} / {{duration}}',
      rateAria: '播放倍速：{{rate}}',
      rateValue: '{{rate}}×',
      volumeAria: '音量',
      volumeValue: '音量 {{percent}}%',
      mute: '静音',
      unmute: '取消静音',

      // —— 片段 ——
      unknownSpeaker: '未识别',
      unfinalized: '识别中',
      segmentRange: '{{start}} – {{end}}',
      backToActive: '回到当前句',

      // —— 三态（空 / 加载 / 错误） ——
      emptyTitle: '这个会话还没有转写文本',
      emptyDescription:
        '回放需要先转写：关闭回放后，在会话列表里对这一行点「发起转写」，完成后即可按句回放。',
      emptyTranscribing: '正在转写，完成后这里会按句显示；音频现在就可以听。',
      emptyFailed: '上一次转写失败了。关闭回放后在会话列表里点「重试转写」再回来。',

      emptyNoAudio: '这场会话没有可读的转写文本。',
      emptyFailedNoAudio: '转写失败，没有可用音频，无法重试。',
      audioNotSaved: {
        title: '未保存音频',
        message: '可以在此阅读转写，在会话列表中导出文本。无音频可供回放、导出或重新转写。',
      },
      audioMissing: {
        title: '音频文件缺失',
        message: '这个会话的音频已经不在数据目录里（可能被手动清理）。转写文本仍可阅读，但无法回放。',
      },
      audioError: {
        title: '音频加载失败',
        message: '无法读取音频流：文件可能已被移动或损坏。文本仍可阅读，需要时到会话库重新导出。',
        retry: '重新加载音频',
      },

      // 时间轴标记（对应后端 contract.Marker.kind）
      markers: {
        degrade: '引擎降级',
        'source-lost': '音频源断开',
        'device-switch': '切换音频设备',
        paused: '录制暂停',
        resumed: '录制继续',
        'perf-throttle': '因整机占用降档',
        'perf-suspend': '因整机占用暂停推理',
        'perf-resume': '整机负载回落后恢复推理',
        generic: '录制过程中的事件',
      },
    },

    // 错误文案按后端错误码（internal/apierr）映射；命中不到时回落 errors.unknown。
    errors: {
      unknown: '操作失败：{{message}}',
      dismiss: '关闭提示',
      ERR_INVALID_ARGUMENT: '请求参数不合法，请检查筛选条件后重试。',
      ERR_NOT_FOUND: '找不到对应的会话，可能已被删除。',
      ERR_DB_WRITE: '写入数据库失败，请稍后重试。',
      ERR_DISK_FULL: '磁盘空间不足，导出或转写无法继续。请清理空间后重试。',
      ERR_PATH_UNWRITABLE: '目标路径不可写，请选择其他位置。',
      ERR_ENGINE_NOT_READY: '推理引擎尚未就绪，请稍后重试。',
      ERR_MODEL_NOT_FOUND: '当前模型不可用，请到「模型」页下载或切换。',
      ERR_MODEL_CORRUPT: '模型文件已损坏，请到「模型」页重新下载。',
      ERR_LOCAL_UNAVAILABLE: '本机未安装声纹模型，请先到「模型」页下载后再发起分离。',
      ERR_STATE_FORBIDDEN: '当前状态下不允许该操作（可能已有转写任务在跑）。',
      ERR_PERF_SUSPENDED: '整机负载过高，推理已暂停，稍后会自动继续。',
      ERR_OFFLINE_ENFORCED: '已开启离线模式，在线模型不可用。',
    },
  },
};
