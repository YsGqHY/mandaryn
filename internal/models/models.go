// Package models 是模型仓库领域包（FR-05 / FR-07 的仓库与迁移部分）。
//
// 职责划分：
//   - catalog.go   清单获取与缓存（远程 → 镜像 → 本地缓存 → 内置兜底）
//   - download.go  分片下载、断点续传、校验、镜像回退（ARCH §4.6.2 六步）
//   - store.go     已装包仓库：扫描、校验、删除、统计、离线导入、按角色定位
//   - migrate.go   模型根迁移状态机（ARCH §4.6.3 六阶段）
//   - proxy.go     代理传输构造（none / http / https / socks5）
//
// 铁律：
//   - 本包**不 import Wails**；事件一律通过 Options 里的回调交给 L3 桥接；
//   - 不持有 *storage.DB，只用 *storage.Holder（切换数据路径会替换句柄）；
//   - 出站请求走 httpx + outbound.Guard / Recorder，且受 MANDARYN_OFFLINE 约束；
//   - 状态文件 / 清单缓存写入一律 filex.WriteAtomic（禁止 os.WriteFile）；
//   - 音频与正文永不进入日志，本包也只写元数据（bundleId / 字节数 / 错误码）。
package models

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gorm.io/gorm"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/outbound"
	"vip.oberon.mandaryn/internal/paths"
	"vip.oberon.mandaryn/internal/storage"
	"vip.oberon.mandaryn/internal/utils/filex"
	"vip.oberon.mandaryn/internal/utils/logx"
)

// 目录与文件名（ARCH §4.6.1）。
const (
	// DirModels 已装模型包子目录。
	DirModels = "models"
	// DirDownloads 分片与断点状态子目录。
	DirDownloads = ".downloads"
	// DirStagingPrefix 迁移 / 导入的临时区前缀（启动与操作结束后必须清理）。
	DirStagingPrefix = ".staging-"
	// DirTrashPrefix 待删除目录前缀（删除失败不阻断可用性）。
	DirTrashPrefix = ".trash-"

	// FileManifestCache 清单缓存文件名。
	FileManifestCache = "manifest.cache.json"
	// FileBundleManifest 每个已装包内的元数据文件名。
	FileBundleManifest = "manifest.json"
	// FileCatalog 清单在远端基址下的默认文件名。
	FileCatalog = "models.json"
)

// 下载与迁移参数（ARCH §4.6.2 / §4.6.3）。
const (
	// defaultChunkSize 单分片 8MB。
	defaultChunkSize int64 = 8 << 20
	// defaultChunkConcurrency 单文件 4 并发分片。
	defaultChunkConcurrency = 4
	// maxConcurrentBundles 同时最多 2 个 bundle 下载（避免占满带宽影响转写）。
	maxConcurrentBundles = 2
	// maxMirrors 镜像最多 3 个生效。
	maxMirrors = 3
	// diskOverheadFactor 预检要求空间 ≥ size*1.15。
	diskOverheadFactor = 1.15
	// progressInterval 进度事件 4Hz 节流。
	progressInterval = 250 * time.Millisecond
	// defaultCatalogTTL 清单本地缓存有效期。
	defaultCatalogTTL = 10 * time.Minute
	// maxFileAttempts 单文件「整包重下」最大尝试次数（含首次）。
	maxFileAttempts = 2
	// maxManifestBytes 清单读取上限，防止失控响应。
	maxManifestBytes int64 = 8 << 20
)

// defaultBackoff 失败退避序列（1s / 4s / 16s）。
var defaultBackoff = []time.Duration{time.Second, 4 * time.Second, 16 * time.Second}

