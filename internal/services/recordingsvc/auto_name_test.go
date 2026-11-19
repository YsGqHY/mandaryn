package recordingsvc

import (
	"context"
	"testing"

	"vip.oberon.mandaryn/internal/capture"
	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/recording"
	"vip.oberon.mandaryn/internal/storage"
)

func TestStartConsumesAutoNameSetting(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(map[bool]string{true: "on", false: "off"}[enabled], func(t *testing.T) {
			svc := newTestService(t, func(o *recording.Options) {
				o.OpenChannel = func(string) (capture.Channel, error) {
					return newFakeChannel(capture.AudioFormat{SampleRate: 16000, Channels: 1}, fillConst(0.3)), nil
				}
				o.ResolveSource = func(string) (string, string, error) { return "Source app", "application", nil }
			}, Emit{})
			if err := svc.holder.Current().GORM.Model(&storage.AppSettings{}).Where("id = ?", 1).Updates(map[string]any{
				"auto_name": enabled, "sensitivity": contract.SensitivityNoisy,
			}).Error; err != nil {
				t.Fatal(err)
			}
			state, err := svc.Start(context.Background(), contract.StartRequest{SourceID: "dev:a"})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _, _ = svc.Stop(context.Background()) })
			var row storage.Session
			if err := svc.holder.Current().GORM.First(&row, "id = ?", state.SessionID).Error; err != nil {
				t.Fatal(err)
			}
			if row.VadPreset != contract.VadPresetNoisyV1 {
				t.Fatalf("preset was not snapshotted: %q", row.VadPreset)
			}
			if err := svc.holder.Current().GORM.Model(&storage.AppSettings{}).Where("id = ?", 1).Update("sensitivity", contract.SensitivityStandard).Error; err != nil {
				t.Fatal(err)
			}
			var after storage.Session
			if err := svc.holder.Current().GORM.First(&after, "id = ?", state.SessionID).Error; err != nil || after.VadPreset != row.VadPreset {
				t.Fatalf("preset changed for existing session: %q, %v", after.VadPreset, err)
			}
			if enabled && row.Title == "" {
				t.Fatal("enabled auto naming did not produce a title")
			}
			if !enabled && row.Title != "" {
				t.Fatalf("disabled auto naming produced title %q", row.Title)
			}
		})
	}
}
