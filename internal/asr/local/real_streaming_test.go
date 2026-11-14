//go:build windows && cgo && sherpa_models && !nollm

// 流式引擎的真实模型验证 + FFD/SLD 采样（默认不参与 CI）。
//
// 运行方式（模型与样本由人工准备，见 docs/DEV-ISSUES-G-streaming.md）：
//
//	export MANDARYN_REAL_STREAM_DIR="C:/path/to/zipformer-zh-streaming@1.0.3"  # 三件套 + tokens.txt
//	export MANDARYN_REAL_WAV="C:/path/to/zh.wav"                               # 16k 单声道真实中文语音
//	export MANDARYN_REAL_OFFLINE_DIR="C:/path/to/sensevoice-small@1.2.0"       # 可选，用于离线对照
//	export MANDARYN_STREAM_BENCH_ROUNDS=20                                    # 可选，采样轮数（默认 20）
//	go test -tags sherpa_models -c -o bin/streambench.exe ./internal/asr/local/
//	cd bin && ./streambench.exe -test.v -test.run TestRealStreaming
//
// Windows 上必须**先编译再从 bin/ 运行**：测试二进制链接 sherpa-onnx 的 3 个 DLL，
// 直接 `go test` 会 0xc0000135（找不到 DLL），而 DLL 在 bin/ 里与 exe 同目录。
package local

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/asr"
)

// —— 采样口径（与 NFR-01 / ARCH §7.3 的关系）——

// 本测试在**引擎层**采样，因此只能测「引擎视角」的那一段：
//
//	FFD（本测试）= t(首个非空增量文本返回) − t(开始送音频)
//	SLD（本测试）= 稳态阶段相邻两次**非空增量**之间的墙钟间隔
//
// 与 ARCH §7.3 的正式定义有两处差异，**必须**在结论里如实标注，别混用：
//   - 正式 FFD 含「前端渲染到屏幕」的时间；本测试在无 GUI 的进程里跑，测不到这一段，
//     所以这里的 FFD 是正式 FFD 的**下界**（真机还要加上 IPC + React 渲染，通常几十 ms）。
//   - 正式 SLD 是「VAD 判定句末 → 片段落库且 finalized」；本测试没有被测的 VAD 与 DB，
//     只能退化成「引擎相邻出字的间隔」，它反映的是**出字节奏**而不是定稿延迟。
//
// 因此本测试额外测一条**更有信息量**的指标（`lag`）：已喂音频时长 − 已耗墙钟。
// 它回答的是真问题「解码跟不跟得上实时」：`lag` 稳态为正且持续增长说明引擎跟不上，
// 无论出字节奏看起来多漂亮，正式 SLD 都必然爆表。`lag` 的 P95 对照 1500ms 才是
// 引擎侧对 SLD 预算的真实可行判据。

const (
	// streamChunkMs 每次 Accept 送 100ms 音频（ARCH §2.3：100ms 帧喂 streaming Zipformer）。
	streamChunkMs = 100
	// benchRoundSeconds 每轮实时采样喂多少秒音频。
	//
	// 5 秒的依据：FFD 预算是 ≤900ms，5 秒足够覆盖「首字 + 稳态若干次出字」；
	// 轮数 × 5 秒就是本测试的墙钟下限（20 轮 ≈ 100 秒，可以接受）。
	benchRoundSeconds = 5
	// benchDefaultRounds 默认采样轮数（PRD §7 验收：20 次采样取 P50/P95）。
	benchDefaultRounds = 20
	// benchThreads 采样线程数：对齐 PRD「4 核 CPU、medium 档」的口径。
	benchThreads = 4
)

