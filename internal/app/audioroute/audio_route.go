package audioroute

// 会话音频流路由（Wave E-2）。
//
// 为什么需要这一层：回放必须能让 WebView 里的 `<audio>` 直接播放会话录音，
// 而唯一正确的做法是用**浏览器原生 Range 请求**流式读取文件。
// 因此这里把 `/audio/<sessionId>` 叠加到 SPA 资源处理器之上：
//   - 命中前缀 → 走本文件的音频处理器（ServeContent + Range，支持 seek）；
//   - 未命中 → 原样交给 SPA 资源处理器（前端路由回退不受影响）。
//
// 明确**不做**的事：不把 WAV 读进内存、不 base64、不整文件复制。
// 几十分钟录音可达数百 MB，任何「先读全量再返回」的实现都会在 seek 时被反复重传打死。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"vip.oberon.mandaryn/internal/paths"
	"vip.oberon.mandaryn/internal/storage"
	"vip.oberon.mandaryn/internal/utils/logx"

	"github.com/wailsapp/wails/v3/pkg/application"
	"gorm.io/gorm"
)

// audioRoutePrefix 是音频路由前缀（前端 `src` 直接用 `/audio/<sessionId>`）。
const audioRoutePrefix = "/audio/"

const (
	audioContentTypeAudioWAV  = "audio/wav"
	audioContentTypeAudioFLAC = "audio/flac"
)

// sessionAudioLookup 按会话 ID 查库里的音频相对路径（相对 dataRoot）。
//
// 抽成函数类型而不是直接用 *storage.Holder：HTTP 层的单元测试可以注入假实现，
// 不必为了测 Range 语义去搭一整套 SQLite + AutoMigrate。
type sessionAudioLookup func(ctx context.Context, sessionID string) (relPath string, ok bool, err error)

// dataRootProvider 返回当前生效的 dataRoot（ADR-02：必须经 paths.Resolver）。
type dataRootProvider func() (string, error)

// NewAssetHandler 把「音频流路由」叠加在 SPA 资源处理器之上。
// assets 为嵌入的前端产物；返回的 Handler 直接交给 application.AssetOptions.Handler。
//
// 接入方式（app.go）：
//
//	Assets: application.AssetOptions{
//		Handler: NewAssetHandler(assets, holder, resolver),
//	},
func NewHandler(assets fs.FS, holder *storage.Holder, resolver *paths.Resolver) http.Handler {
	return newAudioHandler(
		application.AssetFileServerFS(assets),
		gormAudioLookup(holder),
		resolverDataRoot(resolver),
	)
}

// newAudioHandler 是可直接单测的组装点：SPA handler、会话查询、dataRoot 都是依赖注入。
func newAudioHandler(spa http.Handler, lookup sessionAudioLookup, dataRoot dataRootProvider) http.Handler {
	if spa == nil {
		spa = http.NotFoundHandler()
	}
	return &audioRouteHandler{spa: spa, lookup: lookup, dataRoot: dataRoot}
}

// audioRouteHandler 只分流 `/audio/<sessionId>`，其余一律透传给 SPA。
type audioRouteHandler struct {
	spa      http.Handler
	lookup   sessionAudioLookup
	dataRoot dataRootProvider
}

func (h *audioRouteHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 用 RequestURI 判断前缀：前端可能带查询串（`?v=<updatedAt>` 破缓存），
	// 而 r.URL.Path 已经去掉查询串，两者在这里等价；取 Path 更稳妥
	// （某些客户端会发绝对形式 URI）。
	if !strings.HasPrefix(r.URL.Path, audioRoutePrefix) {
		h.spa.ServeHTTP(w, r)
		return
	}
	h.serveAudio(w, r)
}

