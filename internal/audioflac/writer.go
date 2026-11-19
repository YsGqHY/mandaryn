// Package audioflac records lossless 16 kHz, mono, 16-bit audio as native FLAC.
package audioflac

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync"

	"github.com/mewkiz/flac"
	"github.com/mewkiz/flac/frame"
	"github.com/mewkiz/flac/meta"
)

const (
	sampleRate    = 16000
	bitsPerSample = 16
	blockSamples  = sampleRate / 10 // 100 ms per fixed-size block.
	syncSamples   = sampleRate / 5
	maxSamples    = (1 << 36) - 1 // STREAMINFO's sample count.
	maxFrameBytes = blockSamples*2 + 32
)

// recordingFile lets tests inject short writes without any dependence on the disk.
type recordingFile interface {
	io.WriteCloser
	io.WriterAt
	Sync() error
}

// Writer records one stream. All its methods are safe for concurrent use.
// Accepted samples in the last partial block remain in memory until the next
// full block or Seal. The periodic sync bounds the unsynced window to 200 ms.
type Writer struct {
	mu   sync.Mutex
	f    recordingFile
	path string

	encoded  bytes.Buffer
	encoder  *flac.Encoder
	pending  [blockSamples]int32
	n        int
	stats    streamStats
	bytes    int64
	synced   int64
	sealed   bool
	closed   bool
	err      error
	blockErr error // A failed block must be handled by Repair, not by Seal.
}

// Create opens a recording file, creating its parent directories as needed,
// and makes a valid FLAC header durable before returning.
func Create(path string) (*Writer, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("audioflac: create directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("audioflac: create file: %w", err)
	}
	w := &Writer{f: f, path: path, stats: newStreamStats()}
	info := &meta.StreamInfo{
		BlockSizeMin: blockSamples, BlockSizeMax: blockSamples,
		SampleRate: sampleRate, NChannels: 1, BitsPerSample: bitsPerSample,
	}
	w.encoder, err = flac.NewEncoder(&w.encoded, info, &meta.Block{
		Header: meta.Header{Type: meta.TypeApplication, Length: 4 + intentBytes},
		Body:   &meta.Application{ID: applicationID, Data: make([]byte, intentBytes)},
	})
	if err == nil && w.encoded.Len() != headerBytes {
		err = errors.New("unexpected FLAC header length")
	}
	if err == nil {
		err = w.appendLocked(w.encoded.Bytes())
	}
	if err == nil {
		err = f.Sync()
	}
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("audioflac: initialize file: %w", err)
	}
	w.encoded.Reset()
	return w, nil
}

func (w *Writer) Path() string { return w.path }

// Frames is the number of accepted mono samples, including the pending block.
func (w *Writer) Frames() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stats.samples + int64(w.n)
}

func (w *Writer) DurationMs() int64 { return w.Frames() * 1000 / sampleRate }
func (w *Writer) DataBytes() int64  { return w.Frames() * (bitsPerSample / 8) }

// SizeBytes is the number of physical file bytes written, not the PCM size.
func (w *Writer) SizeBytes() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.bytes
}

// Err returns the first recording failure, if any.
func (w *Writer) Err() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

func (w *Writer) Write(samples []float32) error {
	if len(samples) == 0 {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return w.err
	}
	if w.sealed || w.closed {
		return errors.New("audioflac: cannot write after seal or close")
	}
	if int64(len(samples)) > maxSamples-w.stats.samples-int64(w.n) {
		// Do not drop previously accepted samples. Seal can still finalize them.
		w.err = errors.New("audioflac: recording exceeds FLAC sample-count limit")
		return w.err
	}
	for len(samples) != 0 {
		n := min(len(samples), blockSamples-w.n)
		for i, v := range samples[:n] {
			w.pending[w.n+i] = floatToPCM16(v)
		}
		w.n += n
		samples = samples[n:]
		if w.n == blockSamples {
			if err := w.flushBlockLocked(); err != nil {
				return err
			}
			if w.stats.samples-w.synced >= syncSamples {
				if err := w.syncLocked(); err != nil {
					w.err = err
					return err
				}
			}
		}
	}
	return nil
}

