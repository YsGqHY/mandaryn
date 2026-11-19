import { useEffect, useState } from 'react';
import { AppSettingsService } from '@/services';
import { SystrayService } from '@/services/systray';

export function useDesktopPreferences() {
  const [trayEnabled, setTrayEnabled] = useState<boolean | null>(null);
  const [trayAction, setTrayAction] = useState<'panel' | 'window' | null>(null);
  const [cues, setCues] = useState<boolean | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState(false);

  useEffect(() => {
    let canceled = false;
    void SystrayService.isEnabled().then((value) => { if (!canceled) setTrayEnabled(value); })
      .catch(() => { if (!canceled) setError(true); });
    void SystrayService.getClickAction().then((value) => { if (!canceled) setTrayAction(value === 'window' ? 'window' : 'panel'); })
      .catch(() => { if (!canceled) setError(true); });
    const loadCues = () => { void AppSettingsService.get().then((value) => { if (!canceled) setCues(value.recordingCues); })
      .catch(() => { if (!canceled) setError(true); }); };
    loadCues();
    const off = AppSettingsService.onChanged((field) => { if (field === 'recordingCues') loadCues(); });
    return () => { canceled = true; off(); };
  }, []);

  const update = async (save: () => Promise<void>) => {
    if (busy) return;
    setBusy(true);
    setError(false);
    try { await save(); }
    catch { setError(true); }
    finally { setBusy(false); }
  };

  return {
    trayEnabled, trayAction, cues, busy, error,
    changeTrayEnabled: (value: boolean) => update(async () => {
      if (value) await SystrayService.enable();
      else await SystrayService.disable();
      setTrayEnabled(await SystrayService.isEnabled());
    }),
    changeTrayAction: (value: string | null) => {
      if (value !== 'panel' && value !== 'window') return;
      void update(async () => { await SystrayService.setClickAction(value); setTrayAction(value); });
    },
    changeCues: (value: boolean) => update(async () => {
      await AppSettingsService.setRecordingCues(value);
      setCues(value);
    }),
  };
}
