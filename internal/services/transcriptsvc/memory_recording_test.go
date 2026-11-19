package transcriptsvc

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"vip.oberon.mandaryn/internal/asr"
	"vip.oberon.mandaryn/internal/capture"
	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/recording"
	"vip.oberon.mandaryn/internal/services/recordingsvc"
	"vip.oberon.mandaryn/internal/storage"
	"vip.oberon.mandaryn/internal/transcribe"
	"vip.oberon.mandaryn/internal/utils/audioframes"
)

type memoryTestChannel struct {
	samples      []float32
	rate, offset int
	closed       chan struct{}
	once         sync.Once
}

func (c *memoryTestChannel) Format() capture.AudioFormat {
	return capture.AudioFormat{SampleRate: c.rate, Channels: 1}
}
func (c *memoryTestChannel) Err() <-chan error { return nil }
func (c *memoryTestChannel) Close() error      { c.once.Do(func() { close(c.closed) }); return nil }
func (c *memoryTestChannel) ReadPCM(dst []float32) (int, error) {
	if c.offset == len(c.samples) {
		<-c.closed
		return 0, io.EOF
	}
	select {
	case <-c.closed:
		return 0, io.EOF
	case <-time.After(2 * time.Millisecond):
	}
	n := copy(dst, c.samples[c.offset:])
	c.offset += n
	return n, nil
}

func memoryRecordingSamples(t *testing.T) ([]float32, int) {
	t.Helper()
	if filename := os.Getenv("MANDARYN_TEST_MEMORY_WAV"); filename != "" {
		f, err := os.Open(filename)
		if err != nil {
			t.Fatal(err)
		}
		source, err := audioframes.OpenFrameSource(f)
		if err != nil {
			_ = f.Close()
			t.Fatal(err)
		}
		defer source.Close()
		pcm, err := source.ReadFrames(0, source.TotalFrames())
		if err != nil {
			t.Fatal(err)
		}
		return pcm, source.SampleRate()
	}
	return append(makeTone(1700, 16000), makeSilence(800, 16000)...), 16000
}

func TestMemoryRecordingOnlineLifecycle(t *testing.T) {
	runMemoryRecordingOnlineLifecycle(t, nil, nil)
}

