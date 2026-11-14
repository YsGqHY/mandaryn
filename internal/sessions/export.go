package sessions

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/storage"
)

// 导出格式常量（API-3.5）。
const (
	FormatTXT  = "txt"
	FormatSRT  = "srt"
	FormatCSV  = "csv"
	FormatXLSX = "xlsx"
)

// Table 是 xlsx 导出的表格载荷。
//
// 刻意**不复用** exportsvc.Table：那会把 L2 领域包反向依赖到 L3 服务包上。
// 由 L3 做同名字段的一次性转换（字段完全一致，转换无需逻辑）。
type Table struct {
	SheetName string
	Headers   []string
	Rows      [][]any
}

// ExportWriter 是导出落盘的端口（实现由 L3 注入，复用 exportsvc）。
type ExportWriter interface {
	// WriteCSV 写 UTF-8 BOM 的 CSV。
	WriteCSV(path, content string) error
	// WriteXLSX 写 xlsx。
	WriteXLSX(path string, table Table) error
	// WriteText 写纯文本（txt / srt），原子写、UTF-8 无 BOM。
	WriteText(path, content string) error
}

// ExportRequest 是领域层的导出请求（L3 从 contract.ExportRequest 转换而来）。
type ExportRequest struct {
	SessionIDs        []string
	Format            string
	IncludeTimestamps bool
	IncludeSpeakers   bool
	TargetPath        string
}

// exportedSegment 是导出用的片段行（已 JOIN 出角色名）。
type exportedSegment struct {
	SessionID string
	Seq       int
	StartMs   int64
	EndMs     int64
	Text      string
	SpeakerID string
	RoleName  string
}

// exportSession 汇总一个会话的导出素材。
type exportSession struct {
	Session  contract.Session
	Segments []exportedSegment
}

// Export 生成导出文件并返回落盘路径（API-3.5）。
//
// 契约约定「targetPath 为空则弹原生另存为」，但弹窗属于前端职责
// （NativeDialogs）：后端拿不到窗口上下文，也不该在导出这条纯数据路径上
// 引入 UI 依赖。因此空路径直接判为参数错误，由前端先选好目标路径。
func (r *Repo) Export(ctx context.Context, req ExportRequest) (string, error) {
	format := strings.ToLower(strings.TrimSpace(req.Format))
	if err := validateFormat(format); err != nil {
		return "", err
	}
	if r.writer == nil {
		return "", apierr.Newf(apierr.CodeInvalidArgument, "export writer is not configured")
	}
	target := strings.TrimSpace(req.TargetPath)
	if target == "" {
		// 前端必须先用 NativeDialogs 选路径（禁止后端弹窗）。
		return "", apierr.New(apierr.CodeInvalidArgument).
			WithDetail("targetPath 为空：请先用原生「另存为」选定目标路径")
	}
	if !filepath.IsAbs(target) {
		return "", apierr.New(apierr.CodeInvalidArgument).
			WithDetail("targetPath 必须是绝对路径")
	}
	if !strings.EqualFold(filepath.Ext(target), "."+format) {
		return "", apierr.New(apierr.CodeInvalidArgument).
			WithDetail("targetPath 扩展名需为 .%s", format)
	}
	ids := normalizeIDs(req.SessionIDs)
	if len(ids) == 0 {
		return "", apierr.New(apierr.CodeInvalidArgument).WithDetail("sessionIds 不能为空")
	}

	data, err := r.loadExportData(ctx, ids)
	if err != nil {
		return "", err
	}

	switch format {
	case FormatTXT:
		if err := r.writer.WriteText(target, buildTXT(data, req.IncludeTimestamps, req.IncludeSpeakers)); err != nil {
			return "", apierr.Wrap(err, apierr.CodeDiskFull)
		}
	case FormatSRT:
		if err := r.writer.WriteText(target, buildSRT(data, req.IncludeSpeakers)); err != nil {
			return "", apierr.Wrap(err, apierr.CodeDiskFull)
		}
	case FormatCSV:
		if err := r.writer.WriteCSV(target, buildCSV(data, req.IncludeTimestamps, req.IncludeSpeakers)); err != nil {
			return "", apierr.Wrap(err, apierr.CodeDiskFull)
		}
	case FormatXLSX:
		if err := r.writer.WriteXLSX(target, buildTable(data, req.IncludeTimestamps, req.IncludeSpeakers)); err != nil {
			return "", apierr.Wrap(err, apierr.CodeDiskFull)
		}
	}
	return target, nil
}

