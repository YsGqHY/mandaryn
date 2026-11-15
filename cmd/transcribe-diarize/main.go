//go:build windows && cgo

// 离线转写 + 说话人分离的一次性验证工具。
//
//	go run ./cmd/transcribe-diarize -wav x.wav -out out.txt
//
// 模型来自 sherpa-onnx 官方权重（调研.md 推荐的中文主力：SenseVoice），
// 说话人分离用 pyannote segmentation-3.0 + CAM++ 声纹（对应调研.md 的 FunASR+CAM++ 路线）。
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	sherpa "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"
)

func main() {
	var (
		wavPath   = flag.String("wav", "", "输入 wav（16bit PCM）")
		outPath   = flag.String("out", "", "输出 txt")
		modelDir  = flag.String("models", ".narrafork/models", "模型根目录")
		language  = flag.String("lang", "zh", "SenseVoice 语种")
		numThread = flag.Int("threads", 4, "推理线程数")
		// numSpeakers>0 时强制人数；0 表示让模型按阈值自行聚类（用于交叉验证人数估计）。
		numSpeakers = flag.Int("speakers", 2, "预设说话人数；0=由模型自动判定")
	)
	flag.Parse()

	if *wavPath == "" || *outPath == "" {
		fatal("必须指定 -wav 与 -out")
	}

	sv := filepath.Join(*modelDir, "sensevoice", "model.int8.onnx")
	tokens := filepath.Join(*modelDir, "sensevoice", "tokens.txt")
	seg := filepath.Join(*modelDir, "pyannote-seg", "model.onnx")
	spk := filepath.Join(*modelDir, "campplus", "campplus_zh.onnx")
	for _, p := range []string{sv, tokens, seg, spk} {
		if _, err := os.Stat(p); err != nil {
			fatal("缺少模型文件 %s: %v", p, err)
		}
	}

	samples, rate, err := readWav16(*wavPath)
	if err != nil {
		fatal("读取 wav 失败: %v", err)
	}
	if rate != 16000 {
		// sherpa-onnx 的 VAD/分离/声纹都按 16k 训练；这里做一次线性重采样。
		samples = resample(samples, rate, 16000)
		rate = 16000
	}
	fmt.Printf("音频: %s  %.2fs @%dHz\n", filepath.Base(*wavPath), float64(len(samples))/float64(rate), rate)

	// 1) VAD 切段：拿到语音区间，逐段转写即可得到段级时间戳。
	vadCfg := sherpa.VadModelConfig{}
	vadCfg.SileroVad.Model = "" // 用内置 VAD（sherpa-onnx 自带），未配置时走默认
	vadCfg.SampleRate = rate
	vadCfg.NumThreads = 1
	vadCfg.Provider = "cpu"
	vadCfg.SileroVad.Threshold = 0.5
	vadCfg.SileroVad.MinSilenceDuration = 0.5
	vadCfg.SileroVad.MinSpeechDuration = 0.25
	vadCfg.SileroVad.MaxSpeechDuration = 20.0
	windowSize := 512

	cfg := sherpa.OfflineRecognizerConfig{}
	cfg.FeatConfig.SampleRate = rate
	cfg.FeatConfig.FeatureDim = 80
	cfg.ModelConfig.Tokens = tokens
	cfg.ModelConfig.NumThreads = *numThread
	cfg.ModelConfig.Provider = "cpu"
	cfg.ModelConfig.SenseVoice.Model = sv
	cfg.ModelConfig.SenseVoice.Language = *language
	cfg.ModelConfig.SenseVoice.UseInverseTextNormalization = 1
	cfg.DecodingMethod = "greedy_search"

	rec := sherpa.NewOfflineRecognizer(&cfg)
	if rec == nil {
		fatal("初始化 SenseVoice 识别器失败")
	}
	defer sherpa.DeleteOfflineRecognizer(rec)

	segments := transcribeWithVAD(rec, &vadCfg, samples, windowSize, rate)
	if len(segments) == 0 {
		fatal("VAD 没有切出任何语音段")
	}
	fmt.Printf("VAD 切出 %d 段，转写完成\n", len(segments))

	// 2) 说话人分离
	diarCfg := sherpa.OfflineSpeakerDiarizationConfig{}
	diarCfg.Segmentation.Pyannote.Model = seg
	diarCfg.Segmentation.NumThreads = *numThread
	diarCfg.Segmentation.Provider = "cpu"
	diarCfg.Embedding.Model = spk
	diarCfg.Embedding.NumThreads = *numThread
	diarCfg.Embedding.Provider = "cpu"
	// 音频里就是两个人对话时，指定人数比让模型自己猜稳；
	// -speakers 0 则交给阈值聚类，用于交叉验证人数估计是否一致。
	if *numSpeakers > 0 {
		diarCfg.Clustering.NumClusters = *numSpeakers
	} else {
		diarCfg.Clustering.NumClusters = 0
		diarCfg.Clustering.Threshold = 0.5
	}
	diarCfg.Clustering.ComputeConfidence = 1
	diarCfg.MinDurationOn = 0.3
	diarCfg.MinDurationOff = 0.5
	diarCfg.Segmentation.Debug = 0
	diarCfg.Embedding.Debug = 0
	sd := sherpa.NewOfflineSpeakerDiarization(&diarCfg)
	if sd == nil {
		fatal("初始化说话人分离失败")
	}
	defer sherpa.DeleteOfflineSpeakerDiarization(sd)
	turns := sd.Process(samples)
	fmt.Printf("分离出 %d 个说话人片段\n", len(turns))

	// 3) 对齐并输出
	lines := align(segments, turns)
	txt := renderReport(*wavPath, rate, len(samples), segments, turns, lines)
	if err := os.WriteFile(*outPath, []byte(txt), 0o644); err != nil {
		fatal("写输出失败: %v", err)
	}
	fmt.Printf("已写入 %s（%d 行对话）\n", *outPath, len(lines))
}

