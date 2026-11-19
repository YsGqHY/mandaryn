import { spawn, spawnSync } from 'node:child_process';
import { randomInt, randomUUID } from 'node:crypto';
import fs from 'node:fs';
import path from 'node:path';

const WAV_HEADER_BYTES = 44;
const RECORDING_HZ = 16_000;
const TONE_WINDOW_FRAMES = RECORDING_HZ / 10;

// 用系统的 .NET Framework 编译一个单进程播放器。捕获层按 exe 文件名聚合，不能直接用
// powershell.exe / 浏览器充当音源。编译和播放均不启动 shell；产物仅在本用例输出目录内。
const PLAYER_SOURCE = `using System;
using System.Runtime.InteropServices;

internal static class Player {
    [DllImport("winmm.dll", EntryPoint = "PlaySoundW", CharSet = CharSet.Unicode, ExactSpelling = true)]
    private static extern bool PlaySound(string filename, IntPtr module, uint flags);

    private static int Main(string[] args) {
        const uint Filename = 0x00020000, Async = 1, NoDefault = 2, Loop = 8;
        if (!PlaySound(args[0], IntPtr.Zero, Filename | Async | NoDefault | Loop)) {
            Console.Error.WriteLine("PlaySoundW could not start the test WAV on the default output.");
            return 1;
        }
        // 没有子进程。Playwright 关闭这条专属 stdin（包括 worker 退出）就停止播放并退出。
        while (Console.Read() != -1) { }
        PlaySound(null, IntPtr.Zero, 0);
        return 0;
    }
}`;

function writeTone(file: string, tonesHz: readonly number[]): void {
  const sampleRate = 44_100;
  const channels = 2;
  const blockAlign = channels * 2;
  const wav = Buffer.alloc(WAV_HEADER_BYTES + sampleRate * blockAlign);
  wav.write('RIFF', 0);
  wav.writeUInt32LE(wav.length - 8, 4);
  wav.write('WAVEfmt ', 8);
  wav.writeUInt32LE(16, 16);
  wav.writeUInt16LE(1, 20);
  wav.writeUInt16LE(channels, 22);
  wav.writeUInt32LE(sampleRate, 24);
  wav.writeUInt32LE(sampleRate * blockAlign, 28);
  wav.writeUInt16LE(blockAlign, 32);
  wav.writeUInt16LE(16, 34);
  wav.write('data', 36);
  wav.writeUInt32LE(wav.length - WAV_HEADER_BYTES, 40);
  for (let frame = 0; frame < sampleRate; frame++) {
    const angle = (2 * Math.PI * frame) / sampleRate;
    const sample = Math.round(1400 * tonesHz.reduce((sum, hz) => sum + Math.sin(angle * hz), 0));
    for (let channel = 0; channel < channels; channel++) {
      wav.writeInt16LE(sample, WAV_HEADER_BYTES + frame * blockAlign + channel * 2);
    }
  }
  fs.writeFileSync(file, wav);
}

// 录制中的 WAV 头长度仍为 0；按实际字节读 16 kHz 单声道 PCM。用 Goertzel 检查两个
// 频点同时存在且占了该 100ms 窗口的大部分能量；有文件增长或单纯有电平不算命中。
export function matchingToneWindows(wav: Buffer, tonesHz: readonly number[]): number {
  if (wav.length < WAV_HEADER_BYTES || wav.toString('ascii', 36, 40) !== 'data' ||
      wav.readUInt32LE(24) !== RECORDING_HZ || wav.readUInt16LE(22) !== 1 || wav.readUInt16LE(34) !== 16) {
    return 0;
  }
  const coefficients = tonesHz.map((hz) => 2 * Math.cos((2 * Math.PI * hz) / RECORDING_HZ));
  let matches = 0;
  const blockBytes = TONE_WINDOW_FRAMES * 2;
  for (let start = WAV_HEADER_BYTES; start + blockBytes <= wav.length; start += blockBytes) {
    const prev = coefficients.map(() => 0);
    const prev2 = coefficients.map(() => 0);
    let power = 0;
    for (let frame = 0; frame < TONE_WINDOW_FRAMES; frame++) {
      const sample = wav.readInt16LE(start + frame * 2) / 32768;
      power += sample * sample;
      for (let i = 0; i < coefficients.length; i++) {
        const next = sample + coefficients[i] * prev[i] - prev2[i];
        prev2[i] = prev[i];
        prev[i] = next;
      }
    }
    const tonePowers = coefficients.map((c, i) =>
      2 * (prev[i] ** 2 + prev2[i] ** 2 - c * prev[i] * prev2[i]) / TONE_WINDOW_FRAMES,
    );
    if (tonePowers.every((p) => p > 0.001 ** 2 * TONE_WINDOW_FRAMES && p > 0.25 * power)) matches++;
  }
  return matches;
}

