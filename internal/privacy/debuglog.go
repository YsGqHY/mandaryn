package privacy

// 调试模式（设置 → 隐私边界 → 调试模式）的领域规则：
//   - DebugMode：环境变量层（MANDARYN_LOG_LEVEL）与用户层（app_settings.log_level）的裁决；
//   - SanitizeDebugAttr：写入调试日志前的属性脱敏（注入给 logx 当 ReplaceAttr）；
//   - ReadDebugLog：设置页查看器的读取与筛选。
//
// 机制（文件、滚动、开关）在 internal/utils/logx；这里只放规则，便于单测直接驱动。

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/utils/logx"
)

// EnvLogLevel 环境变量：debug 强制开启调试模式，info / off 强制关闭（覆盖设置页）。
const EnvLogLevel = "MANDARYN_LOG_LEVEL"

// app_settings.log_level 的取值（DOC-04 TBL-02）。调试模式开 = debug，关 = info；
// off 视同「调试模式关」，不实现「关闭全部日志」。
const (
	LogLevelInfo  = "info"
	LogLevelDebug = "debug"
	LogLevelOff   = "off"
)

// 截断上限：普通字符串沿用 Redactor 的 160；消息与错误说明保留更长一些便于排查。
const (
	debugMsgMaxLen   = 300
	debugErrorMaxLen = 300
)

// ---------------------------------------------------------------------------
// DebugMode：开关裁决
// ---------------------------------------------------------------------------

// DebugMode 是调试模式的「环境变量 + 用户层」裁决，线程安全。
//
// 生效值 = 环境变量（若设置）否则用户层。环境变量接管时用户层的值原样保留，
// 设置页据 Source 禁用开关并说明原因（与离线开关的 envForced 一致）。
type DebugMode struct {
	mu      sync.Mutex
	envSet  bool
	envOn   bool
	envBad  string // 无法识别的环境变量值（启动时记一条警告）
	user    bool
	applied bool
	apply   func(on bool) error
}

// NewDebugMode 读取环境变量构造裁决器；getenv 为 nil 时用 os.Getenv。
// 默认通过 logx.SetDebugEnabled 落实开关。
func NewDebugMode(getenv func(string) string) *DebugMode {
	if getenv == nil {
		getenv = os.Getenv
	}
	d := &DebugMode{apply: logx.SetDebugEnabled}
	switch v := strings.ToLower(strings.TrimSpace(getenv(EnvLogLevel))); v {
	case "":
	case LogLevelDebug:
		d.envSet, d.envOn = true, true
	case LogLevelInfo, LogLevelOff:
		d.envSet, d.envOn = true, false
	default:
		d.envBad = v
	}
	return d
}

// WithApply 替换落实开关的函数（测试用）。
func (d *DebugMode) WithApply(fn func(on bool) error) *DebugMode {
	d.mu.Lock()
	d.apply = fn
	d.mu.Unlock()
	return d
}

// InvalidEnv 返回无法识别的环境变量值（空 = 未设置或合法）。
func (d *DebugMode) InvalidEnv() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.envBad
}

// EnvManaged 报告是否被环境变量接管。
func (d *DebugMode) EnvManaged() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.envSet
}

// Apply 按当前裁决打开 / 关闭调试日志。开启失败时生效值回落为关并返回错误。
func (d *DebugMode) Apply() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.applyLocked()
}

func (d *DebugMode) applyLocked() error {
	want := d.user
	if d.envSet {
		want = d.envOn
	}
	if d.apply == nil {
		d.applied = want
		return nil
	}
	if err := d.apply(want); err != nil {
		d.applied = false
		return err
	}
	d.applied = want
	return nil
}

// SetUser 更新用户层的值并立即落实，返回新的生效状态。
func (d *DebugMode) SetUser(on bool) (contract.DebugModeState, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.user = on
	err := d.applyLocked()
	return d.stateLocked(), err
}

// SetUserLevel 用 app_settings.log_level 的取值更新用户层。
func (d *DebugMode) SetUserLevel(level string) (contract.DebugModeState, error) {
	return d.SetUser(strings.EqualFold(strings.TrimSpace(level), LogLevelDebug))
}

// State 返回当前生效状态。
func (d *DebugMode) State() contract.DebugModeState {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.stateLocked()
}

func (d *DebugMode) stateLocked() contract.DebugModeState {
	st := contract.DebugModeState{
		Enabled:     d.applied,
		UserEnabled: d.user,
		Source:      contract.DebugModeSourceUser,
	}
	if d.envSet {
		st.Source = contract.DebugModeSourceEnv
		st.EnvVar = EnvLogLevel
	}
	return st
}