// serveAudio 处理单条音频请求。
//
// 安全模型的顺序刻意如此：**先查库、再判越界、再判后缀、最后开文件**。
// 任何一步失败都统一回 404（不区分「不存在」与「越界」），
// 因为区分开就等于给攻击者一个探测「某个路径是否存在」的旁路；
// 真实路径一律不回显到响应或日志正文。
func (h *audioRouteHandler) serveAudio(w http.ResponseWriter, r *http.Request) {
	sessionID := strings.TrimPrefix(r.URL.Path, audioRoutePrefix)
	// 只接受单段 ID：`/audio/a/b`、`/audio/` 之类直接 404。
	if sessionID == "" || strings.Contains(sessionID, "/") {
		h.miss(w, r, "", "bad_request_path")
		return
	}

	if h.lookup == nil || h.dataRoot == nil {
		h.miss(w, r, sessionID, "not_wired")
		return
	}

	relPath, found, err := h.lookup(r.Context(), sessionID)
	if err != nil {
		// 读库失败不回显错误详情（可能是路径/表名等内部信息）。
		h.miss(w, r, sessionID, "lookup_failed")
		return
	}
	if !found || relPath == "" {
		h.miss(w, r, sessionID, "no_session")
		return
	}

	// 按落库路径允许 FLAC 与旧 WAV，不信任客户端 Content-Type / Accept。
	contentType := audioContentType(relPath)
	if contentType != audioContentTypeAudioWAV && contentType != audioContentTypeAudioFLAC {
		h.miss(w, r, sessionID, "unsupported_audio_format")
		return
	}

	root, err := h.dataRoot()
	if err != nil || root == "" {
		h.miss(w, r, sessionID, "no_data_root")
		return
	}

	abs, err := safeJoinUnderRoot(root, relPath)
	if err != nil {
		// 越界 / 非法路径：与「不存在」同样回 404，错误详情只进日志元数据。
		h.miss(w, r, sessionID, "outside_root")
		return
	}

	// 打开前再判一次「解析后的真实路径仍在根内」：挡住符号链接 / junction 越界
	// （safeJoinUnderRoot 只看字面路径，看不到链接指向哪里）。
	if err := ensureRealPathUnderRoot(root, abs); err != nil {
		h.miss(w, r, sessionID, "outside_root_link")
		return
	}

	f, err := os.Open(abs)
	if err != nil {
		h.miss(w, r, sessionID, "open_failed")
		return
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		h.miss(w, r, sessionID, "stat_failed")
		return
	}

	// 用 http.ServeContent 而不是 io.Copy：
	// 它已经实现 Range / If-Range / If-Modified-Since / HEAD / 206 / 416，
	// 手写这些分支是 seek 卡顿与边界错误的常见来源。
	hdr := w.Header()
	hdr.Set("Content-Type", contentType)
	hdr.Set("Accept-Ranges", "bytes")
	// no-store：会话音频可能被重新录制覆盖（同 ID 不可能，但重转/导出会改 mtime），
	// 且这是本机私密音频，不该在 WebView 缓存里留副本。
	hdr.Set("Cache-Control", "no-store")

	// modtime 传零值 time.Time：ServeContent 在零值时不写 Last-Modified、
	// 也不做 If-Modified-Since 的 304 判定，与 Cache-Control: no-store 语义一致。
	// name 传空串：不让 ServeContent 用文件名去猜 Content-Type（我们已显式设置），
	// 也不要把真实文件名泄进 Content-Disposition。
	rec := &statusRecorder{ResponseWriter: w}
	http.ServeContent(rec, r, "", time.Time{}, f)

	// 日志只写元数据（会话 ID / 实际发出的字节数 / 状态码 / Range 头）。
	logx.For("audio-route").Info("served audio",
		"sessionId", sessionID,
		"fileBytes", info.Size(),
		"bytes", rec.written,
		"status", rec.status,
		"range", r.Header.Get("Range"),
	)
}

// statusRecorder 记录实际写出的状态码与字节数，仅用于日志元数据。
//
// 它同时实现 io.ReaderFrom 并把调用转发给底层：http.ServeContent 内部走 io.CopyN，
// 若不转发，大文件传输会从底层的零拷贝路径退化为 32KB 缓冲复制。
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written int64
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.written += int64(n)
	return n, err
}