export function startWindowsRecordingPlayer(outputDir: string) {
  if (process.platform !== 'win32' || !process.env.SystemRoot) {
    throw new Error('测试专用播放器需要 Windows 与 SystemRoot。');
  }
  const compiler = ['Framework64', 'Framework']
    .map((arch) => path.join(process.env.SystemRoot!, 'Microsoft.NET', arch, 'v4.0.30319', 'csc.exe'))
    .find((candidate) => fs.existsSync(candidate));
  if (!compiler) throw new Error('找不到系统 .NET Framework 的 csc.exe，不能构建独立的测试音源。');

  fs.mkdirSync(outputDir, { recursive: true });
  const name = `mandaryn-e2e-player-${randomUUID()}`;
  const exe = path.join(outputDir, `${name}.exe`);
  const source = path.join(outputDir, 'player.cs');
  const tone = path.join(outputDir, 'tone.wav');
  const tonesHz = [10 * randomInt(50, 71), 10 * randomInt(90, 111)];
  fs.writeFileSync(source, PLAYER_SOURCE);
  writeTone(tone, tonesHz);
  const build = spawnSync(compiler, ['/nologo', '/target:exe', `/out:${exe}`, source], {
    windowsHide: true,
    encoding: 'utf8',
    timeout: 10_000,
  });
  if (build.error || build.status !== 0) {
    throw new Error(`无法构建测试专用播放器：${build.error ?? ''}\n${build.stdout ?? ''}${build.stderr ?? ''}`);
  }

  const child = spawn(exe, [tone], { windowsHide: true, stdio: ['pipe', 'ignore', 'pipe'] });
  let diagnostics = '';
  child.stderr.setEncoding('utf8').on('data', (text: string) => { diagnostics += text; });
  child.on('error', (err) => { diagnostics += String(err); });
  child.stdin.on('error', (err) => { diagnostics += String(err); });
  const closed = new Promise<number | null>((resolve) => child.once('close', resolve));
  const running = () => child.pid !== undefined && child.exitCode === null && child.signalCode === null;
  const assertRunning = () => {
    if (!running()) throw new Error(`测试专用播放器未在运行：${diagnostics || child.exitCode}`);
  };
  const waitForClose = async () => {
    let timer: ReturnType<typeof setTimeout> | undefined;
    try {
      return await Promise.race([
        closed,
        new Promise<never>((_, reject) => {
          timer = setTimeout(() => reject(new Error(`测试专用播放器退出超时：${diagnostics}`)), 5_000);
        }),
      ]);
    } finally {
      clearTimeout(timer);
    }
  };
  return {
    name,
    tonesHz,
    assertRunning,
    async exit() {
      assertRunning();
      child.stdin.end();
      const code = await waitForClose();
      if (code !== 0) throw new Error(`测试专用播放器退出失败（${code}）：${diagnostics}`);
    },
    async dispose() {
      // 只清理这个已持有的 ChildProcess，永不按 exe 名 / 枚举出的 PID 结束进程。
      if (running()) child.kill('SIGKILL');
      await waitForClose();
    },
  };
}
