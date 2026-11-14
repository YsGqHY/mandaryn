package recording

import (
	"math"
	"testing"

	"vip.oberon.mandaryn/internal/capture"
)

// TestResamplerLengthRatio 断言 44.1k / 48k / 96k → 16k 的输出样本数与比例一致（误差 < 1%）。
//
// 这条是「时长正确」的地基：会话时长 = 帧数换算，重采样长度错 1% 就是
// 1 小时会话差 36 秒，回放的歌词会整体错位。
func TestResamplerLengthRatio(t *testing.T) {
	cases := []struct {
		inRate int
		ch     int
	}{
		{44100, 2}, {48000, 2}, {96000, 2},
		{48000, 1}, {44100, 1}, {32000, 2}, {22050, 2}, {8000, 1},
	}
	for _, tc := range cases {
		t.Run(itoa(tc.inRate)+"Hz-"+itoa(tc.ch)+"ch", func(t *testing.T) {
			p, err := NewPipeline(capture.AudioFormat{SampleRate: tc.inRate, Channels: tc.ch})
			if err != nil {
				t.Fatalf("构造管线失败：%v", err)
			}
			// 喂 1 秒的源音频（分块喂，模拟真实读取）。
			srcFrames := int64(tc.inRate)
			const chunkFrames = 1024
			buf := make([]float32, chunkFrames*tc.ch)
			var fed int64
			for fed < srcFrames {
				n := int64(chunkFrames)
				if srcFrames-fed < n {
					n = srcFrames - fed
				}
				samples := buf[:int(n)*tc.ch]
				for i := range samples {
					frame := fed + int64(i/tc.ch)
					samples[i] = float32(math.Sin(2 * math.Pi * 440 * float64(frame) / float64(tc.inRate)))
				}
				p.Push(samples)
				fed += n
			}
			p.Flush()

			out := make([]float32, 1024)
			var got int64
			for {
				n := p.Read(out)
				if n == 0 {
					break
				}
				got += int64(n)
			}
			want := float64(srcFrames) * float64(InternalSampleRate) / float64(tc.inRate)
			if math.Abs(float64(got)-want)/want > 0.01 {
				t.Fatalf("重采样输出长度偏差过大：got=%d want≈%.0f（%.2f%%）",
					got, want, math.Abs(float64(got)-want)/want*100)
			}
			if p.Dropped() != 0 {
				t.Fatalf("不应发生环形缓冲溢出，dropped=%d", p.Dropped())
			}
		})
	}
}

// TestResamplerDurationOneSecond 断言 1s 源音频 → 时长仍是 1000ms（帧计数换算）。
func TestResamplerDurationOneSecond(t *testing.T) {
	for _, rate := range []int{44100, 48000, 96000} {
		p, err := NewPipeline(capture.AudioFormat{SampleRate: rate, Channels: 2})
		if err != nil {
			t.Fatalf("构造管线失败：%v", err)
		}
		buf := make([]float32, rate*2)
		p.Push(buf)
		p.Flush()

		out := make([]float32, 4096)
		var frames int64
		for {
			n := p.Read(out)
			if n == 0 {
				break
			}
			frames += int64(n)
		}
		ms := FramesToMs(frames)
		if ms < 990 || ms > 1010 {
			t.Fatalf("%dHz：1s 源音频换算时长 %dms，超出 ±10ms", rate, ms)
		}
	}
}

// TestMonoMixAverages 断言立体声混音策略 = **平均**。
//
// 选平均的理由见 mixToMono 注释：同相保持幅度、反相抵消成 0、
// 不需要额外限幅（相加会溢出到 ±2）。
func TestMonoMixAverages(t *testing.T) {
	// 用 16k→16k 的管线，重采样核在整数位置上等价于直通，便于断言精确值。
	p, err := NewPipeline(capture.AudioFormat{SampleRate: InternalSampleRate, Channels: 2})
	if err != nil {
		t.Fatalf("构造管线失败：%v", err)
	}
	const frames = 800
	in := make([]float32, frames*2)
	for f := 0; f < frames; f++ {
		in[f*2] = 0.2   // L
		in[f*2+1] = 0.6 // R
	}
	p.Push(in)
	got := make([]float32, frames)
	n := p.Read(got)
	if n != frames {
		t.Fatalf("输出帧数不符：got=%d want=%d", n, frames)
	}
	for i := 0; i < n; i++ {
		if math.Abs(float64(got[i])-0.4) > 1e-4 {
			t.Fatalf("第 %d 帧混音结果 %.6f，期望 0.4（(0.2+0.6)/2）", i, got[i])
		}
	}

	// 反相（L=+0.5, R=-0.5）→ 平均后为 0；若实现是「取左声道」这里会失败。
	in2 := make([]float32, frames*2)
	for f := 0; f < frames; f++ {
		in2[f*2] = 0.5
		in2[f*2+1] = -0.5
	}
	p2, _ := NewPipeline(capture.AudioFormat{SampleRate: InternalSampleRate, Channels: 2})
	p2.Push(in2)
	got2 := make([]float32, frames)
	n2 := p2.Read(got2)
	for i := 0; i < n2; i++ {
		if math.Abs(float64(got2[i])) > 1e-4 {
			t.Fatalf("反相输入应抵消为 0，第 %d 帧得到 %.6f", i, got2[i])
		}
	}
}

