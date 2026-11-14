import {
  IconLibrary,
  IconMicrophone,
  IconPackage,
  IconSettings,
} from '@tabler/icons-react';
import { lazy } from 'react';
import { LibraryPageSkeleton } from '@/pages/LibraryPage/LibraryPage.skeleton';
import { ModelsPageSkeleton } from '@/pages/ModelsPage/ModelsPage.skeleton';
import { RecordPageSkeleton } from '@/pages/RecordPage/RecordPage.skeleton';
import { SettingsPageSkeleton } from '@/pages/SettingsPage/SettingsPage.skeleton';
import type { RouteDefinition } from '@/router';

// SettingsPage 走 React.lazy：调色板编辑器较重，首屏不进入 main bundle。
// Record / Models 走 lazy：都带有事件订阅与大块列表，首屏用不到。
//
// 注意：lazy import 必须从具体子文件 (SettingsPage.tsx) 导入，
// 不要从 index.ts 走 —— 否则与 App.tsx 静态 import 同 index 合并到主 chunk。
const SettingsPage = lazy(async () => {
  const mod = await import('@/pages/SettingsPage/SettingsPage');
  return { default: mod.SettingsPage };
});

const RecordPage = lazy(async () => {
  const mod = await import('@/pages/RecordPage/RecordPage');
  return { default: mod.RecordPage };
});

const ModelsPage = lazy(async () => {
  const mod = await import('@/pages/ModelsPage/ModelsPage');
  return { default: mod.ModelsPage };
});

const LibraryPage = lazy(async () => {
  const mod = await import('@/pages/LibraryPage/LibraryPage');
  return { default: mod.LibraryPage };
});

// 路由表是唯一事实源：
// - 新增页面：在这里追加一项即可，Sidebar 会自动渲染对应导航按钮
// - id 必须唯一；slot 决定 Sidebar 把它渲染到顶部还是底部
// - 想要不出现在 Sidebar 但仍可 navigate(id) 进入的页面，设 slot: 'hidden'
// - keepAlive 默认 false（离开即卸载）；若想保留输入 / 滚动 / 局部 state，设 true
// - fallback：lazy 组件加载期间的占位（推荐传该页面的骨架屏组件）
// - labelKey：i18n 路径（约定 'route.<id>'），渲染时 t(labelKey) 翻译；
//   label 是兜底字面量（未配置 labelKey 或翻译失败时使用）
export const routes: RouteDefinition[] = [
  {
    id: 'record',
    labelKey: 'route.record',
    label: 'Record',
    icon: IconMicrophone,
    element: <RecordPage />,
    fallback: <RecordPageSkeleton />,
    slot: 'primary',
  },
  {
    id: 'library',
    labelKey: 'route.library',
    label: 'Library',
    icon: IconLibrary,
    element: <LibraryPage />,
    fallback: <LibraryPageSkeleton />,
    slot: 'primary',
  },
  {
    id: 'models',
    labelKey: 'route.models',
    label: 'Models',
    icon: IconPackage,
    element: <ModelsPage />,
    fallback: <ModelsPageSkeleton />,
    slot: 'primary',
  },
  {
    id: 'settings',
    labelKey: 'route.settings',
    label: 'Settings',
    icon: IconSettings,
    element: <SettingsPage />,
    fallback: <SettingsPageSkeleton />,
    slot: 'footer',
  },
];
