package privacy

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"vip.oberon.mandaryn/internal/contract"
)

// 脱敏阈值与常量。
const (
	// cjkThreshold 连续中日韩字符达到此数量即判为「正文」，整条剔除。
	// 本项目自身的日志 / 错误码 / 模块名都是 ASCII，因此剔除不会损失元数据。
	cjkThreshold = 4
	// defaultMaxStringLen 单个字符串值的保留上限，超出截断（正文往往很长）。
	defaultMaxStringLen = 160
	// defaultVectorLen 数字数组长度达到此值即判为「声纹 / 特征向量」，整体剔除。
	defaultVectorLen = 8
	// defaultMaxLogBytes 诊断包里日志副本的默认读取上限。
	defaultMaxLogBytes = 512 << 10
	// diagnosticsMIME 诊断包 MIME。
	diagnosticsMIME = "application/json"
)

// textMarker 是文本类脱敏的占位符（带长度，便于判断「这里本来有东西」）。
func textMarker(n int) string { return "[REDACTED:text:" + itoa(n) + "]" }

// vectorMarker 是向量类脱敏的占位符。
func vectorMarker(n int) string { return "[REDACTED:vector:" + itoa(n) + "]" }

// binaryMarker 是二进制 / 非 UTF-8 内容的占位符。
func binaryMarker(n int) string { return "[REDACTED:binary:" + itoa(n) + "B]" }

// sensitiveFields 是「键名出现即视为敏感」的集合（归一化后比较：小写、去 _/-/.）。
//
// 覆盖 FR-08 的四类禁区：转写正文、音频样本、声纹向量、凭据。
var sensitiveFields = map[string]bool{
	// 正文
	"text": true, "texts": true, "content": true, "contents": true, "body": true,
	"transcript": true, "transcripts": true, "segment": true, "segments": true,
	"partial": true, "utterance": true, "utterances": true, "sentence": true,
	"sentences": true, "words": true, "srt": true, "vtt": true, "subtitle": true,
	// 音频
	"audio": true, "audiobytes": true, "samples": true, "sample": true, "pcm": true,
	"wav": true, "flac": true, "waveform": true, "blob": true, "clip": true, "clips": true,
	// 声纹 / 特征向量
	"vector": true, "vectors": true, "embedding": true, "embeddings": true,
	"voiceprint": true, "voiceprints": true, "features": true, "centroid": true,
	"embeddingvector": true, "feature": true,
	// 凭据
	"key": true, "keys": true, "apikey": true, "accesskey": true, "secretkey": true,
	"secret": true, "secrets": true, "clientsecret": true, "token": true, "tokens": true,
	"accesstoken": true, "refreshtoken": true, "idtoken": true, "auth": true,
	"authorization": true, "proxyauthorization": true, "cookie": true, "cookies": true,
	"password": true, "passwd": true, "pwd": true, "credential": true, "credentials": true,
	"signature": true, "sig": true, "privatekey": true, "privkey": true, "pem": true,
	"ciphertext": true, "enc": true, "jwt": true, "sessionkey": true,
}

// secretPatterns 是「未挂在敏感键下、但一眼就是凭据」的文本模式，
// 用于兜住日志正文、错误详情、URL query 等自由文本。
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`sk-[A-Za-z0-9_\-]{8,}`),
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{16,}`),
	regexp.MustCompile(`xox[baprs]-[A-Za-z0-9\-]{10,}`),
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	regexp.MustCompile(`eyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}`),
	regexp.MustCompile(`[A-Fa-f0-9]{32,}`),
	regexp.MustCompile(`[A-Za-z0-9+/=_\-]{40,}`),
}

// keyValueSecret 匹配 `token=xxx` / `Authorization: xxx` 这类自由文本里的凭据对。
// 值允许带 `Bearer ` 前缀，避免先被 bearer 规则替换后残留明文。
var keyValueSecret = regexp.MustCompile(`(?i)(authorization|proxy-authorization|api[_-]?key|apikey|access[_-]?token|refresh[_-]?token|token|secret|password|passwd|pwd|cookie|set-cookie)\s*[:=]\s*(?:(?i:bearer|basic|token)\s+)?("[^"]*"|'[^']*'|[^\s,;&}]+)`)

