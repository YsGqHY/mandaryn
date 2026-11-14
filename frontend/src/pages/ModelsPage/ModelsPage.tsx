import {
  ActionIcon,
  Button,
  EmptyState,
  Loader,
  Paper,
  Progress,
  Text,
  Tooltip,
} from '@mantine/core';
import {
  IconAlertTriangle,
  IconArrowRight,
  IconCopy,
  IconDownload,
  IconFolder,
  IconFolderOpen,
  IconPackage,
  IconPlayerPause,
  IconPlayerPlay,
  IconRefresh,
  IconShieldCheck,
  IconTrash,
  IconX,
} from '@tabler/icons-react';
import clsx from 'clsx';
import { useT } from '@/i18n';
import {
  NativeDialogs,
  type BundleInfo,
  type DownloadState,
  type InstalledBundle,
  type MigratePlan,
  type MigrateState,
} from '@/services';
import { isManagedSource, type ResolvedPath } from '@/services/models/PathsService';
import { useModelsPage } from './useModelsPage';
import classes from './ModelsPage.module.css';

// ErrorLike：页面只用到错误码与消息（AppError 与契约 ErrorDTO 都满足）。
type ErrorLike = { code: string; message: string };

// Models 页面（路由 id: models）：
//   1) 仓库概览 —— GetStoreStats() 的根路径 / 占用 / 未完成下载
//   2) 已装列表 —— ListInstalled()，可校验 / 删除（删除走原生确认）
//   3) 可下载列表 —— ListAvailable()，下载 / 暂停 / 继续 / 取消，进度来自 models:download
//   4) 模型目录 —— 原生选目录 → SetModelRoot 预检 → 确认 → MigrateModelRoot，
//      进度来自 models:migrate
//
// 原生对话框（确认 / 选目录 / 提示）全部走 NativeDialogs；文案全部走 t()。
export const ModelsPage = () => {
  const t = useT();
  const vm = useModelsPage();

  // 错误码 → 文案；命中不到时回落 errors.unknown 并带上后端 message。
  // 形参故意只要求 code / message：迁移状态里的 lastError 是 DTO，不是 AppError 实例。
  const errorText = (err: ErrorLike): string => {
    const key = `models.errors.${err.code}`;
    const translated = t(key, { message: err.message });
    if (err.code !== '' && translated !== key) return translated;
    return t('models.errors.unknown', { message: err.message });
  };

  // 清单拉取失败：优先用「获取清单」语境的文案（与下载失败共用错误码，但说法不同），
  // 没有专门文案时回落到通用错误码文案。
  const catalogErrorText = (err: ErrorLike): string => {
    const key = `models.available.loadErrors.${err.code}`;
    const translated = err.code !== '' ? t(key) : key;
    return translated !== key ? translated : errorText(err);
  };

  // 迁移警告键（如 network-path）→ 文案；未知键原样展示。
  const warningText = (warning: string): string => {
    const key = `models.migrate.warnings.${warning}`;
    const translated = t(key, { warning });
    return translated === key ? warning : translated;
  };

  const handleVerify = async (bundle: InstalledBundle): Promise<void> => {
    const result = await vm.verifyBundle(bundle.bundleId);
    if (result === null) return;
    const note = result.note !== '' ? `\n${result.note}` : '';
    if (result.ok) {
      await NativeDialogs.info(
        t('models.verify.title'),
        `${t('models.verify.okMessage', { name: bundle.name })}${note}`,
      );
      return;
    }
    const files = result.badFiles.join(', ');
    const message =
      files !== ''
        ? t('models.verify.failedMessage', { name: bundle.name, files })
        : t('models.verify.failedNoFiles', { name: bundle.name });
    await NativeDialogs.error(t('models.verify.title'), `${message}${note}`);
  };

  const handleRemove = async (bundle: InstalledBundle): Promise<void> => {
    const ok = await NativeDialogs.confirm({
      title: t('models.remove.title'),
      message: t('models.remove.message', { name: bundle.name }),
      okLabel: t('models.remove.ok'),
      cancelLabel: t('models.remove.cancel'),
    });
    if (ok) await vm.removeBundle(bundle.bundleId);
  };

  // 更改模型目录：选目录 → 预检 → （计划卡片）→ 二次确认 → 执行。
  //
  // 分两种情况（由后端给的 source 决定，前端不自行推断优先级，ADR-02）：
  //   - 用户层可生效（default / user）：走迁移（复制 → 校验 → 切换 → 清理）；
  //   - 被 env / cli 接管：只写暂存值，绝不搬数据（搬到不生效的目录等于把
  //     模型文件挪进死角），并说明「解除接管并重启后生效」。
  const handleChangeRoot = async (): Promise<void> => {
    if (vm.migrating) return;
    const dir = await NativeDialogs.openDirectory({
      title: t('models.migrate.dialogTitle'),
      message: t('models.migrate.dialogMessage'),
      buttonText: t('models.migrate.changeBtn'),
    });
    if (dir === null) return;

    const managed = vm.modelRoot !== null && isManagedSource(vm.modelRoot.source);
    if (managed) {
      const resolved = await vm.stageModelRoot(dir);
      if (resolved === null) return;
      const pending = resolved.pendingValue !== '' ? resolved.pendingValue : dir;
      await NativeDialogs.info(
        t('models.paths.stagedTitle'),
        [
          t('models.paths.stagedMessage', { path: pending }),
          resolved.envName !== ''
            ? t('models.paths.stagedEnvNote', { env: resolved.envName })
            : '',
          resolved.overrideHint !== ''
            ? `${t('models.paths.overrideHintLabel')}:\n${resolved.overrideHint}`
            : '',
        ]
          .filter((line) => line !== '')
          .join('\n'),
      );
      return;
    }

    // 预检只出计划、不动数据；不可行时后端直接报 ERR_MIGRATE_PRECHECK，错误已进 vm.error。
    await vm.planMigrate(dir);
  };

  // 计划卡片的「开始迁移」：二次原生确认后执行。
  const handleConfirmPlan = async (plan: MigratePlan): Promise<void> => {
    const warningLines = plan.warnings.map(warningText).join('\n');
    const message = [
      t('models.migrate.confirmMessage', {
        count: plan.bundleCount,
        total: formatBytes(plan.totalBytes),
        from: plan.from,
        to: plan.to,
        free: formatBytes(plan.freeBytes),
      }),
      warningLines !== '' ? `${t('models.migrate.planWarnings')}:\n${warningLines}` : '',
    ]
      .filter((line) => line !== '')
      .join('\n');

    const ok = await NativeDialogs.confirm({
      title: t('models.migrate.confirmTitle'),
      message,
      okLabel: t('models.migrate.okLabel'),
      cancelLabel: t('models.migrate.cancelLabel'),
    });
    if (ok) await vm.startMigrate(plan);
  };

  // 迁移中取消：必须说清「会回到原目录」，避免用户以为会丢掉已复制的数据。
  const handleCancelMigrate = async (): Promise<void> => {
    const ok = await NativeDialogs.confirm({
      title: t('models.migrate.cancelTitle'),
      message: t('models.migrate.cancelMessage'),
      okLabel: t('models.migrate.cancelOk'),
      cancelLabel: t('models.migrate.cancelLabel'),
    });
    if (ok) await vm.cancelMigrate();
  };

  const handleCopyHint = async (text: string): Promise<void> => {
    await vm.copyText(text);
  };

  // 提到局部常量：闭包里再读 vm.plan 会丢窄化（TS 无法保证非 null），
  // 之前靠 `as MigratePlan` 断言，改成本地快照更安全。
  const pendingPlan: MigratePlan | null = vm.plan;

  return (
    <div className={classes.root}>
      <div className={classes.body}>
        <div>
          <Text className={classes.eyebrow}>{t('models.eyebrow')}</Text>
          <h1 className={classes.title}>{t('models.title')}</h1>
        </div>
        <Text className={classes.subtitle}>{t('models.subtitle')}</Text>

        {/* 错误提示 */}
        {vm.error && (
          <div className={classes.errorRow}>
            <Text className={classes.error}>{errorText(vm.error)}</Text>
            <ActionIcon
              variant="subtle"
              aria-label={t('models.errors.dismiss')}
              onClick={vm.dismissError}
            >
              <IconX size={16} stroke={1.75} />
            </ActionIcon>
          </div>
        )}

        {/* —— 仓库概览 + 模型目录 —— */}
        <Paper className={classes.card} withBorder radius="md" p="lg">
          <div className={classes.cardHeader}>
            <div className={classes.cardTitleGroup}>
              <span className={classes.cardIcon}>
                <IconFolder size={18} stroke={1.75} />
              </span>
              <Text className={classes.cardTitle}>{t('models.store.title')}</Text>
            </div>
            <div className={classes.actionsRow}>
              <Tooltip label={t('models.store.refresh')} withArrow>
                <ActionIcon
                  variant="default"
                  aria-label={t('models.store.refresh')}
                  onClick={() => void vm.reload()}
                >
                  <IconRefresh size={18} stroke={1.75} />
                </ActionIcon>
              </Tooltip>
              <Button
                variant="filled"
                leftSection={<IconFolder size={18} stroke={1.75} />}
                disabled={vm.migrating}
                onClick={() => void handleChangeRoot()}
              >
                {t('models.migrate.changeBtn')}
              </Button>
            </div>
          </div>

          {vm.stats === null ? (
            <Text className={classes.cardHint}>{t('models.store.loading')}</Text>
          ) : (
            <div className={classes.statGrid}>
              <div className={classes.statItem}>
                <Text className={classes.statLabel}>{t('models.store.rootLabel')}</Text>
                <Text className={classes.statValue}>{vm.stats.root}</Text>
              </div>
              <div className={classes.statItem}>
                <Text className={classes.statLabel}>{t('models.store.totalLabel')}</Text>
                <Text className={classes.statValue}>{formatBytes(vm.stats.totalBytes)}</Text>
              </div>
              <div className={classes.statItem}>
                <Text className={classes.statLabel}>{t('models.store.downloadsLabel')}</Text>
                <Text className={classes.statValue}>{formatBytes(vm.stats.downloadsBytes)}</Text>
              </div>
              <div className={classes.statItem}>
                <Text className={classes.statLabel}>{t('models.store.bundleCountLabel')}</Text>
                <Text className={classes.statValue}>{vm.stats.bundles.length}</Text>
              </div>
            </div>
          )}

          {/* 生效路径 + 来源（source 由后端给出，前端只按它渲染，ADR-02） */}
          <PathSourceBlock
            root={vm.modelRoot}
            onCopyHint={(text) => void handleCopyHint(text)}
            onReveal={() => void vm.revealModelRoot()}
          />

          {/* 待确认的迁移计划：先看「搬多少 / 目标剩多少」，再决定要不要开始 */}
          {pendingPlan !== null && (
            <PlanCard
              plan={pendingPlan}
              warningText={warningText}
              onConfirm={() => void handleConfirmPlan(pendingPlan)}
              onDiscard={vm.discardPlan}
            />
          )}

          {/* 迁移进度（models:migrate） */}
          {vm.migrate && (
            <MigrateProgress
              state={vm.migrate}
              migrating={vm.migrating}
              canRetry={vm.attemptedPlan !== null}
              errorText={errorText}
              onCancel={() => void handleCancelMigrate()}
              onRetry={() => void vm.retryMigrate()}
            />
          )}
        </Paper>

        {/* —— 已安装模型 —— */}
        <Paper className={classes.card} withBorder radius="md" p="lg">
          <div className={classes.cardHeader}>
            <div className={classes.cardTitleGroup}>
              <span className={classes.cardIcon}>
                <IconShieldCheck size={18} stroke={1.75} />
              </span>
              <Text className={classes.cardTitle}>{t('models.installed.title')}</Text>
            </div>
            <Text className={classes.cardHint}>
              {t('models.installed.countLabel', { count: vm.installed.length })}
            </Text>
          </div>

          {vm.loading ? (
            <Loader size="sm" />
          ) : vm.installed.length === 0 ? (
            <div className={classes.emptyBlock}>
              <EmptyState
                icon={<IconPackage size={32} stroke={1.75} />}
                title={t('models.installed.emptyTitle')}
                description={t('models.installed.emptyDescription')}
              />
            </div>
          ) : (
            <div className={classes.rowList}>
              {vm.installed.map((bundle) => (
                <InstalledRow
                  key={bundle.bundleId}
                  bundle={bundle}
                  busy={vm.busyBundleId === bundle.bundleId}
                  onVerify={() => void handleVerify(bundle)}
                  onRemove={() => void handleRemove(bundle)}
                />
              ))}
            </div>
          )}
        </Paper>

        {/* —— 可下载模型 —— */}
        <Paper className={classes.card} withBorder radius="md" p="lg">
          <div className={classes.cardHeader}>
            <div className={classes.cardTitleGroup}>
              <span className={classes.cardIcon}>
                <IconDownload size={18} stroke={1.75} />
              </span>
              <Text className={classes.cardTitle}>{t('models.available.title')}</Text>
            </div>
            <Text className={classes.cardHint}>
              {t('models.available.countLabel', { count: vm.available.length })}
            </Text>
          </div>

          {vm.loading ? (
            <Loader size="sm" />
          ) : vm.available.length === 0 ? (
            <div className={classes.emptyBlock}>
              <EmptyState
                icon={<IconPackage size={32} stroke={1.75} />}
                title={t('models.available.emptyTitle')}
                description={
                  vm.catalogError !== null
                    ? catalogErrorText(vm.catalogError)
                    : t('models.available.emptyDescription')
                }
              />
            </div>
          ) : (
            <div className={classes.rowList}>
              {vm.available.map((bundle) => (
                <AvailableRow
                  key={bundle.bundleId}
                  bundle={bundle}
                  download={vm.downloads[bundle.bundleId] ?? null}
                  busy={vm.busyBundleId === bundle.bundleId}
                  onDownload={() => void vm.startDownload(bundle.bundleId)}
                  onPause={() => void vm.pauseDownload(bundle.bundleId)}
                  onCancel={() => void vm.cancelDownload(bundle.bundleId)}
                />
              ))}
            </div>
          )}
        </Paper>
      </div>
    </div>
  );
};

