// Package perf 是**整机性能预算与自适应限流**的 L2 领域包（`NFR-08` / DOC-02 §4.7）。
//
// # 核心口径：整机份额，不是「本应用上限」
//
// 用户设的 N（`app_settings.resource_cap_pct`）约束的是**整机总占用**，本应用只吃
// 「N 减去别人占的部分」剩下的额度：
//
//	otherPct = totalCpuPct − selfCpuPct   // 其它进程占用
//	headRoom = N − otherPct               // 本应用当前可用额度（可为负 = 已超额）
//
// 两个占用率**都必须按整机核数归一**，即 `ΔCPU时间 ÷ Δ整机CPU时间 × 100`，而不是
// 「占本进程可用核数的百分比」。混用两种量纲会让 headRoom 变成无意义的差值，这是本域
// 最典型的坑；`sampler_test.go` 用「4 核机器、本进程占满 1 核 ⇒ selfCpuPct == 25」
// 把它锁死。
//
// # 本包表达什么、不表达什么
//
// 只表达「**推理**该怎么让步」：档位 L0/L1/L2/L3/P（`Level`）。**不表达、也不可能
// 表达「停止录制」**——音频采集与落盘在管线最上游，与推理严格解耦（DOC-02 §1.2），
// 任何降档与暂停都必须让录制继续（`NFR-08` 硬要求）。`BudgetState` 里没有任何与
// 录制相关的字段，`service_test.go` 用反射把它钉成结构性约束。
//
// # 与其它模块的关系
//
//   - 产出 `Idle()`（整机空闲判定）供 `transcribe.Options.Idle` 消费，决定**自动排队**
//     的转写任务能否起跑；
//   - 产出 `contract.PerfUsage`（EV-15 `perf:usage`）供 L3 薄壳推给前端；
//   - 产出时间轴标记类型（`contract.MarkerPerf*`）供装配层落库，让降档/暂停/恢复在
//     回放里可见。
//
// 本包不 import Wails，也不做任何 I/O 之外的副作用（不落库、不发事件），因此可被
// 单测直接驱动。
package perf

import (
	"time"

	"vip.oberon.mandaryn/internal/contract"
)

// —— 默认值与边界 ——

const (
	// DefaultCapPct 整机预算 N 的默认值（PRD `NFR-08`：默认 80%）。
	DefaultCapPct = 80
	// MinCapPct / MaxCapPct 是 N 的合法范围（PRD：10%–100%，步进 5%）。
	//
	// 校验本身在 `appsettings.SetResourceCap`（已有，越界返回 ERR_PERF_CAP_RANGE）；
	// 这里只做「读到的值不合规时兜底」，避免脏数据把档位算成 NaN。
	MinCapPct = 10
	MaxCapPct = 100

	// DefaultSampleInterval 采样周期（DOC-02 §4.7.2：500ms）。
	DefaultSampleInterval = 500 * time.Millisecond
	// DefaultWindow 控制判据的滑动平均窗口（DOC-02 §4.7.2：5s / 10 点）。
	DefaultWindow = 5 * time.Second
	// DefaultIdleOtherPctMax 判定「整机空闲」时其它进程占用的上限。
	//
	// 取 10 而不是别的数字，是因为 PRD 的验收条目用「空载（otherPct < 10%）录制
	// 30 分钟不降档」来定义空载，沿用同一口径免得出现两套「空闲」。
	DefaultIdleOtherPctMax = 10.0
)

// Level 是推理档位（DOC-02 §4.7.3）。
//
// 取值直接复用 `contract` 里的常量，避免与 DTO / 数据库 / 前端出现两套字符串。
type Level string

const (
	// LevelL0 正常：两遍转写全开 / 转写任务全速。
	LevelL0 Level = contract.LevelL0
	// LevelL1 降档：关闭第二遍离线校正（仅边录边转有意义）。
	LevelL1 Level = contract.LevelL1
	// LevelL2 降档：引擎 num_threads 降到 1，暂停声纹提取。
	LevelL2 Level = contract.LevelL2
	// LevelL3 降档：流式引擎隔帧处理（丢帧不丢音频）。
	LevelL3 Level = contract.LevelL3
	// LevelP 暂停推理：音频采集与落盘继续。
	LevelP Level = contract.LevelP
)

