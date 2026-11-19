import { memo, useCallback, useLayoutEffect, useRef, useState } from 'react';
import {
  ActionIcon,
  Alert,
  Button,
  Drawer,
  EmptyState,
  Group,
  Loader,
  Menu,
  Slider,
  Stack,
  Text,
  Tooltip,
} from '@mantine/core';
import {
  IconAlertCircle,
  IconAlertTriangle,
  IconArrowsExchange,
  IconCheck,
  IconClockPause,
  IconClockPlay,
  IconFlag,
  IconFocusCentered,
  IconGauge,
  IconPlayerPause,
  IconPlayerPlay,
  IconPlayerTrackNext,
  IconPlayerTrackPrev,
  IconPlugConnectedX,
  IconReload,
  IconRewindBackward5,
  IconRewindForward5,
  IconVolume,
  IconVolumeOff,
  IconWaveSine,
} from '@tabler/icons-react';
import { useI18n, useT } from '@/i18n';
import {
  PLAYBACK_PAGE_SIZE,
  PLAYBACK_RATES,
  SEEK_STEP_MS,
  VOLUME_STEP,
  formatTime,
  type AppError,
  type Marker,
  type Segment,
  type Session,
} from '@/services';
import { usePlayback, type PlaybackSlot, type SpeakerInfo } from './usePlayback';
import classes from './PlaybackDrawer.module.css';

export interface PlaybackDrawerProps {
  session: Session;
  onClose: () => void;
}

// 抽屉宽度：大半宽但不撑满，左侧始终留出会话库的可见部分（可读 + 知道自己在哪）。
const DRAWER_SIZE = 'min(1040px, 94vw)';

