package privacy

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/storage"
	"vip.oberon.mandaryn/internal/utils/logx"

	"gorm.io/gorm"
)

// outbound_log 里的结果取值（DOC-04 TBL-12 的 result 列，size 32）。
const (
	// ResultOK 请求成功。
	ResultOK = "ok"
	// ResultPending 已登记但结果未知（进程崩溃 / 仍在进行中）。
	// 自检会把 pending 当作「无法证明未出站」的可疑记录。
	ResultPending = "pending"
	// ResultBlocked 被离线闸门拦下，**没有真的发出去**。
	ResultBlocked = "blocked"
	// ResultError 请求失败但没有更具体的错误码。
	ResultError = "error"
)

// 列宽约束（对齐 DOC-04 TBL-12 与 storage.OutboundLog 的 gorm tag）。
const (
	// maxKindCol 是 kind 列宽；kind/purpose 合并后截断到此长度。
	maxKindCol = 16
	// maxResultCol 是 result 列宽。
	maxResultCol = 32
	// maxHostCol 是 host 列宽。
	maxHostCol = 255
	// maxURLBytes 是可落日志 / 诊断的 URL 上限，超出截断（避免长 query 刷屏）。
	maxURLBytes = 2048
	// maxPathSegment 是单个路径段的安全长度，超出视为疑似密钥并脱敏。
	maxPathSegment = 64
)

// masked 是脱敏占位符，全局统一，便于逐列断言「不含原始值」。
const masked = "***"

// Record 是一条待落库的出站记录。
//
// schema 现状（DOC-04 TBL-12，本轮不允许改 internal/storage）：
// 只有 at/host/kind/audio/bytes/result 六列，因此
//   - Purpose 只进结构化日志，不落库（补齐独立列需在 storage 增列，见 docs/DEV-ISSUES-S9a.md）；
//   - bytesIn + bytesOut 合进 bytes 列（区分方向需要新列）；
//   - status / errorCode 折进 result 列；
//   - duration 只进结构化日志。
type Record struct {
	// At 记录时刻；零值用 time.Now()。
	At time.Time
	// Kind 业务类型（model-download / online-asr 等）。
	Kind string
	// Purpose 具体用途（catalog / bundle / probe / finalize 等）。
	Purpose string
	// Target 目标 URL（**原始值**，落库前一定脱敏）。
	Target string
	// Audio 该请求是否携带音频。
	Audio bool
	// BytesIn 上传字节数。
	BytesIn int64
	// BytesOut 下载字节数。
	BytesOut int64
	// Status HTTP 状态码；0 表示未拿到响应。
	Status int
	// ErrorCode 失败时的 ERR_* 码（不含自由文本，避免正文泄漏）。
	ErrorCode string
	// Duration 请求耗时（只进日志）。
	Duration time.Duration
	// Result 显式结果；空时按 ErrorCode / Status 推导。
	Result string
}

// result 推导最终写入 result 列的值（≤32 字节）。
func (r Record) result() string {
	v := strings.TrimSpace(r.Result)
	switch {
	case v != "":
	case r.ErrorCode != "":
		v = r.ErrorCode
	case r.Status >= 400:
		v = "http-" + itoa(r.Status)
	case r.Status > 0:
		v = ResultOK
	default:
		v = ResultError
	}
	return truncate(v, maxResultCol)
}

// KindColumn 推导写入 kind 列的值（业务类型，≤16 字节）。
//
// kind 列只放业务类型（model-download / online-asr），与 DOC-04 TBL-12 的取值口径一致；
// purpose 放不下（online-asr/finalize 已超 16 字节），只进结构化日志。
func (r Record) KindColumn() string {
	return kindColumn(r.Kind)
}

// ---------------------------------------------------------------------------
// 脱敏
// ---------------------------------------------------------------------------

