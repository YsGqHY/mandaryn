import { Button, Pagination, Switch, Table, Text } from '@mantine/core';
import {
  IconAlertTriangle,
  IconCircleCheck,
  IconDeviceFloppy,
  IconRefresh,
  IconScan,
  IconShieldLock,
  IconTrash,
  IconWifiOff,
} from '@tabler/icons-react';
import { useI18n, useT } from '@/i18n';
import { NativeDialogs, SAVE_DIAGNOSTICS_SUPPORTED, type OutboundRecord } from '@/services';
import { usePrivacy, type PrivacyNotice } from './usePrivacy';
import { DebugLogPanel } from './debug/DebugLogPanel';
import classes from './Privacy.module.css';
import shared from '../SettingsPage.module.css';

// 隐私边界子页面（FR-08）：
//   1) 离线开关（读来源 cli/env/user；env 接管时禁用并说明，不假装能改）
//   2) 出站记录表（时间 / 类型 / 目标 / 结果 / 错误码）+ 分页 + 清空留痕（原生确认）
//   3) 零出站自检（结论 + 违例明细）
//   4) 调试模式（开关 + 存储与保留说明 + 查看器；独立子组件 debug/DebugLogPanel）
//   5) 诊断包导出（含什么 / 不含什么；保存走原生「另存为」）
//
// 文案 namespace：settings.privacy.*
export const Privacy = () => {
  const vm = usePrivacy();
  const t = useT();
  const { current } = useI18n();

  const status = vm.status;
  const envVar = status?.offlineEnvVar || 'MANDARYN_OFFLINE';

  // 清空留痕：破坏性操作，必须原生确认。
  const handleClear = async (): Promise<void> => {
    if (vm.recordsBusy) return;
    const ok = await NativeDialogs.confirm({
      title: t('settings.privacy.records.confirmClear.title'),
      message: t('settings.privacy.records.confirmClear.message'),
      okLabel: t('settings.privacy.records.confirmClear.ok'),
      cancelLabel: t('settings.privacy.records.confirmClear.cancel'),
    });
    if (ok) await vm.clearRecords();
  };

  // 另存为：路径只能由原生对话框给出（禁止 <input type=file> / a[download]）。
  const handleSave = async (): Promise<void> => {
    if (!vm.artifact || vm.exportBusy) return;
    const path = await NativeDialogs.saveFile({
      title: t('settings.privacy.diagnostics.dialog.title'),
      message: t('settings.privacy.diagnostics.dialog.message'),
      buttonText: t('settings.privacy.diagnostics.dialog.confirm'),
      filename: vm.artifact.filename || 'mandaryn-diagnostics.json',
      filters: [{ displayName: t('settings.privacy.diagnostics.dialog.filter'), pattern: '*.json' }],
      canCreateDirectories: true,
    });
    if (path) await vm.saveArtifact(path);
  };

  return (
    <div className={shared.section}>
      <div className={shared.sectionHeader}>
        <Text className={shared.sectionTitle}>{t('settings.privacy.title')}</Text>
        <Text className={shared.sectionHint}>{t('settings.privacy.hint')}</Text>
      </div>

      {/* —— 1. 离线开关 —— */}
      <div className={classes.card}>
        <div className={classes.cardHeader}>
          <IconWifiOff size={20} stroke={1.75} className={classes.cardIcon} />
          <Text className={classes.cardTitle}>{t('settings.privacy.offline.title')}</Text>
          <span className={classes.stateBadge} data-off={status?.offline ? undefined : 'on'}>
            {status?.offline
              ? t('settings.privacy.offline.stateOn')
              : t('settings.privacy.offline.stateOff')}
          </span>
        </div>

        <div className={classes.switchRow}>
          <Switch
            checked={status?.offline ?? false}
            onChange={(event) => void vm.toggleOffline(event.currentTarget.checked)}
            disabled={vm.offlineBusy || vm.statusLoading || vm.envForced}
            label={t('settings.privacy.offline.switchLabel')}
            aria-label={t('settings.privacy.offline.switchLabel')}
          />
        </div>

        {/* 来源：cli / env / user（后端 offlineSource） */}
        <div className={classes.metaRow}>
          <Text className={classes.metaLabel}>{t('settings.privacy.offline.sourceLabel')}</Text>
          <Text className={classes.metaValue}>
            {sourceLabel(status?.offlineSource, t)}
          </Text>
        </div>

        {vm.envForced && (
          <div className={classes.warnBox} role="status">
            <IconAlertTriangle size={20} stroke={1.75} className={classes.warnIcon} />
            <div className={classes.warnTexts}>
              <Text className={classes.warnTitle}>
                {t('settings.privacy.offline.envForced.title', { env: envVar })}
              </Text>
              <Text className={classes.warnBody}>
                {t('settings.privacy.offline.envForced.body', { env: envVar })}
              </Text>
            </div>
          </div>
        )}

        <div className={classes.statGrid}>
          <Stat
            label={t('settings.privacy.offline.stats.real')}
            value={status ? String(status.realCount) : null}
          />
          <Stat
            label={t('settings.privacy.offline.stats.blocked')}
            value={status ? String(status.blockedCount) : null}
            hint={t('settings.privacy.offline.stats.blockedHint')}
          />
          <Stat
            label={t('settings.privacy.offline.stats.pending')}
            value={status ? String(status.pendingCount) : null}
          />
          <Stat
            label={t('settings.privacy.offline.stats.lastOutbound')}
            value={
              status
                ? status.lastOutboundAt > 0
                  ? formatDateTime(status.lastOutboundAt, current.code)
                  : t('settings.privacy.offline.stats.never')
                : null
            }
          />
        </div>
        <Text className={classes.cardHint}>
          {t('settings.privacy.offline.retention', {
            hours: status?.retentionHours ?? 24,
          })}
        </Text>
      </div>

      {/* —— 2. 出站记录 —— */}
      <div className={classes.card}>
        <div className={classes.cardHeader}>
          <IconScan size={20} stroke={1.75} className={classes.cardIcon} />
          <Text className={classes.cardTitle}>{t('settings.privacy.records.title')}</Text>
          <div className={classes.cardActions}>
            <Button
              variant="default"
              size="xs"
              leftSection={<IconRefresh size={16} stroke={1.75} />}
              disabled={vm.recordsBusy}
              onClick={() => vm.goToPage(vm.pageIndex)}
            >
              {t('settings.privacy.records.refresh')}
            </Button>
            <Button
              variant="default"
              size="xs"
              leftSection={<IconTrash size={16} stroke={1.75} />}
              disabled={vm.recordsBusy || vm.pageCount === 0 || (vm.page?.total ?? 0) === 0}
              onClick={() => void handleClear()}
            >
              {t('settings.privacy.records.clear')}
            </Button>
          </div>
        </div>

        <RecordsTable
          records={vm.page?.records ?? []}
          loading={vm.recordsBusy}
          localeCode={current.code}
        />

        {vm.pageCount > 1 && (
          <div className={classes.paginationRow}>
            <Pagination
              size="sm"
              total={vm.pageCount}
              value={vm.pageIndex + 1}
              onChange={(page) => vm.goToPage(page - 1)}
              disabled={vm.recordsBusy}
              aria-label={t('settings.privacy.records.paginationAria')}
            />
            <Text className={classes.cardHint}>
              {t('settings.privacy.records.total', { total: vm.page?.total ?? 0 })}
            </Text>
          </div>
        )}
      </div>

      {/* —— 3. 零出站自检 —— */}
      <div className={classes.card}>
        <div className={classes.cardHeader}>
          <IconShieldLock size={20} stroke={1.75} className={classes.cardIcon} />
          <Text className={classes.cardTitle}>{t('settings.privacy.audit.title')}</Text>
          <div className={classes.cardActions}>
            <Button
              variant="filled"
              size="xs"
              leftSection={<IconScan size={16} stroke={1.75} />}
              loading={vm.auditBusy}
              onClick={() => void vm.runAudit()}
            >
              {t('settings.privacy.audit.run')}
            </Button>
          </div>
        </div>
        <Text className={classes.cardHint}>{t('settings.privacy.audit.hint')}</Text>

        {vm.audit && <AuditResult audit={vm.audit} localeCode={current.code} />}
      </div>

      {/* —— 4. 调试模式 —— */}
      <DebugLogPanel />

      {/* —— 5. 诊断包 —— */}
      <div className={classes.card}>
        <div className={classes.cardHeader}>
          <IconDeviceFloppy size={20} stroke={1.75} className={classes.cardIcon} />
          <Text className={classes.cardTitle}>{t('settings.privacy.diagnostics.title')}</Text>
        </div>

        {/* 含什么 / 不含什么 —— 必须讲清楚，否则「导出诊断」等于索要信任 */}
        <div className={classes.scopeBox}>
          <div className={classes.scopeColumn}>
            <Text className={classes.scopeTitle}>{t('settings.privacy.diagnostics.includes.title')}</Text>
            <Text className={classes.scopeItem}>{t('settings.privacy.diagnostics.includes.logs')}</Text>
            <Text className={classes.scopeItem}>{t('settings.privacy.diagnostics.includes.models')}</Text>
            <Text className={classes.scopeItem}>{t('settings.privacy.diagnostics.includes.env')}</Text>
            <Text className={classes.scopeItem}>{t('settings.privacy.diagnostics.includes.outbound')}</Text>
          </div>
          <div className={classes.scopeColumn} data-tone="danger">
            <Text className={classes.scopeTitle}>{t('settings.privacy.diagnostics.excludes.title')}</Text>
            <Text className={classes.scopeItem}>{t('settings.privacy.diagnostics.excludes.audio')}</Text>
            <Text className={classes.scopeItem}>{t('settings.privacy.diagnostics.excludes.text')}</Text>
            <Text className={classes.scopeItem}>{t('settings.privacy.diagnostics.excludes.voiceprint')}</Text>
            <Text className={classes.scopeItem}>{t('settings.privacy.diagnostics.excludes.credentials')}</Text>
          </div>
        </div>

        <div className={classes.cardActions}>
          <Button
            variant="filled"
            size="xs"
            leftSection={<IconDeviceFloppy size={16} stroke={1.75} />}
            loading={vm.exportBusy}
            onClick={() => void vm.buildDiagnostics()}
          >
            {t('settings.privacy.diagnostics.build')}
          </Button>
          {vm.artifact && (
            <Button
              variant="default"
              size="xs"
              leftSection={<IconDeviceFloppy size={16} stroke={1.75} />}
              disabled={vm.exportBusy}
              onClick={() => void handleSave()}
            >
              {t('settings.privacy.diagnostics.save')}
            </Button>
          )}
        </div>

        {vm.artifact && (
          <div className={classes.artifactBox}>
            <Text className={classes.artifactName}>{vm.artifact.filename}</Text>
            <Text className={classes.cardHint}>
              {t('settings.privacy.diagnostics.meta', {
                size: formatBytes(vm.artifactBytes),
                redactions: vm.artifact.redactions,
                dropped: vm.artifact.dropped,
              })}
            </Text>
            {!SAVE_DIAGNOSTICS_SUPPORTED && (
              <Text className={classes.saveCaveat}>
                {t('settings.privacy.diagnostics.saveUnavailable')}
              </Text>
            )}
          </div>
        )}
      </div>

      <NoticeLine notice={vm.notice} />
    </div>
  );
};

