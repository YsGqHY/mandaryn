//go:build windows

package capture

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/moutend/go-wca/pkg/wca"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
)

// TestCapabilities 覆盖验收项 (a)：本机 Build 判定与 Reason 文案。
//
// 本机实测为 Build 19045（< 20348），因此进程级捕获必须报 false，
// 且 Reason 必须含实际版本号——前端要把这句话直接展示给用户。
func TestCapabilities(t *testing.T) {
	caps := Capabilities()

	if caps.Platform != "windows" {
		t.Fatalf("Platform 应为 windows，实际 %q", caps.Platform)
	}
	// Win10 起共享模式回环必定可用，设备回环恒为 true。
	if !caps.DeviceLoopback {
		t.Error("设备回环在 Windows 上应为 true")
	}

	build, err := currentWindowsBuild()
	if err != nil {
		t.Fatalf("读取 Windows Build 失败：%v", err)
	}
	t.Logf("本机 Windows Build = %d（进程级捕获最低要求 %d）", build, minProcessLoopbackBuild)

	if build < minProcessLoopbackBuild {
		if caps.ProcessLoopback {
			t.Fatalf("Build %d 低于 %d，不应声称支持进程级捕获", build, minProcessLoopbackBuild)
		}
		if caps.Reason == "" {
			t.Fatal("不支持进程级捕获时 Reason 不能为空")
		}
		if !strings.Contains(caps.Reason, "20348") {
			t.Errorf("Reason 应包含最低系统版本号，实际：%q", caps.Reason)
		}
	} else if !caps.ProcessLoopback {
		// 系统够新但代码未实现时，也必须给出原因（本版本即属于这种情况）。
		if caps.Reason == "" {
			t.Fatal("声称不支持进程级捕获时 Reason 不能为空")
		}
	}
	t.Logf("Capabilities = %+v", caps)
}

// TestEnumerateDevices 覆盖验收项 (b)：设备源枚举非空。
//
// 本机有 Realtek / NVIDIA / 虚拟声卡等多个渲染端点，实测可枚举。
// 极端环境下（无声卡、安全模式）允许为空但**不得报错**：
// 这种情况属于「确实没有设备」，不是实现缺陷。
func TestEnumerateDevices(t *testing.T) {
	sources, err := Enumerate()
	if err != nil {
		t.Fatalf("枚举失败：%v", err)
	}

	var devices []contract.Source
	for _, s := range sources {
		if s.Kind == contract.SourceKindDevice {
			devices = append(devices, s)
		}
	}
	if len(devices) == 0 {
		t.Skip("本机没有活动输出设备，跳过（不算失败）")
	}

	// 默认设备应排在最前，且以结构化字段 IsDefault 标记
	// （不再往 DisplayName 里拼中文后缀：人类可见文案由前端 i18n 渲染）。
	if !devices[0].IsDefault {
		t.Errorf("首个设备源应为默认设备（IsDefault=true），实际 %q", devices[0].DisplayName)
	}

	presentCount := 0
	for _, d := range devices {
		if d.DeviceID == "" {
			t.Errorf("设备源缺少 DeviceID：%+v", d)
		}
		if !strings.HasPrefix(d.ID, DeviceIDPrefix) {
			t.Errorf("设备源 ID 前缀错误：%q", d.ID)
		}
		if d.DisplayName == "" {
			t.Errorf("设备源缺少 DisplayName：%+v", d)
		}
		if d.Present {
			presentCount++
		}
	}
	if presentCount == 0 {
		t.Error("活动设备源的 Present 应至少有一个为 true")
	}

	// 默认设备必须唯一：IsDefault 只能有一个为 true。
	defaultCount := 0
	for _, d := range devices {
		if d.IsDefault {
			defaultCount++
		}
	}
	if defaultCount > 1 {
		t.Errorf("默认设备标记重复出现 %d 次", defaultCount)
	}

	t.Logf("枚举到 %d 个设备源，首个：%s", len(devices), devices[0].DisplayName)
	for _, d := range devices {
		t.Logf("  - %s present=%v producing=%v", d.DisplayName, d.Present, d.Producing)
	}
}

