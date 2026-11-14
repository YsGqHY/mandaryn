package sessions

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/storage"
)

// fixture 是一套测试环境：临时库 + 临时 dataRoot + 造好的语料。
type fixture struct {
	t        *testing.T
	db       *storage.DB
	holder   *storage.Holder
	dataRoot string
	repo     *Repo
}

// corpusSession 描述一条测试会话。
type corpusSession struct {
	ID          string
	Title       string
	SourceID    string
	SourceName  string
	Status      string
	ModelSource string
	Transcribe  string
	StartedAt   int64
	DurationMs  int64
	Audio       bool // 是否真的写一个音频文件
	Segments    []string
}

// corpus 是 12 条会话的固定语料。
//
// 设计原则：
//   - 覆盖 4 种录制状态 × 5 种转写状态 × local/online，供筛选断言使用；
//   - 中文与英文片段都有，用于验证「中文走 LIKE、英文走 FTS」两条路径；
//   - 时长跨越 4 个量级，用于时长区间筛选；
//   - started_at 连续递增，用于时间范围筛选与稳定排序。
var corpus = []corpusSession{
	{
		ID: "s_01", Title: "2026-11-10 10:00 · KOOK", SourceID: "app:kook.exe", SourceName: "KOOK",
		Status: "completed", ModelSource: "local", Transcribe: "done",
		StartedAt: 1_761_000_000_000, DurationMs: 90_000, Audio: true,
		Segments: []string{"今天的会议先对齐一下排期", "hello team we start now"},
	},
	{
		ID: "s_02", Title: "2026-11-10 12:30 · Discord", SourceID: "app:discord.exe", SourceName: "Discord",
		Status: "completed", ModelSource: "local", Transcribe: "done",
		StartedAt: 1_761_010_000_000, DurationMs: 3_600_000, Audio: true,
		Segments: []string{"这本小说的第三章节奏很好", "hello again from discord"},
	},
	{
		ID: "s_03", Title: "2026-11-11 09:00 · 系统声音", SourceID: "device:loopback", SourceName: "系统声音",
		Status: "interrupted", ModelSource: "online", Transcribe: "none",
		StartedAt: 1_761_100_000_000, DurationMs: 12_000, Audio: true,
		Segments: nil,
	},
	{
		ID: "s_04", Title: "2026-11-11 15:00 · KOOK", SourceID: "app:kook.exe", SourceName: "KOOK",
		Status: "failed", ModelSource: "online", Transcribe: "failed",
		StartedAt: 1_761_120_000_000, DurationMs: 0, Audio: false,
		Segments: []string{"录音失败了但片段残留"},
	},
	{
		ID: "s_05", Title: "2026-11-12 08:00 · 微信", SourceID: "app:wechat.exe", SourceName: "微信",
		Status: "completed", ModelSource: "local", Transcribe: "done",
		StartedAt: 1_761_200_000_000, DurationMs: 240_000, Audio: true,
		Segments: []string{"我们把会议纪要发到群里", "会议记录请查收", "hello wechat"},
	},
	{
		ID: "s_06", Title: "2026-11-12 20:00 · KOOK", SourceID: "app:kook.exe", SourceName: "KOOK",
		Status: "completed", ModelSource: "local", Transcribe: "queued",
		StartedAt: 1_761_240_000_000, DurationMs: 600_000, Audio: true,
		Segments: nil,
	},
	{
		ID: "s_07", Title: "2026-11-13 11:00 · Discord", SourceID: "app:discord.exe", SourceName: "Discord",
		Status: "degraded", ModelSource: "online", Transcribe: "done",
		StartedAt: 1_761_300_000_000, DurationMs: 1_000, Audio: true,
		Segments: []string{"degraded run but transcribed"},
	},
	{
		ID: "s_08", Title: "2026-11-13 14:00 · 系统声音", SourceID: "device:loopback", SourceName: "系统声音",
		Status: "completed", ModelSource: "local", Transcribe: "running",
		StartedAt: 1_761_320_000_000, DurationMs: 900_000, Audio: true,
		Segments: []string{"正在转写这一段 hello streaming"},
	},
	{
		ID: "s_09", Title: "纯英文标题 hello world", SourceID: "app:kook.exe", SourceName: "KOOK",
		Status: "completed", ModelSource: "local", Transcribe: "none",
		StartedAt: 1_761_400_000_000, DurationMs: 45_000, Audio: true,
		Segments: nil,
	},
	{
		ID: "s_10", Title: "2026-11-14 09:00 · 100% 纯人声", SourceID: "device:mic", SourceName: "麦克风",
		Status: "completed", ModelSource: "local", Transcribe: "done",
		StartedAt: 1_761_500_000_000, DurationMs: 30_000, Audio: true,
		Segments: []string{"这段是纯人声没有背景音乐"},
	},
	{
		ID: "s_11", Title: "2026-11-14 10:00 · KOOK", SourceID: "app:kook.exe", SourceName: "KOOK",
		Status: "interrupted", ModelSource: "local", Transcribe: "none",
		StartedAt: 1_761_510_000_000, DurationMs: 5_000, Audio: true,
		Segments: nil,
	},
	{
		ID: "s_12", Title: "Hello 大写与中文混排", SourceID: "app:wechat.exe", SourceName: "微信",
		Status: "completed", ModelSource: "online", Transcribe: "done",
		StartedAt: 1_761_600_000_000, DurationMs: 120_000, Audio: true,
		Segments: []string{"HELLO 大写英文片段"},
	},
}