// bearerPattern 匹配 `Bearer xxx`。
var bearerPattern = regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._\-+/=]{8,}`)

// Redactor 是诊断材料的脱敏器。
//
// 设计目标是**断言级**：只要产物经过它，就能用简单断言验证
// 「正文 / 音频 / 声纹向量 / 凭据不出现在输出里」。做法是宁可丢信息：
//   - 敏感键（text / audio / embedding / token …）整个值换成 ***；
//   - 数字数组超过阈值视为向量，整体替换；
//   - 含连续中文或换行的字符串视为正文，整体剔除；
//   - 非 UTF-8 / 二进制内容整体剔除；
//   - 自由文本里命中密钥模式的片段替换为 ***。
type Redactor struct {
	// MaxStringLen 单个字符串保留上限；0 用默认值。
	MaxStringLen int
	// VectorLen 数字数组判为向量的长度阈值；0 用默认值。
	VectorLen int
	// MaxLogBytes 日志副本读取上限；0 用默认值。
	MaxLogBytes int64

	redactions int
	dropped    int
}

// NewRedactor 构造默认脱敏器。
func NewRedactor() *Redactor {
	return &Redactor{
		MaxStringLen: defaultMaxStringLen,
		VectorLen:    defaultVectorLen,
		MaxLogBytes:  defaultMaxLogBytes,
	}
}

// Stats 返回本次使用的脱敏命中数与丢弃条目数。
func (r *Redactor) Stats() (redactions, dropped int) {
	if r == nil {
		return 0, 0
	}
	return r.redactions, r.dropped
}

func (r *Redactor) maxStringLen() int {
	if r.MaxStringLen > 0 {
		return r.MaxStringLen
	}
	return defaultMaxStringLen
}

func (r *Redactor) vectorLen() int {
	if r.VectorLen > 0 {
		return r.VectorLen
	}
	return defaultVectorLen
}

func (r *Redactor) maxLogBytes() int64 {
	if r.MaxLogBytes > 0 {
		return r.MaxLogBytes
	}
	return defaultMaxLogBytes
}

// Redact 对任意产物做脱敏，返回**保证安全**的字节。
//
// 输入是合法 JSON 时按结构脱敏（保留键名与数值元数据）；
// 否则按文本逐行脱敏（日志副本走这条路径）。
func (r *Redactor) Redact(payload []byte) []byte {
	if r == nil {
		r = NewRedactor()
	}
	trimmed := strings.TrimSpace(string(payload))
	if trimmed == "" {
		return payload
	}
	var v any
	if json.Unmarshal(payload, &v) == nil {
		out, err := json.Marshal(r.walk(v))
		if err == nil {
			return out
		}
	}
	return r.redactText(payload)
}

// RedactString 脱敏单个字符串值（正文 / 二进制 / 密钥）。
func (r *Redactor) RedactString(s string) string {
	if r == nil {
		r = NewRedactor()
	}
	return r.sanitizeString(s)
}

// walk 递归脱敏 JSON 结构。
func (r *Redactor) walk(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if sensitiveField(k) {
				r.redactions++
				t[k] = masked
				continue
			}
			t[k] = r.walk(val)
		}
		return t
	case []any:
		if len(t) >= r.vectorLen() && allNumbers(t) {
			r.redactions++
			return vectorMarker(len(t))
		}
		for i := range t {
			t[i] = r.walk(t[i])
		}
		return t
	case string:
		return r.sanitizeString(t)
	default:
		return v
	}
}

// sanitizeString 是字符串级脱敏的单一入口（JSON 与文本路径共用）。
func (r *Redactor) sanitizeString(s string) string {
	if s == "" {
		return s
	}
	if !utf8.ValidString(s) || looksBinary(s) {
		r.redactions++
		return binaryMarker(len(s))
	}
	if cjkCount(s) >= cjkThreshold || strings.ContainsAny(s, "\n\r") {
		// 连续中文或换行：转写正文 / 多行载荷的典型特征，整条剔除。
		r.redactions++
		return textMarker(len(s))
	}
	clean, n := redactText(s)
	if n > 0 {
		r.redactions += n
	}
	if max := r.maxStringLen(); len(clean) > max {
		r.redactions++
		clean = truncate(clean, max) + "…"
	}
	return clean
}

// redactText 返回脱敏后的字节（日志副本路径）。
func (r *Redactor) redactText(payload []byte) []byte {
	s := string(payload)
	if !utf8.ValidString(s) {
		// 非 UTF-8 一律不保留原始字节：转成安全字符后按行剔除。
		s = strings.ToValidUTF8(s, "?")
		r.redactions++
	}
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			out = append(out, line)
			continue
		}
		clean, n := redactText(line)
		r.redactions += n
		// 控制字符（含 NUL）一律清掉：日志注入与二进制残渣都不该进诊断包。
		clean = strings.Map(scrubControl, clean)
		if cjkCount(clean) >= cjkThreshold {
			// 含中文的日志行：可能是正文落到日志里，整行丢弃。
			r.dropped++
			continue
		}
		if max := r.maxStringLen() * 4; len(clean) > max {
			r.redactions++
			clean = truncate(clean, max) + "…"
		}
		out = append(out, clean)
	}
	return []byte(strings.Join(out, "\n"))
}

// redactText 对一段自由文本做密钥 / 凭据脱敏，返回结果与命中次数。
func redactText(s string) (string, int) {
	hits := 0
	out := bearerPattern.ReplaceAllStringFunc(s, func(string) string {
		hits++
		return "Bearer " + masked
	})
	out = keyValueSecret.ReplaceAllStringFunc(out, func(m string) string {
		hits++
		name, _, ok := strings.Cut(m, "=")
		if !ok {
			name, _, _ = strings.Cut(m, ":")
		}
		return strings.TrimSpace(name) + "=" + masked
	})
	for _, re := range secretPatterns {
		out = re.ReplaceAllStringFunc(out, func(string) string {
			hits++
			return masked
		})
	}
	return out, hits
}

// redactSecretsInText 是 redactText 的无计数形态（供 URL 脱敏复用）。
func redactSecretsInText(s string) string {
	out, _ := redactText(s)
	return out
}

// scrubControl 清掉控制字符（含 NUL），把制表 / 换行折成空格。
//
// 日志行与 URL 都可能被外部内容注入控制字符，留着会污染诊断包并干扰断言。
func scrubControl(r rune) rune {
	switch r {
	case '\t', '\n', '\r':
		return ' '
	}
	if unicode.IsControl(r) {
		return -1
	}
	return r
}

// sensitiveField 归一化键名后查表。
func sensitiveField(key string) bool {
	k := strings.ToLower(strings.TrimSpace(key))
	k = strings.NewReplacer("_", "", "-", "", ".", "", " ", "").Replace(k)
	if k == "" {
		return false
	}
	return sensitiveFields[k]
}

// allNumbers 判断数组是否全为数字（向量 / 采样点的判据）。
func allNumbers(arr []any) bool {
	for _, v := range arr {
		switch v.(type) {
		case float64, float32, int, int64, json.Number:
		default:
			return false
		}
	}
	return true
}

// looksBinary 判断字符串是否像二进制（NUL 或大量不可打印字节）。
func looksBinary(s string) bool {
	if strings.ContainsRune(s, 0) {
		return true
	}
	if len(s) < 8 {
		return false
	}
	nonPrint := 0
	for _, r := range s {
		if !unicode.IsPrint(r) && !unicode.IsSpace(r) {
			nonPrint++
		}
	}
	return nonPrint*10 > utf8.RuneCountInString(s)
}

// cjkCount 统计中日韩字符数量。
func cjkCount(s string) int {
	n := 0
	for _, r := range s {
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) ||
			unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r) {
			n++
		}
	}
	return n
}

// Residuals 检查产物里是否还有不该出现的东西，返回问题描述列表（空 = 干净）。
//
// 它是「断言级」脱敏的自检闸门：BuildBundle 在返回前会调它，
// 单测也直接用它逐项断言「正文 / 向量 / 密钥不出现在输出中」。
func Residuals(payload []byte) []string {
	var issues []string
	text := string(payload)
	for _, re := range secretPatterns {
		if loc := re.FindString(text); loc != "" {
			issues = append(issues, "secret-pattern:"+truncate(loc, 12))
		}
	}
	if bearerPattern.MatchString(text) {
		issues = append(issues, "bearer-token")
	}
	if run := longestCJKRun(text); run >= cjkThreshold {
		issues = append(issues, "cjk-text-run:"+itoa(run))
	}
	return issues
}

// longestCJKRun 返回最长的连续中日韩字符数。
func longestCJKRun(s string) int {
	longest, cur := 0, 0
	for _, r := range s {
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) ||
			unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r) {
			cur++
			if cur > longest {
				longest = cur
			}
			continue
		}
		cur = 0
	}
	return longest
}

// ---------------------------------------------------------------------------
// 诊断包
// ---------------------------------------------------------------------------

// BundleInput 是诊断包的原始素材，**全部是元数据**。
//
// 调用方**不得**在这里放音频、正文、声纹向量或凭据；即便放了，
// 也会被 Redactor 剔除并在自检里被发现。
type BundleInput struct {
	// AppVersion / Build / Platform / Arch / GoVersion 环境元数据。
	AppVersion string
	Build      string
	Platform   string
	Arch       string
	GoVersion  string
	// Offline / OfflineSource / OfflineEnvVar 离线状态快照。
	Offline       bool
	OfflineSource string
	OfflineEnvVar string
	// Modules 已装配模块清单。
	Modules []string
	// ErrorCounts 错误码计数（键必须是 ERR_* 码）。
	ErrorCounts map[string]int
	// Outbound 最近出站记录（只有元数据列）。
	Outbound []contract.OutboundRecord
	// LogLines 日志副本的行（会被逐行脱敏）。
	LogLines []string
	// DebugLogLines 调试日志副本的行（调试模式开过才有；同样逐行脱敏）。
	DebugLogLines []string
	// Now 生成时刻；零值用 time.Now()。
	Now time.Time
}

// bundleDoc 是诊断包的落盘结构（字段顺序固定，便于人读与 diff）。
type bundleDoc struct {
	Schema        string              `json:"schema"`
	GeneratedAt   string              `json:"generatedAt"`
	App           bundleApp           `json:"app"`
	Privacy       bundlePrivacy       `json:"privacy"`
	Modules       []string            `json:"modules"`
	Errors        []bundleErrorCount  `json:"errors"`
	Outbound      []bundleOutboundRow `json:"outbound"`
	Logs          []string            `json:"logs"`
	DebugLogs     []string            `json:"debugLogs,omitempty"`
	RedactionNote string              `json:"redactionNote"`
}

type bundleApp struct {
	Version   string `json:"version"`
	Build     string `json:"build"`
	Platform  string `json:"platform"`
	Arch      string `json:"arch"`
	GoVersion string `json:"goVersion"`
}

type bundlePrivacy struct {
	Offline       bool   `json:"offline"`
	OfflineSource string `json:"offlineSource"`
	OfflineEnvVar string `json:"offlineEnvVar"`
	RetentionHour int    `json:"retentionHours"`
}

type bundleErrorCount struct {
	Code  string `json:"code"`
	Count int    `json:"count"`
}

type bundleOutboundRow struct {
	AtMs   int64  `json:"atMs"`
	Host   string `json:"host"`
	Kind   string `json:"kind"`
	Audio  bool   `json:"audio"`
	Bytes  int64  `json:"bytes"`
	Result string `json:"result"`
}

// redactionNote 写进产物，说明「这里为什么是空的」。
const redactionNote = "audio, transcripts, speaker embeddings and credentials are removed; only metadata is kept"

// ErrRedactionResidual 表示脱敏自检失败（产物里仍有不该出现的内容）。
// 失败时**不返回**产物，宁可没有诊断包也不泄漏。
var ErrRedactionResidual = errors.New("privacy: diagnostics redaction self-check failed")

// BuildBundle 生成脱敏诊断包。
//
// 三段式：按结构装配 → Redact 脱敏 → Residuals 自检；
// 自检不过就丢掉日志副本再试一次，仍不过则返回 ErrRedactionResidual。
func BuildBundle(in BundleInput) (contract.DiagnosticsArtifact, error) {
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}

	doc := bundleDoc{
		Schema:      "mandaryn.diagnostics/1",
		GeneratedAt: now.UTC().Format(time.RFC3339),
		App: bundleApp{
			Version:   in.AppVersion,
			Build:     in.Build,
			Platform:  in.Platform,
			Arch:      in.Arch,
			GoVersion: in.GoVersion,
		},
		Privacy: bundlePrivacy{
			Offline:       in.Offline,
			OfflineSource: in.OfflineSource,
			OfflineEnvVar: in.OfflineEnvVar,
			RetentionHour: int(Retention.Hours()),
		},
		Modules:       append([]string(nil), in.Modules...),
		Logs:          append([]string(nil), in.LogLines...),
		DebugLogs:     append([]string(nil), in.DebugLogLines...),
		RedactionNote: redactionNote,
	}

	// 模块清单排序，产物稳定（便于比对）。
	sort.Strings(doc.Modules)

	for code, count := range in.ErrorCounts {
		doc.Errors = append(doc.Errors, bundleErrorCount{Code: code, Count: count})
	}
	sort.Slice(doc.Errors, func(i, j int) bool { return doc.Errors[i].Code < doc.Errors[j].Code })

	for _, row := range in.Outbound {
		doc.Outbound = append(doc.Outbound, bundleOutboundRow{
			AtMs:   row.At,
			Host:   row.Host,
			Kind:   row.Kind,
			Audio:  row.Audio,
			Bytes:  row.Bytes,
			Result: row.Result,
		})
	}

	red := NewRedactor()
	payload, err := buildAndCheck(red, doc)
	if err == nil {
		return artifact(now, payload, red), nil
	}

	// 兜底：日志副本是唯一「外部输入」，丢掉它再试一次。
	doc.Logs = nil
	doc.DebugLogs = nil
	red = NewRedactor()
	payload, err2 := buildAndCheck(red, doc)
	if err2 != nil {
		return contract.DiagnosticsArtifact{}, ErrRedactionResidual
	}
	red.dropped += len(in.LogLines) + len(in.DebugLogLines)
	return artifact(now, payload, red), nil
}

// buildAndCheck 装配 → 脱敏 → 自检。
func buildAndCheck(red *Redactor, doc bundleDoc) ([]byte, error) {
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, ErrRedactionResidual
	}
	payload := red.Redact(raw)
	if len(Residuals(payload)) > 0 {
		return nil, ErrRedactionResidual
	}
	return payload, nil
}

// artifact 组装对外回执。
func artifact(now time.Time, payload []byte, red *Redactor) contract.DiagnosticsArtifact {
	redactions, dropped := red.Stats()
	return contract.DiagnosticsArtifact{
		Filename:   "mandaryn-diagnostics-" + now.UTC().Format("20060102-150405") + ".json",
		MIME:       diagnosticsMIME,
		Bytes:      payload,
		Redactions: redactions,
		Dropped:    dropped,
	}
}

// ReadLogTail 读取日志文件末尾至多 maxBytes（0 用默认上限），按行返回。
//
// 只读日志文本，不碰音频目录；读到的内容交给 Redactor 脱敏后才进诊断包。
func ReadLogTail(path string, maxBytes int64) ([]string, error) {
	if maxBytes <= 0 {
		maxBytes = defaultMaxLogBytes
	}
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := info.Size()
	offset := int64(0)
	if size > maxBytes {
		offset = size - maxBytes
	}
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return nil, err
		}
	}
	buf, err := io.ReadAll(io.LimitReader(f, maxBytes))
	if err != nil {
		return nil, err
	}
	text := strings.ToValidUTF8(string(buf), "?")
	lines := strings.Split(text, "\n")
	if offset > 0 && len(lines) > 0 {
		// 首行可能是被切断的半行，丢弃。
		lines = lines[1:]
	}
	return lines, nil
}