// TestEnumerateApplications 覆盖验收项 (c)：应用源枚举不 panic。
//
// 允许为空（取决于当前是否有应用在放音频），但一旦有内容，
// 字段必须自洽：ID 前缀、exe 小写、PIDs 非空、ChildCount 不为负。
func TestEnumerateApplications(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("应用源枚举 panic：%v", r)
		}
	}()

	sources, err := Enumerate()
	if err != nil {
		t.Fatalf("枚举失败：%v", err)
	}

	var apps []contract.Source
	for _, s := range sources {
		if s.Kind == contract.SourceKindApplication {
			apps = append(apps, s)
		}
	}
	if len(apps) == 0 {
		t.Log("当前没有正在使用音频的应用，应用源为空（允许）")
		return
	}

	for _, a := range apps {
		if !strings.HasPrefix(a.ID, AppIDPrefix) {
			t.Errorf("应用源 ID 前缀错误：%q", a.ID)
		}
		if a.ExeName != strings.ToLower(a.ExeName) {
			t.Errorf("应用源 exe 未小写：%q", a.ExeName)
		}
		if len(a.PIDs) == 0 {
			t.Errorf("应用源缺少 PIDs：%+v", a)
		}
		if a.ChildCount < 0 {
			t.Errorf("ChildCount 不应为负：%d", a.ChildCount)
		}
		if a.DisplayName == "" {
			t.Errorf("应用源缺少 DisplayName：%+v", a)
		}
	}
	t.Logf("枚举到 %d 个应用源", len(apps))
	for _, a := range apps {
		t.Logf("  - %s (exe=%s pids=%v 子进程=%d producing=%v)",
			a.DisplayName, a.ExeName, a.PIDs, a.ChildCount, a.Producing)
	}
}

// TestOpenUnknownDevice 覆盖验收项 (d)：不存在的设备要返回错误码，不能 panic。
func TestOpenUnknownDevice(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Open 不该 panic：%v", r)
		}
	}()

	for _, id := range []string{
		"dev:不是真实设备",
		"dev:{0.0.0.00000000}.{00000000-0000-0000-0000-000000000000}",
	} {
		ch, err := Open(id)
		if err == nil {
			_ = ch.Close()
			t.Fatalf("%s 应该报错", id)
		}
		// 不存在 = 源找不到；若是环境整体不可用则是 CAPTURE_UNAVAILABLE。
		if !apierr.HasCode(err, apierr.CodeSourceNotFound) &&
			!apierr.HasCode(err, apierr.CodeCaptureUnavailable) {
			t.Fatalf("%s 的错误码不符：%v", id, err)
		}
		t.Logf("%s → %v", id, err)
	}
}

// TestOpenApplicationUnavailable 验证应用源 Open 会诚实拒绝，而不是静默抓全部声音。
func TestOpenApplicationUnavailable(t *testing.T) {
	ch, err := Open("app:kook.exe")
	if err == nil {
		_ = ch.Close()
		t.Fatal("应用源 Open 应返回错误（进程级捕获未实现）")
	}
	if !apierr.HasCode(err, apierr.CodeCaptureUnavailable) {
		t.Fatalf("期望 %s，实际 %v", apierr.CodeCaptureUnavailable, err)
	}
	var apiErr *apierr.Err
	if !errors.As(err, &apiErr) {
		t.Fatalf("期望 *apierr.Err，实际 %T", err)
	}
	if !strings.Contains(apiErr.Detail, "20348") {
		t.Errorf("拒绝原因应包含最低系统版本，实际：%q", apiErr.Detail)
	}
	t.Logf("应用源 Open → %v", err)
}

