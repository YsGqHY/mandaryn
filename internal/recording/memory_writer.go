package recording

import "sync"

// memoryWriter keeps only frame counters for no-audio-persist sessions.
// It never touches DataRoot or the filesystem.
type memoryWriter struct {
	mu     sync.Mutex
	frames int64
	closed bool
}

func newMemoryWriter() *memoryWriter { return &memoryWriter{} }

func (w *memoryWriter) Write(samples []float32) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.frames += int64(len(samples))
	return nil
}

func (w *memoryWriter) Seal() error { return nil }

func (w *memoryWriter) Close() error {
	w.mu.Lock()
	w.closed = true
	w.mu.Unlock()
	return nil
}

func (w *memoryWriter) Frames() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.frames
}

func (w *memoryWriter) DurationMs() int64 { return FramesToMs(w.Frames()) }

func (w *memoryWriter) SizeBytes() int64 { return 0 }
