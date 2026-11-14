# S2 录制与录音实例域 — 开发问题记录

> 范围：`internal/recording`（L2）与 `internal/services/recordingsvc`（L3），对应
> `FR-03` 的采集 / 落盘 / 状态机 / 录音实例封存部分（不含转写）。
> 记录原则：凡是踩过的坑立刻记，标注「现象 / 复现 / 根因 / 处置 / 状态」。
> 状态口径：`未解决` = 需要后续域或文档决策；`已绕过` = 有可用但非最优的实现；
> `已解决` = 本域内已修；`改设计` = 与文档口径不同且有意为之（需同步文档）。

---

## S2-01 GORM `Updates(struct)` 会跳过零值，会话时长/字节数可能更新不进去

- 现象：收尾时把 `duration_ms` 写 0（恰好是空录音）、或把 `ended_at` 清空，若用结构体更新会**静默无效**，库里保留旧值。
- 复现条件：`db.Model(&Session{}).Updates(Session{DurationMs: 0})` —— GORM 只更新非零字段。
- 根因：GORM 用反射判断零值，0 / "" / false 被当作「未提供」。
- 临时处置：`Store.UpdateSession` 的签名强制收 `map[string]any`，所有业务更新走字段 map；`docs/PERSISTENCE.md` 的零值陷阱在此再次命中。
- 状态：已解决

## S2-02 `os.File.WriteAt` 不移动文件偏移，写头后第一次数据写入覆盖了 WAV 头

- 现象：文件长度正好比预期少 44 字节，且不是合法 WAV（开头是 PCM 数据而不是 `RIFF`）。
- 复现条件：`CreateWriter` 先用 `WriteAt(header, 0)` 写占位头，随后直接 `Write(data)`。
- 根因：`WriteAt` 只写指定偏移、**不改文件当前偏移**（偏移仍为 0），所以顺序 `Write` 从 0 开始覆盖了头。
- 临时处置：`writeHeaderLocked` 在 `WriteAt` 后显式 `Seek(0, io.SeekEnd)` 复位到文件末尾；`TestWriterWAVHeader` 断言「文件长度 = 44 + 数据长度」以防回归。
- 状态：已解决

## S2-03 收尾没有关闭音频文件句柄，文件被进程占住

- 现象：Windows 上测试临时目录清理报 `The process cannot access the file because it is being used by another process`；真机上用户删除会话/移动文件会失败。
- 复现条件：`Stop` → 封存后未调用 `Writer.Close()`（只 `Seal()`）。
- 根因：`Seal` 只回填头并 fsync，不释放句柄；终态收尾漏了关闭。
- 临时处置：`finalizeLocked` 在 Seal 之后显式 `Close()`，并记录失败日志。
- 状态：已解决

## S2-04 单会话 WAV 超过 4GiB 无法表示

- 现象：RIFF/data 长度字段是 uint32，超过 4GiB 的 WAV 无法写出合法头。
- 复现条件：16kHz/16bit/单声道持续约 37 小时（32KB/s）。
- 根因：WAV（RIFF）格式限制；RF64/BW64 是另一套头格式。
- 临时处置：达到上限时**停止写入并返回 `ERR_DISK_FULL`**（不静默截断，避免「后半段无声但时长正常」），会话按失败收尾、已写音频保留。
- 状态：未解决（37 小时单会话属于极端场景；如产品要求支持，需引入 RF64 或按 4GiB 自动分段）

## S2-05 空录音策略：标 `failed` 而非删除，与「不产生空录音实例」的字面表述不同

- 现象：用户误触录了 0.4 秒，停止后在库里出现一条 `status=failed` 的会话。
- 复现条件：停止时逻辑时长 < 1s，或整场峰值低于 -60dBFS。
- 根因：`FR-03-7` 要求「不产生空录音实例」，但「不产生」究竟是不建行、还是删掉、还是标失败，文档没定义。
- 临时处置：**保留行与音频、状态标 `failed`、`degrade_reason` 写明原因，并通过 `RecordingState.lastError` 提示**。理由：静默删除会让用户以为软件没反应；保留则用户能自查（可能只是选错源）并自行删除。同时 `Stop` **不返回错误**（停止与封存都成功了，空录音是结果而非操作失败）。
- 状态：改设计（需在 `docs/PRD.md`/`ARCHITECTURE.md` 明确空录音的落库口径）

## S2-06 磁盘满时直接结束录制（标 `failed`），不做自动重试

- 现象：磁盘写满后录制在 200ms 内停止，会话标 `failed`，已写音频保留可回放。
- 复现条件：落盘 `Write`/`Sync` 返回 ENOSPC（或 Windows 的 ERROR_DISK_FULL）。
- 根因：磁盘满是用户侧动作（清理空间）才能解除的阻塞，录制器无法自行恢复。
- 临时处置：停止采集、封口、标 `failed` 并带 `ERR_DISK_FULL`；不无限重试（否则会话长期占着设备且日志刷屏）。用户清理空间后可重新开始录。
- 状态：未解决（是否需要在磁盘将满时**提前预警**（如剩余 < 200MB 时提示）属于产品决策；当前 `NFR-08` 预算只管 CPU）

