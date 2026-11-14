package privacy

// 出站留痕与 URL 脱敏测试。重点断言：
//   - 成功与失败都留痕；
//   - Authorization 头与 ?token= 值不出现在 outbound_log 的任何列；
//   - 先留痕后执行：panic 也留痕。

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/storage"
	"vip.oberon.mandaryn/internal/utils/httpx"
)

// TestSanitizeURL 覆盖 query / userinfo / 路径 / fragment 的脱敏。
func TestSanitizeURL(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		want  []string // 必须出现
		avoid []string // 必须不出现
	}{
		{
			name:  "query token redacted",
			in:    "https://api.example.com/v1/audio/transcriptions?token=sk-abcdef123456&lang=zh",
			want:  []string{"api.example.com", "/v1/audio/transcriptions", "lang=zh"},
			avoid: []string{"sk-abcdef123456"},
		},
		{
			name:  "api key param redacted",
			in:    "https://api.example.com/v1/models?api_key=SECRET-VALUE-9876",
			want:  []string{"api.example.com"},
			avoid: []string{"SECRET-VALUE-9876"},
		},
		{
			name:  "userinfo stripped",
			in:    "https://user:password123@api.example.com/v1/x",
			want:  []string{"api.example.com"},
			avoid: []string{"password123", "user:"},
		},
		{
			name:  "fragment dropped",
			in:    "https://api.example.com/v1/x#access_token=xyz987654",
			want:  []string{"api.example.com"},
			avoid: []string{"xyz987654"},
		},
		{
			name:  "secret-like path segment masked",
			in:    "https://cdn.example.com/models/abcdef1234567890abcdef/file.onnx",
			want:  []string{"cdn.example.com", "file.onnx"},
			avoid: []string{"abcdef1234567890abcdef"},
		},
		{
			name:  "plain path kept",
			in:    "https://cdn.example.com/models/v1/encoder.onnx",
			want:  []string{"cdn.example.com/models/v1/encoder.onnx"},
			avoid: nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := SanitizeURL(c.in)
			for _, w := range c.want {
				if !strings.Contains(got, w) {
					t.Fatalf("脱敏结果缺少 %q: %q", w, got)
				}
			}
			for _, a := range c.avoid {
				if strings.Contains(got, a) {
					t.Fatalf("脱敏结果仍含 %q: %q", a, got)
				}
			}
		})
	}
}

// TestSanitizeURLOversized URL 超长必须截断到上限内。
func TestSanitizeURLOversized(t *testing.T) {
	in := "https://api.example.com/v1/x?" + strings.Repeat("a=1&", 2000)
	got := SanitizeURL(in)
	if len(got) > maxURLBytes {
		t.Fatalf("URL 未截断: %d > %d", len(got), maxURLBytes)
	}
	if !strings.Contains(got, "api.example.com") {
		t.Fatalf("截断后应保留 host: %q", truncate(got, 80))
	}
}

// TestSanitizeURLInvalid 无法解析的串也必须安全（不返回原始凭据）。
func TestSanitizeURLInvalid(t *testing.T) {
	in := "not-a-url sk-abcdef123456 token=abc\nsecond-line"
	got := SanitizeURL(in)
	if strings.Contains(got, "sk-abcdef123456") || strings.Contains(got, "abc\nsecond") {
		t.Fatalf("非法 URL 未脱敏: %q", got)
	}
	if strings.Contains(got, "\n") {
		t.Fatalf("成果不应含换行（防日志注入）: %q", got)
	}
}

// TestHostOfAndLabel 域名提取与安全标签。
func TestHostOfAndLabel(t *testing.T) {
	raw := "https://user:pw@api.example.com:8443/v1/audio/transcriptions?token=abc123456789"
	if got := HostOf(raw); got != "api.example.com:8443" {
		t.Fatalf("HostOf 期望 api.example.com:8443，实际 %q", got)
	}
	label := HostLabel(raw)
	if !strings.HasPrefix(label, "api.example.com:8443/v1/audio") {
		t.Fatalf("HostLabel 期望含域名与前两段路径，实际 %q", label)
	}
	if strings.Contains(label, "token") || strings.Contains(label, "pw") {
		t.Fatalf("HostLabel 泄漏凭据: %q", label)
	}
	if got := HostOf(":::"); got != "" {
		t.Fatalf("非法输入应返回空 host，实际 %q", got)
	}
}

