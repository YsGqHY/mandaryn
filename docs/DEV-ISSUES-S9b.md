# DEV-ISSUES — S9-b（FR-09 快捷键体系与精简 UX 后端支撑）

> 本次开发范围：`internal/hotkeys`（L2 领域包）+ 精简模式后端支撑。
> 不涉及 `internal/app` 装配、前端页面、`events` / `storage` / `recording` 等他人负责的文件。
> 日期：2026-11-11

## S9B-01 系统占用只能在「保存时试注册」才知道，无法事前探测
- 现象：`DetectConflicts`（API-8.5）只能判断「软件内重复」与「系统保留组合」，无法回答「这个键现在被别的程序占着吗」。
- 复现条件：`DetectConflicts("Ctrl+Alt+R")` 在另一个程序已注册该全局热键时返回 `ok=true`。
- 根因：Wails 只提供 `GlobalShortcut.Register` 的返回值来表达占用；没有「查询系统级占用」的 API（`IsRegistered` 只反映本应用自己的注册）。真实探测必须在 `Set` 里做「试注册 → 失败回滚」，不能放在只读方法里（会有劫持副作用）。
- 临时处置：`Set`（API-8.2）在冲突检测通过后立刻重建注册；若该动作注册失败则回滚到原绑定并返回 `ERR_HOTKEY_REGISTER`。`DetectConflicts` 明确只报 `duplicate` / `system`（保留组合）两类。
- 状态：已绕过

## S9B-02 加速键格式非法没有专属错误码
- 现象：`"Ctrl"`、`"Ctrl++"`、空串等格式错误返回 `ERR_INVALID_ARGUMENT`，与其它参数错误同码。
- 复现条件：`Set(action, "")`。
- 根因：`internal/apierr` 的码表是唯一权威（DOC-03 §5），本域只登记了 `ERR_HOTKEY_CONFLICT` / `ERR_HOTKEY_REGISTER`；铁律禁止业务代码自造码，故未新增。
- 临时处置：错误 `Detail` 里带稳定原因键 `reason=<empty|too-long|empty-key|no-key|unknown-token|unknown-key|duplicate-modifier|conflicting-modifier|too-many-parts>`，前端按 reason 映射 i18n 文案。`Reason` 常量在 `internal/hotkeys/accel.go`。
- 状态：已绕过（建议 DOC-03 §5 后续为热键格式错误单列一码，或明文承认走 `ERR_INVALID_ARGUMENT` + reason）

## S9B-03 events.go 没有热键事件常量
- 现象：绑定变更 / 精简模式变更无法用现成事件名广播。
- 复现条件：装配层要发 Wails 事件时找不到常量。
- 根因：`internal/events/events.go` 本次禁止修改（并行开发中），且 DOC-03 §3 未定义热键事件。
- 临时处置：`hotkeys.Emit{Changed, Lean}` 回调注入，事件名由装配层决定；**建议** `hotkeys:changed`（payload = `contract.Binding`）与 `hotkeys:lean`（payload = `hotkeys.LeanState`），并补进 `events.go` 与 DOC-03 §3。
- 状态：未解决（需装配层/文档补常量）

## S9B-04 DOC-03 §2.8 的方法表少于本域实际能力
- 现象：API.md 只列了 `List / Set / Reset / Disable / DetectConflicts / ResetAll`，没有「统一触发入口」与「精简模式」。
- 复现条件：按 API.md 做前端时无法通过托盘/按钮复用动作逻辑，也拿不到精简模式清单。
- 根因：S9-b 的任务范围要求提供 `Trigger` 与 `GetLeanState/SetLeanMode`，而文档停留在上一版。
- 临时处置：本包额外导出 `Trigger`、`ListActions`、`Available`、`GetLeanState`、`SetLeanMode`（均无 func 参数，可安全生成绑定）。`List`/`Set`/`Reset`/`Disable`/`ResetAll`/`DetectConflicts` 的签名严格照 API.md。
- 状态：未解决（需回填 DOC-03 §2.8）

## S9B-05 录制域没有对外导出的「标记关键时刻」能力
- 现象：无法提供「标记关键时刻」热键动作。
- 复现条件：查 `internal/recording` 只有私有的 `appendMarker`，`Recorder` 未导出 `Mark`。
- 根因：`internal/recording` 本次禁止修改；DOC-05 §2.2 的 17 个动作里也没有该动作。
- 临时处置：内置动作集严格按 DOC-05 §2.2（17 项）+ 1 项精简模式开关，不提供 Mark 动作。等录制域导出 `Mark` 后，只需在 `BuiltinActions()` 增一条 + 装配层 `SetHandler` 即可，无需改本包其它代码。
- 状态：改设计

## S9B-06 Wails 具名键表不含 Insert
- 现象：把某动作绑到 `Insert` 后注册必然失败，`RegisterErr=system`（设置页会标红）。
- 复现条件：`Set("playback.play", "Insert")`。
- 根因：wails v3 `application/keys.go` 的 `namedKeys` 只有 backspace/tab/return/enter/escape/方向键/space/delete/home/end/page up/page down/f1-f35/numlock；`insert` 不在其中，且长度 > 1 不满足单字符兜底。
- 临时处置：仍允许解析与保存（不阻断用户操作），失败原因如实反映在 `Binding.RegisterErr`，不静默吞掉。
- 状态：已绕过