## S2-07 `session_events.kind` 没有「异常中断」这个取值，恢复标记复用了 `source-lost`

- 现象：崩溃恢复后写入的时间轴标记 `kind=source-lost`，但 note 是「上次进程异常结束」。语义偏了。
- 复现条件：`RecoverUnfinished` / `RecoverSession` 修补会话后写 TBL-10。
- 根因：`DOC-04` TBL-10 的 kind 枚举是 `degrade/source-lost/device-switch/paused/resumed/perf-*`，没有 `interrupted` / `crash`。
- 临时处置：复用 `source-lost` + 明确 note，保证「崩溃点必须在回放里可见」这条不丢。
- 状态：未解决（需在 `DOC-04` TBL-10 增补 `interrupted`，并同步 `contract.Marker*` 常量与前端标记文案）

## S2-08 导出只支持 WAV，FLAC 明确拒绝

- 现象：`ExportAudio` 传 `.flac` 返回 `ERR_INVALID_ARGUMENT`，detail 说明「本版本不支持，请选 .wav」。
- 复现条件：`API-2.7` 的签名允许 `wav`/`flac`。
- 根因：FLAC 是**真编码**（LPC 预测 + 莱斯编码），不是换容器；引 CGO 依赖或手写编码器都不划算。
- 临时处置：只实现 WAV（逐字节复制 + 必要时在副本上补头），拒绝时给出可执行的替代方案。
- 状态：未解决（若产品需要 FLAC，建议用外部工具链或后续引入编码器；当前 UI 应只提供 .wav 选项）

## S2-09 `*apierr.Err` 的 nil 装进 error 接口会变成非 nil

- 现象：封存成功（本该返回 nil error）但调用方 `if err != nil` 判为失败；`apierr.HasCode(nil-ish)` 还会 panic。
- 复现条件：`func f() (T, error) { var e *apierr.Err; return t, e }`。
- 根因：Go 的 typed-nil 陷阱：`*apierr.Err(nil)` 转成 `error` 后接口非 nil。
- 临时处置：`finalizeLocked` 显式分支 `if opErr == nil { return st, nil }`；并给所有返回 `*apierr.Err` 的函数加了同款注释。
- 状态：已解决

## S2-10 状态机补了三条 `→ failed` 与三条 `→ preparing`，文档表格未列

- 现象：`recording → failed`、`paused → failed`、`finalizing → failed`，以及 `completed/interrupted/failed → preparing` 在 `ARCH §6.2` 表格里没有逐条列出。
- 复现条件：磁盘满（写失败要收尾）、以及 `ST-06/08/09` 状态下用户点「开始」。
- 根因：§6.2 只列了主流程；ST-09 的定义里其实已经包含「磁盘满且无音频」这类场景，§6.1 也写明三种终态「开始」按钮可用。
- 临时处置：状态机表按上述语义实现，并在 `state.go` 注释里说明理由；`TestLegalTransitions` 穷举 8×8 组合锁死行为。
- 状态：改设计（需在 `ARCH §6.2` 补这 6 条转换，避免实现与文档不一致）

## S2-11 16k→16k 走 sinc 核会少输出 `2*halfWidth` 个样本

- 现象：源采样率恰好 16kHz 时，流式输出比输入少 32 个样本（约 2ms），尾部样本被丢弃。
- 复现条件：`NewResampler(16000, 16000)` 分块喂 800 帧 → 只得到 768 帧。
- 根因：带窗 sinc 是**非因果**核，实现必须等核支撑区填满才产出，天然有群延迟；源与目标同率时这层延迟完全是浪费。
- 临时处置：同采样率走**直通快路径**（零延迟、零误差，也更省 CPU）；同时 `Flush` 按 `round(totalIn * out/in)` 补齐期望长度。`TestResamplerLengthRatio` 覆盖 8k/22.05k/32k/44.1k/48k/96k。
- 状态：已解决

## S2-12 暂停不会丢弃「已采到但仍在缓冲」的音频，时长会再涨一点

- 现象：`Pause` 后 `ElapsedMs` 还会继续增长几十（真机）到上千毫秒（合成源），然后才稳定。
- 复现条件：暂停瞬间环形缓冲里还有未落盘的数据。
- 根因：这些是**暂停前真实采到的音频**，丢掉就等于丢录音；语义上「暂停」应停止继续采集，而不是回滚已采内容。
- 临时处置：保持「缓冲排空后才停止推进时间轴」的行为，注释写明；测试用 `waitStableElapsed` 等稳定后再断言（合成源比实时快几百倍，不能用固定 sleep 断言）。
- 状态：改设计（属于刻意语义，建议在 `ARCH §6.2` 的 `recording→paused` 副作用里补一句「暂停保留已采音频」）

