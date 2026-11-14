package models

// 已装包仓库（FR-05-2/3/4、FR-07）：扫描、校验、删除、统计、离线导入，
// 以及 S3 本地引擎需要的 BundleLocator 实现。

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"encoding/json"
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

// 角色取值（ManifestBundle.Roles 的元素）。
const (
	RoleStreaming = "streaming"
	RoleOffline   = "offline"
	RoleVAD       = "vad"
	RoleSpeaker   = "speaker"
)

// BundleLocator 按角色定位已装模型目录。
//
// 为什么是接口：S3 的 local 引擎不应依赖整个仓库域，只需要「给我某一角色的
// 模型目录」；用接口隔离后，引擎侧可以拿假实现单测。
type BundleLocator interface {
	// LocateBundle 返回模型目录绝对路径与 bundleId。
	// 没有可用包时返回 ERR_MODEL_NOT_FOUND（前端据此提示「先下载模型」）。
	LocateBundle(role string) (dir string, bundleID string, err error)
}

// ListInstalled 返回已装包列表（API-6.2）。
func (m *Manager) ListInstalled(_ context.Context) ([]contract.InstalledBundle, error) {
	return m.scanInstalled()
}

// scanInstalled 扫描 `<modelRoot>/models/*` 并解析各自 manifest.json。
//
// 解析失败 / 文件缺失的目录仍然列出（标 corrupt / missing），
// 而不是静默隐藏 —— 否则用户只会看到「占了空间但没有包」。
func (m *Manager) scanInstalled() ([]contract.InstalledBundle, error) {
	if err := m.checkBusy(); err != nil {
		return nil, err
	}
	root, err := m.root()
	if err != nil {
		return nil, apierr.Wrap(err, apierr.CodeInvalidArgument)
	}
	modelsDir := filepath.Join(root, DirModels)
	entries, rerr := os.ReadDir(modelsDir)
	if rerr != nil {
		if os.IsNotExist(rerr) {
			return []contract.InstalledBundle{}, nil
		}
		return nil, apierr.Wrap(rerr, apierr.CodePathUnwritable)
	}

	recorded := m.bundleStateMap()
	out := make([]contract.InstalledBundle, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), DirTrashPrefix) {
			continue
		}
		dirName := e.Name()
		dirPath := filepath.Join(modelsDir, dirName)
		// 对外统一用正斜杠：Path 会进 DB（bundles.path）与前端，跨平台必须稳定。
		relPath := filepath.ToSlash(filepath.Join(DirModels, dirName))

		var b ManifestBundle
		if err := readJSONLimit(filepath.Join(dirPath, FileBundleManifest), maxManifestBytes, &b); err != nil {
			// 无法解析元数据：仍然展示，标记 corrupt 让用户能删掉它。
			out = append(out, contract.InstalledBundle{
				BundleInfo: contract.BundleInfo{BundleID: dirName, Name: dirName},
				Path:       relPath,
				State:      contract.BundleCorrupt,
			})
			continue
		}

		state := contract.BundleInstalled
		for _, f := range b.Files {
			rel, serr := sanitizeRelPath(f.Path)
			if serr != nil {
				state = contract.BundleCorrupt
				break
			}
			if _, statErr := os.Stat(filepath.Join(dirPath, rel)); statErr != nil {
				state = contract.BundleMissing
				break
			}
		}

		// DB 里记录了「上次校验失败」的包保持 corrupt，直到重新校验通过：
		// 否则用户刚点完校验、列表又变回 installed，会以为校验没生效。
		if rec, ok := recorded[bundleDirName(b.BundleID, b.Version)]; ok &&
			rec.state == contract.BundleCorrupt && state == contract.BundleInstalled {
			state = contract.BundleCorrupt
		}

		info := contract.BundleInfo{
			BundleID:  b.BundleID,
			Name:      b.Name,
			Version:   b.Version,
			SizeBytes: dirSize(dirPath),
			RAMHintMB: b.RAMHintMB,
			Languages: b.Languages,
			License:   b.License,
			Roles:     b.Roles,
			SHA256:    b.SHA256,
			Installed: true,
		}
		out = append(out, contract.InstalledBundle{
			BundleInfo:  info,
			Path:        relPath,
			State:       state,
			VerifiedAt:  recorded[bundleDirName(b.BundleID, b.Version)].verifiedAt,
			InstalledAt: modTimeMillis(dirPath),
		})
	}
	return out, nil
}

