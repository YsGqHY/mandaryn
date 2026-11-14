import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import {
  HOTKEY_GROUP_ORDER,
  HotkeysService,
  toAppError,
  type ActionInfo,
  type Binding,
  type ConflictReport,
  type HotkeyConflict,
  type LeanState,
} from '@/services';

// 单行的异常状态（视图据此标红并给原因，绝不静默）。
export type HotkeyIssueKind = 'register' | 'conflict' | 'invalid' | 'failed';

export interface HotkeyIssue {
  kind: HotkeyIssueKind;
  // 后端错误码（ERR_HOTKEY_REGISTER / ERR_HOTKEY_CONFLICT / ERR_INVALID_ARGUMENT …）
  code: string;
  // 后端原始消息（已脱敏，只作兜底）
  message: string;
  // 冲突明细（仅 kind === 'conflict' 时有）
  conflicts: HotkeyConflict[];
  // 规范化预览（后端 ConflictReport.preview）
  preview: string;
}

export interface HotkeyGroup {
  group: string;
  actions: ActionInfo[];
}

export interface UseHotkeysResult {
  // available=null 表示还在探测平台能力
  available: boolean | null;
  loading: boolean;
  groups: HotkeyGroup[];
  // 当前绑定（按 action id 索引；缺失表示后端没回该动作）
  bindingOf: (actionId: string) => Binding | null;
  // 正在录入的行（null = 没有）
  capturingId: string | null;
  capturePreview: string;
  // 行级异常（注册失败 / 冲突 / 格式非法）
  issueOf: (actionId: string) => HotkeyIssue | null;
  // 正在提交的行（禁用其按钮，避免重复点）
  busyId: string | null;
  beginCapture: (actionId: string) => void;
  cancelCapture: () => void;
  // 由视图把「按下组合键」的结果交给 VM（输入的组装是纯前端逻辑）
  submitCapture: (actionId: string, accelerator: string) => Promise<void>;
  setCapturePreview: (text: string) => void;
  reportUnsupportedKey: (key: string) => void;
  disableAction: (actionId: string) => Promise<void>;
  resetAction: (actionId: string) => Promise<void>;
  resetAllActions: () => Promise<void>;
  resetAllBusy: boolean;
  lean: LeanState | null;
  leanBusy: boolean;
  setLean: (enabled: boolean) => Promise<void>;
  // 列表级 / 精简模式级错误（不属于某个动作行）
  listIssue: HotkeyIssue | null;
  leanIssue: HotkeyIssue | null;
  refresh: () => Promise<void>;
}