type asrSeg struct {
	Start, End float64
	Text       string
}

func fatal(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "错误: "+f+"\n", a...)
	os.Exit(1)
}

// transcribeWithVAD 用 VAD 切段并逐段识别，得到带时间戳的转写结果。
func transcribeWithVAD(rec *sherpa.OfflineRecognizer, vadCfg *sherpa.VadModelConfig,
	samples []float32, windowSize, rate int) []asrSeg {

	// 简化：不依赖 sherpa 的 VoiceActivityDetector C 对象（其 Go 绑定在各平台差异较大），
	// 直接用能量+静音门槛做切段，够本验证用，且完全可解释。
	return energySegmentAndTranscribe(rec, samples, rate)
}

// energySegmentAndTranscribe 基于短时能量切段后逐段识别。
//
// 为什么不用 sherpa 内置 VAD：这个工具只做一次性验证，能量切段的结果可复核、
// 无隐藏状态；正式管线（internal/asr）仍应走 VAD。
func energySegmentAndTranscribe(rec *sherpa.OfflineRecognizer, samples []float32, rate int) []asrSeg {
	const (
		frameMs      = 20
		silenceMs    = 500 // 连续静音超过这个长度视为句子边界
		minSpeechMs  = 300 // 过短的段丢弃
		energyFactor = 0.06
	)
	frame := rate * frameMs / 1000
	if frame <= 0 {
		return nil
	}

	// 全局能量参考值（用高分位估计，避免被个别爆音拉偏）。
	energies := make([]float64, 0, len(samples)/frame+1)
	for i := 0; i+frame <= len(samples); i += frame {
		var sum float64
		for _, v := range samples[i : i+frame] {
			sum += float64(v) * float64(v)
		}
		energies = append(energies, sum/float64(frame))
	}
	if len(energies) == 0 {
		return nil
	}
	sorted := append([]float64(nil), energies...)
	sort.Float64s(sorted)
	ref := sorted[int(float64(len(sorted))*0.90)]
	thr := ref * energyFactor
	if thr <= 1e-9 {
		thr = 1e-6
	}

	type span struct{ s, e int }
	var spans []span
	start := -1
	silent := 0
	for idx, e := range energies {
		speech := e > thr
		if speech {
			if start < 0 {
				start = idx
			}
			silent = 0
		} else if start >= 0 {
			silent++
			if silent*frameMs >= silenceMs {
				end := idx - silent
				if (end-start)*frameMs >= minSpeechMs {
					spans = append(spans, span{start, end})
				}
				start, silent = -1, 0
			}
		}
	}
	if start >= 0 {
		end := len(energies)
		if (end-start)*frameMs >= minSpeechMs {
			spans = append(spans, span{start, end})
		}
	}

	out := make([]asrSeg, 0, len(spans))
	for _, sp := range spans {
		bs := sp.s * frame
		be := sp.e * frame
		if be > len(samples) {
			be = len(samples)
		}
		if be-bs < frame {
			continue
		}
		// 前后各留 100ms 上下文，切词边界更稳。
		pad := rate / 10
		ps, pe := bs-pad, be+pad
		if ps < 0 {
			ps = 0
		}
		if pe > len(samples) {
			pe = len(samples)
		}
		st := sherpa.NewOfflineStream(rec)
		st.AcceptWaveform(rate, samples[ps:pe])
		rec.Decode(st)
		text := strings.TrimSpace(st.GetResult().Text)
		sherpa.DeleteOfflineStream(st)
		if text == "" {
			continue
		}
		out = append(out, asrSeg{
			Start: float64(bs) / float64(rate),
			End:   float64(be) / float64(rate),
			Text:  text,
		})
	}
	return out
}

