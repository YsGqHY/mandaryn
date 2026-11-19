import {
  ActionIcon,
  Alert,
  Button,
  Loader,
  Paper,
  ScrollArea,
  SegmentedControl,
  Stack,
  Text,
  Tooltip,
  UnstyledButton,
} from '@mantine/core';
import {
  IconAlertTriangle,
  IconApps,
  IconClock,
  IconDeviceFloppy,
  IconDownload,
  IconInfoCircle,
  IconMicrophone,
  IconPlayerPause,
  IconPlayerPlay,
  IconPlayerStop,
  IconRefresh,
  IconReload,
  IconSpeakerphone,
  IconTopologyStar3,
  IconWaveSine,
  IconX,
} from '@tabler/icons-react';
import clsx from 'clsx';
import type { ReactNode } from 'react';
import { useT } from '@/i18n';
import {
  NativeDialogs,
  RecordingStates,
  type AppError,
  type CaptureCaps,
  type EngineStatus,
  type ModelSource,
  type RecordingState,
  type Source,
} from '@/services';
import { useRecordPage, type LiveTranscript } from './useRecordPage';
import classes from './RecordPage.module.css';

// Record 页面（路由 id: record）：
//   1) 顶部能力提示条 —— GetCapabilities() 的结果；processLoopback=false 时显著展示 reason
//   2) 模型来源区 —— 本地 / 在线的当前来源（录制中禁止切换，控件禁用并说明原因）
//   3) 录制控制区 —— 开始 / 暂停 / 继续 / 停止 + 电平条 + 时长 / 已写字节 + 另存录音
//   4) 源列表 —— 应用 / 设备分组，选中项持久化到后端（SetPreferredSource，录制中禁改）
//
// 状态来源：`recording:state`（EV-01）/ `recording:level`（EV-02）/ `recording:tick`（EV-03）；
// 事件到达前用 GetState() 取初值（窗口重开后 UI 能恢复）。
// 所有人类可见文本走 t()；颜色只用 --fd-*；图标只用 @tabler/icons-react。
export const RecordPage = () => {
  const t = useT();
  const vm = useRecordPage();

  // 错误码 → 文案：命中 record.errors.<CODE> 用之，否则回落 errors.unknown 并带上后端 message。
  const errorText = (err: AppError): string => {
    // 空录音：停止与封存都成功，只是没录到声音。后端沿用 ERR_STATE_FORBIDDEN，
    // 用动作键 check-source 区分（见 internal/recording/doc.go EmptyRecordingAction），
    // 不能按码显示成「当前状态下不允许该操作」。
    if (err.code === 'ERR_STATE_FORBIDDEN' && err.action === 'check-source') {
      return t('record.errors.emptyRecording');
    }
    const key = `record.errors.${err.code}`;
    const translated = t(key, { message: err.message });
    if (err.code !== '' && translated !== key) return translated;
    return t('record.errors.unknown', { message: err.message });
  };

  // 引擎失败原因：命中错误码文案就用它（如缺模型 → 去「模型」页），否则给通用说明。
  const engineFailureText = (code: string): string => {
    const key = `record.errors.${code}`;
    const translated = code !== '' ? t(key) : key;
    return translated !== key ? translated : t('record.model.failedHint');
  };

  // 录制主状态 → 文案（ST-01~ST-09 中会出现在 state 上的取值）。
  const stateText = (state: string): string => {
    const key = `record.state.${state}`;
    const translated = t(key);
    return translated === key ? state : translated;
  };

  const caps: CaptureCaps | null = vm.caps;
  const processBlocked = caps !== null && !caps.processLoopback;
  const stateTone = (state: string): string => {
    switch (state) {
      case RecordingStates.Recording:
        return 'danger';
      case RecordingStates.Paused:
        return 'warning';
      case RecordingStates.Completed:
        return 'success';
      case RecordingStates.Failed:
        return 'danger';
      case RecordingStates.Interrupted:
        return 'warning';
      default:
        return 'muted';
    }
  };

  // 电平条：RMS 画常规电平，Peak 决定削波指示（峰值贴顶 = 可能削波）。
  const rmsPercent = Math.round(Math.min(1, vm.metrics.rms) * 100);
  const peakPercent = Math.round(Math.min(1, vm.metrics.peak) * 100);
  const clipping = vm.metrics.peak >= 0.99;

  // 另存录音：路径必须先由原生「另存为」拿到（禁止自己拼路径）。
  const handleExportAudio = async (): Promise<void> => {
    if (!vm.canExportAudio) return;
    const format = vm.recording?.audioFormat === 'flac' ? 'flac' : 'wav';
    const path = await NativeDialogs.saveFile({
      title: t('record.export.dialogTitle'),
      filename: `${t('record.export.fileBaseName')}.${format}`,
      filters: [{ displayName: t(format === 'flac' ? 'record.export.filterFlac' : 'record.export.filterWav'), pattern: `*.${format}` }],
    });
    if (path === null) return;
    const saved = await vm.exportAudio(path);
    if (saved === null) return;
    await NativeDialogs.info(t('record.export.doneTitle'), t('record.export.doneMessage', { path: saved }));
  };

  return (
    <div className={classes.root}>
      <div className={classes.body}>
        <div>
          <Text className={classes.eyebrow}>{t('record.eyebrow')}</Text>
          <h1 className={classes.title}>{t('record.title')}</h1>
        </div>
        <Text className={classes.subtitle}>{t('record.subtitle')}</Text>

        {/* —— 能力提示条 —— */}
        <Alert
          className={classes.capsAlert}
          radius="md"
          icon={<IconInfoCircle size={20} stroke={1.75} className={classes.capsIcon} />}
          title={t('record.caps.title')}
        >
          <Stack gap={4}>
            {caps === null ? (
              <Text className={classes.capsLine}>{t('record.caps.loading')}</Text>
            ) : (
              <>
                <Text className={classes.capsLine}>
                  {caps.processLoopback ? t('record.caps.processOn') : t('record.caps.processOff')}
                </Text>
                <Text className={classes.capsLine}>
                  {caps.deviceLoopback ? t('record.caps.deviceOn') : t('record.caps.deviceOff')}
                </Text>
                {caps.requiresPermission && (
                  <Text className={classes.capsLine}>{t('record.caps.permission')}</Text>
                )}
                <Text className={classes.capsLine}>
                  {t('record.caps.platformLabel')}
                  {': '}
                  {caps.platform}
                </Text>
              </>
            )}
          </Stack>
        </Alert>

        {/* 不支持按应用分路时，把后端给的原因（含系统版本要求）单独显著展示 */}
        {processBlocked && caps !== null && (
          <Alert
            className={clsx(classes.capsAlert, classes.capsAlertWarn)}
            radius="md"
            icon={<IconAlertTriangle size={20} stroke={1.75} className={classes.capsIconWarn} />}
            title={t('record.caps.processOff')}
          >
            <Text className={classes.capsReason}>
              <span className={classes.capsReasonLabel}>{t('record.caps.reasonLabel')}</span>
              {/* reason 由后端按平台 + 系统版本生成（诊断文本），前端不做二次翻译 */}
              {caps.reason !== '' ? caps.reason : '—'}
            </Text>
          </Alert>
        )}

        {/* 选中的源失效：明确提示，不静默丢弃 */}
        {vm.selectedUnavailable && !vm.recordingActive && (
          <Alert
            className={clsx(classes.capsAlert, classes.capsAlertWarn)}
            radius="md"
            icon={<IconAlertTriangle size={20} stroke={1.75} className={classes.capsIconWarn} />}
          >
            <Text className={classes.capsLine}>
              {vm.selectedSource !== null
                ? t('record.sources.unavailableBanner', { name: vm.selectedSource.displayName })
                : t('record.sources.unknownUnavailable')}
            </Text>
          </Alert>
        )}

        {vm.noAudioPersist && (
          <Alert
            className={clsx(classes.capsAlert, classes.capsAlertWarn)}
            icon={<IconAlertTriangle size={20} stroke={1.75} className={classes.capsIconWarn} />}
            title={t('record.noAudio.title')}
          >
            <Text className={classes.capsLine}>
              {t(vm.noAudioTranscriptionFailed ? 'record.noAudio.failed' : 'record.noAudio.hint')}
            </Text>
          </Alert>
        )}

        {/* 错误提示（后端结构化错误 → i18n） */}
        {vm.error && (
          <div className={classes.errorRow}>
            <Text className={classes.error}>{errorText(vm.error)}</Text>
            <ActionIcon
              variant="subtle"
              aria-label={t('record.errors.dismiss')}
              onClick={vm.dismissError}
            >
              <IconX size={16} stroke={1.75} />
            </ActionIcon>
          </div>
        )}

        {/* —— 模型来源：录制中禁止切换（后端会拒绝） —— */}
        <Paper className={classes.card} withBorder radius="md" p="lg">
          <div className={classes.cardHeader}>
            <div className={classes.cardTitleGroup}>
              <span className={classes.cardIcon}>
                <IconTopologyStar3 size={18} stroke={1.75} />
              </span>
              <Text className={classes.cardTitle}>{t('record.model.title')}</Text>
            </div>
            <Text className={classes.cardHint}>
              {vm.recordingActive ? t('record.model.lockedHint') : t('record.model.hint')}
            </Text>
          </div>

          <div className={classes.modelRow}>
            <Tooltip
              label={t('record.model.lockedHint')}
              withArrow
              multiline
              w={280}
              disabled={!vm.recordingActive}
            >
              <span className={classes.tooltipTarget}>
                <SegmentedControl
                  className={classes.modelSwitch}
                  value={modelSourceValue(vm.engine)}
                  disabled={vm.recordingActive}
                  onChange={(value) => void vm.setModelSource(value as ModelSource)}
                  data={[
                    { label: t('record.model.local'), value: 'local' },
                    { label: t('record.model.online'), value: 'online' },
                  ]}
                  aria-label={t('record.model.aria')}
                />
              </span>
            </Tooltip>
            <EngineChips engine={vm.engine} recording={vm.recording} />
          </div>
          {/* 引擎不可用时说明原因（典型：没装模型 → 引导去「模型」页）。只影响转写，录制照常可用。 */}
          {vm.engine?.state === 'failed' && (
            <Text className={classes.cardHint}>{engineFailureText(vm.engine.lastError?.code ?? '')}</Text>
          )}
        </Paper>

        {/* —— 录制控制 —— */}
        <Paper className={classes.card} withBorder radius="md" p="lg">
          <div className={classes.cardHeader}>
            <div className={classes.cardTitleGroup}>
              <span className={classes.cardIcon}>
                <IconMicrophone size={18} stroke={1.75} />
              </span>
              <Text className={classes.cardTitle}>{t('record.controls.title')}</Text>
              <span className={classes.chip} data-tone={stateTone(vm.recordingState)}>
                {stateText(vm.recordingState)}
              </span>
            </div>
            <Text className={classes.cardHint}>{t('record.controls.hint')}</Text>
          </div>

          <div className={classes.controlsRow}>
            <Tooltip
              label={t('record.controls.startHint')}
              withArrow
              multiline
              w={280}
              disabled={vm.canStart}
            >
              <span className={classes.tooltipTarget}>
                <Button
                  variant="filled"
                  disabled={!vm.canStart}
                  leftSection={<IconPlayerPlay size={18} stroke={1.75} />}
                  onClick={() => void vm.start()}
                >
                  {t('record.controls.start')}
                </Button>
              </span>
            </Tooltip>

            {/* 暂停 / 继续共用一个位置：录制动作用暂停，暂停态动作用继续 */}
            {vm.recordingState === RecordingStates.Paused ? (
              <Button
                variant="default"
                disabled={!vm.canResume}
                leftSection={<IconPlayerPlay size={18} stroke={1.75} />}
                onClick={() => void vm.resume()}
              >
                {t('record.controls.resume')}
              </Button>
            ) : (
              <Button
                variant="default"
                disabled={!vm.canPause}
                leftSection={<IconPlayerPause size={18} stroke={1.75} />}
                onClick={() => void vm.pause()}
              >
                {t('record.controls.pause')}
              </Button>
            )}

            <Button
              variant="default"
              disabled={!vm.canStop}
              leftSection={<IconPlayerStop size={18} stroke={1.75} />}
              onClick={() => void vm.stop()}
            >
              {t('record.controls.stop')}
            </Button>

            <Button
              variant="subtle"
              disabled={!vm.canExportAudio}
              leftSection={<IconDeviceFloppy size={18} stroke={1.75} />}
              onClick={() => void handleExportAudio()}
            >
              {t('record.controls.exportAudio')}
            </Button>

            {/* 崩溃 / 断电后遗留的会话：允许修补为可回放 */}
            {vm.recordingState === RecordingStates.Interrupted && (
              <Button
                variant="default"
                disabled={vm.busy}
                leftSection={<IconReload size={18} stroke={1.75} />}
                onClick={() => void vm.recover()}
              >
                {t('record.controls.recover')}
              </Button>
            )}

            {vm.busy && <Loader size={16} />}
            {(vm.recordingState === RecordingStates.Preparing ||
              vm.recordingState === RecordingStates.Finalizing) && (
              <Text className={classes.inProgress}>{stateText(vm.recordingState)}…</Text>
            )}
          </div>

          {/* 电平与指标（只有存在录制快照时才显示） */}
          {vm.recording !== null && vm.recordingState !== RecordingStates.Idle && (
            <div className={classes.metricsBlock}>
              <div className={classes.levelRow}>
                <span className={classes.levelLabel}>{t('record.metrics.levelLabel')}</span>
                <div
                  className={classes.levelTrack}
                  role="progressbar"
                  aria-label={t('record.metrics.levelAria')}
                  aria-valuenow={rmsPercent}
                  aria-valuemin={0}
                  aria-valuemax={100}
                >
                  <div
                    className={classes.levelFill}
                    data-clipping={clipping ? 'true' : 'false'}
                    style={{ width: `${rmsPercent}%` }}
                  />
                </div>
                <span className={classes.levelPeak} data-clipping={clipping ? 'true' : 'false'}>
                  {t('record.metrics.peakLabel', { percent: peakPercent })}
                </span>
              </div>

              <div className={classes.metricGrid}>
                <div className={classes.metricItem}>
                  <span className={classes.metricLabel}>{t('record.metrics.elapsedLabel')}</span>
                  <span className={classes.metricValue}>
                    <IconClock size={14} stroke={1.75} />
                    {formatDuration(vm.metrics.recordedMs)}
                  </span>
                </div>
                <div className={classes.metricItem}>
                  <span className={classes.metricLabel}>{t('record.metrics.bytesLabel')}</span>
                  <span className={classes.metricValue}>
                    <IconDownload size={14} stroke={1.75} />
                    {formatBytes(vm.metrics.bytesWritten)}
                  </span>
                </div>
                <div className={classes.metricItem}>
                  <span className={classes.metricLabel}>{t('record.metrics.sourceLabel')}</span>
                  <span className={classes.metricValue}>{sourceLabel(vm.recording)}</span>
                </div>
              </div>

              {/* 静音超过 10s：非阻塞提示，**不**改变按钮状态 */}
              {vm.metrics.silent && (
                <Text className={classes.silentHint}>
                  {t('record.metrics.silentHint', {
                    seconds: Math.round(vm.metrics.silentMs / 1000),
                  })}
                </Text>
              )}
            </div>
          )}

          {/* —— 边录边转（FR-03-10，默认关）：开关开着才出现 —— */}
          <LiveTranscriptPanel live={vm.live} recordingState={vm.recordingState} />
        </Paper>

        {/* —— 源列表 —— */}
        <Paper className={classes.card} withBorder radius="md" p="lg">
          <div className={classes.cardHeader}>
            <div className={classes.cardTitleGroup}>
              <span className={classes.cardIcon}>
                <IconApps size={18} stroke={1.75} />
              </span>
              <Text className={classes.cardTitle}>{t('record.sources.title')}</Text>
            </div>
            <div className={classes.actionsRow}>
              {vm.recordingActive && (
                <Text className={classes.cardHint}>{t('record.sources.lockedHint')}</Text>
              )}
              <Tooltip label={t('record.sources.refresh')} withArrow>
                <ActionIcon
                  variant="default"
                  aria-label={t('record.sources.refresh')}
                  disabled={vm.refreshing}
                  onClick={() => void vm.refresh()}
                >
                  {vm.refreshing ? <Loader size={16} /> : <IconRefresh size={18} stroke={1.75} />}
                </ActionIcon>
              </Tooltip>
            </div>
          </div>

          {vm.loading ? (
            <Text className={classes.emptyHint}>{t('record.sources.refreshing')}</Text>
          ) : (
            <>
              <SourceGroup
                title={t('record.sources.groupApplications')}
                icon={<IconApps size={14} stroke={1.75} />}
                emptyHint={t('record.sources.emptyApplications')}
                sources={vm.applications}
                selectedId={vm.selectedId}
                locked={vm.recordingActive}
                onSelect={(source) => void vm.select(source)}
              />
              <SourceGroup
                title={t('record.sources.groupDevices')}
                icon={<IconSpeakerphone size={14} stroke={1.75} />}
                emptyHint={t('record.sources.emptyDevices')}
                sources={vm.devices}
                selectedId={vm.selectedId}
                locked={vm.recordingActive}
                onSelect={(source) => void vm.select(source)}
              />
            </>
          )}
        </Paper>
      </div>
    </div>
  );
};

