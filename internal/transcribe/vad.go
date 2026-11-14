package transcribe

import "math"

// 简单 VAD 分句：帧级能量门限 + 静音段判定 + 最小语音时长 + 最大句长。
//
// 为什么自己写而不强依赖 sherpa VAD：VAD 模型包属于可选的 FR-07 下载项，
// 未安装时延后转写仍必须能跑（否则「没下模型就不能转」把默认路径堵死）。
// 需要更准的端点时可注入 extra（见 UtteranceSplitter），但**不强依赖**。
//
// 时间基：全部由**采样帧计数**换算（`samples * 1000 / sampleRate`），不用墙钟
// （ARCH §4.5.1）。因此休眠、设备时钟漂移都不影响时间轴。
const (
	// DefaultFrameMs 分析帧长（20ms 是语音处理的常规粒度）。
	DefaultFrameMs = 20
	// DefaultSilenceMs 连续静音超过该时长判定句尾。
	DefaultSilenceMs = 500
	// DefaultMinSpeechMs 单句最短语音时长，短于此视为噪声丢弃。
	DefaultMinSpeechMs = 200
	// DefaultMaxSegmentMs 单句最长时长（超长强制切分，避免一句话撑爆引擎显存）。
	DefaultMaxSegmentMs = 15000
	// DefaultPaddingMs 句首句尾保留的余量（防止切掉字头字尾）。
	DefaultPaddingMs = 100
	// DefaultEnergyThreshold 帧 RMS 门限（-40dBFS 左右；安静房间实测可过）。
	DefaultEnergyThreshold = 0.01
)

// VadConfig 是分句参数。
//
// 零值字段在 Normalize 里填默认值，因此测试只需要改自己关心的项。
type VadConfig struct {
	// SampleRate 采样率；<=0 视为 16000。
	SampleRate int
	// FrameMs 分析帧长（毫秒）。
	FrameMs int
	// SilenceMs 句尾静音判定时长（毫秒）。
	SilenceMs int
	// MinSpeechMs 最短语音时长（毫秒）。
	MinSpeechMs int
	// MaxSegmentMs 单句最长时长（毫秒）。
	MaxSegmentMs int
	// PaddingMs 句首句尾余量（毫秒）。
	PaddingMs int
	// EnergyThreshold 帧 RMS 门限。
	EnergyThreshold float64
}

// Normalize 用默认值填充零值字段。
func (c VadConfig) Normalize() VadConfig {
	if c.SampleRate <= 0 {
		c.SampleRate = 16000
	}
	if c.FrameMs <= 0 {
		c.FrameMs = DefaultFrameMs
	}
	if c.SilenceMs <= 0 {
		c.SilenceMs = DefaultSilenceMs
	}
	if c.MinSpeechMs <= 0 {
		c.MinSpeechMs = DefaultMinSpeechMs
	}
	if c.MaxSegmentMs <= 0 {
		c.MaxSegmentMs = DefaultMaxSegmentMs
	}
	if c.PaddingMs < 0 {
		c.PaddingMs = DefaultPaddingMs
	}
	if c.PaddingMs == 0 {
		c.PaddingMs = DefaultPaddingMs
	}
	if c.EnergyThreshold <= 0 {
		c.EnergyThreshold = DefaultEnergyThreshold
	}
	if c.MaxSegmentMs < c.MinSpeechMs {
		c.MaxSegmentMs = c.MinSpeechMs
	}
	return c
}

// Utterance 是一「句」切分结果。
//
// Samples 是送进引擎的音频；StartMs/EndMs 是时间轴区间（帧计数换算）。
type Utterance struct {
	// Index 在整段音频中的句子序号（从 0），**同时用作片段 seq**。
	//
	// 之所以用「分句序号」而不是「已落库条数」当 seq：断点续跑时重新分句必须
	// 得到与上次完全一致的编号，否则会覆盖错行。分句对同一输入是确定性的。
	Index      int
	Samples    []float32
	SampleRate int
	StartMs    int64
	EndMs      int64
	// SpeechMs 该句内被判定为语音的时长，用于诊断日志（不含音频内容）。
	SpeechMs int64
}

// samplesToMs 把采样帧数换算成毫秒（帧计数换算，ARCH §4.5.1）。
func samplesToMs(samples int64, sampleRate int) int64 {
	if sampleRate <= 0 {
		return 0
	}
	return samples * 1000 / int64(sampleRate)
}

// UtteranceSplitter 是增量分句器：喂入任意长度的 PCM，吐出已判定完结的句子。
//
// 用法：
//
//	sp := NewUtteranceSplitter(VadConfig{})
//	for 每窗音频 { for _, u := range sp.Push(pcm) { 处理 } }
//	for _, u := range sp.Flush() { 处理 }   // 收尾
//
// 非并发安全：一个转写任务持有一个实例。
type UtteranceSplitter struct {
	cfg        VadConfig
	frameLen   int // 每分析帧采样数
	padSamples int // 句首句尾余量（采样数）
	silenceMax int // 句尾静音判定（采样数）
	minSpeech  int // 最短语音（采样数）
	maxSegment int // 单句最长（采样数）

	// pre 是「尚未进入语音」时保留的预卷，用于把句首余量补回去。
	pre []float32
	// cur 是当前累积中的句子音频。
	cur []float32
	// curStart 是 cur[0] 在整段音频中的采样偏移。
	curStart int64
	// inSpeech 是否已进入语音段。
	inSpeech bool
	// trailingSilence 当前句尾连续静音采样数。
	trailingSilence int
	// speechSamples 当前句内判定为语音的采样数。
	speechSamples int
	// consumed 已消费的采样总数（决定 curStart 与时间轴）。
	consumed int64
	// nextIndex 下一句的序号。
	nextIndex int
}