type turn struct {
	Start, End float64
	Speaker    int
}

// align 把每段转写归属到重叠时长最大的说话人。
func align(segs []asrSeg, turns []sherpa.OfflineSpeakerDiarizationSegment) []string {
	tt := make([]turn, 0, len(turns))
	for _, t := range turns {
		tt = append(tt, turn{Start: float64(t.Start), End: float64(t.End), Speaker: t.Speaker})
	}

	lines := make([]string, 0, len(segs))
	for _, s := range segs {
		best, bestOverlap := -1, 0.0
		for _, t := range tt {
			ov := overlap(s.Start, s.End, t.Start, t.End)
			if ov > bestOverlap {
				bestOverlap, best = ov, t.Speaker
			}
		}
		if best < 0 {
			lines = append(lines, fmt.Sprintf("[%s] 未归属：%s", ts(s.Start), s.Text))
			continue
		}
		lines = append(lines, fmt.Sprintf("角色%s：%s", roleName(best), s.Text))
	}
	return lines
}

func overlap(a1, a2, b1, b2 float64) float64 {
	s := a1
	if b1 > s {
		s = b1
	}
	e := a2
	if b2 < e {
		e = b2
	}
	if e <= s {
		return 0
	}
	return e - s
}

// roleName 把说话人下标映射成「A/B/C…」。
func roleName(i int) string {
	if i < 0 {
		return "?"
	}
	return string(rune('A' + i%26))
}

func ts(sec float64) string {
	m := int(sec) / 60
	s := sec - float64(m*60)
	return fmt.Sprintf("%02d:%05.2f", m, s)
}

