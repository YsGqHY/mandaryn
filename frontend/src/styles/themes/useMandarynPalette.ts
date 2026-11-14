import { useMandarynTheme } from './ThemeProvider';
import type { MandarynPalette } from './types';

// JS 侧读取当前生效的 MandarynPalette（具体颜色值）。
//
// 绝大多数场景应在 *.module.css 里写 var(--fd-xxx)，不需要这个 hook。
// 只有必须拿"真实色值"的地方才用，例如：
//  - 需要计算透明度 / 渐变的 canvas、SVG 自绘图表
//  - 第三方库只接受具体颜色字符串
export const useMandarynPalette = (): MandarynPalette => useMandarynTheme().current.palette;