// 引擎来源取值：本地 / 在线。未知（未读回）按本地展示。
const modelSourceValue = (engine: EngineStatus | null): ModelSource =>
  engine?.source === 'online' ? 'online' : 'local';

// 边录边转文本区（FR-03-10）。
//
// 只在「开关开着」时出现：默认关闭时录制页与之前**完全一致**（一个像素都不多）。
// 三种状态各自说清原因，不笼统显示「未开启」：
//   1. 能力不可用（缺流式模型包）→ 明确提示缺什么；
//   2. 能力可用但没在录 → 提示开始录制后出字；
//   3. 正在录 → 显示已定稿的句子 + 当前 partial。
interface LiveTranscriptPanelProps {
  live: LiveTranscript;
  recordingState: string;
}

const LiveTranscriptPanel = ({ live, recordingState }: LiveTranscriptPanelProps) => {
  const t = useT();
  if (!live.enabled) return null;

  const recording = recordingState === RecordingStates.Recording;
  const hasText = live.partial !== '' || live.finalized.length > 0;

  return (
    <div className={classes.livePanel} data-unavailable={!live.available || undefined}>
      <div className={classes.liveHeader}>
        <IconWaveSine size={16} stroke={1.75} />
        <Text className={classes.liveTitle}>{t('record.live.title')}</Text>
        <span className={classes.chip} data-tone={live.available ? 'accent' : 'muted'}>
          {live.available ? t('record.live.stateOn') : t('record.live.stateUnavailable')}
        </span>
      </div>

      {!live.available ? (
        // 能力不可用：把原因说清楚（缺模型包 / 本构建不含流式引擎）。
        <Text className={classes.liveHint}>
          {capReasonText(live.errorCode, live.reason, t)}
        </Text>
      ) : !recording && !hasText ? (
        <Text className={classes.liveHint}>{t('record.live.idleHint')}</Text>
      ) : (
        <>
          {live.finalized.length > 0 && (
            <div className={classes.liveFinalized}>
              {live.finalized.map((seg) => (
                <Text key={`${seg.seq}-${seg.startMs}`} className={classes.liveLine}>
                  <span className={classes.liveTime}>{formatDuration(seg.startMs)}</span>
                  {seg.text}
                </Text>
              ))}
            </div>
          )}
          {/* partial 是累积文本：**整体替换**显示，绝不追加（否则会重复很多遍）。 */}
          {live.partial !== '' && (
            <Text className={classes.livePartial}>{live.partial}</Text>
          )}
          {live.partial === '' && live.finalized.length === 0 && (
            <Text className={classes.liveHint}>{t('record.live.waiting')}</Text>
          )}
        </>
      )}
    </div>
  );
};