type T = ReturnType<typeof useT>;

// —— 出站记录表 ——
interface RecordsTableProps {
  records: OutboundRecord[];
  loading: boolean;
  localeCode: string;
}

const RecordsTable = ({ records, loading, localeCode }: RecordsTableProps) => {
  const t = useT();
  if (records.length === 0) {
    return (
      <Text className={classes.cardHint}>
        {loading ? t('settings.privacy.records.loading') : t('settings.privacy.records.empty')}
      </Text>
    );
  }
  return (
    <div className={classes.tableWrap}>
      <Table striped highlightOnHover stickyHeader className={classes.table}>
        <Table.Thead>
          <Table.Tr>
            <Table.Th>{t('settings.privacy.records.columns.at')}</Table.Th>
            <Table.Th>{t('settings.privacy.records.columns.kind')}</Table.Th>
            <Table.Th>{t('settings.privacy.records.columns.host')}</Table.Th>
            <Table.Th>{t('settings.privacy.records.columns.result')}</Table.Th>
            <Table.Th>{t('settings.privacy.records.columns.code')}</Table.Th>
          </Table.Tr>
        </Table.Thead>
        <Table.Tbody>
          {records.map((record, index) => (
            <Table.Tr key={`${record.at}-${record.host}-${index}`}>
              <Table.Td className={classes.cellMono}>
                {formatDateTime(record.at, localeCode)}
              </Table.Td>
              <Table.Td>{kindLabel(record.kind, t)}</Table.Td>
              <Table.Td className={classes.cellHost}>{record.host || '—'}</Table.Td>
              <Table.Td>
                <span className={classes.resultTag} data-result={resultTone(record)}>
                  {resultLabel(record, t)}
                </span>
              </Table.Td>
              <Table.Td className={classes.cellMono}>
                {record.result && record.result !== 'ok' ? record.result : '—'}
              </Table.Td>
            </Table.Tr>
          ))}
        </Table.Tbody>
      </Table>
    </div>
  );
};

