import type { Messages } from '@/i18n';

// 个性化子页面文案：挂在 settings.personalization.* 命名空间。
export const personalizationZhCN: Messages = {
  settings: {
    personalization: {
      desktop: {
        title: '桌面交互', hint: '修改立即生效，并在重启后保留。',
        tray: '启用系统托盘', click: '托盘左键行为', panel: '显示托盘面板', window: '显示主窗口',
        cues: '录制提示音', cuesHint: '录制开始、结束或失败时播放短提示音，不改变系统音量。',
        error: '读取或保存桌面设置失败。',
      },
      roles: {
        title: '说话人角色', hint: '前缀只影响新建角色；配色会立即更新已有角色及其历史会话。',
        prefix: '默认角色命名前缀', tooLong: '最多 20 个字符。', save: '保存角色前缀',
        role: '已有角色', selectRole: '选择要改色的角色', color: '角色配色', numberedColor: '角色颜色 {{number}}',
        accent: '主题强调色', neutral: '中性色', failed: '无法读取或保存角色设置，请重试。',
      },
      theme: {
        title: '主题',
        hint: '选择应用的明暗模式。"跟随系统"会随操作系统的偏好自动切换。',
        currentLine: '当前生效：{{label}}',
        followingSystem: '（跟随系统）',
      },
      preferences: {
        title: '显示偏好',
        hint: '调整界面元素的显隐，立即生效。',
        showLogo: {
          label: '显示 Logo',
          description: '侧边栏顶部的应用标识方块',
        },
        showTooltip: {
          label: '显示菜单 Tooltip',
          description: '鼠标悬停时显示菜单按钮的名称气泡',
        },
      },
      customTheme: {
        title: '自定义主题',
        hint: '逐项调整调色板字段，保存后会出现在上方的主题列表中（"自定义"）。点击色块可呼出系统取色器，文本框接受任意 CSS 颜色值（#hex / rgb / rgba）。',
        toolbar: {
          seedLight: '以「明亮」为模板',
          seedDark: '以「黑暗」为模板',
          seedObsidian: '以「黑曜」为模板',
          modeLight: '模式：浅色 (Light)（点击切换）',
          modeDark: '模式：深色 (Dark)（点击切换）',
          remove: '删除自定义主题',
        },
        notUsingHint: '提示：你正在使用「{{label}}」。改色后会自动切换到「自定义」。',
        groups: {
          background: '背景',
          text: '文本',
          border: '边框',
          accent: '强调',
          status: '状态',
        },
        fields: {
          bg: {
            base: { label: '基础背景', description: '标题栏 / 最外层' },
            sidebar: { label: '侧边栏背景' },
            content: { label: '主内容背景' },
            surface: { label: '卡片表面', description: 'Paper / 卡片' },
            elevated: { label: '提升层', description: '输入框 / 提升表面' },
            hover: { label: '悬停覆盖', description: 'hover 半透明色' },
            active: { label: '激活覆盖', description: '选中态半透明色' },
          },
          text: {
            primary: { label: '主要文本' },
            secondary: { label: '次要文本' },
            muted: { label: '弱化文本', description: '提示 / 辅助文字' },
          },
          divider: { label: '分割线' },
          accent: { label: '主强调色' },
          accentHover: { label: '强调色悬停' },
          status: {
            danger: { label: '危险', description: '错误 / 删除' },
            success: { label: '成功' },
            warning: { label: '警告' },
          },
        },
        a11y: {
          colorPickerFor: '颜色选择器：{{label}}',
          colorValueFor: '颜色值：{{label}}',
        },
      },
    },
  },
};
