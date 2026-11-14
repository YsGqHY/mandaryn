import { useState } from 'react';
import { Button, ScrollArea, SegmentedControl, Select, Switch, Text, UnstyledButton } from '@mantine/core';
import {
  IconAlertTriangle,
  IconBug,
  IconChevronDown,
  IconChevronRight,
  IconFolderOpen,
  IconRefresh,
  IconTrash,
} from '@tabler/icons-react';
import { useT } from '@/i18n';
import {
  NativeDialogs,
  type AppError,
  type DebugLogEntry,
  type DebugLogLevel,
  type DebugLogOrigin,
  type DebugLogStatus,
} from '@/services';
import { useDebugLog } from './useDebugLog';
import classes from './DebugLogPanel.module.css';
import pv from '../Privacy.module.css';

type T = ReturnType<typeof useT>;

const ALL = '__all__';
const LEVELS: DebugLogLevel[] = ['', 'DEBUG', 'INFO', 'WARN', 'ERROR'];
const ORIGINS: DebugLogOrigin[] = ['', 'backend', 'frontend'];

// 「调试模式」卡片（设置 → 隐私边界）：开关 + 存储说明 + 查看器。
// 文案 namespace：settings.privacy.debug.*
export const DebugLogPanel = () => {
  const vm = useDebugLog();
  const t = useT();
  const status = vm.status;
  const enabled = status?.mode.enabled ?? false;

  const handleClear = async (): Promise<void> => {
    if (vm.clearing) return;
    const ok = await NativeDialogs.confirm({
      title: t('settings.privacy.debug.confirmClear.title'),
      message: t('settings.privacy.debug.confirmClear.message'),
      okLabel: t('settings.privacy.debug.confirmClear.ok'),
      cancelLabel: t('settings.privacy.debug.confirmClear.cancel'),
    });
    if (ok) await vm.clear();
  };

  return (
    <div className={pv.card} data-testid="debug-mode-card">
      <div className={pv.cardHeader}>
        <IconBug size={20} stroke={1.75} className={pv.cardIcon} />
        <Text className={pv.cardTitle}>{t('settings.privacy.debug.title')}</Text>
        <span className={pv.stateBadge} data-off={enabled ? undefined : 'on'}>
          {enabled ? t('settings.privacy.debug.stateOn') : t('settings.privacy.debug.stateOff')}
        </span>
      </div>

      <div className={pv.switchRow}>
        <Switch
          checked={enabled}
          onChange={(event) => void vm.toggle(event.currentTarget.checked)}
          disabled={vm.statusLoading || vm.toggling || vm.envManaged}
          label={t('settings.privacy.debug.switchLabel')}
          aria-label={t('settings.privacy.debug.switchLabel')}
        />
      </div>
      <Text className={pv.cardHint}>{t('settings.privacy.debug.hint')}</Text>

      {vm.envManaged && status && (
        <div className={pv.warnBox} role="status">
          <IconAlertTriangle size={20} stroke={1.75} className={pv.warnIcon} />
          <div className={pv.warnTexts}>
            <Text className={pv.warnTitle}>
              {t('settings.privacy.debug.envManaged.title', { env: status.mode.envVar })}
            </Text>
            <Text className={pv.warnBody}>
              {t('settings.privacy.debug.envManaged.body', {
                state: status.mode.enabled ? t('settings.privacy.debug.on') : t('settings.privacy.debug.off'),
                user: status.mode.userEnabled ? t('settings.privacy.debug.on') : t('settings.privacy.debug.off'),
              })}
            </Text>
          </div>
        </div>
      )}

      {status && <StorageMeta status={status} t={t} onReveal={() => void vm.revealDir()} />}

      <div className={classes.scope}>
        <Text className={classes.scopeLine}>{t('settings.privacy.debug.scope.records')}</Text>
        <Text className={classes.scopeLine} data-tone="danger">
          {t('settings.privacy.debug.scope.never')}
        </Text>
      </div>

      {/* —— 查看器 —— */}
      <div className={classes.viewerHeader}>
        <Text className={classes.viewerTitle}>{t('settings.privacy.debug.viewer.title')}</Text>
        <div className={pv.cardActions}>
          <Button
            variant="default"
            size="xs"
            leftSection={<IconRefresh size={16} stroke={1.75} />}
            loading={vm.pageLoading}
            onClick={() => void vm.refresh()}
          >
            {t('settings.privacy.debug.viewer.refresh')}
          </Button>
          <Button
            variant="default"
            size="xs"
            className={classes.dangerButton}
            leftSection={<IconTrash size={16} stroke={1.75} />}
            loading={vm.clearing}
            disabled={(status?.files.length ?? 0) === 0}
            onClick={() => void handleClear()}
          >
            {t('settings.privacy.debug.viewer.clear')}
          </Button>
        </div>
      </div>

      <div className={classes.filters}>
        <div className={classes.filter}>
          <Text className={classes.filterLabel}>{t('settings.privacy.debug.viewer.level')}</Text>
          <SegmentedControl
            size="xs"
            value={vm.filter.minLevel || ALL}
            onChange={(value) => vm.setFilter({ minLevel: (value === ALL ? '' : value) as DebugLogLevel })}
            data={LEVELS.map((lv) => ({
              value: lv || ALL,
              label: t(`settings.privacy.debug.viewer.levels.${lv || 'all'}`),
            }))}
            aria-label={t('settings.privacy.debug.viewer.level')}
          />
        </div>
        <div className={classes.filter}>
          <Text className={classes.filterLabel}>{t('settings.privacy.debug.viewer.origin')}</Text>
          <SegmentedControl
            size="xs"
            value={vm.filter.origin || ALL}
            onChange={(value) => vm.setFilter({ origin: (value === ALL ? '' : value) as DebugLogOrigin })}
            data={ORIGINS.map((o) => ({
              value: o || ALL,
              label: t(`settings.privacy.debug.viewer.origins.${o || 'all'}`),
            }))}
            aria-label={t('settings.privacy.debug.viewer.origin')}
          />
        </div>
        <div className={classes.filter}>
          <Text className={classes.filterLabel}>{t('settings.privacy.debug.viewer.component')}</Text>
          <Select
            size="xs"
            className={classes.componentSelect}
            value={vm.filter.component || ALL}
            onChange={(value) => vm.setFilter({ component: !value || value === ALL ? '' : value })}
            data={[
              { value: ALL, label: t('settings.privacy.debug.viewer.componentAll') },
              ...(vm.page?.components ?? []).map((c) => ({ value: c, label: c })),
            ]}
            allowDeselect={false}
            aria-label={t('settings.privacy.debug.viewer.component')}
          />
        </div>
      </div>

      <EntryList vm={vm} enabled={enabled} t={t} />

      {vm.error && (
        <div className={pv.errorLine} role="alert">
          <Text className={pv.errorText}>{errorText(vm.error, t)}</Text>
        </div>
      )}
    </div>
  );
};