// 会话回放抽屉（FR-04 的核心界面）：
//   1) 播放器 —— 播放 / 暂停、±5s、上下一句、倍速、音量、可拖进度条
//   2) 歌词式列表 —— 片段（时间戳 + 说话人色点 / 角色名 + 正文）+ 时间轴标记分隔行
//   3) 同步 —— 当前片段高亮并居中；手动滚动暂停自动居中，浮出「回到当前句」
//   4) 拖拽 —— 拖动进度条即时预览高亮（不 seek、不解码），释放后 seek 并归位
//   5) 点击片段 —— 跳到该时刻并成为当前片段（不自动播放）
//
// 音频走同源 HTTP 流（/audio/{id}，后端支持 Range），用原生 <audio> 播放；
// 不读 base64、不整段 fetch，避免把长录音塞进内存。
//
// 超大通话（E-06）：
//   - 片段**分页**加载，首屏只请求 PLAYBACK_PAGE_SIZE 条（200 条），剩余页由滚动 /
//     播放位置驱动补齐（见 usePagedTimeline）；
//   - 列表**窗口化**：只渲染可视区 ± SLOT_OVERSCAN 行，未加载页的槽位渲染占位行，
//     行高实测 + 估算撑开滚动条（见 services/playback/virtualRows.ts）。
//   列表容器上的 data-* 属性（window / total / page-size）是给开发者核对
//   「首屏只要了 N 条、当前渲染多少行」用的，不影响样式。
export const PlaybackDrawer = ({ session, onClose }: PlaybackDrawerProps) => {
  const t = useT();
  const { current } = useI18n();
  const vm = usePlayback({ session });
  // 挂载即打开；关闭走「先播滑出动画 → 动画结束再交给父组件卸载」，
  // 否则父组件的条件渲染会让抽屉瞬间消失（也没有退出动画）。
  const [opened, setOpened] = useState<boolean>(true);

  // audioRef 是回调 ref（不是 RefObject），故这里不能取 .current；
  // 关闭时由元素自身的 ref 回调持有，改用 DOM 查询会绕开 React，因此保留一个
  // 本地引用：ref 回调里同时记到本地 ref，关闭时暂停它。
  const audioElRef = useRef<HTMLAudioElement | null>(null);
  const setAudioRef = useCallback(
    (element: HTMLAudioElement | null): void => {
      audioElRef.current = element;
      vm.audioRef(element);
    },
    [vm.audioRef],
  );

  const handleClose = useCallback((): void => {
    // 动画期间不再出声：点关闭就是不想听了。
    audioElRef.current?.pause();
    setOpened(false);
  }, []);

  const errorText = (err: AppError): string => {
    const key = `library.errors.${err.code}`;
    const translated = t(key, { message: err.message });
    if (err.code !== '' && translated !== key) return translated;
    return t('library.errors.unknown', { message: err.message });
  };

  const title = session.title !== '' ? session.title : session.id;
  const noSavedAudio = session.hasAudio === false;
  const canPlayAudio = !noSavedAudio && vm.audioExists;
  const noAudio = noSavedAudio ? 'audioNotSaved' : 'audioMissing';

  // 拖动中的预览位置也走 slider value，所以进度条拖到哪、气泡就显示哪。
  const sliderMax = Math.max(1, Math.round(vm.durationMs));
  const sliderValue = Math.min(sliderMax, Math.max(0, Math.round(vm.currentMs)));

  // 必须稳定：PlaybackLyrics 是 memo 组件，回调每次重建会让整份列表白重渲染。
  const seekTo = vm.seekTo;
  const handleJump = useCallback(
    (segment: Segment): void => {
      seekTo(segment.startMs);
    },
    [seekTo],
  );

  return (
    <Drawer
      opened={opened}
      onClose={handleClose}
      transitionProps={{ duration: 150, onExited: onClose }}
      position="right"
      size={DRAWER_SIZE}
      title={
        <div className={classes.titleBlock}>
          <Text className={classes.titleText} title={title}>
            {title}
          </Text>
          <Text className={classes.titleMeta}>
            {t('library.playback.titleMeta', {
              time: formatStartedAt(session.startedAt, current.code),
              source: session.sourceName !== '' ? session.sourceName : session.sourceId,
              count: session.segmentCount,
            })}
          </Text>
        </div>
      }
      closeButtonProps={{ 'aria-label': t(noSavedAudio ? 'library.playback.closeTranscript' : 'library.playback.close') }}
      classNames={{ body: classes.drawerBody, header: classes.drawerHeader, content: classes.drawerContent }}
    >
      {/* 原生音频元素：隐藏不渲染控件，全部交互走下面的自定义播放条。
          key 变化 = 重试加载（仅换 key 才会重新发起请求）。 */}
      {canPlayAudio && (
        <audio
          ref={setAudioRef}
          key={vm.audioKey}
          src={vm.audioSrc}
          preload="metadata"
          className={classes.audioSink}
          onError={vm.onAudioError}
          onLoadedMetadata={vm.onAudioLoadedMetadata}
          onEnded={vm.onAudioEnded}
        />
      )}

      {vm.loading ? (
        <div className={classes.centered}>
          <Loader size="sm" />
          <Text className={classes.hint}>{t('library.playback.loading')}</Text>
        </div>
      ) : vm.error !== null ? (
        <div className={classes.centered}>
          <Alert className={classes.errorAlert} icon={<IconAlertCircle size={18} stroke={1.75} />}>
            <Stack gap={8}>
              <Text className={classes.alertLine}>{errorText(vm.error)}</Text>
              <Group gap="xs">
                <Button
                  size="xs"
                  variant="default"
                  leftSection={<IconReload size={16} stroke={1.75} />}
                  onClick={() => void vm.reload()}
                >
                  {t('library.playback.retry')}
                </Button>
              </Group>
            </Stack>
          </Alert>
        </div>
      ) : (
        <div className={classes.root}>
          <div className={classes.lyricsWrap}>
            {!vm.hasSegments ? (
              <div className={classes.centered}>
                <EmptyState
                  icon={<IconWaveSine size={32} stroke={1.75} />}
                  title={t('library.playback.emptyTitle')}
                  description={emptyDescription(t, vm.transcriptionStatus, canPlayAudio)}
                />
              </div>
            ) : (
              <PlaybackLyrics
                slots={vm.slots}
                highlightIndex={vm.highlightIndex}
                contentHeight={vm.contentHeight}
                windowStart={vm.windowStart}
                windowEnd={vm.windowEnd}
                totalSegments={vm.totalSegments}
                listRef={vm.listRef}
                onListScroll={vm.onListScroll}
                onMeasured={vm.reportSlotHeight}
                onJump={canPlayAudio ? handleJump : undefined}
                speakerOf={vm.speakerOf}
              />
            )}

            {/* 手动滚动后浮出：把高亮拉回当前句（FR-04-3） */}
            {canPlayAudio && !vm.autoScroll && vm.hasSegments && (
              <div className={classes.backToActive}>
                <Button
                  size="xs"
                  variant="filled"
                  leftSection={<IconFocusCentered size={16} stroke={1.75} />}
                  onClick={vm.backToActive}
                >
                  {t('library.playback.backToActive')}
                </Button>
              </div>
            )}
          </div>

          {/* —— 音频缺失：不渲染播放器，只说明原因（歌词仍可读） —— */}
          {!canPlayAudio ? (
            <div className={classes.playerBar}>
              <Alert
                className={classes.warnAlert}
                icon={<IconAlertTriangle size={18} stroke={1.75} />}
                title={t(`library.playback.${noAudio}.title`)}
              >
                <Text className={classes.alertLine}>{t(`library.playback.${noAudio}.message`)}</Text>
              </Alert>
            </div>
          ) : vm.audioError ? (
            <div className={classes.playerBar}>
              <Alert
                className={classes.warnAlert}
                icon={<IconAlertTriangle size={18} stroke={1.75} />}
                title={t('library.playback.audioError.title')}
              >
                <Stack gap={8}>
                  <Text className={classes.alertLine}>{t('library.playback.audioError.message')}</Text>
                  <Group gap="xs">
                    <Button
                      size="xs"
                      variant="default"
                      leftSection={<IconReload size={16} stroke={1.75} />}
                      onClick={vm.retryAudio}
                    >
                      {t('library.playback.audioError.retry')}
                    </Button>
                  </Group>
                </Stack>
              </Alert>
            </div>
          ) : (
            <div className={classes.playerBar}>
              {/* 进度条：拖动即时预览（label 气泡显示目标时间），释放才 seek */}
              <Slider
                className={classes.progress}
                size="sm"
                min={0}
                max={sliderMax}
                value={sliderValue}
                label={(value) => formatTime(value)}
                aria-label={t('library.playback.progressAria')}
                onPointerDown={vm.beginScrub}
                onChange={vm.scrubTo}
                onChangeEnd={vm.endScrub}
              />

              <div className={classes.controls}>
                <Group gap={4} wrap="nowrap">
                  <Tooltip label={t('library.playback.prevSegment')} withArrow>
                    <ActionIcon
                      variant="default"
                      aria-label={t('library.playback.prevSegment')}
                      onClick={vm.prevSegment}
                    >
                      <IconPlayerTrackPrev size={18} stroke={1.75} />
                    </ActionIcon>
                  </Tooltip>

                  <Tooltip label={t('library.playback.back5')} withArrow>
                    <ActionIcon
                      variant="default"
                      aria-label={t('library.playback.back5')}
                      onClick={() => vm.seekBy(-SEEK_STEP_MS)}
                    >
                      <IconRewindBackward5 size={18} stroke={1.75} />
                    </ActionIcon>
                  </Tooltip>

                  <Tooltip
                    label={vm.playing ? t('library.playback.pause') : t('library.playback.play')}
                    withArrow
                  >
                    <ActionIcon
                      variant="filled"
                      size="lg"
                      aria-label={vm.playing ? t('library.playback.pause') : t('library.playback.play')}
                      onClick={vm.togglePlay}
                    >
                      {vm.playing ? (
                        <IconPlayerPause size={20} stroke={1.75} />
                      ) : (
                        <IconPlayerPlay size={20} stroke={1.75} />
                      )}
                    </ActionIcon>
                  </Tooltip>

                  <Tooltip label={t('library.playback.forward5')} withArrow>
                    <ActionIcon
                      variant="default"
                      aria-label={t('library.playback.forward5')}
                      onClick={() => vm.seekBy(SEEK_STEP_MS)}
                    >
                      <IconRewindForward5 size={18} stroke={1.75} />
                    </ActionIcon>
                  </Tooltip>

                  <Tooltip label={t('library.playback.nextSegment')} withArrow>
                    <ActionIcon
                      variant="default"
                      aria-label={t('library.playback.nextSegment')}
                      onClick={vm.nextSegment}
                    >
                      <IconPlayerTrackNext size={18} stroke={1.75} />
                    </ActionIcon>
                  </Tooltip>
                </Group>

                <Text
                  className={classes.clock}
                  aria-label={t('library.playback.positionAria', {
                    position: formatTime(vm.currentMs),
                    duration: formatTime(vm.durationMs),
                  })}
                >
                  {formatTime(vm.currentMs)} / {formatTime(vm.durationMs)}
                </Text>

                <Group gap={8} wrap="nowrap" className={classes.rightControls}>
                  {/* 倍速：档位走 i18n 模板，避免把 '1.5×' 硬编码在 UI 里 */}
                  <Menu position="top-end" withinPortal>
                    <Menu.Target>
                      <Button
                        variant="default"
                        size="compact-sm"
                        aria-label={t('library.playback.rateAria', { rate: rateLabel(t, vm.rate) })}
                      >
                        {rateLabel(t, vm.rate)}
                      </Button>
                    </Menu.Target>
                    <Menu.Dropdown>
                      {PLAYBACK_RATES.map((value) => (
                        <Menu.Item
                          key={value}
                          onClick={() => vm.setRate(value)}
                          rightSection={
                            value === vm.rate ? <IconCheck size={14} stroke={1.75} /> : undefined
                          }
                        >
                          {rateLabel(t, value)}
                        </Menu.Item>
                      ))}
                    </Menu.Dropdown>
                  </Menu>

                  <Tooltip
                    label={vm.muted ? t('library.playback.unmute') : t('library.playback.mute')}
                    withArrow
                  >
                    <ActionIcon
                      variant="subtle"
                      aria-label={vm.muted ? t('library.playback.unmute') : t('library.playback.mute')}
                      onClick={vm.toggleMute}
                    >
                      {vm.muted || vm.volume === 0 ? (
                        <IconVolumeOff size={18} stroke={1.75} />
                      ) : (
                        <IconVolume size={18} stroke={1.75} />
                      )}
                    </ActionIcon>
                  </Tooltip>

                  <Slider
                    className={classes.volume}
                    size="xs"
                    min={0}
                    max={1}
                    step={VOLUME_STEP}
                    value={vm.muted ? 0 : vm.volume}
                    label={(value) => t('library.playback.volumeValue', { percent: Math.round(value * 100) })}
                    aria-label={t('library.playback.volumeAria')}
                    onChange={vm.setVolume}
                  />
                </Group>
              </div>
            </div>
          )}
        </div>
      )}
    </Drawer>
  );
};

