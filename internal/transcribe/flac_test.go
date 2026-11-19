package transcribe

import (
	"bytes"
	"context"
	"testing"

	"github.com/mewkiz/flac"
	"github.com/mewkiz/flac/frame"
	"github.com/mewkiz/flac/meta"

	"vip.oberon.mandaryn/internal/utils/audioframes"
)

type flacFixture struct{ *bytes.Reader }

func (flacFixture) Close() error { return nil }

func encodeTestFLAC(t *testing.T, samples []float32) []byte {
	t.Helper()
	const size = 512
	var b bytes.Buffer
	encoder, err := flac.NewEncoder(&b, &meta.StreamInfo{
		BlockSizeMin: size, BlockSizeMax: size, SampleRate: 16000,
		NChannels: 1, BitsPerSample: 16, NSamples: uint64(len(samples)),
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < len(samples); i += size {
		pcm := samples[i:min(i+size, len(samples))]
		data := make([]int32, len(pcm))
		for j, x := range pcm {
			data[j] = int32(x * 32767)
		}
		if err := encoder.WriteFrame(&frame.Frame{
			Header:    frame.Header{HasFixedBlockSize: true, BlockSize: uint16(len(pcm)), SampleRate: 16000, Channels: frame.ChannelsMono, BitsPerSample: 16},
			Subframes: []*frame.Subframe{{SubHeader: frame.SubHeader{Pred: frame.PredVerbatim}, Samples: data, NSamples: len(data)}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := encoder.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestFLACFrameReads(t *testing.T) {
	pcm := make([]float32, 1199) // includes a short final fixed-block frame
	for i := range pcm {
		pcm[i] = float32(i%251-125) / 256
	}
	encoded := encodeTestFLAC(t, pcm)
	src, err := audioframes.OpenFrameSource(flacFixture{bytes.NewReader(encoded)})
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if src.TotalFrames() != int64(len(pcm)) || src.SampleRate() != 16000 {
		t.Fatalf("frames=%d rate=%d", src.TotalFrames(), src.SampleRate())
	}
	for _, rng := range [][2]int64{{500, 40}, {900, 400}, {25, 33}} {
		start, n := rng[0], rng[1]
		got, err := src.ReadFrames(start, n)
		if err != nil || int64(len(got)) != min(n, int64(len(pcm))-start) {
			t.Fatalf("read %v: len=%d err=%v", rng, len(got), err)
		}
		for j, x := range got {
			want := float32(int32(pcm[start+int64(j)]*32767)) / 32768
			if x != want {
				t.Fatalf("read %v sample %d: %g want %g", rng, j, x, want)
			}
		}
	}
	broken, err := audioframes.OpenFrameSource(flacFixture{bytes.NewReader(encoded[:len(encoded)-1])})
	if err != nil {
		t.Fatal(err)
	}
	defer broken.Close()
	if _, err := broken.ReadFrames(1024, 175); err == nil {
		t.Fatal("truncated frame was accepted")
	}
}

func TestRunReadsSealedFLAC(t *testing.T) {
	st := newFakeStore(SessionRow{ID: "s_flac", AudioPath: "audio.flac", DurationMs: 2300})
	encoded := encodeTestFLAC(t, concat(tone(1000, 16000), silence(500, 16000), tone(800, 16000)))
	eng := newFakeEngine()
	r := mustRunner(t, st, eng, func(context.Context, SessionRow) (AudioSource, error) {
		return flacFixture{bytes.NewReader(encoded)}, nil
	}, Options{})
	res, err := r.Run(context.Background(), TaskRequest{SessionID: st.session.ID, Trigger: "manual"})
	if err != nil || res.Outcome != OutcomeDone || eng.Calls() != 2 {
		t.Fatalf("FLAC run outcome=%q, calls=%d, taskErr=%v, err=%v", res.Outcome, eng.Calls(), res.Err, err)
	}
}
