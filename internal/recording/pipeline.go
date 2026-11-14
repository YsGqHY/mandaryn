package recording

import (
	"fmt"
	"math"
	"sync"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/capture"
)

// KernelHalfTaps 重采样核的半宽（以**输入样本**为单位的基准值）。
//
// 16 抽头半宽 = 32 抽头核，Blackman 窗下阻带衰减约 -74dB、过渡带约 2/32 归一化频率，
// 对 16kHz 语音识别足够（更宽的核只会线性增加 CPU，听感与识别率不再提升）。
const KernelHalfTaps = 16.0

// Resampler 把任意采样率的单声道流式重采样到内部采样率。
//
// 实现选择：**带窗 sinc 插值**（Blackman 窗），而不是线性插值。
//
//	为什么：48k → 16k 是 3 倍降采样，线性插值等于一个极差的低通，会
//	把 8kHz 以上的能量**混叠**进 0-8kHz 语音带，直接表现为啸叫与识别率下降。
//	ARCH §4.3 明确要求带窗 sinc。
//
//	为什么用「无相位表」的直接求和而不是多相表：源采样率可能是 44.1k/48k/96k
//	甚至 47999Hz（设备名义速率与实际不符），多相表需要整数倍比，会引入
//	周期性比率误差。直接按浮点中心位置求和的实现对任意比率都成立，
//	代价是每个输出样本多几十次乘加（16k 输出下 < 5M MAC/s，可忽略）。
//
// 增益说明：每个输出样本的系数和会归一化为 1，因此 DC 增益严格为 1 ——
// 做出这一约束是为了让「混音 + 重采样」这条链路的直流分量可精确断言。
type Resampler struct {
	inRate  int
	outRate int
	// step 每个输出样本在输入轴上前进的样本数（inRate/outRate）。
	step float64
	// ratio 截止频率缩放（outRate/inRate）；降采样时 < 1，用于展宽核抑制混叠。
	ratio float64
	// halfWidth 核半宽（输入样本单位）= KernelHalfTaps / ratio。
	halfWidth float64

	// buf 尚未消费的输入样本；pos 是「下一个输出样本」在 buf 中的浮点位置。
	buf []float32
	pos float64

	// passthrough 源采样率已等于内部采样率：直通（零延迟、零误差）。
	//
	// 没有这条快路径时，16k→16k 会走 sinc 核，虽然整数位置上的核值恰好是
	// delta 函数（数学上仍是直通），但流式实现必须等核支撑区填满才能产出，
	// 于是输出比输入**少 2*halfWidth 个样本**——这种「少几十个样本」的偏差
	// 在会话时长上体现为几毫秒，且边界样本被截断。直通路径同时更省 CPU。
	passthrough bool

	totalIn  int64
	totalOut int64
	flushed  bool
}

// NewResampler 构造重采样器；输入/输出采样率必须为正。
func NewResampler(inRate, outRate int) (*Resampler, error) {
	if inRate <= 0 {
		return nil, apierr.New(apierr.CodeInvalidArgument).
			WithDetail("源采样率非法：%d", inRate)
	}
	if outRate <= 0 {
		return nil, apierr.New(apierr.CodeInvalidArgument).
			WithDetail("目标采样率非法：%d", outRate)
	}
	r := &Resampler{inRate: inRate, outRate: outRate}
	if inRate == outRate {
		r.passthrough = true
		r.ratio = 1
		r.step = 1
		r.halfWidth = 0
		return r, nil
	}
	r.step = float64(inRate) / float64(outRate)
	r.ratio = float64(outRate) / float64(inRate)
	if r.ratio > 1 {
		// 升采样不需要抗混叠，核按输入 Nyquist 截止即可（ratio=1 的宽度）。
		r.halfWidth = KernelHalfTaps
	} else {
		r.halfWidth = KernelHalfTaps / r.ratio
	}
	// 初始中心放在 halfWidth 处，保证第一组抽头的下标非负。
	// 这引入 halfWidth 个输入样本的固定群延迟（约 1-3ms），对时间轴无影响：
	// 时间轴由帧计数决定，而帧计数从输出的第一个样本开始算。
	r.pos = r.halfWidth
	return r, nil
}

// InputRate 源采样率。
func (r *Resampler) InputRate() int { return r.inRate }

// OutputRate 目标采样率。
func (r *Resampler) OutputRate() int { return r.outRate }

// TotalIn 已投入的输入样本数。
func (r *Resampler) TotalIn() int64 { return r.totalIn }

// TotalOut 已产出的输出样本数。
func (r *Resampler) TotalOut() int64 { return r.totalOut }

