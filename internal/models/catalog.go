package models

// 清单（catalog）获取与缓存（FR-05-1 / ARCH §4.6.2 第 1 步）。
//
// 三级回退：远端主基址 → 备用镜像（最多 3 个）→ 本地 manifest.cache.json →
// 代码内置兜底清单。**任何一级都不能让 UI 拿到空列表**，否则用户会以为
// 应用坏了；远程失败时按 DOC-03 返回 ERR_DOWNLOAD_NETWORK 但**同时**返回
// 可用的清单内容。

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/outbound"
	"vip.oberon.mandaryn/internal/utils/httpx"
)

// Manifest 是模型清单（远端 JSON / manifest.cache.json 同构）。
type Manifest struct {
	// SchemaVersion 清单格式版本；不匹配时视为无效（防止旧格式误解析）。
	SchemaVersion int `json:"schemaVersion"`
	// GeneratedAt 远端生成时间（展示用）。
	GeneratedAt string `json:"generatedAt,omitempty"`
	// FetchedAt 本地写入缓存的时间（ms）。
	FetchedAt int64 `json:"fetchedAt,omitempty"`
	// Source 拉取来源（只存 scheme://host，不含 query / 凭据）。
	Source string `json:"source,omitempty"`
	// Bundles 可下载的模型包。
	Bundles []ManifestBundle `json:"bundles"`
}

// ManifestBundle 是清单里的一个模型包。
type ManifestBundle struct {
	BundleID  string   `json:"bundleId"`
	Name      string   `json:"name"`
	Version   string   `json:"version"`
	SizeBytes int64    `json:"sizeBytes,omitempty"`
	RAMHintMB int      `json:"ramHintMb,omitempty"`
	Languages []string `json:"languages,omitempty"`
	License   string   `json:"license,omitempty"`
	// Roles streaming | offline | vad | speaker。
	Roles []string `json:"roles,omitempty"`
	// SHA256 整包摘要（可选；有则安装后复核）。
	SHA256 string `json:"sha256,omitempty"`
	// Files 包内文件清单。path 为相对包根路径。
	Files []ManifestFile `json:"files,omitempty"`
}

// ManifestFile 是包内的一个文件。
type ManifestFile struct {
	// Path 相对包根路径；**必须**是安全的相对路径（见 sanitizeRelPath）。
	Path string `json:"path"`
	// SHA256 单文件摘要（流式校验依据）。
	SHA256 string `json:"sha256"`
	// Size 文件字节数；0 表示未知，下载时先探测。
	Size int64 `json:"size,omitempty"`
	// URL 绝对 URL 或相对当前基址的路径；空表示该文件不可下载（仅内置兜底清单）。
	URL string `json:"url,omitempty"`
}

// manifestSchemaVersion 当前支持的清单格式版本。
const manifestSchemaVersion = 1

// catalogURL 把配置的基址规整为清单直链。
func catalogURL(base string) string {
	base = strings.TrimSpace(base)
	if base == "" {
		return ""
	}
	trimmed := strings.TrimRight(base, "/")
	if strings.HasSuffix(strings.ToLower(trimmed), ".json") {
		return trimmed
	}
	return trimmed + "/" + FileCatalog
}

// manifestBase 返回主清单基址。
func (m *Manager) manifestBase() string { return strings.TrimSpace(m.opts.ManifestBaseURL) }

// mirrorBases 返回生效的镜像基址（最多 3 个，去空去重）。
func (m *Manager) mirrorBases() []string {
	out := make([]string, 0, maxMirrors)
	seen := make(map[string]bool, len(m.opts.Mirrors))
	for _, v := range m.opts.Mirrors {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
		if len(out) >= maxMirrors {
			break
		}
	}
	return out
}