// renderReport 生成最终 txt（含方法说明、时间轴与说话人统计）。
func renderReport(wav string, rate, sampleCount int, segs []asrSeg,
	turns []sherpa.OfflineSpeakerDiarizationSegment, lines []string) string {

	var b strings.Builder
	b.WriteString("# 转写结果\n\n")
	fmt.Fprintf(&b, "音频文件：%s\n", filepath.Base(wav))
	fmt.Fprintf(&b, "时长：%.2f 秒\n", float64(sampleCount)/float64(rate))
	b.WriteString("识别模型：SenseVoice-Small（sherpa-onnx INT8，Apache-2.0）\n")
	b.WriteString("说话人分离：pyannote-segmentation-3.0 + CAM++（3D-Speaker 中文声纹）\n")
	fmt.Fprintf(&b, "语音段：%d 段；说话人片段：%d 段\n\n", len(segs), len(turns))

	b.WriteString("对话文本：\n\n")
	for _, l := range lines {
		b.WriteString(l)
		b.WriteString("\n")
	}

	// 时间轴：把 ASR 段与分离结果并排给出，便于核对归属是否可靠。
	b.WriteString("\n--- 时间轴（供核对）---\n\n")
	b.WriteString("ASR 语音段：\n")
	for _, s := range segs {
		fmt.Fprintf(&b, "  [%s - %s] %.2fs  %s\n",
			ts(s.Start), ts(s.End), s.End-s.Start, s.Text)
	}
	b.WriteString("\n说话人分离片段：\n")
	for _, t := range turns {
		conf := ""
		if t.Confidence > -1.5 {
			conf = fmt.Sprintf(" conf=%.2f", t.Confidence)
		}
		fmt.Fprintf(&b, "  [%s - %s] %.2fs  说话人%d (角色%s)%s\n",
			ts(float64(t.Start)), ts(float64(t.End)),
			float64(t.End-t.Start), t.Speaker, roleName(t.Speaker), conf)
	}
	return b.String()
}

// —— wav 读取与重采样 ——

func readWav16(path string) ([]float32, int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, err
	}
	if len(raw) < 12 || string(raw[0:4]) != "RIFF" || string(raw[8:12]) != "WAVE" {
		return nil, 0, fmt.Errorf("不是 RIFF/WAVE 文件")
	}
	var (
		channels, bits, rate int
		dataOff, dataLen     int
	)
	pos := 12
	for pos+8 <= len(raw) {
		id := string(raw[pos : pos+4])
		size := int(binary.LittleEndian.Uint32(raw[pos+4 : pos+8]))
		body := pos + 8
		if body+size > len(raw) {
			size = len(raw) - body
		}
		switch id {
		case "fmt ":
			channels = int(binary.LittleEndian.Uint16(raw[body+2 : body+4]))
			rate = int(binary.LittleEndian.Uint32(raw[body+4 : body+8]))
			bits = int(binary.LittleEndian.Uint16(raw[body+14 : body+16]))
		case "data":
			dataOff, dataLen = body, size
		}
		pos = body + size
		if size%2 == 1 {
			pos++
		}
	}
	if bits != 16 || channels == 0 || rate == 0 || dataOff == 0 {
		return nil, 0, fmt.Errorf("仅支持 16bit PCM（ch=%d bits=%d rate=%d）", channels, bits, rate)
	}

	frames := dataLen / (2 * channels)
	mono := make([]float32, frames)
	for f := 0; f < frames; f++ {
		var sum int32
		for c := 0; c < channels; c++ {
			off := dataOff + (f*channels+c)*2
			sum += int32(int16(binary.LittleEndian.Uint16(raw[off : off+2])))
		}
		mono[f] = float32(sum/int32(channels)) / 32768.0
	}
	return mono, rate, nil
}

// resample 线性重采样（验证够用；正式管线用 sherpa 内置重采样）。
func resample(in []float32, from, to int) []float32 {
	if from == to || len(in) == 0 {
		return in
	}
	outLen := int(float64(len(in)) * float64(to) / float64(from))
	out := make([]float32, outLen)
	ratio := float64(from) / float64(to)
	for i := 0; i < outLen; i++ {
		x := float64(i) * ratio
		i0 := int(x)
		if i0 >= len(in)-1 {
			out[i] = in[len(in)-1]
			continue
		}
		frac := float32(x - float64(i0))
		out[i] = in[i0]*(1-frac) + in[i0+1]*frac
	}
	return out
}
