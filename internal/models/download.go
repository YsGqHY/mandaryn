package models

// 分片下载（FR-05 / ARCH §4.6.2 六步）。
//
// 流程要点：
//  1. 预检：空间 ≥ size*1.15、目标可写；
//  2. 分片：默认 4 并发、每片 8MB，Range 请求；
//  3. 续传：读 .state.json，ETag 不变 → If-Range 续，ETag 变 → **整包重下**
//     （避免把新旧内容拼在一起产生「看似成功、实际脏数据」）；
//  4. 单文件完成 → 流式 sha256 复核，不一致删除重下（最多 1 次）；
//  5. 整包完成 → 逐文件复核 + 写 manifest.json → 原子 rename 到 models/<id>@<ver>/；
//  6. 失败 → 退避 1s/4s/16s → 换镜像（最多 3 个）→ 收敛为错误。
//
// 并发上限：同时最多 2 个 bundle 下载（dlSem），避免占满带宽影响转写。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/outbound"
	"vip.oberon.mandaryn/internal/storage"
	"vip.oberon.mandaryn/internal/utils/filex"
	"vip.oberon.mandaryn/internal/utils/httpx"
)

// chunkTimeout 单个分片请求的超时上限。
//
// 必须显式给 deadline：httpx.NewRequest 在 ctx 无 deadline 时会加 30s 默认值，
// 那对大分片（慢网下 8MB 可能超过 30s）会误杀。
const chunkTimeout = 10 * time.Minute

// 分片请求的两种「需要整包重下」信号。
var (
	// errNeedRestart 表示服务端忽略了 Range / If-Range 不匹配，必须整包重下。
	errNeedRestart = errors.New("models: server returned full body, restart required")
	// errRangeUnsupported 表示服务端不支持 Range（416），同样只能整包重下。
	errRangeUnsupported = errors.New("models: range not satisfiable")
)

// chunkRange 是一个分片区间（end 含）。
type chunkRange struct {
	idx   int
	start int64
	end   int64
}

// downloadStateFile 是 `.downloads/<bundleId>@<version>.state.json` 的内容。
type downloadStateFile struct {
	BundleID string `json:"bundleId"`
	Version  string `json:"version"`
	// URLIdx 当前镜像索引（0 = 主基址）。
	URLIdx int `json:"urlsIdx"`
	// Retries 累计重试次数（诊断用）。
	Retries    int    `json:"retries"`
	ETag       string `json:"etag,omitempty"`
	BytesDone  int64  `json:"bytesDone"`
	BytesTotal int64  `json:"bytesTotal"`
	// Files 按相对路径记录每个文件的断点。
	Files []*fileProgress `json:"files"`
}

// fileProgress 是单个文件的断点。
type fileProgress struct {
	Path      string `json:"path"`
	Size      int64  `json:"size,omitempty"`
	ETag      string `json:"etag,omitempty"`
	PartsDone []int  `json:"partsDone,omitempty"`
	BytesDone int64  `json:"bytesDone,omitempty"`
	Done      bool   `json:"done,omitempty"`
}

// file 返回（并按需创建）某文件的断点记录。
func (s *downloadStateFile) file(rel string) *fileProgress {
	for _, f := range s.Files {
		if f.Path == rel {
			return f
		}
	}
	fp := &fileProgress{Path: rel}
	s.Files = append(s.Files, fp)
	return fp
}

func (s *downloadStateFile) hasPart(fp *fileProgress, idx int) bool {
	for _, v := range fp.PartsDone {
		if v == idx {
			return true
		}
	}
	return false
}

func (s *downloadStateFile) markPart(fp *fileProgress, idx int, n int64) {
	if s.hasPart(fp, idx) {
		return
	}
	fp.PartsDone = append(fp.PartsDone, idx)
	fp.BytesDone += n
	s.BytesDone += n
}

// downloadTask 是一次 bundle 下载的运行时状态。
type downloadTask struct {
	id     string
	bundle ManifestBundle

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	mu          sync.Mutex
	state       contract.DownloadState
	paused      bool
	canceled    bool
	removeParts bool
	stateFile   string
}

func (t *downloadTask) snapshot() contract.DownloadState {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.state
	if st.LastError != nil {
		e := *st.LastError
		st.LastError = &e
	}
	return st
}

func (t *downloadTask) setState(s string) {
	t.mu.Lock()
	t.state.State = s
	t.mu.Unlock()
}

