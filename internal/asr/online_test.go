package asr

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
)

// newTestOnline 起一个 TLS 测试服务器并返回「信任它证书」的在线引擎。
func newTestOnline(t *testing.T, handler http.HandlerFunc, mutate func(*OnlineOptions)) (*OnlineEngine, *httptest.Server) {
	t.Helper()
	t.Setenv("MANDARYN_OFFLINE", "0") // 保证不被全局离线开关拦截

	ts := httptest.NewTLSServer(handler)
	t.Cleanup(ts.Close)

	opt := OnlineOptions{
		Endpoint:  ts.URL,
		Model:     "whisper-1",
		Language:  "zh",
		APIKey:    "sk-test-1234",
		TimeoutMs: 3000,
		Client:    ts.Client(), // 测试专用：信任自签证书
	}
	if mutate != nil {
		mutate(&opt)
	}
	eng, err := NewOnlineEngine(opt)
	if err != nil {
		t.Fatalf("构造在线引擎失败: %v", err)
	}
	return eng, ts
}

func TestNewOnlineEngine_Validation(t *testing.T) {
	cases := []struct {
		name string
		opt  OnlineOptions
		code string
	}{
		{
			name: "拒绝 http 端点",
			opt:  OnlineOptions{Endpoint: "http://api.example.com", Model: "m"},
			code: apierr.CodeInvalidArgument,
		},
		{
			name: "端点为空",
			opt:  OnlineOptions{Model: "m"},
			code: apierr.CodeInvalidArgument,
		},
		{
			name: "模型名为空",
			opt:  OnlineOptions{Endpoint: "https://api.example.com"},
			code: apierr.CodeInvalidArgument,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewOnlineEngine(tc.opt)
			if !apierr.HasCode(err, tc.code) {
				t.Fatalf("期望 %s，实际 %v", tc.code, err)
			}
		})
	}
}

func TestOnlineEngine_ID(t *testing.T) {
	eng, _ := newTestOnline(t, func(w http.ResponseWriter, r *http.Request) {}, nil)
	if got := eng.ID(); got != "online:whisper-1" {
		t.Fatalf("ID = %q, 期望 online:whisper-1", got)
	}
}

func TestOnlineEngine_FinalizeSuccess(t *testing.T) {
	var gotModel, gotLang, gotAuth string
	var gotFileLen int
	var gotWAVHeader []byte

	eng, _ := newTestOnline(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/transcriptions" {
			t.Errorf("路径 = %q", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		if err := r.ParseMultipartForm(4 << 20); err != nil {
			t.Errorf("解析 multipart 失败: %v", err)
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		gotModel = r.FormValue("model")
		gotLang = r.FormValue("language")
		file, _, err := r.FormFile("file")
		if err != nil {
			t.Errorf("没有 file 字段: %v", err)
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		defer file.Close()
		data, _ := io.ReadAll(file)
		gotFileLen = len(data)
		if len(data) >= 12 {
			gotWAVHeader = data[:12]
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":" 你好，会议开始了 "}`))
	}, nil)

	samples := []float32{0.1, -0.1, 0.5}
	res, err := eng.Finalize(AudioSegment{Samples: samples, SampleRate: 16000, Seq: 3})
	if err != nil {
		t.Fatalf("Finalize 失败: %v", err)
	}
	if res.Text != "你好，会议开始了" {
		t.Fatalf("Text = %q", res.Text)
	}
	if res.EngineID != "online:whisper-1" {
		t.Fatalf("EngineID = %q", res.EngineID)
	}
	if gotModel != "whisper-1" {
		t.Fatalf("model 字段 = %q", gotModel)
	}
	if gotLang != "zh" {
		t.Fatalf("language 字段 = %q", gotLang)
	}
	if gotAuth != "Bearer sk-test-1234" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if want := 44 + len(samples)*2; gotFileLen != want {
		t.Fatalf("上传音频长度 = %d, 期望 %d", gotFileLen, want)
	}
	if string(gotWAVHeader[:4]) != "RIFF" || string(gotWAVHeader[8:12]) != "WAVE" {
		t.Fatalf("上传内容不是 WAV: %q", gotWAVHeader)
	}
}

func TestOnlineEngine_FinalizeErrorClassification(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		body     string
		wantKind FailureKind
		wantCode string
	}{
		{"401", http.StatusUnauthorized, `{"error":"bad key"}`, FailAuth, apierr.CodeEngineOnlineAuth},
		{"403", http.StatusForbidden, `{"error":"nope"}`, FailAuth, apierr.CodeEngineOnlineAuth},
		{"429", http.StatusTooManyRequests, `{"retry_after":5}`, FailRateLimit, apierr.CodeEngineOnlineRateLimit},
		{"500", http.StatusInternalServerError, `{"error":"boom"}`, FailServer, apierr.CodeEngineOnlineServer},
		{"400", http.StatusBadRequest, `{"error":"unsupported"}`, FailRejected, apierr.CodeEngineOnlineRejected},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eng, _ := newTestOnline(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}, nil)

			_, err := eng.Finalize(AudioSegment{Samples: []float32{0.1}, SampleRate: 16000})
			if err == nil {
				t.Fatal("期望返回错误")
			}
			ee, ok := AsEngineError(err)
			if !ok {
				t.Fatalf("期望 EngineError，实际 %T", err)
			}
			if ee.Kind != tc.wantKind {
				t.Fatalf("Kind = %q, 期望 %q", ee.Kind, tc.wantKind)
			}
			if !apierr.HasCode(err, tc.wantCode) {
				t.Fatalf("期望码 %s，实际 %v", tc.wantCode, err)
			}
		})
	}
}