// 结果列的三态：成功 / 被拦下（没有真的出站）/ 失败或未知。
const resultTone = (record: OutboundRecord): 'ok' | 'blocked' | 'error' => {
  if (record.blocked) return 'blocked';
  if (record.pending) return 'error';
  return record.result === 'ok' ? 'ok' : 'error';
};

const resultLabel = (record: OutboundRecord, t: T): string => {
  if (record.blocked) return t('settings.privacy.records.results.blocked');
  if (record.pending) return t('settings.privacy.records.results.pending');
  return record.result === 'ok'
    ? t('settings.privacy.records.results.ok')
    : t('settings.privacy.records.results.failed');
};

// —— 自检结论 ——
interface AuditResultProps {
  audit: ReturnType<typeof usePrivacy>['audit'];
  localeCode: string;
}

const AuditResult = ({ audit, localeCode }: AuditResultProps) => {
  const t = useT();
  if (!audit) return null;

  const violations = audit.violations ?? [];
  return (
    <div className={classes.auditBox}>
      <div className={classes.auditVerdict} data-zero={audit.zeroOutbound || undefined}>
        {audit.zeroOutbound ? (
          <IconCircleCheck size={20} stroke={1.75} />
        ) : (
          <IconAlertTriangle size={20} stroke={1.75} />
        )}
        <Text className={classes.auditVerdictText}>
          {audit.zeroOutbound
            ? t('settings.privacy.audit.verdictZero')
            : t('settings.privacy.audit.verdictNonZero', { count: violations.length })}
        </Text>
      </div>

      <Text className={classes.cardHint}>
        {t('settings.privacy.audit.window', {
          from: formatDateTime(audit.fromMs, localeCode),
          to: formatDateTime(audit.toMs, localeCode),
        })}
      </Text>
      <Text className={classes.cardHint}>
        {t('settings.privacy.audit.counts', {
          scanned: audit.scanned,
          blocked: audit.allowedCount,
        })}
      </Text>

      {violations.length > 0 && (
        <div className={classes.violationList}>
          <Text className={classes.violationTitle}>{t('settings.privacy.audit.violationsTitle')}</Text>
          {violations.map((finding, index) => (
            <div className={classes.violationRow} key={`${finding.atMs}-${finding.host}-${index}`}>
              <Text className={classes.violationTime}>{formatDateTime(finding.atMs, localeCode)}</Text>
              <Text className={classes.violationHost}>{finding.host || '—'}</Text>
              <Text className={classes.violationReason}>{reasonLabel(finding.reason, t)}</Text>
            </div>
          ))}
        </div>
      )}

      {(audit.notes ?? []).length > 0 && (
        <div className={classes.noteList}>
          {audit.notes.map((note, index) => (
            <Text className={classes.cardHint} key={`note-${index}`}>
              {note}
            </Text>
          ))}
        </div>
      )}
    </div>
  );
};