func validateFormat(format string) error {
	switch format {
	case FormatTXT, FormatSRT, FormatCSV, FormatXLSX:
		return nil
	}
	return apierr.Newf(apierr.CodeInvalidArgument, "unsupported export format").
		WithDetail("format=%q（支持 txt|srt|csv|xlsx）", format)
}

// loadExportData 读取会话与片段（按请求里的会话顺序输出）。
func (r *Repo) loadExportData(ctx context.Context, ids []string) ([]exportSession, error) {
	db := r.db().WithContext(ctx)

	var rows []storage.Session
	if err := db.Where("id IN ?", ids).Find(&rows).Error; err != nil {
		return nil, apierr.Wrap(err, apierr.CodeDBWrite)
	}
	if len(rows) == 0 {
		return nil, apierr.New(apierr.CodeNotFound).WithDetail("sessionIds 均不存在")
	}

	byID := make(map[string]exportSession, len(rows))
	for _, row := range rows {
		byID[row.ID] = exportSession{Session: toContract(row)}
	}

	// 片段 JOIN 角色名：**不在 segments 里冗余存名字**（FR-06-3），
	// 靠 session_speakers → roles 现取，保证改名后导出立即反映新名字。
	var segs []exportedSegment
	err := db.Table("segments AS seg").
		Select(`seg.session_id, seg.seq, seg.start_ms, seg.end_ms, seg.text,
			seg.speaker_id, COALESCE(r.name, '') AS role_name`).
		Joins("LEFT JOIN session_speakers ss ON ss.session_id = seg.session_id AND ss.speaker_id = seg.speaker_id").
		Joins("LEFT JOIN roles r ON r.id = ss.role_id").
		Where("seg.session_id IN ?", ids).
		Order("seg.session_id, seg.seq").
		Scan(&segs).Error
	if err != nil {
		return nil, apierr.Wrap(err, apierr.CodeDBWrite)
	}

	for _, seg := range segs {
		item, ok := byID[seg.SessionID]
		if !ok {
			continue
		}
		item.Segments = append(item.Segments, seg)
		byID[seg.SessionID] = item
	}

	// 顺序按调用方给的 IDs（列表页勾选顺序），而不是 started_at：
	// 用户按自己的勾选顺序拿到文件，符合预期。
	out := make([]exportSession, 0, len(ids))
	for _, id := range ids {
		if item, ok := byID[id]; ok {
			out = append(out, item)
		}
	}
	return out, nil
}

// buildTXT 生成纯文本：会话头 + 每句一行。
func buildTXT(data []exportSession, withTime, withSpeaker bool) string {
	var b strings.Builder
	for i, item := range data {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(exportHeader(item.Session))
		if len(item.Segments) == 0 {
			b.WriteString("（未转写）\n")
			continue
		}
		for _, seg := range item.Segments {
			b.WriteString(renderLine(seg, withTime, withSpeaker, " / "))
			b.WriteString("\n")
		}
	}
	return b.String()
}

// exportHeader 生成会话元信息头（模型来源保留是 FR-08-3 的要求）。
func exportHeader(s contract.Session) string {
	var parts []string
	if s.SourceName != "" {
		parts = append(parts, s.SourceName)
	}
	if s.StartedAt != "" {
		parts = append(parts, s.StartedAt)
	}
	if s.DurationMs > 0 {
		parts = append(parts, formatClock(s.DurationMs))
	}
	if s.ModelSource != "" {
		parts = append(parts, s.ModelSource)
	}
	out := s.Title
	if len(parts) > 0 {
		out += "\n" + strings.Join(parts, " · ")
	}
	return out + "\n"
}

// renderLine 渲染一行文本；sep 是时间与说话人之间的分隔符。
func renderLine(seg exportedSegment, withTime, withSpeaker bool, sep string) string {
	var parts []string
	if withTime {
		parts = append(parts, formatClock(seg.StartMs))
	}
	if withSpeaker {
		parts = append(parts, speakerLabel(seg))
	}
	if len(parts) == 0 {
		return seg.Text
	}
	return strings.Join(parts, sep) + ": " + seg.Text
}

// speakerLabel 返回说话人展示名：角色名 → 说话人 ID → 「未识别」。
func speakerLabel(seg exportedSegment) string {
	if name := strings.TrimSpace(seg.RoleName); name != "" {
		return name
	}
	if id := strings.TrimSpace(seg.SpeakerID); id != "" {
		return id
	}
	return "未识别"
}

