import { spawn, type ChildProcess } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

// E2E 自管 server 进程的小工具：崩溃恢复用例需要「杀进程 → 重启 → 再断言」，
// 而 Playwright 的 webServer 只负责首次启动，无法在中途重启，因此进程生命周期自己管。
//
// 铁律：调用方必须传一个**隔离数据目录**（仓库内 .e2e-data-* 下），绝不指向用户真实数据。

const here = path.dirname(fileURLToPath(import.meta.url));
export const REPO_ROOT = path.resolve(here, '..', '..');

// Windows 产物带 .exe，其余平台无后缀；两种都探测，避免把平台差异写进用例。
export const serverBinary = (): string => {
  const candidates = [
    path.join(REPO_ROOT, 'bin', 'mandaryn-server.exe'),
    path.join(REPO_ROOT, 'bin', 'mandaryn-server'),
  ];
  const found = candidates.find((p) => fs.existsSync(p));
  if (found === undefined) {
    throw new Error(`找不到 server 产物，请先执行 wails3 task build:server。试探：${candidates.join(', ')}`);
  }
  return found;
};

export interface ServerHandle {
  child: ChildProcess;
  port: number;
  dataDir: string;
  // 收集 stderr 供失败时诊断（不含音频 / 正文，只是日志元数据）。
  logs: string[];
}

// startServer 用指定端口与数据目录启动一个全新的 server 进程。
// env 追加额外环境变量（如 MANDARYN_LOG_LEVEL），不覆盖端口与数据目录。
export const startServer = (opts: {
  port: number;
  dataDir: string;
  fresh?: boolean;
  managedDataRoot?: boolean;
  env?: Record<string, string>;
}): ServerHandle => {
  const { port, dataDir } = opts;
  if (opts.fresh !== false) {
    fs.rmSync(dataDir, { recursive: true, force: true });
    fs.mkdirSync(dataDir, { recursive: true });
  }
  const logs: string[] = [];
  const child = spawn(serverBinary(), [], {
    stdio: ['ignore', 'pipe', 'pipe'],
    env: { ...process.env, ...opts.env, WAILS_SERVER_PORT: String(port), MANDARYN_DATA_DIR: opts.managedDataRoot === false ? '' : dataDir },
  });
  child.stdout?.on('data', (buf: Buffer) => logs.push(buf.toString('utf8')));
  child.stderr?.on('data', (buf: Buffer) => logs.push(buf.toString('utf8')));
  return { child, port, dataDir, logs };
};

// waitForServer 轮询端口直到能拿到 HTTP 响应（server 模式启动约 200–800ms）。
//
// 传入 handle 时，超时错误里会带上该进程最近的输出：E2E 启动失败最常见的原因是端口被占
// 或产物过期（老二进制不认识新数据目录），有日志才能一眼看出是哪种。
export const waitForServer = async (
  port: number,
  timeoutMs = 20_000,
  handle?: ServerHandle,
): Promise<void> => {
  const deadline = Date.now() + timeoutMs;
  let lastErr: unknown = null;
  while (Date.now() < deadline) {
    try {
      const res = await fetch(`http://127.0.0.1:${port}/`);
      if (res.ok || res.status < 500) return;
      lastErr = new Error(`HTTP ${res.status}`);
    } catch (err) {
      lastErr = err;
    }
    await new Promise((resolve) => setTimeout(resolve, 120));
  }
  const tail = handle === undefined ? '' : `\n--- server 输出 ---\n${handle.logs.join('').slice(-2000)}`;
  throw new Error(`server 未在 ${timeoutMs}ms 内就绪（端口 ${port}）：${String(lastErr)}${tail}`);
};

// killServer 模拟「进程被强杀」：直接终止，不给收尾机会（Windows 上即 TerminateProcess）。
//
// 用 kill 而不是走产品的正常退出：崩溃恢复要的正是「库里有 recording 行 + 头没回填」的现场。
export const killServer = async (handle: ServerHandle): Promise<void> => {
  const { child } = handle;
  if (child.exitCode !== null || child.signalCode !== null) return;
  const exited = new Promise<void>((resolve) => child.once('exit', () => resolve()));
  child.kill('SIGKILL');
  await Promise.race([exited, new Promise<void>((resolve) => setTimeout(resolve, 5_000))]);
};

// stopServer 用于 afterAll：尽量温和地收尾（避免残留进程占用端口）。
export const stopServer = async (handle: ServerHandle | null): Promise<void> => {
  if (handle === null) return;
  await killServer(handle);
};
