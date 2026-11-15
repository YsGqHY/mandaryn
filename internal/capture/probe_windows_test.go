//go:build windows

package capture

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unsafe"
)

// —— P0 相关测试 ——
//
// 这里验证的是「进程级回环（按应用分路捕获）」这条真实路径，而不是内部实现细节。

// TestProcessLoopbackFormat 固定 PCM 格式的断言。
//
// 这份格式在真机上写错了不会报错，只会得到变调/变速的音频，因此必须逐字段锁死。
func TestProcessLoopbackFormat(t *testing.T) {
	wfx, format, bits, blockAlign := processLoopbackFormat()

	if wfx.WFormatTag != waveFormatPCM {
		t.Errorf("WFormatTag = 0x%04X，期望 WAVE_FORMAT_PCM(0x0001)", wfx.WFormatTag)
	}
	if wfx.NChannels != 2 {
		t.Errorf("NChannels = %d，期望 2", wfx.NChannels)
	}
	if wfx.NSamplesPerSec != 44100 {
		t.Errorf("NSamplesPerSec = %d，期望 44100", wfx.NSamplesPerSec)
	}
	if wfx.WBitsPerSample != 16 {
		t.Errorf("WBitsPerSample = %d，期望 16", wfx.WBitsPerSample)
	}
	if wfx.NBlockAlign != 4 {
		t.Errorf("NBlockAlign = %d，期望 4（2 声道 × 16bit / 8）", wfx.NBlockAlign)
	}
	if wfx.NAvgBytesPerSec != 44100*4 {
		t.Errorf("NAvgBytesPerSec = %d，期望 %d", wfx.NAvgBytesPerSec, 44100*4)
	}
	if wfx.CbSize != 0 {
		t.Errorf("CbSize = %d，期望 0（WAVE_FORMAT_PCM 不扩展）", wfx.CbSize)
	}

	// 对外的 AudioFormat 必须与实际字节流一致，否则上层重采样会算错时间基。
	want := AudioFormat{SampleRate: 44100, Channels: 2, Float32: false}
	if format != want {
		t.Errorf("AudioFormat = %+v，期望 %+v", format, want)
	}
	if bits != 16 {
		t.Errorf("bits = %d，期望 16", bits)
	}
	if blockAlign != 4 {
		t.Errorf("blockAlign = %d，期望 4", blockAlign)
	}
}

// TestActivationParamsLayout 锁定 COM 结构体布局。
//
// 布局错位不会编译报错，只会让系统读到垃圾参数（表现为激活失败或抓到错误的进程），
// 所以用 sizeof + 字段偏移把它钉住。
func TestActivationParamsLayout(t *testing.T) {
	// AUDIOCLIENT_ACTIVATION_PARAMS = enum + DWORD + enum = 12 字节。
	if got := unsafe.Sizeof(audioClientActivationParams{}); got != 12 {
		t.Fatalf("audioClientActivationParams 大小 = %d，期望 12", got)
	}
	// PROPVARIANT 在 x64 上是 24 字节（vt+3×wReserved=8，union 从偏移 8 开始）。
	if got := unsafe.Sizeof(propVariantBlob{}); got != 24 {
		t.Fatalf("propVariantBlob 大小 = %d，期望 24", got)
	}

	var pv propVariantBlob
	base := uintptr(unsafe.Pointer(&pv))
	check := func(name string, got uintptr, want uintptr) {
		if got != want {
			t.Errorf("%s 偏移 = %d，期望 %d", name, got, want)
		}
	}
	check("VT", uintptr(unsafe.Pointer(&pv.VT))-base, 0)
	check("CbSize", uintptr(unsafe.Pointer(&pv.CbSize))-base, 8)
	check("BlobData", uintptr(unsafe.Pointer(&pv.BlobData))-base, 16)
}

