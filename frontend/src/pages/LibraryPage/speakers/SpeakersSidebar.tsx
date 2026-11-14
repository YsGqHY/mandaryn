import {
  ActionIcon,
  Alert,
  Button,
  Checkbox,
  Group,
  Loader,
  Modal,
  Paper,
  ScrollArea,
  Select,
  Stack,
  Text,
  TextInput,
  Tooltip,
} from '@mantine/core';
import {
  IconAlertTriangle,
  IconArrowsJoin,
  IconArrowsSplit,
  IconCheck,
  IconPencil,
  IconRefresh,
  IconUserCheck,
  IconUserQuestion,
  IconUsersGroup,
  IconWaveSine,
  IconX,
} from '@tabler/icons-react';
import { useT } from '@/i18n';
import { NativeDialogs, isKnownRoleColorToken, type AppError } from '@/services';
import { useSpeakersSidebar, type SpeakerRow } from './useSpeakersSidebar';
import classes from './SpeakersSidebar.module.css';

export interface SpeakersSidebarProps {
  sessionId: string;
  // 只在标题里展示，方便确认「在给哪个会话分角色」。
  sessionTitle: string;
  onClose: () => void;
}

// 会话详情侧栏：说话人 → 角色映射（FR-06 界面侧）。
//
//   1) 列表 —— 已识别说话人（角色名 / 颜色 / 句数 / 语音时长）+ 单独呈现的「未识别」
//   2) 改名 —— 内联编辑 → RenameRole；成功后靠 roles:changed + 重拉刷新（不做本地替换）
//   3) 合并 —— 源 → 目标 → 原生二次确认（文案说明不可自动还原）→ MergeSpeakers
//   4) 拆分 —— 勾选该说话人在本会话的片段 → SplitSpeaker
//   5) 指派 —— 把某个声纹身份改派到已有角色 → AssignSpeaker
//   6) 发起分离 —— AnalyzeSession；HasDiarization 为 false 时置灰并说明原因
//
// 所有可见文案走 t()；确认框走 NativeDialogs；颜色只用 CSS 变量（见 module.css）。
export const SpeakersSidebar = ({ sessionId, sessionTitle, onClose }: SpeakersSidebarProps) => {
  const t = useT();
  const vm = useSpeakersSidebar(sessionId);

  // 错误码 → 文案；命中不到时回落 library.errors.unknown 并带上后端 message。
  const errorText = (err: AppError): string => {
    const key = `library.errors.${err.code}`;
    const translated = t(key, { message: err.message });
    if (err.code !== '' && translated !== key) return translated;
    return t('library.errors.unknown', { message: err.message });
  };

  const roleName = (row: SpeakerRow): string =>
    row.name !== '' ? row.name : t('library.speakers.rows.unnamed');

  const speakerOptions = vm.rows.map((row) => ({ value: row.speakerId, label: roleName(row) }));

  // 合并：先原生二次确认（不可自动还原），再提交。
  const handleMergeConfirm = async (): Promise<void> => {
    const from = vm.rows.find((row) => row.speakerId === vm.mergeFromId);
    const to = vm.rows.find((row) => row.speakerId === vm.mergeToId);
    if (from === undefined || to === undefined) return;
    const ok = await NativeDialogs.confirm({
      title: t('library.speakers.merge.confirmTitle'),
      message: t('library.speakers.merge.confirmMessage', {
        from: roleName(from),
        to: roleName(to),
      }),
      okLabel: t('library.speakers.merge.confirmOk'),
      cancelLabel: t('library.speakers.merge.confirmCancel'),
    });
    if (!ok) return;
    const done = await vm.submitMerge();
    if (!done) return;
    await NativeDialogs.info(t('library.speakers.merge.doneTitle'), t('library.speakers.merge.doneMessage'));
  };

  const analyzeDisabled = !vm.capable || vm.analyzing || vm.loading;

  return (
    <aside className={classes.root} aria-label={t('library.speakers.title')}>
      <div className={classes.header}>
        <div className={classes.headerTitleGroup}>
          <span className={classes.headerIcon}>
            <IconUsersGroup size={20} stroke={1.75} />
          </span>
          <div className={classes.headerText}>
            <Text className={classes.title}>{t('library.speakers.title')}</Text>
            <Text className={classes.sessionTitle} title={sessionTitle}>
              {sessionTitle}
            </Text>
          </div>
        </div>
        <Group gap={4} wrap="nowrap">
          <Tooltip label={t('library.speakers.refresh')} withArrow>
            <ActionIcon
              variant="default"
              aria-label={t('library.speakers.refresh')}
              disabled={vm.loading}
              onClick={() => void vm.reload()}
            >
              <IconRefresh size={16} stroke={1.75} />
            </ActionIcon>
          </Tooltip>
          <Tooltip label={t('library.speakers.close')} withArrow>
            <ActionIcon variant="default" aria-label={t('library.speakers.close')} onClick={onClose}>
              <IconX size={16} stroke={1.75} />
            </ActionIcon>
          </Tooltip>
        </Group>
      </div>

      <div className={classes.body}>
        <Text className={classes.subtitle}>{t('library.speakers.subtitle')}</Text>

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

        {/* —— 发起分离 —— */}
        <Paper className={classes.card} withBorder radius="md" p="md">
          <Group justify="space-between" gap="xs" wrap="nowrap">
            <div className={classes.cardTitleGroup}>
              <span className={classes.cardIcon}>
                <IconWaveSine size={18} stroke={1.75} />
              </span>
              <Text className={classes.cardTitle}>{t('library.speakers.analyze.title')}</Text>
            </div>
            <Button
              size="xs"
              leftSection={<IconWaveSine size={16} stroke={1.75} />}
              disabled={analyzeDisabled}
              onClick={() => void vm.analyze()}
              loading={vm.analyzing}
            >
              {vm.analyzing
                ? t('library.speakers.analyze.running')
                : vm.rows.length > 0
                  ? t('library.speakers.analyze.rerun')
                  : t('library.speakers.analyze.run')}
            </Button>
          </Group>

          <Text className={classes.cardHint}>{t('library.speakers.analyze.hint')}</Text>

          {/* 能力不可用：如实说明原因并保持按钮置灰（不假装能跑）。 */}
          {vm.capabilityChecked && !vm.capable && (
            <Alert
              className={classes.warnAlert}
              radius="md"
              icon={<IconAlertTriangle size={18} stroke={1.75} className={classes.warnIcon} />}
              title={t('library.speakers.analyze.noModelTitle')}
            >
              <Text className={classes.alertLine}>{t('library.speakers.analyze.noModel')}</Text>
            </Alert>
          )}

          {vm.analyzeResult && (
            <div className={classes.resultBlock}>
              <Group justify="space-between" gap="xs" wrap="nowrap">
                <Text className={classes.resultTitle}>{t('library.speakers.analyze.resultTitle')}</Text>
                <ActionIcon
                  variant="subtle"
                  aria-label={t('library.speakers.analyze.resultDismiss')}
                  onClick={vm.dismissAnalyzeResult}
                >
                  <IconX size={14} stroke={1.75} />
                </ActionIcon>
              </Group>
              <Text className={classes.resultLine}>
                {t('library.speakers.analyze.resultSummary', {
                  speakers: vm.analyzeResult.speakerCount,
                  assigned: vm.analyzeResult.assignedSegments,
                  unidentified: vm.analyzeResult.unidentifiedSegments,
                })}
              </Text>
              <Text className={classes.resultLine}>
                {t('library.speakers.analyze.resultDetail', {
                  segments: vm.analyzeResult.segmentCount,
                  tooShort: vm.analyzeResult.tooShortSegments,
                  failed: vm.analyzeResult.failedSegments,
                })}
              </Text>
              {vm.analyzeResult.modelId !== '' && (
                <Text className={classes.resultLine}>
                  {t('library.speakers.analyze.modelLabel', { model: vm.analyzeResult.modelId })}
                </Text>
              )}
            </div>
          )}
        </Paper>

        {/* —— 已识别说话人 —— */}
        <div className={classes.cardHeaderRow}>
          <Text className={classes.sectionTitle}>{t('library.speakers.identified.title')}</Text>
          <Group gap={4} wrap="nowrap">
            <Tooltip
              label={
                vm.rows.length < 2
                  ? t('library.speakers.merge.needTwo')
                  : t('library.speakers.rows.merge')
              }
              withArrow
            >
              <Button
                size="compact-xs"
                variant="subtle"
                leftSection={<IconArrowsJoin size={16} stroke={1.75} />}
                disabled={vm.rows.length < 2 || vm.busy}
                onClick={() => vm.beginMerge()}
              >
                {t('library.speakers.rows.merge')}
              </Button>
            </Tooltip>
          </Group>
        </div>

        {vm.loading ? (
          <Group gap="xs" className={classes.loadingRow}>
            <Loader size={14} />
            <Text className={classes.cardHint}>{t('library.speakers.loading')}</Text>
          </Group>
        ) : vm.rows.length === 0 ? (
          <Text className={classes.emptyHint}>{t('library.speakers.identified.empty')}</Text>
        ) : (
          <div className={classes.rowList}>
            {vm.rows.map((row) => {
              const editing = vm.editingRoleId === row.roleId;
              return (
                <div key={`${row.roleId}:${row.speakerId}`} className={classes.speakerRow}>
                  <div className={classes.speakerMain}>
                    <div className={classes.speakerTitleRow}>
                      <span
                        className={classes.roleDot}
                        data-token={isKnownRoleColorToken(row.colorToken) ? row.colorToken : 'neutral'}
                        aria-hidden="true"
                      />
                      {editing ? (
                        <div className={classes.renameRow}>
                          <TextInput
                            size="xs"
                            className={classes.renameInput}
                            aria-label={t('library.speakers.rows.nameLabel')}
                            placeholder={t('library.speakers.rows.namePlaceholder')}
                            value={vm.nameDraft}
                            data-autofocus
                            onChange={(event) => vm.setNameDraft(event.currentTarget.value)}
                            onKeyDown={(event) => {
                              if (event.key === 'Enter') void vm.submitRename();
                              if (event.key === 'Escape') vm.cancelRename();
                            }}
                          />
                          <Tooltip label={t('library.speakers.rows.save')} withArrow>
                            <ActionIcon
                              variant="default"
                              aria-label={t('library.speakers.rows.save')}
                              disabled={vm.busy}
                              onClick={() => void vm.submitRename()}
                            >
                              <IconCheck size={16} stroke={1.75} />
                            </ActionIcon>
                          </Tooltip>
                          <Tooltip label={t('library.speakers.rows.cancel')} withArrow>
                            <ActionIcon
                              variant="default"
                              aria-label={t('library.speakers.rows.cancel')}
                              onClick={vm.cancelRename}
                            >
                              <IconX size={16} stroke={1.75} />
                            </ActionIcon>
                          </Tooltip>
                        </div>
                      ) : (
                        <span className={classes.speakerName}>{roleName(row)}</span>
                      )}
                    </div>
                    <div className={classes.speakerMeta}>
                      <span>{t('library.speakers.rows.count', { count: row.utteranceCount })}</span>
                      <span>
                        {t('library.speakers.rows.speech', { duration: formatDuration(row.speechMs) })}
                      </span>
                    </div>
                  </div>

                  {!editing && (
                    <div className={classes.speakerActions}>
                      <Tooltip label={t('library.speakers.rows.rename')} withArrow>
                        <ActionIcon
                          variant="default"
                          aria-label={t('library.speakers.rows.rename')}
                          disabled={vm.busy}
                          onClick={() => vm.beginRename(row)}
                        >
                          <IconPencil size={16} stroke={1.75} />
                        </ActionIcon>
                      </Tooltip>
                      <Tooltip label={t('library.speakers.rows.assign')} withArrow>
                        <ActionIcon
                          variant="default"
                          aria-label={t('library.speakers.rows.assign')}
                          disabled={vm.busy || vm.assignableRoles.length === 0}
                          onClick={() => vm.beginAssign(row)}
                        >
                          <IconUserCheck size={16} stroke={1.75} />
                        </ActionIcon>
                      </Tooltip>
                      <Tooltip label={t('library.speakers.rows.split')} withArrow>
                        <ActionIcon
                          variant="default"
                          aria-label={t('library.speakers.rows.split')}
                          disabled={vm.busy || row.utteranceCount === 0}
                          onClick={() => vm.beginSplit(row)}
                        >
                          <IconArrowsSplit size={16} stroke={1.75} />
                        </ActionIcon>
                      </Tooltip>
                      <Tooltip label={t('library.speakers.rows.mergeInto')} withArrow>
                        <ActionIcon
                          variant="default"
                          aria-label={t('library.speakers.rows.mergeInto')}
                          disabled={vm.busy || vm.rows.length < 2}
                          onClick={() => vm.beginMerge(row.speakerId)}
                        >
                          <IconArrowsJoin size={16} stroke={1.75} />
                        </ActionIcon>
                      </Tooltip>
                    </div>
                  )}
                </div>
              );
            })}
          </div>
        )}

        {/* —— 未识别（不是普通角色，单独一块） —— */}
        {vm.unidentified !== null && (
          <div className={classes.unidentifiedBlock}>
            <div className={classes.unidentifiedTitleRow}>
              <span className={classes.unidentifiedIcon}>
                <IconUserQuestion size={18} stroke={1.75} />
              </span>
              <Text className={classes.sectionTitle}>
                {vm.unidentified.name !== ''
                  ? vm.unidentified.name
                  : t('library.speakers.unidentified.title')}
              </Text>
              <span className={classes.mutedChip}>
                {t('library.speakers.rows.count', { count: vm.unidentified.utteranceCount })}
              </span>
            </div>
            <Text className={classes.cardHint}>{t('library.speakers.unidentified.description')}</Text>
          </div>
        )}
      </div>

      {/* —— 合并弹窗 —— */}
      <Modal
        opened={vm.mergeOpen}
        onClose={vm.cancelMerge}
        title={t('library.speakers.merge.title')}
        centered
      >
        <Stack gap="md">
          <Text className={classes.modalBody}>{t('library.speakers.merge.description')}</Text>
          <Text className={classes.modalWarn}>{t('library.speakers.merge.irreversible')}</Text>
          <Select
            label={t('library.speakers.merge.fromLabel')}
            placeholder={t('library.speakers.merge.placeholder')}
            aria-label={t('library.speakers.merge.fromLabel')}
            value={vm.mergeFromId === '' ? null : vm.mergeFromId}
            data={speakerOptions}
            onChange={(value) => vm.setMergeFromId(value ?? '')}
          />
          <Select
            label={t('library.speakers.merge.toLabel')}
            placeholder={t('library.speakers.merge.placeholder')}
            aria-label={t('library.speakers.merge.toLabel')}
            value={vm.mergeToId === '' ? null : vm.mergeToId}
            data={speakerOptions.filter((option) => option.value !== vm.mergeFromId)}
            onChange={(value) => vm.setMergeToId(value ?? '')}
          />
          <Group justify="flex-end" gap="xs">
            <Button variant="default" onClick={vm.cancelMerge}>
              {t('library.speakers.merge.cancel')}
            </Button>
            <Button
              leftSection={<IconArrowsJoin size={18} stroke={1.75} />}
              disabled={
                vm.busy || vm.mergeFromId === '' || vm.mergeToId === '' || vm.mergeFromId === vm.mergeToId
              }
              onClick={() => void handleMergeConfirm()}
            >
              {t('library.speakers.merge.submit')}
            </Button>
          </Group>
        </Stack>
      </Modal>

      {/* —— 拆分弹窗 —— */}
      <Modal
        opened={vm.splitRow !== null}
        onClose={vm.cancelSplit}
        title={t('library.speakers.split.title', {
          name: vm.splitRow !== null ? roleName(vm.splitRow) : '',
        })}
        size="lg"
        centered
      >
        <Stack gap="md">
          <Text className={classes.modalBody}>{t('library.speakers.split.description')}</Text>
          <Text className={classes.modalHint}>{t('library.speakers.split.note')}</Text>

          {vm.splitLoading ? (
            <Group gap="xs">
              <Loader size={14} />
              <Text className={classes.modalHint}>{t('library.speakers.split.loading')}</Text>
            </Group>
          ) : vm.splitSegments.length === 0 ? (
            <Text className={classes.modalHint}>{t('library.speakers.split.empty')}</Text>
          ) : (
            <ScrollArea.Autosize mah={260} type="auto">
              <div className={classes.segmentList}>
                {vm.splitSegments.map((segment) => (
                  <Checkbox
                    key={segment.id}
                    checked={vm.splitSelected.includes(segment.id)}
                    onChange={() => vm.toggleSplitSegment(segment.id)}
                    aria-label={t('library.speakers.split.segmentAria', { seq: segment.seq + 1 })}
                    label={
                      <span className={classes.segmentLabel}>
                        <span className={classes.segmentClock}>
                          {formatDuration(segment.startMs)}
                        </span>
                        <span className={classes.segmentText}>
                          {segment.text !== '' ? segment.text : t('library.speakers.split.emptyText')}
                        </span>
                      </span>
                    }
                  />
                ))}
              </div>
            </ScrollArea.Autosize>
          )}

          <Group justify="space-between" gap="xs">
            <Text className={classes.modalHint}>
              {t('library.speakers.split.selected', { count: vm.splitSelected.length })}
            </Text>
            <Group gap="xs">
              <Button variant="default" onClick={vm.cancelSplit}>
                {t('library.speakers.split.cancel')}
              </Button>
              <Button
                leftSection={<IconArrowsSplit size={18} stroke={1.75} />}
                disabled={vm.busy || vm.splitSelected.length === 0}
                onClick={() => void vm.submitSplit()}
              >
                {t('library.speakers.split.submit')}
              </Button>
            </Group>
          </Group>
        </Stack>
      </Modal>

      {/* —— 指派弹窗 —— */}
      <Modal
        opened={vm.assignRow !== null}
        onClose={vm.cancelAssign}
        title={t('library.speakers.assign.title', {
          name: vm.assignRow !== null ? roleName(vm.assignRow) : '',
        })}
        centered
      >
        <Stack gap="md">
          <Text className={classes.modalBody}>{t('library.speakers.assign.description')}</Text>
          {vm.assignableRoles.length === 0 ? (
            <Text className={classes.modalHint}>{t('library.speakers.assign.empty')}</Text>
          ) : (
            <Select
              label={t('library.speakers.assign.roleLabel')}
              placeholder={t('library.speakers.assign.placeholder')}
              aria-label={t('library.speakers.assign.roleLabel')}
              value={vm.assignRoleId === '' ? null : vm.assignRoleId}
              data={vm.assignableRoles.map((role) => ({
                value: role.id,
                label: role.name !== '' ? role.name : role.id,
              }))}
              onChange={(value) => vm.setAssignRoleId(value ?? '')}
            />
          )}
          <Group justify="flex-end" gap="xs">
            <Button variant="default" onClick={vm.cancelAssign}>
              {t('library.speakers.assign.cancel')}
            </Button>
            <Button
              leftSection={<IconUserCheck size={18} stroke={1.75} />}
              disabled={vm.busy || vm.assignRoleId === ''}
              onClick={() => void vm.submitAssign()}
            >
              {t('library.speakers.assign.submit')}
            </Button>
          </Group>
        </Stack>
      </Modal>
    </aside>
  );
};

// 毫秒 → "mm:ss" / "h:mm:ss"（与 LibraryPage 行内时长同款展示）。
// 入参是后端按帧计数换算的时间基（startMs / speechMs），不用 wall clock。
const formatDuration = (ms: number): string => {
  const totalSeconds = Math.max(0, Math.round(ms / 1000));
  const hours = Math.floor(totalSeconds / 3600);
  const minutes = Math.floor((totalSeconds % 3600) / 60);
  const seconds = totalSeconds % 60;
  const pad = (v: number): string => (v < 10 ? `0${v}` : String(v));
  return hours > 0 ? `${hours}:${pad(minutes)}:${pad(seconds)}` : `${minutes}:${pad(seconds)}`;
};
