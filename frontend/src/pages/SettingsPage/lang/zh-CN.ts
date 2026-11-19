import type { Messages } from '@/i18n';

// SettingsPage 顶层文案（中文）：仅包含"框架"层级 ——
//   - eyebrow / title（页头）
//   - list.*（左列入口）
//   - themes.*（主题预设的展示名，被 personalization 与全局通用引用）
// 子页面专属文案在 SettingsPage/<name>/lang/ 下，由 register<Name>Locales 注册。
export const settingsPageZhCN: Messages = {
  settings: {
    eyebrow: '设置',
    title: '偏好设置',
    list: {
      personalization: {
        label: '个性化',
        description: '主题、显示偏好与自定义配色',
      },
      layout: {
        label: '布局',
        description: '导航位置与应用外壳的网格布局',
      },
      language: {
        label: '语言',
        description: '切换界面语言',
      },
      database: {
        label: '数据存储',
        description: '数据库位置与占用空间',
      },
      performance: {
        label: '性能预算',
        description: '整机占用份额与推理档位',
      },
      audio: {
        label: '音频与录制',
        description: '边录边转开关（默认关闭）',
      },
      transcription: {
        label: '转写',
        description: '在线端点、语种与降级策略',
      },
      proxy: {
        label: '网络代理',
        description: '模型下载与在线转写的代理配置',
      },
      privacy: {
        label: '隐私边界',
        description: '离线开关、出站记录与诊断包',
      },
      hotkeys: {
        label: '快捷键',
        description: '系统级与应用内快捷键绑定',
      },
      about: {
        label: '关于与更新',
        description: '版本信息与检查新版本',
      },
    },
    themes: {
      system: { label: '跟随系统', description: '随操作系统的明暗模式自动切换' },
      light: { label: '明亮', description: '默认白色主题，适合白天使用' },
      dark: { label: '黑暗', description: '深灰主题，降低低光环境下的眩光' },
      obsidian: { label: '黑曜', description: '纯黑画布 + 紫罗兰强调，对比度最高' },
      custom: { label: '自定义', description: '你保存的自定义配色' },
    },
  },
};
