import { spawnSync } from 'node:child_process';

// 跨平台入口：设置 E2E_PORTABLE=1 后调用 playwright，并把命令行参数原样透传。
//
// 为什么需要这个包装：`E2E_PORTABLE=1 playwright test` 这种前缀写法在 Windows 的 cmd
// 下不生效（npm script 由 cmd.exe 执行），而 `cross-env` 会多引一个依赖。
// 用 Node 设置环境变量再 spawn 是最省事且平台无关的做法。
//
// portable 模式排除 product-flow / crash-recovery：它们依赖真实音频设备做设备回环录制，
// 容器与 CI runner 没有音频端点，用例会失败（而不是跳过）。详见 playwright.config.ts 注释。
const args = process.argv.slice(2);

const result = spawnSync('playwright', ['test', ...args], {
  stdio: 'inherit',
  shell: true, // Windows 上 playwright 实际是 playwright.cmd，需要 shell 解析
  env: { ...process.env, E2E_PORTABLE: '1' },
});

if (result.error !== undefined) {
  console.error(`启动 playwright 失败：${result.error.message}`);
  process.exit(1);
}
process.exit(result.status ?? 1);