// —— 已装行（仅本页使用，不进 index.ts） ——
interface InstalledRowProps {
  bundle: InstalledBundle;
  busy: boolean;
  onVerify: () => void;
  onRemove: () => void;
}

const InstalledRow = ({ bundle, busy, onVerify, onRemove }: InstalledRowProps) => {
  const t = useT();
  const stateTone =
    bundle.state === 'corrupt' ? 'danger' : bundle.state === 'missing' ? 'warning' : 'success';
  const stateLabel =
    bundle.state === 'corrupt'
      ? t('models.installed.state.corrupt')
      : bundle.state === 'missing'
        ? t('models.installed.state.missing')
        : bundle.state === 'installed'
          ? t('models.installed.state.installed')
          : t('models.installed.state.unknown');

  return (
    <div className={classes.row}>
      <div className={classes.rowMain}>
        <div className={classes.rowTitleLine}>
          <span className={classes.rowName}>{bundle.name}</span>
          <span className={classes.chip} data-tone={stateTone}>
            {stateLabel}
          </span>
        </div>
        <div className={classes.rowMetaLine}>
          <span>{t('models.installed.versionLabel', { version: bundle.version })}</span>
          <span>{t('models.installed.sizeLabel', { size: formatBytes(bundle.sizeBytes) })}</span>
          {bundle.languages.length > 0 && (
            <span>
              {t('models.installed.languagesLabel', { languages: bundle.languages.join(', ') })}
            </span>
          )}
          {bundle.roles.length > 0 && (
            <span>{t('models.installed.rolesLabel', { roles: bundle.roles.join(', ') })}</span>
          )}
        </div>
      </div>
      <div className={classes.rowActions}>
        <Button
          variant="default"
          size="xs"
          leftSection={<IconShieldCheck size={16} stroke={1.75} />}
          loading={busy}
          onClick={onVerify}
        >
          {t('models.installed.actions.verify')}
        </Button>
        <Button
          variant="default"
          size="xs"
          leftSection={<IconTrash size={16} stroke={1.75} />}
          disabled={busy}
          onClick={onRemove}
        >
          {t('models.installed.actions.remove')}
        </Button>
      </div>
    </div>
  );
};