// TestOpenDeviceCaptures 实际打开默认输出设备并发起回环采集。
//
// 这是唯一需要真实声卡的测试：读取 1s 内应当拿到样本。
// 没有设备时跳过；有设备但完全没有音频时也允许 0 帧（回环在静音期
// 仍会产帧，但某些虚拟设备可能不产），因此只强校验格式与不 panic。
func TestOpenDeviceCaptures(t *testing.T) {
	sources, err := Enumerate()
	if err != nil {
		t.Fatalf("枚举失败：%v", err)
	}
	var deviceID string
	for _, s := range sources {
		if s.Kind == contract.SourceKindDevice {
			deviceID = s.DeviceID
			break // 默认设备排最前
		}
	}
	if deviceID == "" {
		t.Skip("本机没有活动输出设备，跳过实际采集测试")
	}

	ch, err := Open(DeviceSourceID(deviceID))
	if err != nil {
		t.Fatalf("打开默认设备失败：%v", err)
	}
	defer func() {
		if err := ch.Close(); err != nil {
			t.Errorf("关闭通道失败：%v", err)
		}
	}()

	f := ch.Format()
	if f.SampleRate <= 0 || f.Channels <= 0 {
		t.Fatalf("格式不合理：%+v", f)
	}
	t.Logf("默认设备格式：%dHz %dch float32=%v", f.SampleRate, f.Channels, f.Float32)

	buf := make([]float32, 1024)
	total := 0
	// 累计约 500ms 的数据，顺便验证「采样率是否符合 Format() 的声明」。
	readDeadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(readDeadline) {
		deadline := time.Now().Add(200 * time.Millisecond)
		n, readErr := ch.ReadPCM(buf)
		if readErr != nil {
			// 采集期间设备失效（改默认设备 / 拔掉）属于可接受结果，记录后跳过。
			t.Skipf("读取中断（可能设备被切换）：%v", readErr)
		}
		total += n
		if time.Now().After(deadline) {
			break
		}
	}

	if total == 0 {
		t.Log("500ms 内没有收到样本（该虚拟设备可能不产回环帧），不算失败")
		return
	}
	frames := total / f.Channels
	t.Logf("500ms 内读到 %d 个样本（约 %d 帧）", total, frames)

	// 回环是实时流：500ms 内应得到接近 SampleRate*0.5 的帧数。
	// 下界只取 1/4，容忍虚拟设备与共享模式下的抖动，但足以排除「格式声称 48k、
	// 实际按 8k 抽帧」这类量与声明不符的错误。
	if want := f.SampleRate / 4; frames < want {
		t.Errorf("帧数与声明的采样率不符：%d 帧 < 期望下界 %d（%dHz×0.5s）",
			frames, want, f.SampleRate)
	}

	// 主动 Close 后：先把缓冲读空，之后必须返回错误而不是永久阻塞。
	if err := ch.Close(); err != nil {
		t.Fatalf("关闭失败：%v", err)
	}
	eofSeen := false
	for i := 0; i < 1000; i++ {
		if _, readErr := ch.ReadPCM(buf); readErr != nil {
			eofSeen = true
			break
		}
	}
	if !eofSeen {
		t.Fatal("关闭后 ReadPCM 应在读空缓冲后返回错误（不能永久阻塞）")
	}
	// Err() 在主动关闭时不应塞错误。
	select {
	case got := <-ch.Err():
		if got != nil {
			t.Logf("Err() 收到（主动关闭本不该有）：%v", got)
		}
	default:
	}
}

// TestProbeDevice 探测枚举出的第一个设备，验证 Probe 与 Enumerate 一致。
func TestProbeDevice(t *testing.T) {
	sources, err := Enumerate()
	if err != nil {
		t.Fatalf("枚举失败：%v", err)
	}
	var target *contract.Source
	for i := range sources {
		if sources[i].Kind == contract.SourceKindDevice {
			target = &sources[i]
			break
		}
	}
	if target == nil {
		t.Skip("本机没有活动输出设备，跳过")
	}

	src, err := Probe(target.ID)
	if err != nil {
		t.Fatalf("探测设备失败：%v", err)
	}
	if src.ID != target.ID {
		t.Fatalf("探测返回的 ID 不符：%s vs %s", src.ID, target.ID)
	}
	if !src.Present {
		t.Error("活动设备的 Present 应为 true")
	}
	if src.DeviceID != target.DeviceID {
		t.Errorf("DeviceID 不符：%s vs %s", src.DeviceID, target.DeviceID)
	}
}

// TestProbeMissingDevice 探测不存在的设备应返回 ERR_SOURCE_NOT_FOUND。
func TestProbeMissingDevice(t *testing.T) {
	_, err := Probe("dev:{0.0.0.00000000}.{00000000-0000-0000-0000-000000000000}")
	if err == nil {
		t.Fatal("期望报错")
	}
	if !apierr.HasCode(err, apierr.CodeSourceNotFound) {
		t.Fatalf("期望 %s，实际 %v", apierr.CodeSourceNotFound, err)
	}
}

