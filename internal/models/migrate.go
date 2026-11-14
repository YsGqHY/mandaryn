package models

// 模型根迁移状态机（FR-07-3/4/5 / ARCH §4.6.3）。
//
// 六阶段：preflight → copying → verifying → switching → cleaning，
// 任一步失败或用户取消 → rollback（恢复旧 modelRoot，旧路径保持可用）。
//
// 一致性保障：
//  1. 全局互斥：迁移期间下载 / 加载 / 删除一律 ERR_MODEL_BUSY（migrating 标志）；
//  2. 原子切换：DB 值与目录 rename 配对，rename 失败即回写旧值；
//  3. 不删源直到目标校验通过（copy→verify→switch→clean 顺序不可换）；
//  4. 网络盘：警告 network-path 并降级为「复制模式（不删源）」。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/storage"
	"vip.oberon.mandaryn/internal/utils/filex"
)

// 迁移注意事项里的固定键（前端按 i18n 映射文案）。
const (
	// WarnNetworkPath 目标位于网络 / 跨设备位置，原子性不保证。
	WarnNetworkPath = "network-path"
	// WarnCopyOnly 复制模式：源目录保留（双份共存）。
	WarnCopyOnly = "copy-only"
)

// migrateCopySkip 复制时跳过的目录（临时区与待删区）。
func shouldSkipEntry(name string) bool {
	return strings.HasPrefix(name, DirTrashPrefix) || strings.HasPrefix(name, DirStagingPrefix)
}

// PlanMigrate 预检并给出迁移计划（API-6.9，**不执行**）。
func (m *Manager) PlanMigrate(ctx context.Context, target string) (contract.MigratePlan, error) {
	from, err := m.root()
	if err != nil {
		return contract.MigratePlan{}, apierr.Wrap(err, apierr.CodeMigratePrecheck)
	}
	target = strings.TrimSpace(target)
	if target == "" {
		return contract.MigratePlan{}, apierr.New(apierr.CodeMigratePrecheck).
			WithDetail("target directory is empty")
	}
	abs, aerr := filepath.Abs(expandUserPath(target))
	if aerr != nil {
		return contract.MigratePlan{}, apierr.New(apierr.CodeMigratePrecheck).
			WithDetail("target path is invalid").WithCause(aerr)
	}
	abs = filepath.Clean(abs)

	plan := contract.MigratePlan{From: from, To: abs}

	if samePath(from, abs) {
		return plan, apierr.New(apierr.CodeMigratePrecheck).
			WithDetail("target equals the current model root")
	}
	// 目标不能是源的子目录（否则复制会把复制结果再复制进自己）。
	if strings.HasPrefix(abs+string(os.PathSeparator), from+string(os.PathSeparator)) || abs == from {
		return plan, apierr.New(apierr.CodeMigratePrecheck).
			WithDetail("target must not be inside the current model root")
	}
	// 源也不能是目标的子目录：staging 会落在目标里，而目标又包含源，
	// 切换阶段把 staging 内容搬进目标时可能覆盖源本身，语义混乱。
	if strings.HasPrefix(from+string(os.PathSeparator), abs+string(os.PathSeparator)) {
		return plan, apierr.New(apierr.CodeMigratePrecheck).
			WithDetail("current model root must not be inside the target")
	}

	// 目标可写（`.mandaryn-write-test` 探针，ARCH §4.6.3）。
	if werr := probeWritableDir(abs); werr != nil {
		return plan, apierr.New(apierr.CodeMigratePrecheck).
			WithDetail("target directory is not writable").WithCause(werr)
	}

	plan.TotalBytes = sourceBytes(from)
	plan.BundleCount = countBundles(from)
	free, ferr := freeBytes(abs)
	if ferr == nil {
		plan.FreeBytes = free
	}

	if isNetworkPath(abs) {
		plan.Warnings = append(plan.Warnings, WarnNetworkPath)
		plan.Warnings = append(plan.Warnings, WarnCopyOnly)
	}

	plan.Feasible = true
	if ferr == nil && free > 0 {
		need := int64(float64(plan.TotalBytes) * diskOverheadFactor)
		if free < need {
			plan.Feasible = false
			return plan, apierr.Newf(apierr.CodeMigratePrecheck,
				"target needs %d bytes free, only %d available", need, free)
		}
	}
	_ = ctx
	return plan, nil
}