// TestCapabilitiesMatchesReality FR-02-4：能力声明必须能被 Open 兑现。
//
// 这是本文件最重要的断言：Capabilities 说支持，Open 就必须真的能开起来；
// 反之说 Count 不支持，Open 就必须拒绝而不是静默退化（FR-02-3）。
func TestCapabilitiesMatchesReality(t *testing.T) {
	caps := Capabilities()

	if caps.Platform != "windows" {
		t.Fatalf("Platform = %q，期望 windows", caps.Platform)
	}
	if !caps.DeviceLoopback {
		t.Error("DeviceLoopback 应为 true")
	}

	t.Logf("能力：ProcessLoopback=%v Reason=%q", caps.ProcessLoopback, caps.Reason)
	if caps.ProcessLoopback && caps.Reason != "" {
		t.Errorf("声称支持进程级捕获时 Reason 应为空，实际 %q", caps.Reason)
	}
	if !caps.ProcessLoopback && caps.Reason == "" {
		t.Error("声称不支持进程级捕获时必须给出 Reason")
	}

	// 用自身进程做目标：它一定存在，能被进程树解析到。
	srcID := AppSourceID(ownExeName(t))
	ch, err := Open(srcID)
	if caps.ProcessLoopback {
		if err != nil {
			t.Fatalf("Capabilities 声称支持进程级捕获，但 Open(%s) 失败：%v", srcID, err)
		}
		defer ch.Close()
		if got := ch.Format(); got.SampleRate != 44100 || got.Channels != 2 {
			t.Errorf("进程回环格式 = %+v，期望 44100Hz/2ch", got)
		}
	} else if err == nil {
		ch.Close()
		t.Fatalf("Capabilities 声称不支持，但 Open(%s) 成功了 —— 能力声明与实现不一致", srcID)
	}
}

// TestOpenAppSourceReconnect app:<exe> 的 ID 必须跨「进程重启」稳定，
// 且源失效要通过 Channel.Err() 或 Open 的错误如实上报（FR-02-2 / FR-02-5）。
func TestOpenAppSourceReconnect(t *testing.T) {
	if ok, note := processLoopbackAvailable(); !ok {
		t.Skipf("本机不支持进程回环，跳过：%s", note)
	}

	exe := ownExeName(t)
	srcID := AppSourceID(exe)

	// 源 ID 只由 exe 名决定，不含 PID —— 这正是「进程重启后仍能续抓」的前提。
	lower := strings.ToLower(exe)
	if srcID != "app:"+lower {
		t.Fatalf("源 ID = %q，期望 app:%s", srcID, lower)
	}

	for round := 1; round <= 2; round++ {
		ch, err := Open(srcID)
		if err != nil {
			t.Fatalf("第 %d 次打开 %s 失败：%v", round, srcID, err)
		}
		// 模拟「关闭后重新连接」：每次都是全新的流，等价于目标重启后的续抓。
		if err := ch.Close(); err != nil {
			t.Fatalf("第 %d 次关闭失败：%v", round, err)
		}
	}

	// 不存在的 exe：必须报「源不存在」，而不是含糊的成功。
	if _, err := Open(AppSourceID("definitely-not-running-xyz.exe")); err == nil {
		t.Fatal("对不存在的应用源 Open 应失败")
	}

	// 关闭后所有 ReadPCM 最终都要返回错误（EOF），让上层读循环能退出（Channel 契约）。
	// 注意不能只读一次：队列里可能还有已缓冲的静音包，契约是「关闭**且无数据**时」才 EOF。
	ch, err := Open(srcID)
	if err != nil {
		t.Fatalf("打开 %s 失败：%v", srcID, err)
	}
	_ = ch.Close()
	dst := make([]float32, 4096)
	done := make(chan error, 1)
	go func() {
		for i := 0; i < 1000; i++ {
			if _, rerr := ch.ReadPCM(dst); rerr != nil {
				done <- rerr
				return
			}
		}
		done <- nil // 读满 1000 次仍无错误
	}()
	select {
	case rerr := <-done:
		if rerr == nil {
			t.Fatal("关闭后持续 ReadPCM 始终未返回错误，Channel 契约被破坏")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("关闭后 ReadPCM 仍阻塞，Channel 契约被破坏")
	}
}

// ownExeName 返回当前测试进程的可执行文件名（小写，与 AppSourceID 口径一致）。
func ownExeName(t *testing.T) string {
	t.Helper()
	path, err := os.Executable()
	if err != nil {
		t.Fatalf("取可执行文件路径失败：%v", err)
	}
	return strings.ToLower(filepath.Base(path))
}