// —— 可下载行 + 下载进度 ——
interface AvailableRowProps {
  bundle: BundleInfo;
  download: DownloadState | null;
  busy: boolean;
  onDownload: () => void;
  onPause: () => void;
  onCancel: () => void;
}

const AvailableRow = ({ bundle, download, busy, onDownload, onPause, onCancel }: AvailableRowProps) => {
  const t = useT();

  return (
    <div className={classes.row}>
      <div className={classes.rowMain}>
        <div className={classes.rowTitleLine}>
          <span className={classes.rowName}>{bundle.name}</span>
          <span className={classes.chip} data-tone="muted">
            {bundle.version}
          </span>
          {bundle.installed && (
            <span className={classes.chip} data-tone="success">
              {t('models.available.installedBadge')}
            </span>
          )}
          {bundle.updateAvailable && (
            <span className={classes.chip} data-tone="accent">
              {t('models.available.updateBadge')}
            </span>
          )}
        </div>
        <div className={classes.rowMetaLine}>
          <span>{t('models.available.sizeLabel', { size: formatBytes(bundle.sizeBytes) })}</span>
          <span>{t('models.available.ramLabel', { mb: bundle.ramHintMb })}</span>
          {bundle.languages.length > 0 && (
            <span>
              {t('models.available.languagesLabel', { languages: bundle.languages.join(', ') })}
            </span>
          )}
          {bundle.license !== '' && (
            <span>{t('models.available.licenseLabel', { license: bundle.license })}</span>
          )}
          {bundle.roles.length > 0 && (
            <span>{t('models.available.rolesLabel', { roles: bundle.roles.join(', ') })}</span>
          )}
        </div>
        {download !== null && <DownloadProgress state={download} />}
      </div>
      <div className={classes.rowActions}>
        {download === null || download.state === 'failed' || download.state === 'done' ? (
          <Button
            variant="filled"
            size="xs"
            leftSection={<IconDownload size={16} stroke={1.75} />}
            loading={busy}
            onClick={onDownload}
          >
            {download?.state === 'failed'
              ? t('models.available.actions.retry')
              : t('models.available.actions.download')}
          </Button>
        ) : (
          <>
            {download.state === 'paused' ? (
              <Button
                variant="default"
                size="xs"
                leftSection={<IconPlayerPlay size={16} stroke={1.75} />}
                onClick={onDownload}
              >
                {t('models.available.actions.resume')}
              </Button>
            ) : (
              <Button
                variant="default"
                size="xs"
                leftSection={<IconPlayerPause size={16} stroke={1.75} />}
                onClick={onPause}
              >
                {t('models.available.actions.pause')}
              </Button>
            )}
            <Button
              variant="default"
              size="xs"
              leftSection={<IconX size={16} stroke={1.75} />}
              onClick={onCancel}
            >
              {t('models.available.actions.cancel')}
            </Button>
          </>
        )}
      </div>
    </div>
  );
};

