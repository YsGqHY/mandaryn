package models

// 测试支撑：用 httptest 搭「假清单 + 假分片服务」，全程离线。
//
// 为什么不打真实远端：huggingface.co 不可达、GitHub Releases 不能依赖网络与配额；
// 下载器最容易出错的恰恰是分片 / 续传 / ETag / 镜像这类协议细节，必须能在
// 本地精确构造（包括「服务端半途断连」这种真实网络里的偶发故障）。

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"vip.oberon.mandaryn/internal/contract"
)

// testFile 是一个待下载的远端文件。
type testFile struct {
	// Data 当前内容（可在测试中途替换，模拟「远端内容变了」）。
	Data []byte
	// ETag 当前 ETag；为空则不发。
	ETag string
	// Always500 让该文件的所有请求返回 500（模拟主镜像故障）。
	Always500 bool
	// Corrupt 返回等长但被篡改的内容（模拟传输损坏 / 供应链投毒）。
	Corrupt bool
	// AbortAfter >0：前 N 次请求正常响应，之后一律断连（模拟半途掉线）。
	AbortAfter int
	// successes 已正常响应的次数。
	successes int
}

// rangeServer 是假远端：既提供 /models.json 清单，也提供分片下载。
type rangeServer struct {
	*httptest.Server

	mu       sync.Mutex
	files    map[string]*testFile
	requests []string // "path|Range|If-Range"
	delay    time.Duration
}

func newRangeServer(t *testing.T) *rangeServer {
	t.Helper()
	s := &rangeServer{files: make(map[string]*testFile)}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.Close)
	return s
}

// setFile 注册 / 覆盖一个远端文件。
func (s *rangeServer) setFile(name string, f *testFile) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files[name] = f
}

// setData 替换某文件内容与 ETag（模拟远端内容更新）。
func (s *rangeServer) setData(name string, data []byte, etag string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f, ok := s.files[name]; ok {
		f.Data = data
		f.ETag = etag
		f.successes = 0
	}
}

// heal 让所有文件恢复正常响应（清掉故障注入）。
func (s *rangeServer) heal() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range s.files {
		f.Always500 = false
		f.Corrupt = false
		f.AbortAfter = 0
		f.successes = 0
	}
}

// setDelay 给每个请求加固定延迟（用于并发上限测试）。
func (s *rangeServer) setDelay(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delay = d
}

// takeRequests 取出并清空已记录的请求（便于按「会话」断言 Range 头）。
func (s *rangeServer) takeRequests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.requests
	s.requests = nil
	return out
}

// setCatalog 用给定包列表生成本服务的清单文件。
func (s *rangeServer) setCatalog(t *testing.T, bundles ...ManifestBundle) {
	t.Helper()
	buf, err := json.Marshal(Manifest{SchemaVersion: manifestSchemaVersion, Bundles: bundles})
	if err != nil {
		t.Fatalf("marshal catalog: %v", err)
	}
	s.setFile(FileCatalog, &testFile{Data: buf, ETag: `"catalog-v1"`})
}

// breakCatalog 让清单接口返回 500（模拟远端不可用）。
func (s *rangeServer) breakCatalog() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f, ok := s.files[FileCatalog]; ok {
		f.Always500 = true
	}
}