// TestRealStreamingWarm 断言真实流式模型包能识别、能加载、能出字。
//
// 不断言具体字串：模型版本会更新，硬编码期望文本会让测试变成版本牢笼
// （与 TestRealSenseVoiceTranscribe 同一原则）。
func TestRealStreamingWarm(t *testing.T) {
	dir, wav := realStreamEnv(t)

	// 第一步：包识别。流式包必须是 transducer 三件套。
	spec, err := asr.DetectBundle(dir)
	if err != nil {
		t.Fatalf("DetectBundle 失败: %v", err)
	}
	t.Logf("包识别：kind=%s encoder=%s decoder=%s joiner=%s",
		spec.Kind, base(spec.Encoder), base(spec.Decoder), base(spec.Joiner))
	if spec.Kind != asr.BundleKindTransducer {
		t.Fatalf("流式包被识别成 %q，online recognizer 只吃 transducer 三件套", spec.Kind)
	}

	// 第二步：真实加载。
	eng := newRealStream(t, dir)
	ctx := context.Background()
	if err := eng.Warm(ctx); err != nil {
		t.Fatalf("Warm 失败（真实流式模型加载）: %v", err)
	}
	h := eng.Health(ctx)
	t.Logf("加载后：engineId=%s 状态=%s 加载耗时=%dms", eng.ID(), h.State, h.LoadMs)
	if h.State != "ready" {
		t.Fatalf("加载后状态 = %q，期望 ready", h.State)
	}

	// 第三步：真实流式解码，逐块打印增量（这是「能累积出非空文本」的直接证据）。
	samples := readWAV16kStream(t, wav)
	t.Logf("样本：%d 采样点 = %.2fs", len(samples), float64(len(samples))/16000)

	total, deltas := feedAll(t, eng, samples)
	t.Logf("增量条数=%d 累计文本=%q", len(deltas), total)
	if strings.TrimSpace(total) == "" {
		t.Fatal("真实流式模型整段音频没有吐出任何文本")
	}
	if eng.StreamingRevisions() != 0 {
		// 不改写是正常情况；出现了也**不是失败**（接口本就不保证），只是要留痕。
		t.Logf("注意：流式解码期间发生 %d 次文本改写（增量接口无法撤回，定稿可能多字）",
			eng.StreamingRevisions())
	}

	// 第四步：离线对照（可选，仅打印 + 粗相似度，不做逐字断言）。
	if offDir := strings.TrimSpace(os.Getenv("MANDARYN_REAL_OFFLINE_DIR")); offDir != "" {
		offText := offlineText(t, offDir, samples)
		t.Logf("离线对照：流式=%q", total)
		t.Logf("离线对照：离线=%q", offText)
		t.Logf("离线对照：字符数 流式=%d 离线=%d 最佳子序列相似度=%.2f",
			len([]rune(total)), len([]rune(offText)), lcsRatio(total, offText))
	} else {
		t.Log("未设置 MANDARYN_REAL_OFFLINE_DIR，跳过离线对照")
	}
}

// TestRealStreamingReset 断言 Reset 之后重新喂同一段音频会**从头出字**，
// 不会把上一轮的文本再拼一遍（对应「新会话开始」这个真实场景）。
func TestRealStreamingReset(t *testing.T) {
	dir, wav := realStreamEnv(t)
	eng := newRealStream(t, dir)
	if err := eng.Warm(context.Background()); err != nil {
		t.Fatalf("Warm 失败: %v", err)
	}
	defer func() { _ = eng.Close() }()

	samples := readWAV16kStream(t, wav)
	firstTotal, firstDeltas := feedAll(t, eng, samples)
	if len(firstDeltas) == 0 {
		t.Fatal("第一轮没有吐出任何增量")
	}

	if err := eng.Reset(); err != nil {
		t.Fatalf("Reset 失败: %v", err)
	}
	secondTotal, secondDeltas := feedAll(t, eng, samples)
	if len(secondDeltas) == 0 {
		t.Fatal("Reset 后第二轮没有吐出任何增量")
	}

	t.Logf("第一轮：首增量=%q 累计=%q", firstDeltas[0], firstTotal)
	t.Logf("第二轮：首增量=%q 累计=%q", secondDeltas[0], secondTotal)

	// 核心断言：Reset 清干净了已发出前缀 —— 第二轮首增量与第一轮一致，
	// 而不是「第一轮累计 + 第一轮累计」那种重复。
	if secondDeltas[0] != firstDeltas[0] {
		t.Fatalf("Reset 后首增量 = %q，期望与第一轮一致 %q（说明解码状态或增量前缀没清干净）",
			secondDeltas[0], firstDeltas[0])
	}
	if strings.HasPrefix(secondTotal, firstTotal+firstTotal) {
		t.Fatalf("Reset 后累计文本重复了上一轮内容: %q", secondTotal)
	}
	if secondTotal != firstTotal {
		// 同一段音频 + greedy_search 应当是确定的；出现差异不判失败但必须留痕。
		t.Logf("注意：两轮累计文本不一致（差 %d 字），可能是 int8/多线程的非确定性",
			len([]rune(secondTotal))-len([]rune(firstTotal)))
	}
}

