package app

import (
	"context"
	"encoding/binary"
	"testing"
	"time"

	"vip.oberon.mandaryn/internal/contract"
)

func TestRecordingCueState(t *testing.T) {
	var state recordingCueState
	steps := []struct {
		state, sessionID string
		want             recordingCue
	}{
		{contract.StateIdle, "", noRecordingCue},
		{contract.StatePreparing, "", noRecordingCue},
		{contract.StatePreparing, "", noRecordingCue},
		{contract.StateFailed, "", recordingCueFailure},
		{contract.StateFailed, "", noRecordingCue},
		{contract.StatePreparing, "", noRecordingCue},
		{contract.StateFailed, "", recordingCueFailure}, // another distinct early failure
		{contract.StatePreparing, "", noRecordingCue},
		{contract.StateRecording, "a", recordingCueStart},
		{contract.StateRecording, "a", noRecordingCue},
		{contract.StatePaused, "a", noRecordingCue},
		{contract.StateRecording, "a", noRecordingCue}, // resume
		{contract.StateRecording, "a", noRecordingCue},
		{contract.StateFinalizing, "a", noRecordingCue},
		{contract.StateCompleted, "a", recordingCueComplete},
		{contract.StateCompleted, "a", noRecordingCue},
		{contract.StatePreparing, "a", noRecordingCue},
		{contract.StateCompleted, "a", noRecordingCue}, // late previous session snapshot
		{contract.StateRecording, "a", noRecordingCue},
		{contract.StateRecording, "b", recordingCueStart},
		{contract.StateFailed, "a", noRecordingCue},
		{contract.StateFailed, "b", recordingCueFailure},
		{contract.StateFailed, "b", noRecordingCue},
		{contract.StatePreparing, "b", noRecordingCue},
		{contract.StateFailed, "b", recordingCueFailure}, // preparation failed before replacing old ID
		{contract.StateFailed, "b", noRecordingCue},
		{contract.StatePreparing, "b", noRecordingCue},
		{contract.StateRecording, "c", recordingCueStart},
		{contract.StateInterrupted, "c", recordingCueFailure},
		{contract.StateInterrupted, "c", noRecordingCue},
	}
	for i, step := range steps {
		st := contract.RecordingState{State: step.state, SessionID: step.sessionID, SourceID: "app:player.exe"}
		if step.state == contract.StateInterrupted {
			st.LastError = &contract.ErrorDTO{Code: "ERR_DB_WRITE"}
		}
		if got := state.observe(st); got != step.want {
			t.Fatalf("step %d %s/%s: cue %v, want %v", i, step.state, step.sessionID, got, step.want)
		}
	}
}

func TestRecordingCueDeviceLoopbackHasNoStartTone(t *testing.T) {
	var state recordingCueState
	steps := []struct {
		state, sessionID string
		want             recordingCue
	}{
		{contract.StatePreparing, "", noRecordingCue},
		{contract.StateRecording, "a", noRecordingCue},
		{contract.StatePaused, "a", noRecordingCue},
		{contract.StateRecording, "a", noRecordingCue},
		{contract.StateCompleted, "a", recordingCueComplete},
		{contract.StateCompleted, "a", noRecordingCue},
		{contract.StatePreparing, "a", noRecordingCue},
		{contract.StateRecording, "b", noRecordingCue},
		{contract.StateFailed, "b", recordingCueFailure},
		{contract.StateFailed, "b", noRecordingCue},
	}
	for _, step := range steps {
		st := contract.RecordingState{State: step.state, SessionID: step.sessionID, SourceID: "dev:speaker"}
		if got := state.observe(st); got != step.want {
			t.Fatalf("%s/%s: cue %v, want %v", step.state, step.sessionID, got, step.want)
		}
	}
}

func TestRecordingCueWAV(t *testing.T) {
	var frequency [recordingCueFailure + 1]int
	for _, cue := range []recordingCue{recordingCueStart, recordingCueComplete, recordingCueFailure} {
		wav := recordingCueWAV(cue)
		le := binary.LittleEndian
		if len(wav) <= 44 || string(wav[:4]) != "RIFF" || string(wav[8:16]) != "WAVEfmt " || string(wav[36:40]) != "data" {
			t.Fatalf("cue %v: invalid WAV header", cue)
		}
		if int(le.Uint32(wav[4:])) != len(wav)-8 || int(le.Uint32(wav[40:])) != len(wav)-44 || le.Uint32(wav[16:]) != 16 {
			t.Fatalf("cue %v: invalid WAV size", cue)
		}
		if le.Uint16(wav[20:]) != 1 || le.Uint16(wav[22:]) != 1 || le.Uint16(wav[34:]) != 16 ||
			le.Uint16(wav[32:]) != 2 || le.Uint32(wav[28:]) != 2*le.Uint32(wav[24:]) || le.Uint32(wav[24:]) != recordingCueRate {
			t.Fatalf("cue %v: expected 16-bit mono PCM", cue)
		}
		n := (len(wav) - 44) / 2
		if n*1000/recordingCueRate > 150 || le.Uint16(wav[44:]) != 0 || le.Uint16(wav[len(wav)-2:]) != 0 {
			t.Fatalf("cue %v: expected short, smoothly faded clip", cue)
		}
		peak, cycles, previous := 0, 0, 0
		for i := 44; i < len(wav); i += 2 {
			sample := int(int16(le.Uint16(wav[i:])))
			peak = max(peak, sample, -sample)
			if previous <= 0 && sample > 0 {
				cycles++
			}
			previous = sample
		}
		if peak == 0 || peak*100 > 32767*3 {
			t.Fatalf("cue %v: peak %d outside (0, 3%% full scale]", cue, peak)
		}
		frequency[cue] = cycles * recordingCueRate / n
	}
	if frequency[recordingCueFailure] >= frequency[recordingCueStart] || frequency[recordingCueFailure] >= frequency[recordingCueComplete] {
		t.Fatalf("failure tone must be lower: %v", frequency)
	}
}

func TestRecordingCueWorkerDropsWhenBusyAndRechecksSetting(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := make(chan bool, recordingCueQueueSize+1)
	lookups := make(chan struct{}, recordingCueQueueSize+1)
	played := make(chan struct{}, recordingCueQueueSize+1)
	observe := runRecordingCueWorker(ctx, func(ctx context.Context) bool {
		lookups <- struct{}{}
		select {
		case allow := <-settings:
			return allow
		case <-ctx.Done():
			return false
		}
	}, func([]byte) { played <- struct{}{} })

	fail := func() {
		observe(contract.RecordingState{State: contract.StatePreparing})
		observe(contract.RecordingState{State: contract.StateFailed})
	}
	fail()
	awaitRecordingCueTestSignal(t, lookups)

	// Hold one in-flight lookup while filling past capacity. No mock player
	// or actual device is required to test nonblocking enqueue.
	done := make(chan struct{})
	go func() {
		for i := 0; i < recordingCueQueueSize+4; i++ {
			fail()
		}
		close(done)
	}()
	awaitRecordingCueTestSignal(t, done)
	settings <- true
	awaitRecordingCueTestSignal(t, played)
	for i := 0; i < recordingCueQueueSize; i++ {
		awaitRecordingCueTestSignal(t, lookups)
		settings <- false // already queued, but disabled before playback
	}
	select {
	case <-lookups:
		t.Fatal("worker accepted more than queue capacity")
	case <-played:
		t.Fatal("disabled cue reached player")
	case <-time.After(25 * time.Millisecond):
	}
}

func awaitRecordingCueTestSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for cue worker")
	}
}
