import { expect, type Locator, type Page } from '@playwright/test';

// 等应用就绪：启动骨架淡出（偏好 / 布局已从后端读回）
export const openApp = async (page: Page): Promise<void> => {
  await page.goto('/');
  await expect(page.locator('#startup-skeleton')).toHaveCount(0, { timeout: 10_000 });
};

// 等元素的包围盒连续两次采样一致再返回。
//
// 为什么需要：切布局后悬浮 Dock 的落位是由 effect 异步写入的（初值 → 吸附到某个角），
// 「可见」早于「位置定下来」。用例若在这两步之间测量手柄坐标并按下鼠标，pointerdown
// 会落在已经移走的元素上，拖动整体失效 —— 表现为「拖到左上角却仍停在右侧」的偶发失败
// （首轮更容易命中：布局刚切完、动画还没走完）。
export const waitForStableBox = async (locator: Locator, timeoutMs = 5_000): Promise<void> => {
  const deadline = Date.now() + timeoutMs;
  let previous = await locator.boundingBox();
  while (Date.now() < deadline) {
    await locator.page().waitForTimeout(120);
    const current = await locator.boundingBox();
    if (
      previous !== null &&
      current !== null &&
      Math.abs(previous.x - current.x) < 0.5 &&
      Math.abs(previous.y - current.y) < 0.5
    ) {
      return;
    }
    previous = current;
  }
  throw new Error('元素包围盒在 5s 内未稳定（布局动画未结束？）');
};

// 经设置页切换布局预设（与用户操作一致；写入真实后端 preferences）
//
// 点完 radio 后等 <main> 的包围盒稳定再返回：布局切换会重排整页，紧接着测量坐标
// （悬浮 Dock 用例就是这么做的）会在重排中间取到过渡值。
export const chooseLayout = async (page: Page, label: string): Promise<void> => {
  await page.getByRole('button', { name: '设置' }).first().click();
  await page.getByText('布局', { exact: true }).first().click();
  await page.getByRole('radio', { name: new RegExp(label) }).click();
  await waitForStableBox(page.locator('main'));
};

//
// 这些用例各自拉起自己的 server 进程（端口不同），因此不能依赖配置里的 baseURL，
// 一律用显式地址打开应用。

export const openAppAt = async (page: Page, baseUrl: string): Promise<void> => {
  await page.goto(`${baseUrl}/`);
  await expect(page.locator('#startup-skeleton')).toHaveCount(0, { timeout: 10_000 });
};

// 导航到主页面：录制 / 会话库 / 模型 / 设置
export const navTo = async (page: Page, label: string): Promise<void> => {
  await page.getByRole('button', { name: label }).first().click();
  await expect(page.locator('main')).toBeVisible();
};

// 选「默认设备」回环源（E2E 唯一可用的真实采集路径：进程级捕获需 Win10 20348+）。
export const pickDefaultDevice = async (page: Page): Promise<void> => {
  await page.locator('[aria-label^="选择音频源"]').filter({ hasText: '默认设备' }).first().click();
};

// 让浏览器真的出声的启动参数：Playwright 默认带 --mute-audio，页面里播放的声音到不了扬声器，
// 设备回环也就录不到；同时放开自动播放限制（page.evaluate 不算用户手势）。
// 用法：在需要测试音的 spec 里 `test.use({ launchOptions: TEST_TONE_LAUNCH })`。
export const TEST_TONE_LAUNCH = {
  ignoreDefaultArgs: ['--mute-audio'],
  args: ['--autoplay-policy=no-user-gesture-required'],
};

// 从默认扬声器播放一段小音量正弦波（440Hz，增益 0.03 ≈ -30dBFS），不等它播完就返回。
//
// 为什么需要：空录音策略（FR-03-7）会把全程静音的会话判为 failed，用例不能依赖
// 「跑测试时本机恰好在放音乐」。默认设备回环录的就是这台机器的混音，因此页面自己出声即可。
// 0.03 远高于静音判定阈值（0.001 ≈ -60dBFS），留出系统音量偏低时的余量，听感很轻。
export const playTestTone = async (page: Page, seconds: number): Promise<void> => {
  await page.evaluate(async (durationSec: number) => {
    const ctx = new AudioContext();
    await ctx.resume();
    const osc = ctx.createOscillator();
    osc.frequency.value = 440;
    const gain = ctx.createGain();
    gain.gain.value = 0.03;
    osc.connect(gain).connect(ctx.destination);
    osc.onended = () => void ctx.close();
    osc.start();
    osc.stop(ctx.currentTime + durationSec);
  }, seconds);
};

// 开始录制并保持 n 秒（录制是否真的起来以「停止」可用为准，不用固定 sleep 猜状态）。
//
// 录制期间同时播放测试音（见 playTestTone），录到的不是静音，会话能正常落成 completed；
// 调用方所在的 spec 必须 `test.use({ launchOptions: TEST_TONE_LAUNCH })`，否则浏览器静音、测试音出不来。
export const startRecording = async (page: Page, seconds: number): Promise<void> => {
  await page.getByRole('button', { name: '开始录制' }).click();
  await expect(page.getByRole('button', { name: '停止' })).toBeEnabled({ timeout: 15_000 });
  // 比录制多放 1 秒，保证点「停止」之前一直有声。
  await playTestTone(page, seconds + 1);
  await page.waitForTimeout(seconds * 1000);
};

// 正常停止并等收尾完成（completed = 音频已 Seal、库行已回填）。
export const stopRecording = async (page: Page): Promise<void> => {
  await page.getByRole('button', { name: '停止' }).click();
  await expect(page.locator('main')).toContainText('已完成', { timeout: 20_000 });
};

// 断言音频能真正播放：duration 有值、play() 成功、currentTime 会前进。
// 这是「可回放」的唯一诚实判据（不是「抽屉打开了」这种永真条件）。
export const expectAudioPlayable = async (page: Page): Promise<number> => {
  const audio = page.locator('audio').first();
  await expect(audio).toHaveAttribute('src', /\/audio\//);
  // 元数据加载完成后才有 duration；0 或 NaN 都说明文件/头有问题。
  await expect
    .poll(async () => audio.evaluate((el: HTMLAudioElement) => el.readyState), { timeout: 15_000 })
    .toBeGreaterThanOrEqual(1);
  const duration = await audio.evaluate((el: HTMLAudioElement) => el.duration);
  expect(Number.isFinite(duration)).toBe(true);
  expect(duration).toBeGreaterThan(0.5);

  const advanced = await audio.evaluate(async (el: HTMLAudioElement) => {
    await el.play();
    await new Promise((resolve) => setTimeout(resolve, 900));
    const time = el.currentTime;
    el.pause();
    return time;
  });
  expect(advanced).toBeGreaterThan(0.2);
  return duration;
};