// LogLevelFor 把开关值换成 app_settings.log_level 的取值。
func LogLevelFor(enabled bool) string {
	if enabled {
		return LogLevelDebug
	}
	return LogLevelInfo
}

// ---------------------------------------------------------------------------
// SanitizeDebugAttr：写入前脱敏
// ---------------------------------------------------------------------------

// 用户主目录：盘符路径 / Unix 路径里的用户名一律换成 ~（日志里只需要知道「在用户目录下」）。
var (
	winHomePattern  = regexp.MustCompile(`(?i)([A-Z]:[\\/]+Users[\\/]+)[^\\/\s"':;,]+`)
	unixHomePattern = regexp.MustCompile(`(/(?:home|Users)/)[^/\s"':;,]+`)
)

// maskHomeDir 把路径里的用户名替换为 ~。
func maskHomeDir(s string) string {
	s = winHomePattern.ReplaceAllString(s, "${1}~")
	return unixHomePattern.ReplaceAllString(s, "${1}~")
}

// SanitizeDebugAttr 是调试日志的 slog.ReplaceAttr（由 internal/app 注入 logx）。
//
// 规则（与诊断包同源，见 docs/DEPLOYMENT.md §5.1）：
//   - 敏感键名（正文 / 音频 / 声纹 / 凭据，见 sensitiveFields）→ ***；
//   - 普通字符串：连续 ≥4 个中文或含换行（可能是转写正文、会话标题）、二进制 → 整段替换；
//     密钥模式 → ***；用户主目录 → ~；超长截断；
//   - err / error：只做密钥与路径脱敏并截断（保留中文错误说明，排查必需）；
//   - 数值数组长度 ≥8（声纹 / 特征向量）→ [REDACTED:vector:n]；
//   - msg：开发者写的常量文本，只做密钥与路径脱敏，不按中文规则剔除。
func SanitizeDebugAttr(groups []string, a slog.Attr) slog.Attr {
	if len(groups) == 0 {
		switch a.Key {
		case slog.TimeKey, slog.LevelKey, slog.SourceKey:
			return a
		case slog.MessageKey:
			return slog.String(a.Key, sanitizeFreeText(a.Value.String(), debugMsgMaxLen))
		}
	}
	return sanitizeDebugAttr(a)
}

func sanitizeDebugAttr(a slog.Attr) slog.Attr {
	if sensitiveField(a.Key) {
		return slog.String(a.Key, masked)
	}
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindString:
		if isErrorKey(a.Key) {
			return slog.String(a.Key, sanitizeFreeText(v.String(), debugErrorMaxLen))
		}
		if max, ok := longTextKeys[strings.ToLower(a.Key)]; ok {
			return slog.String(a.Key, sanitizeLongText(v.String(), max))
		}
		return slog.String(a.Key, sanitizeValueString(v.String()))
	case slog.KindGroup:
		attrs := v.Group()
		out := make([]slog.Attr, 0, len(attrs))
		for _, ga := range attrs {
			out = append(out, sanitizeDebugAttr(ga))
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(out...)}
	case slog.KindAny:
		return slog.Attr{Key: a.Key, Value: sanitizeAnyValue(a.Key, v.Any())}
	default:
		// 数值、布尔、时长、时间：都是元数据，原样保留。
		return slog.Attr{Key: a.Key, Value: v}
	}
}

// sanitizeAnyValue 处理 slog.Any 的值：error / []byte / 数值切片 / 结构体等。
func sanitizeAnyValue(key string, x any) slog.Value {
	switch t := x.(type) {
	case nil:
		return slog.AnyValue(nil)
	case error:
		return slog.StringValue(sanitizeFreeText(t.Error(), debugErrorMaxLen))
	case []byte:
		return slog.StringValue(binaryMarker(len(t)))
	case fmt.Stringer:
		if isErrorKey(key) {
			return slog.StringValue(sanitizeFreeText(t.String(), debugErrorMaxLen))
		}
		return slog.StringValue(sanitizeValueString(t.String()))
	}
	rv := reflect.ValueOf(x)
	if (rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array) && isNumberKind(rv.Type().Elem().Kind()) {
		if rv.Len() >= defaultVectorLen {
			return slog.StringValue(vectorMarker(rv.Len()))
		}
	}
	// 其余复合值：转成 JSON 走与诊断包相同的结构化脱敏（敏感键整值替换、长字符串剔除）。
	raw, err := json.Marshal(x)
	if err != nil {
		return slog.StringValue(sanitizeValueString(fmt.Sprint(x)))
	}
	// 先在 JSON 文本上做主目录脱敏（JSON 转义后的双反斜杠同样匹配），再结构化脱敏。
	var generic any
	if json.Unmarshal([]byte(maskHomeDir(string(raw))), &generic) != nil {
		return slog.StringValue(binaryMarker(len(raw)))
	}
	red := NewRedactor()
	return slog.AnyValue(red.walk(generic))
}

