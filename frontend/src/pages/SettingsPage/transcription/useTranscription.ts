import { useEffect, useRef, useState } from 'react';
import { EngineService, type FallbackPolicy, type OnlineConfig } from '@/services/engine/EngineService';
import { hasCode } from '@/services/errors/AppError';

const keys = 'settings.transcription';
export const languageOptions = ['auto', 'zh', 'en', 'other'] as const;
const knownLanguage = (language: string) => languageOptions.slice(0, -1).some((item) => item === language);

type PolicyField = 'fallbackToLocal' | 'autoSwitchBack';
type Notice = { key: string; error?: boolean };

function parseEndpoint(value: string): URL | null {
  if (!/^https:\/\/[^/?#][^\s\\]*$/i.test(value) || /[?#]/.test(value)) return null;
  try {
    const url = new URL(value);
    const authority = value.slice('https://'.length).split('/')[0];
    return url.hostname && !authority?.includes('@') ? url : null;
  } catch {
    return null;
  }
}

export function useTranscription() {
  const [loadAttempt, setLoadAttempt] = useState(0);
  const [loadFailed, setLoadFailed] = useState(false);
  const [saved, setSaved] = useState<OnlineConfig | null>(null);
  const [policy, setPolicy] = useState<FallbackPolicy | null>(null);
  const [authorizedEndpoint, setAuthorizedEndpoint] = useState('');
  const [endpoint, setEndpoint] = useState('');
  const [model, setModel] = useState('');
  const [language, setLanguage] = useState('auto');
  const [customLanguage, setCustomLanguage] = useState('');
  const [apiKey, setApiKey] = useState('');
  const [consentChecked, setConsentChecked] = useState(false);
  const [submitted, setSubmitted] = useState(false);
  const [saving, setSaving] = useState(false);
  const [updatingPolicy, setUpdatingPolicy] = useState(false);
  const [notice, setNotice] = useState<Notice | null>(null);
  const [policyNotice, setPolicyNotice] = useState<Notice | null>(null);
  const savingRef = useRef(false);
  const policyRef = useRef(false);

  useEffect(() => {
    let current = true;
    void Promise.all([EngineService.getOnlineConfig(), EngineService.getOnlineSettings()]).then(
      ([config, settings]) => {
        if (!current) return;
        const lang = config.language || 'auto';
        setSaved({ ...config, language: lang });
        setPolicy(settings.policy);
        setEndpoint(config.endpoint);
        setModel(config.model);
        const selected = parseEndpoint(config.endpoint);
        const consent = parseEndpoint(`https://${settings.consentHost}`);
        if (settings.consentAt > 0 && selected && selected.host === consent?.host) {
          setAuthorizedEndpoint(config.endpoint);
        }
        setLanguage(knownLanguage(lang) ? lang : 'other');
        setCustomLanguage(knownLanguage(lang) ? '' : lang);
      },
      () => { if (current) setLoadFailed(true); },
    );
    return () => { current = false; };
  }, [loadAttempt]);

  const normalizedEndpoint = endpoint.trim();
  const endpointValid = parseEndpoint(normalizedEndpoint) !== null;
  const effectiveLanguage = language === 'other' ? customLanguage.trim() : language;
  const needsConsent = endpointValid && normalizedEndpoint !== authorizedEndpoint;
  const dirty = saved !== null && (normalizedEndpoint !== saved.endpoint || model.trim() !== saved.model
    || effectiveLanguage !== saved.language || apiKey !== '');

  function changeEndpoint(value: string) {
    setEndpoint(value);
    setConsentChecked(false);
    setNotice(null);
  }

  async function saveConfig() {
    if (!saved || savingRef.current || (!dirty && !needsConsent)) return;
    setSubmitted(true);
    setNotice(null);
    if (!endpointValid || !effectiveLanguage || (needsConsent && !consentChecked)) return;
    const config: OnlineConfig = {
      endpoint: normalizedEndpoint,
      model: model.trim(),
      language: effectiveLanguage,
      apiKey,
      timeoutMs: saved.timeoutMs,
    };
    savingRef.current = true;
    setSaving(true);
    try {
      if (needsConsent) await EngineService.setConsentOnline(config.endpoint);
      await EngineService.setOnlineConfig(config);
      setApiKey('');
      setConsentChecked(false);
      setAuthorizedEndpoint(config.endpoint);
      setSubmitted(false);
      // Never reflect the replacement key, even if the follow-up read fails.
      setSaved({ ...config, apiKey: config.apiKey ? '***' : saved.apiKey });
      setNotice({ key: `${keys}.saved` });
      if (config.apiKey) {
        try {
          const refreshed = await EngineService.getOnlineConfig();
          setSaved((previous) => previous && { ...previous, apiKey: refreshed.apiKey });
        } catch { /* A generic mask is safer than retaining the old key's suffix. */ }
      }
    } catch (error) {
      if (hasCode(error, 'ERR_SETTINGS_CONSENT_REQUIRED')) {
        setAuthorizedEndpoint('');
        setConsentChecked(false);
        setNotice({ key: `${keys}.consentRequired`, error: true });
      } else {
        setNotice({ key: `${keys}.saveFailed`, error: true });
      }
    } finally {
      savingRef.current = false;
      setSaving(false);
    }
  }

  async function savePolicy(field: PolicyField, value: boolean) {
    if (!policy || policyRef.current) return;
    const next = { ...policy, [field]: value };
    policyRef.current = true;
    setUpdatingPolicy(true);
    setPolicyNotice(null);
    try {
      await EngineService.setFallbackPolicy(next);
      setPolicy(next);
      setPolicyNotice({ key: `${keys}.policySaved` });
    } catch {
      setPolicyNotice({ key: `${keys}.policySaveFailed`, error: true });
    } finally {
      policyRef.current = false;
      setUpdatingPolicy(false);
    }
  }

  return {
    loaded: saved !== null && policy !== null,
    loadFailed,
    retryLoad: () => { setLoadFailed(false); setLoadAttempt((n) => n + 1); },
    endpoint, changeEndpoint, model, setModel, language, setLanguage,
    customLanguage, setCustomLanguage, apiKey, setApiKey, maskedKey: saved?.apiKey ?? '',
    consentChecked, setConsentChecked, needsConsent, normalizedEndpoint,
    endpointError: (submitted || normalizedEndpoint !== '') && !endpointValid,
    languageError: submitted && !effectiveLanguage,
    saving, saveConfig, canSave: (dirty || needsConsent) && (!needsConsent || consentChecked),
    notice, policy, updatingPolicy, savePolicy, policyNotice,
  };
}