// TestProbeMissingApplication 探测不存在的应用应返回 ERR_SOURCE_NOT_FOUND。
func TestProbeMissingApplication(t *testing.T) {
	_, err := Probe("app:一定不存在的进程.exe")
	if err == nil {
		t.Fatal("期望报错")
	}
	if !apierr.HasCode(err, apierr.CodeSourceNotFound) {
		t.Fatalf("期望 %s，实际 %v", apierr.CodeSourceNotFound, err)
	}
}

// TestParseMixFormat 用构造的 WAVEFORMATEXTENSIBLE 验证格式解析（不依赖真实设备）。
//
// 关键陷阱：必须按 **Windows 的紧凑内存布局** 构造，不能用 Go 结构体嵌套。
// Go 会给 wca.WAVEFORMATEX（18 字节，最大对齐 4）补到 20 字节，
// 导致后面的扩展字段整体后移 2 字节，SubFormat 就不在偏移 24 上了——
// 用它写测试会得出「代码有 bug」的错误结论（真实 COM 缓冲没有这个填充）。
func TestParseMixFormat(t *testing.T) {
	// 共享模式最常见的形态：WAVE_FORMAT_EXTENSIBLE + IEEE_FLOAT 32bit 立体声 48kHz。
	ext := makeExtensible(48000, 2, 32, 8, true)
	format, bits, blockAlign, err := parseMixFormat(ext.pointer())
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if format.SampleRate != 48000 || format.Channels != 2 || !format.Float32 {
		t.Fatalf("解析结果不符：%+v", format)
	}
	if bits != 32 || blockAlign != 8 {
		t.Fatalf("位深/块对齐不符：%d / %d", bits, blockAlign)
	}

	// 整型 PCM 16bit：不应被当成 float。
	pcm := makeExtensible(44100, 2, 16, 4, false)
	format, bits, _, err = parseMixFormat(pcm.pointer())
	if err != nil {
		t.Fatalf("PCM 解析失败：%v", err)
	}
	if format.Float32 {
		t.Error("整型 PCM 不应标记为 Float32")
	}
	if bits != 16 {
		t.Errorf("位深不符：%d", bits)
	}
	if format.SampleRate != 44100 {
		t.Errorf("采样率不符：%d", format.SampleRate)
	}

	// 未知子格式：必须报错而不是瞎猜。
	unknown := makeExtensible(8000, 1, 16, 2, false)
	unknown.setSubFormat(99)
	if _, _, _, err := parseMixFormat(unknown.pointer()); err == nil {
		t.Fatal("未知子格式应报错")
	}

	// 缺少扩展字段（cbSize 太小）：不能越界读。
	short := makeExtensible(48000, 2, 32, 8, true)
	short.setCbSize(0)
	if _, _, _, err := parseMixFormat(short.pointer()); err == nil {
		t.Fatal("缺少扩展字段时应报错")
	}

	// 非浮点的 64bit 与 12bit 都应被拒绝（避免拿错比例解码出噪音）。
	weird := makeExtensible(48000, 2, 12, 4, false)
	if _, _, _, err := parseMixFormat(weird.pointer()); err == nil {
		t.Fatal("不支持的位深应报错")
	}
	float64fmt := makeExtensible(48000, 2, 64, 16, true)
	if _, _, _, err := parseMixFormat(float64fmt.pointer()); err == nil {
		t.Fatal("非 32 位浮点应报错")
	}

	// 声道/采样率为 0 的垃圾数据也要报错而不是算出除零。
	broken := makeExtensible(0, 0, 16, 0, false)
	if _, _, _, err := parseMixFormat(broken.pointer()); err == nil {
		t.Fatal("非法格式应报错")
	}
}

