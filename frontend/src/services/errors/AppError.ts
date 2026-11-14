// AppError：把后端 apierr.Err 的结构化错误投影成前端可用形状。
//
// 后端 internal/apierr 通过 Wails 的 MarshalError 把错误序列化成
// { code, message, action, detail, retryable } JSON；前端 Promise reject 拿到的是
// RuntimeError，真正的载荷在 err.cause 里（可能是 JSON 字符串，也可能已被解析成对象）。
// 这里统一拍平成 AppError，供各 Service 抛出、ViewModel 按 code 映射 i18n 文案。
//
// 铁律：错误码只由后端登记（internal/apierr/apierr.go），前端不自造字符串错误码；
// 未知形状一律降级为 code = '' 并把原始文本放进 message，保证 UI 永远有话可说。

export interface AppErrorShape {
  // Code 后端 ERR_* 错误码；空字符串表示无法识别的错误。
  code: string;
  // Message 后端给的英文短句（已脱敏），仅作诊断兜底；UI 优先按 code 取 i18n 文案。
  message: string;
  // Action 后端建议的下一步动作键（如 'retry' / 'open-settings'），可能为空。
  action: string;
  // Detail 已脱敏的诊断信息，只用于「查看诊断」。
  detail: string;
  // Retryable 是否值得让用户点「重试」。
  retryable: boolean;
}

// AppError 是 AppErrorShape 的 Error 子类：既能 throw / catch，又保留结构化字段。
export class AppError extends Error implements AppErrorShape {
  readonly code: string;
  readonly action: string;
  readonly detail: string;
  readonly retryable: boolean;

  constructor(shape: AppErrorShape) {
    super(shape.message);
    this.name = 'AppError';
    this.code = shape.code;
    this.action = shape.action;
    this.detail = shape.detail;
    this.retryable = shape.retryable;
  }
}

const asRecord = (value: unknown): Record<string, unknown> | null => {
  if (typeof value !== 'object' || value === null) return null;
  return value as Record<string, unknown>;
};

const readString = (source: Record<string, unknown>, key: string): string => {
  const raw = source[key];
  return typeof raw === 'string' ? raw : '';
};

// parseCause 尝试从 rejection reason 的 cause 里取出后端结构化错误。
// Wails 默认普通传输下 cause 是 MarshalError 返回的 JSON 文本；
// 其它传输（含测试桩）可能直接给对象，两种都接受。
const parseCause = (cause: unknown): Record<string, unknown> | null => {
  if (typeof cause === 'string') {
    const text = cause.trim();
    if (!text.startsWith('{')) return null;
    try {
      return asRecord(JSON.parse(text));
    } catch {
      return null;
    }
  }
  return asRecord(cause);
};

// toAppError 把任意 catch 到的值归一化为 AppError。
export const toAppError = (err: unknown): AppError => {
  if (err instanceof AppError) return err;

  const reason = asRecord(err);
  const payload = parseCause(reason?.cause) ?? null;
  const message = err instanceof Error ? err.message : typeof err === 'string' ? err : '';

  if (payload === null) {
    return new AppError({
      code: '',
      message,
      action: '',
      detail: '',
      retryable: false,
    });
  }

  return new AppError({
    code: readString(payload, 'code'),
    // 后端 message 优先：它带上下文；没有才回落到 Error.message。
    message: readString(payload, 'message') || message,
    action: readString(payload, 'action'),
    detail: readString(payload, 'detail'),
    retryable: payload['retryable'] === true,
  });
};

// hasCode 便捷判断（ViewModel 里按错误码分支时用）。
export const hasCode = (err: unknown, code: string): boolean => toAppError(err).code === code;
