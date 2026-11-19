import type { Messages } from '@/i18n';

export const databaseZhCN: Messages = {
  settings: {
    database: {
      title: '数据存储',
      hint: '数据根目录存放数据库、录音和其他应用数据。更改目录后，应用将在下次启动时迁移数据。',
      currentRootLabel: '当前数据根目录',
      currentPathLabel: '当前数据库文件',
      defaultPathLabel: '默认数据库文件',
      sizeLabel: '数据库占用空间',
      customBadge: '非默认位置',
      defaultBadge: '默认',
      pendingRootLabel: '待迁移目标目录',
      targetRootLabel: '目标目录（绝对路径）',
      targetRootHint: '请输入目录路径，不要包含数据库文件名。',
      restartRequired: '重启后才会迁移；在此之前，应用继续使用当前数据根目录。',
      managedByCli: '当前目录由命令行参数接管。',
      managedByEnv: '当前目录由环境变量接管。',
      managedBy: '当前目录由 {{source}} 接管。',
      overrideRestart: '解除接管并重启应用后，才会应用待迁移设置。',
      overrideHintLabel: '解除接管提示',
      migrationFailed: '迁移失败，应用仍在使用原数据根目录。原因：{{message}}',
      actions: {
        change: '更改位置…',
        stage: '安排迁移',
        reset: '重置为默认',
        cancelMigration: '取消待迁移',
        copyHint: '复制解除提示',
      },
      dialog: {
        title: '选择新的数据根目录',
        message: '选定的目录将在应用下次启动时用于迁移。',
        confirm: '选择目录',
      },
      confirmReset: {
        title: '恢复默认位置',
        message: '将平台默认目录设为迁移目标，重启后才会迁移。是否继续？',
        ok: '设为迁移目标',
        cancel: '取消',
      },
      // 单表清空确认对话框
      confirmClear: {
        title: '清空 {{label}}',
        message: '将删除「{{label}}」中的全部 {{rows}} 行数据。该操作不可撤销，是否继续？',
        ok: '清空',
        cancel: '取消',
      },
      feedback: {
        changing: '正在保存迁移设置…',
        changed: '已保存目录设置，当前目录不变。',
        resetting: '正在保存默认目录设置…',
        reset: '已保存默认目录设置，当前目录不变。',
        cancelling: '正在取消待迁移设置…',
        cancelled: '已取消待迁移设置。',
        copied: '已复制解除接管提示。',
        clearing: '正在清空「{{label}}」…',
        cleared: '已清空「{{label}}」',
        failed: '操作失败：{{message}}',
      },
      charts: {
        share: {
          title: '空间占比',
          hint: '各表在数据库中的相对占用比例。',
        },
        bytes: {
          title: '字节用量',
          hint: '各表的实际字节占用（含索引页）。',
        },
        empty: '暂无数据',
        estimatedNote: '部分表为估算值（按行数比例推算）。',
      },
      // 表清单
      tables: {
        title: '数据表',
        empty: '没有可展示的表。',
        meta: '行数 {{rows}} · {{size}}',
        clear: '清空',
        clearAria: '清空 {{label}}',
        protected: '受保护',
        protectedAria: '{{label}} 受保护，不可一键清空',
        estimatedTag: '估算',
        // 每张表的标签 / 描述：键名与 internal/storage/models.go 的 ModelDescriptor.LabelKey 对齐
        preferences: {
          label: '行为偏好',
          description: '界面开关 / 显隐设置等键值对，可重建。',
        },
        appSettings: {
          label: '应用设置',
          description: '主题选择 / 自定义主题 / 语言。涉及视觉一致性，不允许一键清空。',
        },
      },
    },
  },
};
