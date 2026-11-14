import {
  ActionIcon,
  Button,
  Checkbox,
  EmptyState,
  Group,
  Loader,
  Menu,
  Modal,
  NumberInput,
  Pagination,
  Paper,
  Progress,
  Select,
  Stack,
  Text,
  TextInput,
  Tooltip,
} from '@mantine/core';
import {
  IconCalendar,
  IconClock,
  IconFileExport,
  IconFileText,
  IconFilterOff,
  IconHeadphones,
  IconPencil,
  IconPlayerPlay,
  IconRefresh,
  IconRotateClockwise2,
  IconSearch,
  IconTrash,
  IconUsersGroup,
  IconX,
} from '@tabler/icons-react';
import { useI18n, useT } from '@/i18n';
import { Skeleton } from '@/components/Skeleton';
import {
  NativeDialogs,
  TranscriptionStatuses,
  isTranscribing,
  type AppError,
  type ExportFormat,
  type Session,
  type TranscriptionState,
} from '@/services';
import { useLibraryPage } from './useLibraryPage';
import { SpeakersSidebar } from './speakers';
import { PlaybackDrawer } from './playback';
import classes from './LibraryPage.module.css';

// 导出格式 → 扩展名 + 过滤器文案 key（原生「另存为」用）。
interface ExportSpec {
  format: ExportFormat;
  extension: string;
  filterKey: string;
  labelKey: string;
}

const EXPORT_SPECS: ExportSpec[] = [
  { format: 'txt', extension: 'txt', filterKey: 'library.export.filterTxt', labelKey: 'library.export.formatTxt' },
  { format: 'srt', extension: 'srt', filterKey: 'library.export.filterSrt', labelKey: 'library.export.formatSrt' },
  { format: 'csv', extension: 'csv', filterKey: 'library.export.filterCsv', labelKey: 'library.export.formatCsv' },
  { format: 'xlsx', extension: 'xlsx', filterKey: 'library.export.filterXlsx', labelKey: 'library.export.formatXlsx' },
];