// installedIndex 返回 bundleId → 最新版本的已装包。
func (m *Manager) installedIndex() (map[string]contract.InstalledBundle, error) {
	list, err := m.scanInstalled()
	best := make(map[string]contract.InstalledBundle, len(list))
	for _, b := range list {
		if cur, ok := best[b.BundleID]; !ok || compareVersion(b.Version, cur.Version) > 0 {
			best[b.BundleID] = b
		}
	}
	return best, err
}

// recordedState 是 bundles 表里的一条状态记录。
type recordedState struct {
	state      string
	verifiedAt int64
}

// bundleStateMap 从 bundles 表读状态与最近校验时间（无库时返回空表）。
func (m *Manager) bundleStateMap() map[string]recordedState {
	out := make(map[string]recordedState)
	db := m.db()
	if db == nil {
		return out
	}
	var rows []storage.Bundle
	if err := db.Select("id", "state", "verified_at").Find(&rows).Error; err != nil {
		return out
	}
	for _, r := range rows {
		out[r.ID] = recordedState{state: r.State, verifiedAt: r.VerifiedAt}
	}
	return out
}

// Verify 重新逐文件哈希校验（API-6.6）。
//
// 这是一次**全量重读**，GB 级模型会明显耗 IO，因此只由用户显式触发
// （设置页 / 下载完成后按需），不在启动时自动跑。
func (m *Manager) Verify(ctx context.Context, bundleID string) (contract.VerifyResult, error) {
	if err := m.checkBusy(); err != nil {
		return contract.VerifyResult{}, err
	}
	root, err := m.root()
	if err != nil {
		return contract.VerifyResult{}, apierr.Wrap(err, apierr.CodeInvalidArgument)
	}
	list, serr := m.scanInstalled()
	if serr != nil {
		return contract.VerifyResult{}, serr
	}
	var target *contract.InstalledBundle
	for i := range list {
		if list[i].BundleID == bundleID {
			if target == nil || compareVersion(list[i].Version, target.Version) > 0 {
				target = &list[i]
			}
		}
	}
	if target == nil {
		return contract.VerifyResult{}, apierr.Newf(apierr.CodeModelNotFound, "bundle %q is not installed", bundleID)
	}

	dir := filepath.Join(root, filepath.FromSlash(target.Path))
	var b ManifestBundle
	if err := readJSONLimit(filepath.Join(dir, FileBundleManifest), maxManifestBytes, &b); err != nil {
		m.markBundleState(ctx, target, contract.BundleCorrupt)
		return contract.VerifyResult{
			BundleID: bundleID,
			OK:       false,
			Note:     "bundle manifest is unreadable",
		}, nil
	}

	bad := verifyDir(dir, b)
	for _, f := range b.Files {
		if err := ctx.Err(); err != nil {
			return contract.VerifyResult{}, apierr.Wrap(err, apierr.CodeStateForbidden)
		}
		_ = f
	}

	if len(bad) > 0 {
		m.markBundleState(ctx, target, contract.BundleCorrupt)
		m.notifyChanged(contract.ModelChanged{BundleID: bundleID, Installed: true, Verified: false})
		return contract.VerifyResult{
			BundleID: bundleID,
			OK:       false,
			BadFiles: bad,
			Note:     fmt.Sprintf("%d file(s) failed SHA-256 verification", len(bad)),
		}, nil
	}

	m.markBundleState(ctx, target, contract.BundleInstalled)
	m.notifyChanged(contract.ModelChanged{BundleID: bundleID, Installed: true, Verified: true})
	return contract.VerifyResult{
		BundleID: bundleID,
		OK:       true,
		Note:     "all files match the manifest",
	}, nil
}

// markBundleState 更新 bundles 表里某包的状态与校验时间。
//
// 行不存在时补插一行：用户可能手工放入 / 离线导入过包，没有经过
// upsertBundle；此时「校验失败」的结论同样必须落下，否则等下次启动
// 列表又显示为正常。
func (m *Manager) markBundleState(ctx context.Context, b *contract.InstalledBundle, state string) {
	db := m.db()
	if db == nil {
		return
	}
	id := bundleDirName(b.BundleID, b.Version)
	updates := map[string]any{"state": state}
	if state == contract.BundleInstalled {
		updates["verified_at"] = time.Now().UnixMilli()
	}
	res := db.WithContext(ctx).Model(&storage.Bundle{}).Where("id = ?", id).Updates(updates)
	if res.Error != nil {
		m.log().Warn("update bundle state failed", "bundle", b.BundleID, "error", res.Error.Error())
		return
	}
	if res.RowsAffected > 0 {
		return
	}
	roles, _ := json.Marshal(b.Roles)
	langs, _ := json.Marshal(b.Languages)
	row := storage.Bundle{
		ID:         id,
		BundleID:   b.BundleID,
		Version:    b.Version,
		Name:       b.Name,
		Path:       filepath.ToSlash(b.Path),
		SizeBytes:  b.SizeBytes,
		SHA256:     b.SHA256,
		VerifiedAt: b.VerifiedAt,
		State:      state,
		Roles:      string(roles),
		Languages:  string(langs),
		License:    b.License,
		RAMHintMB:  b.RAMHintMB,
	}
	if state == contract.BundleInstalled {
		row.VerifiedAt = time.Now().UnixMilli()
	}
	if err := db.WithContext(ctx).Create(&row).Error; err != nil {
		m.log().Warn("insert bundle state failed", "bundle", b.BundleID, "error", err.Error())
	}
}