// Options 是 Manager 的依赖注入点。
type Options struct {
	// Holder 持久化句柄；nil 时注册表只留在内存（单测无库场景）。
	Holder *storage.Holder
	// Recorder 出站留痕；nil 时仅做离线闸门判断。
	Recorder *outbound.Recorder
	// Root 返回**当前生效**的 modelRoot；nil 时退化为 paths.DefaultModelRoot。
	Root func() (string, error)
	// ManifestBaseURL 清单基址（目录风格则自动拼 models.json；`.json` 结尾直链则原样用）。
	ManifestBaseURL string
	// Mirrors 备用镜像基址，最多 3 个生效。
	Mirrors []string
	// Proxy 返回当前代理配置（每次出站前取一次，改设置后无需重启）。
	Proxy func() (contract.ProxyConfig, error)
	// OnDownload / OnMigrate / OnChanged 是 L3 事件桥接回调，可为 nil。
	OnDownload func(contract.DownloadState)
	OnMigrate  func(contract.MigrateState)
	OnChanged  func(contract.ModelChanged)
	// OnModelRootChanged 在迁移切换成功后调用，让上层（paths.Resolver）失效缓存。
	OnModelRootChanged func(newRoot string) error
	// HTTPClient 覆盖默认客户端（单测注入 httptest 的 client）。
	HTTPClient *http.Client
	// Backoff 失败退避序列，默认 1s/4s/16s。
	Backoff []time.Duration
	// ChunkSize 分片大小，默认 8MB。
	ChunkSize int64
	// ChunkConcurrency 单文件分片并发，默认 4。
	ChunkConcurrency int
	// CatalogTTL 清单缓存有效期，默认 10 分钟。
	CatalogTTL time.Duration
}

// Manager 是模型仓库域的统一入口，同时实现 S3 需要的 BundleLocator 接口。
type Manager struct {
	opts Options

	// rootOverride 迁移切换后临时顶替 opts.Root，等上层解析器缓存失效后再清。
	rootOverride atomic.Value // string

	clientMu   sync.Mutex
	client     *http.Client
	clientKey  string
	clientErr  error

	// fetchMu 串行化清单拉取：并发下载会同时触发 Fetch，
	// 两个 goroutine 同时写 manifest.cache.json 会在 Windows 上撞 rename。
	fetchMu sync.Mutex

	catMu sync.Mutex
	cat   *Manifest
	catAt time.Time

	taskMu sync.Mutex
	tasks  map[string]*downloadTask
	dlSem  chan struct{}

	migrating  atomic.Bool
	migMu      sync.RWMutex
	migState   contract.MigrateState
	migCancel  context.CancelFunc
	migChanged chan struct{} // 阶段变化广播（唤醒 AwaitMigration 轮询）
}

// New 构造 Manager，填充默认值。
func New(opts Options) *Manager {
	if len(opts.Backoff) == 0 {
		opts.Backoff = defaultBackoff
	}
	if opts.ChunkSize <= 0 {
		opts.ChunkSize = defaultChunkSize
	}
	if opts.ChunkConcurrency <= 0 {
		opts.ChunkConcurrency = defaultChunkConcurrency
	}
	if opts.CatalogTTL <= 0 {
		opts.CatalogTTL = defaultCatalogTTL
	}
	m := &Manager{
		opts:       opts,
		tasks:      make(map[string]*downloadTask),
		dlSem:      make(chan struct{}, maxConcurrentBundles),
		migState:   contract.MigrateState{Phase: contract.MigrateIdle},
		migChanged: make(chan struct{}),
	}
	return m
}

// Root 返回当前生效的模型根。
func (m *Manager) Root() (string, error) { return m.root() }

func (m *Manager) root() (string, error) {
	if v, ok := m.rootOverride.Load().(string); ok && strings.TrimSpace(v) != "" {
		return v, nil
	}
	if m.opts.Root != nil {
		return m.opts.Root()
	}
	return paths.DefaultModelRoot()
}

// InvalidateRoot 丢弃迁移期间设置的临时根，改回上层解析器的结果。
//
// L3 在完成 paths.Resolver 的赋值 / 失效后调用；否则迁移后本包会一直用
// 内部覆盖值，用户再次手动改目录时表现不一致。
func (m *Manager) InvalidateRoot() { m.rootOverride.Store("") }

