package asr

// 「加载期参数」：把 NFR-08 的推理档位换算成引擎构造参数（当前离线只有线程数一项）。
//
// # 为什么必须是「加载期」而不是「运行期」
//
// sherpa-onnx 的线程数是 `OfflineRecognizerConfig.ModelConfig.NumThreads`，写进
// `NewOfflineRecognizer` 之后不可改。而 `LocalFactory` 是 `func() (Engine, error)`，
// 由 `Router` 在**加载引擎**时调用一次。因此改档位**不会热生效**：
//
//	档位变化 → 下次加载（热切换来源 / Reload / 冷启动）才带上新线程数
//
// 这是 sherpa 的 API 形状决定的，不是「暂未接线」。要让线程数立刻变，只能整引擎重建
// （几秒加载 + 期间无引擎可用），代价远大于「等下一次加载」；档位本身是慢变量
// （降档要连续 3s 确认、升档要 15s 稳定），等下次加载完全可以接受。
// 这段结论同时写在 `internal/perf/policy.go` 与 `docs/DEV-ISSUES-E-s8.md`，别只改一处。

// LoadThreads 读一次「本次加载」要用的 onnx 推理线程数。
//
// 语义：
//   - `gen == nil` → 0：不注入，由引擎用自己的默认值（`threadsOrDefault(0)` = NumCPU/2）；
//   - `gen()` 返回 ≤0（含负数、拿不到档位）→ 0：同样交给引擎默认值。
//     **不要**把它夹成 1：0 在 sherpa 里明确表示「用库默认」，而 1 是「强制单线程」，
//     把「不知道」变成「单线程」会让正常机器无谓地慢一倍；
//   - 每次调用**只读一次** gen：档位在采样 goroutine 里随时变化，一次加载只该用一个
//     快照，否则同一次构造里可能先按 L2、后按 L0 取到不同的值。
func LoadThreads(gen func() int) int {
	if gen == nil {
		return 0
	}
	if n := gen(); n > 0 {
		return n
	}
	return 0
}
