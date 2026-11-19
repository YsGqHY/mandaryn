package recording

import (
	"testing"

	"vip.oberon.mandaryn/internal/capture"
	"vip.oberon.mandaryn/internal/contract"
)

func TestRecorderAutoNamePolicy(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
		title   string
		want    string
	}{
		{name: "enabled", enabled: true},
		{name: "disabled", want: ""},
		{name: "explicit-title", title: "User title", want: "User title"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := newTestRecorder(t, func(o *Options) {
				o.OpenChannel = func(string) (capture.Channel, error) {
					return newFakeChannel(capture.AudioFormat{SampleRate: 16000, Channels: 1}, fillConst(0.3)), nil
				}
				o.ResolveSource = func(string) (string, string, error) { return "Source app", "application", nil }
			})
			state, err := rec.StartWithAutoName(contract.StartRequest{SourceID: "dev:test", Title: tc.title}, tc.enabled)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _, _ = rec.Stop() })
			want := tc.want
			if tc.enabled {
				want = defaultTitle(rec.opts.Now(), "Source app")
			}
			if got := getSession(t, rec, state.SessionID).Title; got != want {
				t.Fatalf("title = %q, want %q", got, want)
			}
		})
	}
}