// RunMigration 执行迁移（API-6.10）。
//
// preflight 同步完成（不可行时立刻返回错误，无副作用），copy/verify/switch/clean
// 在后台执行，进度经 OnMigrate 事件 + GetMigrationState 轮询两条路径对外。
func (m *Manager) RunMigration(ctx context.Context, req contract.MigrateRequest) (contract.MigrateState, error) {
	if !m.migrating.CompareAndSwap(false, true) {
		return m.MigrationState(ctx), apierr.New(apierr.CodeModelBusy).
			WithDetail("another migration is already running")
	}

	plan, err := m.PlanMigrate(ctx, req.Target)
	if err != nil {
		// 预检失败必须写进对外状态并广播：否则 GetMigrationState（API-6.12 的
		// 轮询兜底）会返回上一次迁移留下的 done/idle，前端据此认为「迁移成功」。
		m.migrating.Store(false)
		m.setMigratePhase(contract.MigrateFailed, err)
		return contract.MigrateState{Phase: contract.MigrateFailed, LastError: errDTO(err)}, err
	}

	copyOnly := req.CopyOnly || containsString(plan.Warnings, WarnNetworkPath)

	// 刻意**不**继承调用方 ctx：Wails 的一次 RPC 结束不该把搬迁中断在半路，
	// 只有 CancelMigration（或进程退出）才该中止。
	runCtx, cancel := context.WithCancel(context.Background())
	m.migMu.Lock()
	m.migCancel = cancel
	m.migState = contract.MigrateState{
		Phase:      contract.MigratePreflight,
		BytesTotal: plan.TotalBytes,
	}
	m.broadcastMigrateLocked()
	state := m.migState
	m.migMu.Unlock()

	// preflight 已完成，对外先发一次，让 UI 立刻进入「迁移中」形态。
	if m.opts.OnMigrate != nil {
		m.opts.OnMigrate(state)
	}

	go m.runMigration(runCtx, plan, copyOnly)
	return state, nil
}

