import { Button, Switch, Text } from '@mantine/core';
import {
  IconAlertTriangle,
  IconBan,
  IconKeyboard,
  IconRestore,
  IconX,
} from '@tabler/icons-react';
import { useCallback, useEffect, useRef, type KeyboardEvent } from 'react';
import { useT } from '@/i18n';
import { NativeDialogs, type ActionInfo, type HotkeyConflict } from '@/services';
import { captureFromEvent, modifierPreview } from './accelerator';
import { useHotkeys, type HotkeyIssue } from './useHotkeys';
import classes from './Hotkeys.module.css';
import shared from '../SettingsPage.module.css';

// 快捷键子页面（FR-09）：
//   1) 平台可用性：Available=false 时整块提示「不支持系统级快捷键」并禁用控件
//   2) 按场景分组列出动作 + 当前绑定（后端已给显示形）
//   3) 重绑 = 「按下组合键即录制」（监听 keydown 组装），保存前先查冲突
//   4) 冲突（system 系统保留 / duplicate 重复）、注册失败（registerErr）在该行标红给原因
//   5) 禁用 / 单项重置 / 全部重置
//   6) 精简模式开关 + 后端给的取舍清单
//
// 文案 namespace：settings.hotkeys.*
export const Hotkeys = () => {
  const vm = useHotkeys();
  const t = useT();

  const disabledPlatform = vm.available === false;

  const handleResetAll = async (): Promise<void> => {
    if (vm.resetAllBusy) return;
    const ok = await NativeDialogs.confirm({
      title: t('settings.hotkeys.resetAll.title'),
      message: t('settings.hotkeys.resetAll.message'),
      okLabel: t('settings.hotkeys.resetAll.ok'),
      cancelLabel: t('settings.hotkeys.resetAll.cancel'),
    });
    if (ok) await vm.resetAllActions();
  };

  return (
    <div className={shared.section}>
      <div className={shared.sectionHeader}>
        <Text className={shared.sectionTitle}>{t('settings.hotkeys.title')}</Text>
        <Text className={shared.sectionHint}>{t('settings.hotkeys.hint')}</Text>
      </div>

      {/* 平台不支持：整块说明，控件全部禁用 —— 不假装能用 */}
      {disabledPlatform && (
        <div className={classes.warnBox} role="status">
          <IconBan size={20} stroke={1.75} className={classes.warnIcon} />
          <div className={classes.warnTexts}>
            <Text className={classes.warnTitle}>{t('settings.hotkeys.unavailable.title')}</Text>
            <Text className={classes.warnBody}>{t('settings.hotkeys.unavailable.body')}</Text>
          </div>
        </div>
      )}

      <div className={classes.toolbar}>
        <Text className={classes.toolbarHint}>{t('settings.hotkeys.capture.hint')}</Text>
        <Button
          variant="default"
          size="xs"
          leftSection={<IconRestore size={16} stroke={1.75} />}
          disabled={disabledPlatform || vm.resetAllBusy || vm.loading}
          loading={vm.resetAllBusy}
          onClick={() => void handleResetAll()}
        >
          {t('settings.hotkeys.resetAll.action')}
        </Button>
      </div>

      {vm.listIssue && <IssueLine issue={vm.listIssue} />}

      {/* —— 动作列表（按场景分组） —— */}
      {vm.groups.map((group) => (
        <div className={classes.group} key={group.group}>
          <Text className={classes.groupTitle}>{groupLabel(group.group, t)}</Text>
          <div className={classes.rows}>
            {group.actions.map((action) => (
              <HotkeyRow
                key={action.id}
                action={action}
                disabled={disabledPlatform}
                vm={vm}
              />
            ))}
          </div>
        </div>
      ))}

      {/* —— 精简模式 —— */}
      <div className={classes.card}>
        <div className={classes.cardHeader}>
          <IconKeyboard size={20} stroke={1.75} className={classes.cardIcon} />
          <Text className={classes.cardTitle}>{t('settings.hotkeys.lean.title')}</Text>
        </div>
        <Switch
          checked={vm.lean?.enabled ?? false}
          onChange={(event) => void vm.setLean(event.currentTarget.checked)}
          disabled={vm.leanBusy || vm.lean === null}
          label={t('settings.hotkeys.lean.switchLabel')}
          aria-label={t('settings.hotkeys.lean.switchLabel')}
        />
        <Text className={classes.cardHint}>{t('settings.hotkeys.lean.hint')}</Text>

        {vm.leanIssue && <IssueLine issue={vm.leanIssue} />}

        {vm.lean && (
          <div className={classes.leanGrid}>
            <LeanList
              title={t('settings.hotkeys.lean.visibleTitle')}
              tone="ok"
              keys={vm.lean.visibleRegions}
              kind="regions"
            />
            <LeanList
              title={t('settings.hotkeys.lean.hiddenRegionsTitle')}
              tone="muted"
              keys={vm.lean.hiddenRegions}
              kind="regions"
            />
            <LeanList
              title={t('settings.hotkeys.lean.hiddenControlsTitle')}
              tone="muted"
              keys={vm.lean.hiddenControls}
              kind="controls"
            />
          </div>
        )}
      </div>
    </div>
  );
};