// Fetch 拉取清单：主基址 → 镜像 → 缓存 → 内置兜底。
//
// 返回值语义（与 API-6.1 对齐）：清单**始终非 nil**；远程刷新失败时同时返回
// ERR_DOWNLOAD_NETWORK，让前端能提示「当前显示的是离线缓存」。
func (m *Manager) Fetch(ctx context.Context) (*Manifest, error) {
	// 串行化：并发的播放/下载请求不应各拉一次清单、各写一次缓存。
	m.fetchMu.Lock()
	defer m.fetchMu.Unlock()

	if err := outbound.Guard(); err != nil {
		// 离线开关优先：错误码必须原样是 ERR_OFFLINE_ENFORCED，
		// 不能被「清单兜底」改写成网络错误（用户要看到的是「你开了离线」）。
		mf, _ := m.fallbackManifest(err)
		return mf, apierr.From(err, apierr.CodeOfflineEnforced)
	}

	bases := make([]string, 0, 1+maxMirrors)
	if b := m.manifestBase(); b != "" {
		bases = append(bases, b)
	}
	bases = append(bases, m.mirrorBases()...)

	var lastErr error
	for _, base := range bases {
		mf, err := m.fetchFrom(ctx, base)
		if err == nil {
			m.catMu.Lock()
			m.cat = mf
			m.catAt = time.Now()
			m.catMu.Unlock()
			if werr := m.saveCache(mf); werr != nil {
				// 缓存失败不影响本次可用，但要留痕。
				m.log().Warn("write manifest cache failed", "error", werr.Error())
			}
			return mf, nil
		}
		lastErr = err
		m.log().Warn("manifest fetch failed", "host", hostOf(catalogURL(base)), "error", err.Error())
	}

	return m.fallbackManifest(lastErr)
}

// fetchFrom 从单个基址拉清单并做基本校验。
func (m *Manager) fetchFrom(ctx context.Context, base string) (*Manifest, error) {
	raw := catalogURL(base)
	if raw == "" {
		return nil, fmt.Errorf("models: empty manifest base url")
	}
	// 用代理感知的 client（FR-05-5）：共享的 httpx.Client 是直连的。
	client, cerr := m.httpClient()
	if cerr != nil {
		return nil, cerr
	}
	req, cancel, rerr := httpx.NewRequest(ctx, http.MethodGet, raw, nil)
	if rerr != nil {
		return nil, rerr
	}
	defer cancel()
	req.Header.Set("Accept", "application/json")

	resp, derr := doWithClient(client, req)
	if derr != nil {
		m.recordOutbound(raw, 0, apierr.CodeDownloadNetwork)
		return nil, derr
	}
	var mf Manifest
	if err := httpx.ReadJSONLimit(resp, maxManifestBytes, &mf); err != nil {
		m.recordOutbound(raw, 0, apierr.CodeDownloadNetwork)
		return nil, fmt.Errorf("models: decode manifest: %w", err)
	}
	m.recordOutbound(raw, int64(len(mf.Bundles)), "ok")
	if mf.SchemaVersion != 0 && mf.SchemaVersion != manifestSchemaVersion {
		return nil, fmt.Errorf("models: unsupported manifest schema %d", mf.SchemaVersion)
	}
	if len(mf.Bundles) == 0 {
		return nil, fmt.Errorf("models: manifest has no bundles")
	}
	for i := range mf.Bundles {
		b := &mf.Bundles[i]
		if b.BundleID == "" || b.Version == "" {
			return nil, fmt.Errorf("models: manifest bundle missing bundleId/version")
		}
		// 清单位置写进入口文件 URL：相对路径统一在此展开为绝对地址，
		// 这样下载器只需要处理「绝对 URL + 镜像改写」两种形态。
		for j := range b.Files {
			f := &b.Files[j]
			if f.URL == "" {
				continue
			}
			if !isAbsoluteURL(f.URL) {
				f.URL = joinBaseURL(base, f.URL)
			}
		}
	}
	mf.SchemaVersion = manifestSchemaVersion
	mf.FetchedAt = time.Now().UnixMilli()
	mf.Source = hostOf(raw)
	return &mf, nil
}

// fallbackTTL 是「离线兜底清单」作为缓存的有效期。
//
// 刻意远短于正常 TTL：兜底内容不能长期顶替真实清单，否则用户网络恢复后
// 仍看到内置的占位条目；但又要有一点窗口，让前端「重试一次」能直接拿到内容。
const fallbackTTL = 30 * time.Second

// fallbackManifest 在远程失败时给出缓存或内置清单。
func (m *Manager) fallbackManifest(cause error) (*Manifest, error) {
	var mf *Manifest
	cached, cacheErr := m.loadCache()
	if cacheErr == nil && cached != nil && len(cached.Bundles) > 0 {
		mf = cached
	} else {
		mf = builtinManifest()
	}

	// 兜底内容也进内存缓存（短 TTL）：否则前端每次重试都会再打一次网络并再失败一次。
	m.catMu.Lock()
	m.cat = mf
	m.catAt = time.Now().Add(fallbackTTL - m.opts.CatalogTTL)
	m.catMu.Unlock()

	detail := "manifest refresh failed, showing built-in catalog"
	if cacheErr == nil && cached != nil && len(cached.Bundles) > 0 {
		detail = "manifest refresh failed, showing cached catalog"
	}
	e := apierr.New(apierr.CodeDownloadNetwork).WithDetail("%s", detail)
	if cause != nil {
		e = e.WithCause(cause)
	}
	return mf, e
}

