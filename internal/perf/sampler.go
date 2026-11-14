package perf

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync"
	"time"
)

// defaultTreeRefresh 进程树枚举的刷新周期。
//
// 枚举子进程（Windows 要快照全系统进程表，Linux 要扫 /proc）比取一次 CPU 时间贵
// 一到两个数量级，而推理子进程的生命周期是分钟级，5s 刷新一次不会漏算。
const defaultTreeRefresh = 5 * time.Second

// —— 采样器的失败信号 ——
//
// 它们都**不是**真错误（不该冒泡成用户可见错误），只是「这一拍算不出占用率」。
// 调用方（Scheduler）遇到它们应当跳过本拍、保持当前档位，不要降档也不要升档。
var (
	// ErrNoBaseline 首次采样，还没有上一拍可做差值。
	ErrNoBaseline = errors.New("perf: 首次采样，无基准值")
	// ErrNoInterval 两次采样之间整机 CPU 时间没有推进（间隔为 0 / 计数器粒度未到）。
	//
	// Windows 的 GetSystemTimes 粒度约 15.6ms，500ms 周期下几乎不会出现；
	// 但单测会注入 0 间隔，且休眠唤醒后也可能读到同一计数，必须显式处理，
	// 否则会算出 0/0 = NaN 并污染滑动平均（NaN 会让所有档位比较失效）。
	ErrNoInterval = errors.New("perf: 采样间隔为 0，本次无法计算占用率")
	// ErrCounterReset 计数器回退（理论上不该发生，但休眠、计数器位宽回绕、容器迁移
	// 都可能造成），此时不能做差值，只能重新建立基准。
	ErrCounterReset = errors.New("perf: CPU 计数器回退，已重建基准")
	// ErrUnsupported 本平台没有可靠的双分量（整机 + 本进程）CPU 采样源。
	//
	// macOS 属于此类：`host_processor_info` 在 x/sys 里没有可用的无 cgo 绑定，
	// 而只用 `getrusage(RUSAGE_SELF)` 只能拿到本进程、拿不到整机，两分量缺一
	// 就凑不出 otherPct。上层据此降级为「不采样、不降档」（不是「假装空闲」）。
	ErrUnsupported = errors.New("perf: 本平台无可靠 CPU 采样源")
)

// rawCounters 是一次平台原始计数器读数。
//
// 单位约定：只要求**同一次读数内三个字段同单位、同倍率**，因为占用率是两个差值之
// 比（Windows 是 100ns FILETIME tick、Linux 是 USER_HZ jiffies），比值与单位无关，
// 因此这里不需要任何 HZ 换算——少一处换算就少一个量纲坑。
type rawCounters struct {
	// SystemTotal 全系统累计 CPU 时间（**含** idle）。
	SystemTotal uint64
	// SystemIdle 全系统累计空闲时间（不是独立分量，是 SystemTotal 的子集）。
	SystemIdle uint64
	// Self 本应用**进程树**累计 CPU 时间（含推理子进程，见 DOC-02 §4.7.5）。
	Self uint64
	// At 读数时刻。
	At time.Time
}

// readFunc 返回一次原始读数；由平台实现注入，单测替换成假计数器。
type readFunc func() (rawCounters, error)

// Sampler 用相邻两次读数算整机份额占用率。
//
// 并发安全（内部有锁）：调度循环与诊断读号可能同时打到它。
type Sampler struct {
	read readFunc

	mu      sync.Mutex
	prev    rawCounters
	hasPrev bool
	// clocks 核数，仅用于诊断展示（占用率本身已按核数归一，不需要它参与计算）。
	clocks int
}

// NewSampler 构造本平台采样器。平台无可靠采样源时返回包装了 ErrUnsupported 的错误。
func NewSampler() (*Sampler, error) {
	read, err := newPlatformReader(os.Getpid(), defaultTreeRefresh)
	if err != nil {
		return nil, fmt.Errorf("perf: %w (%s)", err, runtime.GOOS)
	}
	return newSamplerWithReader(read), nil
}

// newSamplerWithReader 用注入的读数器构造采样器（单测入口）。
func newSamplerWithReader(read readFunc) *Sampler {
	return &Sampler{read: read, clocks: runtime.NumCPU()}
}

// Clocks 返回构造时观测到的核数（诊断用）。
func (s *Sampler) Clocks() int { return s.clocks }

