package privacy

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/utils/logx"
)

func envOf(v string) func(string) string {
	return func(key string) string {
		if key == EnvLogLevel {
			return v
		}
		return ""
	}
}

// TestDebugModeEnvResolution：环境变量优先于设置页；非法值忽略。
func TestDebugModeEnvResolution(t *testing.T) {
	cases := []struct {
		env        string
		user       bool
		wantOn     bool
		wantSource string
		wantBad    string
	}{
		{env: "", user: false, wantOn: false, wantSource: contract.DebugModeSourceUser},
		{env: "", user: true, wantOn: true, wantSource: contract.DebugModeSourceUser},
		{env: "debug", user: false, wantOn: true, wantSource: contract.DebugModeSourceEnv},
		{env: " DEBUG ", user: false, wantOn: true, wantSource: contract.DebugModeSourceEnv},
		{env: "info", user: true, wantOn: false, wantSource: contract.DebugModeSourceEnv},
		{env: "off", user: true, wantOn: false, wantSource: contract.DebugModeSourceEnv},
		{env: "verbose", user: true, wantOn: true, wantSource: contract.DebugModeSourceUser, wantBad: "verbose"},
	}
	for _, c := range cases {
		var applied []bool
		d := NewDebugMode(envOf(c.env)).WithApply(func(on bool) error {
			applied = append(applied, on)
			return nil
		})
		st, err := d.SetUser(c.user)
		if err != nil {
			t.Fatalf("env=%q: %v", c.env, err)
		}
		if st.Enabled != c.wantOn || st.Source != c.wantSource || st.UserEnabled != c.user {
			t.Fatalf("env=%q user=%v: 状态 %+v", c.env, c.user, st)
		}
		if c.wantSource == contract.DebugModeSourceEnv && st.EnvVar != EnvLogLevel {
			t.Fatalf("接管时应给出环境变量名：%+v", st)
		}
		if got := d.InvalidEnv(); got != c.wantBad {
			t.Fatalf("env=%q InvalidEnv=%q want %q", c.env, got, c.wantBad)
		}
		if len(applied) != 1 || applied[0] != c.wantOn {
			t.Fatalf("env=%q 落实的开关 %v want %v", c.env, applied, c.wantOn)
		}
	}
}

// TestDebugModeApplyFailureFallsBack：落实失败（如脱敏器未注入）时生效值回落为关。
func TestDebugModeApplyFailureFallsBack(t *testing.T) {
	d := NewDebugMode(envOf("")).WithApply(func(on bool) error {
		if on {
			return errors.New("no sanitizer")
		}
		return nil
	})
	st, err := d.SetUser(true)
	if err == nil || st.Enabled || !st.UserEnabled {
		t.Fatalf("失败后应为 enabled=false、userEnabled=true 并返回错误：%+v err=%v", st, err)
	}
}

func TestSetUserLevel(t *testing.T) {
	d := NewDebugMode(envOf("")).WithApply(nil)
	if st, _ := d.SetUserLevel("debug"); !st.Enabled {
		t.Fatal("log_level=debug 应开启")
	}
	for _, lv := range []string{"info", "off", ""} {
		if st, _ := d.SetUserLevel(lv); st.Enabled {
			t.Fatalf("log_level=%q 应关闭", lv)
		}
	}
	if LogLevelFor(true) != "debug" || LogLevelFor(false) != "info" {
		t.Fatal("LogLevelFor 映射不对")
	}
}

