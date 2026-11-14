package recording

import (
	"errors"
	"testing"

	"vip.oberon.mandaryn/internal/contract"
)

const winEndpointSourceID = "dev:{0.0.0.00000000}.{12931ccb-a154-447c-8054-f49efe5854bb}"

// TestDefaultResolveSourceStripsBraces：拿不到友好名时的短标识不能带 GUID 的花括号（G-E2E-02）。
func TestDefaultResolveSourceStripsBraces(t *testing.T) {
	name, kind, err := defaultResolveSource(winEndpointSourceID)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if kind != contract.SourceKindDevice {
		t.Fatalf("kind = %q, want device", kind)
	}
	if name != "音频设备 fe5854bb" {
		t.Fatalf("name = %q, want 音频设备 fe5854bb", name)
	}
}

// TestDefaultResolveSourceShortID：短 ID 原样作为展示名（测试与非 Windows 平台常见）。
func TestDefaultResolveSourceShortID(t *testing.T) {
	name, _, err := defaultResolveSource("dev:a")
	if err != nil || name != "a" {
		t.Fatalf("name = %q, err = %v, want a", name, err)
	}
}

// TestResolveSourceWithUsesDisplayName：设备源优先用系统友好名。
func TestResolveSourceWithUsesDisplayName(t *testing.T) {
	resolve := ResolveSourceWith(func(string) (contract.Source, error) {
		return contract.Source{DisplayName: "扬声器 (HECATE G2000)"}, nil
	})
	name, kind, err := resolve(winEndpointSourceID)
	if err != nil || kind != contract.SourceKindDevice || name != "扬声器 (HECATE G2000)" {
		t.Fatalf("got (%q, %q, %v), want 扬声器 (HECATE G2000)", name, kind, err)
	}
}

// TestResolveSourceWithFallsBack：探测失败或空名都回落到短标识，且不阻止开录。
func TestResolveSourceWithFallsBack(t *testing.T) {
	for _, lookup := range []func(string) (contract.Source, error){
		func(string) (contract.Source, error) { return contract.Source{}, errors.New("device gone") },
		func(string) (contract.Source, error) { return contract.Source{DisplayName: "  "}, nil },
	} {
		name, _, err := ResolveSourceWith(lookup)(winEndpointSourceID)
		if err != nil || name != "音频设备 fe5854bb" {
			t.Fatalf("got (%q, %v), want 音频设备 fe5854bb", name, err)
		}
	}
}

// TestResolveSourceWithSkipsApplications：应用源不走设备探测，展示名保持不变。
func TestResolveSourceWithSkipsApplications(t *testing.T) {
	called := false
	resolve := ResolveSourceWith(func(string) (contract.Source, error) {
		called = true
		return contract.Source{DisplayName: "x"}, nil
	})
	name, kind, err := resolve("app:cloudmusic.exe")
	if err != nil || kind != contract.SourceKindApplication || name != "cloudmusic.exe" {
		t.Fatalf("got (%q, %q, %v)", name, kind, err)
	}
	if called {
		t.Fatal("应用源不应触发设备探测")
	}
}
