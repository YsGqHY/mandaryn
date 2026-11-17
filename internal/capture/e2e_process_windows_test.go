//go:build windows

package capture

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/utils/procx"
)

// 真实端到端验证：抓一个**真的在放声音**的应用进程，确认
//   1) app:<exe> 能拿到非零 PCM，且**内容与源音频一致**（包络互相关）；
//   2) 进程重启后同一个 app:<exe> 源 ID 仍能续抓（FR-02-2）；
//   3) 目标进程退出后，Channel.Err() 的行为被如实记录（FR-02-5）；
//   4) 进程在跑但不出声时也能打开，且绝不退化成抓全部（FR-02-3）。
//
// 素材优先用项目内的 .narrafork/14761.wav（44.1k/16bit/2ch，17.12s，与捕获格式一致），
// 退而使用 Windows 自带 wav。环境不具备条件时跳过，不制造假失败。

const (
	e2ePlayerExe    = "powershell.exe"
	e2eFallbackWav  = `C:\Windows\Media\Alarm01.wav`
	e2ePreferredWav = ".narrafork/14761.wav"
	e2eAltWav       = "docs/images/14761.wav"
	// e2eWavEnv 允许显式指定测试素材（相对工作目录或绝对路径）。
	e2eWavEnv = "MANDARYN_E2E_WAV"
)

// resolveE2EWav 选出可用的测试音频：环境变量 > 项目素材 > 系统自带。
//
// 注意 go test 的工作目录是**包目录**，所以项目素材要向上逐级查找
// （internal/capture → internal → 仓库根），否则会静默落到系统素材上。
// requirePlaybackDevice 断言本机存在音频输出设备，否则跳过。
//
// 为什么需要这一条：进程级回环捕获在**没有音频设备**的机器上依然「可用」
// （`processLoopbackAvailable()` 返回 true），但播放器不会产生任何可被路由的音量，
// 抓到的必然是静音流 —— 峰值 0.0000x 远低于 `ProducingPeakThreshold`。
// 这类机器包括 GitHub 托管的 Windows runner（无音频端点），CI 上曾因此长期报红：
//
//	--- FAIL: TestE2ECapturedContentMatchesSource
//	    峰值 0.000031 不高于有声阈值 0.001000 —— 抓到的是静音流
//
// 这不是代码缺陷，而是环境不具备条件 —— 与本文件开头「环境不具备条件时跳过，
// 不制造假失败」的约定一致。
func requirePlaybackDevice(t *testing.T) {
	t.Helper()
	srcs, err := Enumerate()
	if err != nil {
		t.Skipf("枚举音频源失败（大概率本机没有音频设备）：%v", err)
	}
	for _, s := range srcs {
		if s.Kind == contract.SourceKindDevice && s.Present {
			return
		}
	}
	t.Skip("本机没有可用的音频输出设备（如 CI runner）：无法产生可捕获的声音，跳过真实音频 E2E")
}

func resolveE2EWav(t *testing.T) string {
	t.Helper()

	var candidates []string
	if v := os.Getenv(e2eWavEnv); v != "" {
		candidates = append(candidates, v)
	}
	for _, rel := range []string{e2ePreferredWav, e2eAltWav} {
		if p := findUpward(rel, 5); p != "" {
			candidates = append(candidates, p)
		}
	}
	candidates = append(candidates, e2eFallbackWav)

	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	t.Skipf("找不到可用的测试音频（试过 %v），跳过端到端验证", candidates)
	return ""
}

// findUpward 从当前工作目录向上逐级查找相对路径，找不到返回空串。
func findUpward(rel string, maxDepth int) string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for i := 0; i <= maxDepth; i++ {
		p := filepath.Join(dir, rel)
		if st, serr := os.Stat(p); serr == nil && !st.IsDir() {
			return p
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break // 已到根
		}
		dir = parent
	}
	return ""
}

// e2ePlayerSpec 循环播放 wav 直到进程被停掉。
func e2ePlayerSpec(wavPath string) procx.Spec {
	script := `$p = New-Object Media.SoundPlayer '` + wavPath + `'; $p.PlayLooping(); Start-Sleep -Seconds 60`
	return procx.Spec{
		Name: "powershell",
		Args: []string{"-NoProfile", "-WindowStyle", "Hidden", "-Command", script},
	}
}