// capReasonText 把后端不可用原因映射成 i18n 文案（查不到就显示后端原文）。
const capReasonText = (
  code: string,
  reason: string,
  t: ReturnType<typeof useT>,
): string => {
  const key = code ? `record.live.capErrors.${code}` : '';
  if (key !== '') {
    const translated = t(key);
    if (translated !== key) return translated;
  }
  return reason !== '' ? reason : t('record.live.stateUnavailable');
};

// 录制快照里的来源展示：优先会话绑定的 sourceId（录制中就换了列表也不影响事实）。
const sourceLabel = (recording: RecordingState | null): string => {
  if (recording === null) return '—';
  return recording.sourceId !== '' ? recording.sourceId : '—';
};

// 引擎状态 chip（state + 降级 / 预算标记），正交标记不写进主状态。
const EngineChips = ({
  engine,
  recording,
}: {
  engine: EngineStatus | null;
  recording: RecordingState | null;
}) => {
  const t = useT();
  if (engine === null) {
    return <Text className={classes.cardHint}>{t('record.model.loading')}</Text>;
  }
  const stateKey = `record.model.state.${engine.state}`;
  const translated = t(stateKey);
  return (
    <div className={classes.chipRow}>
      <span className={classes.chip} data-tone={engine.state === 'failed' ? 'danger' : 'muted'}>
        {translated === stateKey ? engine.state : translated}
      </span>
      {engine.engineId !== '' && <span className={classes.chip}>{engine.engineId}</span>}
      {engine.degraded && (
        <Tooltip label={engine.degradeNote} withArrow multiline w={280} disabled={engine.degradeNote === ''}>
          <span className={classes.chip} data-tone="warning">
            {t('record.model.degraded')}
          </span>
        </Tooltip>
      )}
      {recording?.throttled && (
        <span className={classes.chip} data-tone="warning">
          {t('record.model.throttled', { level: recording.throttleLevel })}
        </span>
      )}
      {recording?.suspended && (
        <span className={classes.chip} data-tone="warning">
          {t('record.model.suspended')}
        </span>
      )}
      {recording?.lagging && (
        <span className={classes.chip} data-tone="muted">
          {t('record.model.lagging')}
        </span>
      )}
    </div>
  );
};