func (t *downloadTask) setError(err error) {
	t.mu.Lock()
	t.state.LastError = errDTO(err)
	if err != nil {
		t.state.State = contract.DownloadFailed
	}
	t.mu.Unlock()
}

func (t *downloadTask) isPaused() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.paused
}

func (t *downloadTask) isCanceled() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.canceled
}

// progressEmitter 4Hz 节流进度事件。
type progressEmitter struct {
	mu   sync.Mutex
	last time.Time
	fn   func(contract.DownloadState)
}

func (p *progressEmitter) emit(st contract.DownloadState, force bool) {
	if p.fn == nil {
		return
	}
	p.mu.Lock()
	now := time.Now()
	if !force && !p.last.IsZero() && now.Sub(p.last) < progressInterval {
		p.mu.Unlock()
		return
	}
	p.last = now
	p.mu.Unlock()
	p.fn(st)
}

// downloadRun 把一次「运行中的下载」需要的上下文收在一起，
// 避免每个 helper 都带一长串参数。
type downloadRun struct {
	m     *Manager
	t     *downloadTask
	root  string
	work  string
	state *downloadStateFile

	mu       sync.Mutex
	lastSave time.Time
	started  time.Time
	emitter  *progressEmitter
}

// StartDownload 开始 / 继续下载（API-6.3）。
func (m *Manager) StartDownload(ctx context.Context, bundleID string) (contract.DownloadState, error) {
	if err := m.checkBusy(); err != nil {
		return contract.DownloadState{}, err
	}
	if err := outbound.Guard(); err != nil {
		return contract.DownloadState{}, err
	}

	bundle, err := m.lookup(ctx, bundleID)
	if err != nil {
		return contract.DownloadState{}, err
	}
	if err := validateDownloadBundle(bundle); err != nil {
		return contract.DownloadState{}, err
	}

	// 已有任务：运行中直接返回快照；已结束则等它退出后重建。
	m.taskMu.Lock()
	if prev, ok := m.tasks[bundleID]; ok {
		st := prev.snapshot()
		if st.State == contract.DownloadQueued || st.State == contract.DownloadDownloading {
			m.taskMu.Unlock()
			return st, nil
		}
		m.taskMu.Unlock()
		select {
		case <-prev.done:
		case <-ctx.Done():
			return st, apierr.Wrap(ctx.Err(), apierr.CodeDownloadNetwork)
		}
		m.taskMu.Lock()
		delete(m.tasks, bundleID)
	}
	m.taskMu.Unlock()

	id := bundleDirName(bundle.BundleID, bundle.Version)
	total := totalBytesOf(bundle)
	taskCtx, cancel := context.WithCancel(context.Background())
	t := &downloadTask{
		id:     id,
		bundle: bundle,
		ctx:    taskCtx,
		cancel: cancel,
		done:   make(chan struct{}),
		state: contract.DownloadState{
			BundleID:   bundle.BundleID,
			State:      contract.DownloadQueued,
			BytesTotal: total,
		},
	}

	m.taskMu.Lock()
	m.tasks[bundleID] = t
	m.taskMu.Unlock()

	go m.runDownload(t)
	return t.snapshot(), nil
}

// Download 是 StartDownload 的别名（内部命名统一用 Start*，对外用文档动词）。
func (m *Manager) Download(ctx context.Context, bundleID string) (contract.DownloadState, error) {
	return m.StartDownload(ctx, bundleID)
}

// PauseDownload 暂停下载并保留分片（API-6.4）。
func (m *Manager) PauseDownload(bundleID string) error {
	t := m.findTask(bundleID)
	if t == nil {
		return apierr.Newf(apierr.CodeNotFound, "no download in progress for %q", bundleID)
	}
	t.mu.Lock()
	t.paused = true
	t.mu.Unlock()
	t.cancel()
	return nil
}

// CancelDownload 取消下载（API-6.5）；removeParts=true 时同时清理分片与断点。
func (m *Manager) CancelDownload(bundleID string, removeParts bool) error {
	t := m.findTask(bundleID)
	if t == nil {
		// 没有运行中的任务：若要求清理分片，仍执行一次（幂等）。
		if removeParts {
			return m.removeDownloadArtifacts(bundleID)
		}
		return apierr.Newf(apierr.CodeNotFound, "no download in progress for %q", bundleID)
	}
	t.mu.Lock()
	t.canceled = true
	t.removeParts = removeParts
	t.mu.Unlock()
	t.cancel()
	return nil
}

