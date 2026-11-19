package capturesvc

import (
	"context"
	"path/filepath"
	"testing"

	"vip.oberon.mandaryn/internal/services/appsettings"
	"vip.oberon.mandaryn/internal/storage"
)

func TestPreferredSourceUsesAudioSetting(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	holder := storage.NewHolder(db)
	captureSvc := New(holder, nil)
	settingsSvc := appsettings.New(holder, nil, nil)
	ctx := context.Background()
	if err := db.GORM.Save(&storage.Preference{Key: prefKeyPreferredSource, Value: "dev:legacy"}).Error; err != nil {
		t.Fatal(err)
	}
	if got, err := captureSvc.GetPreferredSource(ctx); err != nil || got != "dev:legacy" {
		t.Fatalf("legacy source = %q, %v", got, err)
	}
	snap, err := settingsSvc.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	snap.Audio.DefaultSourceID = "dev:configured"
	if err := settingsSvc.SetAudio(ctx, snap.Audio); err != nil {
		t.Fatal(err)
	}
	if got, err := captureSvc.GetPreferredSource(ctx); err != nil || got != "dev:configured" {
		t.Fatalf("settings source = %q, %v", got, err)
	}
	snap.Audio.DefaultSourceID = ""
	if err := settingsSvc.SetAudio(ctx, snap.Audio); err != nil {
		t.Fatal(err)
	}
	if got, err := captureSvc.GetPreferredSource(ctx); err != nil || got != "" {
		t.Fatalf("explicitly cleared source = %q, %v", got, err)
	}
}