// TestKindColumn 归一化与列宽截断。
func TestKindColumn(t *testing.T) {
	cases := map[string]string{
		"online-asr":                     "online-asr",
		"Model Download":                 "model-download",
		"  model-download ":              "model-download",
		"":                               "other",
		"a-very-long-business-kind-name": "a-very-long-busi", // 16 字节截断
	}
	for in, want := range cases {
		if got := kindColumn(in); got != want {
			t.Fatalf("kindColumn(%q) 期望 %q，实际 %q", in, want, got)
		}
		if len(kindColumn(in)) > maxKindCol {
			t.Fatalf("kind 超过列宽: %q", kindColumn(in))
		}
	}
}

// TestDoRecordedRecordsSuccessAndFailure 成功与失败都必须留痕。
func TestDoRecordedRecordsSuccessAndFailure(t *testing.T) {
	var mode atomic.Int32 // 0 = 200, 1 = 500
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if mode.Load() == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	rec := NewRecorder(newHolder(t))
	pipe := NewPipe(NewGate(GateOptions{Env: boolPtr(false)}), rec)
	ctx := context.Background()

	// 成功
	err := pipe.DoRecorded(ctx, Call{Kind: "model-download", Target: srv.URL + "/pkg/a.onnx"},
		func(ctx context.Context) (Outcome, error) {
			return doGet(ctx, srv.URL+"/pkg/a.onnx")
		})
	if err != nil {
		t.Fatalf("成功请求不应报错: %v", err)
	}

	// 失败（5xx）
	mode.Store(1)
	err = pipe.DoRecorded(ctx, Call{Kind: "model-download", Target: srv.URL + "/pkg/b.onnx"},
		func(ctx context.Context) (Outcome, error) {
			out, derr := doGet(ctx, srv.URL+"/pkg/b.onnx")
			if derr == nil && out.Status >= 400 {
				return out, apierr.New(apierr.CodeDownloadNetwork)
			}
			return out, derr
		})
	if !apierr.HasCode(err, apierr.CodeDownloadNetwork) {
		t.Fatalf("期望 ERR_DOWNLOAD_NETWORK，实际 %v", err)
	}

	rows, total, lerr := rec.List(ListOptions{Limit: 10})
	if lerr != nil {
		t.Fatalf("读取出站记录失败: %v", lerr)
	}
	if total != 2 {
		t.Fatalf("期望 2 条记录（成功 + 失败），实际 %d", total)
	}
	// 新的在前：先失败后成功
	if rows[0].Result != apierr.CodeDownloadNetwork {
		t.Fatalf("失败记录结果错误: %+v", rows[0])
	}
	if rows[1].Result != ResultOK {
		t.Fatalf("成功记录结果错误: %+v", rows[1])
	}
	for _, row := range rows {
		if row.Pending {
			t.Fatalf("记录不应停留在 pending: %+v", row)
		}
	}
}

// TestOutboundLogHasNoSecretsInAnyColumn 逐列断言：凭据不出现在 outbound_log 的任何列。
func TestOutboundLogHasNoSecretsInAnyColumn(t *testing.T) {
	const (
		secretQuery = "sk-live-abcdef123456789"
		secretHdr   = "Bearer json-web-token-XYZ-123456"
	)
	var gotAuth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	holder := newHolder(t)
	rec := NewRecorder(holder)
	pipe := NewPipe(NewGate(GateOptions{Env: boolPtr(false)}), rec)

	target := srv.URL + "/v1/audio/transcriptions?token=" + secretQuery + "&lang=zh"
	err := pipe.DoRecorded(context.Background(), Call{Kind: "online-asr", Target: target, Audio: true},
		func(ctx context.Context) (Outcome, error) {
			req, cancel, rerr := httpx.NewRequest(ctx, http.MethodPost, target, strings.NewReader(`{}`))
			if rerr != nil {
				return Outcome{}, rerr
			}
			defer cancel()
			req.Header.Set("Authorization", secretHdr)
			resp, derr := httpx.Do(req)
			if resp != nil {
				_ = resp.Body.Close()
			}
			if derr != nil {
				return Outcome{}, derr
			}
			return Outcome{Status: resp.StatusCode, BytesIn: 2}, nil
		})
	if err != nil {
		t.Fatalf("请求不应失败: %v", err)
	}
	if got, _ := gotAuth.Load().(string); got != secretHdr {
		t.Fatalf("断言前提不成立：服务端没收到 Authorization 头，实际 %q", got)
	}

	// 直接读原始行，逐列检查（不经 DTO，避免漏掉某列）。
	var raw []storage.OutboundLog
	if err := holder.Current().GORM.Find(&raw).Error; err != nil {
		t.Fatalf("读取原始记录失败: %v", err)
	}
	if len(raw) != 1 {
		t.Fatalf("期望 1 行，实际 %d", len(raw))
	}
	row := raw[0]
	columns := map[string]string{
		"host":   row.Host,
		"kind":   row.Kind,
		"audio":  boolColumn(row.Audio),
		"bytes":  itoa(int(row.Bytes)),
		"result": row.Result,
	}
	for name, value := range columns {
		for _, secret := range []string{secretQuery, secretHdr, "json-web-token", "Bearer"} {
			if strings.Contains(value, secret) {
				t.Fatalf("列 %s 泄漏凭据 %q: %q", name, secret, value)
			}
		}
	}
	if !strings.Contains(row.Host, "127.0.0.1") {
		t.Fatalf("host 列应保留目标域名，实际 %q", row.Host)
	}
	if !strings.Contains(row.Host, "lang=zh") {
		// query 的安全短值保留，便于识别是哪个接口；这里 host 列只到路径前缀，不带 query。
		t.Logf("host 列：%q（只保留域名 + 路径前缀，符合预期）", row.Host)
	}
	if strings.Contains(row.Host, "token") {
		t.Fatalf("host 列不应出现 query 参数名: %q", row.Host)
	}
}