// Remove 删除已装包（API-6.7）。
func (m *Manager) Remove(ctx context.Context, bundleID string) error {
	if err := m.checkBusy(); err != nil {
		return err
	}
	// 有下载在跑时不允许删除（分片目录与目标目录可能同名）。
	if t := m.findTask(bundleID); t != nil {
		if st := t.snapshot(); st.State == contract.DownloadDownloading || st.State == contract.DownloadQueued {
			return apierr.New(apierr.CodeModelBusy).WithDetail("download in progress")
		}
	}

	root, err := m.root()
	if err != nil {
		return apierr.Wrap(err, apierr.CodeInvalidArgument)
	}
	list, serr := m.scanInstalled()
	if serr != nil {
		return serr
	}
	removed := 0
	for _, b := range list {
		if b.BundleID != bundleID {
			continue
		}
		dir := filepath.Join(root, filepath.FromSlash(b.Path))
		if _, statErr := os.Stat(dir); statErr == nil {
			removeDirAsync(dir)
			removed++
		}
		if db := m.db(); db != nil {
			id := bundleDirName(b.BundleID, b.Version)
			if err := db.WithContext(ctx).Where("id = ?", id).Delete(&storage.Bundle{}).Error; err != nil {
				return apierr.Wrap(err, apierr.CodeDBWrite)
			}
		}
		m.notifyChanged(contract.ModelChanged{BundleID: bundleID, Installed: false, Verified: false})
	}
	if removed == 0 {
		return apierr.Newf(apierr.CodeModelNotFound, "bundle %q is not installed", bundleID)
	}
	m.log().Info("model bundle removed", "bundle", bundleID, "dirs", removed)
	return nil
}

// Stats 返回目录与占用（API-6.8）。
func (m *Manager) Stats(_ context.Context) (contract.StoreStats, error) {
	root, err := m.root()
	if err != nil {
		return contract.StoreStats{}, apierr.Wrap(err, apierr.CodeInvalidArgument)
	}
	bundles, _ := m.scanInstalled()
	var total int64
	for _, b := range bundles {
		total += b.SizeBytes
	}
	return contract.StoreStats{
		Root:           root,
		TotalBytes:     total,
		DownloadsBytes: dirSize(filepath.Join(root, DirDownloads)),
		Bundles:        bundles,
	}, nil
}

// LocateBundle 实现 BundleLocator：按角色找已装且状态正常的包。
func (m *Manager) LocateBundle(role string) (string, string, error) {
	if err := m.checkBusy(); err != nil {
		return "", "", err
	}
	root, err := m.root()
	if err != nil {
		return "", "", apierr.Wrap(err, apierr.CodeInvalidArgument)
	}
	list, serr := m.scanInstalled()
	if serr != nil {
		return "", "", serr
	}
	var best *contract.InstalledBundle
	for i := range list {
		b := list[i]
		if b.State != contract.BundleInstalled || !hasRole(b.Roles, role) {
			continue
		}
		if best == nil || compareVersion(b.Version, best.Version) > 0 {
			best = &list[i]
		}
	}
	if best == nil {
		return "", "", apierr.Newf(apierr.CodeModelNotFound,
			"no installed bundle provides role %q", role)
	}
	return filepath.Join(root, filepath.FromSlash(best.Path)), best.BundleID, nil
}

func hasRole(roles []string, role string) bool {
	for _, r := range roles {
		if strings.EqualFold(r, role) {
			return true
		}
	}
	return false
}