func runMemoryRecordingOnlineLifecycle(t *testing.T, recognize func([]byte) (string, error), checkText func(*testing.T, string)) {
	t.Helper()
	t.Setenv("MANDARYN_OFFLINE", "0")
	samples, rate := memoryRecordingSamples(t)
	for _, mode := range []string{"stop", "endpoint-failure", "shutdown", "cancel-after-stop", "redirect"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newFixture(t, false)
			if err := fixture.db.GORM.Model(&storage.AppSettings{}).Where("id = ?", 1).
				Updates(map[string]any{"model_source": contract.ModelSourceOnline, "no_audio_persist": true, "save_format": "flac"}).Error; err != nil {
				t.Fatal(err)
			}
			var sent, redirected atomic.Int32
			arrived, cancelled := make(chan struct{}), make(chan struct{})
			var first sync.Once
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/redirected" {
					redirected.Add(1)
					http.Error(w, "unexpected redirect", 500)
					return
				}
				if r.Method != http.MethodPost || r.URL.Path != "/v1/audio/transcriptions" {
					t.Error("unexpected endpoint request")
					http.NotFound(w, r)
					return
				}
				parts, err := r.MultipartReader()
				if err != nil {
					t.Error(err)
					http.Error(w, "multipart", 400)
					return
				}
				var audio []byte
				for {
					part, err := parts.NextPart()
					if err == io.EOF {
						break
					}
					if err != nil {
						t.Error(err)
						return
					}
					if part.FormName() == "file" {
						data, err := io.ReadAll(io.LimitReader(part, 512<<10))
						if err == nil && len(data) > 44 && string(data[:4]) == "RIFF" && len(data) <= 16000*2*6+44 {
							audio = data
						}
					}
				}
				if audio == nil {
					t.Error("expected a bounded in-memory WAV chunk")
					http.Error(w, "audio", 400)
					return
				}
				sent.Add(1)
				first.Do(func() { close(arrived) })
				switch mode {
				case "endpoint-failure":
					http.Error(w, "denied", http.StatusUnauthorized)
				case "redirect":
					http.Redirect(w, r, "/redirected", http.StatusTemporaryRedirect)
				case "shutdown", "cancel-after-stop":
					<-r.Context().Done()
					close(cancelled)
				default:
					text := "回环端点返回的定稿"
					if recognize != nil {
						var err error
						text, err = recognize(audio)
						if err != nil {
							t.Error(err)
							http.Error(w, "recognition failed", 500)
							return
						}
					}
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]string{"text": text})
				}
			}))
			defer server.Close()
			engine, err := asr.NewOnlineEngine(asr.OnlineOptions{Endpoint: server.URL, Model: "memory-fixture", Client: server.Client(), RefuseRedirects: true, TimeoutMs: 4000})
			if err != nil {
				t.Fatal(err)
			}
			defer engine.Close()
			var sink *transcribe.MemoryRunner
			rec, err := recording.New(recording.Options{
				Store: recording.NewGORMStore(fixture.holder),
				DataRoot: func() (string, error) {
					t.Error("no-audio recording must not resolve a file destination")
					return fixture.data, nil
				},
				OpenChannel: func(string) (capture.Channel, error) {
					return &memoryTestChannel{samples: samples, rate: rate, closed: make(chan struct{})}, nil
				},
				NewMemorySink: func(id string) (recording.MemorySink, error) {
					var err error
					sink, err = NewMemorySink(context.Background(), fixture.svc, id, engine)
					return sink, err
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			svc := recordingsvc.New(rec, fixture.holder, recordingsvc.Emit{})
			t.Cleanup(func() { _ = svc.ServiceShutdown() })
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			st, err := svc.Start(ctx, contract.StartRequest{SourceID: "dev:fixture"})
			if err != nil || !st.NoAudioPersist {
				t.Fatalf("start: %+v %v", st, err)
			}
			assertNoFiles := func() {
				t.Helper()
				err := filepath.WalkDir(fixture.data, func(path string, d os.DirEntry, err error) error {
					if err != nil {
						return err
					}
					if !d.IsDir() && !(strings.HasPrefix(d.Name(), "mandaryn.db")) {
						t.Errorf("unexpected recording output: %s", path)
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			assertNoFiles()
			if mode == "shutdown" {
				select {
				case <-arrived:
				case <-ctx.Done():
					t.Fatal("no upload in flight")
				}
				if err := svc.ServiceShutdown(); err != nil {
					t.Fatal(err)
				}
				if err := fixture.svc.ServiceShutdown(); err != nil {
					t.Fatal(err)
				}
				select {
				case <-cancelled:
				case <-ctx.Done():
					t.Fatal("request not cancelled on exit")
				}
			} else {
				wantMs := int64(len(samples)*1000/rate) - 50
				for rec.State().ElapsedMs < wantMs {
					select {
					case <-ctx.Done():
						t.Fatal("capture did not consume the fixture")
					case <-time.After(5 * time.Millisecond):
					}
				}
				if stopped, err := svc.Stop(ctx); err != nil || stopped.State != contract.StateCompleted {
					t.Fatalf("stop: %+v %v", stopped, err)
				}
			}
			if mode == "cancel-after-stop" {
				select {
				case <-arrived:
				case <-ctx.Done():
					t.Fatal("no upload in flight")
				}
				if state, err := fixture.svc.CancelTranscription(ctx, st.SessionID); err != nil || state.Status != transcribe.StatusFailed {
					t.Fatalf("cancel after stop: %+v %v", state, err)
				}
				select {
				case <-cancelled:
				case <-ctx.Done():
					t.Fatal("request not cancelled by API")
				}
			}
			if !sink.Wait(ctx) {
				t.Fatal("memory worker did not finish")
			}
			row := fixture.session(t, st.SessionID)
			if row.AudioPath != "" || row.AudioFormat != "" || row.SizeBytes != 0 || row.ModelSource != contract.ModelSourceOnline || !row.LiveTranscribed || sent.Load() == 0 || redirected.Load() != 0 {
				t.Fatalf("wrong metadata: %+v; uploads=%d redirects=%d", row, sent.Load(), redirected.Load())
			}
			if mode == "stop" {
				got := fixture.segments(t, st.SessionID)
				if row.TranscriptionStatus != transcribe.StatusDone || len(got) == 0 || !got[0].IsFinal {
					t.Fatalf("missing final text: status=%s, segments=%d", row.TranscriptionStatus, len(got))
				}
				if checkText == nil {
					if got[0].Text != "回环端点返回的定稿" {
						t.Fatal("lost fixture text")
					}
				} else {
					var text strings.Builder
					for _, seg := range got {
						text.WriteString(seg.Text)
					}
					checkText(t, text.String())
				}
			} else if row.TranscriptionStatus != transcribe.StatusFailed {
				t.Fatalf("failure not visible: %+v", row)
			}
			if _, err := fixture.svc.StartTranscription(ctx, st.SessionID); err == nil {
				t.Fatal("text-only session accepted a retry")
			}
			assertNoFiles()
		})
	}
}
