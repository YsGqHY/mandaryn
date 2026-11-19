package recording

import (
	"context"
	"testing"
	"time"

	"vip.oberon.mandaryn/internal/capture"
	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/storage"
)

type policyMemorySink struct {
	closed  int
	aborted int
}

func (s *policyMemorySink) Push([]float32, int64) {}
func (s *policyMemorySink) Close()                { s.closed++ }
func (s *policyMemorySink) Abort()                { s.aborted++ }
func (s *policyMemorySink) CharCount() int64      { return 0 }

func TestStartWithPolicyNoAudioPersistNeverReadsDataRoot(t *testing.T) {
	sink := &policyMemorySink{}
	dataRootReads := 0
	rec, err := New(Options{
		Store: newTestStore(t),
		DataRoot: func() (string, error) {
			dataRootReads++
			panic("no-disk recording must not read DataRoot")
		},
		OpenChannel: func(string) (capture.Channel, error) {
			ch := newFakeChannel(capture.AudioFormat{SampleRate: 16000, Channels: 1}, fillConst(0.3))
			ch.delay = 2 * time.Millisecond
			return ch, nil
		},
		NewMemorySink: func(string) (MemorySink, error) { return sink, nil },
		Now:           func() time.Time { return time.UnixMilli(1_700_000_000_000) },
	})
	if err != nil {
		t.Fatal(err)
	}

	st, err := rec.StartWithPolicy(contract.StartRequest{SourceID: "dev:online"}, true, AudioFormatWAV, contract.SensitivityStandard, contract.ModelSourceOnline, true)
	if err != nil {
		t.Fatalf("StartWithPolicy: %v", err)
	}
	if !st.NoAudioPersist || st.State != contract.StateRecording {
		t.Fatalf("state = %+v, want recording/noAudioPersist", st)
	}
	row := getSession(t, rec, st.SessionID)
	if row.AudioPath != "" || row.AudioFormat != "" || row.SizeBytes != 0 || !row.LiveTranscribed {
		t.Fatalf("no-disk session metadata = %+v", row)
	}
	waitFor(t, 3*time.Second, "录到 1s", func() bool { return rec.Metrics().RecordedMs >= 1000 })
	if _, err := rec.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if dataRootReads != 0 {
		t.Fatalf("DataRoot reads = %d", dataRootReads)
	}
	if sink.closed != 1 || sink.aborted != 0 {
		t.Fatalf("sink lifecycle close=%d abort=%d", sink.closed, sink.aborted)
	}
}

func TestRecoverNoAudioPathDoesNotReadDataRoot(t *testing.T) {
	store := newTestStore(t)
	rec, err := New(Options{
		Store:    store,
		DataRoot: func() (string, error) { panic("missing audio path must not read DataRoot") },
	})
	if err != nil {
		t.Fatal(err)
	}
	id := "s_NODISKRECOVERY0000000000000"
	if err := store.CreateSession(context.Background(), &storage.Session{
		ID: id, SourceID: "app:test", Status: contract.StateRecording,
		StartedAt: 1_700_000_000_000, AudioPath: "", AudioFormat: "",
		TranscriptionStatus: contract.TranscriptionRunning,
	}); err != nil {
		t.Fatal(err)
	}
	results, err := rec.RecoverUnfinished(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != contract.StateInterrupted || !results[0].Missing {
		t.Fatalf("recovery result = %+v", results)
	}
	row, err := store.GetSession(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != contract.StateInterrupted || row.TranscriptionStatus != contract.TranscriptionFailed || row.AudioPath != "" || row.SizeBytes != 0 {
		t.Fatalf("recovered row = %+v", row)
	}
}