// runMigration 是后台迁移主体。
func (m *Manager) runMigration(ctx context.Context, plan contract.MigratePlan, copyOnly bool) {
	defer m.migrating.Store(false)
	defer func() {
		m.migMu.Lock()
		m.migCancel = nil
		m.migMu.Unlock()
	}()

	staging := filepath.Join(plan.To, DirStagingPrefix+newTempID())

	// rollback 回到旧根。userCancel 为真表示是用户主动取消（而不是出错），
	// 此时不写 LastError：前端据此把结果读作「已取消」而不是「失败」。
	rollback := func(cause error, code string, userCancel bool) {
		_ = os.RemoveAll(staging)
		var phaseCause error
		if !userCancel {
			phaseCause = cause
		}
		m.setMigratePhase(contract.MigrateRollback, phaseCause)
		// 恢复旧值：DB 可能已被 switching 阶段改过。
		if rerr := m.persistRoot(ctx, plan.From); rerr != nil {
			m.log().Warn("restore model root failed", "error", rerr.Error())
		}
		if m.opts.OnModelRootChanged != nil {
			if rerr := m.opts.OnModelRootChanged(plan.From); rerr != nil {
				m.log().Warn("restore model root callback failed", "error", rerr.Error())
			}
		}
		m.rootOverride.Store(plan.From)
		m.log().Warn("migration rolled back", "code", code, "canceled", userCancel, "error", cause.Error())
		// 终态是 rollback（DOC-02 §4.6.3 的失败收敛态）：出错时 LastError 说明原因，
		// 用户取消时留空，避免前端把「取消」显示成「失败」。
		m.migMu.Lock()
		if userCancel {
			m.migState.LastError = nil
		} else {
			m.migState.LastError = phaseErrDTO(cause, code)
		}
		m.broadcastMigrateLocked()
		m.migMu.Unlock()
	}

	// —— copying ——
	m.setMigratePhase(contract.MigrateCopying, nil)
	hashes, _, err := copyTree(ctx, plan.From, staging, func(done, total int64, current string) {
		m.setMigrateProgress(done, total, current)
	})
	if err != nil {
		code := apierr.CodeMigrateVerify
		if isUserCancel(err) {
			code = ""
		}
		rollback(err, code, isUserCancel(err))
		return
	}

	// —— verifying ——
	m.setMigratePhase(contract.MigrateVerifying, nil)
	if bad := verifyCopied(staging, hashes); len(bad) > 0 {
		rollback(fmt.Errorf("verification failed for %d file(s)", len(bad)), apierr.CodeMigrateVerify, false)
		return
	}

	// 校验完成到切换之间是**最后一个安全取消点**：此时源目录完好、什么都没改，
	// 在这里尊重用户的取消请求，比「已经点了取消却把目录切走」更符合预期。
	if cerr := ctx.Err(); cerr != nil {
		rollback(cerr, "", true)
		return
	}

	// —— switching ——
	m.setMigratePhase(contract.MigrateSwitching, nil)
	if err := m.switchRoot(ctx, plan, staging); err != nil {
		rollback(err, apierr.CodeMigrateSwitch, false)
		return
	}
	// 阶段切换成功后，本包立刻使用新根（上层解析器缓存由回调负责失效）。
	m.rootOverride.Store(plan.To)

	// —— cleaning ——
	m.setMigratePhase(contract.MigrateCleaning, nil)
	switch {
	case copyOnly:
		m.log().Info("migration kept the source directory (copy-only mode)", "from", plan.From)
	case ctx.Err() != nil:
		// 已切换成功后再取消：不再冒险搬回去，但**保留源目录**（双份共存），
		// 让用户自己决定删哪一份；宁可多占空间，也不能删掉仍是唯一可用副本的数据。
		m.log().Info("migration kept the source directory (canceled during cleanup)", "from", plan.From)
	default:
		removeDirAsync(plan.From)
	}

	m.setMigratePhase(contract.MigrateDone, nil)
	m.log().Info("model root migrated", "from", plan.From, "to", plan.To, "copyOnly", copyOnly)
}

// switchRoot 写 app_settings.model_root 并把 staging 目录改名到位。
//
// 顺序遵循文档：先落库再 rename；rename 失败则把库里的值回写旧值，
// 保证「DB 指向的目录一定存在」（ARCH §4.6.3 一致性保障 2）。
func (m *Manager) switchRoot(ctx context.Context, plan contract.MigratePlan, staging string) error {
	if err := filex.EnsureDir(plan.To); err != nil {
		return err
	}
	if err := m.persistRoot(ctx, plan.To); err != nil {
		return err
	}

	// staging 内是模型根的内容（models/ 与 manifest.cache.json 等），
	// 逐个把顶层条目搬到目标根下。
	entries, err := os.ReadDir(staging)
	if err != nil {
		_ = m.persistRoot(ctx, plan.From)
		return err
	}
	// moved 记录本阶段已搬进目标的条目：中途失败时逐个搬回 staging，
	// 由调用方删掉整个 staging —— 否则目标目录会留下「一半迁移过的」残骸，
	// 下次迁移还得靠改名让位去覆盖它（FR-07-4「不产生两处都不完整」）。
	moved := make([]string, 0, len(entries))
	restore := func() {
		for i := len(moved) - 1; i >= 0; i-- {
			name := moved[i]
			if rerr := os.Rename(filepath.Join(plan.To, name), filepath.Join(staging, name)); rerr != nil {
				m.log().Warn("restore moved entry failed", "entry", name, "error", rerr.Error())
			}
		}
	}
	for _, e := range entries {
		src := filepath.Join(staging, e.Name())
		dst := filepath.Join(plan.To, e.Name())
		if _, statErr := os.Stat(dst); statErr == nil {
			// 目标同名条目已存在：改名让位后原子替换。
			trash := filepath.Join(plan.To,
				fmt.Sprintf("%s%s-%d", DirTrashPrefix, e.Name(), time.Now().UnixNano()))
			if rerr := os.Rename(dst, trash); rerr != nil {
				_ = m.persistRoot(ctx, plan.From)
				restore()
				return rerr
			}
			go func() { _ = os.RemoveAll(trash) }()
		}
		if rerr := os.Rename(src, dst); rerr != nil {
			_ = m.persistRoot(ctx, plan.From)
			restore()
			return rerr
		}
		moved = append(moved, e.Name())
	}
	_ = os.RemoveAll(staging)

	if m.opts.OnModelRootChanged != nil {
		if err := m.opts.OnModelRootChanged(plan.To); err != nil {
			m.log().Warn("invalidate paths cache failed", "error", err.Error())
		}
	}
	return nil
}

