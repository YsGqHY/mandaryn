package app

import (
	"context"
	"path/filepath"
	"testing"

	"vip.oberon.mandaryn/internal/services/appsettings"
	"vip.oberon.mandaryn/internal/storage"
)

func TestRecordingCueSettingsPersistence(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "mandaryn.db")
	open := func() *storage.DB {
		t.Helper()
		db, err := storage.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db
	}
	db := open()
	holder := storage.NewHolder(db)
	settings := appsettings.New(holder, nil, nil, appsettings.WithActiveSession(func() bool { return true }))
	check := func(want bool) {
		t.Helper()
		snap, err := settings.Get(ctx)
		if err != nil || snap.RecordingCues != want {
			t.Fatalf("snapshot recordingCues=%t, want %t; error=%v", snap.RecordingCues, want, err)
		}
		if got := appsettings.RecordingCuesEnabled(ctx, holder); got != want {
			t.Fatalf("worker checks recordingCues=%t, want %t", got, want)
		}
	}
	check(true)
	// Simulate an existing database that predates the setting.
	if err := db.GORM.Exec("ALTER TABLE app_settings DROP COLUMN recording_cues").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = open()
	holder.Swap(db)
	check(true)
	// Active recording is not a restriction, unlike live transcription.
	if err := settings.SetRecordingCues(ctx, false); err != nil {
		t.Fatal(err)
	}
	check(false)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	holder.Swap(open())
	check(false)
	if err := settings.SetRecordingCues(ctx, true); err != nil {
		t.Fatal(err)
	}
	check(true)
}