// TestRealStreamingLatency 按 PRD 口径做 20 次采样，输出 FFD / SLD 的 P50 / P95。
//
// 采样方式：**实时喂**（每 100ms 音频等 100ms 再送下一块）。
// 这一点不能省：若全速灌样本，引擎一直有积压数据，FFD 会退化成「纯算力耗时」，
// 与用户真实体验（音频是实时到达的）完全不是一回事，测出来的数字毫无意义。
func TestRealStreamingLatency(t *testing.T) {
	dir, wav := realStreamEnv(t)
	eng := newRealStream(t, dir)
	if err := eng.Warm(context.Background()); err != nil {
		t.Fatalf("Warm 失败: %v", err)
	}
	defer func() { _ = eng.Close() }()

	rounds := benchDefaultRounds
	if v := strings.TrimSpace(os.Getenv("MANDARYN_STREAM_BENCH_ROUNDS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			rounds = n
		}
	}
	samples := readWAV16kStream(t, wav)
	onset := speechOnsetMs(samples)
	t.Logf("采样：%d 轮，每轮实时喂 %ds（样本共 %.2fs，语音起点 %dms，即前导静音 %dms），线程数=%d",
		rounds, benchRoundSeconds, float64(len(samples))/16000, onset, onset, benchThreads)

	var (
		ffds []time.Duration
		// effectives 是「扣掉样本前导静音」的首字延迟：FFD − 语音起点。
		//
		// 为什么要单列：PRD 口径的 FFD 从「用户触发开始」起算，天然**包含**
		// 开头那段还没人说话的静音。而引擎不可能对静音出字，所以样本前导静音
		// 有多长，FFD 就被垫高多少 —— 这不是引擎慢。把两者分开报，结论才不误导人。
		effectives []time.Duration
		slds       []time.Duration
		lags       []time.Duration
	)
	chunk := 16000 * streamChunkMs / 1000
	limit := 16000 * benchRoundSeconds
	if limit > len(samples) {
		limit = len(samples)
	}

	for round := 0; round < rounds; round++ {
		if err := eng.Reset(); err != nil {
			t.Fatalf("第 %d 轮 Reset 失败: %v", round, err)
		}
		start := time.Now()
		var (
			ffd      time.Duration
			lastText time.Time
			gotAny   bool
			maxLag   time.Duration
		)
		for off := 0; off < limit; off += chunk {
			// 实时节拍：等到「这块音频本该到达」的时刻再送。
			// 注意用 `off * Second / 16000` 而不是 `off / 16000`：后者是整除，
			// 前 1 秒内的每块都会算成 0s，等于把 1 秒音频瞬间灌进去，FFD 就假了。
			due := start.Add(time.Duration(off) * time.Second / 16000)
			if wait := time.Until(due); wait > 0 {
				time.Sleep(wait)
			}
			end := off + chunk
			if end > limit {
				end = limit
			}
			delta, err := eng.Accept(samples[off:end])
			if err != nil {
				t.Fatalf("第 %d 轮 Accept 失败: %v", round, err)
			}
			now := time.Now()
			// lag = 已喂音频时长 − 已耗墙钟（含 sleep，故稳态应在 0 附近）。
			// 这里取「这一块喂完到解码返回」的额外拖尾：now − due 就是真正的积压。
			if lag := now.Sub(due); lag > maxLag {
				maxLag = lag
			}
			if strings.TrimSpace(delta) == "" {
				continue
			}
			if !gotAny {
				ffd = now.Sub(start)
				gotAny = true
			} else {
				slds = append(slds, now.Sub(lastText))
			}
			lastText = now
		}
		if !gotAny {
			t.Fatalf("第 %d 轮整段没有出字，无法统计 FFD", round)
		}
		ffds = append(ffds, ffd)
		if eff := ffd - time.Duration(onset)*time.Millisecond; eff > 0 {
			effectives = append(effectives, eff)
		} else {
			effectives = append(effectives, 0)
		}
		lags = append(lags, maxLag)
		if os.Getenv("MANDARYN_STREAM_BENCH_VERBOSE") != "" {
			t.Logf("第 %2d 轮：FFD=%dms 最大积压=%dms", round, ffd.Milliseconds(), maxLag.Milliseconds())
		}
	}

	ffdP50, ffdP95 := percentile(ffds, 50), percentile(ffds, 95)
	effP50, effP95 := percentile(effectives, 50), percentile(effectives, 95)
	sldP50, sldP95 := percentile(slds, 50), percentile(slds, 95)
	lagP50, lagP95 := percentile(lags, 50), percentile(lags, 95)

	// PRD NFR-01（本地）双指标：
	//   FFD-engine（达标判据）：起点=首个非静音帧，P50 ≤ 1000ms / P95 ≤ 1500ms；
	//   FFD-e2e（仅观测）    ：起点=开始送音频（含源音频前导静音），不设阈值；
	//   SLD P95 ≤ 1500ms。
	t.Log("==== FFD / SLD 采样结果（对照 PRD NFR-01 本地阈值）====")
	t.Logf("FFD-engine 样本=%-3d P50=%4dms  P95=%4dms   阈值 P50≤1000  P95≤1500   %s",
		len(effectives), effP50.Milliseconds(), effP95.Milliseconds(),
		passFail(effP50 <= 1000*time.Millisecond, effP95 <= 1500*time.Millisecond))
	t.Logf("FFD-e2e    样本=%-3d P50=%4dms  P95=%4dms   （仅观测，含样本前导静音 %dms，不设阈值）",
		len(ffds), ffdP50.Milliseconds(), ffdP95.Milliseconds(), onset)
	t.Logf("SLD        样本=%-3d P50=%4dms  P95=%4dms   阈值 P95≤1500（出字节奏代理）    %s",
		len(slds), sldP50.Milliseconds(), sldP95.Milliseconds(),
		passFail(sldP95 <= 1500*time.Millisecond))
	t.Logf("积压       样本=%-3d P50=%4dms  P95=%4dms   阈值 P95≤1500（解码跟不跟得上实时）%s",
		len(lags), lagP50.Milliseconds(), lagP95.Milliseconds(),
		passFail(lagP95 <= 1500*time.Millisecond))

	// 不达标**不**判失败：性能取决于本机 CPU 与整机负载，硬失败会让这个测试
	// 在慢机器上永远红。结论写进日志，由人看（也要如实进汇报）。
	ok := effP50 <= 1000*time.Millisecond && effP95 <= 1500*time.Millisecond &&
		sldP95 <= 1500*time.Millisecond && lagP95 <= 1500*time.Millisecond
	if !ok {
		t.Logf("结论：**未全部达标** —— FFD-engine P50=%dms/P95=%dms（阈值 1000/1500），"+
			"SLD P95=%dms，积压 P95=%dms；FFD-e2e P50=%dms（含静音，仅观测）",
			effP50.Milliseconds(), effP95.Milliseconds(),
			sldP95.Milliseconds(), lagP95.Milliseconds(), ffdP50.Milliseconds())
		t.Log("      录制与落盘不受影响（NFR-08：预算不足时优先保证录制不中断）")
	} else {
		t.Logf("结论：**达标** —— FFD-engine P50=%dms/P95=%dms、SLD P95=%dms、积压 P95=%dms，"+
			"均在 NFR-01 本地阈值内（FFD-e2e P50=%dms 含静音，仅观测不计入判定）",
			effP50.Milliseconds(), effP95.Milliseconds(),
			sldP95.Milliseconds(), lagP95.Milliseconds(), ffdP50.Milliseconds())
	}
}