// upsertBundle 把包写入 bundles 表（TBL-08）。
func (m *Manager) upsertBundle(b ManifestBundle, relPath, state string) {
	db := m.db()
	if db == nil {
		return
	}
	roles, _ := json.Marshal(b.Roles)
	langs, _ := json.Marshal(b.Languages)
	row := storage.Bundle{
		ID:          bundleDirName(b.BundleID, b.Version),
		BundleID:    b.BundleID,
		Version:     b.Version,
		Name:        b.Name,
		Path:        filepath.ToSlash(relPath),
		SizeBytes:   totalBytesOf(b),
		SHA256:      b.SHA256,
		VerifiedAt:  time.Now().UnixMilli(),
		State:       state,
		Roles:       string(roles),
		Languages:   string(langs),
		License:     b.License,
		RAMHintMB:   b.RAMHintMB,
	}
	updates := map[string]any{
		"bundle_id":    row.BundleID,
		"version":      row.Version,
		"name":         row.Name,
		"path":         row.Path,
		"size_bytes":   row.SizeBytes,
		"sha256":       row.SHA256,
		"verified_at":  row.VerifiedAt,
		"state":        row.State,
		"roles":        row.Roles,
		"languages":    row.Languages,
		"license":      row.License,
		"ram_hint_mb":  row.RAMHintMB,
	}
	res := db.Model(&storage.Bundle{}).Where("id = ?", row.ID).Updates(updates)
	if res.Error != nil {
		m.log().Warn("upsert bundle row failed", "bundle", b.BundleID, "error", res.Error.Error())
		return
	}
	if res.RowsAffected == 0 {
		if err := db.Create(&row).Error; err != nil {
			m.log().Warn("insert bundle row failed", "bundle", b.BundleID, "error", err.Error())
		}
	}
}

