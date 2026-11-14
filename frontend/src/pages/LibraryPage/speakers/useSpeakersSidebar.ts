import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import {
  SessionService,
  SpeakerService,
  roleIsUnidentified,
  toAppError,
  type AppError,
  type DiarizeResult,
  type Role,
  type Segment,
  type SessionSpeaker,
} from '@/services';

// 侧栏里一行说话人：已识别（有 voiceId）与未识别（合成行）形状一致，
// 差异只在 unidentified 标记 —— 但两者的能力完全不同（未识别不能指派 / 拆分）。
export interface SpeakerRow {
  // 声纹身份 ID；未识别行为空字符串。
  speakerId: string;
  roleId: string;
  // 角色名由后端 roles JOIN 得到；前端**不缓存**，改名后靠重新拉取刷新。
  name: string;
  colorToken: string;
  utteranceCount: number;
  speechMs: number;
  unidentified: boolean;
}

export interface UseSpeakersSidebarResult {
  // —— 数据 ——
  loading: boolean;
  error: AppError | null;
  dismissError: () => void;
  rows: SpeakerRow[];
  // 未识别行单独返回：它不是普通角色，UI 必须分开呈现。
  unidentified: SpeakerRow | null;
  // 可作为指派目标的角色（排除内置「未识别」）。
  assignableRoles: Role[];
  reload: () => Promise<void>;
  // —— 分离能力 ——
  capable: boolean;
  capabilityChecked: boolean;
  analyzing: boolean;
  analyze: () => Promise<void>;
  analyzeResult: DiarizeResult | null;
  dismissAnalyzeResult: () => void;
  // —— 修改角色名（内联） ——
  editingRoleId: string | null;
  nameDraft: string;
  beginRename: (row: SpeakerRow) => void;
  cancelRename: () => void;
  setNameDraft: (value: string) => void;
  submitRename: () => Promise<boolean>;
  // —— 合并（不可自动还原，View 负责原生二次确认） ——
  mergeOpen: boolean;
  mergeFromId: string;
  mergeToId: string;
  beginMerge: (fromSpeakerId?: string) => void;
  cancelMerge: () => void;
  setMergeFromId: (speakerId: string) => void;
  setMergeToId: (speakerId: string) => void;
  submitMerge: () => Promise<boolean>;
  // —— 拆分 ——
  splitRow: SpeakerRow | null;
  splitSegments: Segment[];
  splitLoading: boolean;
  splitSelected: number[];
  beginSplit: (row: SpeakerRow) => void;
  cancelSplit: () => void;
  toggleSplitSegment: (segmentId: number) => void;
  submitSplit: () => Promise<boolean>;
  // —— 指派 ——
  assignRow: SpeakerRow | null;
  assignRoleId: string;
  beginAssign: (row: SpeakerRow) => void;
  cancelAssign: () => void;
  setAssignRoleId: (roleId: string) => void;
  submitAssign: () => Promise<boolean>;
  // 面板级忙标记（改名 / 合并 / 拆分 / 指派共用，避免重复提交）。
  busy: boolean;
}

// 后端 SessionSpeaker → 侧栏行。
const toRow = (speaker: SessionSpeaker): SpeakerRow => ({
  speakerId: speaker.speakerId,
  roleId: speaker.roleId,
  name: speaker.roleName,
  colorToken: speaker.colorToken,
  utteranceCount: speaker.utteranceCount,
  speechMs: speaker.speechMs,
  // 未识别行由后端合成（speakerId 为空 + roleId = rl_unknown），两个条件都认。
  unidentified: speaker.speakerId === '' || roleIsUnidentified(speaker.roleId),
});