func TestOnlineEngine_NetworkFailureClassification(t *testing.T) {
	t.Setenv("MANDARYN_OFFLINE", "0")
	// 指向一个立刻关闭的服务器：得到连接层错误。
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	client := ts.Client()
	url := ts.URL
	ts.Close() // 关掉，后续连接必然失败

	eng, err := NewOnlineEngine(OnlineOptions{
		Endpoint:  url,
		Model:     "m",
		TimeoutMs: 1500,
		Client:    client,
	})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	_, ferr := eng.Finalize(AudioSegment{Samples: []float32{0.1}, SampleRate: 16000})
	ee, ok := AsEngineError(ferr)
	if !ok {
		t.Fatalf("期望 EngineError，实际 %v", ferr)
	}
	if ee.Kind != FailNetwork && ee.Kind != FailTimeout {
		t.Fatalf("Kind = %q, 期望 network 或 timeout", ee.Kind)
	}
	if !apierr.HasCode(ferr, apierr.CodeEngineOnlineNetwork) &&
		!apierr.HasCode(ferr, apierr.CodeEngineOnlineTimeout) {
		t.Fatalf("期望在线网络 / 超时码，实际 %v", ferr)
	}
}

func TestOnlineEngine_OfflineEnforced(t *testing.T) {
	eng, _ := newTestOnline(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("离线模式不应发出任何请求")
	}, nil)
	t.Setenv("MANDARYN_OFFLINE", "1")

	_, err := eng.Finalize(AudioSegment{Samples: []float32{0.1}, SampleRate: 16000})
	if !apierr.HasCode(err, apierr.CodeOfflineEnforced) {
		t.Fatalf("期望 ERR_OFFLINE_ENFORCED，实际 %v", err)
	}
	ee, _ := AsEngineError(err)
	if ee.Kind != FailBlocked {
		t.Fatalf("Kind = %q, 期望 blocked", ee.Kind)
	}
}

func TestOnlineEngine_WarmProbe(t *testing.T) {
	t.Run("200 → ready", func(t *testing.T) {
		eng, _ := newTestOnline(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/models" {
				t.Errorf("探测路径 = %q", r.URL.Path)
			}
			_, _ = w.Write([]byte(`{"data":[]}`))
		}, nil)
		if err := eng.Warm(context.Background()); err != nil {
			t.Fatalf("Warm 失败: %v", err)
		}
		if h := eng.Health(context.Background()); h.State != contract.EngineStateReady {
			t.Fatalf("State = %q", h.State)
		}
	})

	t.Run("404 视为连通", func(t *testing.T) {
		eng, _ := newTestOnline(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}, nil)
		if err := eng.Warm(context.Background()); err != nil {
			t.Fatalf("404 应视为连通，实际: %v", err)
		}
	})

	t.Run("401 → 鉴权错误", func(t *testing.T) {
		eng, _ := newTestOnline(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}, nil)
		err := eng.Warm(context.Background())
		if !apierr.HasCode(err, apierr.CodeEngineOnlineAuth) {
			t.Fatalf("期望 ERR_ENGINE_ONLINE_AUTH，实际 %v", err)
		}
	})
}

func TestProbeOnline(t *testing.T) {
	t.Setenv("MANDARYN_OFFLINE", "0")
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer ts.Close()

	res, err := ProbeOnline(context.Background(), OnlineOptions{
		Endpoint: ts.URL,
		Model:    "m",
		Client:   ts.Client(),
	})
	if err != nil {
		t.Fatalf("探测应成功: %v", err)
	}
	if !res.OK {
		t.Fatalf("期望 OK，实际 %+v", res)
	}
}