// checkBusy 在迁移期间拦下一切下载 / 加载 / 删除请求（ARCH §4.6.3 一致性保障 1）。
func (m *Manager) checkBusy() error {
	if m.migrating.Load() {
		return apierr.New(apierr.CodeModelBusy).WithDetail("model root migration in progress")
	}
	return nil
}

// db 返回当前活跃的 GORM 句柄；无库（单测）时返回 nil。
func (m *Manager) db() *gorm.DB {
	if m.opts.Holder == nil {
		return nil
	}
	db := m.opts.Holder.Current()
	if db == nil {
		return nil
	}
	return db.GORM
}

// DB 返回当前活跃的 GORM 句柄（无库时为 nil）。
//
// 仅供 L3 做**只读**配置读取（如代理设置）；业务写库仍应走 Holder + 各自领域方法。
func (m *Manager) DB() *gorm.DB { return m.db() }

func (m *Manager) log() *slog.Logger { return logx.For("models") }

// isWindows 便于在纯函数里做平台分支（窗口配置那种 build tag 拆分对
// 这几行路径比较并不划算）。
func isWindows() bool { return runtime.GOOS == "windows" }

// —— 路径安全 ——

// errUnsafePath 表示清单里出现了越界路径（路径穿越）。
var errUnsafePath = errors.New("models: unsafe relative path in manifest")

