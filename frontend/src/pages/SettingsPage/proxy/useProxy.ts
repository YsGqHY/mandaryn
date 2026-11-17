import { useCallback, useEffect, useRef, useState } from 'react';
import {
  AppSettingsService,
  toAppError,
  type ProxyConfigDTO,
  type ProxyTestResult,
  type ProxyType,
} from '@/services';

// 代理子页面 VM（FR-05-5 / API-9.3 / API-9.4）。
//
// 职责边界：
//   1. 读写 app_settings.proxy*（保存 / 清空）；
//   2. 调 TestProxy 做一次真实出站探测，把结果原样暴露；
//   3. **不拼人类可见文案** —— 分类→文案的映射归 View（与其它子页面一致）。
//
// 密码语义（很重要）：后端读出的是**掩码**（如 `ab***yz`）。保存时回传掩码或空串
// 表示「保持库中已存的那份」，因此用户不改密码就能安全地只改主机/端口。

// Feedback 是保存 / 测试的反馈状态。
export type ProxyFeedback =
  | { kind: 'idle' }
  | { kind: 'saving' }
  | { kind: 'saved' }
  | { kind: 'cleared' }
  | { kind: 'testing' }
  | { kind: 'tested'; result: ProxyTestResult }
  | { kind: 'failed'; code: string; message: string };

// ProxyForm 是表单的可编辑字段（type 单独暴露给分段控件）。
export interface ProxyForm {
  type: ProxyType;
  host: string;
  port: string; // 输入框里用字符串，提交时再校验成数字
  username: string;
  password: string; // 掩码或空串（空串 = 不改密码）
  useForDownload: boolean;
  useForOnline: boolean;
  noProxy: string;
}

export interface UseProxyResult {
  form: ProxyForm;
  loading: boolean;
  feedback: ProxyFeedback;
  // envOverride 为真时表示 MANDARYN_HTTP_PROXY 已接管：设置页的配置不会生效。
  envOverride: string;
  // dirty 表单是否有未保存改动（用于提示「改了没保存」）。
  dirty: boolean;
  setField: <K extends keyof ProxyForm>(key: K, value: ProxyForm[K]) => void;
  save: () => Promise<void>;
  clear: () => Promise<void>;
  test: () => Promise<void>;
  resetFeedback: () => void;
}

// EMPTY_FORM 是「未配置代理」的表单初值（与后端 type=none 的语义一致）。
const EMPTY_FORM: ProxyForm = {
  type: 'none',
  host: '',
  port: '',
  username: '',
  password: '',
  useForDownload: true,
  useForOnline: true,
  noProxy: '',
};

// toForm 把后端 DTO 转成表单形状。
const toForm = (p: ProxyConfigDTO): ProxyForm => ({
  type: (['none', 'http', 'https', 'socks5'].includes(p.type) ? p.type : 'none') as ProxyType,
  host: p.host,
  port: p.port > 0 ? String(p.port) : '',
  username: p.username,
  password: p.password,
  useForDownload: p.useForDownload,
  useForOnline: p.useForOnline,
  noProxy: p.noProxy,
});

// toDTO 把表单转成后端 DTO。校验失败返回 error 文本键（由 View 翻译）。
//
// 校验与后端保持一致：主机必填、端口 1–65535。这里先拦一道，
// 避免用户点了保存才看到后端错误。
const toDTO = (f: ProxyForm): { dto: ProxyConfigDTO } | { invalid: 'host' | 'port' } => {
  if (f.type === 'none') {
    return { dto: { ...EMPTY_FORM, port: 0, type: 'none' } as unknown as ProxyConfigDTO };
  }
  const host = f.host.trim();
  if (host === '') return { invalid: 'host' };
  const port = Number(f.port.trim());
  if (!Number.isInteger(port) || port <= 0 || port > 65535) return { invalid: 'port' };
  return {
    dto: {
      type: f.type,
      host,
      port,
      username: f.username.trim(),
      password: f.password,
      useForDownload: f.useForDownload,
      useForOnline: f.useForOnline,
      noProxy: f.noProxy.trim(),
    },
  };
};