// Process 投入一批单声道输入样本，返回本次可产出的输出样本。
//
// 返回值是新分配的切片：调用方可能持有它跨越下一次 Process（写入落盘缓冲），
// 用内部复用缓冲虽然少一次分配，但会让「上一次结果被静默改写」成为隐患。
func (r *Resampler) Process(in []float32) []float32 {
	if len(in) > 0 {
		r.totalIn += int64(len(in))
	}
	if r.passthrough {
		if len(in) == 0 {
			return nil
		}
		out := make([]float32, len(in))
		copy(out, in)
		r.totalOut += int64(len(out))
		return out
	}
	if len(in) > 0 {
		r.buf = append(r.buf, in...)
	}
	return r.produce(nil)
}

// Flush 排空尾部：内核支撑区越界的部分按 0 补齐，使输出样本数与
// `totalIn * outRate/inRate`（四舍五入）严格一致。
//
// 为什么必须产出到期望长度：会话时长 = 帧数换算，少一批帧会让
// 落盘时长与源音频时长差几毫秒，回放末尾会「听不全最后一个字」。
func (r *Resampler) Flush() []float32 {
	if r.flushed {
		return nil
	}
	r.flushed = true
	if r.passthrough {
		return nil
	}
	// 补 halfWidth+1 个零，保证期望长度内的所有输出样本都能算出（缺失抽头按 0）。
	pad := int(math.Ceil(r.halfWidth)) + 1
	r.buf = append(r.buf, make([]float32, pad)...)
	out := r.produce(nil)
	// 冲刷后不再需要保留输入。
	r.buf = nil
	r.pos = 0
	return out
}

// expectedOut 按当前输入总量算出应有的输出样本数（四舍五入）。
func (r *Resampler) expectedOut() int64 {
	return int64(math.Round(float64(r.totalIn) / r.step))
}

// produce 产出可算的输出样本。
func (r *Resampler) produce(dst []float32) []float32 {
	expected := r.expectedOut()
	for r.totalOut < expected {
		center := r.pos
		lo := int(math.Floor(center - r.halfWidth))
		hi := int(math.Ceil(center + r.halfWidth))
		if hi >= len(r.buf) {
			// 尾部数据还没到：等下一次 Process（或 Flush 补零）。
			break
		}
		if lo < 0 {
			lo = 0
		}
		var acc, wsum float64
		for i := lo; i <= hi; i++ {
			d := float64(i) - center
			u := d / r.halfWidth
			if u <= -1 || u >= 1 {
				continue
			}
			w := sinc(r.ratio*d) * blackman(u)
			wsum += w
			if i < len(r.buf) {
				acc += float64(r.buf[i]) * w
			}
		}
		if wsum != 0 {
			acc /= wsum
		}
		if acc > 1 {
			acc = 1
		} else if acc < -1 {
			acc = -1
		}
		dst = append(dst, float32(acc))
		r.totalOut++
		r.pos += r.step
	}
	r.trim()
	return dst
}

// trim 丢弃核支撑区已经走过的输入样本（永不再需要），避免缓冲无界增长。
func (r *Resampler) trim() {
	drop := int(math.Floor(r.pos - r.halfWidth))
	if drop <= 0 {
		return
	}
	if drop > len(r.buf) {
		drop = len(r.buf)
	}
	r.buf = append(r.buf[:0], r.buf[drop:]...)
	r.pos -= float64(drop)
}

// sinc 归一化 sinc：sin(πx)/(πx)，x=0 时为 1。
func sinc(x float64) float64 {
	if x == 0 {
		return 1
	}
	px := math.Pi * x
	return math.Sin(px) / px
}

// blackman Blackman 窗（u ∈ [-1,1]，u=0 时为 1，两端为 0）。
//
// 选 Blackman 而不是 Hann：Hann 的一阶旁瓣滚降在 3 倍降采样时仍会漏出
// 约 -31dB 的混叠能量，Blackman 约 -58dB 起，配合 32 抽头足够淹没在
// 语音带噪声下。
func blackman(u float64) float64 {
	if u <= -1 || u >= 1 {
		return 0
	}
	return 0.42 + 0.5*math.Cos(math.Pi*u) + 0.08*math.Cos(2*math.Pi*u)
}

// mixToMono 把 interleaved 的 float32 样本混成单声道（求平均）。
//
// 策略选择：**平均**而不是取左声道或相加。
//   - 取单声道会丢掉另一侧的语音（系统回环里不同应用常在不同声道上更突出）；
//   - 直接相加会溢出到 [-2,2]，需要额外限幅，且整体响度翻倍会影响
//     后续 VAD 的绝对阈值口径；
//   - 平均是等功率近似（立体声同相内容保持原幅度），且天然不需要限幅。
//
// 输出复用 dst 的容量（长度不足时追加），避免每 10ms 分配一次。
func mixToMono(interleaved []float32, channels int, dst []float32) []float32 {
	if channels <= 1 {
		return append(dst, interleaved...)
	}
	frames := len(interleaved) / channels
	need := len(dst) + frames
	if cap(dst) < need {
		grown := make([]float32, len(dst), need)
		copy(grown, dst)
		dst = grown
	}
	scale := 1 / float32(channels)
	for f := 0; f < frames; f++ {
		base := f * channels
		var sum float32
		for c := 0; c < channels; c++ {
			sum += interleaved[base+c]
		}
		dst = append(dst, sum*scale)
	}
	return dst
}

