import { createContext, useCallback, useContext, useEffect, useRef, useState, type ReactNode } from 'react';
import { HotkeysService, type LeanState } from '@/services/hotkeys/HotkeysService';

interface LeanModeContextValue {
  state: LeanState | null;
  loadFailed: boolean;
  refresh: () => Promise<void>;
  setEnabled: (enabled: boolean) => Promise<void>;
  isRegionHidden: (key: string) => boolean;
  isControlHidden: (key: string) => boolean;
}

const LeanModeContext = createContext<LeanModeContextValue | null>(null);

export const LeanModeProvider = ({ children }: { children: ReactNode }) => {
  const [state, setState] = useState<LeanState | null>(null);
  const [loadFailed, setLoadFailed] = useState(false);
  const revision = useRef(0);

  const refresh = useCallback(async () => {
    const request = ++revision.current;
    try {
      const next = await HotkeysService.getLeanState();
      if (request !== revision.current) return;
      setState(next);
      setLoadFailed(false);
    } catch {
      if (request === revision.current) setLoadFailed(true);
    }
  }, []);

  useEffect(() => {
    // 先订阅再读；任何较旧的快照响应都不能覆盖最近发出的读取。
    const off = HotkeysService.onLeanChanged(() => void refresh());
    void refresh();
    return () => {
      ++revision.current;
      off();
    };
  }, [refresh]);

  const setEnabled = useCallback(async (enabled: boolean) => {
    // 本地调用不等事件往返；若已收到更新的事件，则以后发出的读取为准。
    const request = ++revision.current;
    const next = await HotkeysService.setLeanMode(enabled);
    if (request === revision.current) {
      setState(next);
      setLoadFailed(false);
    }
  }, []);

  return (
    <LeanModeContext.Provider value={{
      state,
      loadFailed,
      refresh,
      setEnabled,
      isRegionHidden: (key) => state?.enabled === true && state.hiddenRegions.includes(key),
      isControlHidden: (key) => state?.enabled === true && state.hiddenControls.includes(key),
    }}>
      {children}
    </LeanModeContext.Provider>
  );
};

export const useLeanMode = (): LeanModeContextValue => {
  const value = useContext(LeanModeContext);
  if (value === null) throw new Error('useLeanMode requires LeanModeProvider');
  return value;
};