interface StatProps {
  label: string;
  value: string | null;
  hint?: string;
}

const Stat = ({ label, value, hint }: StatProps) => (
  <div className={classes.stat}>
    <Text className={classes.statLabel}>{label}</Text>
    <Text className={classes.statValue}>{value ?? '…'}</Text>
    {hint && <Text className={classes.statHint}>{hint}</Text>}
  </div>
);

// —— 反馈行 ——
interface NoticeLineProps {
  notice: PrivacyNotice;
}

const NoticeLine = ({ notice }: NoticeLineProps) => {
  const t = useT();
  if (notice.kind === 'idle') return null;

  if (notice.kind === 'error') {
    const key = notice.code ? `settings.privacy.errors.${notice.code}` : '';
    const translated = key ? t(key) : '';
    return (
      <div className={classes.errorLine} role="alert">
        <IconAlertTriangle size={18} stroke={1.75} />
        <Text className={classes.errorText}>
          {key && translated !== key ? translated : notice.message}
        </Text>
      </div>
    );
  }

  const text =
    notice.kind === 'cleared'
      ? t('settings.privacy.feedback.cleared', { deleted: notice.deleted })
      : notice.kind === 'exported'
        ? t('settings.privacy.feedback.exported')
        : notice.kind === 'saved'
          ? t('settings.privacy.feedback.saved')
          : t('settings.privacy.feedback.saveUnsupported');

  return (
    <div className={classes.infoLine} role="status">
      <IconCircleCheck size={18} stroke={1.75} />
      <Text className={classes.infoText}>{text}</Text>
    </div>
  );
};