// 快捷键子页面 ViewModel（FR-09）：
//   - mount 并行拉 平台可用性 / 动作元数据 / 当前绑定 / 精简模式
//   - 重绑必须「先查冲突再保存」（DOC-05 §2.3），失败要把原因留在该行
//   - 订阅 EV-18 hotkeys:changed：后端注册失败会带上 registerErr，行内直接标红
export const useHotkeys = (): UseHotkeysResult => {
  const [available, setAvailable] = useState<boolean | null>(null);
  const [actions, setActions] = useState<ActionInfo[]>([]);
  const [bindings, setBindings] = useState<Record<string, Binding>>({});
  const [lean, setLeanState] = useState<LeanState | null>(null);
  const [loading, setLoading] = useState(true);

  const [capturingId, setCapturingId] = useState<string | null>(null);
  const [capturePreview, setCapturePreviewState] = useState('');
  const [issues, setIssues] = useState<Record<string, HotkeyIssue>>({});
  const [busyId, setBusyId] = useState<string | null>(null);
  const [resetAllBusy, setResetAllBusy] = useState(false);
  const [leanBusy, setLeanBusy] = useState(false);

  const aliveRef = useRef(true);
  useEffect(() => {
    aliveRef.current = true;
    return () => {
      aliveRef.current = false;
    };
  }, []);

  const refresh = useCallback(async (): Promise<void> => {
    try {
      const [canUse, actionList, bindingList, leanState] = await Promise.all([
        HotkeysService.available(),
        HotkeysService.listActions(),
        HotkeysService.list(),
        HotkeysService.getLeanState(),
      ]);
      if (!aliveRef.current) return;
      setAvailable(canUse);
      setActions(actionList);
      const map: Record<string, Binding> = {};
      for (const binding of bindingList) map[binding.action] = binding;
      setBindings(map);
      setLeanState(leanState);
    } catch (err) {
      if (!aliveRef.current) return;
      const appErr = toAppError(err);
      // 拉取失败不等于平台不支持：保持 available=null 并留一条行外提示由视图展示
      setIssues((prev) => ({
        ...prev,
        __list__: {
          kind: 'failed',
          code: appErr.code,
          message: appErr.message,
          conflicts: [],
          preview: '',
        },
      }));
    } finally {
      if (aliveRef.current) setLoading(false);
    }
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  // EV-18：后端某动作的绑定变化（含注册失败原因）。
  useEffect(() => {
    const off = HotkeysService.onChanged((binding) => {
      if (!aliveRef.current) return;
      setBindings((prev) => ({ ...prev, [binding.action]: binding }));
    });
    return off;
  }, []);

  const setIssue = useCallback((actionId: string, issue: HotkeyIssue | null) => {
    setIssues((prev) => {
      const next = { ...prev };
      if (issue === null) delete next[actionId];
      else next[actionId] = issue;
      return next;
    });
  }, []);

  const bindingOf = useCallback(
    (actionId: string): Binding | null => bindings[actionId] ?? null,
    [bindings],
  );

  const issueOf = useCallback(
    (actionId: string): HotkeyIssue | null => issues[actionId] ?? null,
    [issues],
  );

  const groups = useMemo<HotkeyGroup[]>(() => {
    const byGroup = new Map<string, ActionInfo[]>();
    for (const action of actions) {
      const list = byGroup.get(action.group) ?? [];
      list.push(action);
      byGroup.set(action.group, list);
    }
    const ordered: HotkeyGroup[] = [];
    for (const group of HOTKEY_GROUP_ORDER) {
      const list = byGroup.get(group);
      if (list && list.length > 0) {
        ordered.push({ group, actions: list });
        byGroup.delete(group);
      }
    }
    // 未知分组（后端后加的场景）排在最后，不丢动作
    for (const [group, list] of byGroup) ordered.push({ group, actions: list });
    return ordered;
  }, [actions]);

  const beginCapture = useCallback((actionId: string) => {
    setCapturingId(actionId);
    setCapturePreviewState('');
  }, []);

  const cancelCapture = useCallback(() => {
    setCapturingId(null);
    setCapturePreviewState('');
  }, []);

  const setCapturePreview = useCallback((text: string) => {
    setCapturePreviewState(text);
  }, []);

  // 一次录入结束后收起录入框（成功 / 冲突 / 注册失败都算结束；仅「按键不支持」保留重试）。
  const endCapture = useCallback(() => {
    setCapturingId(null);
    setCapturePreviewState('');
  }, []);

  // 录入到后端的词汇表不认的键：留在行内提示，不当作成功。
  const reportUnsupportedKey = useCallback(
    (key: string) => {
      setIssues((prev) => {
        if (!capturingId) return prev;
        return {
          ...prev,
          [capturingId]: {
            kind: 'invalid',
            code: 'ERR_INVALID_ARGUMENT',
            message: key,
            conflicts: [],
            preview: '',
          },
        };
      });
    },
    [capturingId],
  );

  // 提交一次重绑：先查冲突（软件内 / 系统保留），再保存。
  const submitCapture = useCallback(
    async (actionId: string, accelerator: string): Promise<void> => {
      setBusyId(actionId);
      setIssue(actionId, null);
      try {
        // 预检：只做软件内冲突与系统保留判断，没有注册副作用。
        const report = await HotkeysService.detectConflicts(accelerator);
        if (!aliveRef.current) return;
        if (!report.ok) {
          // 冲突：结束录入态，但把原因留在该行（用户要能读到冲突对象与规范化预览）
          endCapture();
          setIssue(actionId, {
            kind: 'conflict',
            code: 'ERR_HOTKEY_CONFLICT',
            message: '',
            conflicts: report.conflicts ?? [],
            preview: report.preview,
          });
          return;
        }

        // 保存：后端会在落库前再查一次冲突并做预注册。
        await HotkeysService.set(actionId, accelerator);
        if (!aliveRef.current) return;
        endCapture();
        await refresh();
      } catch (err) {
        if (!aliveRef.current) return;
        const appErr = toAppError(err);
        const report = reportFromError(appErr.detail);
        if (appErr.code === 'ERR_HOTKEY_REGISTER') {
          endCapture();
          setIssue(actionId, {
            kind: 'register',
            code: appErr.code,
            message: appErr.message,
            conflicts: [],
            preview: '',
          });
        } else if (appErr.code === 'ERR_HOTKEY_CONFLICT') {
          // 保存阶段才发现的冲突：报告里可能有明细，没有就至少给出原因文案
          endCapture();
          setIssue(actionId, {
            kind: 'conflict',
            code: appErr.code,
            message: appErr.message,
            conflicts: report?.conflicts ?? [],
            preview: report?.preview ?? '',
          });
        } else {
          endCapture();
          setIssue(actionId, {
            kind: 'failed',
            code: appErr.code,
            message: appErr.message,
            conflicts: [],
            preview: '',
          });
        }
      } finally {
        if (aliveRef.current) setBusyId(null);
      }
    },
    [endCapture, refresh, setIssue],
  );

  const disableAction = useCallback(
    async (actionId: string): Promise<void> => {
      setBusyId(actionId);
      setIssue(actionId, null);
      try {
        await HotkeysService.disable(actionId);
        if (!aliveRef.current) return;
        await refresh();
      } catch (err) {
        if (!aliveRef.current) return;
        const appErr = toAppError(err);
        setIssue(actionId, {
          kind: 'failed',
          code: appErr.code,
          message: appErr.message,
          conflicts: [],
          preview: '',
        });
      } finally {
        if (aliveRef.current) setBusyId(null);
      }
    },
    [refresh, setIssue],
  );

  const resetAction = useCallback(
    async (actionId: string): Promise<void> => {
      setBusyId(actionId);
      setIssue(actionId, null);
      try {
        await HotkeysService.reset(actionId);
        if (!aliveRef.current) return;
        await refresh();
      } catch (err) {
        if (!aliveRef.current) return;
        const appErr = toAppError(err);
        setIssue(actionId, {
          kind: 'failed',
          code: appErr.code,
          message: appErr.message,
          conflicts: [],
          preview: '',
        });
      } finally {
        if (aliveRef.current) setBusyId(null);
      }
    },
    [refresh, setIssue],
  );

  const resetAllActions = useCallback(async (): Promise<void> => {
    setResetAllBusy(true);
    setIssues({});
    try {
      await HotkeysService.resetAll();
      if (!aliveRef.current) return;
      await refresh();
    } catch (err) {
      if (!aliveRef.current) return;
      const appErr = toAppError(err);
      setIssues((prev) => ({
        ...prev,
        __list__: {
          kind: 'failed',
          code: appErr.code,
          message: appErr.message,
          conflicts: [],
          preview: '',
        },
      }));
    } finally {
      if (aliveRef.current) setResetAllBusy(false);
    }
  }, [refresh]);

  const setLean = useCallback(async (enabled: boolean): Promise<void> => {
    setLeanBusy(true);
    try {
      const next = await HotkeysService.setLeanMode(enabled);
      if (!aliveRef.current) return;
      setLeanState(next);
    } catch (err) {
      if (!aliveRef.current) return;
      const appErr = toAppError(err);
      setIssues((prev) => ({
        ...prev,
        __lean__: {
          kind: 'failed',
          code: appErr.code,
          message: appErr.message,
          conflicts: [],
          preview: '',
        },
      }));
    } finally {
      if (aliveRef.current) setLeanBusy(false);
    }
  }, []);

  return {
    available,
    loading,
    groups,
    bindingOf,
    capturingId,
    capturePreview,
    issueOf,
    busyId,
    beginCapture,
    cancelCapture,
    submitCapture,
    setCapturePreview,
    reportUnsupportedKey,
    disableAction,
    resetAction,
    resetAllActions,
    resetAllBusy,
    lean,
    leanBusy,
    setLean,
    listIssue: issues['__list__'] ?? null,
    leanIssue: issues['__lean__'] ?? null,
    refresh,
  };
};

// 后端把冲突明细（ConflictReport）塞进 err.Detail 时可能带 JSON；能解析就用它补明细。
const reportFromError = (detail: string): ConflictReport | null => {
  const text = detail.trim();
  if (!text.startsWith('{')) return null;
  try {
    const parsed = JSON.parse(text) as Partial<ConflictReport>;
    return {
      conflicts: parsed.conflicts ?? [],
      preview: parsed.preview ?? '',
      ok: parsed.ok ?? false,
    };
  } catch {
    return null;
  }
};
