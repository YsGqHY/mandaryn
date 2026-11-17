import {
  Alert,
  Button,
  Group,
  NumberInput,
  PasswordInput,
  SegmentedControl,
  Switch,
  Text,
  TextInput,
} from '@mantine/core';
import {
  IconAlertTriangle,
  IconCircleCheck,
  IconDeviceFloppy,
  IconEraser,
  IconInfoCircle,
  IconPlugConnected,
  IconRoute,
} from '@tabler/icons-react';
import { useT } from '@/i18n';
import { useProxy, type ProxyFeedback } from './useProxy';
import classes from './Proxy.module.css';
import shared from '../SettingsPage.module.css';

// 代理子页面（FR-05-5 / API-9.3 / API-9.4）。
//
// 界面要说清三件事，否则用户会困惑「配了为什么不生效」：
//   1. 代理分成下载与在线转写两条链路，可分别开关；
//   2. 密码**不回显**（显示掩码），不改密码只改其它字段是安全的；
//   3. 环境变量 MANDARYN_HTTP_PROXY 优先级**高于**设置页，被接管时设置页不生效。
export const Proxy = () => {
  const vm = useProxy();
  const t = useT();

  const busy = vm.feedback.kind === 'saving' || vm.feedback.kind === 'testing';
  const disabled = vm.loading || busy;
  const isNone = vm.form.type === 'none';

  // 测试结果按 Category 映射文案（稳定取值 → i18n 键）；
  // 未命中的分类回落成后端给的脱敏说明，而不是显示英文键。
  const testCategoryText = (category: string): string => {
    const key = `settings.proxy.test.category.${category}`;
    const translated = t(key);
    return translated !== key ? translated : '';
  };

  return (
    <div className={shared.section}>
      <div className={shared.sectionHeader}>
        <Text className={shared.sectionTitle}>{t('settings.proxy.title')}</Text>
        <Text className={shared.sectionHint}>{t('settings.proxy.hint')}</Text>
      </div>

      {/* —— 环境变量接管提示：优先级说清，且明确「设置页当前不生效」 —— */}
      {vm.envOverride !== '' && (
        <Alert
          className={classes.envAlert}
          radius="md"
          icon={<IconInfoCircle size={20} stroke={1.75} />}
          title={t('settings.proxy.env.title', { name: vm.envOverride })}
        >
          <Text className={classes.envText}>
            {t('settings.proxy.env.body', { name: vm.envOverride })}
          </Text>
        </Alert>
      )}

      <div className={classes.card}>
        <div className={classes.headRow}>
          <IconRoute size={20} stroke={1.75} className={classes.headIcon} />
          <div className={classes.headTexts}>
            <Text className={classes.headTitle}>{t('settings.proxy.form.title')}</Text>
            <Text className={classes.headDesc}>{t('settings.proxy.form.desc')}</Text>
          </div>
        </div>

        {/* —— 代理类型 —— */}
        <div className={classes.field}>
          <Text className={classes.fieldLabel}>{t('settings.proxy.form.typeLabel')}</Text>
          <SegmentedControl
            className={classes.typeSwitch}
            value={vm.form.type}
            onChange={(value) => vm.setField('type', value as typeof vm.form.type)}
            disabled={disabled}
            data={[
              { label: t('settings.proxy.form.type.none'), value: 'none' },
              { label: t('settings.proxy.form.type.http'), value: 'http' },
              { label: t('settings.proxy.form.type.https'), value: 'https' },
              { label: t('settings.proxy.form.type.socks5'), value: 'socks5' },
            ]}
          />
          <Text className={classes.fieldHint}>{t('settings.proxy.form.typeHint')}</Text>
        </div>

        {/* —— 地址 / 端口 —— */}
        <div className={classes.row}>
          <TextInput
            className={classes.hostInput}
            label={t('settings.proxy.form.hostLabel')}
            placeholder={t('settings.proxy.form.hostPlaceholder')}
            value={vm.form.host}
            onChange={(event) => vm.setField('host', event.currentTarget.value)}
            disabled={disabled || isNone}
            autoComplete="off"
            spellCheck={false}
          />
          <NumberInput
            className={classes.portInput}
            label={t('settings.proxy.form.portLabel')}
            placeholder={t('settings.proxy.form.portPlaceholder')}
            value={vm.form.port === '' ? '' : Number(vm.form.port)}
            onChange={(value) => vm.setField('port', value === '' ? '' : String(value))}
            disabled={disabled || isNone}
            min={1}
            max={65535}
            allowDecimal={false}
            allowNegative={false}
          />
        </div>

        {/* —— 用户名 / 密码 —— */}
        <div className={classes.row}>
          <TextInput
            className={classes.hostInput}
            label={t('settings.proxy.form.usernameLabel')}
            placeholder={t('settings.proxy.form.usernamePlaceholder')}
            value={vm.form.username}
            onChange={(event) => vm.setField('username', event.currentTarget.value)}
            disabled={disabled || isNone}
            autoComplete="off"
            spellCheck={false}
          />
          <PasswordInput
            className={classes.portInput}
            label={t('settings.proxy.form.passwordLabel')}
            placeholder={t('settings.proxy.form.passwordPlaceholder')}
            value={vm.form.password}
            onChange={(event) => vm.setField('password', event.currentTarget.value)}
            disabled={disabled || isNone}
            autoComplete="new-password"
          />
        </div>
        <Text className={classes.fieldHint}>{t('settings.proxy.form.passwordHint')}</Text>

        {/* —— 直连例外 —— */}
        <div className={classes.field}>
          <TextInput
            label={t('settings.proxy.form.noProxyLabel')}
            placeholder={t('settings.proxy.form.noProxyPlaceholder')}
            value={vm.form.noProxy}
            onChange={(event) => vm.setField('noProxy', event.currentTarget.value)}
            disabled={disabled || isNone}
            autoComplete="off"
            spellCheck={false}
          />
          <Text className={classes.fieldHint}>{t('settings.proxy.form.noProxyHint')}</Text>
        </div>

        {/* —— 两条链路分别开关 —— */}
        <div className={classes.switchRow}>
          <Switch
            checked={vm.form.useForDownload}
            onChange={(event) => vm.setField('useForDownload', event.currentTarget.checked)}
            disabled={disabled || isNone}
            label={t('settings.proxy.form.useForDownload')}
            aria-label={t('settings.proxy.form.useForDownload')}
          />
          <Switch
            checked={vm.form.useForOnline}
            onChange={(event) => vm.setField('useForOnline', event.currentTarget.checked)}
            disabled={disabled || isNone}
            label={t('settings.proxy.form.useForOnline')}
            aria-label={t('settings.proxy.form.useForOnline')}
          />
        </div>

        {/* —— 动作：保存 / 清空 / 测试连通性 —— */}
        <Group className={classes.actions} gap="sm">
          <Button
            leftSection={<IconDeviceFloppy size={18} stroke={1.75} />}
            onClick={() => void vm.save()}
            loading={vm.feedback.kind === 'saving'}
            disabled={vm.loading || vm.feedback.kind === 'testing'}
          >
            {t('settings.proxy.actions.save')}
          </Button>
          <Button
            variant="default"
            leftSection={<IconPlugConnected size={18} stroke={1.75} />}
            onClick={() => void vm.test()}
            loading={vm.feedback.kind === 'testing'}
            disabled={vm.loading || vm.feedback.kind === 'saving'}
          >
            {t('settings.proxy.actions.test')}
          </Button>
          <Button
            variant="subtle"
            color="red"
            leftSection={<IconEraser size={18} stroke={1.75} />}
            onClick={() => void vm.clear()}
            disabled={disabled}
          >
            {t('settings.proxy.actions.clear')}
          </Button>
          {vm.dirty && (
            <Text className={classes.dirtyHint}>{t('settings.proxy.actions.dirty')}</Text>
          )}
        </Group>

        {/* —— 反馈区 —— */}
        <Feedback feedback={vm.feedback} testCategoryText={testCategoryText} />
      </div>
    </div>
  );
};

