import type { Messages } from '@/i18n';

export const aboutZhCN: Messages = {
  settings: {
    about: {
      title: '关于与更新',
      hint: '查看当前版本并检查是否有新版本。检查需要联网；应用不会自动下载或安装更新。',
      version: {
        label: '当前版本',
        loading: '正在读取…',
        unknown: '未知',
      },
      check: {
        button: '检查更新',
        upToDate: '已是最新版本。',
        available: '有新版本可用：{{version}}',
        openDownload: '打开发布页',
        manualNote: '发现新版本后请手动下载安装包完成更新 —— 本应用不做静默自更新。',
        offline: '已开启离线模式，未发起任何网络请求。请关闭离线模式后重试。',
        failed: '检查更新失败：{{message}}',
        errors: {
          ERR_DOWNLOAD_NETWORK: '无法访问版本清单（网络不可达，或清单地址对当前仓库不可匿名访问）。',
          ERR_OFFLINE_ENFORCED: '已开启离线模式，未发起任何网络请求。',
          ERR_INVALID_ARGUMENT: '版本清单地址不可用，请检查配置。',
        },
      },
      policy: {
        title: '为什么不自动更新',
        body: '自动替换安装目录中的可执行文件需要提权，也会绕过系统的软件管控；因此本应用只提示新版本，由你手动下载安装包完成更新。',
      },
    },
  },
};
