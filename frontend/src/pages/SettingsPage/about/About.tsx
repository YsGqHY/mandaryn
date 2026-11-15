import { Button, Text } from '@mantine/core';
import {
  IconAlertTriangle,
  IconCheck,
  IconDownload,
  IconInfoCircle,
  IconRefresh,
  IconShieldLock,
} from '@tabler/icons-react';
import { useT } from '@/i18n';
import { isOfflineBlocked, useAbout, type UpdateStatus } from './useAbout';
import classes from './About.module.css';
import shared from '../SettingsPage.module.css';

// 「关于与更新」子页面（API-11.1 / 11.2）：
//   1) 当前版本号（不联网，离线也可见）
//   2) 检查更新按钮 → 只回答"有没有新版本"，**不下载不安装**
//   3) 四种结果各自明确展示：已最新 / 有新版本（含打开发布页）/ 离线拦截 / 其它失败
//   4) 说明为什么不做静默自更新（避免用户以为"检查了就会自动装"）
//
// 文案 namespace：settings.about.*
export const About = () => {
  const vm = useAbout();
  const t = useT();
  const checking = vm.status.kind === 'checking';

  return (
    <div className={shared.section}>
      <div className={shared.sectionHeader}>
        <Text className={shared.sectionTitle}>{t('settings.about.title')}</Text>
        <Text className={shared.sectionHint}>{t('settings.about.hint')}</Text>
      </div>

      {/* —— 版本 —— */}
      <div className={classes.card}>
        <div className={classes.row}>
          <div className={classes.rowTexts}>
            <Text className={classes.label}>{t('settings.about.version.label')}</Text>
            <Text className={classes.value} data-testid="about-version">
              {vm.versionLoading ? t('settings.about.version.loading') : vm.version || t('settings.about.version.unknown')}
            </Text>
          </div>
          <Button
            variant="filled"
            size="xs"
            leftSection={<IconRefresh size={16} stroke={1.75} />}
            loading={checking}
            onClick={() => void vm.check()}
          >
            {t('settings.about.check.button')}
          </Button>
        </div>

        <ResultLine status={vm.status} onOpen={() => void vm.openDownload()} />

        <Text className={shared.sectionHint}>{t('settings.about.check.manualNote')}</Text>
      </div>

      {/* —— 为什么不做静默自更新 —— */}
      <div className={classes.card}>
        <div className={classes.cardHeader}>
          <IconShieldLock size={18} stroke={1.75} className={classes.cardIcon} />
          <Text className={classes.cardTitle}>{t('settings.about.policy.title')}</Text>
        </div>
        <Text className={shared.sectionHint}>{t('settings.about.policy.body')}</Text>
      </div>
    </div>
  );
};

type T = ReturnType<typeof useT>;

// 把四种结果映射成一行反馈。文案全部走 i18n，错误码命中不到时回退到后端原文。
const ResultLine = ({ status, onOpen }: { status: UpdateStatus; onOpen: () => void }) => {
  const t = useT();
  if (status.kind === 'idle' || status.kind === 'checking') return null;

  if (status.kind === 'latest') {
    return (
      <div className={classes.result} data-tone="ok" data-testid="about-result-latest">
        <IconCheck size={16} stroke={2} />
        <Text className={classes.resultText}>{t('settings.about.check.upToDate')}</Text>
      </div>
    );
  }

  if (status.kind === 'available') {
    return (
      <div className={classes.result} data-tone="accent" data-testid="about-result-available">
        <IconDownload size={16} stroke={1.75} />
        <div className={classes.resultBody}>
          <Text className={classes.resultText}>
            {t('settings.about.check.available', { version: status.info.latest })}
          </Text>
          <Button variant="light" size="compact-xs" onClick={onOpen} leftSection={<IconDownload size={14} stroke={1.75} />}>
            {t('settings.about.check.openDownload')}
          </Button>
        </div>
      </div>
    );
  }

  // 失败：离线拦截要说清"是你开了离线"，与网络故障区分开。
  const offline = isOfflineBlocked(status);
  return (
    <div className={classes.result} data-tone="warn" data-testid="about-result-failed">
      {offline ? <IconInfoCircle size={16} stroke={1.75} /> : <IconAlertTriangle size={16} stroke={1.75} />}
      <Text className={classes.resultText}>{failureText(status.code, status.message, offline, t)}</Text>
    </div>
  );
};

const failureText = (code: string, message: string, offline: boolean, t: T): string => {
  if (offline) return t('settings.about.check.offline');
  const key = `settings.about.check.errors.${code}`;
  const translated = code !== '' ? t(key) : key;
  if (translated !== key) return translated;
  return t('settings.about.check.failed', { message: message !== '' ? message : code });
};