// Library 页面（路由 id: library）：
//   1) 搜索 —— 关键词同时匹配标题与片段正文
//   2) 筛选 —— 时间范围 / 来源应用 / 转写状态走后端；时长区间只筛当前页（后端契约无该项）
//   3) 列表 —— 标题 / 开始时间 / 时长 / 来源 / 转写状态徽标；行内改名、删除、导出、转写
//   4) 转写 —— 发起 / 取消 / 失败重试，进度来自 transcription:state（EV-16）
//
// 原生对话框（确认删除 / 另存为 / 结果提示）全部走 NativeDialogs；文案全部走 t()。
export const LibraryPage = () => {
  const t = useT();
  const { current } = useI18n();
  const vm = useLibraryPage();

  // 错误码 → 文案；命中不到时回落 errors.unknown 并带上后端 message。
  const errorText = (err: AppError): string => {
    const key = `library.errors.${err.code}`;
    const translated = t(key, { message: err.message });
    if (err.code !== '' && translated !== key) return translated;
    return t('library.errors.unknown', { message: err.message });
  };

  // 后端错误码（转写失败时的 errorCode）→ 文案；未知码原样展示。
  const codeText = (code: string): string => {
    if (code === '') return '';
    const key = `library.errors.${code}`;
    const translated = t(key, { message: code });
    return translated === key ? code : translated;
  };

  const formatDateTime = (iso: string): string => {
    if (iso === '') return '—';
    const parsed = new Date(iso);
    if (Number.isNaN(parsed.getTime())) return iso;
    return parsed.toLocaleString(current.code, {
      year: 'numeric',
      month: '2-digit',
      day: '2-digit',
      hour: '2-digit',
      minute: '2-digit',
    });
  };

  const statusLabel = (status: string): string => {
    const key = `library.transcription.${status}`;
    const translated = t(key);
    return translated === key ? status : translated;
  };

  const statusTone = (status: string): string => {
    switch (status) {
      case 'done':
        return 'success';
      case 'running':
      case 'queued':
        return 'accent';
      case 'failed':
        return 'danger';
      default:
        return 'muted';
    }
  };

  // —— 导出：路径先由原生「另存为」拿到，再交给后端落盘 ——
  const handleExport = async (session: Session, spec: ExportSpec): Promise<void> => {
    const baseName = session.title !== '' ? session.title : session.id;
    const path = await NativeDialogs.saveFile({
      title: t('library.export.dialogTitle'),
      filename: `${baseName}.${spec.extension}`,
      filters: [{ displayName: t(spec.filterKey), pattern: `*.${spec.extension}` }],
    });
    if (path === null) return;
    const saved = await vm.exportSession(session.id, spec.format, path);
    if (saved === null) return;
    await NativeDialogs.info(t('library.export.doneTitle'), t('library.export.doneMessage', { path: saved }));
  };

  // —— 删除：弹窗选「是否同时删音频」→ 原生二选一确认 → 执行 ——
  const handleDeleteConfirm = async (): Promise<void> => {
    const session = vm.deleting;
    if (session === null) return;
    const ok = await NativeDialogs.confirm({
      title: t('library.delete.confirmTitle'),
      message: t('library.delete.confirmMessage', { title: session.title }),
      okLabel: t('library.delete.confirmOk'),
      cancelLabel: t('library.delete.confirmCancel'),
    });
    if (!ok) return;

    const result = await vm.confirmDelete();
    if (result === null) return;
    if (result.audioFailed.length > 0) {
      // 记录已删、音频仍在磁盘：如实告知，不让用户以为磁盘干净了。
      await NativeDialogs.warning(
        t('library.delete.audioLeftTitle'),
        t('library.delete.audioLeftMessage', { count: result.audioFailed.length }),
      );
      return;
    }
    await NativeDialogs.info(
      t('library.delete.doneTitle'),
      t('library.delete.doneMessage', {
        count: result.deleted,
        audio: result.audioDeleted,
      }),
    );
  };

  const emptyAfterFilter =
    vm.visibleItems.length === 0 &&
    (vm.filters.keyword !== '' ||
      vm.filters.from !== '' ||
      vm.filters.to !== '' ||
      vm.filters.sourceId !== '' ||
      vm.filters.transcriptionStatus !== '' ||
      vm.filters.minSeconds !== '' ||
      vm.filters.maxSeconds !== '');

  return (
    // data-speakers="open" 时 shell 从纵向堆叠切成「列表 + 右侧说话人侧栏」两栏（见 module.css）。
    <div className={classes.root} data-speakers={vm.speakerSession !== null ? 'open' : undefined}>
      <div className={classes.body}>
        <div>
          <Text className={classes.eyebrow}>{t('library.eyebrow')}</Text>
          <h1 className={classes.title}>{t('library.title')}</h1>
        </div>
        <Text className={classes.subtitle}>{t('library.subtitle')}</Text>

        {/* 错误提示（后端结构化错误 → i18n） */}
        {vm.error && (
          <div className={classes.errorRow}>
            <Text className={classes.error}>{errorText(vm.error)}</Text>
            <ActionIcon
              variant="subtle"
              aria-label={t('library.errors.dismiss')}
              onClick={vm.dismissError}
            >
              <IconX size={16} stroke={1.75} />
            </ActionIcon>
          </div>
        )}

        {/* —— 搜索与筛选 —— */}
        <Paper className={classes.card} withBorder radius="md" p="lg">
          <div className={classes.cardHeader}>
            <div className={classes.cardTitleGroup}>
              <span className={classes.cardIcon}>
                <IconSearch size={18} stroke={1.75} />
              </span>
              <Text className={classes.cardTitle}>{t('library.search.title')}</Text>
            </div>
            <div className={classes.actionsRow}>
              <Button
                variant="subtle"
                leftSection={<IconFilterOff size={18} stroke={1.75} />}
                onClick={vm.resetFilters}
              >
                {t('library.search.reset')}
              </Button>
              <Tooltip label={t('library.search.refresh')} withArrow>
                <ActionIcon
                  variant="default"
                  aria-label={t('library.search.refresh')}
                  onClick={() => void vm.reload()}
                >
                  <IconRefresh size={18} stroke={1.75} />
                </ActionIcon>
              </Tooltip>
            </div>
          </div>

          <div className={classes.filterGrid}>
            <TextInput
              className={classes.filterWide}
              label={t('library.search.keywordLabel')}
              placeholder={t('library.search.keywordPlaceholder')}
              aria-label={t('library.search.keywordAria')}
              leftSection={<IconSearch size={16} stroke={1.75} />}
              value={vm.filters.keyword}
              onChange={(event) => vm.setFilter({ keyword: event.currentTarget.value })}
            />

            <TextInput
              label={t('library.search.fromLabel')}
              aria-label={t('library.search.fromAria')}
              type="date"
              leftSection={<IconCalendar size={16} stroke={1.75} />}
              value={vm.filters.from}
              onChange={(event) => vm.setFilter({ from: event.currentTarget.value })}
            />

            <TextInput
              label={t('library.search.toLabel')}
              aria-label={t('library.search.toAria')}
              type="date"
              leftSection={<IconCalendar size={16} stroke={1.75} />}
              value={vm.filters.to}
              onChange={(event) => vm.setFilter({ to: event.currentTarget.value })}
            />

            <Select
              label={t('library.search.sourceLabel')}
              placeholder={t('library.search.sourcePlaceholder')}
              aria-label={t('library.search.sourceAria')}
              clearable
              searchable
              value={vm.filters.sourceId === '' ? null : vm.filters.sourceId}
              data={vm.sourceOptions}
              onChange={(value) => vm.setFilter({ sourceId: value ?? '' })}
            />

            <Select
              label={t('library.search.statusLabel')}
              placeholder={t('library.search.statusPlaceholder')}
              aria-label={t('library.search.statusAria')}
              clearable
              value={vm.filters.transcriptionStatus === '' ? null : vm.filters.transcriptionStatus}
              data={TranscriptionStatuses.map((status) => ({
                value: status,
                label: statusLabel(status),
              }))}
              onChange={(value) => vm.setFilter({ transcriptionStatus: value ?? '' })}
            />

            <NumberInput
              label={t('library.search.minDurationLabel')}
              aria-label={t('library.search.minDurationAria')}
              min={0}
              allowDecimal={false}
              suffix={` ${t('library.search.secondsSuffix')}`}
              value={vm.filters.minSeconds === '' ? '' : Number(vm.filters.minSeconds)}
              onChange={(value) => vm.setFilter({ minSeconds: value === '' ? '' : String(value) })}
            />

            <NumberInput
              label={t('library.search.maxDurationLabel')}
              aria-label={t('library.search.maxDurationAria')}
              min={0}
              allowDecimal={false}
              suffix={` ${t('library.search.secondsSuffix')}`}
              value={vm.filters.maxSeconds === '' ? '' : Number(vm.filters.maxSeconds)}
              onChange={(value) => vm.setFilter({ maxSeconds: value === '' ? '' : String(value) })}
            />
          </div>

          <Text className={classes.filterHint}>{t('library.search.durationHint')}</Text>
        </Paper>

        {/* —— 会话列表 —— */}
        <Paper className={classes.card} withBorder radius="md" p="lg">
          <div className={classes.cardHeader}>
            <div className={classes.cardTitleGroup}>
              <span className={classes.cardIcon}>
                <IconFileText size={18} stroke={1.75} />
              </span>
              <Text className={classes.cardTitle}>{t('library.list.title')}</Text>
              <span className={classes.chip} data-tone="muted">
                {t('library.list.countLabel', { count: vm.total })}
              </span>
              {vm.durationFiltered && (
                <span className={classes.chip} data-tone="warning">
                  {t('library.list.durationFiltered', { shown: vm.visibleItems.length })}
                </span>
              )}
            </div>
            <Select
              size="xs"
              aria-label={t('library.list.pageSizeAria')}
              value={String(vm.pageSize)}
              data={['10', '20', '50'].map((size) => ({
                value: size,
                label: t('library.list.pageSizeOption', { count: Number(size) }),
              }))}
              onChange={(value) => {
                if (value !== null) vm.setPageSize(Number(value));
              }}
            />
          </div>

          {vm.loading ? (
            // 加载骨架：与真实行同高，避免列表撑开时跳动（不出文字，不消费 i18n）
            <div className={classes.rowList} aria-busy="true">
              {[0, 1, 2].map((i) => (
                <Skeleton key={i} variant="rect" height={72} style={{ borderRadius: 6 }} />
              ))}
            </div>
          ) : vm.visibleItems.length === 0 ? (
            <div className={classes.emptyBlock}>
              <EmptyState
                icon={<IconFileText size={32} stroke={1.75} />}
                title={emptyAfterFilter ? t('library.list.emptyFilteredTitle') : t('library.list.emptyTitle')}
                description={
                  emptyAfterFilter
                    ? t('library.list.emptyFilteredDescription')
                    : t('library.list.emptyDescription')
                }
              />
            </div>
          ) : (
            <div className={classes.rowList}>
              {vm.visibleItems.map((session) => (
                <SessionRow
                  key={session.id}
                  session={session}
                  active={vm.speakerSession?.id === session.id}
                  playbackActive={vm.playbackSession?.id === session.id}
                  transcription={vm.activeTranscription(session)}
                  status={vm.statusOf(session)}
                  canTranscribe={vm.canTranscribe(session)}
                  busy={vm.busyId === session.id}
                  formatDateTime={formatDateTime}
                  statusLabel={statusLabel}
                  statusTone={statusTone}
                  codeText={codeText}
                  onRename={() => vm.beginRename(session)}
                  onDelete={() => vm.beginDelete(session)}
                  onExport={(spec) => void handleExport(session, spec)}
                  onSpeakers={() => vm.openSpeakers(session)}
                  onPlayback={() => vm.openPlayback(session)}
                  onStartTranscription={() => void vm.startTranscription(session.id)}
                  onCancelTranscription={() => void vm.cancelTranscription(session.id)}
                />
              ))}
            </div>
          )}

          {vm.pageCount > 1 && (
            <div className={classes.paginationRow}>
              <Pagination total={vm.pageCount} value={vm.page} onChange={vm.setPage} size="sm" />
            </div>
          )}
        </Paper>
      </div>

      {/* —— 会话详情的说话人侧栏（FR-06）：选中会话后才出现 —— */}
      {vm.speakerSession !== null && (
        <SpeakersSidebar
          sessionId={vm.speakerSession.id}
          sessionTitle={vm.speakerSession.title !== '' ? vm.speakerSession.title : vm.speakerSession.id}
          onClose={vm.closeSpeakers}
        />
      )}

      {/* —— 会话回放抽屉（FR-04）：与说话人侧栏互斥，同时只开一个 —— */}
      {vm.playbackSession !== null && (
        <PlaybackDrawer session={vm.playbackSession} onClose={vm.closePlayback} />
      )}

      {/* —— 改名弹窗 —— */}
      <Modal
        opened={vm.renaming !== null}
        onClose={vm.cancelRename}
        title={t('library.rename.title')}
        centered
      >
        <Stack gap="md">
          <TextInput
            label={t('library.rename.label')}
            placeholder={t('library.rename.placeholder')}
            aria-label={t('library.rename.label')}
            value={vm.renameDraft}
            data-autofocus
            onChange={(event) => vm.setRenameDraft(event.currentTarget.value)}
          />
          <Group justify="flex-end" gap="xs">
            <Button variant="default" onClick={vm.cancelRename}>
              {t('library.rename.cancel')}
            </Button>
            <Button disabled={vm.busyId !== null} onClick={() => void vm.submitRename()}>
              {t('library.rename.submit')}
            </Button>
          </Group>
        </Stack>
      </Modal>

      {/* —— 删除弹窗（「同时删除音频」选项 + 原生确认） —— */}
      <Modal
        opened={vm.deleting !== null}
        onClose={vm.cancelDelete}
        title={t('library.delete.title')}
        centered
      >
        <Stack gap="md">
          <Text className={classes.modalBody}>
            {t('library.delete.message', { title: vm.deleting?.title ?? '' })}
          </Text>
          <Checkbox
            checked={vm.deleteAudio}
            onChange={(event) => vm.setDeleteAudio(event.currentTarget.checked)}
            label={t('library.delete.alsoAudio')}
            description={t('library.delete.alsoAudioHint')}
          />
          <Group justify="flex-end" gap="xs">
            <Button variant="default" onClick={vm.cancelDelete}>
              {t('library.delete.cancel')}
            </Button>
            <Button
              variant="filled"
              leftSection={<IconTrash size={18} stroke={1.75} />}
              disabled={vm.busyId !== null}
              onClick={() => void handleDeleteConfirm()}
            >
              {t('library.delete.submit')}
            </Button>
          </Group>
        </Stack>
      </Modal>
    </div>
  );
};

