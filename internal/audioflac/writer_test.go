package audioflac

import (
	"bytes"
	"crypto/md5"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/mewkiz/flac"
	"github.com/mewkiz/flac/frame"
	"github.com/mewkiz/flac/meta"
)

func newTestWriter(t *testing.T) *Writer {
	t.Helper()
	w, err := Create(filepath.Join(t.TempDir(), "session", "audio.flac"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.f.Close() })
	return w
}

func requireOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	p, err := os.ReadFile(path)
	requireOK(t, err)
	return p
}

func decode(t *testing.T, data []byte) (*meta.StreamInfo, []*frame.Frame, []int32) {
	t.Helper()
	s, err := flac.New(bytes.NewReader(data))
	requireOK(t, err)
	var frames []*frame.Frame
	var pcm []int32
	for {
		f, err := s.ParseNext()
		if errors.Is(err, io.EOF) {
			break
		}
		requireOK(t, err)
		frames = append(frames, f)
		pcm = append(pcm, f.Subframes[0].Samples...)
	}
	return s.Info, frames, pcm
}

func verifyInfo(t *testing.T, info *meta.StreamInfo, pcm []int32) {
	t.Helper()
	if info.NSamples != uint64(len(pcm)) || info.NChannels != 1 || info.BitsPerSample != 16 ||
		info.SampleRate != 16000 || info.BlockSizeMin != blockSamples || info.BlockSizeMax != blockSamples {
		t.Fatalf("wrong stream info: %+v", info)
	}
	raw := make([]byte, 2*len(pcm))
	for i, v := range pcm {
		binary.LittleEndian.PutUint16(raw[i*2:], uint16(v))
	}
	if want := md5.Sum(raw); info.MD5sum != want {
		t.Fatalf("MD5 = %x, want %x", info.MD5sum, want)
	}
}

func TestWriterRoundTrip(t *testing.T) {
	w := newTestWriter(t)
	input := make([]float32, 2*blockSamples+7)
	for i := range input {
		input[i] = float32(math.Sin(float64(i)*0.08) * 0.5)
	}
	copy(input, []float32{-2, -1, -0.5, 0, 0.5, 1, 2, float32(math.NaN())})
	if data := readFile(t, w.Path()); len(data) != headerBytes {
		t.Fatalf("initial length = %d, want %d", len(data), headerBytes)
	} else if info, frames, _ := decode(t, data); len(frames) != 0 || info.NSamples != 0 || info.MD5sum != [16]byte{} {
		t.Fatalf("initial stream info: %+v, frames=%d", info, len(frames))
	}
	for start := 0; start < len(input); start += 441 {
		requireOK(t, w.Write(input[start:min(start+441, len(input))]))
	}
	requireOK(t, w.Sync())
	if size := len(readFile(t, w.Path())); w.SizeBytes() != int64(size) || size <= headerBytes {
		t.Fatalf("active size = %d, physical file = %d", w.SizeBytes(), size)
	}
	if w.Frames() != int64(len(input)) || w.DataBytes() != int64(len(input)*2) || w.DurationMs() != 200 {
		t.Fatalf("counters: frames=%d PCM bytes=%d duration=%d", w.Frames(), w.DataBytes(), w.DurationMs())
	}
	requireOK(t, w.Seal())
	sealed := readFile(t, w.Path())
	requireOK(t, w.Seal())
	if err := w.Write([]float32{0}); err == nil {
		t.Fatal("write succeeded after seal")
	}
	requireOK(t, w.Close())
	requireOK(t, w.Seal())
	requireOK(t, w.Close())
	if !bytes.Equal(readFile(t, w.Path()), sealed) || w.SizeBytes() != int64(len(sealed)) {
		t.Fatal("repeated seal or close changed the file")
	}
	info, frames, pcm := decode(t, sealed)
	if len(frames) != 3 || frames[2].BlockSize != 7 || len(pcm) != len(input) {
		t.Fatalf("wrong framing: frames=%d samples=%d", len(frames), len(pcm))
	}
	wants := []int32{-32768, -32768, -16384, 0, 16384, 32767, 32767, 0}
	if !slices.Equal(pcm[:len(wants)], wants) {
		t.Fatalf("PCM boundary values = %v, want %v", pcm[:len(wants)], wants)
	}
	for i := len(wants); i < len(input); i++ {
		if want := int32(math.Round(float64(input[i]) * 32767)); pcm[i] != want {
			t.Fatalf("PCM[%d] = %d, want %d", i, pcm[i], want)
		}
	}
	verifyInfo(t, info, pcm)
	if info.FrameSizeMin == 0 || info.FrameSizeMin >= info.FrameSizeMax {
		t.Fatalf("wrong frame size bounds: %+v", info)
	}
	assertRepair(t, w.Path(), int64(len(pcm)))
}