// ActiveDownloads 返回当前占用下载槽位的 bundle 数（诊断与单测用）。
func (m *Manager) ActiveDownloads() int { return len(m.dlSem) }

func (m *Manager) findTask(bundleID string) *downloadTask {
	m.taskMu.Lock()
	defer m.taskMu.Unlock()
	return m.tasks[bundleID]
}

// removeDownloadArtifacts 清理某 bundle 的全部分片与断点。
func (m *Manager) removeDownloadArtifacts(bundleID string) error {
	root, err := m.root()
	if err != nil {
		return apierr.Wrap(err, apierr.CodeInvalidArgument)
	}
	dlDir := filepath.Join(root, DirDownloads)
	// 分片目录命名带版本（bundleId@version），按前缀匹配一并清掉。
	entries, _ := os.ReadDir(dlDir)
	ids := make([]string, 0, 2)
	for _, e := range entries {
		name := e.Name()
		base := strings.TrimSuffix(name, ".state.json")
		if name == bundleID || base == bundleID || strings.HasPrefix(name, bundleID+"@") {
			_ = os.RemoveAll(filepath.Join(dlDir, name))
		}
		if base != "" && (base == bundleID || strings.HasPrefix(base, bundleID+"@")) {
			ids = append(ids, base)
		}
	}
	ids = append(ids, bundleID)
	if db := m.db(); db != nil {
		// 用显式 id 列表而不是 LIKE：`_` 在 SQL LIKE 里是通配符，
		// 用 LIKE 前缀可能误删其它 bundle 的断点行。
		_ = db.Where("id IN ?", ids).Delete(&storage.Download{}).Error
	}
	return nil
}

// runDownload 是下载协程主体：排队 → 执行 → 收敛状态。
func (m *Manager) runDownload(t *downloadTask) {
	defer close(t.done)
	defer t.cancel()

	m.emitDownload(t)

	select {
	case m.dlSem <- struct{}{}:
	case <-t.ctx.Done():
		m.finishTask(t, t.ctx.Err())
		return
	}
	defer func() { <-m.dlSem }()

	err := m.executeDownload(t)
	m.finishTask(t, err)
}

// finishTask 按错误类别收敛最终状态并清理任务表。
func (m *Manager) finishTask(t *downloadTask, err error) {
	switch {
	case err == nil:
		t.mu.Lock()
		t.state.State = contract.DownloadDone
		t.state.LastError = nil
		t.state.BytesDone = t.state.BytesTotal
		t.state.EtaMs = 0
		t.mu.Unlock()
	case t.isCanceled():
		t.setState(contract.DownloadFailed)
		t.mu.Lock()
		t.state.LastError = errDTO(apierr.Newf(apierr.CodeDownloadNetwork, "download canceled"))
		removeParts := t.removeParts
		t.mu.Unlock()
		if removeParts {
			if rerr := m.removeDownloadArtifacts(t.bundle.BundleID); rerr != nil {
				m.log().Warn("remove download parts failed", "bundle", t.bundle.BundleID, "error", rerr.Error())
			}
		}
	case t.isPaused():
		t.setState(contract.DownloadPaused)
	default:
		t.setError(err)
		m.log().Warn("download failed", "bundle", t.bundle.BundleID, "error", err.Error())
	}

	// 先落库再发事件：事件消费方（前端 / 其他模块）可能立刻回读 downloads 表。
	m.persistDownloadRow(t)
	m.emitDownload(t)

	if st := t.snapshot(); st.State == contract.DownloadDone {
		m.notifyChanged(contract.ModelChanged{BundleID: t.bundle.BundleID, Installed: true, Verified: true})
	}
	if st := t.snapshot(); st.State == contract.DownloadDone || st.State == contract.DownloadFailed {
		// 结束态任务不留在表里：下一次 Download 从断点文件重建，避免状态粘连。
		if m.findTask(t.bundle.BundleID) == t {
			m.taskMu.Lock()
			delete(m.tasks, t.bundle.BundleID)
			m.taskMu.Unlock()
		}
	}
}