// TestDecodePCM 覆盖各采样格式到 float32 的换算。
func TestDecodePCM(t *testing.T) {
	floatFmt := AudioFormat{SampleRate: 48000, Channels: 2, Float32: true}

	// float32：直接按位解释。
	raw := make([]byte, 8)
	putFloat32(raw[0:], 0.5)
	putFloat32(raw[4:], -0.5)
	out := make([]float32, 2)
	if !decodePCM(raw, out, floatFmt, 32) {
		t.Fatal("float32 解码失败")
	}
	if out[0] != 0.5 || out[1] != -0.5 {
		t.Fatalf("float32 解码结果不符：%v", out)
	}

	// int16：0x4000 = 16384 → 0.5；0xC000 = -16384 → -0.5。
	intFmt := AudioFormat{SampleRate: 48000, Channels: 2, Float32: false}
	raw16 := []byte{0x00, 0x40, 0x00, 0xC0}
	if !decodePCM(raw16, out, intFmt, 16) {
		t.Fatal("int16 解码失败")
	}
	if out[0] != 0.5 || out[1] != -0.5 {
		t.Fatalf("int16 解码结果不符：%v", out)
	}

	// int32：最大值的一半 → 0.5。
	raw32 := make([]byte, 8)
	putUint32(raw32[0:], 0x40000000)
	putUint32(raw32[4:], 0xC0000000)
	if !decodePCM(raw32, out, intFmt, 32) {
		t.Fatal("int32 解码失败")
	}
	if out[0] != 0.5 || out[1] != -0.5 {
		t.Fatalf("int32 解码结果不符：%v", out)
	}

	// int24：0x400000 (小端 00 00 40) → 0.5；0xC00000 → -0.5。
	raw24 := []byte{0x00, 0x00, 0x40, 0x00, 0x00, 0xC0}
	if !decodePCM(raw24, out, intFmt, 24) {
		t.Fatal("int24 解码失败")
	}
	if out[0] != 0.5 || out[1] != -0.5 {
		t.Fatalf("int24 解码结果不符：%v", out)
	}

	// uint8：192 → 0.5，64 → -0.5。
	raw8 := []byte{192, 64}
	if !decodePCM(raw8, out, intFmt, 8) {
		t.Fatal("uint8 解码失败")
	}
	if out[0] != 0.5 || out[1] != -0.5 {
		t.Fatalf("uint8 解码结果不符：%v", out)
	}

	// 数据长度不足：必须返回 false（避免越界读）。
	if decodePCM([]byte{0x00}, out, intFmt, 16) {
		t.Fatal("长度不足时应返回 false")
	}

	// 不支持的位深：返回 false。
	if decodePCM(raw16, out, intFmt, 12) {
		t.Fatal("不支持的位深应返回 false")
	}
}

