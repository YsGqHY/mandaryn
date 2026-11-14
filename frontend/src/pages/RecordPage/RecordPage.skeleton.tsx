import { Skeleton, SkeletonText } from '@/components/Skeleton';
import classes from './RecordPage.skeleton.module.css';

// RecordPage 骨架屏：复刻「头部 + 能力提示条 + 录制控制 + 源列表」的版式节奏，
// 避免骨架 → 真实切换时布局跳动。加载占位不出文字，不消费 i18n。
export const RecordPageSkeleton = () => (
  <div className={classes.root}>
    <div className={classes.body}>
      {/* eyebrow + title + subtitle */}
      <div>
        <Skeleton variant="text" width={80} height={12} />
      </div>
      <Skeleton variant="rect" width="45%" height={30} style={{ borderRadius: 4 }} />
      <SkeletonText lines={2} lineHeight={14} gap={10} lastLineWidth="70%" />

      {/* 能力提示条 */}
      <Skeleton variant="rect" height={92} style={{ borderRadius: 8 }} />

      {/* 录制控制 */}
      <div className={classes.card}>
        <Skeleton variant="text" width={120} height={14} />
        <div className={classes.row}>
          <Skeleton variant="rect" width={112} height={36} style={{ borderRadius: 4 }} />
          <Skeleton variant="rect" width={88} height={36} style={{ borderRadius: 4 }} />
          <Skeleton variant="rect" width={88} height={36} style={{ borderRadius: 4 }} />
        </div>
      </div>

      {/* 源列表：两组 */}
      <div className={classes.card}>
        <Skeleton variant="text" width={100} height={14} />
        {[4, 2].map((rows, groupIndex) => (
          <div key={groupIndex} className={classes.listRows}>
            <Skeleton variant="text" width={64} height={11} />
            {Array.from({ length: rows }, (_, i) => (
              <Skeleton key={i} variant="rect" height={52} style={{ borderRadius: 6 }} />
            ))}
          </div>
        ))}
      </div>
    </div>
  </div>
);