// TestDoRecordedPanicStillRecords panic 也必须留痕并原样抛出。
func TestDoRecordedPanicStillRecords(t *testing.T) {
	rec := NewRecorder(newHolder(t))
	pipe := NewPipe(NewGate(GateOptions{Env: boolPtr(false)}), rec)

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("panic 应原样抛出")
			}
		}()
		_ = pipe.DoRecorded(context.Background(), Call{Kind: "model-download", Target: "https://cdn.example.com/a.onnx"},
			func(context.Context) (Outcome, error) {
				panic("boom")
			})
	}()

	rows, total, err := rec.List(ListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("读取出站记录失败: %v", err)
	}
	if total != 1 {
		t.Fatalf("panic 也应留痕，实际 %d 条", total)
	}
	if rows[0].Result != ResultError {
		t.Fatalf("panic 记录结果期望 error，实际 %q", rows[0].Result)
	}
}

// TestRecorderPendingThenFinish pending → finish 覆盖同一行。
func TestRecorderPendingThenFinish(t *testing.T) {
	rec := NewRecorder(newHolder(t))
	id := rec.Record(Record{Kind: "online-asr", Target: "https://api.example.com/v1/x", Result: ResultPending})
	if id == 0 {
		t.Fatal("预登记应返回行 id")
	}
	rows, _, err := rec.List(ListOptions{})
	if err != nil {
		t.Fatalf("%v", err)
	}
	if !rows[0].Pending {
		t.Fatalf("预登记行应为 pending: %+v", rows[0])
	}
	rec.Finish(id, Record{Kind: "online-asr", Target: "https://api.example.com/v1/x", Result: ResultOK, BytesOut: 42})
	rows, total, err := rec.List(ListOptions{})
	if err != nil {
		t.Fatalf("%v", err)
	}
	if total != 1 {
		t.Fatalf("Finish 应覆盖同一行，实际 %d 行", total)
	}
	if rows[0].Result != ResultOK || rows[0].Bytes != 42 || rows[0].Pending {
		t.Fatalf("Finish 未正确覆盖: %+v", rows[0])
	}
}

// TestDoRecordedNilFunc 空 fn 直接报错且不留痕（没有出站意图）。
func TestDoRecordedNilFunc(t *testing.T) {
	rec := NewRecorder(newHolder(t))
	pipe := NewPipe(NewGate(GateOptions{Env: boolPtr(false)}), rec)
	if err := pipe.DoRecorded(context.Background(), Call{Kind: "model-download", Target: "https://x.example.com/a"}, nil); !errors.Is(err, ErrNilFunc) {
		t.Fatalf("期望 ErrNilFunc，实际 %v", err)
	}
	if _, total, _ := rec.List(ListOptions{}); total != 0 {
		t.Fatalf("空 fn 不应留痕，实际 %d 条", total)
	}
}

// TestRecorderNilHolder 无库时退化：不 panic、返回 0、查询为空。
func TestRecorderNilHolder(t *testing.T) {
	rec := NewRecorder(nil)
	if id := rec.Record(Record{Kind: "model-download", Target: "https://x.example.com/a"}); id != 0 {
		t.Fatalf("无库时应返回 0，实际 %d", id)
	}
	rec.Finish(1, Record{Kind: "model-download"})
	rec.Blocked("model-download", "", "https://x.example.com/a", false)
	rows, total, err := rec.List(ListOptions{})
	if err != nil || total != 0 || len(rows) != 0 {
		t.Fatalf("无库时应返回空: rows=%v total=%d err=%v", rows, total, err)
	}
	if _, err := rec.Stats(time.Now().Add(-time.Hour), time.Now()); err != nil {
		t.Fatalf("无库时 Stats 不应报错: %v", err)
	}
	if n, err := rec.Clear(); err != nil || n != 0 {
		t.Fatalf("无库时 Clear 应返回 0: n=%d err=%v", n, err)
	}
	if err := rec.Purge(time.Now()); err != nil {
		t.Fatalf("无库时 Purge 不应报错: %v", err)
	}
}