// —— 下载进度条 ——
const DownloadProgress = ({ state }: { state: DownloadState }) => {
  const t = useT();
  const percent = percentOf(state.bytesDone, state.bytesTotal);
  const stateLabel =
    state.state === 'queued'
      ? t('models.available.state.queued')
      : state.state === 'downloading'
        ? t('models.available.state.downloading')
        : state.state === 'verifying'
          ? t('models.available.state.verifying')
          : state.state === 'paused'
            ? t('models.available.state.paused')
            : state.state === 'done'
              ? t('models.available.state.done')
              : state.state === 'failed'
                ? t('models.available.state.failed')
                : t('models.available.state.unknown');

  const sectionClass =
    state.state === 'failed'
      ? clsx(classes.progressSection, classes.progressDanger)
      : state.state === 'paused'
        ? clsx(classes.progressSection, classes.progressWarning)
        : classes.progressSection;

  return (
    <div className={classes.progressBlock}>
      <Progress
        value={percent}
        size="sm"
        aria-label={stateLabel}
        classNames={{ root: classes.progressRoot, section: sectionClass }}
      />
      <div className={classes.progressStats}>
        <span className={classes.progressState}>{stateLabel}</span>
        <span>{t('models.available.progressPercent', { percent })}</span>
        <span>
          {t('models.available.progressBytes', {
            done: formatBytes(state.bytesDone),
            total: formatBytes(state.bytesTotal),
          })}
        </span>
        {state.state === 'downloading' && state.speedBps > 0 && (
          <span>
            {t('models.available.progressSpeed', { speed: formatBytes(state.speedBps) })}
          </span>
        )}
        {state.state === 'downloading' && state.etaMs > 0 && (
          <span>{t('models.available.progressEta', { eta: formatDuration(state.etaMs) })}</span>
        )}
        {state.retries > 0 && (
          <span>{t('models.available.progressRetries', { count: state.retries })}</span>
        )}
        {state.mirror !== '' && (
          <span>{t('models.available.progressMirror', { mirror: state.mirror })}</span>
        )}
      </div>
      {state.state === 'failed' && state.lastError !== null && (
        <Text className={classes.progressError}>
          {t('models.errors.unknown', { message: state.lastError.message })}
        </Text>
      )}
    </div>
  );
};

