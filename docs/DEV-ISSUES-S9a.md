# DEV-ISSUES S9-a（FR-08 隐私边界）

本文件记录 S9-a（隐私边界域）实施过程中发现、但不在本轮授权范围内修复的问题。
格式：`## S9A-NN 标题` + 现象 / 复现条件 / 根因 / 临时处置 / 状态。

状态取值：未解决 / 已绕过 / 已解决 / 改设计。

---

## S9A-01 `outbound_log` 缺 purpose / 方向字节 / status / duration 列

- 现象：S9-a 要求出站记录覆盖 `kind`、`purpose`、脱敏 URL、`bytesIn`、`bytesOut`、`status`、`errorCode`、`duration`，但表里只有 `at` / `host` / `kind` / `audio` / `bytes` / `result` 六列。
- 复现条件：读 `internal/storage/models_model.go` 的 `OutboundLog` 与 [docs/DATA-MODEL.md](./DATA-MODEL.md) TBL-12。
- 根因：TBL-12 设计时只要求「域名 + 是否含音频 + 结果」，后续需求（发起前可见、失败也留痕、耗时诊断）超出其表达力；`kind` 列宽 16 字节装不下 `kind/purpose`（`online-asr/finalize` 已 21 字节）。
- 临时处置：
  - `purpose` 只进结构化日志（`logx`），不落库；因此 `contract.OutboundRecord` **不设** `Purpose` 字段，避免发布一个恒空字段；
  - `bytesIn + bytesOut` 合并写入 `bytes` 列；
  - `status` / `errorCode` 折进 `result` 列（`ok` / `http-500` / `ERR_*` / `pending` / `blocked`）；
  - `duration` 只进结构化日志。
- 状态：未解决（需在 `internal/storage` 增列并同步 DOC-04 TBL-12；本轮禁止修改 `internal/storage/**`）。

---

## S9A-02 `app_settings` 没有「用户离线开关」字段

- 现象：`FR-08` 要求「提供全局离线开关，开启后禁止一切出站」，但 `AppSettings` 只有 `no_audio_persist` / `log_level` 两个隐私字段，没有 offline 列；`contract.PrivacySettings` 同样没有。
- 复现条件：`internal/storage/models.go` 的 `AppSettings`（「—— 隐私 ——」段）与 `internal/contract/settings.go` 的 `PrivacySettings`。
- 根因：DOC-04 TBL-02 未登记该字段；`FR-08` 的离线开关在 ROADMAP 仍是未实现项，此前只靠环境变量 `MANDARYN_OFFLINE`。
- 临时处置：用户层开关落在开放键值表 `preferences`，键名 `privacy.offline`（值 JSON `true` / `false`），由 `privacysvc.SetOffline` 读写；离线判定 = `env || user`（`privacy.Gate`）。同时通过 `privacy.Gate.InstallGlobal` 把用户层注册进 `internal/outbound`，让 `asr` / `models` / `enginesvc` 等既有调用方**无需改动**即遵守该开关。
- 状态：已绕过（建议二选一并写进文档：① DOC-04 TBL-02 增 `offline_mode` 列 + `PrivacySettings.offline`；② 正式确认 `preferences` 键 `privacy.offline` 为标准位置）。

---

## S9A-03 无法判定「历史上某一时刻离线是否生效」

- 现象：零出站自检只能拿**自检执行时刻**的离线状态去判定窗口内的记录；若用户在窗口内先在线后离线（或反之），判定口径与事实可能不一致。
- 复现条件：在 `outbound_log` 有记录的同时切换离线开关，再 `RunAudit`。
- 根因：`outbound_log` 不记录开关状态，也没有开关审计表。
- 临时处置：`AuditReport.Notes` 明确写出该口径（`offline state is evaluated at audit time; outbound_log keeps no switch history`），保证结论是**可判定且解释得清**的；离线生效期间的真实出站仍被判为 `real-outbound-while-offline` 硬违例。
- 状态：改设计（接受当前口径）。若产品要求「历史可判定」，需新增开关审计表或在记录里冗余开关快照。

---

## S9A-04 诊断包缺「模块清单」与「日志副本」（等 app.go 接线）

- 现象：`privacysvc.Deps.Modules` / `Deps.LogDir` 目前没有生产注入点，因此真实运行路径生成的诊断包不含模块清单，也不含日志脱敏副本。
- 复现条件：不带 `Modules` / `LogDir` 构造 `privacysvc.New` 后调 `ExportDiagnostics`。
- 根因：装配集中在 `internal/app/app.go`，本轮禁止修改（并行开发冲突面大）。
- 临时处置：两者都做成可选依赖，nil 时安全降级（诊断包其余部分照常产出）；单测已验证注入后模块清单与日志副本（含中文正文行剔除）均正确。
- 状态：未解决（待 `internal/app` 装配 `privacysvc` 时注入 `Modules` 与 `LogDir`，并注册服务）。

