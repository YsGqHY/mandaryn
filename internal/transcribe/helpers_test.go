package transcribe

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/asr"
)

// —— 测试替身：音频 ——

// writeWAV 生成 16kHz 单声道 PCM16 WAV 文件，返回路径。
//
// extraChunk=true 时在 fmt 与 data 之间插入一个 LIST 块：
// 真实录音常有附加块，硬编码 44 字节偏移的解析器在这里会失败。
func writeWAV(t *testing.T, dir string, samples []float32, sampleRate int, extraChunk bool) string {
	t.Helper()
	name := fmt.Sprintf("a_%d.wav", len(samples))
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create wav: %v", err)
	}
	defer func() { _ = f.Close() }()

	dataLen := len(samples) * 2
	extra := 0
	if extraChunk {
		extra = 8 + 4 // "LIST" + size + 4 字节内容
	}
	riffSize := 4 + (8 + 16) + extra + (8 + dataLen)

	var hdr []byte
	hdr = append(hdr, "RIFF"...)
	hdr = appendLE32(hdr, uint32(riffSize))
	hdr = append(hdr, "WAVE"...)
	hdr = append(hdr, "fmt "...)
	hdr = appendLE32(hdr, 16)
	hdr = appendLE16(hdr, 1)                       // PCM
	hdr = appendLE16(hdr, 1)                       // mono
	hdr = appendLE32(hdr, uint32(sampleRate))      // sample rate
	hdr = appendLE32(hdr, uint32(sampleRate*2))    // byte rate
	hdr = appendLE16(hdr, 2)                       // block align
	hdr = appendLE16(hdr, 16)                      // bits
	if extraChunk {
		hdr = append(hdr, "LIST"...)
		hdr = appendLE32(hdr, 4)
		hdr = append(hdr, "INFO"...)
	}
	hdr = append(hdr, "data"...)
	hdr = appendLE32(hdr, uint32(dataLen))

	if _, err := f.Write(hdr); err != nil {
		t.Fatalf("write header: %v", err)
	}
	buf := make([]byte, dataLen)
	for i, s := range samples {
		v := int16(math.Round(float64(s) * 32767))
		binary.LittleEndian.PutUint16(buf[i*2:], uint16(v))
	}
	if _, err := f.Write(buf); err != nil {
		t.Fatalf("write data: %v", err)
	}
	return path
}

func appendLE32(b []byte, v uint32) []byte {
	var tmp [4]byte
	binary.LittleEndian.PutUint32(tmp[:], v)
	return append(b, tmp[:]...)
}

func appendLE16(b []byte, v uint16) []byte {
	var tmp [2]byte
	binary.LittleEndian.PutUint16(tmp[:], v)
	return append(b, tmp[:]...)
}

// silence 生成静音（全零）。
func silence(ms, sampleRate int) []float32 {
	n := sampleRate * ms / 1000
	return make([]float32, n)
}

// tone 生成正弦「语音」（幅度 0.3 远高于门限 0.01）。
func tone(ms, sampleRate int) []float32 {
	n := sampleRate * ms / 1000
	out := make([]float32, n)
	for i := range out {
		out[i] = float32(0.3 * math.Sin(2*math.Pi*220*float64(i)/float64(sampleRate)))
	}
	return out
}

// concat 拼接多段音频。
func concat(parts ...[]float32) []float32 {
	var out []float32
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// openerFor 返回一个 AudioOpener：固定打开 path。
func openerFor(path string) AudioOpener {
	return func(_ context.Context, _ SessionRow) (AudioSource, error) {
		return os.Open(path)
	}
}

// —— 测试替身：引擎 ——

// fakeEngine 是离线假引擎：可返回固定文本、可注入错误、可计数。
type fakeEngine struct {
	mu       sync.Mutex
	id       string
	calls    int
	segments []asr.AudioSegment
	// text 返回该句文本；nil 时返回 "句<seq>。
	text func(seg asr.AudioSegment) string
	// err 返回错误（非 nil 时该句失败）。
	err func(seg asr.AudioSegment) error
	// onCall 在计数后调用（用来在「第 N 句时」触发取消 / 让位 / 阻塞）。
	onCall func(call int, seg asr.AudioSegment)
}

func newFakeEngine() *fakeEngine {
	return &fakeEngine{id: "fake@1"}
}

func (f *fakeEngine) ID() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.id
}