// isUserCancel 判断错误是否来自用户取消（CancelMigration 取消 runCtx）。
func isUserCancel(err error) bool {
	return errors.Is(err, context.Canceled)
}

// phaseErrDTO 把回滚原因投影成 DTO，并**强制使用阶段错误码**。
//
// 为什么不能让底层码透传：切换阶段的失败往往来自写库（ERR_DB_WRITE）或
// rename（裸 error），原样透传会让前端落到「未知错误」文案，用户读不到
// 「切换失败、已回滚到原目录」这个真正有用的结论。原因仍留在 message 里。
func phaseErrDTO(cause error, code string) *contract.ErrorDTO {
	dto := errDTO(apierr.From(cause, code))
	if dto == nil || code == "" {
		return dto
	}
	dto.Code = code
	return dto
}

// CancelMigration 取消并回滚（API-6.11）。
func (m *Manager) CancelMigration(_ context.Context) error {
	m.migMu.RLock()
	cancel := m.migCancel
	m.migMu.RUnlock()
	if cancel == nil {
		return apierr.New(apierr.CodeNotFound).WithDetail("no migration is running")
	}
	cancel()
	return nil
}

// MigrationState 返回当前迁移状态（API-6.12 轮询兜底）。
func (m *Manager) MigrationState(_ context.Context) contract.MigrateState {
	m.migMu.RLock()
	defer m.migMu.RUnlock()
	return m.migState
}

// AwaitMigration 阻塞直到迁移结束（单测与退出前收尾用）。
func (m *Manager) AwaitMigration(ctx context.Context) contract.MigrateState {
	for {
		st := m.MigrationState(ctx)
		terminal := st.Phase == contract.MigrateDone || st.Phase == contract.MigrateRollback ||
			st.Phase == contract.MigrateFailed || st.Phase == contract.MigrateIdle
		if terminal && !m.migrating.Load() {
			return st
		}
		m.migMu.RLock()
		ch := m.migChanged
		m.migMu.RUnlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return m.MigrationState(ctx)
		case <-time.After(time.Second):
		}
	}
}

// —— 状态与广播 ——

func (m *Manager) setMigratePhase(phase string, cause error) {
	m.migMu.Lock()
	m.migState.Phase = phase
	if cause != nil {
		m.migState.LastError = errDTO(cause)
	}
	m.broadcastMigrateLocked()
	st := m.migState
	m.migMu.Unlock()

	if m.opts.OnMigrate != nil {
		m.opts.OnMigrate(st)
	}
}

func (m *Manager) setMigrateProgress(done, total int64, current string) {
	m.migMu.Lock()
	m.migState.BytesDone = done
	if total > 0 {
		m.migState.BytesTotal = total
	}
	m.migState.Current = current
	m.broadcastMigrateLocked()
	st := m.migState
	m.migMu.Unlock()

	if m.opts.OnMigrate != nil {
		m.opts.OnMigrate(st)
	}
}

// broadcastMigrateLocked 关闭并替换广播通道（唤醒 AwaitMigration）。
// 调用方必须持写锁。
func (m *Manager) broadcastMigrateLocked() {
	close(m.migChanged)
	m.migChanged = make(chan struct{})
}

// persistRoot 把 modelRoot 用户值写入 app_settings（单事务单行更新）。
func (m *Manager) persistRoot(ctx context.Context, root string) error {
	db := m.db()
	if db == nil {
		return nil
	}
	if err := db.WithContext(ctx).Model(&storage.AppSettings{}).Where("id = ?", 1).
		Update("model_root", root).Error; err != nil {
		return apierr.Wrap(err, apierr.CodeDBWrite)
	}
	return nil
}