// TestRealStreamingChunkSensitivity 测「每次 Accept 送多长的音频」对 FFD 的影响。
//
// 为什么值得测：上层 `transcribe.LiveRunner.take()` 一次最多取 **200ms**，
// 而块越大，音频就得先在队列里攒够 200ms 才送进解码器 —— 这直接把 FFD 垫高。
// 这条用例给出「块长 → FFD」的实测对照，接线时才有依据决定 take() 的上限。
//
// 默认跳过（要额外跑约半分钟），需要时显式打开：
//
//	MANDARYN_STREAM_BENCH_CHUNKS=1 ./streambench.exe -test.v -test.run TestRealStreamingChunkSensitivity
func TestRealStreamingChunkSensitivity(t *testing.T) {
	if os.Getenv("MANDARYN_STREAM_BENCH_CHUNKS") == "" {
		t.Skip("未设置 MANDARYN_STREAM_BENCH_CHUNKS，跳过块长敏感度测量")
	}
	dir, wav := realStreamEnv(t)
	eng := newRealStream(t, dir)
	if err := eng.Warm(context.Background()); err != nil {
		t.Fatalf("Warm 失败: %v", err)
	}
	defer func() { _ = eng.Close() }()

	samples := readWAV16kStream(t, wav)
	onset := speechOnsetMs(samples)
	limit := 16000 * benchRoundSeconds
	if limit > len(samples) {
		limit = len(samples)
	}

	const roundsPerSize = 5
	t.Logf("块长敏感度（每档 %d 轮，前导静音 %dms）", roundsPerSize, onset)
	for _, chunkMs := range []int{20, 100, 200} {
		var ffds []time.Duration
		chunk := 16000 * chunkMs / 1000
		for round := 0; round < roundsPerSize; round++ {
			if err := eng.Reset(); err != nil {
				t.Fatalf("Reset 失败: %v", err)
			}
			start := time.Now()
			var ffd time.Duration
			for off := 0; off < limit; off += chunk {
				due := start.Add(time.Duration(off) * time.Second / 16000)
				if wait := time.Until(due); wait > 0 {
					time.Sleep(wait)
				}
				end := off + chunk
				if end > limit {
					end = limit
				}
				delta, err := eng.Accept(samples[off:end])
				if err != nil {
					t.Fatalf("Accept 失败: %v", err)
				}
				if strings.TrimSpace(delta) != "" {
					ffd = time.Since(start)
					break // 只要首字，拿到就停
				}
			}
			if ffd > 0 {
				ffds = append(ffds, ffd)
			}
		}
		if len(ffds) == 0 {
			t.Logf("块长 %3dms：未出字", chunkMs)
			continue
		}
		p50, p95 := percentile(ffds, 50), percentile(ffds, 95)
		t.Logf("块长 %3dms：FFD P50=%4dms P95=%4dms（扣前导静音 %4dms / %4dms）",
			chunkMs, p50.Milliseconds(), p95.Milliseconds(),
			(p50 - time.Duration(onset)*time.Millisecond).Milliseconds(),
			(p95 - time.Duration(onset)*time.Millisecond).Milliseconds())
	}
}