// Catalog 返回清单：优先内存缓存（TTL 内），否则尝试刷新。
//
// 与 Fetch 的差别：Catalog 是**内部**入口（下载 / 列表共用），不把远程失败
// 当错误上抛——它总是给出可用的清单，失败细节已经在 Fetch 里留痕。
func (m *Manager) Catalog(ctx context.Context) *Manifest {
	m.catMu.Lock()
	if m.cat != nil && time.Since(m.catAt) < m.opts.CatalogTTL {
		mf := m.cat
		m.catMu.Unlock()
		return mf
	}
	m.catMu.Unlock()

	mf, err := m.Fetch(ctx)
	if mf == nil {
		// 理论上不会发生（fallback 保证非 nil），留兜底以防未来改动。
		return builtinManifest()
	}
	if err != nil {
		m.log().Warn("catalog fell back", "error", err.Error())
	}
	return mf
}

// lookup 在清单里按 bundleId 找包（优先取最高版本）。
func (m *Manager) lookup(ctx context.Context, bundleID string) (ManifestBundle, error) {
	mf := m.Catalog(ctx)
	var best ManifestBundle
	found := false
	for _, b := range mf.Bundles {
		if b.BundleID != bundleID {
			continue
		}
		if !found || compareVersion(b.Version, best.Version) > 0 {
			best = b
			found = true
		}
	}
	if !found {
		return ManifestBundle{}, apierr.Newf(apierr.CodeModelNotFound, "bundle %q not in catalog", bundleID)
	}
	return best, nil
}

// cachePath 返回清单缓存文件路径。
func (m *Manager) cachePath() (string, error) {
	root, err := m.root()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, FileManifestCache), nil
}

// saveCache 写 manifest.cache.json（filex.WriteAtomic）。
func (m *Manager) saveCache(mf *Manifest) error {
	p, err := m.cachePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return writeJSONAtomic(p, mf)
}

// loadCache 读 manifest.cache.json。
func (m *Manager) loadCache() (*Manifest, error) {
	p, err := m.cachePath()
	if err != nil {
		return nil, err
	}
	var mf Manifest
	if err := readJSONLimit(p, maxManifestBytes, &mf); err != nil {
		return nil, err
	}
	return &mf, nil
}

// ListAvailable 返回清单，并按已装状态补 Installed / UpdateAvailable（API-6.1）。
//
// 错误语义：只有**本次真的尝试了远程刷新并失败**才返回 ERR_DOWNLOAD_NETWORK
// （此时内容来自缓存 / 内置兜底）。缓存仍在有效期内时不触发网络，直接返回 nil 错误
// ——这样前端「重试一次」就能拿到离线内容，而不是反复被拒。
func (m *Manager) ListAvailable(ctx context.Context) ([]contract.BundleInfo, error) {
	var mf *Manifest
	var fetchErr error

	m.catMu.Lock()
	fresh := m.cat != nil && time.Since(m.catAt) < m.opts.CatalogTTL
	if fresh {
		mf = m.cat
	}
	m.catMu.Unlock()

	if !fresh {
		mf, fetchErr = m.Fetch(ctx)
	}
	if mf == nil {
		mf = builtinManifest()
	}
	installed, _ := m.installedIndex()

	out := make([]contract.BundleInfo, 0, len(mf.Bundles))
	for _, b := range mf.Bundles {
		info := contract.BundleInfo{
			BundleID:  b.BundleID,
			Name:      b.Name,
			Version:   b.Version,
			SizeBytes: totalBytesOf(b),
			RAMHintMB: b.RAMHintMB,
			Languages: b.Languages,
			License:   b.License,
			Roles:     b.Roles,
			SHA256:    b.SHA256,
		}
		if cur, ok := installed[b.BundleID]; ok {
			info.Installed = cur.State == contract.BundleInstalled || cur.State == contract.BundleCorrupt
			info.UpdateAvailable = compareVersion(b.Version, cur.Version) > 0
		}
		out = append(out, info)
	}
	return out, fetchErr
}