// TestMonoMixArbitraryChannels 断言任意声道数（5.1 六声道）也能正确混音。
func TestMonoMixArbitraryChannels(t *testing.T) {
	const ch = 6
	p, err := NewPipeline(capture.AudioFormat{SampleRate: InternalSampleRate, Channels: ch})
	if err != nil {
		t.Fatalf("构造管线失败：%v", err)
	}
	const frames = 400
	in := make([]float32, frames*ch)
	for f := 0; f < frames; f++ {
		for c := 0; c < ch; c++ {
			in[f*ch+c] = 0.3
		}
	}
	p.Push(in)
	got := make([]float32, frames)
	n := p.Read(got)
	if n != frames {
		t.Fatalf("输出帧数不符：got=%d want=%d", n, frames)
	}
	if math.Abs(float64(got[10])-0.3) > 1e-4 {
		t.Fatalf("六声道全 0.3 混音应为 0.3，得到 %.6f", got[10])
	}
}

// TestPipelineDCGain 断言带窗 sinc 的直流增益严格为 1（中段样本 == 输入常量）。
//
// 若忘记归一化系数和，48k→16k 的直流会被放大/衰减若干个百分点，
// 表现为「录音音量不对」——这是此类实现最常见的坑。
func TestPipelineDCGain(t *testing.T) {
	p, err := NewPipeline(capture.AudioFormat{SampleRate: 48000, Channels: 1})
	if err != nil {
		t.Fatalf("构造管线失败：%v", err)
	}
	in := make([]float32, 48000)
	for i := range in {
		in[i] = 0.4
	}
	p.Push(in)
	p.Flush()
	out := make([]float32, 16000)
	var all []float32
	for {
		n := p.Read(out)
		if n == 0 {
			break
		}
		all = append(all, out[:n]...)
	}
	if len(all) < 15000 {
		t.Fatalf("输出样本过少：%d", len(all))
	}
	// 跳过两端（核边缘），中段应严格等于 0.4。
	for i := 100; i < len(all)-100; i++ {
		if math.Abs(float64(all[i])-0.4) > 2e-3 {
			t.Fatalf("第 %d 个输出样本 %.6f，期望 0.4（直流增益应为 1）", i, all[i])
		}
	}
}

// TestPipelineRejectsBadFormat 断言非法源格式直接报错而不是静默兜底。
func TestPipelineRejectsBadFormat(t *testing.T) {
	if _, err := NewPipeline(capture.AudioFormat{SampleRate: 0, Channels: 2}); err == nil {
		t.Fatal("采样率为 0 应返回错误")
	}
	if _, err := NewPipeline(capture.AudioFormat{SampleRate: -44100, Channels: 2}); err == nil {
		t.Fatal("负采样率应返回错误")
	}
	// 声道数 0 归一到单声道（某些平台在没协商成功时会给出 0）。
	p, err := NewPipeline(capture.AudioFormat{SampleRate: 16000, Channels: 0})
	if err != nil {
		t.Fatalf("声道数 0 应被归一为单声道：%v", err)
	}
	if p.Format().Channels != 1 {
		t.Fatalf("声道数应归一为 1，得到 %d", p.Format().Channels)
	}
}

// TestRingBufferDropsOldest 断言环形缓冲溢出时丢**最旧**的样本并计数。
func TestRingBufferDropsOldest(t *testing.T) {
	rb := NewRingBuffer(4)
	rb.Write([]float32{1, 2, 3, 4})
	rb.Write([]float32{5, 6})
	if rb.Dropped() != 2 {
		t.Fatalf("应丢弃 2 个最旧样本，实际 %d", rb.Dropped())
	}
	out := make([]float32, 4)
	n := rb.Read(out)
	if n != 4 {
		t.Fatalf("应读出 4 个样本，实际 %d", n)
	}
	want := []float32{3, 4, 5, 6}
	for i := range want {
		if out[i] != want[i] {
			t.Fatalf("读出的样本 %v，期望 %v（应丢最旧）", out[:n], want)
		}
	}
}

// TestFramesToMs 断言帧计数换算（DOC-02 §4.5.1 的唯一公式）。
func TestFramesToMs(t *testing.T) {
	cases := []struct {
		frames int64
		ms     int64
	}{
		{0, 0}, {16, 1}, {16000, 1000}, {8000, 500}, {160, 10},
		{16000 * 3600, 3600 * 1000}, // 1 小时
	}
	for _, c := range cases {
		if got := FramesToMs(c.frames); got != c.ms {
			t.Fatalf("FramesToMs(%d)=%d，期望 %d", c.frames, got, c.ms)
		}
	}
}

// itoa 测试内的极小整数转字符串（避免每处都 fmt.Sprintf）。
func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	neg := v < 0
	if neg {
		v = -v
	}
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