// newFixture 建临时库与 dataRoot，并写入固定语料。
func newFixture(t *testing.T, mode SearchMode) *fixture {
	t.Helper()
	dir := t.TempDir()
	dataRoot := filepath.Join(dir, "data")
	if err := os.MkdirAll(dataRoot, 0o755); err != nil {
		t.Fatalf("mkdir dataRoot: %v", err)
	}
	db, err := storage.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	f := &fixture{t: t, db: db, holder: storage.NewHolder(db), dataRoot: dataRoot}
	repo, err := New(Options{
		Holder:     f.holder,
		DataRoot:   func() (string, error) { return dataRoot, nil },
		SearchMode: mode,
		Writer:     &memWriter{},
	})
	if err != nil {
		t.Fatalf("new repo: %v", err)
	}
	f.repo = repo
	f.seed()
	return f
}

// seed 写入语料（含真实音频文件，供删除/audioExists 断言）。
func (f *fixture) seed() {
	f.t.Helper()
	for _, cs := range corpus {
		f.insertSession(cs)
	}
}

func (f *fixture) insertSession(cs corpusSession) {
	f.t.Helper()
	audioPath := ""
	if cs.Audio {
		rel := filepath.ToSlash(filepath.Join("sessions", cs.ID, "audio.wav"))
		abs := filepath.Join(f.dataRoot, "sessions", cs.ID, "audio.wav")
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			f.t.Fatalf("mkdir audio dir: %v", err)
		}
		// 内容不重要（不进日志、不进导出），但不能是 0 字节，size 断言要用。
		if err := os.WriteFile(abs, []byte("RIFFxxxxWAVE"), 0o644); err != nil {
			f.t.Fatalf("write audio: %v", err)
		}
		audioPath = rel
	}
	row := storage.Session{
		ID:                  cs.ID,
		Title:               cs.Title,
		SourceID:            cs.SourceID,
		SourceName:          cs.SourceName,
		SourceKind:          "application",
		ModelSource:         cs.ModelSource,
		EngineID:            "bundle@sensevoice-small@1.2.0",
		Status:              cs.Status,
		StartedAt:           cs.StartedAt,
		EndedAt:             cs.StartedAt + cs.DurationMs,
		DurationMs:          cs.DurationMs,
		AudioPath:           audioPath,
		AudioFormat:         "wav",
		SizeBytes:           int64(len("RIFFxxxxWAVE")),
		SegmentCount:        len(cs.Segments),
		TranscriptionStatus: cs.Transcribe,
	}
	if err := f.db.GORM.Create(&row).Error; err != nil {
		f.t.Fatalf("insert session %s: %v", cs.ID, err)
	}
	for i, text := range cs.Segments {
		seg := storage.Segment{
			SessionID: cs.ID,
			Seq:       i,
			StartMs:   int64(i) * 2000,
			EndMs:     int64(i)*2000 + 1800,
			Text:      text,
			IsFinal:   true,
			EngineID:  "bundle@sensevoice-small@1.2.0",
		}
		if err := f.db.GORM.Create(&seg).Error; err != nil {
			f.t.Fatalf("insert segment %s/%d: %v", cs.ID, i, err)
		}
	}
}

// ids 提取结果里的会话 ID，便于断言。
func idsOf(items []contract.Session) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.ID
	}
	return out
}

// contains 判断 ID 是否在列表里。
func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// memWriter 是导出端口的测试替身：只记录内容，不落盘。
type memWriter struct {
	text string
	csv  string
	xlsx Table
}

func (w *memWriter) WriteCSV(_ string, content string) error {
	w.csv = content
	return nil
}

func (w *memWriter) WriteXLSX(_ string, table Table) error {
	w.xlsx = table
	return nil
}

func (w *memWriter) WriteText(_ string, content string) error {
	w.text = content
	return nil
}

var _ = fmt.Sprintf
