import { Button, Select, Switch, Text } from '@mantine/core';
import {
  IconAlertTriangle,
  IconCpu,
  IconMicrophone2,
  IconWaveSine,
} from '@tabler/icons-react';
import { useT } from '@/i18n';
import { useAudio, type SaveStatus } from './useAudio';
import classes from './Audio.module.css';
import shared from '../SettingsPage.module.css';

// 只展示已有运行时消费的音频设置；FLAC、灵敏度尚未接线，不提供假控件。
export const Audio = () => {
  const vm = useAudio();
  const t = useT();

  const saving = vm.status.kind === 'saving';
  // 能力不可用时开关仍然可点（配置是配置），但必须给出原因。
  const unavailable = !vm.loading && !vm.available;
  const controlsDisabled = saving || vm.loading || vm.audio === null;
  const defaultId = vm.audio?.defaultSourceId ?? '';
  const sourceUnavailable = defaultId !== '' && !vm.sources.some((source) => source.id === defaultId && source.present);
  const sourceOptions = vm.sources.map((source) => ({
    value: source.id,
    label: source.displayName,
    disabled: !source.present,
  }));
  if (defaultId !== '' && !sourceOptions.some((source) => source.value === defaultId)) {
    sourceOptions.push({ value: defaultId, label: t('settings.audio.defaults.unavailableOption'), disabled: true });
  }

  return (
    <div className={shared.section}>
      <div className={shared.sectionHeader}>
        <Text className={shared.sectionTitle}>{t('settings.audio.title')}</Text>
        <Text className={shared.sectionHint}>{t('settings.audio.hint')}</Text>
      </div>

      <div className={classes.card} data-testid="audio-defaults">
        <Select
          label={t('settings.audio.defaults.source')}
          description={t('settings.audio.defaults.sourceHint')}
          placeholder={t('settings.audio.defaults.noSource')}
          value={defaultId || null}
          data={sourceOptions}
          searchable
          nothingFoundMessage={t('settings.audio.defaults.noSources')}
          disabled={controlsDisabled}
          onChange={(id) => void vm.updateAudio({ defaultSourceId: id ?? '' })}
        />
        {sourceUnavailable && <Text role="status" className={classes.headDesc}>{t('settings.audio.defaults.unavailable')}</Text>}
        {vm.sourcesFailed && <Text role="status" className={classes.headDesc}>{t('settings.audio.defaults.sourcesFailed')}</Text>}
        {defaultId !== '' && (
          <Button variant="default" size="xs" disabled={controlsDisabled}
            onClick={() => void vm.updateAudio({ defaultSourceId: '' })}>
            {t('settings.audio.defaults.clearSource')}
          </Button>
        )}
        <Button variant="default" size="xs" disabled={saving || vm.loading} onClick={() => void vm.refresh()}>
          {t('settings.audio.defaults.refresh')}
        </Button>
        <Select
          label={t('settings.audio.defaults.maxSpeakers')}
          description={t('settings.audio.defaults.maxSpeakersHint')}
          data={[{ value: '0', label: t('settings.audio.defaults.auto') }, ...[2, 3, 4, 5, 6, 7, 8].map((count) => ({ value: String(count), label: t('settings.audio.defaults.speakers', { count }) }))]}
          value={String(vm.audio?.maxSpeakers ?? 0)}
          allowDeselect={false}
          disabled={controlsDisabled}
          onChange={(value) => { if (value !== null) void vm.updateAudio({ maxSpeakers: Number(value) }); }}
        />
        <Select
          label={t('settings.audio.defaults.sensitivity')}
          description={t('settings.audio.defaults.sensitivityHint')}
          data={[
            { value: 'standard', label: t('settings.audio.defaults.standard') },
            { value: 'noisy', label: t('settings.audio.defaults.noisy') },
          ]}
          value={vm.audio?.sensitivity === 'noisy' ? 'noisy' : 'standard'}
          allowDeselect={false}
          disabled={controlsDisabled}
          onChange={(value) => { if (value === 'standard' || value === 'noisy') void vm.updateAudio({ sensitivity: value }); }}
        />
        <Switch
          label={t('settings.audio.defaults.autoName')}
          description={t('settings.audio.defaults.autoNameHint')}
          checked={vm.audio?.autoName ?? false}
          disabled={controlsDisabled}
          onChange={(event) => void vm.updateAudio({ autoName: event.currentTarget.checked })}
        />
        <Select
          label={t('settings.audio.defaults.format')}
          description={t('settings.audio.defaults.formatHint')}
          data={[{ value: 'wav', label: 'WAV' }, { value: 'flac', label: 'FLAC' }]}
          value={vm.audio?.saveFormat === 'flac' ? 'flac' : 'wav'}
          allowDeselect={false}
          disabled={controlsDisabled}
          onChange={(value) => { if (value === 'wav' || value === 'flac') void vm.updateAudio({ saveFormat: value }); }}
        />
        <StatusLine status={vm.status} t={t} />
      </div>

      <div className={classes.card}>
        <div className={classes.headRow}>
          <IconMicrophone2 size={20} stroke={1.75} className={classes.headIcon} />
          <div className={classes.headTexts}>
            <Text className={classes.headTitle}>{t('settings.audio.live.title')}</Text>
            <Text className={classes.headDesc}>{t('settings.audio.live.desc')}</Text>
          </div>
          <span className={classes.stateBadge} data-on={vm.enabled || undefined}>
            {vm.enabled ? t('settings.audio.live.stateOn') : t('settings.audio.live.stateOff')}
          </span>
        </div>

        <div className={classes.switchRow}>
          <Switch
            checked={vm.enabled}
            onChange={(event) => void vm.toggle(event.currentTarget.checked)}
            disabled={saving || vm.loading}
            label={t('settings.audio.live.switchLabel')}
            aria-label={t('settings.audio.live.switchLabel')}
          />
        </div>

        {/* —— 三条边界说明：默认关闭 / 下一个会话生效 / 不影响录完再转 —— */}
        <ul className={classes.notes}>
          <li className={classes.note}>
            <IconWaveSine size={18} stroke={1.75} className={classes.noteIcon} />
            <Text className={classes.noteText}>{t('settings.audio.live.noteDefault')}</Text>
          </li>
          <li className={classes.note}>
            <IconCpu size={18} stroke={1.75} className={classes.noteIcon} />
            <Text className={classes.noteText}>{t('settings.audio.live.noteNextSession')}</Text>
          </li>
          <li className={classes.note}>
            <IconCpu size={18} stroke={1.75} className={classes.noteIcon} />
            <Text className={classes.noteText}>{t('settings.audio.live.noteDeferred')}</Text>
          </li>
        </ul>

        {/* —— 能力状态：可用 / 缺流式模型包 / 本构建不含 —— */}
        <div className={classes.capRow} data-ok={vm.available || undefined}>
          <Text className={classes.capLabel}>{t('settings.audio.live.capLabel')}</Text>
          <Text className={classes.capValue}>
            {vm.available
              ? t('settings.audio.live.capReady', {
                  engine: vm.engineId !== '' ? vm.engineId : t('settings.audio.live.capReadyUnknown'),
                })
              : unavailable
                ? capReasonText(vm.errorCode, vm.reason, t)
                : t('settings.audio.live.capChecking')}
          </Text>
        </div>

        {unavailable && vm.enabled && (
          <div className={classes.warnBox} role="status">
            <IconAlertTriangle size={18} stroke={1.75} className={classes.warnIcon} />
            <Text className={classes.warnText}>{t('settings.audio.live.enabledButUnavailable')}</Text>
          </div>
        )}

      </div>
    </div>
  );
};