// Sample 取一拍并计算占用率。
//
// 返回 (Snapshot{}, ErrNoBaseline) 等失败信号时，Snapshot.Valid 一定是 false；
// 调用方按「本拍无数据」处理即可，不要当成错误上报给用户。
func (s *Sampler) Sample() (Snapshot, error) {
	if s == nil || s.read == nil {
		return Snapshot{SampledAt: time.Now()}, ErrUnsupported
	}
	cur, err := s.read()
	if err != nil {
		return Snapshot{SampledAt: time.Now()}, err
	}
	if cur.At.IsZero() {
		cur.At = time.Now()
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.hasPrev {
		s.prev, s.hasPrev = cur, true
		return Snapshot{SampledAt: cur.At}, ErrNoBaseline
	}

	prev := s.prev
	// 计数器回退：任一分量变小都说明「基线作废」。
	// 为什么不用差值取绝对值硬算：回退后的差值没有物理意义，硬算会得到离谱的
	// 占用率（比如 4000%），进而误判 P 档把转写停掉。
	if cur.SystemTotal < prev.SystemTotal || cur.SystemIdle < prev.SystemIdle || cur.Self < prev.Self {
		s.prev = cur
		return Snapshot{SampledAt: cur.At}, ErrCounterReset
	}

	dTotal := float64(cur.SystemTotal - prev.SystemTotal)
	dIdle := float64(cur.SystemIdle - prev.SystemIdle)
	dSelf := float64(cur.Self - prev.Self)

	// 间隔为 0：**不更新基准**，让窗口自然拉长到下一拍，避免把一次 0 间隔
	// 当成「有效但为 0」的读数污染滑动平均。
	if dTotal <= 0 {
		return Snapshot{SampledAt: cur.At}, ErrNoInterval
	}

	s.prev = cur

	// 整机占用：所有核的可用时间里有多少是忙的。
	// 分母是 dTotal（= 核数 × 墙钟），所以结果**天然按核数归一**。
	totalPct := (dTotal - dIdle) / dTotal * 100
	totalPct = clampF(totalPct, 0, 100)

	// 本应用占用：**同一个分母 dTotal**，这是量纲正确的关键。
	// 若这里改用「本进程可用的核数」当分母，selfCpuPct 会是整机口径的 N 倍，
	// otherPct = total − self 就会变成大负数，headRoom 永远充足 → 永不降档。
	selfPct := dSelf / dTotal * 100
	if selfPct < 0 {
		selfPct = 0
	}
	// GetProcessTimes 的分辨率比 GetSystemTimes 细，极短窗口下可能算出
	// self > total（本进程刚跑完、整机闲）。夹到 total 以内，保证 otherPct ≥ 0。
	if selfPct > totalPct {
		selfPct = totalPct
	}

	return Snapshot{
		TotalCpuPct: totalPct,
		SelfCpuPct:  selfPct,
		OtherPct:    totalPct - selfPct,
		SampledAt:   cur.At,
		Valid:       true,
	}, nil
}

// Reset 丢弃基准值，下次 Sample 重新建立基线（路径切换 / 休眠唤醒后调用）。
func (s *Sampler) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prev = rawCounters{}
	s.hasPrev = false
}

// —— 滑动平均（DOC-02 §4.7.2：5s / 10 点）——

// avgWindow 是控制判据的滑动平均。
//
// 为什么必须平均而不是用瞬时值：编译、杀毒扫描、Windows 更新都会把 CPU 瞬间打满，
// 用瞬时值会「闪停」推理再「闪恢复」，用户看到的是转写反复中断。
type avgWindow struct {
	mu    sync.Mutex
	size  int
	total []float64
	self  []float64
	other []float64
	pos   int
}

// newAvgWindow 按窗口时长与采样周期算点数（至少 1 点）。
func newAvgWindow(window, interval time.Duration) *avgWindow {
	if window <= 0 {
		window = DefaultWindow
	}
	if interval <= 0 {
		interval = DefaultSampleInterval
	}
	size := int(window / interval)
	if size < 1 {
		size = 1
	}
	return &avgWindow{
		size:  size,
		total: make([]float64, 0, size),
		self:  make([]float64, 0, size),
		other: make([]float64, 0, size),
	}
}

// Size 返回窗口点数（诊断用）。
func (w *avgWindow) Size() int { return w.size }

// Add 把一拍有效读数并入窗口，返回窗口内的平均值。
//
// 返回的 Snapshot.Valid 在窗口里至少有一个点时为 true；失败拍的读数不进入窗口
// （由调用方先行过滤），所以这里不需要「跳过零点」的逻辑。
func (w *avgWindow) Add(s Snapshot) Snapshot {
	w.mu.Lock()
	defer w.mu.Unlock()

	push := func(buf []float64, v float64) []float64 {
		if len(buf) < w.size {
			return append(buf, v)
		}
		buf[w.pos%w.size] = v
		return buf
	}
	w.total = push(w.total, s.TotalCpuPct)
	w.self = push(w.self, s.SelfCpuPct)
	w.other = push(w.other, s.OtherPct)
	w.pos++

	avg := func(buf []float64) float64 {
		if len(buf) == 0 {
			return 0
		}
		var sum float64
		for _, v := range buf {
			sum += v
		}
		return sum / float64(len(buf))
	}

	out := Snapshot{
		TotalCpuPct: avg(w.total),
		SelfCpuPct:  avg(w.self),
		OtherPct:    avg(w.other),
		SampledAt:   s.SampledAt,
		Valid:       len(w.other) > 0,
	}
	return out
}

// Reset 清空窗口（采样源重建 / 预算设定大变时用）。
func (w *avgWindow) Reset() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.total = w.total[:0]
	w.self = w.self[:0]
	w.other = w.other[:0]
	w.pos = 0
}

func clampF(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