func (f *fakeEngine) Finalize(seg asr.AudioSegment) (asr.FinalResult, error) {
	f.mu.Lock()
	f.calls++
	call := f.calls
	f.segments = append(f.segments, seg)
	textFn, errFn, hook := f.text, f.err, f.onCall
	id := f.id
	f.mu.Unlock()

	if hook != nil {
		hook(call, seg)
	}
	if errFn != nil {
		if err := errFn(seg); err != nil {
			return asr.FinalResult{}, err
		}
	}
	text := fmt.Sprintf("句%d", seg.Seq)
	if textFn != nil {
		text = textFn(seg)
	}
	return asr.FinalResult{Text: text, EngineID: id, Confidence: 0.9}, nil
}

func (f *fakeEngine) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// resetCalls 清空计数（模拟「上一次运行」与「本次运行」的区分）。
func (f *fakeEngine) resetCalls() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = 0
	f.segments = nil
}

// —— 测试替身：Store ——

// fakeStore 是内存版 transcribe.Store。
//
// 刻意**不**提供任何写 `State` 的入口：转写状态与录制主状态正交，
// 若实现方想越界写录制状态，会在这里编译不过（见状态正交测试）。
type fakeStore struct {
	mu      sync.Mutex
	session SessionRow
	// segs 以 seq 为键：模拟 TBL-04 的 `(session_id, seq)` 唯一约束。
	segs    map[int]SegmentRow
	markers []MarkerRow
	// updateCalls 记录 UpdateTranscription 次数（诊断用）。
	updateCalls int
	// lastErr 注入持久化错误。
	lastErr error
	nextID      int64
}

func newFakeStore(s SessionRow) *fakeStore {
	if s.TranscriptionStatus == "" {
		s.TranscriptionStatus = StatusNone
	}
	if s.State == "" {
		s.State = "completed" // ST-06：录音实例已封存
	}
	return &fakeStore{session: s, segs: make(map[int]SegmentRow)}
}

func (f *fakeStore) LoadSession(_ context.Context, sessionID string) (SessionRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if sessionID != f.session.ID {
		return SessionRow{}, apierr.New(apierr.CodeNotFound)
	}
	return f.session, nil
}

func (f *fakeStore) UpdateTranscription(_ context.Context, u TranscriptionUpdate) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lastErr != nil {
		return f.lastErr
	}
	if u.SessionID != f.session.ID {
		return apierr.New(apierr.CodeNotFound)
	}
	f.updateCalls++
	if u.Status != "" {
		f.session.TranscriptionStatus = u.Status
	}
	if u.ProgressMs != nil {
		f.session.TranscribeProgressMs = *u.ProgressMs
	}
	if u.EngineID != nil {
		f.session.TranscribeEngineID = *u.EngineID
	}
	if u.TranscribedAt != nil {
		f.session.TranscribedAt = *u.TranscribedAt
	}
	if u.SegmentCount != nil {
		f.session.SegmentCount = *u.SegmentCount
	}
	if u.CharCount != nil {
		f.session.CharCount = *u.CharCount
	}
	// 注意：这里**没有** f.session.State 的赋值——这正是正交性的保证。
	return nil
}