export const useProxy = (): UseProxyResult => {
  const [form, setForm] = useState<ProxyForm>(EMPTY_FORM);
  const [saved, setSaved] = useState<ProxyForm>(EMPTY_FORM);
  const [loading, setLoading] = useState<boolean>(true);
  const [feedback, setFeedback] = useState<ProxyFeedback>({ kind: 'idle' });

  const aliveRef = useRef<boolean>(true);
  useEffect(() => {
    aliveRef.current = true;
    return () => {
      aliveRef.current = false;
    };
  }, []);

  const load = useCallback(async (): Promise<void> => {
    setLoading(true);
    try {
      const proxy = await AppSettingsService.getProxy();
      if (!aliveRef.current) return;
      const next = toForm(proxy);
      setForm(next);
      setSaved(next);
    } catch (err) {
      if (!aliveRef.current) return;
      const appErr = toAppError(err);
      setFeedback({ kind: 'failed', code: appErr.code, message: appErr.message });
    } finally {
      if (aliveRef.current) setLoading(false);
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  // 环境变量接管状态：由后端在设置快照里给出（MANDARYN_HTTP_PROXY 非空即接管）。
  const [envOverride, setEnvOverride] = useState<string>('');
  useEffect(() => {
    void AppSettingsService.getProxyEnvOverride()
      .then((name) => {
        if (aliveRef.current) setEnvOverride(name);
      })
      .catch(() => {
        // 读不到就按「未接管」展示：这不影响保存，只是少一条提示。
      });
  }, []);

  const setField = useCallback(
    <K extends keyof ProxyForm>(key: K, value: ProxyForm[K]): void => {
      setForm((prev) => ({ ...prev, [key]: value }));
      // 改动后清掉上一次的保存/测试反馈，避免「已保存」与当前输入不符。
      setFeedback((prev) => (prev.kind === 'testing' ? prev : { kind: 'idle' }));
    },
    [],
  );

  const save = useCallback(async (): Promise<void> => {
    const parsed = toDTO(form);
    if ('invalid' in parsed) {
      setFeedback({ kind: 'failed', code: parsed.invalid, message: '' });
      return;
    }
    setFeedback({ kind: 'saving' });
    try {
      await AppSettingsService.setProxy(parsed.dto);
      if (!aliveRef.current) return;
      // 重新读一次：密码回显要以库中掩码为准，不能用用户输入的值。
      const proxy = await AppSettingsService.getProxy();
      if (!aliveRef.current) return;
      const next = toForm(proxy);
      setForm(next);
      setSaved(next);
      setFeedback({ kind: 'saved' });
    } catch (err) {
      if (!aliveRef.current) return;
      const appErr = toAppError(err);
      setFeedback({ kind: 'failed', code: appErr.code, message: appErr.message });
    }
  }, [form]);

  const clear = useCallback(async (): Promise<void> => {
    setFeedback({ kind: 'saving' });
    try {
      await AppSettingsService.clearProxy();
      if (!aliveRef.current) return;
      setForm(EMPTY_FORM);
      setSaved(EMPTY_FORM);
      setFeedback({ kind: 'cleared' });
    } catch (err) {
      if (!aliveRef.current) return;
      const appErr = toAppError(err);
      setFeedback({ kind: 'failed', code: appErr.code, message: appErr.message });
    }
  }, []);

  const test = useCallback(async (): Promise<void> => {
    const parsed = toDTO(form);
    if ('invalid' in parsed) {
      setFeedback({ kind: 'failed', code: parsed.invalid, message: '' });
      return;
    }
    setFeedback({ kind: 'testing' });
    try {
      const result = await AppSettingsService.testProxy(parsed.dto);
      if (!aliveRef.current) return;
      setFeedback({ kind: 'tested', result });
    } catch (err) {
      if (!aliveRef.current) return;
      const appErr = toAppError(err);
      setFeedback({ kind: 'failed', code: appErr.code, message: appErr.message });
    }
  }, [form]);

  const resetFeedback = useCallback((): void => setFeedback({ kind: 'idle' }), []);

  const dirty =
    form.type !== saved.type ||
    form.host !== saved.host ||
    form.port !== saved.port ||
    form.username !== saved.username ||
    form.useForDownload !== saved.useForDownload ||
    form.useForOnline !== saved.useForOnline ||
    form.noProxy !== saved.noProxy ||
    (form.password !== '' && form.password !== saved.password);

  return {
    form,
    loading,
    feedback,
    envOverride,
    dirty,
    setField,
    save,
    clear,
    test,
    resetFeedback,
  };
};
