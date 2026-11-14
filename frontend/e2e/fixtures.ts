import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';

// E2E 音频夹具：**运行时生成**，不依赖任何本机或仓库内的语音文件。
//
// 为什么改成生成：
//   原先两个回放用例硬编码了 `C:/Users/mayn/AppData/Local/Temp/mverify/zh.wav` 这类
//   个人机器上的绝对路径，缺失时用 `test.skip` 静默跳过。后果是
//     - CI（尤其 Linux runner）上这两个用例**根本不会执行**，只显示为 skipped，
//       绿灯并不代表回放同步被验证过；
//     - 路径与某台开发机绑定，换机器就静默失效。
//
// 为什么生成音频就够（不需要真实语音）：
//   回放高亮类用例只依赖**音频时长**——它们把 `audio.currentTime` 映射到片段区间，
//   与音频内容、是否含人声完全无关（不涉及 VAD / ASR）。因此用确定性的正弦波即可。
//   需要真实语音的是转写类用例（如 product-flow 的录制路径），它们与本模块无关。
//
// 生成位置放在系统临时目录（而不是仓库或 `.e2e-data`）：
//   Playwright 配置里的共享 webServer 会在启动时清空 `.e2e-data`，把夹具放那里会被误删。
const FIXTURE_DIR = path.join(os.tmpdir(), 'mandaryn-e2e-fixtures');

const SAMPLE_RATE = 16000;
const BYTES_PER_SAMPLE = 2; // 16bit

/**
 * 生成（或复用）一段 16kHz / 单声道 / 16bit 的确定性正弦波 WAV，返回绝对路径。
 *
 * @param durationMs 音频时长（毫秒）。回放类用例按片段跨度传入即可。
 */
export const ensureToneWav = (durationMs: number): string => {
  if (!Number.isFinite(durationMs) || durationMs <= 0) {
    throw new Error(`ensureToneWav 需要正的时长，收到：${durationMs}`);
  }
  const ms = Math.ceil(durationMs);
  const target = path.join(FIXTURE_DIR, `tone-${ms}ms-16k-mono-16bit.wav`);

  const frames = Math.round((SAMPLE_RATE * ms) / 1000);
  const dataBytes = frames * BYTES_PER_SAMPLE;
  const expectedSize = 44 + dataBytes;

  // 已存在且尺寸吻合就复用：避免每个用例重复生成（内容由参数唯一决定，无需校验内容）。
  if (fs.existsSync(target) && fs.statSync(target).size === expectedSize) {
    return target;
  }

  fs.mkdirSync(FIXTURE_DIR, { recursive: true });

  const buf = Buffer.alloc(expectedSize);
  // —— 44 字节标准 PCM 头 ——
  buf.write('RIFF', 0, 'ascii');
  buf.writeUInt32LE(36 + dataBytes, 4); // RIFF chunk size
  buf.write('WAVE', 8, 'ascii');
  buf.write('fmt ', 12, 'ascii');
  buf.writeUInt32LE(16, 16); // fmt chunk size
  buf.writeUInt16LE(1, 20); // PCM
  buf.writeUInt16LE(1, 22); // 单声道
  buf.writeUInt32LE(SAMPLE_RATE, 24);
  buf.writeUInt32LE(SAMPLE_RATE * BYTES_PER_SAMPLE, 28); // byte rate
  buf.writeUInt16LE(BYTES_PER_SAMPLE, 32); // block align
  buf.writeUInt16LE(16, 34); // bits per sample
  buf.write('data', 36, 'ascii');
  buf.writeUInt32LE(dataBytes, 40);

  // 440Hz 正弦、振幅 0.2（≈ -14 dBFS）：足够让波形非零，又不至于刺耳。
  // 逐样本确定性计算 —— 同一 durationMs 永远产出同一文件。
  const amplitude = 0.2 * 32767;
  for (let i = 0; i < frames; i += 1) {
    const value = Math.round(amplitude * Math.sin((2 * Math.PI * 440 * i) / SAMPLE_RATE));
    buf.writeInt16LE(value, 44 + i * BYTES_PER_SAMPLE);
  }

  fs.writeFileSync(target, buf);
  return target;
};

/** 供用例断言使用的夹具目录（便于失败时提示去哪找）。 */
export const fixtureDir = (): string => FIXTURE_DIR;
