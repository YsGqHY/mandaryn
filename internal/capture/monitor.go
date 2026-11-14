package capture

import (
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"vip.oberon.mandaryn/internal/contract"
)

// DefaultPollInterval 源可用性轮询间隔。
//
// 为什么轮询而不是平台事件回调（ARCH §4.1.2 的 IMMNotificationClient /
// AudioObjectAddPropertyListener / PipeWire registry）：
//   - 三个平台各写一套回调，代码量与崩溃面都大，且回调线程模型各不相同；
//   - 我们关心的变化（应用启动/退出、设备增减）是秒级的人操作事件，
//     2s 的延迟用户无感；
//   - 轮询复用同一个 Enumerate 路径，行为与「手动刷新」完全一致，易测。
//
// 代价是每次轮询要做一次 COM 枚举（本机实测 8 个设备 + 数个会话，
// 单次几毫秒），2s 一次可以忽略。若后续发现耗电/唤醒问题，
// 再针对 Windows 换成 IMMNotificationClient 即可，接口不变。
const DefaultPollInterval = 2 * time.Second

// Monitor 周期性对比源快照，检测「应用启动/退出、设备增减」。
//
// 用法：
//
//	m := capture.NewMonitor(capture.DefaultPollInterval, func(srcs []contract.Source) { ... })
//	m.Start()
//	defer m.Stop()
//
// 回调在 Monitor 自己的 goroutine 上执行，**不要在里面做阻塞操作**
// （需要耗时处理时自己再起 goroutine）。
type Monitor struct {
	interval time.Duration
	onChange func([]contract.Source)

	mu      sync.RWMutex
	last    []contract.Source
	sig     string
	primed  bool // 是否已经建立过基线（首次 poll 只建立基线，不触发回调）
	started bool
	stop    chan struct{}
	done    chan struct{}
}

// NewMonitor 构造监视器；onChange 可为 nil（只保留快照，不发通知）。
func NewMonitor(interval time.Duration, onChange func([]contract.Source)) *Monitor {
	if interval <= 0 {
		interval = DefaultPollInterval
	}
	return &Monitor{
		interval: interval,
		onChange: onChange,
	}
}

// Start 启动轮询（幂等：重复调用无副作用）。首次快照**不触发**回调。
//
// 首次不回调的理由：启动时 UI 自己会调 ListSources 建立初始视图，
// 若这里也回调一次，会立刻弹出一批「新增源」事件，属于噪音。
func (m *Monitor) Start() {
	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		return
	}
	m.started = true
	m.stop = make(chan struct{})
	m.done = make(chan struct{})
	stop, done := m.stop, m.done
	m.mu.Unlock()

	go func() {
		defer close(done)
		ticker := time.NewTicker(m.interval)
		defer ticker.Stop()

		// 先取一次基线快照，避免第一次变化被当成「全部新增」。
		m.poll(false)

		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				m.poll(true)
			}
		}
	}()
}

// Stop 停止轮询并等待 goroutine 退出（幂等）。
func (m *Monitor) Stop() {
	m.mu.Lock()
	if !m.started {
		m.mu.Unlock()
		return
	}
	m.started = false
	stop, done := m.stop, m.done
	m.mu.Unlock()

	close(stop)
	<-done
}

// Snapshot 返回最近一次成功枚举的结果（可能为空）。
//
// 返回副本，调用方可以直接读写而不影响内部状态。
func (m *Monitor) Snapshot() []contract.Source {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]contract.Source, len(m.last))
	copy(out, m.last)
	return out
}

// poll 执行一次枚举并按需触发回调。
func (m *Monitor) poll(notify bool) {
	sources, err := Enumerate()
	if err != nil {
		// 枚举失败（音频服务重启、COM 异常）不改变已知快照：
		// 一次瞬时失败不该让 UI 把全部源标成「不可用」。
		lg().Warn("轮询音频源失败", "err", err)
		return
	}

	sig := snapshotSignature(sources)

	m.mu.Lock()
	// primed 用独立布尔而不是「sig 不为空」来判断：源列表**全部消失**时
	// 指纹恰好也是空串，若用 sig != "" 当基线标志，就会把「所有设备被拔掉 /
	// 应用全退出」这次最重要的事件吞掉。
	changed := m.primed && sig != m.sig
	m.sig = sig
	m.last = sources
	m.primed = true
	cb := m.onChange
	m.mu.Unlock()

	if !notify || !changed {
		return
	}
	lg().Debug("音频源发生变化", "count", len(sources))
	if cb != nil {
		cb(sources)
	}
}

// snapshotSignature 提取「哪些源存在 / 是否可用」的指纹。
//
// 刻意**不包含 Producing**：电平在放音频时会来回跳变，
// 若纳入指纹，2s 一次的回调几乎是连续的噪音。电平由 UI 单独轮询 ProbeSource。
func snapshotSignature(sources []contract.Source) string {
	if len(sources) == 0 {
		return ""
	}
	parts := make([]string, 0, len(sources))
	for _, s := range sources {
		parts = append(parts, s.ID+"|"+s.Kind+"|"+strconv.FormatBool(s.Present))
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}