// —— 源分组（仅本页使用，不进 index.ts） ——
interface SourceGroupProps {
  title: string;
  icon: ReactNode;
  emptyHint: string;
  sources: Source[];
  selectedId: string;
  // 录制中禁止切源（当前会话已绑定采集流）
  locked: boolean;
  onSelect: (source: Source) => void;
}

const SourceGroup = ({
  title,
  icon,
  emptyHint,
  sources,
  selectedId,
  locked,
  onSelect,
}: SourceGroupProps) => {
  return (
    <div className={classes.groupBlock}>
      <div className={classes.groupHeader}>
        {icon}
        <span>{title}</span>
        <span className={classes.groupCount}>{sources.length}</span>
      </div>
      {sources.length === 0 ? (
        <Text className={classes.emptyHint}>{emptyHint}</Text>
      ) : (
        <ScrollArea.Autosize mah={320} type="auto">
          <div className={classes.sourceList}>
            {sources.map((source) => (
              <SourceRow
                key={source.id}
                source={source}
                selected={source.id === selectedId}
                locked={locked}
                onSelect={onSelect}
              />
            ))}
          </div>
        </ScrollArea.Autosize>
      )}
    </div>
  );
};

interface SourceRowProps {
  source: Source;
  selected: boolean;
  locked: boolean;
  onSelect: (source: Source) => void;
}