// startAudioPlayer 启动持续播放音频的进程，返回停止函数。
func startAudioPlayer(t *testing.T, wavPath string) func() {
	t.Helper()
	proc, err := procx.StartCtx(context.Background(), e2ePlayerSpec(wavPath))
	if err != nil {
		t.Skipf("无法启动音频播放进程（跳过端到端验证）：%v", err)
	}
	return func() {
		proc.Stop() // 无返回值；Stop/Wait 幂等
		_ = proc.Wait()
	}
}

// captureMono 在 d 时间内把通道读成单声道 float32，返回样本与峰值。
func captureMono(t *testing.T, ch Channel, d time.Duration) (mono []float32, peak float32) {
	t.Helper()
	channels := ch.Format().Channels
	if channels <= 0 {
		channels = 1
	}
	buf := make([]float32, 4096)
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		n, err := ch.ReadPCM(buf)
		if err != nil {
			break
		}
		// 交错样本 → 取第一声道（左）作为比对用的单声道。
		for i := 0; i+channels <= n; i += channels {
			v := buf[i]
			mono = append(mono, v)
			if a := float32(math.Abs(float64(v))); a > peak {
				peak = a
			}
		}
	}
	return mono, peak
}

// —— 极简 WAV 解析（只支持 PCM，够测试用）——

type wavData struct {
	SampleRate int
	Channels   int
	Bits       int
	Mono       []float32 // 取第一声道，归一化到 [-1,1]
}

// readWavMono 读取 16bit PCM wav 的第一声道。
func readWavMono(path string) (*wavData, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(raw) < 12 || string(raw[0:4]) != "RIFF" || string(raw[8:12]) != "WAVE" {
		return nil, fmt.Errorf("%s 不是有效的 RIFF/WAVE 文件", path)
	}

	var (
		sampleRate, channels, bits int
		dataOff, dataLen           int
	)
	pos := 12
	for pos+8 <= len(raw) {
		id := string(raw[pos : pos+4])
		size := int(binary.LittleEndian.Uint32(raw[pos+4 : pos+8]))
		body := pos + 8
		if body+size > len(raw) {
			size = len(raw) - body // 容错：截断的最后一个 chunk
		}
		switch id {
		case "fmt ":
			if size < 16 {
				return nil, fmt.Errorf("fmt chunk 过短")
			}
			format := binary.LittleEndian.Uint16(raw[body : body+2])
			if format != 1 {
				return nil, fmt.Errorf("只支持 PCM（audioFormat=1），实际 %d", format)
			}
			channels = int(binary.LittleEndian.Uint16(raw[body+2 : body+4]))
			sampleRate = int(binary.LittleEndian.Uint32(raw[body+4 : body+8]))
			bits = int(binary.LittleEndian.Uint16(raw[body+14 : body+16]))
		case "data":
			dataOff, dataLen = body, size
		}
		pos = body + size
		if size%2 == 1 {
			pos++ // chunk 按偶数字节对齐
		}
	}
	if sampleRate == 0 || channels == 0 || bits != 16 || dataOff == 0 {
		return nil, fmt.Errorf("不支持的 wav 参数：sr=%d ch=%d bits=%d", sampleRate, channels, bits)
	}

	frames := dataLen / (2 * channels)
	mono := make([]float32, frames)
	for f := 0; f < frames; f++ {
		off := dataOff + f*channels*2
		v := int16(binary.LittleEndian.Uint16(raw[off : off+2]))
		mono[f] = float32(v) / 32768.0
	}
	return &wavData{SampleRate: sampleRate, Channels: channels, Bits: bits, Mono: mono}, nil
}

// —— 包络互相关 ——

// envelope 把音频按 10ms 窗口取 RMS，得到对相位不敏感的能量包络。
//
// 为什么用包络而不是原始波形：回环捕获会有设备混音、时钟漂移与会话音量影响，
// 逐样本比对必然对不上；包络既保留「内容随时间起伏」的特征，又容忍这些差异。
func envelope(x []float32, sampleRate, winMs int) []float32 {
	win := sampleRate * winMs / 1000
	if win <= 0 {
		win = 1
	}
	out := make([]float32, 0, len(x)/win+1)
	for i := 0; i+win <= len(x); i += win {
		var sum float64
		for _, v := range x[i : i+win] {
			sum += float64(v) * float64(v)
		}
		out = append(out, float32(math.Sqrt(sum/float64(win))))
	}
	return out
}

