// 前端日志上报（设置 → 隐私边界 → 调试模式）。
//
// 调试模式开启时，把前端的未捕获异常、未处理的 Promise 拒绝、console.error / console.warn
// 转发到后端调试日志（privacysvc.ReportFrontendLog → <dataRoot>/logs/debug.log）。
// 桌面生产包里没有可见的开发者控制台，不转发就无从排查界面侧问题。
//
// 约束：
//   - 调试模式关闭时**不发任何 IPC**（开关状态启动时取一次，之后随 appsettings:changed 刷新）；
//   - 只带级别、消息、堆栈、当前路由与窗口类型；URL 去掉协议、主机与 query；
//     正文类内容由后端脱敏器按字段规则剔除（这里只做截断）；
//   - 限流：每分钟最多 60 条，同一条文案 5 秒内只报一次；上报失败后暂停 10 秒；
//   - 防递归：上报过程中产生的 console 输出不再上报。
//
// 直接 import 具体 service 文件而不是 '@/services'：子窗口与托盘入口不应被整个 services 索引拖大。
import { AppSettingsService } from '@/services/appsettings/AppSettingsService';
import { PrivacyService } from '@/services/privacy/PrivacyService';

export type FrontendWindowKind = 'main' | 'child' | 'systray';

type ReportLevel = 'error' | 'warn';
type ReportKind = 'error' | 'rejection' | 'console';

const MAX_PER_MINUTE = 60;
const DEDUPE_MS = 5_000;
const FAILURE_PAUSE_MS = 10_000;
const MESSAGE_MAX = 500;
const STACK_MAX = 2_000;

let installed = false;
let enabled = false;
let route = '';
let windowKind: FrontendWindowKind = 'main';
let inReport = false;
let pausedUntil = 0;
let windowStart = 0;
let sentInWindow = 0;
const recent = new Map<string, number>();

// 路由切换时由 RouterProvider 调用：上报里带上「出错时在哪个页面」。
export const setFrontendLogRoute = (id: string): void => {
  route = id;
};

const clip = (text: string, max: number): string => (text.length > max ? `${text.slice(0, max)}…` : text);

// 去掉 URL 的协议、主机与 query / hash，只留路径（堆栈里的文件位置够用了）。
const stripUrls = (text: string): string =>
  text.replace(/[a-z][a-z0-9+.-]*:\/\/[^/\s)]+/gi, '').replace(/(\/[^\s)?#]*)[?#][^\s):]*/g, '$1');

const describe = (value: unknown): string => {
  if (value instanceof Error) return `${value.name}: ${value.message}`;
  if (typeof value === 'string') return value;
  try {
    return JSON.stringify(value) ?? String(value);
  } catch {
    return String(value);
  }
};

const send = (level: ReportLevel, kind: ReportKind, message: string, stack: string): void => {
  if (!enabled || inReport) return;
  const now = Date.now();
  if (now < pausedUntil) return;
  if (now - windowStart >= 60_000) {
    windowStart = now;
    sentInWindow = 0;
  }
  if (sentInWindow >= MAX_PER_MINUTE) return;
  const key = `${level}|${message}`;
  const last = recent.get(key);
  if (last !== undefined && now - last < DEDUPE_MS) return;
  if (recent.size > 200) recent.clear();
  recent.set(key, now);
  sentInWindow += 1;

  inReport = true;
  try {
    PrivacyService.reportFrontendLog({
      level,
      kind,
      message: clip(stripUrls(message), MESSAGE_MAX),
      stack: clip(stripUrls(stack), STACK_MAX),
      route,
      window: windowKind,
    }).catch(() => {
      // 上报失败只暂停，不再上报这次失败本身（否则失败会自我放大）。
      pausedUntil = Date.now() + FAILURE_PAUSE_MS;
    });
  } finally {
    inReport = false;
  }
};

const refresh = async (): Promise<void> => {
  try {
    const status = await PrivacyService.getDebugLogStatus();
    enabled = status.mode.enabled;
  } catch {
    enabled = false;
  }
};

// 在每个窗口入口（main / child / systray）调用一次。重复调用无副作用。
export const installFrontendLogReporter = (kind: FrontendWindowKind): void => {
  if (installed || typeof window === 'undefined') return;
  installed = true;
  windowKind = kind;

  window.addEventListener('error', (event) => {
    const err: unknown = event.error;
    const message = event.message || describe(err);
    const stack =
      err instanceof Error && err.stack ? err.stack : `${event.filename}:${event.lineno}:${event.colno}`;
    send('error', 'error', message, stack);
  });
  window.addEventListener('unhandledrejection', (event) => {
    const reason: unknown = event.reason;
    send('error', 'rejection', describe(reason), reason instanceof Error ? (reason.stack ?? '') : '');
  });

  for (const level of ['error', 'warn'] as const) {
    const original = console[level].bind(console);
    console[level] = (...args: unknown[]): void => {
      original(...args);
      // 调用点堆栈：去掉 Error 本身与这层包装两帧。
      const stack = (new Error().stack ?? '').split('\n').slice(2).join('\n');
      send(level, 'console', args.map(describe).join(' '), stack);
    };
  }

  void refresh();
  AppSettingsService.onChanged((field) => {
    if (field === 'debugMode' || field === 'privacy') void refresh();
  });
};
