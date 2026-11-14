import type { Messages } from '@/i18n';

export const performanceZhCN: Messages = {
  settings: {
    performance: {
      title: '性能预算',
      hint: '设定本应用在整机上允许占用的份额。预算只约束推理（转写 / 说话人分离）的强度，不会中断录制与落盘。',
      cap: {
        label: '整机预算 N',
        value: '{{value}}%',
        aria: '整机性能预算',
        hint: '可选范围 10–100，步进 5。松开滑块即保存。',
      },
      formula: {
        title: '口径说明：这是「整机份额」，不是「本应用上限」',
        other: '其它进程占用 otherPct = 整机总占用 − 本应用占用',
        headroom: '本应用可用余量 headroom = N − otherPct',
        note: '也就是说：别的程序用得越多，留给本应用的额度就越少。N 调高一档，本应用能拿到的推理份额才更大 —— 但整机仍然不会被本应用占满。',
      },
      readout: {
        title: '实时读数',
        levelValue: '当前档位 {{level}}',
        levelUnknown: '当前档位 —',
        sampledAt: '最近采样 {{time}}',
        waiting: '正在等待第一次采样…',
      },
      levels: {
        L0: 'L0 全速：预算充裕，推理按最高档运行。',
        L1: 'L1 轻度降档：整机开始吃紧，推理降低线程 / 批次。',
        L2: 'L2 中度降档：推理进一步收敛，转写会变慢但仍在进行。',
        L3: 'L3 重度降档：只保留最低限度推理，尽量不打扰其它程序。',
        P: 'P 暂停推理：预算已耗尽，推理暂停，等余量恢复后自动继续。',
        unknown: '档位由后端上报，未知取值按原样显示。',
      },
      metrics: {
        total: '整机总占用',
        self: '本应用占用',
        other: '其它进程占用',
        headroom: '余量 headroom',
      },
      paused: {
        title: '推理已暂停（档位 P）',
        body: '录制与音频落盘不受影响，仍会继续采集并写入磁盘；转写任务会排队等余量恢复后自动继续。',
      },
      feedback: {
        saving: '正在保存预算…',
        saved: '预算已生效',
      },
      errors: {
        ERR_PERF_CAP_RANGE: '预算取值不合法：只接受 10–100 之间的数值，且必须是 5 的倍数（步进 5）。',
        ERR_DB_WRITE: '预算写入数据库失败，请稍后重试。',
        ERR_INVALID_ARGUMENT: '请求参数不合法，请重试。',
      },
    },
  },
};