// speechOnsetMs 估算语音起点（毫秒）：按 20ms 帧算 RMS，连续 3 帧超过阈值即算开始。
//
// 用途是把「样本前导静音」从 FFD 里拆出来，让结论能区分
// 「引擎慢」与「这段音频开头本来就没人在说话」。
// 阈值取 0.01（≈ −40dBFS）：真实语音的 RMS 通常在 0.03–0.2，
// 环境底噪在 0.001–0.005，0.01 落在两者之间且对常见样本稳定。
// speechOnsetMs 返回首个语音帧相对音频开头的毫秒偏移。
//
// 判定用**相对底噪**的门限，不用绝对 RMS 阈值。原因（实测教训）：
// 本样本的起音段从约 690ms 就开始爬升（-41.8dB → -34.5dB → -33dB…），但任何
// 绝对门限（如 RMS>0.01 ≈ -40dBFS）都会把这段爬升判成静音，直到 720ms 能量冲到
// -33dB 以上才开始计数 —— 结果把静音高估 ~30ms，进而把「扣静音后的首字延迟」低估同样幅度。
// 而 PRD 的 FFD-engine 正是以「首个非静音帧」为起点，这个偏差会直接决定是否达标。
//
// 因此：底噪取**音频前段中位数**，门限 = 底噪 + onsetSNRDb；并要求连续 need 帧命中，
// 避免被单帧瞬态尖峰（起音咔哒、按键声）骗到最早位置。
func speechOnsetMs(samples []float32) int64 {
	const (
		frame     = 320 // 20ms @ 16k
		need      = 3   // 连续 3 帧（60ms）才算真的开口
		onsetSNR  = 12.0
		noiseWinS = 0.60 // 用前 600ms 估底噪
	)
	nFrames := len(samples) / frame
	if nFrames == 0 {
		return 0
	}

	// 逐帧能量（均方值，避免每帧开方）。
	energy := make([]float64, nFrames)
	for i := 0; i < nFrames; i++ {
		var sum float64
		for _, s := range samples[i*frame : (i+1)*frame] {
			sum += float64(s) * float64(s)
		}
		energy[i] = sum / float64(frame)
	}

	// 底噪：前 noiseWinS 段能量的中位数。样本开头若正好有语音，中位数仍稳健。
	win := int(noiseWinS * 16000 / frame)
	if win > nFrames {
		win = nFrames
	}
	sorted := make([]float64, win)
	copy(sorted, energy[:win])
	sort.Float64s(sorted)
	noise := sorted[win/2]

	// 门限换算成能量：+onsetSNR dB ⇒ 能量乘 10^(SNR/10)。
	thr := noise * math.Pow(10, onsetSNR/10.0)

	hit := 0
	for i := 0; i < nFrames; i++ {
		if energy[i] > thr {
			hit++
			if hit >= need {
				// 起点回退 need 帧：真正开口在第一次命中的位置。
				return int64(i-(need-1)) * frame * 1000 / 16000
			}
		} else {
			hit = 0
		}
	}
	return 0
}

