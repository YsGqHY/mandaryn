import type { CSSProperties } from 'react';
import barsUrl from '@/assets/icons/bars.svg?url';
import cubeUrl from '@/assets/icons/cube.svg?url';
import exportUrl from '@/assets/icons/export.svg?url';
import gaugeUrl from '@/assets/icons/gauge.svg?url';
import micUrl from '@/assets/icons/mic.svg?url';
import pauseUrl from '@/assets/icons/pause.svg?url';
import shieldUrl from '@/assets/icons/shield.svg?url';
import waveUrl from '@/assets/icons/wave.svg?url';
import windowUrl from '@/assets/icons/window.svg?url';

// 品牌线性图标（sunburst 图标集，逐图 hand-authored SVG）。
//
// 实现方式：**CSS mask**，不是 <img> 直接显示。
//   为什么要多这一层：SVG 里的描边色是写死的 #2064f8，而深色主题的主色是 #60a5fa。
//   用 mask 把图形当"镂空模板"，颜色由 `background-color: currentColor` 提供，
//   于是图标自动跟随主题与文字色，不必为明暗两套各存一份文件。
//   同时它也不产生内联 <svg> 节点，规避前端铁律里「禁止内联 svg」那条。
//
// ⚠️ 使用边界：项目铁律要求 UI 图标一律用 `@tabler/icons-react`。
// 本组件面向「品牌资产 / 空状态 / 关于页 / 营销位」；
// 若要用于导航或工具栏，需先明确放宽该铁律（见交付说明）。

export type BrandIconName =
  | 'mic'
  | 'bars'
  | 'cube'
  | 'window'
  | 'wave'
  | 'shield'
  | 'gauge'
  | 'pause'
  | 'export';

// 原始 viewBox 的纵横比（宽/高），用于按宽度推导高度，避免压扁。
const RATIO: Record<BrandIconName, number> = {
  mic: 17.5 / 21.7,
  bars: 18.9 / 14.3,
  cube: 20.6 / 21.8,
  window: 22.2 / 19.2,
  wave: 19.6 / 16.7,
  shield: 20.6 / 21.9,
  gauge: 22.7 / 14.6,
  pause: 12.5 / 16.5,
  export: 20.6 / 16.8,
};

const SRC: Record<BrandIconName, string> = {
  mic: micUrl,
  bars: barsUrl,
  cube: cubeUrl,
  window: windowUrl,
  wave: waveUrl,
  shield: shieldUrl,
  gauge: gaugeUrl,
  pause: pauseUrl,
  export: exportUrl,
};

export interface BrandIconProps {
  name: BrandIconName;
  /** 图标宽度（px）；高度按原始比例推导。 */
  size?: number;
  /** 无障碍标题。提供时用 <span role="img">，否则视为纯装饰隐藏。 */
  title?: string;
  className?: string;
  /** 仅覆盖颜色等少量属性；尺寸由 size 控制。 */
  style?: CSSProperties;
}

export const BrandIcon = ({ name, size = 20, title, className, style }: BrandIconProps) => {
  const height = Math.round(size / RATIO[name]);
  const url = `url(${JSON.stringify(SRC[name])})`;
  const maskStyle: CSSProperties = {
    width: size,
    height,
    backgroundColor: 'currentColor',
    maskImage: url,
    WebkitMaskImage: url,
    maskSize: 'contain',
    WebkitMaskSize: 'contain',
    maskRepeat: 'no-repeat',
    WebkitMaskRepeat: 'no-repeat',
    maskPosition: 'center',
    WebkitMaskPosition: 'center',
    flexShrink: 0,
    display: 'inline-block',
    ...style,
  };

  if (title === undefined) {
    return <span aria-hidden className={className} style={maskStyle} />;
  }
  return <span role="img" aria-label={title} className={className} style={maskStyle} />;
};