// Feedback 渲染保存 / 测试的结果。
//
// 测试成功与失败用不同色调，失败按 Category 给**可操作**的建议
// （改密码 / 稍后重试 / 换 TLS 设置），而不是笼统一句「测试失败」。
const Feedback = ({
  feedback,
  testCategoryText,
}: {
  feedback: ProxyFeedback;
  testCategoryText: (category: string) => string;
}) => {
  const t = useT();

  if (feedback.kind === 'idle' || feedback.kind === 'saving' || feedback.kind === 'testing') {
    return null;
  }

  if (feedback.kind === 'saved' || feedback.kind === 'cleared') {
    return (
      <div className={classes.feedback} data-tone="success">
        <IconCircleCheck size={18} stroke={1.75} className={classes.feedbackIcon} />
        <Text className={classes.feedbackText}>
          {feedback.kind === 'saved'
            ? t('settings.proxy.feedback.saved')
            : t('settings.proxy.feedback.cleared')}
        </Text>
      </div>
    );
  }

  if (feedback.kind === 'tested') {
    const { result } = feedback;
    if (result.ok) {
      return (
        <div className={classes.feedback} data-tone="success">
          <IconCircleCheck size={18} stroke={1.75} className={classes.feedbackIcon} />
          <Text className={classes.feedbackText}>
            {t('settings.proxy.feedback.testOk', { latency: String(result.latencyMs) })}
          </Text>
        </div>
      );
    }
    const categoryText = testCategoryText(result.category);
    return (
      <div className={classes.feedback} data-tone="warning">
        <IconAlertTriangle size={18} stroke={1.75} className={classes.feedbackIcon} />
        <Text className={classes.feedbackText}>
          {/* 分类文案优先；没有对应键时退回后端的脱敏说明 */}
          {categoryText !== '' ? categoryText : t('settings.proxy.test.failedGeneric')}
          {result.note !== '' && (
            <Text component="span" className={classes.feedbackNote}>
              {` ${result.note}`}
            </Text>
          )}
        </Text>
      </div>
    );
  }

  // failed：可能是表单校验（host / port）或后端错误码。
  const message = (() => {
    if (feedback.code === 'host') return t('settings.proxy.errors.host');
    if (feedback.code === 'port') return t('settings.proxy.errors.port');
    const key = `settings.proxy.errors.${feedback.code}`;
    const translated = feedback.code !== '' ? t(key) : key;
    if (translated !== key) return translated;
    return t('settings.proxy.errors.generic', { message: feedback.message });
  })();

  return (
    <div className={classes.feedback} data-tone="danger">
      <IconAlertTriangle size={18} stroke={1.75} className={classes.feedbackIcon} />
      <Text className={classes.feedbackText}>{message}</Text>
    </div>
  );
};
