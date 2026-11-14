// E2E 后端启动器：清空隔离数据目录 → 启动 server 模式二进制 → 转发信号。
//
// 为什么不用 `rm -rf ... && mkdir -p ...` 这种 shell 命令串：
// Playwright 的 webServer.command 在 Windows 上由 cmd.exe 执行，`mkdir -p` 会被当成
// 「建一个名为 -p 的目录」，第二次运行就因目录已存在而启动失败（并且脏目录会留在仓库里）。
// 用 Node 脚本做同样的三件事，跨平台语义一致，也不需要本机额外装 rm/mkdir。
import { spawn } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const root = path.resolve(here, '..', '..');

const port = process.env.WAILS_SERVER_PORT ?? process.env.E2E_PORT ?? '9346';
const dataDir = process.env.MANDARYN_DATA_DIR ?? path.join(root, '.e2e-data');

// Windows 产物带 .exe；其余平台无后缀。两种都探测，避免配置里写死平台。
const candidates = [
  path.join(root, 'bin', 'mandaryn-server.exe'),
  path.join(root, 'bin', 'mandaryn-server'),
];
const binary = candidates.find((p) => fs.existsSync(p));
if (binary === undefined) {
  console.error(`[e2e] 找不到 server 产物，请先执行：wails3 task build:server\n  试探路径：${candidates.join(', ')}`);
  process.exit(1);
}

// 每次启动都从空目录开始：用例之间不共享数据库，也不会污染用户真实数据目录。
// 「自管进程」的用例把现场放在本目录的兄弟子目录（.e2e-data/{product,crash,playback}），
// 它们都在用例内部以 fresh 方式自建现场，因此这里整体清空不会伤到任何人。
fs.rmSync(dataDir, { recursive: true, force: true });
fs.mkdirSync(dataDir, { recursive: true });

const child = spawn(binary, [], {
  stdio: 'inherit',
  env: { ...process.env, WAILS_SERVER_PORT: port, MANDARYN_DATA_DIR: dataDir },
});

child.on('exit', (code, signal) => {
  // 子进程自己退出（崩溃 / 端口被占）时立刻退出，否则 Playwright 会一直等到超时。
  process.exit(signal !== null ? 1 : (code ?? 1));
});

const forward = (signal) => {
  if (!child.killed) child.kill(signal);
};
process.on('SIGINT', () => forward('SIGINT'));
process.on('SIGTERM', () => forward('SIGTERM'));
process.on('exit', () => forward('SIGTERM'));