// —— 文案映射助手 ——
const sourceLabel = (source: string | undefined, t: T): string => {
  switch (source) {
    case 'env':
      return t('settings.privacy.offline.sources.env');
    case 'user':
      return t('settings.privacy.offline.sources.user');
    case 'env+user':
      return t('settings.privacy.offline.sources.envAndUser');
    case 'cli':
      return t('settings.privacy.offline.sources.cli');
    case '':
    case undefined:
      return t('settings.privacy.offline.sources.none');
    default:
      // 后端将来新增来源层时原样显示，不要假装是「未开启」
      return source;
  }
};

const kindLabel = (kind: string, t: T): string => {
  const key = `settings.privacy.records.kinds.${kind}`;
  const translated = t(key);
  return translated === key ? kind || '—' : translated;
};

// 违例原因键（后端 AuditFinding.reason：real-outbound-while-offline / unfinished-attempt …）
const reasonLabel = (reason: string, t: T): string => {
  const key = `settings.privacy.audit.reasons.${reason}`;
  const translated = t(key);
  return translated === key ? reason || '—' : translated;
};

const formatDateTime = (ms: number, localeCode: string): string => {
  if (!ms) return '—';
  const at = new Date(ms);
  if (Number.isNaN(at.getTime())) return '—';
  return at.toLocaleString(localeCode);
};

const KB = 1024;
const MB = KB * 1024;

const formatBytes = (n: number): string => {
  if (n < KB) return `${n} B`;
  if (n < MB) return `${(n / KB).toFixed(1)} KB`;
  return `${(n / MB).toFixed(1)} MB`;
};