// —— URL 工具 ——

func isAbsoluteURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

// joinBaseURL 把相对路径接到基址后。
func joinBaseURL(base, rel string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	rel = strings.TrimLeft(strings.ReplaceAll(rel, `\`, "/"), "/")
	return base + "/" + rel
}

// mirrorURL 按镜像索引改写文件 URL：
//   - idx 0 → 原样；
//   - idx>0  → 取原 URL 的 path 部分接到镜像基址后（基址可以是 huggingface
//     风格 resolve 目录，也可以是 GitHub Releases 目录），规则单一、可预测。
func mirrorURL(rawURL string, idx int, bases []string) string {
	if idx <= 0 || idx > len(bases) {
		return rawURL
	}
	base := strings.TrimRight(strings.TrimSpace(bases[idx-1]), "/")
	if base == "" {
		return rawURL
	}
	// 取 path 部分：scheme://host/a/b?x → /a/b
	rest := rawURL
	if i := strings.Index(rest, "://"); i >= 0 {
		rest = rest[i+3:]
		if j := strings.IndexByte(rest, '/'); j >= 0 {
			rest = rest[j:]
		} else {
			rest = "/"
		}
	}
	if k := strings.IndexAny(rest, "?#"); k >= 0 {
		rest = rest[:k]
	}
	return base + "/" + strings.TrimLeft(rest, "/")
}

// compareVersion 比较点分版本号（1.2.0 > 1.10? 否，按数字段比较）。
// 非数字段按字典序，保证确定性即可。
func compareVersion(a, b string) int {
	as := strings.Split(strings.TrimPrefix(a, "v"), ".")
	bs := strings.Split(strings.TrimPrefix(b, "v"), ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		var av, bv string
		if i < len(as) {
			av = as[i]
		}
		if i < len(bs) {
			bv = bs[i]
		}
		an, aok := atoiSafe(av)
		bn, bok := atoiSafe(bv)
		if aok && bok {
			if an != bn {
				if an < bn {
					return -1
				}
				return 1
			}
			continue
		}
		if av != bv {
			if av < bv {
				return -1
			}
			return 1
		}
	}
	return 0
}

func atoiSafe(s string) (int, bool) {
	if s == "" {
		return 0, true
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + int(r-'0')
	}
	return n, true
}

// builtinManifest 是离线 / 断网时的兜底清单。
//
// 只提供**元数据**（名称、体积提示、角色、语言、许可），不含可用 URL：
// 真正下载必须刷新远端清单，这样既不会让 UI 空白，也不会在断网时假装能下。
func builtinManifest() *Manifest {
	return &Manifest{
		SchemaVersion: manifestSchemaVersion,
		GeneratedAt:   "builtin",
		Bundles: []ManifestBundle{
			{
				BundleID:  "sensevoice-small",
				Name:      "SenseVoice Small（中英日韩粤）",
				Version:   "1.2.0",
				SizeBytes: 228 << 20,
				RAMHintMB: 900,
				Languages: []string{"zh", "en", "ja", "ko", "yue"},
				License:   "Apache-2.0",
				Roles:     []string{"offline"},
			},
			{
				BundleID:  "zipformer-zh-streaming",
				Name:      "Zipformer 中文流式",
				Version:   "1.0.3",
				SizeBytes: 152 << 20,
				RAMHintMB: 640,
				Languages: []string{"zh"},
				License:   "Apache-2.0",
				Roles:     []string{"streaming"},
			},
			{
				BundleID:  "silero-vad",
				Name:      "Silero VAD",
				Version:   "2.0.0",
				SizeBytes: 2 << 20,
				RAMHintMB: 64,
				Languages: []string{"*"},
				License:   "MIT",
				Roles:     []string{"vad"},
			},
			{
				BundleID:  "3dspeaker-zh",
				Name:      "3D-Speaker 中文声纹",
				Version:   "1.1.0",
				SizeBytes: 28 << 20,
				RAMHintMB: 320,
				Languages: []string{"zh"},
				License:   "Apache-2.0",
				Roles:     []string{"speaker"},
			},
		},
	}
}

// recordOutbound 落出站留痕（只写域名，不写完整 URL）。
func (m *Manager) recordOutbound(rawURL string, n int64, result string) {
	if m.opts.Recorder == nil {
		return
	}
	m.opts.Recorder.Record(hostOf(rawURL), contract.OutboundModelDownload, false, n, result)
}