func (s *statusRecorder) ReadFrom(r io.Reader) (int64, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	if rf, ok := s.ResponseWriter.(io.ReaderFrom); ok {
		n, err := rf.ReadFrom(r)
		s.written += n
		return n, err
	}
	// 包一层裸 Writer，避免 io.Copy 又回到本类型的 ReadFrom 上形成递归。
	n, err := io.Copy(struct{ io.Writer }{s.ResponseWriter}, r)
	s.written += n
	return n, err
}

// miss 统一输出 404，并记录**只有元数据**的日志（会话 ID + 原因码，绝不含真实路径）。
func (h *audioRouteHandler) miss(w http.ResponseWriter, r *http.Request, sessionID, reason string) {
	logx.For("audio-route").Warn("audio request rejected",
		"sessionId", sessionID, "reason", reason, "method", r.Method)
	// 显式 no-store：否证结果也不要被缓存（否则「录制后立刻可播」会拿到旧 404）。
	w.Header().Set("Cache-Control", "no-store")
	http.NotFound(w, r)
}

// gormAudioLookup 用 *storage.Holder 查 sessions.audio_path。
//
// 关键：每次访问 holder.Current()（切换数据目录会替换 DB 句柄），
// 绝不缓存 *storage.DB。
func gormAudioLookup(holder *storage.Holder) sessionAudioLookup {
	return func(ctx context.Context, sessionID string) (string, bool, error) {
		if holder == nil || holder.Current() == nil {
			return "", false, errors.New("audio: storage holder not ready")
		}
		var row struct {
			AudioPath string
		}
		err := holder.Current().GORM.WithContext(ctx).
			Model(&storage.Session{}).
			Select("audio_path").
			Where("id = ?", sessionID).
			Take(&row).Error
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return "", false, nil
			}
			return "", false, err
		}
		return row.AudioPath, true, nil
	}
}

// resolverDataRoot 从解析器取 dataRoot（不自己读环境变量，ADR-02）。
func resolverDataRoot(resolver *paths.Resolver) dataRootProvider {
	return func() (string, error) {
		if resolver == nil {
			return "", errors.New("audio: paths resolver not ready")
		}
		return resolver.Effective(paths.KeyDataRoot)
	}
}

// audioContentType 按扩展名给 Content-Type。当前会话音频只有 WAV 一种，
// 但保留映射便于将来加 mp3/opus 时不改调用点。
func audioContentType(p string) string {
	switch strings.ToLower(path.Ext(p)) {
	case ".wav":
		return audioContentTypeAudioWAV
	case ".mp3":
		return "audio/mpeg"
	case ".opus", ".ogg":
		return "audio/ogg"
	case ".flac":
		return audioContentTypeAudioFLAC
	case ".m4a":
		return "audio/mp4"
	default:
		// 兜底声明为二进制：不猜类型，避免浏览器按 HTML 解析本地文件。
		return "application/octet-stream"
	}
}