// executeDownload 执行真实下载流程。
func (m *Manager) executeDownload(t *downloadTask) error {
	root, err := m.root()
	if err != nil {
		return apierr.Wrap(err, apierr.CodeInvalidArgument)
	}
	modelsDir := filepath.Join(root, DirModels)
	dlDir := filepath.Join(root, DirDownloads)
	work := filepath.Join(dlDir, t.id)
	statePath := filepath.Join(dlDir, t.id+".state.json")
	t.stateFile = statePath

	if err := filex.EnsureDir(modelsDir); err != nil {
		return apierr.Wrap(err, apierr.CodePathUnwritable).WithDetail("cannot create models dir")
	}
	if err := filex.EnsureDir(work); err != nil {
		return apierr.Wrap(err, apierr.CodePathUnwritable).WithDetail("cannot create download workspace")
	}

	// —— 1. 预检：磁盘空间 ≥ size*1.15 ——
	total := totalBytesOf(t.bundle)
	if total > 0 {
		if free, ferr := freeBytes(root); ferr == nil && free > 0 {
			need := int64(float64(total) * diskOverheadFactor)
			if free < need {
				return apierr.Newf(apierr.CodeDownloadDiskFull,
					"need %d bytes free, only %d available", need, free)
			}
		}
	}

	// —— 3. 续传：读断点 ——
	st := m.loadStateFile(statePath)
	if st == nil || st.BundleID != t.bundle.BundleID || st.Version != t.bundle.Version {
		st = &downloadStateFile{
			BundleID:   t.bundle.BundleID,
			Version:    t.bundle.Version,
			BytesTotal: total,
		}
	}

	d := &downloadRun{
		m:       m,
		t:       t,
		root:    root,
		work:    work,
		state:   st,
		started: time.Now(),
		emitter: &progressEmitter{fn: m.opts.OnDownload},
	}
	t.mu.Lock()
	t.state.State = contract.DownloadDownloading
	t.state.BytesTotal = total
	t.state.BytesDone = st.BytesDone
	t.state.Mirror = m.mirrorLabel(st.URLIdx)
	t.mu.Unlock()
	d.emit(true)

	// —— 2/4. 逐文件分片下载 + 校验 ——
	for _, f := range t.bundle.Files {
		if err := t.ctx.Err(); err != nil {
			return err
		}
		rel, perr := sanitizeRelPath(f.Path)
		if perr != nil {
			return apierr.Newf(apierr.CodeInvalidArgument, "unsafe file path %q", f.Path).WithCause(perr)
		}
		dest := filepath.Join(work, rel)
		fp := st.file(rel)
		if fp.Done {
			if _, serr := os.Stat(dest); serr == nil {
				continue
			}
			fp.Done = false
			fp.PartsDone = nil
			fp.BytesDone = 0
		}
		if err := d.downloadFile(f, rel, dest, fp); err != nil {
			d.saveState(true)
			return err
		}
		d.saveState(true)
	}

	// —— 5. 校验归档后构建安装树，写 manifest.json 并原子提交 ——
	installBundle := t.bundle
	installDir := work
	starterArchive := isApprovedSenseVoiceStarterArchive(t.bundle)
	if starterArchive {
		staging, serr := os.MkdirTemp(modelsDir, DirStagingPrefix+"sensevoice-")
		if serr != nil {
			return apierr.Wrap(serr, apierr.CodePathUnwritable).WithDetail("create starter extraction staging")
		}
		defer func() { _ = os.RemoveAll(staging) }()

		archivePath := filepath.Join(work, SenseVoiceINT8ArchivePath)
		files, extractedBytes, xerr := extractSenseVoiceStarterArchive(t.ctx, archivePath, staging)
		if xerr != nil {
			return apierr.New(apierr.CodeModelCorrupt).
				WithDetail("extract approved SenseVoice starter archive: %s", xerr.Error()).WithCause(xerr)
		}
		installBundle.Files = files
		installBundle.SizeBytes = extractedBytes
		installDir = staging
	}
	if err := writeJSONAtomic(filepath.Join(installDir, FileBundleManifest), installBundle); err != nil {
		return apierr.Wrap(err, apierr.CodePathUnwritable).WithDetail("write bundle manifest")
	}
	t.setState(contract.DownloadVerifying)
	d.emit(true)

	if bad := verifyDir(installDir, installBundle); len(bad) > 0 {
		return apierr.Newf(apierr.CodeDownloadChecksum,
			"bundle verification failed for %d file(s)", len(bad))
	}
	if err := t.ctx.Err(); err != nil {
		return err
	}

	target := filepath.Join(modelsDir, t.id)
	if starterArchive {
		if err := commitSenseVoiceStarterDir(installDir, target); err != nil {
			return apierr.Wrap(err, apierr.CodePathUnwritable).WithDetail("commit starter bundle directory")
		}
	} else if err := commitDir(installDir, target); err != nil {
		return apierr.Wrap(err, apierr.CodePathUnwritable).WithDetail("commit bundle directory")
	}
	_ = os.Remove(statePath)
	if starterArchive {
		if err := os.RemoveAll(work); err != nil {
			m.log().Warn("remove starter archive workspace failed", "bundle", t.bundle.BundleID, "error", err.Error())
		}
	}

	m.upsertBundle(installBundle, filepath.Join(DirModels, t.id), contract.BundleInstalled)
	m.log().Info("model bundle installed",
		"bundle", t.bundle.BundleID, "version", t.bundle.Version, "bytes", total)
	return nil
}

