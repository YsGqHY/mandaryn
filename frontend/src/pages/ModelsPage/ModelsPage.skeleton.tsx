import { Skeleton, SkeletonText } from '@/components/Skeleton';
import classes from './ModelsPage.skeleton.module.css';

// ModelsPage 骨架屏：复刻「头部 + 仓库概览 + 已装列表 + 可下载列表」的版式节奏。
// 加载占位不出文字，不消费 i18n。
export const ModelsPageSkeleton = () => (
  <div className={classes.root}>
    <div className={classes.body}>
      <div>
        <Skeleton variant="text" width={72} height={12} />
      </div>
      <Skeleton variant="rect" width="45%" height={30} style={{ borderRadius: 4 }} />
      <SkeletonText lines={2} lineHeight={14} gap={10} lastLineWidth="70%" />

      {/* 仓库概览 */}
      <div className={classes.card}>
        <Skeleton variant="text" width={120} height={14} />
        <div className={classes.statGrid}>
          {[0, 1, 2, 3].map((i) => (
            <Skeleton key={i} variant="rect" height={40} style={{ borderRadius: 6 }} />
          ))}
        </div>
      </div>

      {/* 已装 / 可下载两个列表 */}
      {[2, 3].map((rows, cardIndex) => (
        <div key={cardIndex} className={classes.card}>
          <Skeleton variant="text" width={110} height={14} />
          <div className={classes.rowList}>
            {Array.from({ length: rows }, (_, i) => (
              <Skeleton key={i} variant="rect" height={64} style={{ borderRadius: 6 }} />
            ))}
          </div>
        </div>
      ))}
    </div>
  </div>
);
