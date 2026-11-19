import { Button, Checkbox, Group, NativeSelect, Stack, Switch, Text, TextInput, Title } from '@mantine/core';
import { useT } from '@/i18n';
import shared from '../SettingsPage.module.css';
import classes from './Transcription.module.css';
import { languageOptions, useTranscription } from './useTranscription';

export function Transcription() {
  const vm = useTranscription();
  const t = useT();
  const copy = (key: string) => t(`settings.transcription.${key}`);

  if (vm.loadFailed) {
    return (
      <Stack gap="sm" align="flex-start">
        <Text role="alert" className={classes.notice} data-error>{copy('loadFailed')}</Text>
        <Button type="button" variant="default" onClick={vm.retryLoad}>{copy('retry')}</Button>
      </Stack>
    );
  }
  if (!vm.loaded || !vm.policy) return <Text role="status" className={shared.sectionHint}>{copy('loading')}</Text>;

  return (
    <>
      <section className={shared.section} aria-labelledby="online-settings-title">
        <Title order={2} className={shared.sectionTitle} id="online-settings-title">{copy('onlineTitle')}</Title>
        <Text className={shared.sectionHint}>{copy('onlineHint')}</Text>
        <form className={classes.form} onSubmit={(event) => { event.preventDefault(); void vm.saveConfig(); }} noValidate>
          <TextInput label={copy('endpoint')} description={copy('endpointHint')} type="url"
            value={vm.endpoint} onChange={(event) => vm.changeEndpoint(event.currentTarget.value)}
            autoComplete="off" spellCheck={false} disabled={vm.saving}
            placeholder={copy('endpointPlaceholder')} error={vm.endpointError ? copy('endpointError') : undefined} />
          <TextInput label={copy('model')} description={copy('modelHint')} value={vm.model}
            onChange={(event) => vm.setModel(event.currentTarget.value)} disabled={vm.saving} spellCheck={false} />
          <NativeSelect label={copy('language')} value={vm.language} disabled={vm.saving}
            onChange={(event) => vm.setLanguage(event.currentTarget.value)}
            data={languageOptions.map((value) => ({ value, label: copy(`languages.${value}`) }))} />
          {vm.language === 'other' && (
            <TextInput label={copy('customLanguage')} placeholder={copy('customLanguagePlaceholder')}
              value={vm.customLanguage} onChange={(event) => vm.setCustomLanguage(event.currentTarget.value)}
              disabled={vm.saving} spellCheck={false} error={vm.languageError ? copy('languageError') : undefined} />
          )}
          <TextInput label={copy('apiKey')} description={copy('apiKeyHint')} type="password"
            value={vm.apiKey} placeholder={vm.maskedKey || undefined} autoComplete="new-password"
            spellCheck={false} disabled={vm.saving} onChange={(event) => vm.setApiKey(event.currentTarget.value)} />
          {vm.needsConsent && (
            <div className={classes.consent} aria-labelledby="online-consent-title" role="group">
              <Title order={3} id="online-consent-title" className={classes.consentTitle}>{copy('consentTitle')}</Title>
              <Text className={classes.endpoint}>{vm.normalizedEndpoint}</Text>
              <Text className={shared.sectionHint}>{copy('consentHint')}</Text>
              <Checkbox label={copy('consentLabel')} checked={vm.consentChecked} disabled={vm.saving}
                onChange={(event) => vm.setConsentChecked(event.currentTarget.checked)} />
            </div>
          )}
          <Group gap="sm">
            <Button type="submit" disabled={!vm.canSave} loading={vm.saving}>{copy('save')}</Button>
            {vm.notice && <Text role={vm.notice.error ? 'alert' : 'status'} className={classes.notice} data-error={vm.notice.error || undefined}>{t(vm.notice.key)}</Text>}
          </Group>
        </form>
      </section>
      <section className={shared.section} aria-labelledby="fallback-policy-title">
        <Title order={2} className={shared.sectionTitle} id="fallback-policy-title">{copy('policyTitle')}</Title>
        <Stack gap="md" className={classes.form}>
          <Switch label={copy('fallbackToLocal')} description={copy('fallbackToLocalHint')}
            checked={vm.policy.fallbackToLocal} disabled={vm.updatingPolicy}
            onChange={(event) => { void vm.savePolicy('fallbackToLocal', event.currentTarget.checked); }} />
          <Switch label={copy('autoSwitchBack')} description={copy('autoSwitchBackHint')}
            checked={vm.policy.autoSwitchBack} disabled={vm.updatingPolicy}
            onChange={(event) => { void vm.savePolicy('autoSwitchBack', event.currentTarget.checked); }} />
          {vm.policyNotice && <Text role={vm.policyNotice.error ? 'alert' : 'status'} className={classes.notice} data-error={vm.policyNotice.error || undefined}>{t(vm.policyNotice.key)}</Text>}
        </Stack>
      </section>
    </>
  );
}