// downloadFile 下载单个文件（含整包重下的重试）。
func (d *downloadRun) downloadFile(f ManifestFile, rel, dest string, fp *fileProgress) error {
	size := f.Size
	if size <= 0 {
		size = fp.Size
	}
	if size <= 0 {
		probed, err := d.m.probeFileSize(d.t.ctx, f.URL)
		if err != nil {
			return err
		}
		size = probed
	}
	if isSenseVoiceStarterBundle(d.t.bundle) && rel == SenseVoiceINT8ArchivePath &&
		(size <= 0 || size > maxSenseVoiceArchiveBytes) {
		return apierr.New(apierr.CodeDownloadChecksum).
			WithDetail("SenseVoice starter archive exceeds the approved size limit")
	}
	fp.Size = size

	for attempt := 1; attempt <= maxFileAttempts; attempt++ {
		if info, serr := os.Stat(dest); serr == nil && info.Size() != size {
			// 残留下尺寸不对的分片文件：丢弃重下，避免 WriteAt 稀疏文件长度错乱。
			_ = os.Remove(dest)
			resetProgress(fp)
		}
		err := d.fetchChunks(f, dest, chunkRanges(size, d.m.opts.ChunkSize), fp)
		if errors.Is(err, errNeedRestart) || errors.Is(err, errRangeUnsupported) {
			d.m.log().Warn("download must restart from scratch",
				"file", rel, "reason", err.Error())
			_ = os.Remove(dest)
			resetProgress(fp)
			// 必须清掉旧 ETag：否则下一次请求还会带同样的 If-Range，
			// 服务端仍然回整包，重试必然再次失败（见 DEV-ISSUES S6-03）。
			d.setETag("")
			d.saveState(true)
			continue
		}
		if err != nil {
			return err
		}

		// —— 4. 单文件 sha256 复核 ——
		if f.SHA256 != "" {
			got, _, herr := sha256File(dest)
			if herr != nil {
				return apierr.Wrap(herr, apierr.CodeDownloadChecksum)
			}
			if !strings.EqualFold(got, f.SHA256) {
				d.m.log().Warn("file checksum mismatch",
					"file", rel, "attempt", attempt, "expected", f.SHA256, "actual", got)
				_ = os.Remove(dest)
				resetProgress(fp)
				d.saveState(true)
				if attempt == maxFileAttempts {
					return apierr.Newf(apierr.CodeDownloadChecksum,
						"file %s failed SHA-256 verification", rel)
				}
				continue
			}
		}
		fp.Done = true
		fp.BytesDone = size
		return nil
	}
	return apierr.Newf(apierr.CodeDownloadChecksum, "file %s could not be verified", rel)
}

// fetchChunks 用固定并发下载全部分片。
func (d *downloadRun) fetchChunks(f ManifestFile, dest string, chunks []chunkRange, fp *fileProgress) error {
	pending := make([]chunkRange, 0, len(chunks))
	for _, ch := range chunks {
		if !d.state.hasPart(fp, ch.idx) {
			pending = append(pending, ch)
		}
	}
	// 已全部分片完成但文件被删：重下
	if len(pending) == 0 && len(chunks) > 0 {
		if _, err := os.Stat(dest); err == nil {
			return nil
		}
		pending = chunks
		resetProgress(fp)
	}

	file, err := os.OpenFile(dest, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return apierr.Wrap(err, apierr.CodePathUnwritable)
	}
	defer file.Close()
	if fp.Size > 0 {
		_ = file.Truncate(fp.Size)
	}

	ctx, cancel := context.WithCancel(d.t.ctx)
	defer cancel()

	conc := d.m.opts.ChunkConcurrency
	if conc > len(pending) {
		conc = len(pending)
	}
	if conc <= 0 {
		conc = 1
	}

	queue := make(chan chunkRange)
	var wg sync.WaitGroup
	var once sync.Once
	var firstErr error

	for i := 0; i < conc; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ch := range queue {
				if err := d.fetchChunk(ctx, f, file, ch, fp); err != nil {
					once.Do(func() {
						firstErr = err
						cancel()
					})
					return
				}
			}
		}()
	}