// sensitiveParams 是「参数名出现即视为凭据」的集合（归一化后比较：小写、去 _ / -）。
var sensitiveParams = map[string]bool{
	"token": true, "accesstoken": true, "refreshtoken": true, "idtoken": true,
	"key": true, "apikey": true, "apikeys": true, "accesskey": true, "secretkey": true,
	"secret": true, "clientsecret": true, "password": true, "passwd": true, "pwd": true,
	"auth": true, "authorization": true, "credential": true, "credentials": true,
	"sig": true, "signature": true, "code": true, "session": true, "sessionkey": true,
	"sid": true, "sk": true, "ak": true, "cookie": true, "jwt": true,
}

// SanitizeURL 返回可安全落库 / 落日志的 URL 串：
//   - 去掉 userinfo 里的用户名与密码；
//   - query 中敏感参数（token / key / signature / code …）的值一律换成 ***；
//   - query 中「看起来像密钥」的值（≥16 位且字母数字混排等）也换成 ***；
//   - 路径中疑似密钥的长串（≥16 位字母数字混排、≥32 位十六进制）换成 ***；
//   - 丢弃 fragment（从不用于诊断，却是常见的凭据夹带位置）；
//   - 总长度超过 2KB 时截断。
//
// 解析失败时退化为「正则 + 截断」的保守处理，绝不返回原始串。
func SanitizeURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !utf8.ValidString(raw) {
		raw = strings.ToValidUTF8(raw, "?")
	}

	u, err := url.Parse(raw)
	if err != nil || u == nil {
		out := redactSecretsInText(raw)
		out = strings.ReplaceAll(out, "\n", " ")
		return truncate(out, maxURLBytes)
	}

	// userinfo：永远不保留
	if u.User != nil {
		u.User = url.User(masked)
	}

	u.Fragment = ""
	u.RawFragment = ""

	if u.RawQuery != "" {
		u.RawQuery = sanitizeQuery(u.RawQuery)
	}

	if u.Path != "" {
		u.Path = sanitizePath(u.Path)
		u.RawPath = ""
	}
	if u.Host != "" {
		u.Host = strings.ReplaceAll(u.Host, "\n", "")
	}

	return truncate(u.String(), maxURLBytes)
}

// sanitizeQuery 逐个参数脱敏，保留参数名（诊断价值）与安全的短值。
func sanitizeQuery(rawQuery string) string {
	pairs := strings.Split(rawQuery, "&")
	out := make([]string, 0, len(pairs))
	for _, pair := range pairs {
		if pair == "" {
			continue
		}
		name, value, _ := strings.Cut(pair, "=")
		if sensitiveParam(name) || looksSecret(value) {
			value = masked
		} else {
			value = redactSecretsInText(value)
			value = strings.ReplaceAll(value, "\n", "")
		}
		if value == "" {
			out = append(out, name)
			continue
		}
		out = append(out, name+"="+value)
	}
	return strings.Join(out, "&")
}

// sanitizePath 逐段处理路径：疑似密钥的长段整体脱敏，其余段截断。
func sanitizePath(p string) string {
	segs := strings.Split(p, "/")
	for i, seg := range segs {
		switch {
		case seg == "":
		case len(seg) > maxPathSegment || looksSecret(seg):
			segs[i] = masked
		default:
			segs[i] = redactSecretsInText(seg)
		}
	}
	return strings.Join(segs, "/")
}

// sensitiveParam 归一化参数名后查表。
func sensitiveParam(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	n = strings.NewReplacer("_", "", "-", "", ".", "", "[", "", "]", "").Replace(n)
	if n == "" {
		return false
	}
	return sensitiveParams[n]
}