// —— 生效路径与来源 ——
//
// 后端 ResolvedPath 已经把「优先级判定」做完了：前端只读 source 决定文案与
// 可用操作，绝不自己比较 env / cli / user 谁优先（ADR-02）。
interface PathSourceBlockProps {
  root: ResolvedPath | null;
  onCopyHint: (text: string) => void;
  onReveal: () => void;
}

const PathSourceBlock = ({ root, onCopyHint, onReveal }: PathSourceBlockProps) => {
  const t = useT();
  if (root === null) return null;

  const sourceKey = `models.paths.source.${root.source}`;
  const sourceTranslated = t(sourceKey);
  const sourceLabel =
    sourceTranslated === sourceKey
      ? t('models.paths.source.unknown', { source: root.source })
      : sourceTranslated;
  const managed = isManagedSource(root.source);
  // 受管时用户值可能被接管；只要后端给了非空 userValue 就要显示出来，
  // 否则用户会以为「我设的目录丢了」（FR-07-7）。
  const showUserValue = root.userValue !== '' && root.userValue !== root.effective;

  return (
    <div className={classes.pathBlock}>
      <div className={classes.rowTitleLine}>
        <Text className={classes.statLabel}>{t('models.paths.effectiveLabel')}</Text>
        <span className={classes.chip} data-tone={managed ? 'warning' : 'success'}>
          {sourceLabel}
        </span>
        {!root.writable && (
          <span className={classes.chip} data-tone="danger">
            {t('models.paths.notWritable')}
          </span>
        )}
        <div className={classes.actionsRow}>
          <Tooltip label={t('models.paths.open')} withArrow>
            <ActionIcon variant="default" aria-label={t('models.paths.open')} onClick={onReveal}>
              <IconFolderOpen size={16} stroke={1.75} />
            </ActionIcon>
          </Tooltip>
        </div>
      </div>

      <Text className={classes.pathValue}>{root.effective}</Text>

      {showUserValue && (
        <Text className={classes.cardHint}>
          {t('models.paths.userValueLabel', { path: root.userValue })}
        </Text>
      )}

      {root.pendingValue !== '' && (
        <Text className={classes.pendingNotice}>
          {t('models.paths.pendingLabel', { path: root.pendingValue })}
        </Text>
      )}

      {/* 被 env / cli 接管：给出解除提示（可复制），并说明改目录只会写暂存值 */}
      {managed && (
        <div className={classes.hintBlock}>
          <Text className={classes.cardHint}>
            {root.envName !== ''
              ? t('models.paths.managedByEnv', { env: root.envName })
              : t('models.paths.managedByCli')}
          </Text>
          {root.overrideHint !== '' && (
            <>
              <div className={classes.rowTitleLine}>
                <Text className={classes.statLabel}>{t('models.paths.overrideHintLabel')}</Text>
                <Tooltip label={t('models.paths.copyHint')} withArrow>
                  <ActionIcon
                    variant="default"
                    aria-label={t('models.paths.copyHint')}
                    onClick={() => onCopyHint(root.overrideHint)}
                  >
                    <IconCopy size={16} stroke={1.75} />
                  </ActionIcon>
                </Tooltip>
              </div>
              <Text className={classes.monoHint}>{root.overrideHint}</Text>
            </>
          )}
          <Text className={classes.cardHint}>{t('models.paths.managedChangeNote')}</Text>
        </div>
      )}

      {/* 高优先级路径不可用时后端会回退并给 fallbackFrom，必须可见（FR-07-7） */}
      {root.fallbackFrom !== '' && (
        <Text className={classes.fallbackNotice}>
          {t('models.paths.fallbackNotice', { path: root.fallbackFrom })}
        </Text>
      )}
    </div>
  );
};