// —— 存储位置 / 格式 / 保留策略 / 占用 / 开关保存 ——
const StorageMeta = ({ status, t, onReveal }: { status: DebugLogStatus; t: T; onReveal: () => void }) => {
  const total = status.maxFileBytes * status.maxFiles;
  return (
    <dl className={classes.meta}>
      <dt>{t('settings.privacy.debug.meta.location')}</dt>
      <dd className={classes.pathRow}>
        <code className={classes.path} data-testid="debug-log-dir">
          {status.dir}
        </code>
        <Button
          variant="subtle"
          size="compact-xs"
          leftSection={<IconFolderOpen size={14} stroke={1.75} />}
          onClick={onReveal}
        >
          {t('settings.privacy.debug.meta.openDir')}
        </Button>
      </dd>
      <dt>{t('settings.privacy.debug.meta.format')}</dt>
      <dd>{t('settings.privacy.debug.meta.formatValue')}</dd>
      <dt>{t('settings.privacy.debug.meta.retention')}</dt>
      <dd>
        {t('settings.privacy.debug.meta.retentionValue', {
          fileSize: formatBytes(status.maxFileBytes),
          files: status.maxFiles,
          total: formatBytes(total),
          days: status.retentionDays,
        })}
      </dd>
      <dt>{t('settings.privacy.debug.meta.usage')}</dt>
      <dd data-testid="debug-log-usage">
        {status.files.length === 0
          ? t('settings.privacy.debug.meta.usageEmpty')
          : t('settings.privacy.debug.meta.usageValue', {
              size: formatBytes(status.totalBytes),
              files: status.files.length,
            })}
      </dd>
      <dt>{t('settings.privacy.debug.meta.persistence')}</dt>
      <dd>{t('settings.privacy.debug.meta.persistenceValue')}</dd>
    </dl>
  );
};

