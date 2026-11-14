// 托盘面板窗口入口（systray.html → 本文件）。
//
// 面板是独立的 OS 窗口，不共享主窗口 React 树：
//  - I18nProvider / MandarynThemeProvider 各自从 AppSettingsService（SQLite）读取
//    用户的语言与主题选择，因此面板与主窗口外观一致（含自定义主题、跟随系统）
//  - MandarynThemeProvider 内部即 MantineProvider + buildMantineTheme / buildCssVariablesResolver，
//    与 child.tsx 的主题编译方式相同
//  - 内置模块通过副作用 import 自注册到 systrayRegistry
import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import '@/styles';
import { I18nProvider, registerMandarynLocales } from '@/i18n';
import { MandarynThemeProvider, registerMandarynThemes } from '@/styles/themes';
import { SystrayPanel, registerSystrayLocales } from '@/systray';
import '@/systray/modules/AppStatus';
import '@/systray/modules/QuickActions';
import '@/systray/modules/Notifications';
import '@/systray/modules/ExitButton';
import { installFrontendLogReporter } from '@/shared/frontendLogReporter';

installFrontendLogReporter('systray');

registerMandarynThemes();
registerMandarynLocales();
registerSystrayLocales();

const SystrayApp = () => (
  <I18nProvider>
    <MandarynThemeProvider>
      <SystrayPanel />
    </MandarynThemeProvider>
  </I18nProvider>
);

const root = document.getElementById('systray-root');
if (root) {
  createRoot(root).render(
    <StrictMode>
      <SystrayApp />
    </StrictMode>,
  );
}