// looksSecret 判断一个值 / 路径段是否像密钥：
// 长且字母数字混排（含大小写或数字），或典型密钥前缀，或长十六进制串。
func looksSecret(v string) bool {
	v = strings.TrimSpace(v)
	if len(v) < 12 {
		return false
	}
	lower := strings.ToLower(v)
	for _, p := range []string{"sk-", "sk_", "ghp_", "gho_", "ghs_", "xoxb-", "akia", "bearer ", "eyj"} {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}

	var letters, digits int
	foreign := 0
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
			letters++
		case r >= '0' && r <= '9':
			digits++
		case r == '-', r == '_', r == '.', r == '~', r == '+', r == '/', r == '=':
		default:
			foreign++
		}
	}
	if foreign > 0 {
		return false
	}
	if digits == 0 {
		// 纯字母长串（如路径里的 transcriptions）不算密钥。
		return false
	}
	return letters+digits >= 16
}

// HostOf 只返回 host[:port]，不含 userinfo / path / query / fragment。
// 这是 outbound_log.host 与日志里允许出现的目标信息。
func HostOf(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u == nil || u.Host == "" {
		return ""
	}
	// u.Host 本身不含 userinfo（userinfo 在 u.User 里），直接用它。
	return truncate(strings.Map(dropControl, u.Host), maxHostCol)
}