const SourceRow = ({ source, selected, locked, onSelect }: SourceRowProps) => {
  const t = useT();
  // 不可用的源：语义上 disabled，但仍可 hover 出 Tooltip 说明原因
  //（原生 disabled 的按钮不派发指针事件，Tooltip 就永远不出现）。
  const meta =
    source.exeName !== '' ? source.exeName : source.deviceId !== '' ? source.deviceId : source.id;
  const disabled = !source.present || locked;

  // warn 是后端给的非阻塞警告**键**（如 exclusive-mode / process-loopback-unavailable），
  // 文案必须按键查 i18n；查不到就整条隐藏，而不是把所有键都渲染成「独占模式」——
  // 那会把「本机不支持按应用分路」误报成音频独占问题，误导用户。
  const warnKey = source.warn !== '' ? `record.sources.warn.${source.warn}` : '';
  const warnTranslated = warnKey !== '' ? t(warnKey) : '';
  const warnText = warnTranslated !== warnKey ? warnTranslated : '';

  return (
    <Tooltip
      label={locked ? t('record.sources.lockedTooltip') : t('record.sources.absentTooltip')}
      disabled={!disabled}
      withArrow
      multiline
      w={280}
    >
      <UnstyledButton
        type="button"
        className={clsx(
          classes.sourceRow,
          selected && classes.sourceRowSelected,
          disabled && classes.sourceRowAbsent,
        )}
        aria-pressed={selected}
        aria-disabled={disabled}
        aria-label={t('record.sources.selectAria', { name: source.displayName })}
        onClick={() => {
          if (disabled) return;
          onSelect(source);
        }}
      >
        <span className={classes.sourceMain}>
          <span className={classes.sourceName}>{source.displayName}</span>
          <span className={classes.sourceMeta}>{meta}</span>
        </span>
        <span className={classes.sourceBadges}>
          {/* 默认输出设备：由后端结构化字段 isDefault 标记，文案走 i18n
              （后端不再往 displayName 里拼「（默认）」，避免硬编码中文）。 */}
          {source.isDefault && (
            <span className={classes.chip} data-tone="accent">
              {t('record.sources.defaultDevice')}
            </span>
          )}
          {source.childCount > 0 && (
            <span className={classes.chip} data-tone="muted">
              {t('record.sources.childCount', { count: source.childCount })}
            </span>
          )}
          {source.producing && (
            <span className={classes.chip} data-tone="accent">
              {t('record.sources.producing')}
            </span>
          )}
          {warnText !== '' && (
            <span className={classes.chip} data-tone="warning">
              {warnText}
            </span>
          )}
          <span className={classes.chip} data-tone={source.present ? 'success' : 'muted'}>
            {source.present ? t('record.sources.present') : t('record.sources.absent')}
          </span>
          {selected && (
            <span className={classes.chip} data-tone="accent">
              {t('record.sources.selected')}
            </span>
          )}
        </span>
      </UnstyledButton>
    </Tooltip>
  );
};

const KB = 1024;
const MB = KB * 1024;
const GB = MB * 1024;

// 字节格式化（与模型页 / 数据库设置页一致的朴素实现）。
const formatBytes = (n: number): string => {
  if (n < KB) return `${n} B`;
  if (n < MB) return `${(n / KB).toFixed(1)} KB`;
  if (n < GB) return `${(n / MB).toFixed(1)} MB`;
  return `${(n / GB).toFixed(2)} GB`;
};

// 毫秒 → "mm:ss" / "h:mm:ss"。入参永远是后端帧计数换算值，不做前端自算。
const formatDuration = (ms: number): string => {
  const totalSeconds = Math.max(0, Math.round(ms / 1000));
  const hours = Math.floor(totalSeconds / 3600);
  const minutes = Math.floor((totalSeconds % 3600) / 60);
  const seconds = totalSeconds % 60;
  const pad = (v: number): string => (v < 10 ? `0${v}` : String(v));
  return hours > 0 ? `${hours}:${pad(minutes)}:${pad(seconds)}` : `${minutes}:${pad(seconds)}`;
};