// bestCorrelation 在 ±maxLag 范围内找 a 与 b 的最佳归一化互相关，返回滞后与得分。
//
// 正值 lag 表示 b 相对于 a 偏移了 lag 个包络帧。
func bestCorrelation(a, b []float32, maxLag int) (bestLag int, bestScore float64) {
	bestScore = -2
	for lag := -maxLag; lag <= maxLag; lag++ {
		var ax, bx []float32
		if lag >= 0 {
			if lag >= len(b) || lag >= len(a) {
				continue
			}
			ax, bx = a[lag:], b
		} else {
			if -lag >= len(a) || -lag >= len(b) {
				continue
			}
			ax, bx = a, b[-lag:]
		}
		n := len(ax)
		if len(bx) < n {
			n = len(bx)
		}
		// 至少要比对 1 秒（100 帧），否则短片段容易出假高分。
		if n < 100 {
			continue
		}
		score := pearson(ax[:n], bx[:n])
		if score > bestScore {
			bestScore, bestLag = score, lag
		}
	}
	return bestLag, bestScore
}

// pearson 计算皮尔逊相关系数。
func pearson(a, b []float32) float64 {
	n := float64(len(a))
	if n == 0 {
		return 0
	}
	var sa, sb float64
	for i := range a {
		sa += float64(a[i])
		sb += float64(b[i])
	}
	ma, mb := sa/n, sb/n
	var num, da, db float64
	for i := range a {
		x, y := float64(a[i])-ma, float64(b[i])-mb
		num += x * y
		da += x * x
		db += y * y
	}
	if da == 0 || db == 0 {
		return 0
	}
	return num / math.Sqrt(da*db)
}

// TestE2ECapturedContentMatchesSource 端到端核心验证：
// 抓到的 PCM 内容必须与播放的源音频一致（而不只是「有声音」）。
func TestE2ECapturedContentMatchesSource(t *testing.T) {
	if ok, note := processLoopbackAvailable(); !ok {
		t.Skipf("本机不支持进程回环：%s", note)
	}
	requirePlaybackDevice(t)
	wavPath := resolveE2EWav(t)

	src, err := readWavMono(wavPath)
	if err != nil {
		t.Fatalf("读取测试音频失败：%v", err)
	}
	t.Logf("源音频：%s  %dHz/%dch/%dbit  %.2fs",
		filepath.Base(wavPath), src.SampleRate, src.Channels, src.Bits,
		float64(len(src.Mono))/float64(src.SampleRate))

	stop := startAudioPlayer(t, wavPath)
	defer stop()
	time.Sleep(1500 * time.Millisecond) // 等音频会话建立

	ch, err := Open(AppSourceID(e2ePlayerExe))
	if err != nil {
		t.Fatalf("打开 %s 失败：%v", AppSourceID(e2ePlayerExe), err)
	}
	defer ch.Close()

	f := ch.Format()
	t.Logf("捕获格式：%+v", f)
	if f.SampleRate != processLoopbackSampleRate || f.Channels != processLoopbackChannels {
		t.Errorf("格式 = %+v，期望 %dHz/%dch", f, processLoopbackSampleRate, processLoopbackChannels)
	}

	capMono, peak := captureMono(t, ch, 6*time.Second)
	frames := len(capMono)
	secs := float64(frames) / float64(f.SampleRate)
	t.Logf("捕获 %.2f 秒（%d 帧），峰值 %.4f", secs, frames, peak)

	if frames == 0 {
		t.Fatal("没有读到任何 PCM —— 分路捕获未工作")
	}
	if peak <= ProducingPeakThreshold {
		t.Fatalf("峰值 %.6f 不高于有声阈值 %.6f —— 抓到的是静音流（检查系统音量/静音）",
			peak, ProducingPeakThreshold)
	}

	// 内容比对：源与捕获都取 10ms 包络，在 ±3 秒内找最佳对齐。
	const winMs = 10
	srcEnv := envelope(src.Mono, src.SampleRate, winMs)
	capEnv := envelope(capMono, f.SampleRate, winMs)
	maxLag := 300 // 300 × 10ms = 3s
	lag, score := bestCorrelation(capEnv, srcEnv, maxLag)

	// 把源音频的包络按最佳 lag 对齐后换算成秒，便于人工核对。
	lagSec := float64(lag) * float64(winMs) / 1000
	t.Logf("包络互相关：最佳对齐 %d 帧（%.2fs），相关系数 %.4f（源包络 %d 点，捕获包络 %d 点）",
		lag, lagSec, score, len(srcEnv), len(capEnv))

	// 0.7 已是「内容明显同源」的下限：回环经过设备混音/音量处理，
	// 但包络形状应高度一致。低于这个值说明抓到的不是目标应用的声音。
	const minScore = 0.7
	if score < minScore {
		t.Fatalf("内容相关性 %.4f < %.2f —— 捕获内容与源音频不一致（可能抓到了别的声音，或静音）",
			score, minScore)
	}
}