// RingBuffer 固定容量的环形缓冲（单生产者/单消费者，内部加锁）。
//
// 语义与 ARCH §4.3 一致：满时**丢最旧**并计数，绝不阻塞生产者 ——
// 采集线程被阻塞会直接导致源侧丢包（WASAPI 队列溢出），比丢几毫秒旧音频更糟。
type RingBuffer struct {
	mu      sync.Mutex
	buf     []float32
	head    int
	size    int
	dropped int64
}

// NewRingBuffer 构造容量为 capacity 个样本的环形缓冲。
func NewRingBuffer(capacity int) *RingBuffer {
	if capacity <= 0 {
		capacity = RingBufferCapacity
	}
	return &RingBuffer{buf: make([]float32, capacity)}
}

// Write 写入样本（满时丢最旧）。
func (b *RingBuffer) Write(samples []float32) {
	if len(samples) == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, s := range samples {
		if b.size == len(b.buf) {
			b.head = (b.head + 1) % len(b.buf)
			b.size--
			b.dropped++
		}
		b.buf[(b.head+b.size)%len(b.buf)] = s
		b.size++
	}
}

// Read 读出最多 len(dst) 个样本，返回实际读数。
func (b *RingBuffer) Read(dst []float32) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(dst)
	if n > b.size {
		n = b.size
	}
	for i := 0; i < n; i++ {
		dst[i] = b.buf[(b.head+i)%len(b.buf)]
	}
	b.head = (b.head + n) % len(b.buf)
	b.size -= n
	return n
}

// Len 当前缓冲的样本数。
func (b *RingBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.size
}

// Dropped 累计因溢出丢弃的样本数（>0 时需要告警，说明磁盘或调度跟不上）。
func (b *RingBuffer) Dropped() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.dropped
}

// Cap 容量。
func (b *RingBuffer) Cap() int { return len(b.buf) }

// Pipeline 是音频管线：源格式 → 混音单声道 → 重采样 16kHz → 环形缓冲。
//
// 它只负责「把任意源格式变成内部统一格式并缓存」，不认识会话、不认识数据库，
// 因此可被单测用合成音频精确断言（样本数比例、直流增益、丢帧计数）。
type Pipeline struct {
	format    capture.AudioFormat
	resampler *Resampler
	ring      *RingBuffer

	// mono 是混音中间缓冲，跨调用复用。
	mono []float32
}

// NewPipeline 按源格式构造管线。
//
// 采样率必须 > 0：源格式未经协商就进管线属于上游 bug，静默兜底成 48kHz
// 会让「时长正确但音调错误」这种最难查的问题流入用户现场。
func NewPipeline(format capture.AudioFormat) (*Pipeline, error) {
	if format.SampleRate <= 0 {
		return nil, apierr.New(apierr.CodeInvalidArgument).
			WithDetail("源采样率非法：%d", format.SampleRate)
	}
	channels := format.Channels
	if channels <= 0 {
		channels = 1
	}
	format.Channels = channels

	rs, err := NewResampler(format.SampleRate, InternalSampleRate)
	if err != nil {
		return nil, err
	}
	return &Pipeline{
		format:    format,
		resampler: rs,
		ring:      NewRingBuffer(RingBufferCapacity),
	}, nil
}

// Format 返回源格式（已把非法声道数归一到 1）。
func (p *Pipeline) Format() capture.AudioFormat { return p.format }

// Push 投入一批 interleaved 源样本：混音 → 重采样 → 入环形缓冲。
func (p *Pipeline) Push(interleaved []float32) {
	if len(interleaved) == 0 {
		return
	}
	p.mono = mixToMono(interleaved, p.format.Channels, p.mono[:0])
	if len(p.mono) == 0 {
		return
	}
	p.ring.Write(p.resampler.Process(p.mono))
}

// Flush 结束输入：把重采样器尾部与环形缓冲里剩余的数据都吐出来。
func (p *Pipeline) Flush() {
	p.ring.Write(p.resampler.Flush())
}

// Read 从环形缓冲取出最多 len(dst) 个内部格式样本。
func (p *Pipeline) Read(dst []float32) int { return p.ring.Read(dst) }

// Buffered 环形缓冲中的样本数。
func (p *Pipeline) Buffered() int { return p.ring.Len() }

// Dropped 因环形缓冲溢出丢弃的样本数。
func (p *Pipeline) Dropped() int64 { return p.ring.Dropped() }

// TotalOut 已产出的内部格式样本总数（= 帧数）。
func (p *Pipeline) TotalOut() int64 { return p.resampler.TotalOut() }

// String 便于日志/诊断（只含格式元数据，不含音频内容）。
func (p *Pipeline) String() string {
	return fmt.Sprintf("%dHz/%dch→%dHz/%dch", p.format.SampleRate, p.format.Channels, InternalSampleRate, InternalChannels)
}