// —— 记录列表 ——
const EntryList = ({ vm, enabled, t }: { vm: ReturnType<typeof useDebugLog>; enabled: boolean; t: T }) => {
  const [open, setOpen] = useState<Set<string>>(new Set());
  const page = vm.page;
  const entries = page?.entries ?? [];

  if (entries.length === 0) {
    return (
      <Text className={pv.cardHint} data-testid="debug-log-empty">
        {vm.pageLoading
          ? t('settings.privacy.debug.viewer.loading')
          : enabled
            ? t('settings.privacy.debug.viewer.emptyOn')
            : t('settings.privacy.debug.viewer.emptyOff')}
      </Text>
    );
  }

  const toggleRow = (key: string): void => {
    setOpen((prev) => {
      const next = new Set(prev);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      return next;
    });
  };

  return (
    <>
      <ScrollArea.Autosize mah={360} className={classes.list} data-testid="debug-log-list">
        {entries.map((entry, index) => {
          const key = `${entry.time}-${index}`;
          const expanded = open.has(key);
          return (
            <div key={key} className={classes.entry} data-level={entry.level} data-origin={entry.origin}>
              <UnstyledButton className={classes.entryRow} onClick={() => toggleRow(key)} aria-expanded={expanded}>
                {expanded ? (
                  <IconChevronDown size={14} stroke={1.75} className={classes.chevron} />
                ) : (
                  <IconChevronRight size={14} stroke={1.75} className={classes.chevron} />
                )}
                <span className={classes.time}>{shortTime(entry.time)}</span>
                <span className={classes.level} data-level={entry.level}>
                  {entry.level}
                </span>
                <span className={classes.component}>{entry.component || '—'}</span>
                <span className={classes.message}>{displayMessage(entry)}</span>
              </UnstyledButton>
              {expanded && <EntryDetails entry={entry} t={t} />}
            </div>
          );
        })}
      </ScrollArea.Autosize>
      {page && (
        <Text className={pv.cardHint}>
          {t('settings.privacy.debug.viewer.summary', { shown: entries.length, scanned: page.scanned })}
          {page.truncated && ` ${t('settings.privacy.debug.viewer.truncated')}`}
          {page.malformed > 0 && ` ${t('settings.privacy.debug.viewer.malformed', { count: page.malformed })}`}
        </Text>
      )}
    </>
  );
};

const EntryDetails = ({ entry, t }: { entry: DebugLogEntry; t: T }) => {
  const fields = prettyFields(entry.fields);
  return (
    <div className={classes.details}>
      {entry.source && (
        <Text className={classes.detailLine}>
          <span className={classes.detailLabel}>{t('settings.privacy.debug.viewer.source')}</span>
          <code>{entry.source}</code>
        </Text>
      )}
      <Text className={classes.detailLabel}>{t('settings.privacy.debug.viewer.details')}</Text>
      {fields === '' ? (
        <Text className={pv.cardHint}>{t('settings.privacy.debug.viewer.noFields')}</Text>
      ) : (
        <pre className={classes.fields}>{fields}</pre>
      )}
    </div>
  );
};

// 前端记录的 msg 是固定文案（「前端控制台输出」等），真正的内容在 fields.message；
// 固定文案仍可在展开的上下文里看到（kind 字段）。
const displayMessage = (entry: DebugLogEntry): string => {
  if (entry.origin === 'frontend') {
    const parsed = parseFields(entry.fields);
    const message = parsed?.['message'];
    if (typeof message === 'string' && message !== '') return message;
  }
  return entry.msg;
};

const parseFields = (raw: string): Record<string, unknown> | null => {
  try {
    const value: unknown = JSON.parse(raw);
    return typeof value === 'object' && value !== null ? (value as Record<string, unknown>) : null;
  } catch {
    return null;
  }
};

const prettyFields = (raw: string): string => {
  const parsed = parseFields(raw);
  if (parsed === null || Object.keys(parsed).length === 0) return '';
  return JSON.stringify(parsed, null, 2);
};

// slog 的时间是 RFC3339 毫秒（2026-11-14T00:06:20.210+08:00）：只显示「月-日 时:分:秒.毫秒」。
const shortTime = (iso: string): string => {
  const m = /^\d{4}-(\d{2}-\d{2})T(\d{2}:\d{2}:\d{2}(?:\.\d{1,3})?)/.exec(iso);
  return m ? `${m[1]} ${m[2]}` : iso;
};

const formatBytes = (n: number): string => {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  return `${(n / (1024 * 1024)).toFixed(1)} MB`;
};

const errorText = (err: AppError, t: T): string => {
  const key = `settings.privacy.errors.${err.code}`;
  const translated = err.code !== '' ? t(key) : key;
  return translated !== key ? translated : err.message;
};
