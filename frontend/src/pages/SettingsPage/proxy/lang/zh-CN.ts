import type { Messages } from '@/i18n';

// 代理子页面文案（中文）。namespace: settings.proxy.*
export const proxyZhCN: Messages = {
  settings: {
    proxy: {
      title: '网络代理',
      hint: '模型下载与模型清单获取共用同一份代理配置；在线转写可单独决定是否走代理。',
      env: {
        title: '已由环境变量 {{name}} 接管',
        body: '该环境变量优先级高于本页设置，因此本页的代理配置当前不会生效（模型下载与清单获取都走它）。如需在本页配置生效，请先清除该环境变量并重启应用。',
      },
      form: {
        title: '代理配置',
        desc: '留空并选择「不使用代理」即为直连。代理不可用时不会自动回落直连，以免你以为流量已走代理而实际裸奔。',
        typeLabel: '代理类型',
        typeHint: 'https 表示以 TLS 连接代理本身，与目标站点是否为 https 无关。',
        type: {
          none: '不使用代理',
          http: 'HTTP',
          https: 'HTTPS',
          socks5: 'SOCKS5',
        },
        hostLabel: '代理地址',
        hostPlaceholder: '例如 127.0.0.1',
        portLabel: '端口',
        portPlaceholder: '例如 7890',
        usernameLabel: '用户名（可选）',
        usernamePlaceholder: '留空表示代理不需要认证',
        passwordLabel: '密码（可选）',
        passwordPlaceholder: '留空表示不修改已保存的密码',
        passwordHint: '密码只以加密形式保存、界面不回显明文；留空即保持原有密码不变。',
        noProxyLabel: '直连例外（可选）',
        noProxyPlaceholder: '例如 localhost,127.0.0.1,.internal',
        noProxyHint: '逗号分隔；支持 *（全部直连）、精确主机与 .后缀 形式。',
        useForDownload: '模型下载与清单获取走代理',
        useForOnline: '在线转写走代理',
      },
      actions: {
        save: '保存',
        test: '测试连通性',
        clear: '清空',
        dirty: '有未保存的改动',
      },
      feedback: {
        saved: '已保存。',
        cleared: '已清空代理配置，当前为直连。',
        testOk: '连通性正常（{{latency}} ms）。',
      },
      test: {
        failedGeneric: '代理连通性测试未通过。',
        category: {
          auth: '代理认证失败：请检查用户名与密码。',
          'rate-limit': '目标或代理限流，请稍后重试。',
          tls: 'TLS 握手失败：代理证书可能不受信任，或代理类型选错了（https 表示以 TLS 连接代理）。',
          timeout: '连接超时：请检查地址与端口，以及代理是否在运行。',
          network: '网络不可达：请确认代理地址、端口与网络连通性。',
          proxy: '代理拒绝了连接：请检查代理配置与认证要求。',
          unknown: '测试失败，原因未知。',
        },
      },
      errors: {
        host: '请填写代理地址。',
        port: '端口必须是 1–65535 之间的整数。',
        generic: '操作失败：{{message}}',
        ERR_INVALID_ARGUMENT: '代理配置无效：请检查类型、地址与端口。',
        ERR_DOWNLOAD_PROXY: '代理配置无效（或凭据无法解密）：请重新填写。',
        ERR_OFFLINE_ENFORCED: '已开启离线模式，未发起任何请求。请先在隐私设置里关闭离线模式。',
        ERR_DB_WRITE: '保存失败：数据库写入出错。',
      },
    },
  },
};
