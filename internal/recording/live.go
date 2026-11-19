package recording

// 边录边转的音频旁路（FR-03-10，**默认关闭**）。
//
// 设计约束（逐条对应产品铁律）：
//  1. **默认零开销**：`Options.LiveEnabled` / `Options.NewLiveSink` 都为 nil 时，
//     录制路径与未实现该功能前**完全一致**（`run.sink` 恒为 nil，每批样本只多一次
//     nil 判断），既有测试无需修改即可守护这条不变量。
//  2. **不阻塞采集与落盘**：`Push` 只在落盘 goroutine 上做「拷贝进有界队列」这一件事，
//     队列满时由 sink 自己丢最旧的推理样本；绝不在落盘路径上等推理。
//  3. **收尾不跑推理**：`Close` 必须是**非阻塞**的（发信号即可），
//     未定稿尾部的定稿/丢弃由 sink 自己的 goroutine 决定 —— `Stop` 的收尾窗口
//     一秒都不能被推理拖长（那是崩溃丢音频的高危窗口）。
//  4. **失败隔离**：sink 内部任何错误都不得回灌录制；这里的调用点不接收 error。

// LiveSink 是边录边转的音频接收端（由 internal/transcribe 的 LiveRunner 实现）。
//
// 实现必须满足：
//   - `Push` **非阻塞**（满则丢最旧并计数），允许被落盘 goroutine 调用；
//   - `Push` 的 `samples` 只在调用期间有效，需要保留必须自行拷贝；
//   - `Close` **非阻塞**、幂等（只发停止信号，不在里面等推理结束）。
//
// 用接口而不是 `func([]float32)`：需要 `Close` 与 `CharCount` 两个生命周期动作，
// 拆成三个函数字段会让「谁在什么时候调」变得含糊。
type LiveSink interface {
	// Push 投递一段内部格式（16k 单声道）音频。
	//
	// atFrame 是该批样本在会话时间轴上的起始**帧偏移**（= 已落盘帧数），
	// 因此 sink 侧时间轴与录音帧计数严格一致（不用墙钟，ARCH §4.3）。
	Push(samples []float32, atFrame int64)
	// Close 请求正常退出（非阻塞、幂等）：实现可异步排空已接收的音频。
	Close()
	// CharCount 已定稿的字符数（FR-03-9：开启边录边转时录制页追加显示）。
	CharCount() int64
}

// MemorySink 是在线内存直传接收端。Abort 必须非阻塞，并取消未完成的上传、
// 将对应转写标记为失败；它不应将尚未发送的音频排队等待后续补发。
type MemorySink interface {
	LiveSink
	Abort()
}

// framesToMs 把帧数换算成毫秒（与 writer.DurationMs 同一口径）。
func framesToMs(frames int64) int64 {
	if frames <= 0 {
		return 0
	}
	return frames * 1000 / InternalSampleRate
}
