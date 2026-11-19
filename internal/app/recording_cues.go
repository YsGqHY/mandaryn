package app

import (
	"context"
	"encoding/binary"
	"math"
	"strings"
	"sync"
	"time"

	"vip.oberon.mandaryn/internal/capture"
	"vip.oberon.mandaryn/internal/contract"
)

type recordingCue uint8

const (
	noRecordingCue recordingCue = iota
	recordingCueStart
	recordingCueComplete
	recordingCueFailure
)

// A real start attempt always emits preparing before either recording or a
// preparation failure. Preparing (and early failure) may carry the *previous*
// session ID because the recorder retains its last run until start succeeds.
type recordingCueState struct {
	preparing      bool
	active         bool
	deviceLoopback bool
	sessionID      string
}

func (s *recordingCueState) observe(st contract.RecordingState) recordingCue {
	switch st.State {
	case contract.StatePreparing:
		if !s.active && st.SessionID == s.sessionID {
			s.preparing = true
		}
	case contract.StateRecording:
		if s.preparing && st.SessionID != "" && st.SessionID != s.sessionID {
			s.preparing, s.active, s.sessionID = false, true, st.SessionID
			// Device loopback would record our own chime and turn silence
			// into a non-empty recording. Only process-isolated capture can
			// play a start cue; unknown sources also fail closed.
			s.deviceLoopback = !strings.HasPrefix(st.SourceID, capture.AppIDPrefix)
			if !s.deviceLoopback {
				return recordingCueStart
			}
		}
	case contract.StateCompleted, contract.StateFailed, contract.StateInterrupted:
		if st.State == contract.StateInterrupted && st.LastError == nil {
			return noRecordingCue // a historical interruption is not an operation failure
		}
		matched := s.active && st.SessionID == s.sessionID
		if s.preparing {
			// Early start failures can have no new ID. If a stop races the
			// recording snapshot, its completion must have a new session ID.
			matched = st.State == contract.StateFailed || st.SessionID != "" && st.SessionID != s.sessionID
		}
		if matched {
			s.preparing, s.active, s.deviceLoopback = false, false, false
			s.sessionID = st.SessionID
			if st.State == contract.StateCompleted {
				return recordingCueComplete
			}
			return recordingCueFailure
		}
	}
	return noRecordingCue
}

const recordingCueQueueSize = 8

// Only one worker touches the player. The observer holds its mutex solely to
// classify/offer a cue; it never waits for the worker, database or audio device.
func runRecordingCueWorker(ctx context.Context, enabled func(context.Context) bool, play func([]byte)) func(contract.RecordingState) {
	var mu sync.Mutex
	var state recordingCueState
	queue := make(chan recordingCue, recordingCueQueueSize)
	go func() {
		var wavs [recordingCueFailure + 1][]byte
		for {
			select {
			case <-ctx.Done():
				return
			case cue := <-queue:
				if ctx.Err() != nil {
					return
				}
				if wavs[cue] == nil {
					wavs[cue] = recordingCueWAV(cue)
				}
				readCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
				allow := enabled(readCtx)
				cancel()
				// Recheck at dequeue time: disabling cues or starting a new
				// device loopback suppresses queued clips. Allow setup to finish
				// before playing anything, since its source is not known yet.
				mu.Lock()
				blocked := state.preparing || state.deviceLoopback
				mu.Unlock()
				if allow && !blocked && ctx.Err() == nil {
					play(wavs[cue])
				}
			}
		}
	}()
	return func(st contract.RecordingState) {
		mu.Lock()
		defer mu.Unlock()
		if cue := state.observe(st); cue != noRecordingCue {
			select {
			case queue <- cue:
			default: // Drop if busy; recording never depends on cue delivery.
			}
		}
	}
}

const (
	recordingCueRate = 22050
	recordingCuePeak = 900 // signed 16-bit full scale: 900 / 32767 < 3%
)

func recordingCueWAV(cue recordingCue) []byte {
	var hz float64
	var durationMs int
	switch cue {
	case recordingCueStart:
		hz, durationMs = 720, 90
	case recordingCueComplete:
		hz, durationMs = 960, 110
	case recordingCueFailure:
		hz, durationMs = 360, 140
	default:
		return nil
	}

	n := recordingCueRate * durationMs / 1000
	wav := make([]byte, 44+2*n)
	le := binary.LittleEndian
	copy(wav, "RIFF")
	le.PutUint32(wav[4:], uint32(len(wav)-8))
	copy(wav[8:], "WAVEfmt ")
	le.PutUint32(wav[16:], 16) // fmt chunk length
	le.PutUint16(wav[20:], 1)  // PCM
	le.PutUint16(wav[22:], 1)  // mono
	le.PutUint32(wav[24:], recordingCueRate)
	le.PutUint32(wav[28:], 2*recordingCueRate)
	le.PutUint16(wav[32:], 2)  // block align
	le.PutUint16(wav[34:], 16) // bits per sample
	copy(wav[36:], "data")
	le.PutUint32(wav[40:], uint32(2*n))

	// Fade the ends in and out to avoid a click louder than the tone.
	ramp := recordingCueRate * 8 / 1000
	step := 2 * math.Pi * hz / recordingCueRate
	for i := 0; i < n; i++ {
		gain := float64(min(i, n-1-i, ramp)) / float64(ramp)
		sample := int16(math.Round(recordingCuePeak * gain * math.Sin(step*float64(i))))
		le.PutUint16(wav[44+2*i:], uint16(sample))
	}
	return wav
}