func TestWriterCompression(t *testing.T) {
	w := newTestWriter(t)
	input := make([]float32, 5*blockSamples)
	for i := blockSamples; i < len(input); i++ {
		input[i] = float32(math.Sin(float64(i)*0.07) * 0.8)
	}
	requireOK(t, w.Write(input))
	requireOK(t, w.Close())
	_, frames, _ := decode(t, readFile(t, w.Path()))
	if len(frames) != 5 || frames[0].Subframes[0].Pred != frame.PredConstant || frames[1].Subframes[0].Pred != frame.PredFixed {
		t.Fatal("expected constant and fixed-prediction frames")
	}
	if w.SizeBytes() >= w.DataBytes()/2 {
		t.Fatalf("expected meaningful compression, compressed=%d PCM=%d", w.SizeBytes(), w.DataBytes())
	}
}

func TestWriterEmptyAndSingleSample(t *testing.T) {
	for _, input := range [][]float32{nil, {-1}} {
		w := newTestWriter(t)
		requireOK(t, w.Write(input))
		requireOK(t, w.Close())
		info, _, pcm := decode(t, readFile(t, w.Path()))
		if len(pcm) != len(input) {
			t.Fatalf("samples = %v for input %v", pcm, input)
		}
		verifyInfo(t, info, pcm)
		assertRepair(t, w.Path(), int64(len(pcm)))
	}
}

func TestRepairAbruptClose(t *testing.T) {
	w := newTestWriter(t)
	requireOK(t, w.Write(make([]float32, 2*blockSamples+47)))
	requireOK(t, w.f.Close())
	assertRepair(t, w.Path(), 2*blockSamples)
	info, _, pcm := decode(t, readFile(t, w.Path()))
	verifyInfo(t, info, pcm)
}

func TestRepairTruncatedTail(t *testing.T) {
	w := newTestWriter(t)
	requireOK(t, w.Write(make([]float32, 3*blockSamples)))
	requireOK(t, w.f.Close())
	orig := readFile(t, w.Path())
	intent := parseIntent(orig[intentOffset:headerBytes])
	if intent.offset+int64(intent.size) != int64(len(orig)) {
		t.Fatalf("wrong intent: %+v, file length=%d", intent, len(orig))
	}
	// Exercise every cut, including the first sync byte and first CRC byte.
	for keep := 0; keep < intent.size; keep++ {
		requireOK(t, os.WriteFile(w.Path(), orig[:int(intent.offset)+keep], 0o644))
		assertRepair(t, w.Path(), 2*blockSamples)
		repaired := readFile(t, w.Path())
		if int64(len(repaired)) != intent.offset || !bytes.Equal(repaired[headerBytes:], orig[headerBytes:intent.offset]) {
			t.Fatalf("cut %d changed the verified prefix", keep)
		}
		info, _, pcm := decode(t, repaired)
		verifyInfo(t, info, pcm)
		assertRepair(t, w.Path(), 2*blockSamples)
	}
}

