package transcribe

import (
	"context"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/asr"
	"vip.oberon.mandaryn/internal/contract"
)

// MemoryEngine belongs to a single session and supports cancelling an HTTP request.
type MemoryEngine interface {
	ID() string
	FinalizeContext(context.Context, asr.AudioSegment) (asr.FinalResult, error)
	Close() error
}

type MemoryOptions struct {
	Store     Store
	Engine    MemoryEngine
	SessionID string
	Vad       VadConfig
	Allowed   func() error
	OnSegment func(contract.Segment)
	OnState   func(contract.TranscriptionState)
}

// MemoryRunner never opens a file. Its queue holds at most 20 seconds of PCM;
// overflow fails transcription rather than silently losing speech.
type MemoryRunner struct {
	opt        MemoryOptions
	ctx        context.Context
	cancel     context.CancelFunc
	mu         sync.Mutex
	queue      []float32
	capSamples int
	nextFrame  int64
	chars      int64
	closed     bool
	failure    error
	wake       chan struct{}
	done       chan struct{}
}

func NewMemoryRunner(ctx context.Context, opt MemoryOptions) (*MemoryRunner, error) {
	if opt.Engine == nil || opt.Store == nil || opt.SessionID == "" {
		return nil, apierr.New(apierr.CodeInvalidArgument).WithDetail("invalid memory transcription configuration")
	}
	opt.Vad.SampleRate = 16000
	opt.Vad.MaxSegmentMs = 5000
	opt.Vad = opt.Vad.Normalize()
	if err := opt.Store.UpdateTranscription(ctx, TranscriptionUpdate{SessionID: opt.SessionID, Status: StatusRunning}); err != nil {
		return nil, err
	}
	r := &MemoryRunner{opt: opt, capSamples: opt.Vad.SampleRate * 20, wake: make(chan struct{}, 1), done: make(chan struct{})}
	r.ctx, r.cancel = context.WithCancel(context.Background())
	go r.run()
	return r, nil
}

func (r *MemoryRunner) Push(samples []float32, atFrame int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.failure != nil || len(samples) == 0 {
		return
	}
	if atFrame != r.nextFrame || len(samples) > r.capSamples-len(r.queue) {
		r.failure = apierr.New(apierr.CodeEngineOnlineTimeout).WithDetail("memory transcription could not keep up with audio capture")
		r.queue = nil
		r.cancel()
		return
	}
	r.nextFrame += int64(len(samples))
	r.queue = append(r.queue, samples...)
	r.signal()
}

func (r *MemoryRunner) signal() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *MemoryRunner) Close() {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	r.signal()
}

func (r *MemoryRunner) Abort() {
	r.mu.Lock()
	r.closed = true
	r.queue = nil
	r.cancel()
	r.mu.Unlock()
}

func (r *MemoryRunner) Wait(ctx context.Context) bool {
	select {
	case <-r.done:
		return true
	case <-ctx.Done():
		return false
	}
}

func (r *MemoryRunner) SessionID() string { return r.opt.SessionID }

func (r *MemoryRunner) CharCount() int64 { // LiveSink's counter is polled independently of transcription events.
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.chars
}

func (r *MemoryRunner) take() ([]float32, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.queue) == 0 || r.ctx.Err() != nil {
		return nil, r.closed || r.ctx.Err() != nil
	}
	n := min(len(r.queue), r.opt.Vad.SampleRate/5)
	out := append([]float32(nil), r.queue[:n]...)
	remaining := copy(r.queue, r.queue[n:])
	clear(r.queue[remaining:])
	r.queue = r.queue[:remaining]
	return out, false
}

func (r *MemoryRunner) run() {
	defer close(r.done)
	defer r.opt.Engine.Close()
	defer r.cancel()
	state := contract.TranscriptionState{SessionID: r.opt.SessionID, Status: StatusRunning, EngineID: r.opt.Engine.ID(), StartedAt: time.Now().UnixMilli()}
	emit := func() {
		if r.opt.OnState != nil {
			r.opt.OnState(state)
		}
	}
	emit()
	sp := NewUtteranceSplitter(r.opt.Vad)
	var prev *SegmentRow
	process := func(utterances []Utterance) error {
		for _, u := range utterances {
			if err := r.ctx.Err(); err != nil {
				return err
			}
			if r.opt.Allowed != nil {
				if err := r.opt.Allowed(); err != nil {
					return err
				}
			}
			res, err := r.opt.Engine.FinalizeContext(r.ctx, asr.AudioSegment{Samples: u.Samples, SampleRate: u.SampleRate, SessionID: r.opt.SessionID, Seq: u.Index, StartMs: u.StartMs, EndMs: u.EndMs})
			if err != nil {
				return err
			}
			if err := r.ctx.Err(); err != nil {
				return err
			}
			if text := strings.TrimSpace(res.Text); text != "" {
				row := SegmentRow{SessionID: r.opt.SessionID, Seq: u.Index, StartMs: u.StartMs, EndMs: u.EndMs, Text: text, EngineID: state.EngineID, Confidence: res.Confidence, IsFinal: true}
				if err := assertSegmentOrder(prev, row); err != nil {
					return err
				}
				ctx, cancel := context.WithTimeout(r.ctx, liveWriteTimeout)
				saved, err := r.opt.Store.UpsertSegment(ctx, row)
				cancel()
				if err != nil {
					return apierr.From(err, apierr.CodeDBWrite)
				}
				prev = &saved
				state.Segments++
				r.mu.Lock()
				r.chars += int64(utf8.RuneCountInString(text))
				r.mu.Unlock()
				if r.opt.OnSegment != nil {
					r.opt.OnSegment(contract.Segment{ID: saved.ID, SessionID: saved.SessionID, Seq: saved.Seq, StartMs: saved.StartMs, EndMs: saved.EndMs, Text: saved.Text, EngineID: saved.EngineID, Confidence: saved.Confidence, Finalized: true})
				}
			}
			state.ProgressMs = u.EndMs
		}
		return nil
	}
	var err error
	for err == nil {
		pcm, finished := r.take()
		switch {
		case finished:
			if err = r.ctx.Err(); err == nil {
				err = process(sp.Flush())
			}
		case len(pcm) != 0:
			err = process(sp.Push(pcm))
		default:
			select {
			case <-r.ctx.Done():
				err = r.ctx.Err()
			case <-r.wake:
			}
		}
		if finished {
			break
		}
	}
	r.mu.Lock()
	if r.failure != nil {
		err = r.failure
	} else if err == nil {
		err = r.ctx.Err()
	}
	r.queue = nil
	r.closed = true
	chars := r.chars
	r.mu.Unlock()
	state.Status = StatusDone
	if err != nil {
		state.Status = StatusFailed
		state.ErrorCode = asrMemoryErrorCode(err)
	}
	state.FinishedAt = time.Now().UnixMilli()
	ctx, cancel := context.WithTimeout(context.Background(), liveWriteTimeout)
	defer cancel()
	if err := r.opt.Store.UpdateTranscription(ctx, TranscriptionUpdate{SessionID: r.opt.SessionID, Status: state.Status,
		ProgressMs: &state.ProgressMs, EngineID: &state.EngineID, TranscribedAt: &state.FinishedAt, SegmentCount: &state.Segments, CharCount: &chars}); err != nil {
		state.Status, state.ErrorCode = StatusFailed, apierr.CodeDBWrite
	}
	emit()
}

func asrMemoryErrorCode(err error) string {
	if e, ok := err.(*asr.EngineError); ok && e.Err != nil {
		return e.Err.Code
	}
	return apierr.From(err, apierr.CodeEngineNotReady).Code
}