// sanitizeValueString：普通字符串字段（Redactor 规则 + 主目录脱敏）。
func sanitizeValueString(s string) string {
	return NewRedactor().sanitizeString(maskHomeDir(s))
}

// longTextKeys 是允许多行 / 较长内容的字段（前端上报的消息与堆栈）及其截断上限。
// 它们来自页面，可能夹带界面上的正文，因此仍按中文规则整段剔除，只放宽长度与换行。
var longTextKeys = map[string]int{
	"message": 500,
	"stack":   1000,
}

// sanitizeLongText：含 ≥4 个中文视为正文整段替换；否则做密钥 / 路径脱敏、折叠换行、截断。
func sanitizeLongText(s string, max int) string {
	if cjkCount(s) >= cjkThreshold {
		return textMarker(len(s))
	}
	return sanitizeFreeText(s, max)
}

// sanitizeFreeText：消息与错误说明（只做密钥 / 路径脱敏、折叠换行与控制字符、截断）。
func sanitizeFreeText(s string, max int) string {
	if !utf8.ValidString(s) {
		return binaryMarker(len(s))
	}
	out := redactSecretsInText(maskHomeDir(s))
	out = strings.Map(scrubControl, out)
	if len(out) > max {
		out = truncate(out, max) + "…"
	}
	return out
}

func isErrorKey(key string) bool {
	switch strings.ToLower(key) {
	case "err", "error", "cause":
		return true
	}
	return false
}

func isNumberKind(k reflect.Kind) bool {
	switch k {
	case reflect.Float32, reflect.Float64,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// ReadDebugLog：查看器读取
// ---------------------------------------------------------------------------

const (
	// debugReadBudget 一次查询最多读取的字节数（跨所有滚动文件，从最新往旧读）。
	debugReadBudget int64 = 2 << 20
	debugDefaultLimit     = 200
	debugMaxLimit         = 500
)

// levelRank 把级别名映射为可比较的序号；未知级别按 INFO 处理。
func levelRank(level string) int {
	switch strings.ToUpper(strings.TrimSpace(level)) {
	case "DEBUG":
		return 0
	case "WARN", "WARNING":
		return 2
	case "ERROR":
		return 3
	default:
		return 1
	}
}

// ReadDebugLog 读取 dir 下的调试日志并按条件筛选，新的在前。
//
// 只读打开，与写入互不影响；调试模式关闭时同样可读。
func ReadDebugLog(dir string, q contract.DebugLogQuery) (contract.DebugLogPage, error) {
	page := contract.DebugLogPage{Entries: []contract.DebugLogEntry{}, Components: []string{}}
	minLevel := strings.ToUpper(strings.TrimSpace(q.MinLevel))
	switch minLevel {
	case "", "DEBUG", "INFO", "WARN", "ERROR":
	default:
		return page, apierr.Newf(apierr.CodeInvalidArgument, "minLevel must be DEBUG, INFO, WARN or ERROR")
	}
	origin := strings.ToLower(strings.TrimSpace(q.Origin))
	switch origin {
	case "", logx.OriginBackend, logx.OriginFrontend:
	default:
		return page, apierr.Newf(apierr.CodeInvalidArgument, "origin must be backend or frontend")
	}
	limit := q.Limit
	if limit <= 0 {
		limit = debugDefaultLimit
	}
	if limit > debugMaxLimit {
		limit = debugMaxLimit
	}
	if strings.TrimSpace(dir) == "" {
		return page, nil
	}

	components := map[string]bool{}
	budget := debugReadBudget
	base := filepath.Join(dir, logx.DebugFileName)
	paths := []string{base}
	for i := 1; i < logx.DebugMaxFiles; i++ {
		paths = append(paths, fmt.Sprintf("%s.%d", base, i))
	}

	for _, p := range paths {
		if budget <= 0 {
			// 预算用完但还有更旧的文件：标记截断。
			if _, err := os.Stat(p); err == nil {
				page.Truncated = true
			}
			break
		}
		st, err := os.Stat(p)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return page, apierr.Wrap(err, apierr.CodeNotFound)
		}
		if st.Size() > budget {
			page.Truncated = true
		}
		lines, err := ReadLogTail(p, budget)
		if err != nil {
			return page, apierr.Wrap(err, apierr.CodeNotFound)
		}
		budget -= st.Size()
		// 文件内是时间正序：倒着遍历得到新的在前。
		for i := len(lines) - 1; i >= 0; i-- {
			line := strings.TrimSpace(lines[i])
			if line == "" {
				continue
			}
			entry, ok := parseDebugLine(line)
			page.Scanned++
			if !ok {
				page.Malformed++
				continue
			}
			if entry.Component != "" {
				components[entry.Component] = true
			}
			if minLevel != "" && levelRank(entry.Level) < levelRank(minLevel) {
				continue
			}
			if origin != "" && entry.Origin != origin {
				continue
			}
			if q.Component != "" && entry.Component != q.Component {
				continue
			}
			if len(page.Entries) >= limit {
				page.Truncated = true
				continue
			}
			page.Entries = append(page.Entries, entry)
		}
	}

	for c := range components {
		page.Components = append(page.Components, c)
	}
	sort.Strings(page.Components)
	return page, nil
}

// parseDebugLine 把一行 JSON 拆成固定字段 + 其余上下文字段。
func parseDebugLine(line string) (contract.DebugLogEntry, bool) {
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		return contract.DebugLogEntry{}, false
	}
	take := func(key string) string {
		v, ok := m[key]
		if !ok {
			return ""
		}
		delete(m, key)
		if s, ok := v.(string); ok {
			return s
		}
		return fmt.Sprint(v)
	}
	e := contract.DebugLogEntry{
		Time:      take(slog.TimeKey),
		Level:     take(slog.LevelKey),
		Source:    take(slog.SourceKey),
		Msg:       take(slog.MessageKey),
		Component: take("component"),
		Origin:    take(logx.OriginKey),
	}
	if e.Time == "" && e.Msg == "" {
		return contract.DebugLogEntry{}, false
	}
	fields := "{}"
	if len(m) > 0 {
		if raw, err := json.Marshal(m); err == nil {
			fields = string(raw)
		}
	}
	e.Fields = fields
	return e, true
}