// —— 歌词式列表（窗口化 + memo）：播放位置每帧变化时只有时钟 state 变，列表靠 props 相等跳过重渲染 ——
interface PlaybackLyricsProps {
  // 当前可视窗口内的槽位（已按全局下标排序，只含可视区 ± 缓冲行）。
  slots: PlaybackSlot[];
  highlightIndex: number;
  // 内容总高（未加载页按估算撑开），用于 spacer 占位。
  contentHeight: number;
  windowStart: number;
  windowEnd: number;
  totalSegments: number;
  listRef: React.RefObject<HTMLDivElement | null>;
  onListScroll: () => void;
  onMeasured: (index: number, height: number) => void;
  onJump: ((segment: Segment) => void) | undefined;
  speakerOf: (segment: Segment) => SpeakerInfo;
}

const PlaybackLyrics = memo(
  ({
    slots,
    highlightIndex,
    contentHeight,
    windowStart,
    windowEnd,
    totalSegments,
    listRef,
    onListScroll,
    onMeasured,
    onJump,
    speakerOf,
  }: PlaybackLyricsProps) => {
    const t = useT();

    return (
      <div
        className={classes.list}
        ref={listRef}
        onScroll={onListScroll}
        aria-label={t('library.playback.lyricsAria')}
        // 开发者自查用：首屏只请求了 PLAYBACK_PAGE_SIZE 条，这里能看到总句数 /
        // 当前渲染窗口 / 每页条数（不看样式、不影响布局）。
        data-playback-total={totalSegments}
        data-playback-window={`${windowStart}-${windowEnd}`}
        data-playback-page-size={PLAYBACK_PAGE_SIZE}
      >
        {/* spacer：撑出「全部句数」的滚动长度；视口内的槽位绝对定位在上面。
            未加载页的槽位没有实测高度，用估算值顶上（滚动条长度略有误差，不抖动）。 */}
        <div className={classes.spacer} style={{ height: contentHeight }}>
          {slots.map((slot) => (
            <PlaybackSlotView
              key={slot.key}
              index={slot.index}
              top={slot.top}
              segment={slot.segment}
              markers={slot.markers}
              active={slot.index === highlightIndex}
              onMeasured={onMeasured}
              onJump={onJump}
              speakerOf={speakerOf}
            />
          ))}
        </div>
      </div>
    );
  },
);