// —— 迁移计划卡片 ——
interface PlanCardProps {
  plan: MigratePlan;
  warningText: (warning: string) => string;
  onConfirm: () => void;
  onDiscard: () => void;
}

const PlanCard = ({ plan, warningText, onConfirm, onDiscard }: PlanCardProps) => {
  const t = useT();
  const infeasible = !plan.feasible;

  return (
    <div className={classes.planBlock}>
      <div className={classes.rowTitleLine}>
        <IconArrowRight size={16} stroke={1.75} className={classes.planIcon} />
        <Text className={classes.planTitle}>{t('models.migrate.planTitle')}</Text>
      </div>

      <div className={classes.planGrid}>
        <div className={classes.statItem}>
          <Text className={classes.statLabel}>{t('models.migrate.planFrom')}</Text>
          <Text className={classes.planValue}>{plan.from}</Text>
        </div>
        <div className={classes.statItem}>
          <Text className={classes.statLabel}>{t('models.migrate.planTo')}</Text>
          <Text className={classes.planValue}>{plan.to}</Text>
        </div>
        <div className={classes.statItem}>
          <Text className={classes.statLabel}>{t('models.migrate.planTotal')}</Text>
          <Text className={classes.planValue}>{formatBytes(plan.totalBytes)}</Text>
        </div>
        <div className={classes.statItem}>
          <Text className={classes.statLabel}>{t('models.migrate.planFree')}</Text>
          <Text className={classes.planValue}>{formatBytes(plan.freeBytes)}</Text>
        </div>
        <div className={classes.statItem}>
          <Text className={classes.statLabel}>{t('models.migrate.planCount')}</Text>
          <Text className={classes.planValue}>{plan.bundleCount}</Text>
        </div>
      </div>

      {plan.warnings.length > 0 && (
        <div className={classes.hintBlock}>
          <Text className={classes.statLabel}>{t('models.migrate.planWarnings')}</Text>
          {plan.warnings.map((w) => (
            <Text key={w} className={classes.cardHint}>
              {warningText(w)}
            </Text>
          ))}
        </div>
      )}

      {infeasible && (
        <Text className={classes.progressError}>{t('models.migrate.infeasibleMessage')}</Text>
      )}

      <div className={classes.actionsRow}>
        <Button
          variant="filled"
          size="xs"
          leftSection={<IconArrowRight size={16} stroke={1.75} />}
          disabled={infeasible}
          onClick={onConfirm}
        >
          {t('models.migrate.okLabel')}
        </Button>
        <Button variant="default" size="xs" onClick={onDiscard}>
          {t('models.migrate.cancelLabel')}
        </Button>
      </div>
    </div>
  );
};