// DebugLogStatusFor 汇总设置页卡片要展示的状态（目录、文件、占用、保留策略）。
func DebugLogStatusFor(mode contract.DebugModeState) (contract.DebugLogStatus, error) {
	st := contract.DebugLogStatus{
		Mode:          mode,
		Dir:           logx.DebugLogDir(),
		Format:        "jsonl",
		Files:         []contract.DebugLogFile{},
		MaxFileBytes:  logx.DebugMaxFileBytes,
		MaxFiles:      logx.DebugMaxFiles,
		RetentionDays: int(logx.DebugMaxAge.Hours() / 24),
	}
	files, err := logx.DebugLogFiles()
	if err != nil {
		return st, err
	}
	for _, f := range files {
		st.Files = append(st.Files, contract.DebugLogFile{
			Name:       f.Name,
			SizeBytes:  f.SizeBytes,
			ModifiedAt: f.ModifiedAt.UnixMilli(),
		})
		st.TotalBytes += f.SizeBytes
	}
	return st, nil
}

// ReadDebugLogTail 返回所有调试日志文件末尾合计至多 maxBytes 的行（诊断包用，时间正序）。
func ReadDebugLogTail(dir string, maxBytes int64) []string {
	if strings.TrimSpace(dir) == "" {
		return nil
	}
	if maxBytes <= 0 {
		maxBytes = defaultMaxLogBytes
	}
	base := filepath.Join(dir, logx.DebugFileName)
	var chunks [][]string
	budget := maxBytes
	for i := 0; i < logx.DebugMaxFiles && budget > 0; i++ {
		p := base
		if i > 0 {
			p = fmt.Sprintf("%s.%d", base, i)
		}
		st, err := os.Stat(p)
		if err != nil {
			continue
		}
		lines, err := ReadLogTail(p, budget)
		if err != nil {
			continue
		}
		chunks = append(chunks, lines)
		budget -= st.Size()
	}
	var out []string
	for i := len(chunks) - 1; i >= 0; i-- {
		for _, l := range chunks[i] {
			if strings.TrimSpace(l) != "" {
				out = append(out, l)
			}
		}
	}
	return out
}