feed:
	for _, ch := range pending {
		select {
		case queue <- ch:
		case <-ctx.Done():
			break feed
		}
	}
	close(queue)
	wg.Wait()

	if firstErr != nil {
		return firstErr
	}
	if err := d.t.ctx.Err(); err != nil {
		return err
	}
	return ctx.Err()
}

// fetchChunk 下载单个分片；失败按「退避 → 换镜像」收敛。
func (d *downloadRun) fetchChunk(ctx context.Context, f ManifestFile, file *os.File, ch chunkRange, fp *fileProgress) error {
	bases := d.m.mirrorBases()
	backoff := d.m.opts.Backoff
	var lastErr error

	startIdx := d.mirrorIndex()

	for idx := startIdx; idx <= len(bases); idx++ {
		if idx > startIdx {
			// 换镜像：记录到断点，便于 UI 展示「正在用备用源」。
			d.setMirrorIndex(idx)
		}
		url := mirrorURL(f.URL, idx, bases)
		for retry := 0; retry <= len(backoff); retry++ {
			if retry > 0 {
				wait := backoff[retry-1]
				d.bumpRetries()
				if wait > 0 {
					select {
					case <-time.After(wait):
					case <-ctx.Done():
						return ctx.Err()
					}
				}
			}
			err := d.doChunkRequest(ctx, url, ch, file, fp)
			if err == nil {
				return nil
			}
			if errors.Is(err, errNeedRestart) || errors.Is(err, errRangeUnsupported) {
				return err
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			lastErr = err
			d.m.log().Warn("chunk request failed",
				"mirror", idx, "retry", retry, "range", fmt.Sprintf("%d-%d", ch.start, ch.end),
				"error", err.Error())
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no mirror available")
	}
	return apierr.New(apierr.CodeDownloadNetwork).WithCause(lastErr)
}

// doChunkRequest 发一次 Range 请求并把内容写到目标偏移。
func (d *downloadRun) doChunkRequest(ctx context.Context, rawURL string, ch chunkRange, file *os.File, fp *fileProgress) error {
	reqCtx, cancel := context.WithTimeout(ctx, chunkTimeout)
	defer cancel()

	req, cancelReq, err := httpx.NewRequest(reqCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	defer cancelReq()
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", ch.start, ch.end))
	if etag := d.etag(); etag != "" {
		req.Header.Set("If-Range", etag)
	}

	// 代理感知 client：分片下载同样要遵守用户的代理设置（FR-05-5）。
	client, cerr := d.m.httpClient()
	if cerr != nil {
		return cerr
	}
	resp, err := client.Do(req)
	if err != nil {
		d.recordOutbound(rawURL, 0, apierr.CodeDownloadNetwork)
		return err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		_ = resp.Body.Close()
	}()

	switch resp.StatusCode {
	case http.StatusPartialContent:
		if etag := resp.Header.Get("ETag"); etag != "" {
			d.setETag(etag)
		}
		n, cerr := io.Copy(&sectionWriter{w: file, off: ch.start}, resp.Body)
		if cerr != nil {
			d.recordOutbound(rawURL, n, apierr.CodeDownloadNetwork)
			return cerr
		}
		d.stateMarkPart(fp, ch.idx, n)
		d.progress(n)
		d.recordOutbound(rawURL, n, "ok")
		return nil
	case http.StatusOK:
		// 服务端忽略了 Range，或 If-Range 不匹配（ETag 已变）→ 必须整包重下。
		d.recordOutbound(rawURL, 0, apierr.CodeDownloadChecksum)
		return errNeedRestart
	case http.StatusRequestedRangeNotSatisfiable:
		d.recordOutbound(rawURL, 0, apierr.CodeDownloadNetwork)
		return errRangeUnsupported
	default:
		d.recordOutbound(rawURL, 0, apierr.CodeDownloadNetwork)
		return fmt.Errorf("unexpected status %s", resp.Status)
	}
}

// probeFileSize 用 Range: bytes=0-0 探测远端文件长度（不下载整包）。
func (m *Manager) probeFileSize(ctx context.Context, rawURL string) (int64, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, cancelReq, err := httpx.NewRequest(reqCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, apierr.Wrap(err, apierr.CodeDownloadNetwork)
	}
	defer cancelReq()
	req.Header.Set("Range", "bytes=0-0")

	client, cerr := m.httpClient()
	if cerr != nil {
		return 0, cerr
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, apierr.Wrap(err, apierr.CodeDownloadNetwork)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		_ = resp.Body.Close()
	}()

	if total, ok := parseContentRangeTotal(resp.Header.Get("Content-Range")); ok {
		return total, nil
	}
	if resp.StatusCode == http.StatusOK && resp.ContentLength > 0 {
		return resp.ContentLength, nil
	}
	return 0, apierr.New(apierr.CodeDownloadNetwork).
		WithDetail("cannot determine remote file size")
}

// parseContentRangeTotal 解析 `bytes 0-0/12345` 里的总长度。
func parseContentRangeTotal(v string) (int64, bool) {
	if v == "" {
		return 0, false
	}
	i := strings.LastIndexByte(v, '/')
	if i < 0 || i == len(v)-1 {
		return 0, false
	}
	var total int64
	for _, r := range v[i+1:] {
		if r < '0' || r > '9' {
			return 0, false
		}
		total = total*10 + int64(r-'0')
	}
	return total, total > 0
}

// chunkRanges 把文件切成固定大小的分片（end 含）。
func chunkRanges(size, chunk int64) []chunkRange {
	if size <= 0 {
		return nil
	}
	if chunk <= 0 {
		chunk = defaultChunkSize
	}
	n := (size + chunk - 1) / chunk
	out := make([]chunkRange, 0, n)
	var idx int
	for start := int64(0); start < size; start += chunk {
		end := start + chunk - 1
		if end >= size {
			end = size - 1
		}
		out = append(out, chunkRange{idx: idx, start: start, end: end})
		idx++
	}
	return out
}

// sectionWriter 把顺序写入映射到文件指定偏移（分片并发写同一文件）。
type sectionWriter struct {
	w   io.WriterAt
	off int64
}

func (s *sectionWriter) Write(p []byte) (int, error) {
	n, err := s.w.WriteAt(p, s.off)
	s.off += int64(n)
	return n, err
}

// resetProgress 清空单文件断点（整包重下时使用）。
func resetProgress(fp *fileProgress) {
	fp.PartsDone = nil
	fp.BytesDone = 0
	fp.Done = false
}

// —— 断点状态 ——

func (m *Manager) loadStateFile(p string) *downloadStateFile {
	var st downloadStateFile
	if err := readJSONLimit(p, 4<<20, &st); err != nil {
		return nil
	}
	return &st
}

// saveState 落断点（1s 节流；force=true 立即写）。
func (d *downloadRun) saveState(force bool) {
	d.mu.Lock()
	if !force && !d.lastSave.IsZero() && time.Since(d.lastSave) < time.Second {
		d.mu.Unlock()
		return
	}
	d.lastSave = time.Now()
	st := d.state
	path := d.t.stateFile
	d.mu.Unlock()

	if path == "" {
		return
	}
	if err := writeJSONAtomic(path, st); err != nil {
		d.m.log().Warn("write download state failed", "error", err.Error())
	}
}

func (d *downloadRun) etag() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.state.ETag != "" {
		return d.state.ETag
	}
	return ""
}

func (d *downloadRun) setETag(v string) {
	d.mu.Lock()
	d.state.ETag = v
	d.mu.Unlock()
}

func (d *downloadRun) mirrorIndex() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.state.URLIdx
}

func (d *downloadRun) setMirrorIndex(i int) {
	d.mu.Lock()
	d.state.URLIdx = i
	d.mu.Unlock()
	d.t.mu.Lock()
	d.t.state.Mirror = d.m.mirrorLabel(i)
	d.t.mu.Unlock()
}

func (d *downloadRun) bumpRetries() {
	d.mu.Lock()
	d.state.Retries++
	n := d.state.Retries
	d.mu.Unlock()
	d.t.mu.Lock()
	d.t.state.Retries = n
	d.t.mu.Unlock()
}

func (d *downloadRun) stateMarkPart(fp *fileProgress, idx int, n int64) {
	d.mu.Lock()
	d.state.markPart(fp, idx, n)
	d.mu.Unlock()
	// 分片一落盘就刷新断点（1s 节流）：进程被杀后能续传，
	// 也保证「暂停」时磁盘上已有可观察的进度。
	d.saveState(false)
}

// progress 累加已下载字节并节流发事件。
func (d *downloadRun) progress(n int64) {
	d.t.mu.Lock()
	d.t.state.BytesDone += n
	done := d.t.state.BytesDone
	total := d.t.state.BytesTotal
	elapsed := time.Since(d.started)
	if elapsed > 0 && done > 0 {
		speed := int64(float64(done) / elapsed.Seconds())
		d.t.state.SpeedBps = speed
		if speed > 0 && total > done {
			d.t.state.EtaMs = int64(float64(total-done) / float64(speed) * 1000)
		} else {
			d.t.state.EtaMs = 0
		}
	}
	d.t.mu.Unlock()
	d.emit(false)
}

func (d *downloadRun) emit(force bool) {
	if d.emitter == nil {
		return
	}
	d.emitter.emit(d.t.snapshot(), force)
}

// recordOutbound 落出站留痕（只写域名，不写完整 URL）。
func (d *downloadRun) recordOutbound(rawURL string, n int64, result string) {
	if d.m.opts.Recorder == nil {
		return
	}
	d.m.opts.Recorder.Record(hostOf(rawURL), contract.OutboundModelDownload, false, n, result)
}

// emitDownload 把任务状态转发给 L3。
//
// 只在状态机节点（排队 / 开始 / 校验 / 结束）调用，字节级节流由 downloadRun.emitter 负责。
func (m *Manager) emitDownload(t *downloadTask) {
	if m.opts.OnDownload == nil {
		return
	}
	m.opts.OnDownload(t.snapshot())
}

// mirrorLabel 返回某镜像索引在 UI 上的展示串（只到 host，不含 query / 凭据）。
func (m *Manager) mirrorLabel(idx int) string {
	if idx <= 0 {
		return hostOf(catalogURL(m.manifestBase()))
	}
	bases := m.mirrorBases()
	if idx <= len(bases) {
		return hostOf(catalogURL(bases[idx-1]))
	}
	return ""
}

// notifyChanged 通知仓库变化（EV-10）。
func (m *Manager) notifyChanged(payload contract.ModelChanged) {
	if m.opts.OnChanged != nil {
		m.opts.OnChanged(payload)
	}
}

// persistDownloadRow 把断点状态同步到 downloads 表（TBL-09）。
func (m *Manager) persistDownloadRow(t *downloadTask) {
	db := m.db()
	if db == nil {
		return
	}
	st := t.snapshot()
	etag, parts := "", ""
	if t.stateFile != "" {
		if sf := m.loadStateFile(t.stateFile); sf != nil {
			etag = sf.ETag
			if buf, err := json.Marshal(sf.Files); err == nil {
				parts = string(buf)
			}
		}
	}
	row := storage.Download{
		ID:         t.id,
		BytesDone:  st.BytesDone,
		BytesTotal: st.BytesTotal,
		ETag:       etag,
		Retries:    st.Retries,
		State:      st.State,
		PartsDone:  parts,
	}
	if st.LastError != nil {
		row.LastError = st.LastError.Code
	}
	if err := db.Save(&row).Error; err != nil {
		m.log().Warn("persist download row failed", "error", err.Error())
	}
}

// verifyDir 逐文件复核目录内文件摘要，返回不一致（或缺失）的相对路径。
func verifyDir(dir string, b ManifestBundle) []string {
	var bad []string
	for _, f := range b.Files {
		rel, err := sanitizeRelPath(f.Path)
		if err != nil {
			bad = append(bad, f.Path)
			continue
		}
		got, _, herr := sha256File(filepath.Join(dir, rel))
		if herr != nil {
			bad = append(bad, rel)
			continue
		}
		if f.SHA256 != "" && !strings.EqualFold(got, f.SHA256) {
			bad = append(bad, rel)
		}
	}
	return bad
}