func TestRepairRejectsUnconfirmedOrCorruptTail(t *testing.T) {
	w := newTestWriter(t)
	input := make([]float32, 3*blockSamples)
	for i := range input {
		input[i] = float32(math.Sin(float64(i)*0.07) * 0.8)
	}
	requireOK(t, w.Write(input))
	requireOK(t, w.f.Close())
	orig := readFile(t, w.Path())
	intent := parseIntent(orig[intentOffset:headerBytes])

	t.Run("frame CRC", func(t *testing.T) {
		data := bytes.Clone(orig)
		data[len(data)-1] ^= 1
		assertRepairRejects(t, w.Path(), data)
	})
	t.Run("earlier frame CRC", func(t *testing.T) {
		data := bytes.Clone(orig)
		data[intent.offset-1] ^= 1
		assertRepairRejects(t, w.Path(), data)
	})
	t.Run("full frame that decodes as EOF", func(t *testing.T) {
		data := bytes.Clone(orig)
		r := bytes.NewReader(data[intent.offset:])
		_, err := frame.New(r)
		requireOK(t, err)
		parsed, err := frame.Parse(bytes.NewReader(data[intent.offset:]))
		requireOK(t, err)
		sf := parsed.Subframes[0]
		if sf.Pred != frame.PredFixed {
			t.Fatal("expected fixed predictor for fixture")
		}
		pos := len(data) - r.Len() + 1 + 2*sf.Order + 2
		clear(data[pos:]) // Destroy unary terminators without changing the length.
		_, err = frame.Parse(bytes.NewReader(data[intent.offset:]))
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("fixture did not decode as EOF: %v", err)
		}
		assertRepairRejects(t, w.Path(), data)
	})
	t.Run("torn intent", func(t *testing.T) {
		data := bytes.Clone(orig[:len(orig)-1])
		data[headerBytes-1] ^= 1 // Invalidate the intent's CRC.
		assertRepairRejects(t, w.Path(), data)
	})
	t.Run("wrong byte after last good frame", func(t *testing.T) {
		data := bytes.Clone(orig[:intent.offset+1])
		data[len(data)-1] = 0
		assertRepairRejects(t, w.Path(), data)
	})
}

func TestRepairRejectsSealedDamage(t *testing.T) {
	w := newTestWriter(t)
	requireOK(t, w.Write(make([]float32, 2*blockSamples)))
	requireOK(t, w.Close())
	orig := readFile(t, w.Path())
	assertRepairRejects(t, w.Path(), orig[:len(orig)-1])
	intent := parseIntent(orig[intentOffset:headerBytes])
	assertRepairRejects(t, w.Path(), orig[:intent.offset])
	orig[streamInfoOffset+streamInfoBytes-1] ^= 1
	assertRepairRejects(t, w.Path(), orig)
}

func assertRepair(t *testing.T, path string, want int64) {
	t.Helper()
	got, err := Repair(path)
	requireOK(t, err)
	if got != want {
		t.Fatalf("repaired samples = %d, want %d", got, want)
	}
}

func assertRepairRejects(t *testing.T, path string, data []byte) {
	t.Helper()
	requireOK(t, os.WriteFile(path, data, 0o644))
	if _, err := Repair(path); err == nil {
		t.Fatal("repair unexpectedly accepted damaged data")
	}
	if !bytes.Equal(data, readFile(t, path)) {
		t.Fatal("rejected repair changed the original file")
	}
}

type shortWriter struct{ recordingFile }

func (f shortWriter) Write(p []byte) (int, error) {
	return f.recordingFile.Write(p[:len(p)-1])
}

func TestWriterShortWriteRecovery(t *testing.T) {
	w := newTestWriter(t)
	requireOK(t, w.Write(make([]float32, blockSamples)))
	w.f = shortWriter{w.f}
	if err := w.Write(make([]float32, blockSamples)); !errors.Is(err, io.ErrShortWrite) || !errors.Is(w.Err(), io.ErrShortWrite) {
		t.Fatalf("expected sticky short write, got %v", err)
	}
	if w.Frames() != blockSamples || int64(len(readFile(t, w.Path()))) != w.SizeBytes() {
		t.Fatalf("wrong counters after short write: %d, %d", w.Frames(), w.SizeBytes())
	}
	if err := w.Close(); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("close error = %v", err)
	}
	assertRepair(t, w.Path(), blockSamples)
}

func TestWriterSampleLimit(t *testing.T) {
	w := newTestWriter(t)
	w.stats.samples = maxSamples - 1
	before := w.SizeBytes()
	if err := w.Write([]float32{1, 1}); err == nil || err != w.Err() {
		t.Fatalf("expected sticky sample-limit error, got %v", err)
	}
	if w.Frames() != maxSamples-1 || w.SizeBytes() != before {
		t.Fatal("sample-limit error changed counters")
	}
}