// HostLabel 返回「host + 截断后的路径前缀」的安全标签（≤255 字节）。
//
// 落 outbound_log.host 用它：保留「打到哪个接口」的可读性，同时保证
// query / userinfo / 疑似密钥一律不出现。
func HostLabel(raw string) string {
	host := HostOf(raw)
	if host == "" {
		return ""
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u == nil || u.Path == "" {
		return host
	}
	path := sanitizePath(u.Path)
	path = strings.TrimSuffix(path, "/")
	// 只保留前两段，够定位接口又不至于把具体资源 ID 写进去。
	segs := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(segs) > 2 {
		segs = segs[:2]
	}
	if len(segs) == 0 || (len(segs) == 1 && segs[0] == "") {
		return host
	}
	return truncate(strings.Map(dropControl, host+"/"+strings.Join(segs, "/")), maxHostCol)
}

// dropControl 去掉控制字符与换行，防止日志 / 数据库出现多行注入。
func dropControl(r rune) rune {
	if r == '\n' || r == '\r' || r == '\t' {
		return ' '
	}
	if unicode.IsControl(r) {
		return -1
	}
	return r
}

// kindColumn 把业务类型压进 kind 列（≤16 字节）。
func kindColumn(kind string) string {
	k := normalizeKind(kind)
	if k == "" {
		k = "other"
	}
	return truncate(k, maxKindCol)
}

// normalizeKind 归一化业务类型：小写、非 [a-z0-9] 折成 '-'、去掉首尾 '-'。
func normalizeKind(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	b.Grow(len(s))
	prevDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// truncate 按字节截断但保证不切坏 UTF-8。
func truncate(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// itoa 是无依赖的小整数格式化（避免为了一个状态码引入 strconv 语义扩散）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// ---------------------------------------------------------------------------
// 留痕
// ---------------------------------------------------------------------------

// Recorder 负责 outbound_log 的读写。留痕失败**不影响**业务（返回错误只记日志），
// 因为「最近出站点面板缺一条」不该让用户的下载 / 转写失败。
type Recorder struct {
	holder *storage.Holder
}

// NewRecorder 构造记录器；holder 为 nil 时只做内存判定、不落库（单测可用）。
func NewRecorder(holder *storage.Holder) *Recorder {
	return &Recorder{holder: holder}
}

func (r *Recorder) db() *gorm.DB {
	if r == nil || r.holder == nil {
		return nil
	}
	db := r.holder.Current()
	if db == nil || db.GORM == nil {
		return nil
	}
	return db.GORM
}

// Record 落一条出站记录，返回行 id（未落库为 0）。
func (r *Recorder) Record(rec Record) int64 {
	db := r.db()
	if db == nil {
		return 0
	}
	if rec.At.IsZero() {
		rec.At = time.Now()
	}
	row := storage.OutboundLog{
		At:     rec.At.UnixMilli(),
		Host:   HostLabel(rec.Target),
		Kind:   rec.KindColumn(),
		Audio:  rec.Audio,
		Bytes:  rec.BytesIn + rec.BytesOut,
		Result: rec.result(),
	}
	if err := db.Create(&row).Error; err != nil {
		logx.For("privacy").Warn("outbound log write failed", "kind", row.Kind, "host", row.Host, "error", err.Error())
		return 0
	}
	return row.ID
}

// Finish 把预登记的 pending 行补成最终结果。
//
// 两段式（先登记 pending、执行完再 update）保证：
//   - 即使请求失败、甚至进程崩溃，也已经在库里留下「有过一次出站意图」的痕；
//   - 崩溃遗留的 pending 行会被 audit 当作可疑记录报出来，不会被静默忽略。
func (r *Recorder) Finish(id int64, rec Record) {
	if id == 0 {
		return
	}
	db := r.db()
	if db == nil {
		return
	}
	updates := map[string]any{
		"result": rec.result(),
		"bytes":  rec.BytesIn + rec.BytesOut,
	}
	if err := db.Model(&storage.OutboundLog{}).Where("id = ?", id).Updates(updates).Error; err != nil {
		logx.For("privacy").Warn("outbound log update failed", "id", id, "result", rec.result(), "error", err.Error())
	}
}

// Blocked 记一条「被闸门拦下、没有真的发出去」的记录。
func (r *Recorder) Blocked(kind, purpose, target string, audio bool) {
	r.Record(Record{Kind: kind, Purpose: purpose, Target: target, Audio: audio, Result: ResultBlocked})
}

// Scan 按时间窗与类型过滤读取记录（新的在前），供自检与统计使用。
func (r *Recorder) Scan(from, to time.Time, kinds []string, limit int) ([]contract.OutboundRecord, error) {
	db := r.db()
	if db == nil {
		return nil, nil
	}
	q := db.Model(&storage.OutboundLog{})
	if !from.IsZero() {
		q = q.Where("at >= ?", from.UnixMilli())
	}
	if !to.IsZero() {
		q = q.Where("at <= ?", to.UnixMilli())
	}
	if len(kinds) > 0 {
		normalized := make([]string, 0, len(kinds))
		for _, k := range kinds {
			if n := normalizeKind(k); n != "" {
				normalized = append(normalized, n)
			}
		}
		if len(normalized) > 0 {
			q = q.Where("kind IN ?", normalized)
		}
	}
	if limit > 0 {
		q = q.Limit(limit)
	}
	var rows []storage.OutboundLog
	if err := q.Order("at DESC, id DESC").Find(&rows).Error; err != nil {
		return nil, apierr.Wrap(err, apierr.CodeDBWrite).WithDetail("outbound_log read failed")
	}
	out := make([]contract.OutboundRecord, 0, len(rows))
	for _, row := range rows {
		out = append(out, toRecord(row))
	}
	return out, nil
}

// ListOptions 是分页读取参数。
type ListOptions struct {
	Limit  int
	Offset int
}

// List 分页读取出站记录，同时返回总条数（前端分页器需要）。
func (r *Recorder) List(opt ListOptions) ([]contract.OutboundRecord, int64, error) {
	db := r.db()
	if db == nil {
		return nil, 0, nil
	}
	limit := opt.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	offset := opt.Offset
	if offset < 0 {
		offset = 0
	}
	var total int64
	if err := db.Model(&storage.OutboundLog{}).Count(&total).Error; err != nil {
		return nil, 0, apierr.Wrap(err, apierr.CodeDBWrite).WithDetail("outbound_log count failed")
	}
	var rows []storage.OutboundLog
	if err := db.Order("at DESC, id DESC").Limit(limit).Offset(offset).Find(&rows).Error; err != nil {
		return nil, 0, apierr.Wrap(err, apierr.CodeDBWrite).WithDetail("outbound_log read failed")
	}
	out := make([]contract.OutboundRecord, 0, len(rows))
	for _, row := range rows {
		out = append(out, toRecord(row))
	}
	return out, total, nil
}

// Stats 是出站记录的汇总计数（最近 24h）。
type Stats struct {
	// Total 窗口内总条数。
	Total int64
	// Blocked 被闸门拦下的条数（没有真的出站）。
	Blocked int64
	// Real 真实出站条数。
	Real int64
	// Pending 结果未知的条数。
	Pending int64
	// LastAt 最近一条记录的时刻（ms，0 表示无记录）。
	LastAt int64
}

// Stats 统计窗口内的出站情况。
func (r *Recorder) Stats(from, to time.Time) (Stats, error) {
	var st Stats
	db := r.db()
	if db == nil {
		return st, nil
	}
	rows, err := r.Scan(from, to, nil, 0)
	if err != nil {
		return st, err
	}
	st.Total = int64(len(rows))
	for _, row := range rows {
		switch {
		case isBlockedResult(row.Result):
			st.Blocked++
		case isPendingResult(row.Result):
			st.Pending++
		default:
			st.Real++
		}
		if row.At > st.LastAt {
			st.LastAt = row.At
		}
	}
	return st, nil
}

// Clear 清空全部出站记录，返回删除条数。
func (r *Recorder) Clear() (int64, error) {
	db := r.db()
	if db == nil {
		return 0, nil
	}
	res := db.Where("1 = 1").Delete(&storage.OutboundLog{})
	if res.Error != nil {
		return 0, apierr.Wrap(res.Error, apierr.CodeDBWrite).WithDetail("outbound_log clear failed")
	}
	return res.RowsAffected, nil
}

// Purge 清理超出保留期的记录（启动时调用一次）。
func (r *Recorder) Purge(now time.Time) error {
	db := r.db()
	if db == nil {
		return nil
	}
	if err := storage.PurgeOldOutbound(db, now, Retention); err != nil {
		return apierr.Wrap(err, apierr.CodeDBWrite).WithDetail("outbound_log purge failed")
	}
	return nil
}

// toRecord 把数据库行转成对外 DTO，并标出 blocked / pending。
func toRecord(row storage.OutboundLog) contract.OutboundRecord {
	return contract.OutboundRecord{
		At:      row.At,
		Host:    row.Host,
		Kind:    row.Kind,
		Audio:   row.Audio,
		Bytes:   row.Bytes,
		Result:  row.Result,
		Blocked: isBlockedResult(row.Result),
		Pending: isPendingResult(row.Result),
	}
}

// isBlockedResult 判断某条记录是否只是「被拦下的尝试」。
func isBlockedResult(result string) bool {
	r := strings.ToLower(strings.TrimSpace(result))
	return r == ResultBlocked || r == strings.ToLower(apierr.CodeOfflineEnforced)
}

// isPendingResult 判断某条记录是否结果未知。
func isPendingResult(result string) bool {
	return strings.EqualFold(strings.TrimSpace(result), ResultPending)
}

// ---------------------------------------------------------------------------
// 先留痕再执行
// ---------------------------------------------------------------------------

// Call 描述一次出站意图。
type Call struct {
	// Kind 业务类型（model-download / online-asr）。
	Kind string
	// Purpose 具体用途（catalog / bundle / probe / finalize）。
	Purpose string
	// Target 目标 URL。
	Target string
	// Audio 是否携带音频（FR-08：出站可见要能区分）。
	Audio bool
}

// Outcome 是一次出站的实际结果（由业务方在 fn 里填）。
type Outcome struct {
	// BytesIn 上传字节数。
	BytesIn int64
	// BytesOut 下载字节数。
	BytesOut int64
	// Status HTTP 状态码。
	Status int
	// ErrorCode 失败时的 ERR_* 码。
	ErrorCode string
	// Result 显式结果（如 ok），空时按 ErrorCode / Status 推导。
	Result string
}

// Pipe 把离线闸门与留痕串成一个入口：业务方不直接配对 Check/Record，
// 避免「某个分支忘了留痕」或「先发了再判离线」。
type Pipe struct {
	gate *Gate
	rec  *Recorder
}

// NewPipe 构造管道。gate / rec 为 nil 时退化为「只看环境变量」/「不落库」。
func NewPipe(gate *Gate, rec *Recorder) *Pipe {
	if gate == nil {
		gate = NewGate(GateOptions{})
	}
	return &Pipe{gate: gate, rec: rec}
}

// Gate 暴露闸门，供只做检查（不发请求）的场景使用。
func (p *Pipe) Gate() *Gate {
	if p == nil {
		return NewGate(GateOptions{})
	}
	return p.gate
}

// Recorder 暴露记录器，供 L3 直接查询。
func (p *Pipe) Recorder() *Recorder {
	if p == nil {
		return nil
	}
	return p.rec
}

// ErrNilFunc 是 DoRecorded 的内部错误（fn 为空说明调用方写错了）。
var ErrNilFunc = errors.New("privacy: nil outbound function")

// DoRecorded 是**唯一**推荐的出站入口：
//
//  1. 先过离线闸门：离线时立即返回 ERR_OFFLINE_ENFORCED，**绝不调用 fn**，
//     只往库里记一条 blocked（证明「拦住了」，而不是「没发生过」）；
//  2. 发起前登记一行 pending（先留痕）；
//  3. 执行 fn，无论成功 / 失败 / panic 都把结果补进同一行。
//
// 返回值就是 fn 的错误，调用方可直接透传给前端。
func (p *Pipe) DoRecorded(ctx context.Context, call Call, fn func(context.Context) (Outcome, error)) error {
	if fn == nil {
		return ErrNilFunc
	}
	if p == nil {
		p = NewPipe(nil, nil)
	}

	if err := p.gate.Check(call.Kind, call.Target); err != nil {
		if p.rec != nil {
			p.rec.Blocked(call.Kind, call.Purpose, call.Target, call.Audio)
		}
		return err
	}

	started := time.Now()
	rec := Record{Kind: call.Kind, Purpose: call.Purpose, Target: call.Target, Audio: call.Audio, Result: ResultPending}
	id := int64(0)
	if p.rec != nil {
		id = p.rec.Record(rec)
	}

	out := Outcome{}
	var callErr error
	finished := false
	finalize := func() {
		if finished {
			return
		}
		finished = true
		rec.At = started
		rec.Duration = time.Since(started)
		rec.BytesIn = out.BytesIn
		rec.BytesOut = out.BytesOut
		rec.Status = out.Status
		rec.ErrorCode = out.ErrorCode
		rec.Result = out.Result
		if rec.ErrorCode != "" {
			// 失败但不带码时归到 ResultError；日志只写码与耗时（不写响应体）。
			logx.For("privacy").Warn("outbound failed",
				"kind", rec.KindColumn(), "purpose", rec.Purpose, "host", HostOf(call.Target),
				"status", rec.Status, "errorCode", rec.ErrorCode,
				"ms", rec.Duration.Milliseconds())
		}
		if p.rec != nil {
			p.rec.Finish(id, rec)
		}
	}

	defer func() {
		if r := recover(); r != nil {
			// panic 也要留痕：结果记成 error，然后原样抛回去（不吞异常）。
			out.ErrorCode = rec.ErrorCode
			rec.Result = ResultError
			finalize()
			panic(r)
		}
		finalize()
	}()

	out, callErr = fn(ctx)
	if callErr != nil && out.ErrorCode == "" {
		out.ErrorCode = errorCodeOf(callErr)
	}
	return callErr
}

// errorCodeOf 从 error 里取 ERR_* 码；取不到时给通用 ResultError。
func errorCodeOf(err error) string {
	if err == nil {
		return ""
	}
	var e *apierr.Err
	if errors.As(err, &e) && e != nil && e.Code != "" {
		return e.Code
	}
	return ResultError
}