## S2-13 环形缓冲持续溢出时日志刷屏

- 现象：磁盘跟不上时每 250ms 一条「已丢弃最旧音频」，日志被淹没。
- 复现条件：环形缓冲持续满（写入慢于采集）。
- 根因：溢出告警写在了指标回调里（4Hz）。
- 临时处置：只在**首次**溢出时告警一次（`run.dropsLogged`），累积丢弃量在封存日志里输出 `dropped`。
- 状态：已解决

## S2-14 `API-2.7` 写「targetPath 空则弹原生另存为」，但后端不该弹 UI

- 现象：`ExportAudio` 传空 `targetPath` 会返回 `ERR_INVALID_ARGUMENT` 并提示「请先用原生另存为对话框选择目标文件」。
- 复现条件：前端不传路径直接调用。
- 根因：`CLAUDE.md` 铁律要求原生对话框统一走前端 `NativeDialogs`（禁止后端弹窗）；文档表述是产品视角而非实现分层视角。
- 临时处置：后端只负责「复制到给定路径」；空路径明确报错而不是猜一个默认位置（猜错会写到用户找不到的地方）。`ExportFileName` 提供前后端一致的默认文件名（含非法字符清洗）。
- 状态：改设计（建议把 `API-2.7` 的描述改成「targetPath 必填，由前端弹窗取得」）

## S2-15 `API-2.6 RecoverSession` 的错误码列表与实现不符

- 现象：`API-2.6` 写可能错误为 `ERR_MODEL_NOT_FOUND`、`ERR_DB_WRITE`，但修复会话与模型无关；实际会返回 `ERR_NOT_FOUND`（会话不存在）。
- 复现条件：对不存在的 sessionID 调 `RecoverSession`。
- 根因：文档疑似从「重新转写」语义沿用下来的残留（v1.3.0 把 `Retranscribe` 改成 `ExportAudio` 时未同步）。
- 临时处置：实现按真实语义返回 `ERR_NOT_FOUND` / `ERR_DB_WRITE` / `ERR_INVALID_ARGUMENT`。
- 状态：未解决（需修 `docs/API.md` §2.2 的错误码列；涉及前端错误文案映射）

## S2-16 设备丢失后 Resume 只重连一次，没有退避重试

- 现象：设备拔出导致 `recording → paused` 后，`Resume` 只调一次 `OpenChannel`；失败即停留 `paused`。
- 复现条件：源在暂停期间仍不可用。
- 根因：`ARCH §4.1.4` 的「重连 3 次 + 退避」属于 capture 层（`Channel.Err()` + Monitor）与后续 UC-03 重连逻辑；S2 只做「会话不丢」的降级。
- 临时处置：一次重试即返回 `ERR_CAPTURE_UNAVAILABLE`，UI 引导用户重选源或稍后再试；已录音频完整保留，随时可 Stop 封存。
- 状态：未解决（重连退避与「目标应用退出 3 次失败 → interrupted」由 S2 之后的设备韧性工作补齐）

## S2-17 指标回调里回查录制状态会造成收尾与回调相互等待

- 现象：`Stop` 偶发卡到 3s 超时（`等待落盘超时` 告警），日志显示耗时异常。
- 复现条件：L3 在 `onMetrics` 里调用 `rec.State()`（要拿持有 L2 大锁）。
- 根因：`finalizeLocked` 持锁等落盘 goroutine 结束，而落盘 goroutine 正在回调里抢同一把锁 → 相互等待直到超时。
- 临时处置：L3 改为**缓存** `sessionID`（来自 EV-01 快照），指标回调不再回查 L2；并在 `recordingsvc` 里注释锁序原因。
- 状态：已解决（通用约束：L2 的回调实现必须非阻塞、且不得回调 L2 的带锁方法）

## S2-18 单会话音频写盘只有一路落盘 goroutine，慢盘可能丢尾部

- 现象：磁盘极慢时环形缓冲溢出（`dropped > 0`），已经丢弃的音频不可恢复。
- 复现条件：落盘持续慢于采集（机械盘 + 杀毒软件扫描 + 同时转写）。
- 根因：3s 环形缓冲是唯一缓冲；超出即按设计丢最旧（`ARCH §4.3` 明确要丢最旧而不是阻塞采集）。
- 临时处置：丢最旧 + 计数 + 首次告警 + 封存时输出总丢弃量；采集永不阻塞（阻塞会导致源侧 WASAPI 队列溢出，损失更大）。
- 状态：已绕过（如真机频繁出现，应调大 `RingBufferSeconds` 或降低转写并发，而非改丢帧策略）
