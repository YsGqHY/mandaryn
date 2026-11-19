import { test } from '@playwright/test';

// 明确「不自动化」的场景清单。
//
// 这个文件不产出绿灯假象：每条都是一次 skip + 可读原因，并指向 docs/DEV-ISSUES-G-e2e.md
// 里对应的人工验证步骤。写在这里而不是干脆不写，是为了让「为什么没自动化」在
// 测试报告里直接可见 —— 否则下一个人只会看到「少了几条用例」。
//
// 判定标准：只有在「无法在隔离环境中真实触发」或「触发会动到本机真实资源」时才 skip；
// 能自动化的（录制、转写失败、回放高亮、崩溃恢复）都在别的 spec 里真跑。

test.describe.configure({ mode: 'serial' });

test('录制中设备切换 / 设备拔出——无法自动化', async () => {
  test.skip(
    true,
    '需要改动本机默认音频设备或禁用设备（系统级副作用），违反「E2E 不得改动本机默认音频设备」的铁律',
  );
  test.info().annotations.push({
    type: '人工验证',
    description:
      '录制中把默认输出设备切到另一个（或在设备管理器禁用当前设备）→ 期望：录制不停、' +
      '时间轴出现 device-switch 标记、结束后音频连续可回放。步骤见 G-E2E-06。',
  });
});

test('导出（另存为 txt/srt/csv/xlsx、另存录音文件）——server 模式无原生对话框', async () => {
  test.skip(
    true,
    'server 模式没有原生窗口：点「导出为纯文本」时运行时直接抛 "file dialogs not available in server mode"，Playwright 也无法驱动系统保存框',
  );
  test.info().annotations.push({
    type: '补充说明',
    description:
      '导出**引擎**本身已被 Go 单测覆盖（ROADMAP 14.1「导出服务（CSV / XLSX）有单测覆盖」），' +
      '未自动化的只是「原生另存为 → 后端落盘」这一段链路。人工步骤见 G-E2E-07。',
  });
});

test('边录边转 / 说话人分离——同样依赖模型包', async () => {
  test.skip(
    true,
    '两者都需要已安装的流式 / 声纹模型包；隔离数据目录里没有模型（启动日志即报 ERR_MODEL_NOT_FOUND 降级）',
  );
  test.info().annotations.push({
    type: '人工验证',
    description: '下载流式中文模型后打开「边录边转」开关录制；下载声纹模型后对已转写会话「发起分离」。步骤见 G-E2E-09。',
  });
});