---

## S9A-05 `docs/API.md` 缺本域服务方法条目

- 现象：API.md 只有 `API-9.7 SetPrivacy`、`API-9.8 GetOutboundLog`，没有离线状态、分页查询、清空、零出站自检、诊断包导出、离线开关写入的签名。
- 复现条件：`grep -n "privacysvc\|Diagnostics\|outbound" docs/API.md`。
- 根因：本域此前只有「隐私设置」两个接口，S9-a 才补齐可验证性与自证能力。
- 临时处置：按 [docs/ARCHITECTURE.md](./ARCHITECTURE.md) §8 与 PRD `FR-08` 实现最小集，签名以代码为准（见下方「待补 API.md 条目」）。
- 状态：未解决（需补 API.md；本轮禁止改其它文档）。

---

## S9A-06 本机无法做 `CGO_ENABLED=1` 的 Linux 交叉编译

- 现象：`CGO_ENABLED=1 GOOS=linux go build ./internal/privacy/...` 在 `runtime/cgo` 阶段失败（`grp.h: No such file or directory`、`sys/mman.h` 缺失、`sigaction` 未声明）。
- 复现条件：Windows + mingw（ucrt64）环境执行上述命令。
- 根因：本机没有 Linux sysroot / 交叉 C 工具链；与代码无关（`internal/privacy` 与 `privacysvc` 都是纯 Go，不 import CGO 包）。
- 临时处置：用 `CGO_ENABLED=0 GOOS=linux go build ./internal/privacy/... ./internal/services/privacysvc/...` 验证可移植性（通过），`GOOS=darwin`（CGO 开启）交叉编译亦通过；CGO 全量交叉编译留给 CI（构建脚本走 Docker）。
- 状态：已绕过

---

## S9A-07 诊断脱敏自检失败没有专用错误码

- 现象：`BuildBundle` 的自检（产物里仍有正文 / 密钥残留）失败时，薄壳只能借用 `ERR_STATE_FORBIDDEN` 并附 detail。
- 复现条件：让 `privacy.BuildBundle` 返回 `ErrRedactionResidual`（极端构造）。
- 根因：DOC-03 §5 的 33 个错误码里没有「脱敏自检失败」，新增码需同步 DOC-03 §5 与前端 `errors.<code>` 文案，本轮不在授权范围。
- 临时处置：借用 `ERR_STATE_FORBIDDEN` + `action=view-diagnostics` + 明确 detail；行为是**失败即不返回产物**（宁可没有诊断包也不泄漏）。
- 状态：未解决（建议新增 `ERR_DIAGNOSTICS_REDACTION`）。

---

## 待补 `docs/API.md` 条目（S9A-05 的具体内容）

新增一节 `§2.10 privacysvc`（错峰编号可按文档现状调整），方法如下：

| 编号 | 方法 | 签名 | 说明 | 错误码 |
|------|------|------|------|--------|
| `API-9.16` | `GetStatus` | `() (PrivacyStatus, error)` | 离线是否生效、来源（`env`/`user`/`env+user`）、接管变量名、最近 24h 出站计数、保留时长 | — |
| `API-9.17` | `SetOffline` | `(enabled bool) (PrivacyStatus, error)` | 写用户离线开关（`preferences.privacy.offline`）；env 接管无法在此解除 | `ERR_DB_WRITE`、`ERR_STATE_FORBIDDEN` |
| `API-9.18` | `ListOutbound` | `(limit, offset int) (OutboundPage, error)` | 分页读取出站记录（新的在前）；`limit` 非法归一为 100 | `ERR_DB_WRITE` |
| `API-9.19` | `ClearOutbound` | `() (ClearResult, error)` | 清空出站记录，返回删除条数 | `ERR_DB_WRITE` |
| `API-9.20` | `RunAudit` | `(filter AuditFilter) (AuditReport, error)` | 零出站自检：`zeroOutbound` 结论 + 违例明细 + 口径说明 | `ERR_DB_WRITE`、`ERR_STATE_FORBIDDEN` |
| `API-9.21` | `ExportDiagnostics` | `() (DiagnosticsArtifact, error)` | 返回**已脱敏**诊断包字节（前端经 NativeDialogs 另存）；自检失败不返回产物 | `ERR_STATE_FORBIDDEN` |

新增 DTO（已在 `internal/contract/privacy.go` 实现）：`PrivacyStatus`、`OutboundRecord`、`OutboundPage`、`ClearResult`、`AuditFilter`、`AuditFinding`、`AuditReport`、`DiagnosticsArtifact`。

另需在 `API-9.8 GetOutboundLog` 处加注：该接口等价于 `ListOutbound` 的全量形态，长期建议收敛到 `ListOutbound`，避免两套出站查询口径。