func (f *fakeStore) UpsertSegment(_ context.Context, row SegmentRow) (SegmentRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lastErr != nil {
		return SegmentRow{}, f.lastErr
	}
	if row.SessionID != f.session.ID {
		return SegmentRow{}, apierr.New(apierr.CodeNotFound)
	}
	if exist, ok := f.segs[row.Seq]; ok {
		// 幂等覆盖：保留 ID 与 Edited（人工修正优先级最高）。
		exist.StartMs = row.StartMs
		exist.EndMs = row.EndMs
		if !exist.Edited {
			exist.Text = row.Text
		}
		exist.EngineID = row.EngineID
		exist.Confidence = row.Confidence
		exist.IsFinal = row.IsFinal
		f.segs[row.Seq] = exist
		return exist, nil
	}
	f.nextID++
	row.ID = f.nextID
	if !row.IsFinal && !row.Edited {
		row.IsFinal = true
	}
	f.segs[row.Seq] = row
	return row, nil
}

func (f *fakeStore) ListSegments(_ context.Context, q SegmentFilter) (SegmentPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var rows []SegmentRow
	for _, r := range f.segs {
		if q.SessionID != "" && r.SessionID != q.SessionID {
			continue
		}
		if q.Keyword != "" && !strings.Contains(r.Text, q.Keyword) {
			continue
		}
		if q.SpeakerID != "" && r.SpeakerID != q.SpeakerID {
			continue
		}
		if q.FromMs > 0 && r.EndMs < q.FromMs {
			continue
		}
		if q.ToMs > 0 && r.StartMs > q.ToMs {
			continue
		}
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Seq < rows[j].Seq })
	total := len(rows)
	if q.Offset > 0 {
		if q.Offset >= len(rows) {
			rows = nil
		} else {
			rows = rows[q.Offset:]
		}
	}
	if q.Limit > 0 && len(rows) > q.Limit {
		rows = rows[:q.Limit]
	}
	return SegmentPage{Total: total, Rows: rows}, nil
}

func (f *fakeStore) ListMarkers(_ context.Context, sessionID string) ([]MarkerRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if sessionID != f.session.ID {
		return nil, apierr.New(apierr.CodeNotFound)
	}
	out := make([]MarkerRow, len(f.markers))
	copy(out, f.markers)
	return out, nil
}

func (f *fakeStore) UpdateSegmentText(_ context.Context, segmentID int64, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for seq, r := range f.segs {
		if r.ID == segmentID {
			r.Text = text
			r.Edited = true
			f.segs[seq] = r
			return nil
		}
	}
	return apierr.New(apierr.CodeNotFound)
}

func (f *fakeStore) MaxSeq(_ context.Context, sessionID string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if sessionID != f.session.ID {
		return -1, apierr.New(apierr.CodeNotFound)
	}
	max := -1
	for seq := range f.segs {
		if seq > max {
			max = seq
		}
	}
	return max, nil
}

// snapshot 返回当前的会话行与片段（测试断言用）。
func (f *fakeStore) snapshot() (SessionRow, []SegmentRow) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rows := make([]SegmentRow, 0, len(f.segs))
	for _, r := range f.segs {
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Seq < rows[j].Seq })
	return f.session, rows
}

// setState 直接改录制主状态（模拟录制侧 / 其它路径写入，验证转写不会碰它）。
func (f *fakeStore) setState(state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.session.State = state
}

// —— 常用构造 ——

// twoSentenceStore 造一个「语音 1s + 静音 0.5s + 语音 0.8s」的会话（音频写在 dir 下）。
func twoSentenceStore(t *testing.T) (*fakeStore, string, int) {
	t.Helper()
	const sr = 16000
	dir := t.TempDir()
	audio := concat(tone(1000, sr), silence(500, sr), tone(800, sr))
	path := writeWAV(t, dir, audio, sr, false)
	totalMs := int64(len(audio) * 1000 / sr)
	st := newFakeStore(SessionRow{
		ID:         "s_test01",
		Title:      "测试会话",
		AudioPath:  filepath.Base(path),
		DurationMs: totalMs,
	})
	return st, path, sr
}