// —— 迁移进度 + 终态结果 ——
interface MigrateProgressProps {
  state: MigrateState;
  migrating: boolean;
  canRetry: boolean;
  errorText: (err: ErrorLike) => string;
  onCancel: () => void;
  onRetry: () => void;
}

const MigrateProgress = ({
  state,
  migrating,
  canRetry,
  errorText,
  onCancel,
  onRetry,
}: MigrateProgressProps) => {
  const t = useT();
  const percent = percentOf(state.bytesDone, state.bytesTotal);
  const phaseLabel = phaseText(t, state.phase);
  const failed = state.phase === 'failed';
  const rolledBack = state.phase === 'rollback';
  // 回滚但 lastError 为空 = 用户取消（后端约定），文案要区分开。
  const canceled = rolledBack && state.lastError === null;
  const sectionClass =
    failed || (rolledBack && !canceled)
      ? clsx(classes.progressSection, classes.progressDanger)
      : canceled
        ? clsx(classes.progressSection, classes.progressWarning)
        : classes.progressSection;

  return (
    <div className={classes.progressBlock}>
      <div className={classes.progressStats}>
        <span className={classes.progressState}>{phaseLabel}</span>
        <span>
          {t('models.migrate.phaseProgress', {
            done: formatBytes(state.bytesDone),
            total: formatBytes(state.bytesTotal),
          })}
        </span>
        <span>{t('models.migrate.phasePercent', { percent })}</span>
        {state.current !== '' && <span>{state.current}</span>}
      </div>
      <Progress
        value={percent}
        size="sm"
        aria-label={phaseLabel}
        classNames={{ root: classes.progressRoot, section: sectionClass }}
      />

      {migrating && (
        <>
          <Text className={classes.migrateNotice}>{t('models.migrate.notice')}</Text>
          <div className={classes.actionsRow}>
            <Button
              variant="default"
              size="xs"
              leftSection={<IconAlertTriangle size={16} stroke={1.75} />}
              onClick={onCancel}
            >
              {t('models.migrate.cancelMigrate')}
            </Button>
          </div>
        </>
      )}

      {/* 终态：先说清「模型现在在哪儿」，再给重试入口 */}
      {canceled && (
        <Text className={classes.progressWarningText}>{t('models.migrate.result.canceled')}</Text>
      )}
      {rolledBack && !canceled && state.lastError !== null && (
        <Text className={classes.progressError}>{errorText(state.lastError)}</Text>
      )}
      {(failed || rolledBack) && (
        <Text className={classes.cardHint}>{t('models.migrate.result.oldDirUsable')}</Text>
      )}
      {state.phase === 'done' && (
        <Text className={classes.progressSuccessText}>{t('models.migrate.result.done')}</Text>
      )}

      {(failed || rolledBack) && canRetry && (
        <div className={classes.actionsRow}>
          <Button
            variant="default"
            size="xs"
            leftSection={<IconRefresh size={16} stroke={1.75} />}
            onClick={onRetry}
          >
            {t('models.migrate.result.retry')}
          </Button>
        </div>
      )}
    </div>
  );
};