func TestEncodeWAV(t *testing.T) {
	samples := []float32{0, 0.5, -0.5, 2.0, -2.0} // 含越界值，验证钳位
	wav := encodeWAV(samples, 16000)

	if len(wav) != 44+len(samples)*2 {
		t.Fatalf("长度 = %d, 期望 %d", len(wav), 44+len(samples)*2)
	}
	if string(wav[0:4]) != "RIFF" || string(wav[8:12]) != "WAVE" ||
		string(wav[12:16]) != "fmt " || string(wav[36:40]) != "data" {
		t.Fatalf("WAV 头不合规: %q", wav[:44])
	}
	if got := binary.LittleEndian.Uint32(wav[4:8]); got != uint32(36+len(samples)*2) {
		t.Fatalf("RIFF size = %d", got)
	}
	if got := binary.LittleEndian.Uint16(wav[20:22]); got != 1 {
		t.Fatalf("audio format = %d, 期望 1 (PCM)", got)
	}
	if got := binary.LittleEndian.Uint16(wav[22:24]); got != 1 {
		t.Fatalf("channels = %d, 期望 1", got)
	}
	if got := binary.LittleEndian.Uint32(wav[24:28]); got != 16000 {
		t.Fatalf("sampleRate = %d", got)
	}
	if got := binary.LittleEndian.Uint16(wav[34:36]); got != 16 {
		t.Fatalf("bitsPerSample = %d", got)
	}
	if got := binary.LittleEndian.Uint32(wav[40:44]); got != uint32(len(samples)*2) {
		t.Fatalf("data size = %d", got)
	}

	// 采样量化：0.5 → 16383；越界 ±2 钳到 ±32767。
	want := []int16{0, 16383, -16383, 32767, -32767}
	for i, w := range want {
		got := int16(binary.LittleEndian.Uint16(wav[44+i*2:]))
		if got != w {
			t.Fatalf("第 %d 个采样 = %d, 期望 %d", i, got, w)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		limit time.Duration
		want  time.Duration
	}{
		{"顶层字段", `{"retry_after":5}`, 30 * time.Second, 5 * time.Second},
		{"嵌套字段", `{"error":{"retry_after":2}}`, 30 * time.Second, 2 * time.Second},
		{"纯数字", `7`, 30 * time.Second, 7 * time.Second},
		{"超上限被钳", `{"retry_after":600}`, 30 * time.Second, 30 * time.Second},
		{"无信息", `{"error":"x"}`, 30 * time.Second, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseRetryAfter([]byte(tc.body), tc.limit); got != tc.want {
				t.Fatalf("= %v, 期望 %v", got, tc.want)
			}
		})
	}
}

func TestParseTranscriptionResponse(t *testing.T) {
	t.Run("标准 text", func(t *testing.T) {
		got, err := parseTranscriptionResponse([]byte(`{"text":"  你好 "}`))
		if err != nil || got.Text != "你好" {
			t.Fatalf("got=%+v err=%v", got, err)
		}
	})
	t.Run("嵌套 data.text", func(t *testing.T) {
		got, err := parseTranscriptionResponse([]byte(`{"data":{"text":"你好"}}`))
		if err != nil || got.Text != "你好" {
			t.Fatalf("got=%+v err=%v", got, err)
		}
	})
	t.Run("空 text 是真结果", func(t *testing.T) {
		got, err := parseTranscriptionResponse([]byte(`{"text":""}`))
		if err != nil || got.Text != "" {
			t.Fatalf("got=%+v err=%v", got, err)
		}
	})
	t.Run("无 text 字段视为格式错误", func(t *testing.T) {
		if _, err := parseTranscriptionResponse([]byte(`{"foo":1}`)); err == nil {
			t.Fatal("期望解析错误")
		}
	})
	t.Run("非 JSON", func(t *testing.T) {
		if _, err := parseTranscriptionResponse([]byte(`<html>`)); err == nil {
			t.Fatal("期望解析错误")
		}
	})
}

func TestBuildTranscriptionMultipart(t *testing.T) {
	body, contentType, err := buildTranscriptionMultipart([]byte("RIFFdata"), "whisper-1", "auto")
	if err != nil {
		t.Fatalf("构造 multipart 失败: %v", err)
	}
	if !strings.HasPrefix(contentType, "multipart/form-data; boundary=") {
		t.Fatalf("Content-Type = %q", contentType)
	}
	s := string(body)
	if !strings.Contains(s, `name="model"`) || !strings.Contains(s, "whisper-1") {
		t.Fatal("缺少 model 字段")
	}
	if !strings.Contains(s, `name="file"`) || !strings.Contains(s, "audio.wav") {
		t.Fatal("缺少 file 字段")
	}
	// language=auto 时不该发该字段（让端点自行判断）。
	if strings.Contains(s, `name="language"`) {
		t.Fatal("auto 时不应发送 language 字段")
	}
}

// 确保 multipart 里的 JSON 响应能被端到端解析（回归保护）。
func TestOnlineEngine_FinalizeParsesNestedResponse(t *testing.T) {
	eng, _ := newTestOnline(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"text": "嵌套文本"},
		})
	}, nil)
	res, err := eng.Finalize(AudioSegment{Samples: []float32{0.1}, SampleRate: 16000})
	if err != nil {
		t.Fatalf("Finalize 失败: %v", err)
	}
	if res.Text != "嵌套文本" {
		t.Fatalf("Text = %q", res.Text)
	}
}