func (s *rangeServer) handle(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/")
	rangeHdr := r.Header.Get("Range")
	ifRange := r.Header.Get("If-Range")

	s.mu.Lock()
	f, ok := s.files[name]
	var data []byte
	etag := ""
	always500, corrupt, abort := false, false, false
	if ok {
		data = append([]byte(nil), f.Data...)
		etag = f.ETag
		always500 = f.Always500
		corrupt = f.Corrupt
		if f.AbortAfter > 0 && f.successes >= f.AbortAfter {
			abort = true
		}
	}
	s.requests = append(s.requests, fmt.Sprintf("%s|%s|%s", name, rangeHdr, ifRange))
	delay := s.delay
	s.mu.Unlock()

	if delay > 0 {
		time.Sleep(delay)
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	if always500 {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if abort {
		// 半途断连：ErrAbortHandler 让 httptest 直接断开连接且不打印噪音日志。
		panic(http.ErrAbortHandler)
	}

	s.mu.Lock()
	if f2, ok2 := s.files[name]; ok2 {
		f2.successes++
	}
	s.mu.Unlock()

	w.Header().Set("ETag", etag)

	// If-Range 不匹配（远端内容已变）→ 回整包 200，这是客户端必须「整包重下」的信号。
	if ifRange != "" && etag != "" && ifRange != etag {
		writeFull(w, data, corrupt)
		return
	}

	start, end, ranged := parseRangeHeader(rangeHdr, int64(len(data)))
	if !ranged {
		writeFull(w, data, corrupt)
		return
	}
	if start >= int64(len(data)) {
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	body := data[start : end+1]
	if corrupt {
		body = flipBytes(body)
	}
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusPartialContent)
	_, _ = w.Write(body)
}

func writeFull(w http.ResponseWriter, data []byte, corrupt bool) {
	if corrupt {
		data = flipBytes(data)
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// flipBytes 返回等长但内容被篡改的副本。
func flipBytes(b []byte) []byte {
	out := make([]byte, len(b))
	for i := range b {
		out[i] = b[i] ^ 0xFF
	}
	return out
}

// parseRangeHeader 解析 `bytes=start-end`。
func parseRangeHeader(v string, size int64) (int64, int64, bool) {
	if !strings.HasPrefix(v, "bytes=") || size <= 0 {
		return 0, 0, false
	}
	v = strings.TrimPrefix(v, "bytes=")
	parts := strings.SplitN(v, "-", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	start, err := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
	if err != nil || start < 0 {
		return 0, 0, false
	}
	end := size - 1
	if strings.TrimSpace(parts[1]) != "" {
		parsed, perr := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
		if perr != nil {
			return 0, 0, false
		}
		end = parsed
	}
	if end >= size {
		end = size - 1
	}
	if end < start {
		return 0, 0, false
	}
	return start, end, true
}

// —— 清单构造 ——

// sha256Hex 计算测试用摘要。
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// newBundle 构造包元数据，并把它的文件登记到假服务上。
func newBundle(s *rangeServer, bundleID, version string, roles []string, files map[string][]byte) ManifestBundle {
	b := ManifestBundle{
		BundleID:  bundleID,
		Name:      bundleID,
		Version:   version,
		Roles:     roles,
		License:   "MIT",
		Languages: []string{"zh"},
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)

	var total int64
	for _, name := range names {
		data := files[name]
		s.setFile(name, &testFile{Data: data, ETag: `"` + name + `-v1"`})
		b.Files = append(b.Files, ManifestFile{
			Path:   name,
			SHA256: sha256Hex(data),
			Size:   int64(len(data)),
			// 相对路径：由 fetchFrom 按当前基址展开，便于镜像改写测试。
			URL: name,
		})
		total += int64(len(data))
	}
	b.SizeBytes = total
	return b
}

// —— Manager 构造与状态观察 ——

// testOptions 返回适合单测的默认选项（小分片、无退避等待）。
func testOptions(root, baseURL string) Options {
	return Options{
		Root:             func() (string, error) { return root, nil },
		ManifestBaseURL:  baseURL,
		Backoff:          []time.Duration{time.Millisecond},
		ChunkSize:        16,
		ChunkConcurrency: 1,
	}
}

// stateRecorder 记录回调收到的下载 / 迁移状态。
type stateRecorder struct {
	mu        sync.Mutex
	downloads map[string]contract.DownloadState
	migrates  []contract.MigrateState
}

func newStateRecorder() *stateRecorder {
	return &stateRecorder{downloads: make(map[string]contract.DownloadState)}
}

func (r *stateRecorder) onDownload(st contract.DownloadState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.downloads[st.BundleID] = st
}

func (r *stateRecorder) onMigrate(st contract.MigrateState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.migrates = append(r.migrates, st)
}

func (r *stateRecorder) download(bundleID string) (contract.DownloadState, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.downloads[bundleID]
	return st, ok
}

func (r *stateRecorder) resetDownload(bundleID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.downloads, bundleID)
}

func (r *stateRecorder) phases() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.migrates))
	for _, st := range r.migrates {
		out = append(out, st.Phase)
	}
	return out
}

// waitDownload 等到某 bundle 进入终态（done / failed / paused）。
func (r *stateRecorder) waitDownload(t *testing.T, bundleID string) contract.DownloadState {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if st, ok := r.download(bundleID); ok {
			switch st.State {
			case contract.DownloadDone, contract.DownloadFailed, contract.DownloadPaused:
				return st
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("download %q did not reach a terminal state", bundleID)
	return contract.DownloadState{}
}

// newTestManager 构造一个只依赖本地临时目录与假服务的 Manager。
func newTestManager(t *testing.T, opts Options) (*Manager, *stateRecorder) {
	t.Helper()
	rec := newStateRecorder()
	opts.OnDownload = rec.onDownload
	opts.OnMigrate = rec.onMigrate
	m := New(opts)
	t.Cleanup(m.shutdownForTest)
	return m, rec
}

// shutdownForTest 停止所有运行中的下载，避免测试间串扰。
func (m *Manager) shutdownForTest() {
	m.taskMu.Lock()
	tasks := make([]*downloadTask, 0, len(m.tasks))
	for _, t := range m.tasks {
		tasks = append(tasks, t)
	}
	m.taskMu.Unlock()
	for _, t := range tasks {
		t.cancel()
		select {
		case <-t.done:
		case <-time.After(5 * time.Second):
		}
	}
}