// Sync makes all emitted blocks durable. Seal also emits the final partial
// block; a short block cannot be followed by another in this fixed-size stream.
func (w *Writer) Sync() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	if err := w.syncLocked(); err != nil && w.err == nil {
		w.err = err
	}
	return w.err
}

func (w *Writer) syncLocked() error {
	if err := w.f.Sync(); err != nil {
		return fmt.Errorf("audioflac: sync file: %w", err)
	}
	w.synced = w.stats.samples
	return nil
}

// Seal emits any remaining samples, writes STREAMINFO and syncs the file.
// Successful seals are idempotent and prevent any further nonempty writes.
func (w *Writer) Seal() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.sealLocked()
}

func (w *Writer) sealLocked() error {
	if w.sealed {
		return nil
	}
	if w.closed {
		return errors.New("audioflac: cannot seal a closed file")
	}
	if w.blockErr != nil {
		return w.blockErr
	}
	if err := w.flushBlockLocked(); err != nil {
		return err
	}
	// Make complete frames durable before publishing their count and digest.
	if err := w.syncLocked(); err != nil {
		return w.rememberLocked(err)
	}
	info := w.stats.streamInfo()
	if err := writeAt(w.f, info[:], streamInfoOffset); err != nil {
		return w.rememberLocked(fmt.Errorf("audioflac: finalize STREAMINFO: %w", err))
	}
	if err := w.syncLocked(); err != nil {
		return w.rememberLocked(err)
	}
	w.sealed = true
	return nil
}

// Close seals and closes the file, even if sealing fails.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	sealErr := w.sealLocked()
	w.closed = true
	return errors.Join(sealErr, w.f.Close())
}

func (w *Writer) rememberLocked(err error) error {
	if w.err == nil {
		w.err = err
	}
	return err
}

func (w *Writer) flushBlockLocked() (err error) {
	if w.n == 0 {
		return nil
	}
	defer func() {
		if err != nil {
			w.blockErr = w.rememberLocked(err)
		}
	}()
	samples := w.pending[:w.n]
	w.n = 0 // Encoder advances even on error; failed blocks are never retried.
	w.encoded.Reset()
	f := frame.Frame{
		Header: frame.Header{HasFixedBlockSize: true, BlockSize: uint16(len(samples)),
			SampleRate: sampleRate, Channels: frame.ChannelsMono, BitsPerSample: bitsPerSample},
		Subframes: []*frame.Subframe{{SubHeader: frame.SubHeader{Pred: frame.PredVerbatim},
			NSamples: len(samples), Samples: samples}},
	}
	// The dependency chooses constant, fixed prediction + Rice, or verbatim.
	if err := w.encoder.WriteFrame(&f); err != nil {
		return fmt.Errorf("audioflac: encode block: %w", err)
	}
	encoded := w.encoded.Bytes()
	if len(encoded) < 2 || len(encoded) > maxFrameBytes {
		return errors.New("audioflac: encoded frame size out of bounds")
	}
	intent := frameIntent{offset: w.bytes, size: len(encoded)}.marshal()
	if err := writeAt(w.f, intent[:], intentOffset); err != nil {
		return fmt.Errorf("audioflac: record frame intent: %w", err)
	}
	if err := w.appendLocked(encoded); err != nil {
		return fmt.Errorf("audioflac: append frame: %w", err)
	}
	w.stats.add(samples, len(encoded))
	return nil
}

func (w *Writer) appendLocked(p []byte) error {
	n, err := w.f.Write(p)
	w.bytes += int64(n)
	if err == nil && n != len(p) {
		return io.ErrShortWrite
	}
	return err
}

func floatToPCM16(v float32) int32 {
	switch {
	case v != v:
		return 0 // NaN has no equivalent in PCM.
	case v >= 1:
		return 32767
	case v <= -1:
		return -32768
	default:
		return int32(math.Round(float64(v) * 32767))
	}
}
