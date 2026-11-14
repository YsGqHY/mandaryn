package capture

import (
	"errors"
	"testing"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
)

// TestParseSourceID 覆盖源 ID 的编码/解码规则（ARCH §4.1.1）。
//
// 这段逻辑不分平台，因此放在无 build tag 的测试里，
// 保证 darwin/linux 上也能验证 ID 契约。
func TestParseSourceID(t *testing.T) {
	cases := []struct {
		name      string
		input     string
		wantKind  string
		wantValue string
		wantErr   bool
	}{
		{
			name:      "设备源",
			input:     "dev:{0.0.0.00000000}.{102dd111-8739-4bba-a506-02182d864fee}",
			wantKind:  contract.SourceKindDevice,
			wantValue: "{0.0.0.00000000}.{102dd111-8739-4bba-a506-02182d864fee}",
		},
		{
			name:      "应用源统一小写",
			input:     "app:KOOK.exe",
			wantKind:  contract.SourceKindApplication,
			wantValue: "kook.exe",
		},
		{
			name:    "空串",
			input:   "",
			wantErr: true,
		},
		{
			name:    "缺少前缀",
			input:   "kook.exe",
			wantErr: true,
		},
		{
			name:    "设备前缀但无内容",
			input:   "dev:",
			wantErr: true,
		},
		{
			name:    "应用前缀但无内容",
			input:   "app:   ",
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind, value, err := ParseSourceID(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("期望报错，实际 kind=%q value=%q", kind, value)
				}
				// 参数非法必须是 ERR_INVALID_ARGUMENT，前端据此提示「源无效」。
				if !apierr.HasCode(err, apierr.CodeInvalidArgument) {
					t.Fatalf("期望错误码 %s，实际 %v", apierr.CodeInvalidArgument, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("未预期的错误：%v", err)
			}
			if kind != tc.wantKind || value != tc.wantValue {
				t.Fatalf("解析结果不符：kind=%q value=%q，期望 kind=%q value=%q",
					kind, value, tc.wantKind, tc.wantValue)
			}
		})
	}
}

// TestSourceIDRoundTrip 验证构造出来的 ID 能被解析回同样的信息。
func TestSourceIDRoundTrip(t *testing.T) {
	deviceID := "{0.0.0.00000000}.{cbf4812a-6545-4083-893f-a18855ba35ef}"
	if got := DeviceSourceID(deviceID); got != "dev:"+deviceID {
		t.Fatalf("设备源 ID 构造错误：%s", got)
	}
	kind, value, err := ParseSourceID(DeviceSourceID(deviceID))
	if err != nil {
		t.Fatalf("设备源 ID 回解析失败：%v", err)
	}
	if kind != contract.SourceKindDevice || value != deviceID {
		t.Fatalf("设备源 ID 回解析不符：kind=%q value=%q", kind, value)
	}

	// exe 名大小写不同必须得到同一个 ID（跨次枚举稳定，重连靠它）。
	if AppSourceID("KOOK.exe") != AppSourceID("kook.exe") {
		t.Fatal("应用源 ID 未做小写归一")
	}
	kind, value, err = ParseSourceID(AppSourceID("WeChat.exe"))
	if err != nil {
		t.Fatalf("应用源 ID 回解析失败：%v", err)
	}
	if kind != contract.SourceKindApplication || value != "wechat.exe" {
		t.Fatalf("应用源 ID 回解析不符：kind=%q value=%q", kind, value)
	}
}

// TestProbeRejectsBadID 保证 Probe 在参数非法时返回错误码而不是 panic。
func TestProbeRejectsBadID(t *testing.T) {
	_, err := Probe("这不是一个源 ID")
	if err == nil {
		t.Fatal("期望报错")
	}
	if !apierr.HasCode(err, apierr.CodeInvalidArgument) {
		t.Fatalf("期望 %s，实际 %v", apierr.CodeInvalidArgument, err)
	}
}

// TestSourceUnavailableError 校验内部错误构造器带上了错误码与原因文案。
func TestSourceUnavailableError(t *testing.T) {
	err := sourceUnavailable("测试原因")
	var target *apierr.Err
	if !errors.As(err, &target) {
		t.Fatalf("期望 *apierr.Err，实际 %T", err)
	}
	if target.Code != apierr.CodeCaptureUnavailable {
		t.Fatalf("错误码不符：%s", target.Code)
	}
	if target.Detail != "测试原因" {
		t.Fatalf("Detail 不符：%q", target.Detail)
	}
}