// 迁移阶段 → 文案（未知阶段原样展示后端值）
const phaseText = (
  t: (key: string, vars?: Record<string, string | number | boolean>) => string,
  phase: string,
): string => {
  const key = `models.migrate.phase.${phase}`;
  const translated = t(key);
  return translated === key ? (phase !== '' ? phase : t('models.migrate.phase.idle')) : translated;
};

const KB = 1024;
const MB = KB * 1024;
const GB = MB * 1024;

// 字节格式化：与数据库设置页一致的朴素实现（脚手架够用）。
const formatBytes = (n: number): string => {
  if (n < KB) return `${n} B`;
  if (n < MB) return `${(n / KB).toFixed(1)} KB`;
  if (n < GB) return `${(n / MB).toFixed(1)} MB`;
  return `${(n / GB).toFixed(2)} GB`;
};

// 毫秒 → "mm:ss" / "h:mm:ss"，用于下载剩余时间。
const formatDuration = (ms: number): string => {
  const totalSeconds = Math.max(0, Math.round(ms / 1000));
  const hours = Math.floor(totalSeconds / 3600);
  const minutes = Math.floor((totalSeconds % 3600) / 60);
  const seconds = totalSeconds % 60;
  const pad = (v: number): string => (v < 10 ? `0${v}` : String(v));
  return hours > 0 ? `${hours}:${pad(minutes)}:${pad(seconds)}` : `${minutes}:${pad(seconds)}`;
};

const percentOf = (done: number, total: number): number => {
  if (total <= 0) return 0;
  return Math.min(100, Math.round((done / total) * 100));
};