type T = ReturnType<typeof useT>;

// capReasonText 把后端的不可用原因映射成 i18n 文案（查不到就用后端原文）。
const capReasonText = (code: string, reason: string, t: T): string => {
  const key = code ? `settings.audio.live.capErrors.${code}` : '';
  if (key !== '') {
    const translated = t(key);
    if (translated !== key) return translated;
  }
  return reason !== '' ? reason : t('settings.audio.live.capUnavailable');
};

interface StatusLineProps {
  status: SaveStatus;
  t: T;
}

// 保存反馈：逐 kind 列出，TS 才能在 failed 分支收敛到 code / message。
const StatusLine = ({ status, t }: StatusLineProps) => {
  switch (status.kind) {
    case 'idle':
      return null;
    case 'saving':
      return <Text className={classes.status}>{t('settings.audio.feedback.saving')}</Text>;
    case 'saved':
      return <Text className={classes.status}>{t('settings.audio.feedback.saved')}</Text>;
    case 'failed': {
      // 录制中切换被拒：给出可操作的说明（停止录制后再设）。
      if (status.code === 'ERR_STATE_FORBIDDEN') {
        return (
          <div className={classes.errorLine} role="alert">
            <IconAlertTriangle size={18} stroke={1.75} />
            <Text className={classes.errorText}>{t('settings.audio.feedback.ERR_STATE_FORBIDDEN')}</Text>
          </div>
        );
      }
      const key = status.code ? `settings.audio.errors.${status.code}` : '';
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
