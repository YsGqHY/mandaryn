package recording

import (
	"math"
	"sync"
)

// Metrics 是采集期间的实时指标（EV-02 / EV-03 的数据来源）。
//
// 全部字段都由**帧计数**推导，不用时钟：
//   - RecordedMs = frames / 16000 * 1000（与落盘时长严格一致）
//   - ZeroInputMs = 连续静音样本数换算的毫秒
//
// BytesWritten 取落盘 writer 的实际字节数（含 44 字节头），UI 直接显示「已存 x MB」。
type Metrics struct {
	// LevelPeak 最近一个上报窗口内的峰值电平（0–1）。
	LevelPeak float32
	// LevelRMS 最近一个上报窗口内的均方根电平（0–1）。
	//
	// 为什么在规定的四个字段之外多这一个：contract.RecordingLevel 同时要
	// RMS 与 Peak（前端电平条用 RMS、削波指示用 Peak），若在这里丢掉 RMS，
	// 薄壳就只能拿 Peak 冒充 RMS，UI 的电平条会变成「一跳一跳的削波指示」。
	LevelRMS float32
	// RecordedMs 已录时长（逻辑时长，帧计数换算）。
	RecordedMs int64
	// BytesWritten 已写入音频文件的字节数。
	BytesWritten int64
	// ZeroInputMs 自上次有音频以来连续静音了多久。
	ZeroInputMs int64
}

// Monitor 累计电平与时长指标，并把「上报节拍」与「无音频告警」做成**帧驱动**的判定。
//
// 为什么用帧数而不是 time.Ticker 切节拍：
//   - 节拍只随音频进度推进 → 暂停时不会继续上报（UI 计时不动才对）；
//   - 单测可以用合成样本精确驱动到「第 4 个窗口」「第 10 秒静音」，
//     不会因为 CI 机器慢而出现 flaky。
//
// 并发安全：Feed 由落盘 goroutine 驱动，Snapshot / Metrics / RecordedMs 等读取
// 可能来自 L3 查询或事件回调（不同 goroutine），因此**本类型自带互斥**。
//
// 曾经的做法是「请调用方持 Recorder 的锁」—— 但 Recorder.Metrics() 在读取前就
// 释放了 r.mu，这条路走不通，实际存在数据竞争（Wave E 发现，E8-06）。
type Monitor struct {
	// mu 保护下面全部累加器与 last（读取与写入来自不同 goroutine）。
	mu sync.Mutex
	// 窗口累加器（250ms 上报一次）。
	winPeak    float32
	winSumSq   float64
	winSamples int64

	// 连续静音样本数（遇到有音频立即归零）。
	silentSamples int64
	// 累计样本数（= 帧数，用来算 RecordedMs）。
	recordedSamples int64

	// maxPeak 整场会话的最大峰值，用来判定「全零空录音」。
	maxPeak float32

	noAudioWarned bool
	last          Metrics
}

// NewMonitor 构造指标累计器。
func NewMonitor() *Monitor { return &Monitor{} }

// Feed 累计一批内部格式（16kHz 单声道）样本。
//
// 返回：
//   - report 是否到了 250ms 上报节拍（调用方据此调用 OnMetrics 回调，4Hz 节流）；
//   - noAudio 是否**首次**触发「未检测到音频」告警（连续静音 ≥ 10s，
//     FR-03-7）。它只上报，**不改状态、不产生空录音实例** —— 状态由用户或
//     设备事件驱动，静音可能只是对方还没说话。
//
// bytesWritten 由调用方传入落盘 writer 的实际字节数。
func (m *Monitor) Feed(samples []float32, bytesWritten int64) (report bool, noAudio bool) {
	if len(samples) == 0 {
		return false, false
	}
	var peak float32
	var sumSq float64
	for _, s := range samples {
		a := float32(math.Abs(float64(s)))
		if a > peak {
			peak = a
		}
		sumSq += float64(s) * float64(s)
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	n := int64(len(samples))
	m.winSamples += n
	m.recordedSamples += n
	m.winSumSq += sumSq
	if peak > m.winPeak {
		m.winPeak = peak
	}
	if peak > m.maxPeak {
		m.maxPeak = peak
	}

	if float64(peak) < SilentPeakThreshold {
		m.silentSamples += n
	} else {
		// 有音频：静音连续段结束，告警可以重新武装。
		m.silentSamples = 0
		m.noAudioWarned = false
	}

	if m.silentSamples >= MsToFrames(SilenceWarnMs) && !m.noAudioWarned {
		m.noAudioWarned = true
		noAudio = true
	}

	if m.winSamples >= ReportWindowSamples {
		rms := float32(0)
		if m.winSamples > 0 {
			rms = float32(math.Sqrt(m.winSumSq / float64(m.winSamples)))
		}
		m.last = Metrics{
			LevelPeak:    m.winPeak,
			LevelRMS:     rms,
			RecordedMs:   FramesToMs(m.recordedSamples),
			BytesWritten: bytesWritten,
			ZeroInputMs:  FramesToMs(m.silentSamples),
		}
		m.winPeak = 0
		m.winSumSq = 0
		m.winSamples = 0
		report = true
	}
	return report, noAudio
}

// Metrics 最近一次上报节拍的指标快照（未到节拍时为零值窗口）。
func (m *Monitor) Metrics() Metrics {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.last
}

// Snapshot 当前累计值（不等节拍），用于状态查询时给出准确时长。
func (m *Monitor) Snapshot(bytesWritten int64) Metrics {
	m.mu.Lock()
	defer m.mu.Unlock()
	return Metrics{
		LevelPeak:    m.winPeak,
		LevelRMS:     0,
		RecordedMs:   FramesToMs(m.recordedSamples),
		BytesWritten: bytesWritten,
		ZeroInputMs:  FramesToMs(m.silentSamples),
	}
}

// RecordedFrames 累计帧数。
func (m *Monitor) RecordedFrames() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.recordedSamples
}

// RecordedMs 累计时长（帧计数换算）。
func (m *Monitor) RecordedMs() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return FramesToMs(m.recordedSamples)
}

// MaxPeak 整场会话的最大峰值（空录音判定用）。
func (m *Monitor) MaxPeak() float32 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.maxPeak
}

// SilentMs 当前连续静音时长。
func (m *Monitor) SilentMs() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return FramesToMs(m.silentSamples)
}

// NoAudioWarned 是否已经报过「未检测到音频」。
func (m *Monitor) NoAudioWarned() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.noAudioWarned
}