// String 实现 fmt.Stringer（值本身就是 "L0".."L3"/"P"）。
func (l Level) String() string { return string(l) }

// Rank 返回档位的严格程度（越大越保守），用于比较与逐级升降。
//
// 未知值按 L0 处理：宁可先不限制，也不要因为一个脏字符串把推理永久停掉。
func (l Level) Rank() int {
	switch l {
	case LevelL1:
		return 1
	case LevelL2:
		return 2
	case LevelL3:
		return 3
	case LevelP:
		return 4
	default:
		return 0
	}
}

// Throttled 报告是否处于降档（正交标记 `throttled`，DOC-02 §6.1）。
func (l Level) Throttled() bool { return l.Rank() > 0 }

// Paused 报告是否暂停推理（正交标记 `suspended`，DOC-02 §6.1）。
func (l Level) Paused() bool { return l.Rank() == 4 }

// ParseLevel 把外部字符串解析成档位（未知值 → L0）。
func ParseLevel(s string) Level {
	switch Level(s) {
	case LevelL1, LevelL2, LevelL3:
		return Level(s)
	case LevelP:
		return LevelP
	default:
		return LevelL0
	}
}

// stepUp 返回「升一档」的结果（P→L3→L2→L1→L0），已在 L0 则原样返回。
func (l Level) stepUp() Level {
	switch l {
	case LevelP:
		return LevelL3
	case LevelL3:
		return LevelL2
	case LevelL2:
		return LevelL1
	default:
		return LevelL0
	}
}

// Snapshot 是一次**整机份额**读数（NFR-08 口径）。
//
// 三个分量都是「占整机 CPU 容量的百分比」，按核数归一，因此取值范围 0–100；
// SelfCpuPct 可以接近 100（多线程推理跑满全机），但不会超过 TotalCpuPct。
type Snapshot struct {
	// TotalCpuPct 整机 CPU 占用 = (Δbusy / Δtotal) × 100。
	TotalCpuPct float64 `json:"totalCpuPct"`
	// SelfCpuPct 本应用进程树占用 = (Δself / Δtotal) × 100（**整机归一**）。
	SelfCpuPct float64 `json:"selfCpuPct"`
	// OtherPct 其它进程占用 = Total − Self。
	OtherPct float64 `json:"otherPct"`
	// HeadRoom 本应用可用额度 = N − OtherPct，可为负。
	//
	// 它依赖 N，所以**只由判定方填**（`WithCap`）；`Budget.Evaluate` 会用当次
	// N 重算，保证「设置一改立即生效」。
	HeadRoom float64 `json:"headRoom"`
	// SampledAt 采样时刻。
	SampledAt time.Time `json:"sampledAt"`
	// Valid 是否有可用的**差值**读数。
	//
	// 首次采样（无基准）、两次采样间隔为 0（计数器还没走动）、计数器回绕三种情况
	// 都为 false：这时不该拿它去判档位。消费者应保持当前档位不动。
	Valid bool `json:"valid"`
}

// WithCap 返回按给定 N 填好 HeadRoom 的快照副本。
func (s Snapshot) WithCap(capPct int) Snapshot {
	s.HeadRoom = float64(normalizeCap(capPct)) - s.OtherPct
	return s
}

// normalizeCap 把 N 夹到合法范围；缺省（0 或负数）用默认 80。
//
// 为什么不在越界时报错：N 的校验在 `appsettings.SetResourceCap`，这里若再报错，
// 一个坏值就能让性能域整体不可用；夹紧后行为可预期，且下一次读库会纠正。
func normalizeCap(capPct int) int {
	switch {
	case capPct <= 0:
		return DefaultCapPct
	case capPct < MinCapPct:
		return MinCapPct
	case capPct > MaxCapPct:
		return MaxCapPct
	default:
		return capPct
	}
}
