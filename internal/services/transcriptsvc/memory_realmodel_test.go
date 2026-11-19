//go:build windows && cgo && !nollm && sherpa_models

package transcriptsvc

import (
	"context"
	"encoding/binary"
	"os"
	"strings"
	"testing"
	"unicode"

	"golang.org/x/text/unicode/norm"
	"vip.oberon.mandaryn/internal/asr"
	asrlocal "vip.oberon.mandaryn/internal/asr/local"
)

func TestMemoryRecordingSenseVoice(t *testing.T) {
	dir, refFile := os.Getenv("MANDARYN_TEST_MEMORY_MODEL"), os.Getenv("MANDARYN_TEST_MEMORY_REFERENCE")
	if dir == "" || refFile == "" || os.Getenv("MANDARYN_TEST_MEMORY_WAV") == "" {
		t.Skip("opt-in: provide WAV, SenseVoice model directory and reference")
	}
	ref, err := os.ReadFile(refFile)
	if err != nil {
		t.Fatal(err)
	}
	var expected strings.Builder
	for _, line := range strings.Split(strings.SplitN(string(ref), "---", 2)[0], "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "角色") {
			continue
		}
		if i := strings.IndexAny(line, ":："); i >= 0 {
			expected.WriteString(line[i+len(string([]rune(line[i:])[0])):])
		}
	}
	normalize := func(s string) string {
		return strings.Map(func(r rune) rune {
			if unicode.IsLetter(r) || unicode.IsNumber(r) {
				return unicode.ToLower(r)
			}
			return -1
		}, norm.NFKC.String(s))
	}
	want := []rune(normalize(expected.String()))
	if len(want) == 0 {
		t.Fatal("empty reference dialogue")
	}
	eng, err := asrlocal.New(asrlocal.Spec{Dir: dir, BundleID: "sensevoice", Version: "fixture", Language: "zh", Threads: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	if err := eng.Warm(context.Background()); err != nil {
		t.Fatal(err)
	}
	runMemoryRecordingOnlineLifecycle(t, func(wav []byte) (string, error) {
		pcm := make([]float32, (len(wav)-44)/2)
		for i := range pcm {
			pcm[i] = float32(int16(binary.LittleEndian.Uint16(wav[44+i*2:]))) / 32768
		}
		res, err := eng.Finalize(asr.AudioSegment{Samples: pcm, SampleRate: 16000})
		return res.Text, err
	}, func(t *testing.T, text string) {
		got := []rune(normalize(text))
		dp := make([]int, len(got)+1)
		for j := range dp {
			dp[j] = j
		}
		for i, a := range want {
			prev := dp[0]
			dp[0] = i + 1
			for j, b := range got {
				v := dp[j+1]
				delta := 0
				if a != b {
					delta = 1
				}
				dp[j+1] = min(dp[j+1]+1, dp[j]+1, prev+delta)
				prev = v
			}
		}
		edits := dp[len(got)]
		t.Logf("reference=%d actual=%d edits=%d CER=%.2f%%", len(want), len(got), edits, 100*float64(edits)/float64(len(want)))
		if edits != 0 {
			t.Fatal("normalized transcript differs from approved fixture")
		}
	})
}