// TestRecordNonUTF8Target 非 UTF-8 target 不 panic 且落库值合法。
func TestRecordNonUTF8Target(t *testing.T) {
	rec := NewRecorder(newHolder(t))
	raw := "https://api.example.com/v1/\xff\xfe?token=\xff\xfe"
	rec.Record(Record{Kind: "online-asr", Target: raw, Result: ResultOK})
	rows, total, err := rec.List(ListOptions{})
	if err != nil {
		t.Fatalf("%v", err)
	}
	if total != 1 {
		t.Fatalf("期望 1 行，实际 %d", total)
	}
	// 非法字节被替换成 U+FFFD（合法 UTF-8），不得原样落库。
	if !utf8.ValidString(rows[0].Host) {
		t.Fatalf("host 必须是合法 UTF-8: %q", rows[0].Host)
	}
	if strings.ContainsRune(rows[0].Host, 0) {
		t.Fatalf("host 不应含 NUL: %q", rows[0].Host)
	}
	if strings.Contains(rows[0].Host, "token") {
		t.Fatalf("host 不应含 query: %q", rows[0].Host)
	}
}

// TestRecorderStats 统计口径：blocked / real / pending 分开计。
func TestRecorderStats(t *testing.T) {
	rec := NewRecorder(newHolder(t))
	rec.Record(Record{Kind: "model-download", Target: "https://a.example.com/x", Result: ResultOK})
	rec.Record(Record{Kind: "online-asr", Target: "https://b.example.com/y", Result: ResultPending})
	rec.Blocked("online-asr", "probe", "https://c.example.com/z", true)

	st, err := rec.Stats(time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("%v", err)
	}
	if st.Total != 3 || st.Real != 1 || st.Blocked != 1 || st.Pending != 1 {
		t.Fatalf("统计口径错误: %+v", st)
	}
	if st.LastAt == 0 {
		t.Fatal("LastAt 应有值")
	}
}

// TestDoRecordedEmptyBody 空响应体也能正常留痕（bytes 为 0 不算异常）。
func TestDoRecordedEmptyBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent) // 没有任何 body
	}))
	defer srv.Close()

	rec := NewRecorder(newHolder(t))
	pipe := NewPipe(NewGate(GateOptions{Env: boolPtr(false)}), rec)

	err := pipe.DoRecorded(context.Background(), Call{Kind: "model-download", Target: srv.URL + "/empty"},
		func(ctx context.Context) (Outcome, error) {
			req, cancel, rerr := httpx.NewRequest(ctx, http.MethodGet, srv.URL+"/empty", nil)
			if rerr != nil {
				return Outcome{}, rerr
			}
			defer cancel()
			resp, derr := httpx.Do(req)
			if resp != nil {
				_ = resp.Body.Close()
			}
			if derr != nil {
				return Outcome{}, derr
			}
			return Outcome{Status: resp.StatusCode}, nil
		})
	if err != nil {
		t.Fatalf("空 body 不应报错: %v", err)
	}

	rows, total, lerr := rec.List(ListOptions{})
	if lerr != nil {
		t.Fatalf("%v", lerr)
	}
	if total != 1 {
		t.Fatalf("应留痕 1 条，实际 %d", total)
	}
	if rows[0].Bytes != 0 || rows[0].Result != ResultOK {
		t.Fatalf("空 body 记录不正确: %+v", rows[0])
	}
}

// doGet 是测试用的最小 GET，返回状态码与响应字节数。
func doGet(ctx context.Context, url string) (Outcome, error) {
	req, cancel, err := httpx.NewRequest(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Outcome{}, err
	}
	defer cancel()
	resp, err := httpx.Do(req)
	if err != nil {
		return Outcome{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	buf := make([]byte, 256)
	n, _ := resp.Body.Read(buf)
	return Outcome{Status: resp.StatusCode, BytesOut: int64(n)}, nil
}

// boolColumn 把布尔列转成字符串，便于统一做「逐列不含凭据」断言。
func boolColumn(v bool) string {
	if v {
		return "1"
	}
	return "0"
}

// 保证 contract 在本文件被引用（DTO 断言用）。
var _ = contract.OutboundRecord{}