// NewUtteranceSplitter 构造分句器。
func NewUtteranceSplitter(cfg VadConfig) *UtteranceSplitter {
	cfg = cfg.Normalize()
	frameLen := cfg.SampleRate * cfg.FrameMs / 1000
	if frameLen <= 0 {
		frameLen = 1
	}
	return &UtteranceSplitter{
		cfg:        cfg,
		frameLen:   frameLen,
		padSamples: cfg.SampleRate * cfg.PaddingMs / 1000,
		silenceMax: cfg.SampleRate * cfg.SilenceMs / 1000,
		minSpeech:  cfg.SampleRate * cfg.MinSpeechMs / 1000,
		maxSegment: cfg.SampleRate * cfg.MaxSegmentMs / 1000,
	}
}

// Config 返回归一化后的配置。
func (s *UtteranceSplitter) Config() VadConfig { return s.cfg }

// Push 喂入一段 PCM（16kHz 单声道 [-1,1]），返回本次判定完结的句子。
func (s *UtteranceSplitter) Push(pcm []float32) []Utterance {
	var out []Utterance
	rest := pcm
	for len(rest) > 0 {
		n := s.frameLen
		if n > len(rest) {
			n = len(rest)
		}
		frame := rest[:n]
		rest = rest[n:]

		// 不足一个完整分析帧时按实际长度算能量（避免末尾丢音）。
		speech := frameRMS(frame) >= s.cfg.EnergyThreshold
		if !s.inSpeech {
			// 未进入语音：只保留末尾 padding 长度的预卷。
			s.pre = append(s.pre, frame...)
			if len(s.pre) > s.padSamples {
				s.pre = s.pre[len(s.pre)-s.padSamples:]
			}
			if !speech {
				s.consumed += int64(len(frame))
				continue
			}
			// 命中语音：把预卷接回句首。
			s.curStart = s.consumed - int64(len(s.pre)) + int64(len(frame))
			s.cur = append(s.cur[:0], s.pre...)
			s.cur = append(s.cur, frame...)
			s.consumed += int64(len(frame))
			s.pre = s.pre[:0]
			s.inSpeech = true
			s.trailingSilence = 0
			s.speechSamples = len(frame)
			continue
		}

		s.cur = append(s.cur, frame...)
		s.consumed += int64(len(frame))
		if speech {
			s.speechSamples += len(frame)
			s.trailingSilence = 0
		} else {
			s.trailingSilence += len(frame)
		}

		// 句尾判定：静音够长；或超出单句上限（强制切分，避免无限累积）。
		closeBySilence := s.trailingSilence >= s.silenceMax
		closeByLength := len(s.cur) >= s.maxSegment
		if !closeBySilence && !closeByLength {
			continue
		}
		if u, ok := s.close(closeBySilence); ok {
			out = append(out, u)
		}
	}
	return out
}

// Flush 在音频结束时收尾，返回最后一句（若有）。
func (s *UtteranceSplitter) Flush() []Utterance {
	if !s.inSpeech {
		s.pre = s.pre[:0]
		return nil
	}
	if u, ok := s.close(true); ok {
		return []Utterance{u}
	}
	return nil
}

// close 结算当前句：裁剪句尾多余静音、过滤过短噪声、返回句子。
//
// bySilence=true 表示句尾静音过长，需要只保留 padding 长度的静音尾巴；
// false 表示因超长强制切分，整段保留。
func (s *UtteranceSplitter) close(bySilence bool) (Utterance, bool) {
	keep := len(s.cur)
	if bySilence {
		// 句尾最多保留 paddingSamples 静音（句中的停顿不动）。
		if s.trailingSilence > s.padSamples {
			keep -= s.trailingSilence - s.padSamples
		}
	}
	if keep < 0 {
		keep = 0
	}
	samples := s.cur[:keep]

	// 丢弃过短（噪声 / 咳嗽 / 单帧咔哒声）。
	usable := s.speechSamples >= s.minSpeech

	idx := s.nextIndex
	startMs := samplesToMs(s.curStart, s.cfg.SampleRate)
	endMs := samplesToMs(s.curStart+int64(keep), s.cfg.SampleRate)
	speechMs := samplesToMs(int64(s.speechSamples), s.cfg.SampleRate)

	// 重置，准备下一句（强制切分时同样从零开始，句子之间不重叠）。
	s.cur = s.cur[:0]
	s.inSpeech = false
	s.trailingSilence = 0
	s.speechSamples = 0
	// 已丢弃的预卷对下一句没有意义：强制切分后新句从当前采样位置起算。
	s.pre = s.pre[:0]

	if !usable {
		return Utterance{}, false
	}
	s.nextIndex = idx + 1
	return Utterance{
		Index:      idx,
		Samples:    samples,
		SampleRate: s.cfg.SampleRate,
		StartMs:    startMs,
		EndMs:      endMs,
		SpeechMs:   speechMs,
	}, true
}

// frameRMS 计算一帧的均方根能量。
func frameRMS(frame []float32) float64 {
	if len(frame) == 0 {
		return 0
	}
	var sum float64
	for _, v := range frame {
		sum += float64(v) * float64(v)
	}
	return math.Sqrt(sum / float64(len(frame)))
}

// SplitAll 是一次性分句的便捷封装（测试与短音频用）。
//
// 长音频请用 UtteranceSplitter 逐窗喂入，避免整段驻留内存。
func SplitAll(pcm []float32, cfg VadConfig) []Utterance {
	sp := NewUtteranceSplitter(cfg)
	out := sp.Push(pcm)
	out = append(out, sp.Flush()...)
	return out
}