type T = ReturnType<typeof useT>;
type Vm = ReturnType<typeof useHotkeys>;

// —— 单行动作 ——
interface HotkeyRowProps {
  action: ActionInfo;
  disabled: boolean;
  vm: Vm;
}

const HotkeyRow = ({ action, disabled, vm }: HotkeyRowProps) => {
  const t = useT();
  const binding = vm.bindingOf(action.id);
  const issue = vm.issueOf(action.id);
  const capturing = vm.capturingId === action.id;
  const busy = vm.busyId === action.id;

  const enabled = binding?.enabled ?? false;
  const accelerator = binding?.accelerator ?? '';
  const registerErr = binding?.registerErr ?? '';

  const label = t(action.labelKey) === action.labelKey ? action.id : t(action.labelKey);
  const hasCustom = enabled && accelerator !== '' && accelerator !== action.default;

  return (
    <div className={classes.row} data-error={(issue || registerErr) ? true : undefined}>
      <div className={classes.rowMain}>
        <div className={classes.rowTitleLine}>
          <Text className={classes.rowLabel}>{label}</Text>
          <span className={classes.scopeTag} data-scope={action.scope}>
            {action.scope === 'global'
              ? t('settings.hotkeys.scope.global')
              : t('settings.hotkeys.scope.app')}
          </span>
          {!action.hasHandler && (
            <span className={classes.scopeTag} data-scope="unwired">
              {t('settings.hotkeys.row.unwired')}
            </span>
          )}
        </div>

        <div className={classes.rowValues}>
          {capturing ? (
            <CaptureField
              preview={vm.capturePreview}
              onPreview={vm.setCapturePreview}
              onCancel={vm.cancelCapture}
              onUnsupported={vm.reportUnsupportedKey}
              onSubmit={(acceleratorText) => void vm.submitCapture(action.id, acceleratorText)}
            />
          ) : (
            <>
              <span className={classes.accel} data-disabled={!enabled || undefined}>
                {enabled && accelerator
                  ? accelerator
                  : enabled
                    ? t('settings.hotkeys.row.unbound')
                    : t('settings.hotkeys.row.disabled')}
              </span>
              <span className={classes.defaultHint}>
                {action.default
                  ? t('settings.hotkeys.row.defaultValue', { accel: action.default })
                  : t('settings.hotkeys.row.noDefault')}
              </span>
            </>
          )}
        </div>

        {/* 注册失败（被系统或别的程序占用）：必须标红给原因 */}
        {registerErr && (
          <Text className={classes.rowError}>
            {t('settings.hotkeys.row.registerFailed', { reason: registerErr })}
          </Text>
        )}
        {/* 冲突 / 注册 / 格式错误：录入中也照常展示，别把原因藏起来 */}
        {issue && <IssueLine issue={issue} />}
      </div>

      <div className={classes.rowActions}>
        {capturing ? (
          <Button
            variant="default"
            size="xs"
            leftSection={<IconX size={16} stroke={1.75} />}
            onClick={vm.cancelCapture}
          >
            {t('settings.hotkeys.row.cancel')}
          </Button>
        ) : (
          <Button
            variant="default"
            size="xs"
            leftSection={<IconKeyboard size={16} stroke={1.75} />}
            disabled={disabled || busy}
            onClick={() => vm.beginCapture(action.id)}
          >
            {t('settings.hotkeys.row.capture')}
          </Button>
        )}
        <Button
          variant="default"
          size="xs"
          disabled={disabled || busy || !enabled}
          onClick={() => void vm.disableAction(action.id)}
        >
          {t('settings.hotkeys.row.disable')}
        </Button>
        <Button
          variant="default"
          size="xs"
          disabled={disabled || busy || (!hasCustom && enabled && registerErr === '')}
          onClick={() => void vm.resetAction(action.id)}
        >
          {t('settings.hotkeys.row.reset')}
        </Button>
      </div>
    </div>
  );
};

// —— 组合键录入控件：按下即录制（不让用户手打字符串） ——
interface CaptureFieldProps {
  preview: string;
  onPreview: (text: string) => void;
  onCancel: () => void;
  onUnsupported: (key: string) => void;
  onSubmit: (accelerator: string) => void;
}

