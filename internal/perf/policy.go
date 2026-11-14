package perf

import "runtime"

// 档位 → 引擎参数的**纯函数**（DOC-02 §4.7.4「档位动作」的落地换算）。
//
// 为什么单独成文件、且只放纯函数：
//   - 换算规则要能被单测穷举（cores × level 的笛卡尔积），不能藏在引擎或装配层；
//   - 本包不 import 任何引擎，换算规则与「谁来用」解耦，`internal/asr/local` 只消费结果；
//   - 所有函数无副作用、无锁、无 I/O，因此可以放心在采样 goroutine 与加载 goroutine
//     之间随便调。
//
// ⚠️ 生效时机（重要，别写错文档）：
// 线程数只能在**引擎构造时**生效 —— sherpa-onnx 的 `NumThreads` 写进
// `OfflineRecognizerConfig.ModelConfig.NumThreads`，构造完成后不可改。引擎工厂
// `asr.LocalFactory` 由 Router 在「加载引擎」时调用，因此**改档位不会热生效**，
// 最早在**下一次加载引擎**（热切换来源 / Reload / 冷启动）时才带上新值。
// 这不是「暂未实现」，是 sherpa 的 API 形状决定的；要让档位立刻影响推理，
// 只能整引擎重建（代价是几秒加载 + 期间无引擎可用），因此不做。

// ThreadsFor 返回某档位下应当给 onnx 推理用的线程数。
//
// 规则（`cores` 是整机逻辑核数）：
//
//	L0 / L1 → max(1, cores/2)   正常档：留一半给系统与音频采集
//	L2      → max(1, cores/4)   降档：再砍一半（8 核 → 2 线程）
//	L3 / P  → 1                 最省：单线程；P 档根本不该有推理（见 CheckInferenceAllowed），
//	                            这里给 1 是为了「万一还有在途任务」时别把整机吃满
//
// `cores <= 0`（拿不到核数）按 1 处理：宁可保守，也不要算出 0 线程——
// 0 在 sherpa 里是「用库默认值」，会绕过我们所有的预算约束。
//
// 单调性：对任意固定 cores，`ThreadsFor` 随档位变保守**单调不增**，且永不为 0。
// 这条性质由 `policy_test.go` 穷举断言，因为它是「降档不会反而更吃 CPU」的唯一保证。
func ThreadsFor(level Level, cores int) int {
	if cores <= 0 {
		cores = 1
	}
	switch level {
	case LevelL2:
		return atLeastOne(cores / 4)
	case LevelL3, LevelP:
		return 1
	default:
		// L0 / L1 与**未知档位**同路：未知值按「正常档」处理（见 Level.Rank 的同一取向），
		// 免得一个脏字符串把推理线程压到 1 拖慢所有转写。
		return atLeastOne(cores / 2)
	}
}

// atLeastOne 保证线程数至少 1。
func atLeastOne(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

// RefinementAllowed 报告该档位是否允许做**第二遍离线校正**。
//
// 语义边界（必须读清，否则会误以为本函数是死代码）：
// 这里的「第二遍离线校正」指的是**边录边转**（FR-03-10，默认关）路径上的动作：
// 流式引擎先出即时结果，录音结束后再用非流式模型整段重解一遍以提高准确率。
// DOC-02 §4.7.3 的 L1 档动作就是「关闭第二遍离线校正」。
//
// **当前没有任何调用方**：边录边转尚未实现（见 `docs/DEV-ISSUES-E-s8.md` E8-01），
// 延后转写路径（默认模式）本来就只有一遍推理，不存在可关的第二遍。
// 保留本函数是为了让「L1 到底降了什么」有一个**可测试、可接线**的定义，
// 而不是在文档里留一句没人实现的话；接线时只应在边录边转的二次校正分支前调用它。
//
// 取值：只有 L0 允许；L1/L2/L3/P 一律不允许。
// 理由：L1 的定义就是「关掉它」，更保守的档位没有理由反倒允许；
// P 档连第一遍都不该跑，更谈不上第二遍。
func RefinementAllowed(level Level) bool {
	return level == LevelL0
}

// Threads 返回**当前档位**对应的 onnx 推理线程数，供装配层注入引擎工厂。
//
// 用法（装配层，`internal/app`）：把 `perfSvc.Threads` 这个函数值交给
// `asrlocal.NewFactoryWithThreads(locator, perfSvc.Threads)`，工厂在**每次加载引擎**时
// 调它一次，于是「下一次加载」自然带上最新档位。
//
// 它读的是 `State().Level` 而不是内部 `Budget.Level()`：档位与 `throttled/suspended`
// 快照永远出自同一次判定，避免出现「线程数按 L2 给、对外状态还是 L1」的撕裂。
func (s *Service) Threads() int {
	return ThreadsFor(s.State().Level, runtime.NumCPU())
}
