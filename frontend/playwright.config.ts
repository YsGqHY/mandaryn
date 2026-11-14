import { defineConfig, devices } from '@playwright/test';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

// E2E：以 Wails server 模式（-tags server，纯 HTTP，无原生窗口）运行真实后端 + 前端，
// 由 Chromium 驱动。数据目录用 MANDARYN_DATA_DIR 隔离，绝不触碰用户真实数据库。
//   1) 构建：wails3 task build:server（产物 bin/mandaryn-server[.exe]）
//   2) 运行：cd frontend && pnpm e2e
const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const PORT = Number(process.env.E2E_PORT ?? 9346);
// 共享 server 的数据目录**保持** .e2e-data 本身：screenshots.spec.ts 会直接往
// .e2e-data/mandaryn.db 写偏好来产出确定性截图，路径不能挪。
// 「自管进程」的用例把现场放在 .e2e-data/{product,crash,playback} 子目录里，
// 且各自以 fresh 方式自建现场 —— 共享启动器在开跑时清空 .e2e-data 不影响它们
// （那发生在任何用例执行之前）。
const DATA_DIR = process.env.E2E_DATA_DIR ?? path.join(root, '.e2e-data');

// 崩溃恢复用例要自己「杀进程再重启」，因此它不共用上面的 webServer：
// 独立端口 + 独立数据目录，用例内部用 child_process 管理进程生命周期。
// 端口刻意与共享 server 拉开距离，两个 server 可同时在跑、互不干扰。
const CRASH_PORT = Number(process.env.E2E_CRASH_PORT ?? 9356);

// 「自管进程」的用例清单：这些 spec 各自用 serverctl 拉起独立 server（不同端口 + 不同数据目录），
// 所以既不吃共享 webServer，也不受它影响。新增同类用例只需加入这个列表。
// 「平台无关子集」开关（E2E_PORTABLE=1）：
// product-flow 与 crash-recovery 都依赖**真实音频设备**做设备回环录制 —— 容器与 CI runner
// 没有音频端点，捕获层在这些环境里是桩（返回 ERR_CAPTURE_UNAVAILABLE），用例会**失败而非跳过**。
// 因此把这两个文件在 portable 模式下排除；它们仍需在带声卡的机器（本地或 self-hosted）上跑。
// screenshots 同理排除：它是**截图归档**用例，用系统 `sqlite3` CLI 直接改库来产出确定性图片，
// 而 CI runner 与容器里没有该命令（spawnSync sqlite3 ENOENT）；它不验证产品行为，
// 因而留在本地 `pnpm e2e` / `pnpm e2e:screenshots` 跑。
const PORTABLE = process.env.E2E_PORTABLE === '1';
const PORTABLE_EXCLUDE = [
  /product-flow\.spec\.ts/,
  /crash-recovery\.spec\.ts/,
  /screenshots\.spec\.ts/,
];

const ISOLATED_SPECS = [
  /crash-recovery\.spec\.ts/,
  /product-flow\.spec\.ts/,
  /playback-highlight\.spec\.ts/,
  /playback-advance\.spec\.ts/,
  /not-automatable\.spec\.ts/,
  /debug-mode\.spec\.ts/,
];

export default defineConfig({
  testDir: './e2e',
  timeout: 30_000,
  fullyParallel: false, // 共用一个后端 + 一个数据库，串行执行
  workers: 1,
  reporter: [['list']],
  use: {
    baseURL: `http://127.0.0.1:${PORT}`,
    locale: 'zh-CN',
    viewport: { width: 1280, height: 800 },
    deviceScaleFactor: 2,
    ...devices['Desktop Chrome'],
    // 用本机已安装的 Chromium 系浏览器，免去 `playwright install` 下载；
    // CI 中可设 E2E_CHANNEL=chromium 并预先执行 `npx playwright install chromium`
    channel: process.env.E2E_CHANNEL ?? 'msedge',
  },
  // 两个 project 分工：
  //   - shared  ：共享上面 webServer 的用例（既有 floating-dock / screenshots）；
  //   - isolated：自管进程的用例（崩溃恢复 / 主流程 / 回放高亮 / 无法自动化说明），
  //               各自用 e2e/serverctl.ts 拉起独立 server（不同端口 + 不同数据目录）。
  projects: [
    {
      name: 'shared',
      testIgnore: PORTABLE ? [...ISOLATED_SPECS, ...PORTABLE_EXCLUDE] : ISOLATED_SPECS,
    },
    {
      name: 'isolated',
      testMatch: ISOLATED_SPECS,
      testIgnore: PORTABLE ? PORTABLE_EXCLUDE : undefined,
      // 这些用例自己拉起 server 并用绝对地址访问，不使用 baseURL。
    },
  ],
  webServer: {
    // 用 Node 启动器而不是 shell 命令串：Windows 上 webServer.command 由 cmd.exe 执行，
    // `mkdir -p` 会退化成「建一个名为 -p 的目录」，导致第二次运行启动失败。
    command: `node "${path.join(root, 'frontend', 'e2e', 'serve.mjs')}"`,
    url: `http://127.0.0.1:${PORT}`,
    reuseExistingServer: false,
    env: { WAILS_SERVER_PORT: String(PORT), MANDARYN_DATA_DIR: DATA_DIR, E2E_PORT: String(PORT) },
    stdout: 'ignore',
    stderr: 'pipe',
  },
});
