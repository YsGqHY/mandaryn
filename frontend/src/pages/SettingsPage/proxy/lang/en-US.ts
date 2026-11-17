import type { Messages } from '@/i18n';

// 代理子页面文案（英文）。namespace: settings.proxy.*
export const proxyEnUS: Messages = {
  settings: {
    proxy: {
      title: 'Network proxy',
      hint: 'Model downloads and manifest fetching share one proxy configuration; online transcription can opt in separately.',
      env: {
        title: 'Overridden by {{name}}',
        body: 'This environment variable takes precedence over this page, so the proxy settings below currently have no effect (model downloads and manifest fetching both use it). To make this page effective, unset the variable and restart the app.',
      },
      form: {
        title: 'Proxy settings',
        desc: 'Leave it empty with "No proxy" for a direct connection. When the proxy is unreachable we never silently fall back to direct — that would make you believe traffic is proxied while it is not.',
        typeLabel: 'Proxy type',
        typeHint: 'https means the connection to the proxy itself uses TLS; it is unrelated to the target site scheme.',
        type: {
          none: 'No proxy',
          http: 'HTTP',
          https: 'HTTPS',
          socks5: 'SOCKS5',
        },
        hostLabel: 'Proxy host',
        hostPlaceholder: 'e.g. 127.0.0.1',
        portLabel: 'Port',
        portPlaceholder: 'e.g. 7890',
        usernameLabel: 'Username (optional)',
        usernamePlaceholder: 'Leave empty if the proxy needs no authentication',
        passwordLabel: 'Password (optional)',
        passwordPlaceholder: 'Leave empty to keep the saved password',
        passwordHint: 'The password is stored encrypted only and never shown in plain text; leave it empty to keep the existing one.',
        noProxyLabel: 'Bypass list (optional)',
        noProxyPlaceholder: 'e.g. localhost,127.0.0.1,.internal',
        noProxyHint: 'Comma separated; supports * (bypass everything), exact hosts and .suffix entries.',
        useForDownload: 'Proxy model downloads and manifest fetching',
        useForOnline: 'Proxy online transcription',
      },
      actions: {
        save: 'Save',
        test: 'Test connection',
        clear: 'Clear',
        dirty: 'Unsaved changes',
      },
      feedback: {
        saved: 'Saved.',
        cleared: 'Proxy cleared — using a direct connection now.',
        testOk: 'Connection OK ({{latency}} ms).',
      },
      test: {
        failedGeneric: 'The proxy connection test failed.',
        category: {
          auth: 'Proxy authentication failed: check the username and password.',
          'rate-limit': 'Rate limited by the proxy or target; please retry later.',
          tls: 'TLS handshake failed: the proxy certificate may be untrusted, or the proxy type is wrong (https means TLS to the proxy itself).',
          timeout: 'Connection timed out: check the host, port and whether the proxy is running.',
          network: 'Network unreachable: verify the proxy host, port and connectivity.',
          proxy: 'The proxy refused the connection: check its configuration and authentication.',
          unknown: 'The test failed for an unknown reason.',
        },
      },
      errors: {
        host: 'Please enter the proxy host.',
        port: 'The port must be an integer between 1 and 65535.',
        generic: 'Operation failed: {{message}}',
        ERR_INVALID_ARGUMENT: 'Invalid proxy configuration: check the type, host and port.',
        ERR_DOWNLOAD_PROXY: 'Invalid proxy configuration (or the credentials cannot be decrypted): please re-enter them.',
        ERR_OFFLINE_ENFORCED: 'Offline mode is enabled, so no request was sent. Turn off offline mode in Privacy settings first.',
        ERR_DB_WRITE: 'Save failed: database write error.',
      },
    },
  },
};
