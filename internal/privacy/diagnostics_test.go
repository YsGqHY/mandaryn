package privacy

// 诊断包脱敏测试：断言级验证「中文正文 / 声纹 float 数组 / sk-xxx 密钥」
// 不出现在产物里，同时元数据（版本 / 平台 / 错误码）必须保留。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"vip.oberon.mandaryn/internal/contract"
)

// 测试用的「禁区样本」。
const (
	sampleTranscript = "今天下午的会议记录，讨论第三季度的预算分配和人员安排"
	sampleSecret     = "sk-live-abcdef1234567890abcdef"
	samplePassword   = "hunter2-super-password"
)

// sampleDiagnosticsJSON 是一份「什么都有」的假诊断输入。
func sampleDiagnosticsJSON(t *testing.T) []byte {
	t.Helper()
	payload := map[string]any{
		"appVersion": "0.9.0",
		"platform":   "windows",
		"session": map[string]any{
			"id":    "s-001",
			"title": sampleTranscript,
			"segments": []any{
				map[string]any{"startMs": 0, "endMs": 1200, "text": sampleTranscript},
			},
		},
		"speaker": map[string]any{
			// 声纹向量：16 维 float
			"embedding": []any{0.123456, -0.987654, 0.5, 0.25, 0.125, 0.0625, 1.5, 2.5, 3.5, 4.5, 5.5, 6.5, 7.5, 8.5, 9.5, 10.5},
		},
		"online": map[string]any{
			"endpoint": "https://api.example.com/v1",
			"apiKey":   sampleSecret,
			"proxy":    map[string]any{"password": samplePassword},
		},
		"audio": map[string]any{"path": "sessions/s-001/audio.wav", "pcm": "UklGRiQAAABXQVZF"},
		"errors": map[string]any{
			"ERR_ENGINE_ONLINE_AUTH": 3,
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("构造样本失败: %v", err)
	}
	return raw
}

// TestRedactRemovesTextVectorAndSecret 默认脱敏器的三项核心断言。
func TestRedactRemovesTextVectorAndSecret(t *testing.T) {
	red := NewRedactor()
	out := red.Redact(sampleDiagnosticsJSON(t))
	text := string(out)

	for _, banned := range []string{sampleTranscript, sampleSecret, samplePassword, "UklGRiQAAABXQVZF", "audio.wav"} {
		if strings.Contains(text, banned) {
			t.Fatalf("产物泄漏了 %q\n输出: %s", banned, truncate(text, 400))
		}
	}
	// 声纹向量：数值不得成串出现。
	if strings.Contains(text, "0.123456") || strings.Contains(text, "-0.987654") {
		t.Fatalf("声纹向量数值泄漏: %s", truncate(text, 400))
	}
	if !strings.Contains(text, "REDACTED") {
		t.Fatalf("应留下脱敏标记: %s", truncate(text, 400))
	}

	// 元数据必须保留。
	for _, keep := range []string{"appVersion", "0.9.0", "windows", "ERR_ENGINE_ONLINE_AUTH", "s-001"} {
		if !strings.Contains(text, keep) {
			t.Fatalf("元数据 %q 被误删: %s", keep, truncate(text, 400))
		}
	}
	if issues := Residuals(out); len(issues) != 0 {
		t.Fatalf("产物仍不干净: %v", issues)
	}
	if n, _ := red.Stats(); n == 0 {
		t.Fatal("应统计到脱敏命中数")
	}
}

// TestRedactTextLogLines 日志副本：Bearer / token= / 中文正文行都被处理。
func TestRedactTextLogLines(t *testing.T) {
	input := strings.Join([]string{
		`time=2026-01-01 level=INFO msg="engine ready" component=asr model=paraformer`,
		`time=2026-01-01 level=DEBUG msg="request" Authorization: Bearer abcdef1234567890xyz`,
		`time=2026-01-01 level=DEBUG msg="url" https://api.example.com/v1?token=` + sampleSecret,
		`time=2026-01-01 level=INFO msg="转写结果：` + sampleTranscript + `"`,
	}, "\n")

	red := NewRedactor()
	out := string(red.Redact([]byte(input)))

	if strings.Contains(out, "abcdef1234567890xyz") {
		t.Fatalf("Bearer 凭据泄漏: %s", truncate(out, 300))
	}
	if strings.Contains(out, sampleSecret) {
		t.Fatalf("token 泄漏: %s", truncate(out, 300))
	}
	if strings.Contains(out, sampleTranscript) {
		t.Fatalf("正文行未剔除: %s", truncate(out, 300))
	}
	if !strings.Contains(out, "engine ready") {
		t.Fatalf("无害日志行被误删: %s", truncate(out, 300))
	}
	if _, dropped := red.Stats(); dropped == 0 {
		t.Fatal("应统计到被丢弃的行数")
	}
	if issues := Residuals([]byte(out)); len(issues) != 0 {
		t.Fatalf("日志副本仍不干净: %v", issues)
	}
}

// TestRedactMultilineStringValue 含换行的字符串值整体剔除。
func TestRedactMultilineStringValue(t *testing.T) {
	payload := []byte(`{"note":"line1\nline2\nline3","keep":"ok"}`)
	out := string(NewRedactor().Redact(payload))
	if strings.Contains(out, "line2") {
		t.Fatalf("多行值未剔除: %s", out)
	}
	if !strings.Contains(out, `"keep":"ok"`) {
		t.Fatalf("无害字段被误删: %s", out)
	}
}

// TestRedactNonUTF8 非 UTF-8 字节不 panic，且不留原始字节。
func TestRedactNonUTF8(t *testing.T) {
	payload := append([]byte("log line "), 0xff, 0xfe, 0x00, 0x01, 'x', 'y')
	out := NewRedactor().Redact(payload)
	if !utf8.Valid(out) {
		t.Fatalf("输出必须是合法 UTF-8: %q", out)
	}
	if strings.ContainsRune(string(out), 0) {
		t.Fatalf("输出不应含 NUL: %q", out)
	}
}

// TestRedactEmptyAndLongInput 空输入 / 超长字符串的边界。
func TestRedactEmptyAndLongInput(t *testing.T) {
	red := NewRedactor()
	if out := red.Redact(nil); len(out) != 0 {
		t.Fatalf("空输入应返回空: %q", out)
	}
	long := strings.Repeat("a-very-long-ascii-token-", 200)
	out := red.Redact([]byte(`{"msg":"` + long + `"}`))
	if len(out) > len(long) {
		t.Fatalf("超长字符串未被截断: %d", len(out))
	}
}

// TestBuildBundleKeepsMetadataAndHidesSecrets 诊断包：元数据在，禁区内容不在。
func TestBuildBundleKeepsMetadataAndHidesSecrets(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	in := BundleInput{
		AppVersion:    "0.9.0",
		Build:         "dev",
		Platform:      "windows",
		Arch:          "amd64",
		GoVersion:     "go1.26",
		Offline:       true,
		OfflineSource: string(SourceEnv),
		OfflineEnvVar: EnvOffline,
		Modules:       []string{"capture", "asr", "privacy"},
		ErrorCounts:   map[string]int{"ERR_OFFLINE_ENFORCED": 2},
		Outbound: []contract.OutboundRecord{
			{At: now.UnixMilli(), Host: "api.example.com/v1/audio", Kind: "online-asr", Audio: true, Bytes: 2048, Result: ResultOK},
		},
		// 故意塞入禁区内容：正文 + 密钥 + 声纹（模拟调用方失误）。
		LogLines: []string{
			`level=INFO msg="engine ready" component=asr`,
			`level=DEBUG msg="转写结果：` + sampleTranscript + `"`,
			`level=DEBUG msg="apiKey=` + sampleSecret + `"`,
			`level=DEBUG msg="embedding=[0.1,0.2,0.3,0.4,0.5,0.6,0.7,0.8,0.9,1.0]"`,
		},
		Now: now,
	}

	art, err := BuildBundle(in)
	if err != nil {
		t.Fatalf("生成诊断包失败: %v", err)
	}
	if art.MIME != "application/json" {
		t.Fatalf("MIME 期望 application/json，实际 %q", art.MIME)
	}
	if !strings.HasPrefix(art.Filename, "mandaryn-diagnostics-") || !strings.HasSuffix(art.Filename, ".json") {
		t.Fatalf("文件名不合预期: %q", art.Filename)
	}

	text := string(art.Bytes)
	for _, banned := range []string{sampleTranscript, sampleSecret, "0.123456"} {
		if strings.Contains(text, banned) {
			t.Fatalf("诊断包泄漏 %q: %s", banned, truncate(text, 400))
		}
	}
	if issues := Residuals(art.Bytes); len(issues) != 0 {
		t.Fatalf("诊断包仍不干净: %v", issues)
	}

	var doc map[string]any
	if err := json.Unmarshal(art.Bytes, &doc); err != nil {
		t.Fatalf("诊断包不是合法 JSON: %v", err)
	}
	app, _ := doc["app"].(map[string]any)
	if app["version"] != "0.9.0" || app["platform"] != "windows" {
		t.Fatalf("版本 / 平台元数据缺失: %v", app)
	}
	privacyDoc, _ := doc["privacy"].(map[string]any)
	if privacyDoc["offline"] != true || privacyDoc["offlineSource"] != "env" {
		t.Fatalf("离线状态元数据缺失: %v", privacyDoc)
	}
	mods, _ := doc["modules"].([]any)
	if len(mods) != 3 {
		t.Fatalf("模块清单缺失: %v", mods)
	}
	errs, _ := doc["errors"].([]any)
	if len(errs) != 1 {
		t.Fatalf("错误码计数缺失: %v", errs)
	}
	if art.Redactions == 0 {
		t.Fatal("应统计到脱敏命中数（日志副本里的正文 / 密钥）")
	}
}

// TestBuildBundleDropsAllLogsWhenUnsafe 极端情况下丢弃全部日志副本仍能产出包。
func TestBuildBundleDropsAllLogsWhenUnsafe(t *testing.T) {
	in := BundleInput{
		AppVersion: "1.0.0",
		Platform:   "linux",
		// 纯正文日志（无任何 ASCII 元数据），必须整包仍然安全。
		LogLines: []string{sampleTranscript, sampleTranscript},
		Now:      time.Unix(0, 0),
	}
	art, err := BuildBundle(in)
	if err != nil {
		t.Fatalf("生成诊断包失败: %v", err)
	}
	if strings.Contains(string(art.Bytes), sampleTranscript) {
		t.Fatal("诊断包泄漏正文")
	}
	if !strings.Contains(string(art.Bytes), "1.0.0") {
		t.Fatal("版本元数据应保留")
	}
}

// TestResidualsDetectsProblems Residuals 必须能抓出残留。
func TestResidualsDetectsProblems(t *testing.T) {
	if issues := Residuals([]byte(sampleTranscript)); len(issues) == 0 {
		t.Fatal("应检出中文正文")
	}
	if issues := Residuals([]byte("apiKey=" + sampleSecret)); len(issues) == 0 {
		t.Fatal("应检出密钥")
	}
	if issues := Residuals([]byte("Authorization: Bearer abcdef1234567890")); len(issues) == 0 {
		t.Fatal("应检出 Bearer 凭据")
	}
	if issues := Residuals([]byte(`{"version":"1.0.0","platform":"windows"}`)); len(issues) != 0 {
		t.Fatalf("干净内容不应误报: %v", issues)
	}
}

// TestReadLogTail 读日志尾部：超限时只取尾部且丢弃被切断的首行。
func TestReadLogTail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	var b strings.Builder
	for i := 0; i < 100; i++ {
		b.WriteString(`{"level":"INFO","msg":"line-` + itoa(i) + `"}` + "\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("写日志失败: %v", err)
	}

	lines, err := ReadLogTail(path, 500)
	if err != nil {
		t.Fatalf("读日志失败: %v", err)
	}
	if len(lines) == 0 {
		t.Fatal("应读到尾部内容")
	}
	if !strings.Contains(strings.Join(lines, ""), "line-99") {
		t.Fatal("应包含最后一行")
	}
	if strings.Contains(lines[0], "line-0") {
		t.Fatalf("应只取尾部: %q", lines[0])
	}
	if _, err := ReadLogTail(filepath.Join(dir, "missing.log"), 0); err == nil {
		t.Fatal("文件不存在应返回错误")
	}
}

// TestRedactorSensitiveFields 敏感键名的覆盖度（正文 / 音频 / 声纹 / 凭据）。
func TestRedactorSensitiveFields(t *testing.T) {
	for _, key := range []string{"text", "transcript", "audio", "pcm", "embedding", "voiceprint", "apiKey", "api_key", "secret", "access_token", "password", "Authorization", "cookie"} {
		if !sensitiveField(key) {
			t.Fatalf("敏感键 %q 未被识别", key)
		}
	}
	for _, key := range []string{"version", "platform", "startMs", "errCode", "count"} {
		if sensitiveField(key) {
			t.Fatalf("元数据键 %q 被误判为敏感", key)
		}
	}
}