// SpeakersSidebar ViewModel：会话内说话人 → 角色映射的读取与全部写操作。
//
// 铁律对照：
//   - 所有调用都经 services/speaker（本 hook 不 import bindings）；
//   - 角色改名后**重新拉取**列表，不在本地做名字替换（片段角色名由后端 JOIN 得到）；
//   - roles:changed（EV-07）与 transcription:state（EV-16，本会话转写完成）都触发重拉；
//   - 每个 useEffect 的订阅都在 cleanup 里取消。
export const useSpeakersSidebar = (sessionId: string): UseSpeakersSidebarResult => {
  const [rows, setRows] = useState<SpeakerRow[]>([]);
  const [unidentified, setUnidentified] = useState<SpeakerRow | null>(null);
  const [roles, setRoles] = useState<Role[]>([]);
  const [loading, setLoading] = useState<boolean>(true);
  const [error, setError] = useState<AppError | null>(null);
  const [capable, setCapable] = useState<boolean>(false);
  const [capabilityChecked, setCapabilityChecked] = useState<boolean>(false);
  const [analyzing, setAnalyzing] = useState<boolean>(false);
  const [analyzeResult, setAnalyzeResult] = useState<DiarizeResult | null>(null);
  const [busy, setBusy] = useState<boolean>(false);
  const [reloadToken, setReloadToken] = useState<number>(0);

  const [editingRoleId, setEditingRoleId] = useState<string | null>(null);
  const [nameDraft, setNameDraft] = useState<string>('');

  const [mergeOpen, setMergeOpen] = useState<boolean>(false);
  const [mergeFromId, setMergeFromIdState] = useState<string>('');
  const [mergeToId, setMergeToIdState] = useState<string>('');

  const [splitRow, setSplitRow] = useState<SpeakerRow | null>(null);
  const [splitSegments, setSplitSegments] = useState<Segment[]>([]);
  const [splitLoading, setSplitLoading] = useState<boolean>(false);
  const [splitSelected, setSplitSelected] = useState<number[]>([]);

  const [assignRow, setAssignRow] = useState<SpeakerRow | null>(null);
  const [assignRoleId, setAssignRoleId] = useState<string>('');

  const aliveRef = useRef<boolean>(true);

  useEffect(() => {
    aliveRef.current = true;
    return () => {
      aliveRef.current = false;
    };
  }, []);

  // 换会话时清掉上一个会话的弹窗 / 内联编辑状态，避免把 A 的说话人操作到 B 上。
  useEffect(() => {
    setEditingRoleId(null);
    setNameDraft('');
    setMergeOpen(false);
    setMergeFromIdState('');
    setMergeToIdState('');
    setSplitRow(null);
    setSplitSegments([]);
    setSplitSelected([]);
    setAssignRow(null);
    setAssignRoleId('');
    setAnalyzeResult(null);
  }, [sessionId]);

  const reload = useCallback(async (): Promise<void> => setReloadToken((token) => token + 1), []);

  // 主加载：说话人映射 + 全局角色 + 分离能力（三者互相独立，失败不互相拖死）。
  useEffect(() => {
    if (sessionId === '') return undefined;
    let cancelled = false;
    const load = async (): Promise<void> => {
      setLoading(true);
      try {
        const [speakers, allRoles] = await Promise.all([
          SpeakerService.listSessionSpeakers(sessionId),
          SpeakerService.listRoles(),
        ]);
        if (cancelled || !aliveRef.current) return;
        const mapped = speakers.map(toRow);
        setRows(mapped.filter((row) => !row.unidentified));
        // 后端只会附一条合成行；仍按「取第一条」处理，避免重复渲染。
        setUnidentified(mapped.find((row) => row.unidentified) ?? null);
        setRoles(allRoles.filter((role) => !roleIsUnidentified(role.id)));
        setError(null);
      } catch (err) {
        if (cancelled || !aliveRef.current) return;
        setRows([]);
        setUnidentified(null);
        setRoles([]);
        setError(toAppError(err));
      } finally {
        if (!cancelled && aliveRef.current) setLoading(false);
      }
    };
    void load();
    return () => {
      cancelled = true;
    };
  }, [sessionId, reloadToken]);

  // 分离能力单独查：HasDiarization 返回 false 不是错误，只是按钮要置灰。
  useEffect(() => {
    if (sessionId === '') return undefined;
    let cancelled = false;
    const load = async (): Promise<void> => {
      try {
        const ok = await SpeakerService.hasDiarization();
        if (cancelled || !aliveRef.current) return;
        setCapable(ok);
      } catch {
        // 查询失败按「不可用」处理：宁可不给按钮，也不假装能跑。
        if (cancelled || !aliveRef.current) return;
        setCapable(false);
      } finally {
        if (!cancelled && aliveRef.current) setCapabilityChecked(true);
      }
    };
    void load();
    return () => {
      cancelled = true;
    };
  }, [sessionId, reloadToken]);

  // EV-07：任何角色变更都可能影响本会话（改名 / 换色 / 合并 / 拆分 / 指派）。
  // 一律重拉，**不做**本地名字替换（片段角色名由后端 JOIN 得到）。
  useEffect(() => {
    const cancel = SpeakerService.onRolesChanged(() => {
      if (!aliveRef.current) return;
      void reload();
    });
    return cancel;
  }, [reload]);

  // EV-16：本会话转写完成 → 新增片段可能带来新的说话人 / 未识别片段，重拉一次。
  useEffect(() => {
    if (sessionId === '') return undefined;
    const cancel = SessionService.onTranscriptionState((state) => {
      if (!aliveRef.current) return;
      if (state.sessionId !== sessionId) return;
      if (state.status === 'done') void reload();
    });
    return cancel;
  }, [sessionId, reload]);

  // —— 发起分离 ——
  const analyze = useCallback(async (): Promise<void> => {
    if (sessionId === '') return;
    setAnalyzing(true);
    setError(null);
    setAnalyzeResult(null);
    try {
      const result = await SpeakerService.analyzeSession(sessionId);
      if (!aliveRef.current) return;
      setAnalyzeResult(result);
      await reload();
    } catch (err) {
      if (!aliveRef.current) return;
      setError(toAppError(err));
    } finally {
      if (aliveRef.current) setAnalyzing(false);
    }
  }, [sessionId, reload]);

  const dismissAnalyzeResult = useCallback((): void => setAnalyzeResult(null), []);

  const dismissError = useCallback((): void => setError(null), []);

  // —— 改名（内联） ——
  const beginRename = useCallback((row: SpeakerRow): void => {
    setEditingRoleId(row.roleId);
    setNameDraft(row.name);
  }, []);

  const cancelRename = useCallback((): void => {
    setEditingRoleId(null);
    setNameDraft('');
  }, []);

  const submitRename = useCallback(async (): Promise<boolean> => {
    const roleId = editingRoleId;
    const name = nameDraft.trim();
    if (roleId === null) return false;
    // 空名字没有意义（后端也会拒），留在内联编辑里让用户改。
    if (name === '') return false;
    setBusy(true);
    setError(null);
    try {
      await SpeakerService.renameRole(roleId, name);
      if (!aliveRef.current) return false;
      setEditingRoleId(null);
      setNameDraft('');
      await reload();
      return true;
    } catch (err) {
      if (aliveRef.current) setError(toAppError(err));
      return false;
    } finally {
      if (aliveRef.current) setBusy(false);
    }
  }, [editingRoleId, nameDraft, reload]);

  // —— 合并 ——
  const beginMerge = useCallback((fromSpeakerId?: string): void => {
    setMergeFromIdState(fromSpeakerId ?? '');
    setMergeToIdState('');
    setMergeOpen(true);
  }, []);

  const cancelMerge = useCallback((): void => {
    setMergeOpen(false);
    setMergeFromIdState('');
    setMergeToIdState('');
  }, []);

  const setMergeFromId = useCallback((speakerId: string): void => {
    setMergeFromIdState(speakerId);
    // 源换了且与目标相同：清掉目标，避免出现「合并到自己」。
    setMergeToIdState((current) => (current === speakerId ? '' : current));
  }, []);

  const setMergeToId = useCallback((speakerId: string): void => setMergeToIdState(speakerId), []);

  const submitMerge = useCallback(async (): Promise<boolean> => {
    if (mergeFromId === '' || mergeToId === '' || mergeFromId === mergeToId) return false;
    setBusy(true);
    setError(null);
    try {
      await SpeakerService.mergeSpeakers(mergeFromId, mergeToId);
      if (!aliveRef.current) return false;
      cancelMerge();
      await reload();
      return true;
    } catch (err) {
      if (aliveRef.current) setError(toAppError(err));
      return false;
    } finally {
      if (aliveRef.current) setBusy(false);
    }
  }, [mergeFromId, mergeToId, cancelMerge, reload]);

  // —— 拆分（片段来自时间轴，按该说话人过滤） ——
  const beginSplit = useCallback(
    (row: SpeakerRow): void => {
      setSplitRow(row);
      setSplitSelected([]);
      setSplitSegments([]);
      setSplitLoading(true);
      const load = async (): Promise<void> => {
        try {
          const timeline = await SessionService.getTimeline(sessionId);
          if (!aliveRef.current) return;
          setSplitSegments(timeline.segments.filter((segment) => segment.speakerId === row.speakerId));
          setError(null);
        } catch (err) {
          if (!aliveRef.current) return;
          setSplitSegments([]);
          setError(toAppError(err));
        } finally {
          if (aliveRef.current) setSplitLoading(false);
        }
      };
      void load();
    },
    [sessionId],
  );

  const cancelSplit = useCallback((): void => {
    setSplitRow(null);
    setSplitSegments([]);
    setSplitSelected([]);
  }, []);

  const toggleSplitSegment = useCallback((segmentId: number): void => {
    setSplitSelected((prev) =>
      prev.includes(segmentId) ? prev.filter((id) => id !== segmentId) : [...prev, segmentId],
    );
  }, []);

  const submitSplit = useCallback(async (): Promise<boolean> => {
    const row = splitRow;
    if (row === null || splitSelected.length === 0) return false;
    setBusy(true);
    setError(null);
    try {
      await SpeakerService.splitSpeaker(sessionId, row.speakerId, splitSelected);
      if (!aliveRef.current) return false;
      cancelSplit();
      await reload();
      return true;
    } catch (err) {
      if (aliveRef.current) setError(toAppError(err));
      return false;
    } finally {
      if (aliveRef.current) setBusy(false);
    }
  }, [splitRow, splitSelected, sessionId, cancelSplit, reload]);

  // —— 指派 ——
  const beginAssign = useCallback((row: SpeakerRow): void => {
    setAssignRow(row);
    setAssignRoleId('');
  }, []);

  const cancelAssign = useCallback((): void => {
    setAssignRow(null);
    setAssignRoleId('');
  }, []);

  const submitAssign = useCallback(async (): Promise<boolean> => {
    const row = assignRow;
    if (row === null || row.speakerId === '' || assignRoleId === '') return false;
    setBusy(true);
    setError(null);
    try {
      await SpeakerService.assignSpeaker(sessionId, row.speakerId, assignRoleId);
      if (!aliveRef.current) return false;
      cancelAssign();
      await reload();
      return true;
    } catch (err) {
      if (aliveRef.current) setError(toAppError(err));
      return false;
    } finally {
      if (aliveRef.current) setBusy(false);
    }
  }, [assignRow, assignRoleId, sessionId, cancelAssign, reload]);

  // 指派目标：排除内置「未识别」，其余全部可选（含该说话人当前角色 —— 后端幂等）。
  const assignableRoles = useMemo(
    () => roles.filter((role) => !roleIsUnidentified(role.id)),
    [roles],
  );

  return {
    loading,
    error,
    dismissError,
    rows,
    unidentified,
    assignableRoles,
    reload,
    capable,
    capabilityChecked,
    analyzing,
    analyze,
    analyzeResult,
    dismissAnalyzeResult,
    editingRoleId,
    nameDraft,
    beginRename,
    cancelRename,
    setNameDraft,
    submitRename,
    mergeOpen,
    mergeFromId,
    mergeToId,
    beginMerge,
    cancelMerge,
    setMergeFromId,
    setMergeToId,
    submitMerge,
    splitRow,
    splitSegments,
    splitLoading,
    splitSelected,
    beginSplit,
    cancelSplit,
    toggleSplitSegment,
    submitSplit,
    assignRow,
    assignRoleId,
    beginAssign,
    cancelAssign,
    setAssignRoleId,
    submitAssign,
    busy,
  };
};