## S9B-07 Linux 交叉编译需关闭 CGO
- 现象：`CGO_ENABLED=1 GOOS=linux go build ./internal/hotkeys/...` 在 Windows 主机上失败（`runtime/cgo` 编译错误：`sys/mman.h`、`grp.h` 缺失等）。
- 复现条件：见上（本机只有 ucrt64 的 Windows gcc，没有 linux 交叉工具链）。
- 根因：交叉编译 cgo 需要目标平台 C 工具链，属工具链限制，与本包代码无关（本包依赖链无 cgo 代码：sqlite 用 `glebarez/sqlite` 纯 Go 实现）。
- 临时处置：Linux 交叉编译用 `CGO_ENABLED=0`，已通过；`GOOS=darwin` 在 `CGO_ENABLED=1` 与 `0` 下均通过。正式三平台产物按 `ADR-01` 在各平台本地构建，不受影响。
- 状态：已绕过

## S9B-08 全局热键的「系统键」在三平台同名不同义
- 现象：同一个 `Meta` 修饰键在不同平台是 macOS `⌘`、Windows `Win`、Linux `Super`；Wails 的修饰键词汇表里**没有** `win` / `meta`，只有 `cmd`/`command`/`ctrl`/`alt`/`option`/`shift`/`super`。
- 复现条件：给 Wails 传 `"Win+L"` 会解析失败。
- 根因：Wails `parseAccelerator` 的 `modifierMap` 缺 `win`/`meta`；而 UX-SPEC 的显示文案要求 macOS 用 ⌘、Windows 用 Win。
- 临时处置：三层表示分离 —— `String()` 跨平台规范形（落库/传输）、`Display(os)` 人读显示形（⌘⌥R / Ctrl+Alt+R / Win+L）、`PlatformString(os)` 交给 Wails 的平台形（`Cmd`/`Option`/`Super`）。冲突检测另用 `PlatformKey(os)`（修饰键按字典序排列），保证「macOS 上 ⌘⌥R 与 CmdOrCtrl+Alt+R 判为同一物理键」。
- 状态：已解决

## S9B-09 启动期与保存期的全局注册失败，处理策略不同
- 现象：同一个「全局组合被占用」的失败，启动时降级、保存时驳回。
- 复现条件：启动时 `Ctrl+Alt+R` 已被占用 → `record.toggle` 的 `Scope` 变 `app` 且 `RegisterErr=system`；用户手动改成被占用的组合 → 返回 `ERR_HOTKEY_REGISTER`，绑定不变。
- 根因：DOC-05 §2.3 两条规则（启动失败降级 / 保存被拒）本就不同，实现上必须区分「谁发起的」。
- 临时处置：`applyLocked()` 在启动路径对 global 失败自动回退到 app 作用域并标红；`Set()` 在保存路径读到 `effErr` 非空即回滚并报错。两条路径都在 `service.go` 注释里写明，避免后来者「统一」掉其中一条。
- 状态：已解决（需 DOC-03/DOC-05 明示两条路径）

## S9B-10 「精简模式」是本次新增的产品概念，文档里没有定义
- 现象：DOC-05 只有「精简原则」（§4 移除 7 项 / 合并 5 组），没有「精简模式开关」及其隐去清单。
- 复现条件：前端要渲染「精简模式」时无权威清单可依。
- 根因：S9-b 任务要求提供全局精简开关的后端支撑，属对 FR-09-3 的落地补充。
- 临时处置：`internal/hotkeys/lean.go` 把清单固化为后端权威副本 —— 保留 `status/record/library/session`（DOC-05 §5.1 的 L1 + L2），隐去 `settings.advanced` / `settings.diagnostics` / `models.admin` / `library.insights` / `diagnostics` 与 7 个零散控件（对应 §4.1 移除项与 §4.2 固定策略项）。清单与开关状态一起返回，前端不得各页各写一份。
- 状态：未解决（需产品/DOC-05 确认清单内容）

## S9B-11 「保存未改动的绑定」必须是无操作，否则默认值被平台化
- 现象：前端把列表里的显示形原样存回（macOS 的 `⌘⌥R`）时，若照常写覆盖，覆盖表里会存成 `Meta+Alt+R`，此后默认值调整不再跟随该动作。
- 复现条件：`List()` 取到 `record.toggle` 的 `⌘⌥R` → 不改动直接 `Set("record.toggle", "⌘⌥R")`。
- 根因：默认值用跨平台形 `CmdOrCtrl`，而显示形在 macOS 上是字形 `⌘`，解析回来必然是 `Meta`；两者规范形不同但物理键相同。
- 临时处置：`Set` 在写库前比较**平台物理键**（`Combo.PlatformKey`），相等则直接返回成功且不产生覆盖。这样「没改过的动作」永远保持跨平台默认值。副作用：用户若确实想「显式固定成 ⌘」，在 macOS 上做不到（物理等价）—— 这是刻意的，无实际损失。
- 状态：改设计