// sanitizeRelPath 校验并归一化清单里的相对路径。
//
// 为什么必须做：清单是**远端数据**，一旦被投毒给出 `../../evil`，下载器就会
// 往模型根之外写文件。规则：拒绝绝对路径、盘符、`..` 逃逸与 NUL。
func sanitizeRelPath(p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", fmt.Errorf("%w: empty", errUnsafePath)
	}
	if strings.ContainsRune(p, 0) {
		return "", fmt.Errorf("%w: %q contains NUL", errUnsafePath, p)
	}
	q := strings.ReplaceAll(p, `\`, "/")
	if strings.HasPrefix(q, "/") {
		return "", fmt.Errorf("%w: %q is absolute", errUnsafePath, p)
	}
	// Windows 盘符（C:/...）与 UNC 前缀
	if len(q) >= 2 && q[1] == ':' {
		return "", fmt.Errorf("%w: %q has a drive letter", errUnsafePath, p)
	}
	clean := path.Clean(q)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("%w: %q escapes the target directory", errUnsafePath, p)
	}
	return filepath.FromSlash(clean), nil
}

// resolveInside 把相对路径接到 root 下，并二次确认结果仍位于 root 内。
func resolveInside(root, rel string) (string, error) {
	cleanRel, err := sanitizeRelPath(rel)
	if err != nil {
		return "", err
	}
	joined := filepath.Join(root, cleanRel)
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	absJoined, err := filepath.Abs(joined)
	if err != nil {
		return "", err
	}
	prefix := absRoot + string(os.PathSeparator)
	if absJoined != absRoot && !strings.HasPrefix(absJoined, prefix) {
		return "", fmt.Errorf("%w: %q resolves outside %s", errUnsafePath, rel, root)
	}
	return absJoined, nil
}

// sha256File 流式计算文件摘要（不把内容读进内存）。
func sha256File(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", n, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// sha256Valid 判断摘要字符串形态是否合法（64 位十六进制）。
func sha256Valid(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// writeJSONAtomic 用 filex.WriteAtomic 落一份 JSON（缩进便于人工诊断）。
func writeJSONAtomic(p string, v any) error {
	buf, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("models: marshal json: %w", err)
	}
	return filex.WriteAtomic(p, buf, 0o644)
}

// readJSONLimit 读取受大小上限保护的 JSON 文件。
func readJSONLimit(p string, max int64, out any) error {
	buf, err := filex.ReadLimit(p, max)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(buf, out); err != nil {
		return fmt.Errorf("models: decode %s: %w", filepath.Base(p), err)
	}
	return nil
}

// dirSize 递归统计目录占用（忽略不存在的目录）。
func dirSize(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if info, statErr := d.Info(); statErr == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

// removeDirAsync 把目录改名后异步删除（Windows 上正在被占用的文件删不掉，
// 改名成功即可让可用性不受影响，ARCH §4.6.3 cleaning 阶段）。
func removeDirAsync(dir string) {
	if _, err := os.Stat(dir); err != nil {
		return
	}
	parent := filepath.Dir(dir)
	trash := filepath.Join(parent, fmt.Sprintf("%s%s-%d", DirTrashPrefix, filepath.Base(dir), time.Now().UnixNano()))
	if err := os.Rename(dir, trash); err != nil {
		// 改名失败：仍尝试直接删，失败就留着（不影响可用性）。
		if rmErr := os.RemoveAll(dir); rmErr != nil {
			logx.For("models").Warn("remove dir failed", "dir", filepath.Base(dir), "error", rmErr.Error())
		}
		return
	}
	go func() {
		if err := os.RemoveAll(trash); err != nil {
			logx.For("models").Warn("cleanup trash dir failed", "dir", filepath.Base(trash), "error", err.Error())
		}
	}()
}

// bundleDirName 返回 `<bundleId>@<version>` 目录名。
func bundleDirName(bundleID, version string) string {
	if version == "" {
		return bundleID
	}
	return bundleID + "@" + version
}

// totalBytesOf 汇总清单里各文件大小（清单未给 sizeBytes 时兜底）。
func totalBytesOf(b ManifestBundle) int64 {
	if b.SizeBytes > 0 {
		return b.SizeBytes
	}
	var total int64
	for _, f := range b.Files {
		total += f.Size
	}
	return total
}

// errDTO 把 apierr 投影成可嵌进 DTO 的纯数据。
func errDTO(err error) *contract.ErrorDTO {
	if err == nil {
		return nil
	}
	e := apierr.From(err, apierr.CodeStateForbidden)
	return &contract.ErrorDTO{
		Code:      e.Code,
		Message:   e.Message,
		Action:    e.Action,
		Detail:    e.Detail,
		Retryable: e.Retryable,
	}
}

// hostOf 只取 scheme://host，避免把 query / 凭据写进任何记录（NFR-03）。
func hostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Host
}

// newTempID 生成短随机标识（跨平台，不引入 uuid 依赖）。
func newTempID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 熵源不可用时退化为时间戳：只用于临时目录名，不参与安全判定。
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return fmt.Sprintf("%x", b)
}

// cleanupStaging 清理启动时残留的 `.staging-*` 目录（ARCH §4.6.1）。
func (m *Manager) cleanupStaging(ctx context.Context) {
	root, err := m.root()
	if err != nil {
		return
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), DirStagingPrefix) {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if err := os.RemoveAll(dir); err != nil {
			m.log().Warn("cleanup stale staging failed", "dir", e.Name(), "error", err.Error())
			continue
		}
		m.log().Info("cleanup stale staging", "dir", e.Name())
	}
}

// Cleanup 是启动钩子：清理残留 staging 目录并把 downloads 表里过期的
// `downloading` 行改成 paused（进程被杀后不可能仍在下载）。
func (m *Manager) Cleanup(ctx context.Context) {
	m.cleanupStaging(ctx)
	db := m.db()
	if db == nil {
		return
	}
	if err := db.WithContext(ctx).Model(&storage.Download{}).
		Where("state = ?", "downloading").
		Update("state", "paused").Error; err != nil {
		m.log().Warn("reset stale download rows failed", "error", err.Error())
	}
}

// 保证 Manager 实现 S3 需要的按角色定位接口。
var _ BundleLocator = (*Manager)(nil)