// —— 会话行（仅本页使用，不进 index.ts） ——
interface SessionRowProps {
  session: Session;
  // 该会话正开着说话人侧栏（行上做高亮）
  active: boolean;
  // 该会话正开着回放抽屉（行上做高亮，与侧栏独立）
  playbackActive: boolean;
  transcription: TranscriptionState | null;
  status: string;
  canTranscribe: boolean;
  busy: boolean;
  formatDateTime: (iso: string) => string;
  statusLabel: (status: string) => string;
  statusTone: (status: string) => string;
  codeText: (code: string) => string;
  onRename: () => void;
  onDelete: () => void;
  onExport: (spec: ExportSpec) => void;
  onSpeakers: () => void;
  onPlayback: () => void;
  onStartTranscription: () => void;
  onCancelTranscription: () => void;
}

const SessionRow = ({
  session,
  active,
  playbackActive,
  transcription,
  status,
  canTranscribe,
  busy,
  formatDateTime,
  statusLabel,
  statusTone,
  codeText,
  onRename,
  onDelete,
  onExport,
  onSpeakers,
  onPlayback,
  onStartTranscription,
  onCancelTranscription,
}: SessionRowProps) => {
  const t = useT();
  const running = isTranscribing(status);
  // 进度优先用事件里的 progressMs；总时长用会话的帧计数时长（后端给）。
  const progressMs = transcription?.progressMs ?? session.transcribeProgressMs;
  const totalMs = transcription?.audioMs ?? session.durationMs;
  const percent = totalMs > 0 ? Math.min(100, Math.round((progressMs / totalMs) * 100)) : 0;
  const failedCode = transcription?.errorCode ?? '';

  return (
    <div
      className={classes.sessionRow}
      data-active={active ? 'true' : undefined}
      data-playback={playbackActive ? 'true' : undefined}
    >
      <div className={classes.sessionMain}>
        <div className={classes.sessionTitleRow}>
          <span className={classes.sessionTitle}>{session.title !== '' ? session.title : session.id}</span>
          <span className={classes.chip} data-tone={statusTone(status)}>
            {statusLabel(status)}
          </span>
          {session.degraded && (
            <span className={classes.chip} data-tone="warning">
              {t('library.list.degraded')}
            </span>
          )}
          {session.modelSource === 'online' && (
            <span className={classes.chip} data-tone="accent">
              {t('library.list.online')}
            </span>
          )}
        </div>

        <div className={classes.sessionMeta}>
          <span className={classes.metaItem}>
            <IconCalendar size={14} stroke={1.75} />
            {formatDateTime(session.startedAt)}
          </span>
          <span className={classes.metaItem}>
            <IconClock size={14} stroke={1.75} />
            {formatDuration(session.durationMs)}
          </span>
          <span className={classes.metaItem}>
            {t('library.list.sourceLabel')}
            {': '}
            {session.sourceName !== '' ? session.sourceName : session.sourceId}
          </span>
          <span className={classes.metaItem}>
            {t('library.list.segmentLabel', { count: session.segmentCount })}
          </span>
        </div>

        {running && (
          <div className={classes.progressRow}>
            <Progress value={percent} size="sm" className={classes.progress} aria-label={t('library.list.progressAria')} />
            <span className={classes.progressText}>{t('library.list.progressText', { percent })}</span>
          </div>
        )}

        {status === 'failed' && failedCode !== '' && (
          <Text className={classes.failedHint}>{codeText(failedCode)}</Text>
        )}
      </div>

      <div className={classes.sessionActions}>
        {running ? (
          <Tooltip label={t('library.list.cancelTranscription')} withArrow>
            <ActionIcon
              variant="default"
              aria-label={t('library.list.cancelTranscription')}
              disabled={busy}
              onClick={onCancelTranscription}
            >
              <IconX size={16} stroke={1.75} />
            </ActionIcon>
          </Tooltip>
        ) : (
          <Tooltip
            label={status === 'failed' ? t('library.list.retryTranscription') : t('library.list.startTranscription')}
            withArrow
          >
            <ActionIcon
              variant="default"
              aria-label={
                status === 'failed' ? t('library.list.retryTranscription') : t('library.list.startTranscription')
              }
              disabled={busy || !canTranscribe}
              onClick={onStartTranscription}
            >
              {status === 'failed' ? (
                <IconRotateClockwise2 size={16} stroke={1.75} />
              ) : (
                <IconPlayerPlay size={16} stroke={1.75} />
              )}
            </ActionIcon>
          </Tooltip>
        )}

        <Tooltip label={t('library.list.playback')} withArrow>
          <ActionIcon
            variant="default"
            aria-label={t('library.list.playback')}
            disabled={busy}
            onClick={onPlayback}
          >
            <IconHeadphones size={16} stroke={1.75} />
          </ActionIcon>
        </Tooltip>

        <Menu position="bottom-end" withinPortal>
          <Menu.Target>
            <Tooltip label={t('library.list.export')} withArrow>
              <ActionIcon variant="default" aria-label={t('library.list.export')} disabled={busy}>
                <IconFileExport size={16} stroke={1.75} />
              </ActionIcon>
            </Tooltip>
          </Menu.Target>
          <Menu.Dropdown>
            {EXPORT_SPECS.map((spec) => (
              <Menu.Item key={spec.format} onClick={() => onExport(spec)}>
                {t(spec.labelKey)}
              </Menu.Item>
            ))}
          </Menu.Dropdown>
        </Menu>

        <Tooltip label={t('library.list.rename')} withArrow>
          <ActionIcon
            variant="default"
            aria-label={t('library.list.rename')}
            disabled={busy}
            onClick={onRename}
          >
            <IconPencil size={16} stroke={1.75} />
          </ActionIcon>
        </Tooltip>

        <Tooltip label={t('library.list.speakers')} withArrow>
          <ActionIcon
            variant="default"
            aria-label={t('library.list.speakers')}
            disabled={busy}
            onClick={onSpeakers}
          >
            <IconUsersGroup size={16} stroke={1.75} />
          </ActionIcon>
        </Tooltip>

        <Tooltip label={t('library.list.delete')} withArrow>
          <ActionIcon
            variant="default"
            aria-label={t('library.list.delete')}
            disabled={busy}
            onClick={onDelete}
            className={classes.deleteAction}
          >
            <IconTrash size={16} stroke={1.75} />
          </ActionIcon>
        </Tooltip>

        {busy && <Loader size={14} />}
      </div>
    </div>
  );
};

// 毫秒 → "mm:ss" / "h:mm:ss"（入参是后端帧计数换算值，前端不自算）。
const formatDuration = (ms: number): string => {
  const totalSeconds = Math.max(0, Math.round(ms / 1000));
  const hours = Math.floor(totalSeconds / 3600);
  const minutes = Math.floor((totalSeconds % 3600) / 60);
  const seconds = totalSeconds % 60;
  const pad = (v: number): string => (v < 10 ? `0${v}` : String(v));
  return hours > 0 ? `${hours}:${pad(minutes)}:${pad(seconds)}` : `${minutes}:${pad(seconds)}`;
};
