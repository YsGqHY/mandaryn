// 键盘事件 → 后端可解析的加速键显示形。
//
// 为什么不用手打字符串：用户无法记住 "CmdOrCtrl+Alt+R" 这种写法，且手打必然
// 产生大量格式错误（后端 ERR_INVALID_ARGUMENT 带 Reason 明细）。这里直接监听
// keydown 组装，产出与后端 List/Display 一致的**显示形**（Set / DetectConflicts
// 都接受该形态，后端 Parse 会把字形还原成规范形）。
//
// 平台差异（与 internal/hotkeys/accel.go 的 Display 对齐）：
//   - macOS：修饰键用字形且不加分隔符（⌘⌥R）；⌘=Meta、⌃=Ctrl、⌥=Alt、⇧=Shift
//   - 其它：修饰键按 Ctrl / Alt / Shift / Win（Linux 为 Super）顺序以 + 连接
//   - 方向键统一用箭头字形（后端 keyAliases 可还原）；其余主键用规范名（Space / F2 / Delete …）
import { isMac, isWindows } from '@/shared/platform';

// 只按了修饰键（还没按主键）时返回等待信号，不算一次输入完成。
export type CaptureOutcome =
  | { kind: 'waiting' } // 只有修饰键，继续等主键
  | { kind: 'cancel' } // 单独按 Esc = 放弃录入
  | { kind: 'unsupported'; key: string } // 主键不在后端词汇表里（无法绑定）
  | { kind: 'ready'; accelerator: string; preview: string };

// 物理键位 → 后端规范主键名（用 KeyboardEvent.code，避免 Shift 改变字符）。
const NAMED_BY_CODE: Record<string, string> = {
  Space: 'Space',
  Escape: 'Escape',
  Tab: 'Tab',
  Backspace: 'Backspace',
  Enter: 'Enter',
  NumpadEnter: 'Enter',
  Delete: 'Delete',
  Insert: 'Insert',
  Home: 'Home',
  End: 'End',
  PageUp: 'PageUp',
  PageDown: 'PageDown',
  ArrowLeft: 'Left',
  ArrowRight: 'Right',
  ArrowUp: 'Up',
  ArrowDown: 'Down',
  Comma: ',',
  Period: '.',
  Slash: '/',
  Semicolon: ';',
  Quote: "'",
  BracketLeft: '[',
  BracketRight: ']',
  Backslash: '\\',
  Minus: '-',
  Equal: '=',
  Backquote: '`',
};

// 箭头字形（跨平台共用，后端可解析回来）。
const ARROW_GLYPH: Record<string, string> = {
  Left: '←',
  Right: '→',
  Up: '↑',
  Down: '↓',
};

// KeyboardEvent.code → 规范主键名。无法识别时返回 null。
const keyNameOf = (event: KeyboardEvent): string | null => {
  const named = NAMED_BY_CODE[event.code];
  if (named) return named;

  // 字母：KeyR → R
  if (/^Key[A-Z]$/.test(event.code)) return event.code.slice(3);

  // 数字（含数字键盘）：Digit1 / Numpad1 → 1
  const digit = /^(?:Digit|Numpad)([0-9])$/.exec(event.code);
  if (digit?.[1]) return digit[1];

  // 功能键：F1..F35
  const fn = /^F([0-9]{1,2})$/.exec(event.code);
  if (fn?.[1]) return `F${fn[1]}`;

  return null;
};

const isModifierEvent = (event: KeyboardEvent): boolean =>
  event.key === 'Shift' ||
  event.key === 'Control' ||
  event.key === 'Alt' ||
  event.key === 'AltGraph' ||
  event.key === 'Meta' ||
  event.key === 'OS' ||
  event.key === 'CapsLock';

interface Modifiers {
  ctrl: boolean;
  alt: boolean;
  shift: boolean;
  meta: boolean;
}

// 显示形：修饰键 + 主键。glyph=false 时主键不换成箭头字形（录入中的提示用）。
const formatAccelerator = (mods: Modifiers, key: string, glyph = true): string => {
  const keyText = glyph ? (ARROW_GLYPH[key] ?? key) : key;
  if (isMac) {
    // 与后端 darwinModOrder 一致的阅读顺序：⌘ ⌥ ⇧ ⌃
    let out = '';
    if (mods.meta) out += '⌘';
    if (mods.alt) out += '⌥';
    if (mods.shift) out += '⇧';
    if (mods.ctrl) out += '⌃';
    return out + keyText;
  }
  const parts: string[] = [];
  if (mods.ctrl) parts.push('Ctrl');
  if (mods.alt) parts.push('Alt');
  if (mods.shift) parts.push('Shift');
  // Meta 在 Windows 叫 Win、Linux 叫 Super（与后端 platformModName 对齐）
  if (mods.meta) parts.push(isWindows ? 'Win' : 'Super');
  parts.push(keyText);
  return parts.join('+');
};

// 把一次 keydown 组装成加速键；不完成时返回 waiting / cancel。
export const captureFromEvent = (event: KeyboardEvent): CaptureOutcome => {
  if (isModifierEvent(event)) return { kind: 'waiting' };

  const key = keyNameOf(event);
  if (!key) {
    // 不在后端词汇表：给出明确「不支持该键」而不是静默吞掉
    if (event.key === 'Escape') return { kind: 'cancel' };
    return { kind: 'unsupported', key: event.key || event.code };
  }

  // 单独按 Esc 视为放弃（Esc 本身带修饰键时仍可绑定）
  const mods: Modifiers = {
    ctrl: event.ctrlKey,
    alt: event.altKey,
    shift: event.shiftKey,
    meta: event.metaKey,
  };
  const hasModifier = mods.ctrl || mods.alt || mods.shift || mods.meta;
  if (!hasModifier && key === 'Escape') return { kind: 'cancel' };
  if (!hasModifier && (key === 'Shift' || key === 'Control' || key === 'Alt' || key === 'Meta')) {
    return { kind: 'waiting' };
  }

  const accelerator = formatAccelerator(mods, key);
  return { kind: 'ready', accelerator, preview: accelerator };
};

// 录入过程的实时提示：只组装修饰键（还没按主键时）或「修饰键 + 待定主键」。
export const modifierPreview = (event: KeyboardEvent): string =>
  formatAccelerator(
    {
      ctrl: event.ctrlKey,
      alt: event.altKey,
      shift: event.shiftKey,
      meta: event.metaKey,
    },
    isModifierEvent(event) ? '' : (keyNameOf(event) ?? ''),
    false,
  );
