import { Slider, Text } from '@mantine/core';
import { IconAlertTriangle, IconGauge, IconPlayerPause, IconWaveSine } from '@tabler/icons-react';
import { useI18n, useT } from '@/i18n';
import { useLeanMode } from '@/shared/LeanModeProvider';
import {
  RESOURCE_CAP_MAX,
  RESOURCE_CAP_MIN,
  RESOURCE_CAP_STEP,
  isInferencePaused,
  perfLevelOf,
} from '@/services';
import { usePerformance, type PerformanceStatus } from './usePerformance';
import classes from './Performance.module.css';
import shared from '../SettingsPage.module.css';

// 性能预算子页面（NFR-08）：
//   1) 整机预算 N（10–100，步进 5）→ appsettings.SetResourceCap
//   2) 实时三分量读数（EV-15 perf:usage）：整机 / 本应用 / 其它进程 / 余量 + 当前档位
//   3) 口径说明：这是「整机份额」预算，不是「本应用上限」
//   4) 档位 P（暂停推理）时明确提示：录制与落盘不受影响
//
// 文案 namespace：settings.performance.*
export const Performance = () => {
  const vm = usePerformance();
  const t = useT();
  const { current } = useI18n();
  const showDetails = !useLeanMode().isRegionHidden('settings.advanced');

  const usage = vm.usage;
  const level = usage ? perfLevelOf(usage.level) : null;
  const paused = isInferencePaused(usage);
  const saving = vm.status.kind === 'saving';

  return (
    <div className={shared.section}>
      <div className={shared.sectionHeader}>
        <Text className={shared.sectionTitle}>{t('settings.performance.title')}</Text>
        <Text className={shared.sectionHint}>{t('settings.performance.hint')}</Text>
      </div>

      {/* —— 预算滑块 —— */}
      <div className={classes.card}>
        <div className={classes.capHeader}>
          <Text className={classes.capLabel}>{t('settings.performance.cap.label')}</Text>
          <Text className={classes.capValue}>{t('settings.performance.cap.value', { value: vm.draft })}</Text>
        </div>
        <Slider
          className={classes.slider}
          min={RESOURCE_CAP_MIN}
          max={RESOURCE_CAP_MAX}
          step={RESOURCE_CAP_STEP}
          value={vm.draft}
          onChange={vm.setDraft}
          onChangeEnd={() => void vm.commit()}
          disabled={saving}
          label={(value) => `${value}%`}
          marks={[
            { value: RESOURCE_CAP_MIN, label: `${RESOURCE_CAP_MIN}%` },
            { value: 50, label: '50%' },
            { value: RESOURCE_CAP_MAX, label: `${RESOURCE_CAP_MAX}%` },
          ]}
          thumbLabel={t('settings.performance.cap.aria')}
        />
        <Text className={classes.capHint}>{t('settings.performance.cap.hint')}</Text>

        {/* 口径说明：预算按「整机份额」算，不是「本应用上限」 */}
        {showDetails && (
          <div className={classes.formulaBox}>
            <IconWaveSine size={20} stroke={1.75} className={classes.formulaIcon} />
            <div className={classes.formulaTexts}>
              <Text className={classes.formulaTitle}>{t('settings.performance.formula.title')}</Text>
              <Text className={classes.formulaLine}>{t('settings.performance.formula.other')}</Text>
              <Text className={classes.formulaLine}>{t('settings.performance.formula.headroom')}</Text>
              <Text className={classes.formulaNote}>{t('settings.performance.formula.note')}</Text>
            </div>
          </div>
        )}

        <StatusLine status={vm.status} t={t} />
      </div>

      {/* —— 实时读数（详细采样可收起，暂停告警始终保留） —— */}
      {(showDetails || paused) && (
        <div className={classes.card}>
          {showDetails && (
            <>
              <div className={classes.readoutHeader}>
                <IconGauge size={20} stroke={1.75} className={classes.readoutIcon} />
                <Text className={classes.readoutTitle}>{t('settings.performance.readout.title')}</Text>
                <span className={classes.levelBadge} data-paused={paused || undefined}>
                  {usage
                    ? t('settings.performance.readout.levelValue', { level: usage.level })
                    : t('settings.performance.readout.levelUnknown')}
                </span>
              </div>
              <Text className={classes.readoutHint}>
                {level
                  ? t(`settings.performance.levels.${level}`)
                  : t('settings.performance.levels.unknown')}
              </Text>

              <div className={classes.readoutGrid}>
                <Metric
                  label={t('settings.performance.metrics.total')}
                  value={usage ? pct(usage.totalCpuPct) : null}
                />
                <Metric
                  label={t('settings.performance.metrics.self')}
                  value={usage ? pct(usage.selfCpuPct) : null}
                />
                <Metric
                  label={t('settings.performance.metrics.other')}
                  value={usage ? pct(usage.otherPct) : null}
                />
                <Metric
                  label={t('settings.performance.metrics.headroom')}
                  value={usage ? pct(usage.headroomPct) : null}
                  tone="accent"
                />
              </div>

              <Text className={classes.readoutMeta}>
                {usage && usage.sampledAt
                  ? t('settings.performance.readout.sampledAt', {
                      time: formatTime(usage.sampledAt, current.code),
                    })
                  : t('settings.performance.readout.waiting')}
              </Text>
            </>
          )}

          {paused && (
            <div className={classes.pausedBox} role="status">
              <IconPlayerPause size={20} stroke={1.75} className={classes.pausedIcon} />
              <div className={classes.pausedTexts}>
                <Text className={classes.pausedTitle}>{t('settings.performance.paused.title')}</Text>
                <Text className={classes.pausedBody}>{t('settings.performance.paused.body')}</Text>
              </div>
            </div>
          )}
        </div>
      )}
    </div>
  );
};

