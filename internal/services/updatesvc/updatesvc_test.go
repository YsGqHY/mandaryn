package updatesvc

import (
	"context"
	"testing"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/outbound"
)

// TestNewUsesEnvManifestURL 环境变量可覆盖清单地址（私有仓库/企业内网场景）。
func TestNewUsesEnvManifestURL(t *testing.T) {
	svc, err := New(Deps{
		Version: "2.0.0",
		LookupEnv: func(k string) (string, bool) {
			if k == EnvManifestURL {
				return "https://mirror.internal/update.json", true
			}
			return "", false
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := svc.ManifestURL(); got != "https://mirror.internal/update.json" {
		t.Fatalf("ManifestURL = %q", got)
	}
}

// TestNewFallsBackToDefault 无覆盖时用默认 GitHub Releases 地址。
func TestNewFallsBackToDefault(t *testing.T) {
	svc, err := New(Deps{Version: "2.0.0", LookupEnv: func(string) (string, bool) { return "", false }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := svc.ManifestURL(); got != DefaultManifestURL {
		t.Fatalf("ManifestURL = %q, want default", got)
	}
}

// TestCheckRespectsOfflineGate 离线模式必须在**发请求之前**拒绝（FR-08）。
//
// 断言手段：把清单地址指向一个必然不可达的 host，并让它失败于"离线"而不是"网络"。
// 若实现顺序写反（先请求再判断），这里会拿到 ERR_DOWNLOAD_NETWORK 而不是 ERR_OFFLINE_ENFORCED。
func TestCheckRespectsOfflineGate(t *testing.T) {
	// 让全局闸门进入离线状态；测试结束恢复。
	outbound.SetUserOffline(func() bool { return true })
	t.Cleanup(func() { outbound.SetUserOffline(nil) })

	svc, err := New(Deps{
		Version:     "2.0.0",
		ManifestURL: "https://127.0.0.1:1/should-not-be-requested",
		LookupEnv:   func(string) (string, bool) { return "", false },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	info, err := svc.Check(context.Background())
	if !apierr.HasCode(err, apierr.CodeOfflineEnforced) {
		t.Fatalf("应返回 ERR_OFFLINE_ENFORCED，实际 %v", err)
	}
	if info.Current != "2.0.0" {
		t.Fatalf("即便离线也应回填当前版本，实际 %q", info.Current)
	}
}

// TestHostOfStripsPathAndQuery outbound_log 只记 host（NFR-03：不存完整 URL）。
func TestHostOfStripsPathAndQuery(t *testing.T) {
	cases := map[string]string{
		"https://api.github.com/repos/o/r/releases/latest": "api.github.com",
		"https://mirror.internal/a/b?token=secret":         "mirror.internal",
		"not a url":                                        "",
		"":                                                 "",
	}
	for in, want := range cases {
		if got := hostOf(in); got != want {
			t.Errorf("hostOf(%q) = %q, want %q", in, got, want)
		}
	}
}