// commitDir 把下载 / 导入的工作目录原子搬到最终位置。
//
// 目标已存在（重下或升级）时先改名让位：Windows 上 os.Rename 到已存在的目录
// 会直接失败，不能靠「覆盖」语义（见 docs/DEV-ISSUES-S6.md）。
func commitDir(src, target string) error {
	if _, err := os.Stat(target); err == nil {
		trash := filepath.Join(filepath.Dir(target),
			fmt.Sprintf("%s%s-%d", DirTrashPrefix, filepath.Base(target), time.Now().UnixNano()))
		if err := os.Rename(target, trash); err != nil {
			return fmt.Errorf("models: move existing bundle aside: %w", err)
		}
		go func() { _ = os.RemoveAll(trash) }()
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	return os.Rename(src, target)
}

// ImportOffline 导入离线包（API-6.13 / UC-08 内网部署）。
func (m *Manager) ImportOffline(ctx context.Context, archivePath string) (contract.InstalledBundle, error) {
	if err := m.checkBusy(); err != nil {
		return contract.InstalledBundle{}, err
	}
	if err := ctx.Err(); err != nil {
		return contract.InstalledBundle{}, apierr.Wrap(err, apierr.CodeStateForbidden)
	}
	if _, err := os.Stat(archivePath); err != nil {
		return contract.InstalledBundle{}, apierr.Newf(apierr.CodeNotFound,
			"archive not found: %s", filepath.Base(archivePath))
	}
	root, err := m.root()
	if err != nil {
		return contract.InstalledBundle{}, apierr.Wrap(err, apierr.CodeInvalidArgument)
	}

	staging := filepath.Join(root, DirStagingPrefix+newTempID())
	if err := filex.EnsureDir(staging); err != nil {
		return contract.InstalledBundle{}, apierr.Wrap(err, apierr.CodePathUnwritable)
	}
	// 成功时 manifest 目录已被 rename 走，这里只清理残余。
	defer func() { _ = os.RemoveAll(staging) }()

	name := strings.ToLower(filepath.Base(archivePath))
	switch {
	case strings.HasSuffix(name, ".zip"):
		err = extractZip(archivePath, staging)
	case strings.HasSuffix(name, ".tar.gz"), strings.HasSuffix(name, ".tgz"):
		err = extractTarGz(archivePath, staging)
	default:
		return contract.InstalledBundle{}, apierr.New(apierr.CodeInvalidArgument).
			WithDetail("unsupported archive type: only .zip and .tar.gz are supported")
	}
	if err != nil {
		return contract.InstalledBundle{}, apierr.New(apierr.CodeModelCorrupt).
			WithDetail("extract archive failed: %s", err.Error()).WithCause(err)
	}

	manifestDir, bundle, ferr := findBundleManifest(staging)
	if ferr != nil {
		return contract.InstalledBundle{}, apierr.New(apierr.CodeModelCorrupt).
			WithDetail("archive has no readable bundle manifest: %s", ferr.Error()).WithCause(ferr)
	}
	if bad := verifyDir(manifestDir, bundle); len(bad) > 0 {
		return contract.InstalledBundle{}, apierr.Newf(apierr.CodeModelCorrupt,
			"archive verification failed for %d file(s): %s", len(bad), strings.Join(bad, ", "))
	}

	target := filepath.Join(root, DirModels, bundleDirName(bundle.BundleID, bundle.Version))
	if err := commitDir(manifestDir, target); err != nil {
		return contract.InstalledBundle{}, apierr.Wrap(err, apierr.CodePathUnwritable)
	}
	m.upsertBundle(bundle, filepath.Join(DirModels, filepath.Base(target)), contract.BundleInstalled)
	m.notifyChanged(contract.ModelChanged{BundleID: bundle.BundleID, Installed: true, Verified: true})
	m.log().Info("offline bundle imported", "bundle", bundle.BundleID, "version", bundle.Version)

	return contract.InstalledBundle{
		BundleInfo: contract.BundleInfo{
			BundleID:  bundle.BundleID,
			Name:      bundle.Name,
			Version:   bundle.Version,
			SizeBytes: dirSize(target),
			RAMHintMB: bundle.RAMHintMB,
			Languages: bundle.Languages,
			License:   bundle.License,
			Roles:     bundle.Roles,
			SHA256:    bundle.SHA256,
			Installed: true,
		},
		Path:        filepath.ToSlash(filepath.Join(DirModels, filepath.Base(target))),
		State:       contract.BundleInstalled,
		VerifiedAt:  time.Now().UnixMilli(),
		InstalledAt: time.Now().UnixMilli(),
	}, nil
}

// findBundleManifest 在解压目录里定位 manifest.json（根目录或唯一一级子目录）。
func findBundleManifest(dir string) (string, ManifestBundle, error) {
	var bundle ManifestBundle
	direct := filepath.Join(dir, FileBundleManifest)
	if _, err := os.Stat(direct); err == nil {
		if err := readJSONLimit(direct, maxManifestBytes, &bundle); err != nil {
			return "", bundle, fmt.Errorf("manifest.json is unreadable: %w", err)
		}
		return dir, bundle, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", bundle, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		candidate := filepath.Join(dir, e.Name(), FileBundleManifest)
		if _, statErr := os.Stat(candidate); statErr != nil {
			continue
		}
		if err := readJSONLimit(candidate, maxManifestBytes, &bundle); err != nil {
			return "", bundle, fmt.Errorf("manifest.json is unreadable: %w", err)
		}
		return filepath.Join(dir, e.Name()), bundle, nil
	}
	return "", bundle, fmt.Errorf("archive has no %s", FileBundleManifest)
}

// extractZip 解压 zip（拒绝路径穿越与符号链接）。
func extractZip(archive, dest string) error {
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer zr.Close()

	for _, f := range zr.File {
		rel, perr := sanitizeRelPath(f.Name)
		if perr != nil {
			return fmt.Errorf("zip entry %q: %w", f.Name, perr)
		}
		target := filepath.Join(dest, rel)
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		// 只接受普通文件：符号链接 / 设备文件在离线包里没有正当用途。
		if mode := f.Mode(); !mode.IsRegular() {
			return fmt.Errorf("zip entry %q has unsupported mode %s", f.Name, mode)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, oerr := f.Open()
		if oerr != nil {
			return oerr
		}
		werr := writeStream(target, rc)
		_ = rc.Close()
		if werr != nil {
			return werr
		}
	}
	return nil
}

// extractTarGz 解压 tar.gz（同样拒绝路径穿越与链接条目）。
func extractTarGz(archive, dest string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()

	gzr, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)
	for {
		hdr, nerr := tr.Next()
		if nerr == io.EOF {
			return nil
		}
		if nerr != nil {
			return nerr
		}
		rel, perr := sanitizeRelPath(hdr.Name)
		if perr != nil {
			return fmt.Errorf("tar entry %q: %w", hdr.Name, perr)
		}
		target := filepath.Join(dest, rel)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := writeStream(target, io.LimitReader(tr, hdr.Size)); err != nil {
				return err
			}
		default:
			return fmt.Errorf("tar entry %q has unsupported type %d", hdr.Name, hdr.Typeflag)
		}
	}
}

// writeStream 把流写进目标文件（不带 O_APPEND，覆盖语义）。
func writeStream(target string, r io.Reader) error {
	f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// modTimeMillis 返回目录修改时间（ms），用作 InstalledAt。
func modTimeMillis(p string) int64 {
	info, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return info.ModTime().UnixMilli()
}