// TestSanitizeDebugAttr：脱敏规则表。
func TestSanitizeDebugAttr(t *testing.T) {
	str := func(a slog.Attr) string { return a.Value.Resolve().String() }
	cases := []struct {
		name string
		in   slog.Attr
		want func(string) bool
	}{
		{"敏感键整值替换", slog.String("apiKey", "abc"), func(s string) bool { return s == masked }},
		{"正文键整值替换", slog.String("text", "随便什么"), func(s string) bool { return s == masked }},
		{"连续中文视为正文", slog.String("title", "今天的会议纪要"), func(s string) bool { return strings.HasPrefix(s, "[REDACTED:text:") }},
		{"三个以内中文保留", slog.String("device", "扬声器 (HECATE)"), func(s string) bool { return s == "扬声器 (HECATE)" }},
		{"换行视为多行载荷", slog.String("note", "a\nb"), func(s string) bool { return strings.HasPrefix(s, "[REDACTED:text:") }},
		{"密钥模式", slog.String("detail", "used sk-abcdefghijklmnop"), func(s string) bool { return !strings.Contains(s, "sk-abcdefghijklmnop") }},
		{"Windows 主目录", slog.String("path", `C:\Users\mayn\AppData\x.db`), func(s string) bool { return s == `C:\Users\~\AppData\x.db` }},
		{"Unix 主目录", slog.String("path", "/home/alice/.mandaryn"), func(s string) bool { return s == "/home/~/.mandaryn" }},
		{"err 保留中文说明", slog.String("err", "模型包未安装，请先下载"), func(s string) bool { return s == "模型包未安装，请先下载" }},
		{"error 值脱敏路径", slog.Any("err", errors.New(`open C:\Users\bob\a.wav: denied`)), func(s string) bool { return s == `open C:\Users\~\a.wav: denied` }},
		{"向量", slog.Any("v", []float32{1, 2, 3, 4, 5, 6, 7, 8}), func(s string) bool { return s == "[REDACTED:vector:8]" }},
		{"短数值数组保留", slog.Any("v", []int{1, 2}), func(s string) bool { return s == "[1 2]" }},
		{"字节切片", slog.Any("raw", []byte("RIFF....")), func(s string) bool { return strings.HasPrefix(s, "[REDACTED:binary:") }},
		{"数值原样", slog.Int("threads", 4), func(s string) bool { return s == "4" }},
		{"长字符串截断", slog.String("id", strings.Repeat("ab ", 100)), func(s string) bool { return len(s) < 200 && strings.HasSuffix(s, "…") }},
		{"前端堆栈保留多行并折叠", slog.String("stack", "TypeError: x\n  at f (/src/a.ts:1:2)"), func(s string) bool { return s == "TypeError: x   at f (/src/a.ts:1:2)" }},
		{"前端消息含正文整段剔除", slog.String("message", "加载失败：今天的会议纪要"), func(s string) bool { return strings.HasPrefix(s, "[REDACTED:text:") }},
	}
	for _, c := range cases {
		got := str(SanitizeDebugAttr(nil, c.in))
		if !c.want(got) {
			t.Errorf("%s：得到 %q", c.name, got)
		}
	}
}

// TestSanitizeDebugMessage：msg 保留中文，只脱敏密钥与路径；内置键原样。
func TestSanitizeDebugMessage(t *testing.T) {
	got := SanitizeDebugAttr(nil, slog.String(slog.MessageKey, "引擎预热失败，已回滚 token=abc123456 at C:\\Users\\mayn\\x"))
	s := got.Value.String()
	if !strings.Contains(s, "引擎预热失败，已回滚") || strings.Contains(s, "abc123456") || strings.Contains(s, "mayn") {
		t.Fatalf("msg 脱敏不对：%q", s)
	}
	lv := slog.Any(slog.LevelKey, slog.LevelDebug)
	if SanitizeDebugAttr(nil, lv).Value.Any() != slog.LevelDebug {
		t.Fatal("level 应原样")
	}
	// 组内的 msg 键不是内置 msg，按普通字段处理。
	if v := SanitizeDebugAttr([]string{"g"}, slog.String(slog.MessageKey, "今天的会议纪要")).Value.String(); !strings.HasPrefix(v, "[REDACTED:text:") {
		t.Fatalf("组内字段应按普通字符串脱敏：%q", v)
	}
}

// TestSanitizeDebugStruct：复合值走结构化脱敏（敏感键整值替换）。
func TestSanitizeDebugStruct(t *testing.T) {
	type payload struct {
		Host     string `json:"host"`
		Password string `json:"password"`
		Path     string `json:"path"`
	}
	v := SanitizeDebugAttr(nil, slog.Any("cfg", payload{Host: "example.com", Password: "p@ss", Path: `C:\Users\mayn\a`}))
	s := fmt.Sprint(v.Value.Any())
	if strings.Contains(s, "p@ss") || strings.Contains(s, "mayn") || !strings.Contains(s, "example.com") {
		t.Fatalf("结构化脱敏不对：%s", s)
	}
}

func writeLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestReadDebugLogOrderAndFilter：跨滚动文件新的在前，按级别 / 来源 / 模块过滤，坏行跳过。
func TestReadDebugLogOrderAndFilter(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, logx.DebugFileName)
	writeLines(t, base+".1",
		`{"time":"t1","level":"DEBUG","msg":"oldest","component":"asr","origin":"backend","threads":4}`,
		`{"time":"t2","level":"WARN","msg":"older","component":"capture","origin":"backend"}`,
	)
	writeLines(t, base,
		`not json`,
		`{"time":"t3","level":"ERROR","msg":"boom","component":"frontend","origin":"frontend","route":"library"}`,
		`{"time":"t4","level":"INFO","msg":"newest","component":"asr","origin":"backend"}`,
	)

	page, err := ReadDebugLog(dir, contract.DebugLogQuery{})
	if err != nil {
		t.Fatal(err)
	}
	var msgs []string
	for _, e := range page.Entries {
		msgs = append(msgs, e.Msg)
	}
	if strings.Join(msgs, ",") != "newest,boom,older,oldest" {
		t.Fatalf("顺序应为新的在前，实际 %v", msgs)
	}
	if page.Malformed != 1 || page.Scanned != 5 {
		t.Fatalf("scanned=%d malformed=%d", page.Scanned, page.Malformed)
	}
	if strings.Join(page.Components, ",") != "asr,capture,frontend" {
		t.Fatalf("模块列表 %v", page.Components)
	}
	if page.Entries[3].Fields != `{"threads":4}` || page.Entries[0].Fields != "{}" {
		t.Fatalf("上下文字段不对：%q / %q", page.Entries[3].Fields, page.Entries[0].Fields)
	}

	warnUp, _ := ReadDebugLog(dir, contract.DebugLogQuery{MinLevel: "warn"})
	if len(warnUp.Entries) != 2 {
		t.Fatalf("WARN 及以上应有 2 条，实际 %d", len(warnUp.Entries))
	}
	fe, _ := ReadDebugLog(dir, contract.DebugLogQuery{Origin: "frontend"})
	if len(fe.Entries) != 1 || fe.Entries[0].Component != "frontend" {
		t.Fatalf("前端过滤不对：%+v", fe.Entries)
	}
	asr, _ := ReadDebugLog(dir, contract.DebugLogQuery{Component: "asr", Limit: 1})
	if len(asr.Entries) != 1 || asr.Entries[0].Msg != "newest" || !asr.Truncated {
		t.Fatalf("模块过滤 + 条数上限不对：%+v truncated=%v", asr.Entries, asr.Truncated)
	}
}

func TestReadDebugLogValidationAndMissing(t *testing.T) {
	if _, err := ReadDebugLog(t.TempDir(), contract.DebugLogQuery{MinLevel: "TRACE"}); !apierr.HasCode(err, apierr.CodeInvalidArgument) {
		t.Fatalf("非法级别应报 ERR_INVALID_ARGUMENT：%v", err)
	}
	if _, err := ReadDebugLog(t.TempDir(), contract.DebugLogQuery{Origin: "x"}); !apierr.HasCode(err, apierr.CodeInvalidArgument) {
		t.Fatalf("非法来源应报 ERR_INVALID_ARGUMENT：%v", err)
	}
	page, err := ReadDebugLog(t.TempDir(), contract.DebugLogQuery{})
	if err != nil || len(page.Entries) != 0 {
		t.Fatalf("没有文件时应返回空页：%+v %v", page, err)
	}
}

// TestBundleIncludesDebugLogs：诊断包带上调试日志副本，且同样通过脱敏自检。
func TestBundleIncludesDebugLogs(t *testing.T) {
	art, err := BuildBundle(BundleInput{
		AppVersion:    "0.1",
		DebugLogLines: []string{`{"level":"DEBUG","msg":"tick","component":"perf","token":"sk-abcdefghijklmnopq"}`},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(art.Bytes)
	if !strings.Contains(s, `"debugLogs"`) || !strings.Contains(s, "tick") || strings.Contains(s, "sk-abcdefghijklmnopq") {
		t.Fatalf("诊断包调试日志不对：%s", s)
	}
}
