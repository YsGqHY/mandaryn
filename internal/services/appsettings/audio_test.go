package appsettings

import (
	"context"
	"testing"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/storage"
)

func TestAudioLegacySourceAndExplicitClear(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	if err := svc.db().Save(&storage.Preference{Key: "capture.preferredSource", Value: "dev:legacy"}).Error; err != nil {
		t.Fatal(err)
	}
	snap, err := svc.Get(ctx)
	if err != nil || snap.Audio.DefaultSourceID != "dev:legacy" {
		t.Fatalf("legacy source not preserved: %+v, %v", snap.Audio, err)
	}
	audio := snap.Audio
	audio.DefaultSourceID = ""
	audio.AutoName = false
	audio.MaxSpeakers = 3
	audio.SaveFormat = "flac"
	audio.Sensitivity = "noisy"
	if err := svc.SetAudio(ctx, audio); err != nil {
		t.Fatal(err)
	}
	snap, err = svc.Get(ctx)
	if err != nil || snap.Audio != audio {
		t.Fatalf("explicit empty/false lost or hidden fields changed: %+v, %v", snap.Audio, err)
	}
	if err := SaveDefaultSource(ctx, svc.holder, "dev:new"); err != nil {
		t.Fatal(err)
	}
	snap, err = svc.Get(ctx)
	audio.DefaultSourceID = "dev:new"
	if err != nil || snap.Audio != audio {
		t.Fatalf("capture compatibility write changed unrelated fields: %+v, %v", snap.Audio, err)
	}
	var legacy storage.Preference
	if err := svc.db().First(&legacy, "key = ?", "capture.preferredSource").Error; err != nil || legacy.Value != "dev:new" {
		t.Fatalf("legacy mirror mismatch: %+v, %v", legacy, err)
	}
}

func TestAudioInvalidLimitDoesNotChangeSettings(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	before, err := svc.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, count := range []int{-1, 1, 9} {
		audio := before.Audio
		audio.MaxSpeakers = count
		audio.AutoName = false
		audio.DefaultSourceID = "dev:must-not-save"
		if err := svc.SetAudio(ctx, audio); !apierr.HasCode(err, apierr.CodeInvalidArgument) {
			t.Fatalf("limit %d: %v", count, err)
		}
	}
	after, err := svc.Get(ctx)
	if err != nil || after.Audio != before.Audio {
		t.Fatalf("rejected save changed settings: %+v, %v", after.Audio, err)
	}
}