const CaptureField = ({ preview, onPreview, onCancel, onUnsupported, onSubmit }: CaptureFieldProps) => {
  const t = useT();
  const ref = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    ref.current?.focus();
  }, []);

  const handleKeyDown = useCallback(
    (event: KeyboardEvent<HTMLDivElement>) => {
      // 录入期间不让按键冒泡到全局快捷键（避免一边录一边触发动作）
      event.preventDefault();
      event.stopPropagation();
      const outcome = captureFromEvent(event.nativeEvent);
      switch (outcome.kind) {
        case 'waiting':
          onPreview(modifierPreview(event.nativeEvent));
          return;
        case 'cancel':
          onCancel();
          return;
        case 'unsupported':
          onUnsupported(outcome.key);
          return;
        case 'ready':
          onSubmit(outcome.accelerator);
          return;
      }
    },
    [onCancel, onPreview, onSubmit, onUnsupported],
  );

  return (
    <div
      ref={ref}
      className={classes.captureField}
      role="button"
      tabIndex={0}
      aria-label={t('settings.hotkeys.capture.aria')}
      onKeyDown={handleKeyDown}
      onBlur={onCancel}
    >
      <Text className={classes.captureText}>
        {preview || t('settings.hotkeys.capture.placeholder')}
      </Text>
      <Text className={classes.captureHint}>{t('settings.hotkeys.capture.cancelHint')}</Text>
    </div>
  );
};

// —— 冲突 / 注册 / 格式错误提示 ——
interface IssueLineProps {
  issue: HotkeyIssue;
}

const IssueLine = ({ issue }: IssueLineProps) => {
  const t = useT();
  return (
    <div className={classes.issueLine} role="alert">
      <IconAlertTriangle size={16} stroke={1.75} />
      <div className={classes.issueTexts}>
        <Text className={classes.issueText}>{issueText(issue, t)}</Text>
        {issue.kind === 'conflict' && issue.preview && (
          <Text className={classes.issueMeta}>
            {t('settings.hotkeys.issue.preview', { accel: issue.preview })}
          </Text>
        )}
        {issue.conflicts.map((conflict, index) => (
          <Text className={classes.issueMeta} key={`${conflict.action}-${index}`}>
            {conflictText(conflict, t)}
          </Text>
        ))}
      </div>
    </div>
  );
};

const issueText = (issue: HotkeyIssue, t: T): string => {
  switch (issue.kind) {
    case 'register':
      return t('settings.hotkeys.issue.register');
    case 'conflict':
      return t('settings.hotkeys.issue.conflict');
    case 'invalid':
      return t('settings.hotkeys.issue.invalidKey', { key: issue.message });
    case 'failed': {
      const key = issue.code ? `settings.hotkeys.errors.${issue.code}` : '';
      const translated = key ? t(key) : '';
      return key && translated !== key ? translated : issue.message;
    }
  }
};

// 冲突原因（system 系统保留 / duplicate 与其它动作重复）+ 涉及的动作名。
const conflictText = (conflict: HotkeyConflict, t: T): string => {
  const reason =
    conflict.reason === 'system'
      ? t('settings.hotkeys.issue.reasonSystem')
      : conflict.reason === 'duplicate'
        ? t('settings.hotkeys.issue.reasonDuplicate')
        : conflict.reason;
  // 动作名统一走后端约定的 i18n 键（hotkeys.actions.<id>），查不到就显示动作 id。
  const actionKey = conflict.action ? `hotkeys.actions.${conflict.action}` : '';
  const translated = actionKey ? t(actionKey) : '';
  const action = actionKey && translated !== actionKey ? translated : conflict.action;
  return action
    ? t('settings.hotkeys.issue.conflictItem', { reason, action })
    : t('settings.hotkeys.issue.conflictItemNoAction', { reason });
};

// —— 精简模式清单 ——
interface LeanListProps {
  title: string;
  tone: 'ok' | 'muted';
  keys: string[];
  kind: 'regions' | 'controls';
}

const LeanList = ({ title, tone, keys, kind }: LeanListProps) => {
  const t = useT();
  return (
    <div className={classes.leanColumn} data-tone={tone}>
      <Text className={classes.leanTitle}>{title}</Text>
      {keys.length === 0 ? (
        <Text className={classes.cardHint}>—</Text>
      ) : (
        keys.map((raw) => (
          <Text className={classes.leanItem} key={`${kind}-${raw}`}>
            {leanLabel(kind, raw, t)}
          </Text>
        ))
      )}
    </div>
  );
};

// 后端清单是稳定英文键（status / settings.advanced / library.bulkActions）；
// i18n 键把点号压成驼峰，查不到就原样显示（保证清单永远渲染得出来）。
const leanLabel = (kind: 'regions' | 'controls', raw: string, t: T): string => {
  const camel = raw.replace(/\.([a-z])/g, (_, ch: string) => ch.toUpperCase());
  const key = `settings.hotkeys.lean.${kind}.${camel}`;
  const translated = t(key);
  return translated === key ? raw : translated;
};

const groupLabel = (group: string, t: T): string => {
  const key = `settings.hotkeys.groups.${group}`;
  const translated = t(key);
  return translated === key ? group : translated;
};