// —— 工具 ——

// readWAV16kStream 读 16k/单声道/16bit PCM 的 wav。
//
// 为什么不复用 real_model_test.go 的 readWAV16k：那个文件的构建标签是
// `windows && cgo && sherpa_models && !nollm`，与本文件的 `mandaryn_realmodel`
// 互不包含，两个 tag 组合下都会各编译一次；共用同一份实现会让「跑流式验证」
// 被迫也开 sherpa_models 标签（多拖一批离线用例进来）。宁可重复二十行解析代码。
//
// 只支持验证用的最小子集：非 16k / 非单声道 / 非 16bit 直接 Skip，
// 避免把「样本格式不对」误报成「引擎有问题」。
func readWAV16kStream(t *testing.T, path string) []float32 {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 wav 失败: %v", err)
	}
	if len(raw) < 44 || string(raw[0:4]) != "RIFF" || string(raw[8:12]) != "WAVE" {
		t.Fatalf("不是 wav: %s", base(path))
	}
	var (
		channels, bits, rate int
		pcm                  []byte
	)
	for off := 12; off+8 <= len(raw); {
		id := string(raw[off : off+4])
		size := int(binary.LittleEndian.Uint32(raw[off+4 : off+8]))
		body := off + 8
		if body+size > len(raw) {
			size = len(raw) - body
		}
		switch id {
		case "fmt ":
			channels = int(binary.LittleEndian.Uint16(raw[body+2 : body+4]))
			rate = int(binary.LittleEndian.Uint32(raw[body+4 : body+8]))
			bits = int(binary.LittleEndian.Uint16(raw[body+14 : body+16]))
		case "data":
			pcm = raw[body : body+size]
		}
		off = body + size + size%2 // chunk 按偶数字节对齐
	}
	if channels != 1 || bits != 16 || rate != 16000 {
		t.Skipf("样本不是 16k/单声道/16bit（rate=%d ch=%d bits=%d），换一个样本: %s",
			rate, channels, bits, base(path))
	}
	if len(pcm) == 0 {
		t.Fatalf("wav 里没有 data: %s", base(path))
	}
	out := make([]float32, len(pcm)/2)
	for i := range out {
		v := int16(binary.LittleEndian.Uint16(pcm[i*2 : i*2+2]))
		out[i] = float32(v) / 32768
	}
	return out
}

// realStreamEnv 读环境变量并给出「跳过」判定。
func realStreamEnv(t *testing.T) (dir, wav string) {
	t.Helper()
	dir = strings.TrimSpace(os.Getenv("MANDARYN_REAL_STREAM_DIR"))
	wav = strings.TrimSpace(os.Getenv("MANDARYN_REAL_WAV"))
	if dir == "" || wav == "" {
		t.Skip("未设置 MANDARYN_REAL_STREAM_DIR / MANDARYN_REAL_WAV，跳过真实流式验证")
	}
	return dir, wav
}