// safeJoinUnderRoot 把**相对 dataRoot** 的 rel 拼到 root 下，并判定是否越界。
//
// 越界判定策略（为什么这样做）：
//  1. 先拒绝一切"看起来就是绝对路径/盘符/UNC"的输入（`C:\x`、`/etc/passwd`、
//     `\\server\share`、`/`）。理由：这些输入即使 `filepath.Join` 之后落在根外，
//     也不会因为 Join 的相对语义被"收回"到根内，直接判非法最省心。
//     —— Windows 上 `C:foo` 这种**盘符相对**形式也要一并挡掉。
//  2. 统一把分隔符归一为 `/` 并 `path.Clean`，失败（非法字节）判非法。
//  3. 再拒绝 Clean 后仍是绝对路径、或以 `..` 起头的相对路径
//     （`..` 起头意味着 Clean 之后仍然向上跳，Join 到 root 必然逃逸）。
//  4. 最后用 `filepath.Rel(rootAbs, targetAbs)` 双保险：Rel 的结果若以 `..`
//     起头或本身是绝对路径，说明 target 不在 root 之下 —— 这是唯一与平台
//     大小写/分隔符规则一致的判定方式，不要手工字符串前缀比较
//     （`strings.HasPrefix("/data/root", "/data/ro")` 这类前缀比较会误放行）。
//
// 注意：本函数只看**字面路径**，符号链接指向哪里它看不到；链接越界由调用点
// 的 ensureRealPathUnderRoot 兜底。
func safeJoinUnderRoot(root, rel string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", errors.New("audio: empty root")
	}
	if strings.TrimSpace(rel) == "" {
		return "", errors.New("audio: empty relative path")
	}
	// rel 入库时是正斜杠形式（recording 用 path.Join 生成），这里允许两种分隔符。
	normalized := strings.ReplaceAll(rel, `\`, "/")

	// (1) 绝对路径 / UNC / 根路径一律非法。
	// `\\server\share` 在上面归一后变成 `//server/share`，同样被这一个前缀检查挡住。
	if strings.HasPrefix(normalized, "/") {
		return "", fmt.Errorf("audio: absolute path rejected")
	}
	// 盘符：`C:/x` 或 `C:x`（盘符相对）。统一按「含冒号的首段是盘符」处理，
	// 因为 Windows 上 `C:foo` 也是逃逸手段（相对该盘的当前目录）。
	if len(normalized) >= 2 && normalized[1] == ':' {
		return "", fmt.Errorf("audio: drive letter rejected")
	}

	// (2) 归一化并清掉 `.` / 尾部 `/`。
	cleaned := path.Clean(normalized)
	if cleaned == "." || cleaned == "/" {
		return "", errors.New("audio: empty cleaned path")
	}
	// (3) Clean 之后仍以 .. 起头 → 依然在向上逃逸。
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", errors.New("audio: parent traversal rejected")
	}
	// path.Clean 之后不可能含中间 `..`（`a/../../b` 会被压成 `../b`，已被上一步拦住），
	// 但保留一次显式检查让不变量在代码里可见。
	if strings.Contains(cleaned, "/../") {
		return "", errors.New("audio: parent traversal rejected")
	}

	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("audio: resolve root: %w", err)
	}
	targetAbs := filepath.Join(rootAbs, filepath.FromSlash(cleaned))

	// (4) Rel 判定：唯一可信的「是否在子树内」检查。
	inRoot, err := isUnderRoot(rootAbs, targetAbs)
	if err != nil || !inRoot {
		return "", errors.New("audio: path escapes data root")
	}
	return targetAbs, nil
}

// isUnderRoot 判定 target 是否严格位于 root 之下（两者都应已是绝对、已 Clean）。
func isUnderRoot(rootAbs, targetAbs string) (bool, error) {
	relToRoot, err := filepath.Rel(rootAbs, targetAbs)
	if err != nil {
		return false, err
	}
	if filepath.IsAbs(relToRoot) {
		return false, nil
	}
	// Rel 用 `..` 表示「分别在两棵树上」，以 .. 起头即越界（含 `..` 本身）。
	if relToRoot == ".." || strings.HasPrefix(relToRoot, ".."+string(filepath.Separator)) {
		return false, nil
	}
	return true, nil
}

// ensureRealPathUnderRoot 解析符号链接后再次判定越界。
//
// 为什么要额外一步：safeJoinUnderRoot 是纯字符串/路径运算，
// 若 dataRoot/sessions/<id>/audio.wav 其实是一个指向 `/etc/shadow`
// 的符号链接（或 Windows junction），字面判定会通过。
// EvalSymlinks 拿到真实路径后再比对一次即可堵住这个缺口。
func ensureRealPathUnderRoot(root, abs string) error {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	realRoot, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		// 根本身解析不了（例如刚被删除）：按失败处理，不回退到字面判定。
		return err
	}
	realTarget, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return err
	}
	ok, err := isUnderRoot(realRoot, realTarget)
	if err != nil || !ok {
		return errors.New("audio: resolved path escapes data root")
	}
	return nil
}
