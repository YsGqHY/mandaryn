import { useEffect, useRef, useState } from 'react';
import { ModelsService, type ModelMirror } from '@/services/models/ModelsService';
import { toAppError } from '@/services';

type Status = 'idle' | 'saved' | 'invalid' | 'failed';

export function useModelMirror(onSaved: () => void) {
  const [config, setConfig] = useState<ModelMirror | null>(null);
  const [value, setValue] = useState('');
  const [status, setStatus] = useState<Status>('idle');
  const [busy, setBusy] = useState(true);
  const mounted = useRef(false);
  useEffect(() => {
    mounted.current = true;
    let canceled = false;
    void ModelsService.getModelMirror().then((result) => {
      if (!canceled) { setConfig(result); setValue(result.savedUrl); }
    }).catch(() => { if (!canceled) setStatus('failed'); })
      .finally(() => { if (!canceled) setBusy(false); });
    return () => { canceled = true; mounted.current = false; };
  }, []);

  const save = async () => {
    if (busy || config?.managedBy) return;
    setBusy(true);
    try {
      const result = await ModelsService.setModelMirror(value);
      if (!mounted.current) return;
      setConfig(result);
      setValue(result.savedUrl);
      setStatus('saved');
      onSaved();
    } catch (err) {
      if (mounted.current) setStatus(toAppError(err).code === 'ERR_INVALID_ARGUMENT' ? 'invalid' : 'failed');
    } finally { if (mounted.current) setBusy(false); }
  };
  return { value, setValue, status, busy, save, managedBy: config?.managedBy ?? '' };
}