// —— 复制与校验 ——

// copyTree 把 src 内容流式复制到 dst，返回每个文件的 sha256。
//
// 复制阶段就记录源文件摘要：verifying 阶段重算目标摘要与之比对，
// 这样校验不依赖包内 manifest（迁移的是整个模型根，包含未登记的缓存文件）。
func copyTree(ctx context.Context, src, dst string, progress func(done, total int64, current string)) (map[string]string, int64, error) {
	hashes := make(map[string]string)
	total := sourceBytes(src)

	if err := filex.EnsureDir(dst); err != nil {
		return nil, 0, err
	}

	var done int64
	err := filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		rel, rerr := filepath.Rel(src, p)
		if rerr != nil {
			return rerr
		}
		if rel == "." {
			return nil
		}
		// 临时区 / 待删区不参与迁移。
		for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
			if shouldSkipEntry(part) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			// 符号链接等不作为模型资产迁移（避免把绝对路径带过去）。
			return nil
		}
		sum, n, cerr := copyFileHashed(p, target)
		if cerr != nil {
			return cerr
		}
		hashes[filepath.ToSlash(rel)] = sum
		done += n
		if progress != nil {
			progress(done, total, filepath.ToSlash(rel))
		}
		return nil
	})
	return hashes, done, err
}

// copyFileHashed 复制单文件并同时算源文件与目标文件的 sha256 校验。
func copyFileHashed(src, dst string) (string, int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", 0, err
	}
	defer in.Close()

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", 0, err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return "", 0, err
	}
	n, cerr := io.Copy(out, in)
	if cerr != nil {
		_ = out.Close()
		return "", n, cerr
	}
	if serr := out.Sync(); serr != nil {
		_ = out.Close()
		return "", n, serr
	}
	if cerr := out.Close(); cerr != nil {
		return "", n, cerr
	}
	return sha256File(src)
}

// verifyCopied 重算目标摘要并与源摘要比对，返回不一致的相对路径。
func verifyCopied(dst string, hashes map[string]string) []string {
	var bad []string
	for rel, want := range hashes {
		got, _, err := sha256File(filepath.Join(dst, filepath.FromSlash(rel)))
		if err != nil || !strings.EqualFold(got, want) {
			bad = append(bad, rel)
		}
	}
	return bad
}

// probeWritableDir 建目录 + 写探针文件 + 删除（与 paths 层同一口径）。
func probeWritableDir(dir string) error {
	if err := filex.EnsureDir(dir); err != nil {
		return err
	}
	probe := filepath.Join(dir, ".mandaryn-write-test")
	if err := filex.WriteAtomic(probe, []byte("ok"), 0o644); err != nil {
		return err
	}
	return os.Remove(probe)
}

// sourceBytes 统计模型根占用（跳过临时区与待删区）。
func sourceBytes(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != root && shouldSkipEntry(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if info, statErr := d.Info(); statErr == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

// countBundles 数已装包目录数。
func countBundles(root string) int {
	entries, err := os.ReadDir(filepath.Join(root, DirModels))
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() && !shouldSkipEntry(e.Name()) {
			n++
		}
	}
	return n
}

// isNetworkPath 判断路径是否位于网络位置。
func isNetworkPath(p string) bool {
	if strings.TrimSpace(p) == "" {
		return false
	}
	if strings.HasPrefix(strings.ReplaceAll(p, `\`, "/"), "//") {
		return true
	}
	return isNetworkMount(p)
}

// samePath 比较两个路径是否同一位置（大小写与分隔符归一，Windows 不敏感）。
func samePath(a, b string) bool {
	aa, err1 := filepath.Abs(a)
	bb, err2 := filepath.Abs(b)
	if err1 != nil || err2 != nil {
		return a == b
	}
	aa = filepath.Clean(aa)
	bb = filepath.Clean(bb)
	if aa == bb {
		return true
	}
	if isWindows() {
		return strings.EqualFold(aa, bb)
	}
	return false
}

// expandUserPath 展开 `~` 前缀（用户可能手输 ~/models）。
func expandUserPath(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		home, err := os.UserHomeDir()
		if err == nil {
			if p == "~" {
				return home
			}
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