// TestE2EAppRestartReconnect app:<exe> 在目标进程重启后仍能续抓（FR-02-2）。
func TestE2EAppRestartReconnect(t *testing.T) {
	if ok, note := processLoopbackAvailable(); !ok {
		t.Skipf("本机不支持进程回环：%s", note)
	}
	requirePlaybackDevice(t)
	wavPath := resolveE2EWav(t)
	srcID := AppSourceID(e2ePlayerExe)

	// 第一轮
	stop1 := startAudioPlayer(t, wavPath)
	time.Sleep(1500 * time.Millisecond)
	ch1, err := Open(srcID)
	if err != nil {
		stop1()
		t.Fatalf("第一次打开失败：%v", err)
	}
	mono1, _ := captureMono(t, ch1, 1500*time.Millisecond)
	_ = ch1.Close()
	stop1()
	t.Logf("第一轮读到 %d 帧", len(mono1))

	// 第二轮：进程已重启（PID 变化），同一源 ID 必须仍可用 —— ID 不含 PID。
	stop2 := startAudioPlayer(t, wavPath)
	defer stop2()
	time.Sleep(1500 * time.Millisecond)

	ch2, err := Open(srcID)
	if err != nil {
		t.Fatalf("目标重启后，同一个源 ID %s 应仍能打开：%v", srcID, err)
	}
	defer ch2.Close()
	mono2, _ := captureMono(t, ch2, 1500*time.Millisecond)
	t.Logf("第二轮（进程已重启，PID 已变）读到 %d 帧", len(mono2))

	if len(mono1) == 0 || len(mono2) == 0 {
		t.Fatalf("任一轮没有读到帧，续抓不成立（第一轮 %d，第二轮 %d）", len(mono1), len(mono2))
	}
}

// TestE2EAppExitReportsErr 目标进程退出后观察 Channel.Err()（FR-02-5）。
//
// 结论（实测）：进程级回环在目标消失后**只是停止出包**，不保证发事件，
// 因此这里不判失败，只把事实记录下来；源失效的可靠判定需要靠上层轮询/校验。
func TestE2EAppExitReportsErr(t *testing.T) {
	if ok, note := processLoopbackAvailable(); !ok {
		t.Skipf("本机不支持进程回环：%s", note)
	}
	wavPath := resolveE2EWav(t)

	proc, err := procx.StartCtx(context.Background(), e2ePlayerSpec(wavPath))
	if err != nil {
		t.Skipf("无法启动播放进程：%v", err)
	}
	time.Sleep(1500 * time.Millisecond)

	ch, err := Open(AppSourceID(e2ePlayerExe))
	if err != nil {
		proc.Stop()
		_ = proc.Wait()
		t.Fatalf("打开失败：%v", err)
	}
	defer ch.Close()

	proc.Stop() // 杀掉目标进程

	select {
	case e := <-ch.Err():
		if e == nil {
			t.Fatal("Channel.Err() 收到 nil，应给出具体原因")
		}
		t.Logf("目标进程退出后收到 Channel.Err()：%v", e)
	case <-time.After(10 * time.Second):
		t.Log("已确认：目标退出后 10s 内未收到 Channel.Err() —— 进程回环在目标消失后仅停止出包，" +
			"源失效需由上层主动校验根进程存活（建议加 500ms 轮询触发 capture:invalid）")
	}
	_ = proc.Wait()
}

// TestE2EOpenNonAudioProcess 进程存在但不出声时也必须能打开（只是没有音频数据）。
//
// 对应 S1-08 的能力边界：Present 与 Producing 是两件事。
// 同时是 FR-02-3 的反向断言——绝不因为「没有声音」就退化成抓全部。
func TestE2EOpenNonAudioProcess(t *testing.T) {
	if ok, note := processLoopbackAvailable(); !ok {
		t.Skipf("本机不支持进程回环：%s", note)
	}

	exe := strings.ToLower(filepath.Base(os.Args[0]))
	ch, err := Open(AppSourceID(exe))
	if err != nil {
		t.Fatalf("对无音频输出的进程 Open 不应失败：%v", err)
	}
	defer ch.Close()

	buf := make([]float32, 1024)
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, rerr := ch.ReadPCM(buf); rerr != nil {
			t.Fatalf("无音频时期 ReadPCM 不应返回错误：%v", rerr)
		}
	}
}