// newRealStream 构造真实流式引擎（返回具体类型以访问诊断方法）。
func newRealStream(t *testing.T, dir string) *StreamingEngine {
	t.Helper()
	eng, err := NewStreaming(Spec{
		BundleID: "zipformer-zh-streaming",
		Version:  "1.0.3",
		Dir:      dir,
		Threads:  benchThreads,
	})
	if err != nil {
		t.Fatalf("构造流式引擎失败: %v", err)
	}
	concrete, ok := eng.(*StreamingEngine)
	if !ok {
		t.Fatalf("流式引擎类型异常: %T", eng)
	}
	return concrete
}

// feedAll 把整段音频按 100ms 一块全速灌进引擎（**不**节拍），返回累计文本与增量序列。
//
// 全速是为了尽快拿到「能出字」的事实；延迟测量另有实时节拍的那条用例。
func feedAll(t *testing.T, eng *StreamingEngine, samples []float32) (string, []string) {
	t.Helper()
	chunk := 16000 * streamChunkMs / 1000
	var (
		total  strings.Builder
		deltas []string
	)
	for off := 0; off < len(samples); off += chunk {
		end := off + chunk
		if end > len(samples) {
			end = len(samples)
		}
		delta, err := eng.Accept(samples[off:end])
		if err != nil {
			t.Fatalf("Accept 失败（offset=%d）: %v", off, err)
		}
		if strings.TrimSpace(delta) == "" {
			continue
		}
		deltas = append(deltas, delta)
		total.WriteString(delta)
	}
	return total.String(), deltas
}

// offlineText 用离线模型对同一段音频转写，作为流式结果的人工对照。
//
// 离线包缺失 / 加载失败时返回说明性字符串而**不** Fatal：
// 对照是辅助信息，不该让「没装离线包」变成流式验证失败。
func offlineText(t *testing.T, dir string, samples []float32) string {
	t.Helper()
	eng, err := New(Spec{BundleID: "sensevoice-small", Version: "1.2.0", Dir: dir, Threads: benchThreads})
	if err != nil {
		return fmt.Sprintf("<离线引擎构造失败: %v>", err)
	}
	defer func() { _ = eng.Close() }()
	if err := eng.Warm(context.Background()); err != nil {
		return fmt.Sprintf("<离线引擎加载失败: %s>", apierr.From(err, apierr.CodeEngineLoad).Code)
	}
	res, err := eng.Finalize(asr.AudioSegment{Samples: samples, SampleRate: 16000})
	if err != nil {
		return fmt.Sprintf("<离线转写失败: %s>", apierr.From(err, apierr.CodeEngineLoad).Code)
	}
	return strings.TrimSpace(res.Text)
}

// lcsRatio 返回两个字符串（按 rune）最长公共子序列 / 较长者长度，0–1。
//
// 只用于给人一个「流式和离线差多远」的量级感，不做断言：
// 两遍法的意义正是「流式先出字、离线再校正」，它们本来就不会逐字相同。
func lcsRatio(a, b string) float64 {
	ra, rb := []rune(a), []rune(b)
	if len(ra) == 0 || len(rb) == 0 {
		return 0
	}
	// 只保留两行，避免长句时分配 (n+1)*(m+1) 的二维表。
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for i := 1; i <= len(ra); i++ {
		for j := 1; j <= len(rb); j++ {
			switch {
			case ra[i-1] == rb[j-1]:
				cur[j] = prev[j-1] + 1
			case prev[j] >= cur[j-1]:
				cur[j] = prev[j]
			default:
				cur[j] = cur[j-1]
			}
		}
		prev, cur = cur, prev
	}
	longest := len(ra)
	if len(rb) > longest {
		longest = len(rb)
	}
	return float64(prev[len(rb)]) / float64(longest)
}

// percentile 取 p 分位（最近秩法，样本少时比插值更保守）。
func percentile(xs []time.Duration, p int) time.Duration {
	if len(xs) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), xs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := (len(sorted)*p+99)/100 - 1 // ceil(p% * n) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// passFail 把若干布尔条件渲染成中文判定（日志表格用）。
func passFail(conds ...bool) string {
	for _, ok := range conds {
		if !ok {
			return "不达标"
		}
	}
	return "达标"
}

// base 只取路径最后一段（日志里不打印完整用户路径，NFR-03：不泄漏本机目录）。
func base(p string) string {
	if p == "" {
		return ""
	}
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}
