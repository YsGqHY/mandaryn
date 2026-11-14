package transcriptsvc

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"vip.oberon.mandaryn/internal/asr"
	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/paths"
	"vip.oberon.mandaryn/internal/storage"
	"vip.oberon.mandaryn/internal/transcribe"
)

// —— 测试夹具 ——

// fakeEngine 是离线假引擎（固定文本，可注入错误、统计调用次数）。
type fakeEngine struct {
	id    string
	calls int
	err   error
	text  string
}

func (f *fakeEngine) ID() string { return f.id }

func (f *fakeEngine) Finalize(seg asr.AudioSegment) (asr.FinalResult, error) {
	f.calls++
	if f.err != nil {
		return asr.FinalResult{}, f.err
	}
	text := f.text
	if text == "" {
		text = "句"
	}
	return asr.FinalResult{
		Text:       text + string(rune('0'+seg.Seq%10)),
		EngineID:   f.id,
		Confidence: 0.9,
	}, nil
}

// fixture 是一套完整的服务环境：真实 SQLite + 临时 dataRoot + 假引擎。
type fixture struct {
	svc    *Service
	db     *storage.DB
	holder *storage.Holder
	engine *fakeEngine
	data   string
	dir    string
}

// newFixture 建库、建目录、造一个已封存的会话。
func newFixture(t *testing.T, withAudio bool) *fixture {
	t.Helper()
	dir := t.TempDir()
	dataRoot := filepath.Join(dir, "data")
	if err := os.MkdirAll(filepath.Join(dataRoot, paths.SessionsDirName), 0o755); err != nil {
		t.Fatalf("mkdir dataRoot: %v", err)
	}
	db, err := storage.Open(filepath.Join(dataRoot, paths.DBFileName))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	holder := storage.NewHolder(db)

	// 会话音频（相对 dataRoot 的路径，与 TBL-03 约定一致）。
	audioRel := filepath.Join(paths.SessionsDirName, "s_audio.wav")
	totalMs := int64(0)
	if withAudio {
		const sr = 16000
		samples := append(append(makeTone(1000, sr), makeSilence(600, sr)...), makeTone(800, sr)...)
		writeWAV(t, filepath.Join(dataRoot, audioRel), samples, sr)
		totalMs = int64(len(samples) * 1000 / sr)
	}

	row := storage.Session{
		ID:                  "s_audio01",
		Title:               "测试录音实例",
		SourceKind:          "application",
		ModelSource:         contract.ModelSourceLocal,
		EngineID:            "fake@1",
		Status:              contract.StateCompleted, // ST-06：录制已完成
		StartedAt:           time.Now().UnixMilli(),
		EndedAt:             time.Now().UnixMilli(),
		DurationMs:          totalMs,
		AudioPath:           audioRel,
		AudioFormat:         "wav",
		TranscriptionStatus: transcribe.StatusNone,
	}
	if err := db.GORM.Create(&row).Error; err != nil {
		t.Fatalf("seed session: %v", err)
	}

	// 会话事件（降级打点）——时间轴要能看到。
	if err := db.GORM.Create(&storage.SessionEvent{
		SessionID: row.ID, AtMs: 500, Kind: contract.MarkerDegrade, Note: "在线不可用，已回退本地",
	}).Error; err != nil {
		t.Fatalf("seed marker: %v", err)
	}

	resolver := paths.New(paths.Options{
		CLI: map[paths.Key]string{paths.KeyDataRoot: dataRoot},
		// 显式给默认层：测试机可能没有 %AppData% / $HOME，
		// 而 paths 的默认层解析失败会连带否决 CLI 接管（见 docs/DEV-ISSUES-S4.md S4-04）。
		DefaultDataRoot:  func() (string, error) { return dataRoot, nil },
		DefaultModelRoot: func() (string, error) { return filepath.Join(dir, "models"), nil },
	})

	eng := &fakeEngine{id: "fake@1"}
	svc, err := New(Deps{
		Holder:       holder,
		Engine:       eng,
		Resolver:     resolver,
		PollInterval: 2 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New service: %v", err)
	}
	t.Cleanup(svc.Close)

	return &fixture{svc: svc, db: db, holder: holder, engine: eng, data: dataRoot, dir: dir}
}

// session 读回会话行。
func (f *fixture) session(t *testing.T, id string) storage.Session {
	t.Helper()
	var row storage.Session
	if err := f.db.GORM.First(&row, "id = ?", id).Error; err != nil {
		t.Fatalf("read session: %v", err)
	}
	return row
}

// segments 读回片段（按 seq 升序）。
func (f *fixture) segments(t *testing.T, id string) []storage.Segment {
	t.Helper()
	var rows []storage.Segment
	if err := f.db.GORM.Where("session_id = ?", id).Order("seq asc").Find(&rows).Error; err != nil {
		t.Fatalf("read segments: %v", err)
	}
	return rows
}

// waitDone 等到转写轴到达 done。
func (f *fixture) waitDone(t *testing.T, id string) storage.Session {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		row := f.session(t, id)
		if row.TranscriptionStatus == transcribe.StatusDone {
			return row
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("超时等待转写完成，当前状态 %q", f.session(t, id).TranscriptionStatus)
	return storage.Session{}
}

// —— 音频帮助 ——

func makeTone(ms, sr int) []float32 {
	n := sr * ms / 1000
	out := make([]float32, n)
	for i := range out {
		out[i] = float32(0.3 * math.Sin(2*math.Pi*220*float64(i)/float64(sr)))
	}
	return out
}

func makeSilence(ms, sr int) []float32 { return make([]float32, sr*ms/1000) }

// writeWAV 写 16kHz 单声道 PCM16。
func writeWAV(t *testing.T, path string, samples []float32, sr int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	dataLen := len(samples) * 2
	le32 := func(v uint32) []byte {
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], v)
		return b[:]
	}
	le16 := func(v uint16) []byte {
		var b [2]byte
		binary.LittleEndian.PutUint16(b[:], v)
		return b[:]
	}
	var hdr []byte
	hdr = append(hdr, "RIFF"...)
	hdr = append(hdr, le32(uint32(36+dataLen))...)
	hdr = append(hdr, "WAVE"...)
	hdr = append(hdr, "fmt "...)
	hdr = append(hdr, le32(16)...)
	hdr = append(hdr, le16(1)...)
	hdr = append(hdr, le16(1)...)
	hdr = append(hdr, le32(uint32(sr))...)
	hdr = append(hdr, le32(uint32(sr*2))...)
	hdr = append(hdr, le16(2)...)
	hdr = append(hdr, le16(16)...)
	hdr = append(hdr, "data"...)
	hdr = append(hdr, le32(uint32(dataLen))...)

	body := make([]byte, dataLen)
	for i, s := range samples {
		binary.LittleEndian.PutUint16(body[i*2:], uint16(int16(math.Round(float64(s)*32767))))
	}
	if err := os.WriteFile(path, append(hdr, body...), 0o600); err != nil {
		t.Fatalf("write wav: %v", err)
	}
}