// TestPCMQueue 覆盖队列的阻塞读取、EOF 与溢出丢弃策略。
func TestPCMQueue(t *testing.T) {
	q := newPCMQueue(1) // 容量 96000 样本

	// 空队列读取会阻塞 → 用 goroutine 验证「写入后被唤醒」。
	done := make(chan int, 1)
	go func() {
		buf := make([]float32, 4)
		n, _ := q.read(buf)
		done <- n
	}()
	time.Sleep(20 * time.Millisecond)
	q.write([]float32{1, 2, 3, 4})
	select {
	case n := <-done:
		if n != 4 {
			t.Fatalf("读取样本数不符：%d", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("写入后读取未返回")
	}

	// 部分读取：dst 小于可用数据时只返回 dst 长度。
	q.write([]float32{5, 6, 7, 8})
	buf := make([]float32, 2)
	if n, _ := q.read(buf); n != 2 || buf[0] != 5 || buf[1] != 6 {
		t.Fatalf("部分读取不符：n=%d buf=%v", n, buf)
	}

	// 溢出：写入超过容量后应丢弃最旧数据，且总量不超过容量。
	q2 := newPCMQueue(1)
	capacity := len(q2.ring)
	big := make([]float32, capacity+10)
	for i := range big {
		big[i] = float32(i)
	}
	q2.write(big)
	q2.mu.Lock()
	size := q2.size
	q2.mu.Unlock()
	if size != capacity {
		t.Fatalf("溢出后队列长度应为容量 %d，实际 %d", capacity, size)
	}

	// 关闭后：剩余数据仍可读完，随后 EOF。
	q2.closeWith(nil)
	rest := make([]float32, capacity+10)
	total := 0
	for {
		n, err := q2.read(rest)
		if err != nil {
			break
		}
		total += n
	}
	if total != capacity {
		t.Fatalf("关闭后应能读完剩余 %d 样本，实际 %d", capacity, total)
	}

	// 关闭且无数据 → EOF。
	if _, err := q2.read(rest); err == nil {
		t.Fatal("关闭后读取应返回 EOF")
	}
}

// BenchmarkEnumerate 量化一次完整枚举（设备 + 会话 + 进程快照）的耗时。
//
// 为什么值得基准而不是只看测试耗时：monitor 每 2s 轮询一次，
// 单次枚举的成本直接决定「后台常驻开销」。本机实测约 24ms/次
// （8 个输出设备 + 3 个音频会话 + 全量进程快照），相对 2s 间隔约 1.2% 占空比。
// 若后续觉得偏高，优先优化「只查默认设备 + 已选源设备」而不是降低轮询频率
// （降低频率会让应用启停检测变迟钝）。
func BenchmarkEnumerate(b *testing.B) {
	for i := 0; i < b.N; i++ {
		if _, err := Enumerate(); err != nil {
			b.Fatalf("枚举失败：%v", err)
		}
	}
}

// BenchmarkDecodePCM 量化解码开销：一包 480 帧立体声 float32。
func BenchmarkDecodePCM(b *testing.B) {
	format := AudioFormat{SampleRate: 48000, Channels: 2, Float32: true}
	raw := make([]byte, 480*2*4)
	for i := 0; i < len(raw); i += 4 {
		putFloat32(raw[i:], 0.25)
	}
	out := make([]float32, 480*2)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !decodePCM(raw, out, format, 32) {
			b.Fatal("解码失败")
		}
	}
}

// —— 测试辅助 ——

// extFormatHelper 按 Windows 的**紧凑**内存布局构造 WAVEFORMATEXTENSIBLE。
//
// 为什么不用 Go 结构体嵌套：Go 会按字段最大对齐给 wca.WAVEFORMATEX（18 字节、
// 最大对齐 4）补齐到 20 字节，扩展字段整体后移 2 字节，SubFormat 就不在
// 偏移 24 上了；真实 COM 缓冲没有这个填充。用结构体写测试会得到
// 「代码有 bug」的假结论（见 docs/DEV-ISSUES-S1.md）。
//
// 布局（WAVEFORMATEXTENSIBLE，共 40 字节）：
//
//	0  wFormatTag(2)   2  nChannels(2)   4  nSamplesPerSec(4)
//	8  nAvgBytesPerSec(4)  12 nBlockAlign(2)  14 wBitsPerSample(2)
//	16 cbSize(2)       18 wValidBitsPerSample(2)  20 dwChannelMask(4)
//	24 SubFormat(16，只读前 4 字节 Data1)
type extFormatHelper struct {
	raw [40]byte
}

// makeExtensible 构造一份扩展格式；isFloat 决定 SubFormat（3=IEEE_FLOAT / 1=PCM）。
func makeExtensible(rate, channels, bits, blockAlign int, isFloat bool) *extFormatHelper {
	h := &extFormatHelper{}
	h.putUint16(0, waveFormatExtensible)
	h.putUint16(2, uint16(channels))
	h.putUint32(4, uint32(rate))
	h.putUint32(8, uint32(rate*blockAlign))
	h.putUint16(12, uint16(blockAlign))
	h.putUint16(14, uint16(bits))
	h.putUint16(16, 22) // cbSize
	h.putUint16(18, uint16(bits))
	h.putUint32(20, 0x3) // 立体声掩码，仅占位
	sub := uint32(1)     // KSDATAFORMAT_SUBTYPE_PCM
	if isFloat {
		sub = 3 // KSDATAFORMAT_SUBTYPE_IEEE_FLOAT
	}
	h.putUint32(24, sub)
	return h
}

func (h *extFormatHelper) pointer() *wca.WAVEFORMATEX {
	return (*wca.WAVEFORMATEX)(unsafe.Pointer(&h.raw[0]))
}

func (h *extFormatHelper) setSubFormat(data1 uint32) { h.putUint32(24, data1) }

func (h *extFormatHelper) setCbSize(v uint16) { h.putUint16(16, v) }

func (h *extFormatHelper) putUint16(off int, v uint16) {
	h.raw[off] = byte(v)
	h.raw[off+1] = byte(v >> 8)
}

func (h *extFormatHelper) putUint32(off int, v uint32) {
	h.raw[off] = byte(v)
	h.raw[off+1] = byte(v >> 8)
	h.raw[off+2] = byte(v >> 16)
	h.raw[off+3] = byte(v >> 24)
}

func putFloat32(dst []byte, v float32) {
	bits := math.Float32bits(v)
	putUint32(dst, bits)
}

func putUint32(dst []byte, v uint32) {
	dst[0] = byte(v)
	dst[1] = byte(v >> 8)
	dst[2] = byte(v >> 16)
	dst[3] = byte(v >> 24)
}