// buildSRT 生成标准 SubRip：`hh:mm:ss,mmm --> hh:mm:ss,mmm`（逗号是规范要求）。
//
// 多会话导出时续接同一序号区间，不重置为 1：播放器按序号排序，
// 重置会让整段文件在多会话处出现并列序号而错乱。
func buildSRT(data []exportSession, withSpeaker bool) string {
	var b strings.Builder
	idx := 1
	for _, item := range data {
		for _, seg := range item.Segments {
			b.WriteString(strconv.Itoa(idx))
			b.WriteString("\n")
			b.WriteString(formatSRTTime(seg.StartMs))
			b.WriteString(" --> ")
			b.WriteString(formatSRTTime(seg.EndMs))
			b.WriteString("\n")
			b.WriteString(renderLine(seg, false, withSpeaker, " "))
			b.WriteString("\n\n")
			idx++
		}
	}
	return b.String()
}

// formatSRTTime 输出 hh:mm:ss,mmm。小时不截断到两位（超长录音仍正确）。
func formatSRTTime(ms int64) string {
	if ms < 0 {
		ms = 0
	}
	h := ms / 3_600_000
	ms -= h * 3_600_000
	m := ms / 60_000
	ms -= m * 60_000
	s := ms / 1_000
	ms -= s * 1_000
	return fmt.Sprintf("%02d:%02d:%02d,%03d", h, m, s, ms)
}

// formatClock 输出 hh:mm:ss（用于 txt 与表格里的可读时间戳）。
func formatClock(ms int64) string {
	if ms < 0 {
		ms = 0
	}
	h := ms / 3_600_000
	ms -= h * 3_600_000
	m := ms / 60_000
	ms -= m * 60_000
	s := ms / 1_000
	return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
}

// csvHeaders / csvRow 统一表格列定义，CSV 与 XLSX 共用，避免两种格式列不一致。
func exportHeaders(withTime, withSpeaker bool) []string {
	headers := []string{"会话", "开始时间", "来源应用", "模型来源"}
	if withTime {
		headers = append(headers, "时间戳", "起始毫秒")
	}
	if withSpeaker {
		headers = append(headers, "说话人")
	}
	return append(headers, "文本")
}

func exportRow(title string, s contract.Session, seg exportedSegment, withTime, withSpeaker bool) []any {
	row := []any{title, s.StartedAt, s.SourceName, s.ModelSource}
	if withTime {
		row = append(row, formatClock(seg.StartMs), seg.StartMs)
	}
	if withSpeaker {
		row = append(row, speakerLabel(seg))
	}
	return append(row, seg.Text)
}

// buildCSV 生成 CSV 正文（BOM 由 writer 负责）。
func buildCSV(data []exportSession, withTime, withSpeaker bool) string {
	var b strings.Builder
	b.WriteString(csvLine(headersAsAny(exportHeaders(withTime, withSpeaker))))
	for _, item := range data {
		for _, seg := range item.Segments {
			b.WriteString(csvLine(exportRow(item.Session.Title, item.Session, seg, withTime, withSpeaker)))
		}
	}
	return b.String()
}

// headersAsAny 把表头转成统一的单元格类型（与 exportRow 的输出同构）。
func headersAsAny(headers []string) []any {
	out := make([]any, len(headers))
	for i, h := range headers {
		out[i] = h
	}
	return out
}

// csvLine 按 RFC 4180 转义：含逗号 / 引号 / 换行的字段加引号并翻倍引号。
func csvLine(cells []any) string {
	var b strings.Builder
	for i, cell := range cells {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(csvCell(cell))
	}
	b.WriteString("\n")
	return b.String()
}

func csvCell(v any) string {
	var s string
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		s = x
	case int64:
		return strconv.FormatInt(x, 10)
	case int:
		return strconv.Itoa(x)
	default:
		s = fmt.Sprint(x)
	}
	if strings.ContainsAny(s, ",\"\r\n") {
		return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
	}
	return s
}

// buildTable 生成 xlsx 载荷。
func buildTable(data []exportSession, withTime, withSpeaker bool) Table {
	rows := make([][]any, 0, len(data))
	for _, item := range data {
		for _, seg := range item.Segments {
			rows = append(rows, exportRow(item.Session.Title, item.Session, seg, withTime, withSpeaker))
		}
	}
	return Table{
		SheetName: "会话记录",
		Headers:   exportHeaders(withTime, withSpeaker),
		Rows:      rows,
	}
}