type T = ReturnType<typeof useT>;

interface MetricProps {
  label: string;
  // null 表示尚未收到采样（显示占位而不是 0，避免被误读成「真的没有占用」）。
  value: string | null;
  tone?: 'accent';
}

const Metric = ({ label, value, tone }: MetricProps) => (
  <div className={classes.metric} data-tone={tone}>
    <Text className={classes.metricLabel}>{label}</Text>
    <Text className={classes.metricValue}>{value ?? '…'}</Text>
  </div>
);

interface StatusLineProps {
  status: PerformanceStatus;
  t: T;
}

// 把每种 status.kind 显式列出，TS 才能在 'failed' 分支收敛到 code / message。
const StatusLine = ({ status, t }: StatusLineProps) => {
  switch (status.kind) {
    case 'idle':
      return null;
    case 'saving':
      return <Text className={classes.status}>{t('settings.performance.feedback.saving')}</Text>;
    case 'saved':
      return <Text className={classes.status}>{t('settings.performance.feedback.saved')}</Text>;
    case 'failed': {
      // 越界有专门文案（10–100，步进 5）；其它错误码按 code 查文案，查不到回原始消息。
      if (status.code === 'ERR_PERF_CAP_RANGE') {
        return (
          <div className={classes.errorLine} role="alert">
            <IconAlertTriangle size={18} stroke={1.75} />
            <Text className={classes.errorText}>{t('settings.performance.errors.ERR_PERF_CAP_RANGE')}</Text>
          </div>
        );
      }
      const key = status.code ? `settings.performance.errors.${status.code}` : '';
      const translated = key ? t(key) : '';
      const text = key && translated !== key ? translated : status.message;
      return (
        <div className={classes.errorLine} role="alert">
          <IconAlertTriangle size={18} stroke={1.75} />
          <Text className={classes.errorText}>{text}</Text>
        </div>
      );
    }
  }
};

// 保留 1 位小数（NFR-08 读数不需要更高精度）。
const pct = (value: number): string => `${value.toFixed(1)}%`;

// RFC3339Nano → 本地时间（只显示时分秒，日期由「最近采样」语义兜住）。
const formatTime = (rfc3339: string, localeCode: string): string => {
  const at = new Date(rfc3339);
  if (Number.isNaN(at.getTime())) return rfc3339;
  return at.toLocaleTimeString(localeCode);
};
