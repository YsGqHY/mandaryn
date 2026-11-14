package sessionsvc

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/xuri/excelize/v2"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/paths"
	"vip.oberon.mandaryn/internal/storage"
)

// srtTimePattern 是 SubRip 时间码（逗号毫秒）。
var srtTimePattern = regexp.MustCompile(`\d{2,}:\d{2}:\d{2},\d{3} --> \d{2,}:\d{2}:\d{2},\d{3}`)

// env 是一套测试环境：临时库 + 临时 dataRoot + 真实 Service（走 exportsvc 落盘）。
type env struct {
	t        *testing.T
	svc      *Service
	db       *storage.DB
	dataRoot string
}

// newEnv 构造环境并写入 2 条会话（含中文片段与本机音频文件）。
func newEnv(t *testing.T) *env {
	t.Helper()
	root := t.TempDir()
	dataRoot := filepath.Join(root, "data")
	if err := os.MkdirAll(dataRoot, 0o755); err != nil {
		t.Fatalf("mkdir dataRoot: %v", err)
	}
	db, err := storage.Open(filepath.Join(root, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// 用真实 resolver，但把「默认层」指到临时 dataRoot：
	// 这样既不依赖测试机的 AppData/XDG 环境，又能验证真值链路
	// （resolver.Effective → 音频绝对路径 → stat）。
	resolver := paths.New(paths.Options{
		DefaultDataRoot: func() (string, error) { return dataRoot, nil },
	})
	svc, err := New(storage.NewHolder(db), resolver)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	e := &env{t: t, svc: svc, db: db, dataRoot: dataRoot}
	e.seed()
	return e
}

// seed 写入两条会话：s_a 已转写（两句中文）、s_b 未转写。
func (e *env) seed() {
	e.t.Helper()
	type seg struct {
		sessionID string
		seq       int
		start     int64
		end       int64
		text      string
	}
	sessions := []storage.Session{
		{
			ID: "s_a", Title: "2026-11-10 10:00 · KOOK", SourceID: "app:kook.exe", SourceName: "KOOK",
			SourceKind: "application", ModelSource: "local", EngineID: "sensevoice-small@1.2.0",
			Status: "completed", StartedAt: 1_761_000_000_000, EndedAt: 1_761_000_090_000,
			DurationMs: 90_000, AudioPath: "sessions/s_a/audio.wav", AudioFormat: "wav",
			SizeBytes: 2048, SegmentCount: 2, TranscriptionStatus: "done",
		},
		{
			ID: "s_b", Title: "2026-11-11 09:00 · 系统声音", SourceID: "device:loopback", SourceName: "系统声音",
			SourceKind: "device", ModelSource: "online", EngineID: "online:whisper-1",
			Status: "interrupted", StartedAt: 1_761_100_000_000, EndedAt: 1_761_100_012_000,
			DurationMs: 12_000, SegmentCount: 0, TranscriptionStatus: "none",
		},
	}
	for _, s := range sessions {
		if err := e.db.GORM.Create(&s).Error; err != nil {
			e.t.Fatalf("insert session %s: %v", s.ID, err)
		}
	}

	// 真实音频文件（s_a 有，s_b 没有）
	audioAbs := filepath.Join(e.dataRoot, "sessions", "s_a", "audio.wav")
	if err := os.MkdirAll(filepath.Dir(audioAbs), 0o755); err != nil {
		e.t.Fatalf("mkdir audio: %v", err)
	}
	if err := os.WriteFile(audioAbs, []byte("RIFFxxxxWAVEfmt "), 0o644); err != nil {
		e.t.Fatalf("write audio: %v", err)
	}

	segs := []seg{
		{"s_a", 0, 0, 1800, "今天的会议先对齐一下排期"},
		{"s_a", 1, 2000, 3800, "hello team we start now"},
	}
	for _, s := range segs {
		row := storage.Segment{
			SessionID: s.sessionID, Seq: s.seq, StartMs: s.start, EndMs: s.end,
			Text: s.text, IsFinal: true, EngineID: "sensevoice-small@1.2.0",
		}
		if err := e.db.GORM.Create(&row).Error; err != nil {
			e.t.Fatalf("insert segment: %v", err)
		}
	}
}

// TestListAndGet 断言薄壳的 List/Get/GetDetail 串起来是通的。
func TestListAndGet(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	res, err := e.svc.List(ctx, contract.ListQuery{Limit: 10})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if res.Total != 2 {
		t.Fatalf("total = %d, want 2", res.Total)
	}
	if len(res.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(res.Items))
	}
	// 排序 started_at DESC
	if res.Items[0].ID != "s_b" {
		t.Fatalf("first = %s, want s_b", res.Items[0].ID)
	}

	got, err := e.svc.Get(ctx, "s_a")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Title != "2026-11-10 10:00 · KOOK" || got.SegmentCount != 2 {
		t.Fatalf("get 结果不符: %+v", got)
	}

	detail, err := e.svc.GetDetail(ctx, "s_a")
	if err != nil {
		t.Fatalf("get detail: %v", err)
	}
	if !detail.AudioExists {
		t.Fatal("s_a 音频存在，audioExists 应为 true")
	}
	detailB, err := e.svc.GetDetail(ctx, "s_b")
	if err != nil {
		t.Fatalf("get detail b: %v", err)
	}
	if detailB.AudioExists {
		t.Fatal("s_b 无音频，audioExists 应为 false")
	}
}

// TestListInvalidTime 断言非法时间参数被映射成 ERR_INVALID_ARGUMENT。
func TestListInvalidTime(t *testing.T) {
	e := newEnv(t)
	_, err := e.svc.List(context.Background(), contract.ListQuery{From: "昨天"})
	if !apierr.HasCode(err, apierr.CodeInvalidArgument) {
		t.Fatalf("err = %v, want ERR_INVALID_ARGUMENT", err)
	}
}

// TestRenameAndDelete 断言改名与批量删除（含音频）在薄壳层的行为。
func TestRenameAndDelete(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	if err := e.svc.Rename(ctx, "s_a", "新标题"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	got, err := e.svc.Get(ctx, "s_a")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Title != "新标题" {
		t.Fatalf("title = %q", got.Title)
	}

	res, err := e.svc.DeleteDetailed(ctx, []string{"s_a", "s_missing"}, true)
	if err != nil {
		t.Fatalf("delete detailed: %v", err)
	}
	if res.Deleted != 1 || res.AudioDeleted != 1 {
		t.Fatalf("delete 结果 = %+v, want 1/1", res)
	}
	if len(res.Missing) != 1 || res.Missing[0] != "s_missing" {
		t.Fatalf("missing = %v", res.Missing)
	}
	if len(res.AudioFailed) != 0 {
		t.Fatalf("audioFailed = %v", res.AudioFailed)
	}
	// Missing/AudioFailed 必须是空切片而不是 nil（JSON 里不能是 null）
	if res.AudioFailed == nil {
		t.Fatal("audioFailed 应为空切片而不是 nil")
	}
	// 音频目录已删
	if _, err := os.Stat(filepath.Join(e.dataRoot, "sessions", "s_a")); !os.IsNotExist(err) {
		t.Fatalf("音频目录应被删除，stat err = %v", err)
	}

	// Delete 简易形式：全部不存在 → ERR_NOT_FOUND
	if err := e.svc.Delete(ctx, []string{"nope"}, true); !apierr.HasCode(err, apierr.CodeNotFound) {
		t.Fatalf("err = %v, want ERR_NOT_FOUND", err)
	}
}

// TestExportTXT 断言 txt 落盘、非空、含正文。
func TestExportTXT(t *testing.T) {
	e := newEnv(t)
	target := filepath.Join(t.TempDir(), "会话导出.txt")

	got, err := e.svc.Export(context.Background(), contract.ExportRequest{
		SessionIDs: []string{"s_a"}, Format: "txt",
		IncludeTimestamps: true, IncludeSpeakers: true,
		TargetPath: target,
	})
	if err != nil {
		t.Fatalf("export txt: %v", err)
	}
	if got != target {
		t.Fatalf("返回路径 = %q, want %q", got, target)
	}
	data := readFile(t, target)
	if len(data) == 0 {
		t.Fatal("txt 文件为空")
	}
	content := string(data)
	for _, want := range []string{"今天的会议先对齐一下排期", "hello team we start now", "KOOK"} {
		if !strings.Contains(content, want) {
			t.Fatalf("txt 缺少 %q:\n%s", want, content)
		}
	}
	// 中文以 UTF-8 落盘（不是 GBK）：中文 3 字节，且能被 utf8 校验
	if !isValidUTF8(data) {
		t.Fatal("txt 不是合法 UTF-8（中文乱码）")
	}
	if strings.Contains(content, "\uFFFD") {
		t.Fatal("txt 出现替换字符，编码链路有问题")
	}
}

// TestExportSRT 断言 srt 时间码格式与条目结构。
func TestExportSRT(t *testing.T) {
	e := newEnv(t)
	target := filepath.Join(t.TempDir(), "out.srt")

	if _, err := e.svc.Export(context.Background(), contract.ExportRequest{
		SessionIDs: []string{"s_a"}, Format: "srt", IncludeSpeakers: true,
		TargetPath: target,
	}); err != nil {
		t.Fatalf("export srt: %v", err)
	}
	content := string(readFile(t, target))
	if len(content) == 0 {
		t.Fatal("srt 文件为空")
	}

	// 每条字幕 4 行：序号 / 时间码 / 文本 / 空行
	blocks := strings.Split(strings.TrimSpace(content), "\n\n")
	if len(blocks) != 2 {
		t.Fatalf("srt 条目数 = %d, want 2:\n%s", len(blocks), content)
	}
	lines := strings.Split(blocks[0], "\n")
	if len(lines) != 3 {
		t.Fatalf("首条应为 序号/时间码/文本 3 行，实际 %d 行: %q", len(lines), lines)
	}
	if lines[0] != "1" {
		t.Fatalf("首条序号 = %q, want 1", lines[0])
	}
	if !srtTimePattern.MatchString(lines[1]) {
		t.Fatalf("时间码格式错误: %q（期望 hh:mm:ss,mmm --> hh:mm:ss,mmm）", lines[1])
	}
	if lines[1] != "00:00:00,000 --> 00:00:01,800" {
		t.Fatalf("首条时间码 = %q", lines[1])
	}
	if !strings.HasPrefix(lines[2], "未识别:") {
		t.Fatalf("首条文本应带说话人前缀，实际 %q", lines[2])
	}
	// 第二条序号递进
	if !strings.HasPrefix(blocks[1], "2\n") {
		t.Fatalf("第二条序号未递进:\n%s", blocks[1])
	}
}

// TestExportCSV 断言 csv 落盘、带 UTF-8 BOM、列与转义正确。
func TestExportCSV(t *testing.T) {
	e := newEnv(t)
	target := filepath.Join(t.TempDir(), "out.csv")

	if _, err := e.svc.Export(context.Background(), contract.ExportRequest{
		SessionIDs: []string{"s_a"}, Format: "csv",
		IncludeTimestamps: true, IncludeSpeakers: true,
		TargetPath: target,
	}); err != nil {
		t.Fatalf("export csv: %v", err)
	}
	data := readFile(t, target)
	if len(data) == 0 {
		t.Fatal("csv 文件为空")
	}
	// Excel 双击打开中文不乱码的前提：UTF-8 BOM
	if !(len(data) >= 3 && data[0] == 0xEF && data[1] == 0xBB && data[2] == 0xBF) {
		t.Fatalf("csv 缺少 UTF-8 BOM，前 3 字节 = %v", data[:3])
	}
	content := string(data[3:])
	lines := strings.Split(strings.TrimSpace(content), "\n")
	if len(lines) != 3 {
		t.Fatalf("csv 行数 = %d, want 3（1 表头 + 2 数据）:\n%s", len(lines), content)
	}
	if !strings.Contains(lines[0], "会话") || !strings.Contains(lines[0], "文本") {
		t.Fatalf("csv 表头不符: %q", lines[0])
	}
	if !strings.Contains(content, "今天的会议先对齐一下排期") {
		t.Fatalf("csv 缺少正文:\n%s", content)
	}
}

// TestExportXLSX 断言 xlsx 能被 excelize 读回、单元格内容与行列数正确。
func TestExportXLSX(t *testing.T) {
	e := newEnv(t)
	target := filepath.Join(t.TempDir(), "out.xlsx")

	if _, err := e.svc.Export(context.Background(), contract.ExportRequest{
		SessionIDs: []string{"s_a", "s_b"}, Format: "xlsx",
		IncludeTimestamps: true, IncludeSpeakers: true,
		TargetPath: target,
	}); err != nil {
		t.Fatalf("export xlsx: %v", err)
	}
	st, err := os.Stat(target)
	if err != nil {
		t.Fatalf("xlsx 未生成: %v", err)
	}
	if st.Size() == 0 {
		t.Fatal("xlsx 文件为空")
	}

	f, err := excelize.OpenFile(target)
	if err != nil {
		t.Fatalf("excelize 打开失败: %v", err)
	}
	defer func() { _ = f.Close() }()

	sheets := f.GetSheetList()
	if len(sheets) != 1 || sheets[0] != "会话记录" {
		t.Fatalf("sheet 列表 = %v, want [会话记录]", sheets)
	}
	rows, err := f.GetRows(sheets[0])
	if err != nil {
		t.Fatalf("读 sheet: %v", err)
	}
	// 1 表头 + 2 片段（s_b 未转写没有片段）
	if len(rows) != 3 {
		t.Fatalf("行数 = %d, want 3:\n%v", len(rows), rows)
	}
	// 表头：会话 / 开始时间 / 来源应用 / 模型来源 / 时间戳 / 起始毫秒 / 说话人 / 文本
	if len(rows[0]) != 8 {
		t.Fatalf("表头列数 = %d, want 8: %v", len(rows[0]), rows[0])
	}
	// 第一列是会话标题，最后一列是正文
	if rows[1][0] != "2026-11-10 10:00 · KOOK" {
		t.Fatalf("首行会话列 = %q", rows[1][0])
	}
	if rows[1][len(rows[1])-1] != "今天的会议先对齐一下排期" {
		t.Fatalf("首行文本列 = %q", rows[1][len(rows[1])-1])
	}
	// 数字列保持数值类型（不是字符串）：起始毫秒 0 与 2000
	cellVal, err := f.GetCellValue(sheets[0], "F2")
	if err != nil {
		t.Fatalf("读单元格: %v", err)
	}
	if cellVal != "0" {
		t.Fatalf("F2（起始毫秒）= %q, want 0", cellVal)
	}
}

// TestExportValidation 断言薄壳的导出参数校验（不弹窗，空路径即报错）。
func TestExportValidation(t *testing.T) {
	e := newEnv(t)
	dir := t.TempDir()

	cases := []struct {
		name string
		req  contract.ExportRequest
		code string
	}{
		{
			name: "空 targetPath",
			req:  contract.ExportRequest{SessionIDs: []string{"s_a"}, Format: "txt"},
			code: apierr.CodeInvalidArgument,
		},
		{
			name: "格式非法",
			req:  contract.ExportRequest{SessionIDs: []string{"s_a"}, Format: "docx", TargetPath: filepath.Join(dir, "a.docx")},
			code: apierr.CodeInvalidArgument,
		},
		{
			name: "扩展名与格式不符",
			req:  contract.ExportRequest{SessionIDs: []string{"s_a"}, Format: "xlsx", TargetPath: filepath.Join(dir, "a.csv")},
			code: apierr.CodeInvalidArgument,
		},
		{
			name: "会话不存在",
			req:  contract.ExportRequest{SessionIDs: []string{"nope"}, Format: "txt", TargetPath: filepath.Join(dir, "b.txt")},
			code: apierr.CodeNotFound,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := e.svc.Export(context.Background(), tc.req); !apierr.HasCode(err, tc.code) {
				t.Fatalf("err = %v, want %s", err, tc.code)
			}
		})
	}

	// 父目录不存在：csv/xlsx 走 exportsvc 的路径校验（要求父目录已存在，由
	// 原生「另存为」保证），因此报错；txt/srt 走 filex.WriteAtomic 会自动
	// 建目录（写配置类文件的既有语义），这是两条路径的已知差异，见 S5-05。
	missingCSV := filepath.Join(dir, "no-such-dir", "c.csv")
	if _, err := e.svc.Export(context.Background(), contract.ExportRequest{
		SessionIDs: []string{"s_a"}, Format: "csv", TargetPath: missingCSV,
	}); err == nil {
		t.Fatal("csv 导出到不存在的父目录时应报错")
	}
	missingTXT := filepath.Join(dir, "auto-created", "c.txt")
	if _, err := e.svc.Export(context.Background(), contract.ExportRequest{
		SessionIDs: []string{"s_a"}, Format: "txt", TargetPath: missingTXT,
	}); err != nil {
		t.Fatalf("txt 导出应自动创建父目录，实际: %v", err)
	}
	if _, err := os.Stat(missingTXT); err != nil {
		t.Fatalf("txt 未落盘: %v", err)
	}
}

// readFile 读文件内容。
func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读文件 %s: %v", path, err)
	}
	return data
}

// isValidUTF8 判断字节序列是否是合法 UTF-8。
func isValidUTF8(data []byte) bool {
	return strings.ToValidUTF8(string(data), "\uFFFD") == string(data)
}