PlaybackLyrics.displayName = 'PlaybackLyrics';

// —— 单个槽位：若干个标记行 + 一行歌词（或未加载占位行） ——
// props 全是原始值 / 稳定引用（index / top / segment / markers），因此滚动时只有
// 「新进入窗口的行」会真正渲染，窗口内的老行靠 memo 直接跳过（顶部偏移没变就不重排）。
interface PlaybackSlotViewProps {
  index: number;
  top: number;
  segment: Segment | null;
  markers: readonly Marker[];
  active: boolean;
  onMeasured: (index: number, height: number) => void;
  onJump: ((segment: Segment) => void) | undefined;
  speakerOf: (segment: Segment) => SpeakerInfo;
}

const PlaybackSlotView = memo(
  ({ index, top, segment, markers, active, onMeasured, onJump, speakerOf }: PlaybackSlotViewProps) => {
    const t = useT();
    const ref = useRef<HTMLDivElement | null>(null);

    // 行高实测：正文会折行，高度只能量。每一次渲染后（布局已定）回报一次，
    // 未变化时 hook 内部会直接丢弃，不会引发循环更新。
    useLayoutEffect(() => {
      const element = ref.current;
      if (element !== null) onMeasured(index, element.offsetHeight);
    });

    const speaker = segment !== null ? speakerOf(segment) : null;
    const Row = onJump ? 'button' : 'div';

    return (
      <div ref={ref} className={classes.slot} style={{ top }} data-playback-slot={index}>
        {markers.map((marker) => (
          <MarkerLine key={`mk-${marker.kind}-${marker.atMs}`} marker={marker} />
        ))}

        {segment === null || speaker === null ? (
          // 所在页还没加载：先占位（滚动会触发加载），不显示空行以免误以为「这句没了」。
          <div className={classes.slotPlaceholder} aria-hidden="true">
            <span className={classes.placeholderBar} />
            <span className={classes.placeholderBarShort} />
          </div>
        ) : (
          <Row
            type={onJump ? 'button' : undefined}
            className={classes.lyricRow}
            data-playback-row={index}
            data-active={active ? 'true' : undefined}
            data-unidentified={speaker.unidentified ? 'true' : undefined}
            aria-current={active ? 'true' : undefined}
            title={t('library.playback.segmentRange', {
              start: formatTime(segment.startMs),
              end: formatTime(segment.endMs),
            })}
            onClick={onJump ? () => onJump(segment) : undefined}
          >
            <span className={classes.lyricClock}>{formatTime(segment.startMs)}</span>
            <span className={classes.roleDot} data-token={speaker.colorToken} aria-hidden="true" />
            <span className={classes.lyricBody}>
              <span className={classes.speakerName} data-token={speaker.colorToken}>
                {speakerLabel(t, speaker)}
              </span>
              <span className={classes.lyricText}>{segment.text}</span>
              {!segment.finalized && (
                <span className={classes.unfinalized}>{t('library.playback.unfinalized')}</span>
              )}
            </span>
          </Row>
        )}
      </div>
    );
  },
);

