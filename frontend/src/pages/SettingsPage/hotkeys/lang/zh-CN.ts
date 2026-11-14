import type { Messages } from '@/i18n';

// 注意：动作名走**后端约定的** `hotkeys.actions.<id>` 键（internal/hotkeys/registry.go
// 的 LabelKey 约定），不是 settings.* 命名空间 —— 因为 ActionInfo.labelKey 直接就是它，
// 两处共用同一份文案，避免「设置页叫一个名、托盘/热键提示叫另一个名」。
export const hotkeysZhCN: Messages = {
  hotkeys: {
    actions: {
      record: {
        toggle: '开始 / 停止录制',
        pause: '暂停 / 继续录制',
      },
      window: {
        show: '显示 / 隐藏主窗口',
      },
      playback: {
        play: '播放 / 暂停',
        back: '后退 5 秒',
        forward: '前进 5 秒',
        prevSegment: '上一句',
        nextSegment: '下一句',
        recenter: '回到当前句',
      },
      nav: {
        record: '前往录制页',
        library: '前往会话库',
        models: '前往模型页',
        settings: '前往设置页',
      },
      search: {
        focus: '聚焦搜索框',
      },
      role: {
        rename: '重命名说话人角色',
      },
      session: {
        delete: '删除会话',
      },
      model: {
        switchSource: '切换模型来源（本地 / 在线）',
      },
      ux: {
        leanMode: '开关精简模式',
      },
    },
  },
  settings: {
    hotkeys: {
      title: '快捷键',
      hint: '按场景分组的可自定义快捷键。重绑时直接按下组合键即可录入；保存前后端会先判冲突，被系统或其它程序占用时会明确告诉你原因。',
      unavailable: {
        title: '当前平台不支持系统级快捷键',
        body: '本构建无法在系统层注册全局快捷键（可能缺少平台支持或权限）。列表仍可查看，但重绑 / 禁用 / 重置都不可用。',
      },
      capture: {
        hint: '点「重绑」后在出现的输入框里直接按下组合键，按 Esc 取消。',
        aria: '按下组合键以录入快捷键',
        placeholder: '请按下组合键…',
        cancelHint: 'Esc 取消',
      },
      resetAll: {
        action: '全部恢复默认',
        title: '恢复全部默认快捷键',
        message: '将把所有动作的快捷键恢复为默认值，你自定义的绑定会被丢弃。是否继续？',
        ok: '恢复默认',
        cancel: '取消',
      },
      scope: {
        global: '全局',
        app: '应用内',
      },
      row: {
        unwired: '尚未接线',
        unbound: '未指定组合',
        disabled: '已禁用',
        defaultValue: '默认 {{accel}}',
        noDefault: '默认不绑定',
        registerFailed: '注册失败：{{reason}}',
        capture: '重绑',
        cancel: '取消',
        disable: '禁用',
        reset: '重置',
      },
      issue: {
        register: '该组合已被系统或其它程序占用，未能注册（已回滚为原绑定）。',
        conflict: '该组合与其他动作冲突或属于系统保留，无法保存。',
        preview: '规范化预览：{{accel}}',
        invalidKey: '该按键不在可绑定范围内：{{key}}',
        reasonSystem: '系统保留',
        reasonDuplicate: '与其它动作重复',
        conflictItem: '{{reason}}：{{action}}',
        conflictItemNoAction: '{{reason}}',
      },
      errors: {
        ERR_HOTKEY_CONFLICT: '存在快捷键冲突，未保存。',
        ERR_HOTKEY_REGISTER: '系统拒绝注册该组合，未保存。',
        ERR_INVALID_ARGUMENT: '组合键格式不合法，请重新录入。',
        ERR_NOT_FOUND: '找不到该动作，可能已被移除。',
        ERR_STATE_FORBIDDEN: '当前状态下不允许该操作。',
        ERR_DB_WRITE: '写入数据库失败，请稍后重试。',
      },
      groups: {
        recording: '录制',
        playback: '回放',
        navigation: '导航',
        search: '搜索',
        editing: '编辑',
        model: '模型',
        ux: '精简体验',
      },
      lean: {
        title: '精简模式',
        switchLabel: '开启精简模式（只保留核心区域，隐去高级面板）',
        hint: '下面的清单由后端统一裁决，开启后各页面按同一份清单决定显隐 —— 不会出现「这页隐了那页没隐」。',
        visibleTitle: '保留的区域',
        hiddenRegionsTitle: '隐去的区域',
        hiddenControlsTitle: '隐去的控件',
        regions: {
          status: '顶部状态条（录制 / 降级 / 错误）',
          record: '录制页主体',
          library: '会话库列表',
          session: '会话详情（回放 + 歌词）',
          settingsAdvanced: '设置页「高级」分区',
          settingsDiagnostics: '设置页诊断分区',
          modelsAdmin: '模型管理细节',
          libraryInsights: '会话库统计与趋势',
          diagnostics: '诊断面板',
        },
        controls: {
          statusSecondaryBadges: '状态条上的次要徽章',
          libraryBulkActions: '批量选择与批量删除',
          sessionExportOptions: '导出格式等次要选项',
          recordSourceHints: '源列表的技术提示',
          settingsUpdateOptions: '更新检查频率',
          settingsDatabaseStats: '表行数统计',
          settingsSecurityNotes: '安全说明长文',
        },
      },
    },
  },
};
