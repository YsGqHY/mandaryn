import { Skeleton, SkeletonText } from '@/components/Skeleton';
import classes from './LibraryPage.skeleton.module.css';

// LibraryPage 骨架屏：复刻「头部 + 搜索筛选 + 会话列表 + 分页」的版式节奏，
// 避免骨架 → 真实切换时布局跳动。加载占位不出文字，不消费 i18n。
export const LibraryPageSkeleton = () => (
  <div className={classes.root}>
    <div className={classes.body}>
      <div>
        <Skeleton variant="text" width={72} height={12} />
      </div>
      <Skeleton variant="rect" width="45%" height={30} style={{ borderRadius: 4 }} />
      <SkeletonText lines={2} lineHeight={14} gap={10} lastLineWidth="70%" />

      {/* 搜索与筛选 */}
      <div className={classes.card}>
        <Skeleton variant="text" width={110} height={14} />
        <div className={classes.filterGrid}>
          {[0, 1, 2, 3, 4, 5, 6].map((i) => (
            <Skeleton key={i} variant="rect" height={36} style={{ borderRadius: 4 }} />
          ))}
        </div>
      </div>

      {/* 会话列表 */}
      <div className={classes.card}>
        <Skeleton variant="text" width={100} height={14} />
        <div className={classes.rowList}>
          {Array.from({ length: 4 }, (_, i) => (
            <Skeleton key={i} variant="rect" height={72} style={{ borderRadius: 6 }} />
          ))}
        </div>
      </div>
    </div>
  </div>
);