PlaybackSlotView.displayName = 'PlaybackSlotView';

// 时间轴标记行（不参与高亮，只做分隔与说明）。
const MarkerLine = memo(({ marker }: { marker: Marker }) => {
  const t = useT();
  return (
    <div className={classes.markerRow}>
      <span className={classes.markerIcon}>{markerIcon(marker.kind)}</span>
      <span className={classes.markerClock}>{formatTime(marker.atMs)}</span>
      <span className={classes.markerLabel}>{markerLabel(t, marker)}</span>
      {marker.note !== '' && <span className={classes.markerNote}>{marker.note}</span>}
    </div>
  );
});

MarkerLine.displayName = 'MarkerLine';

// 说话人展示名：未识别 / 名字为空时走「未识别」文案。
const speakerLabel = (
  t: (key: string, vars?: Record<string, string | number>) => string,
  speaker: SpeakerInfo,
): string =>
  speaker.unidentified || speaker.name === '' ? t('library.playback.unknownSpeaker') : speaker.name;

// 标记种类 → 文案（未知种类回落 generic）。
const markerLabel = (
  t: (key: string, vars?: Record<string, string | number>) => string,
  marker: Marker,
): string => {
  const key = `library.playback.markers.${marker.kind}`;
  const translated = t(key);
  return translated === key ? t('library.playback.markers.generic') : translated;
};

// 时间轴标记 → 图标（种类来自 contract.Marker，未知种类用旗标兜底）。
const markerIcon = (kind: string): React.ReactNode => {
  switch (kind) {
    case 'degrade':
      return <IconAlertTriangle size={14} stroke={1.75} />;
    case 'source-lost':
      return <IconPlugConnectedX size={14} stroke={1.75} />;
    case 'device-switch':
      return <IconArrowsExchange size={14} stroke={1.75} />;
    case 'paused':
      return <IconClockPause size={14} stroke={1.75} />;
    case 'resumed':
      return <IconClockPlay size={14} stroke={1.75} />;
    case 'perf-throttle':
      return <IconGauge size={14} stroke={1.75} />;
    case 'perf-suspend':
      return <IconClockPause size={14} stroke={1.75} />;
    case 'perf-resume':
      return <IconClockPlay size={14} stroke={1.75} />;
    default:
      return <IconFlag size={14} stroke={1.75} />;
  }
};

// 倍速标签：'1' / '1.25' → '1×' / '1.25×'（走 i18n 模板，不硬编码符号）。
const rateLabel = (t: (key: string, vars?: Record<string, string | number>) => string, rate: number): string =>
  t('library.playback.rateValue', { rate: String(rate) });

// 无片段时的说明：区分「没转写」与「转写中」，并给出下一步该去哪。
const emptyDescription = (
  t: (key: string, vars?: Record<string, string | number>) => string,
  status: string,
  canPlayAudio: boolean,
): string => {
  if (status === 'queued' || status === 'running') return t('library.playback.emptyTranscribing');
  if (!canPlayAudio) return t(status === 'failed' ? 'library.playback.emptyFailedNoAudio' : 'library.playback.emptyNoAudio');
  if (status === 'failed') return t('library.playback.emptyFailed');
  return t('library.playback.emptyDescription');
};

// 会话开始时间：ISO → 本地 'YYYY-MM-DD HH:mm'（与列表页同口径）。
const formatStartedAt = (iso: string, locale: string): string => {
  if (iso === '') return '—';
  const parsed = new Date(iso);
  if (Number.isNaN(parsed.getTime())) return iso;
  return parsed.toLocaleString(locale, {
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
  });
};
