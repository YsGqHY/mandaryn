# E-fr07 开发问题记录（FR-07 模型目录与迁移收尾）

本文件记录 Wave E-3（`FR-07` 模型目录与迁移）收尾过程中**实际发现并修复**的后端缺陷、
以及仍然存在的验证缺口。格式与既有 `DEV-ISSUES-*.md` 一致，状态取值：
`未解决` / `已绕过` / `已解决` / `改设计`。

本轮新增测试 11 条（`internal/models/migrate_rollback_test.go`）+ 2 条薄壳测试
（`internal/services/modelssvc/modelssvc_test.go`）。

---

## E07-01 预检失败不更新对外状态，`GetMigrationState` 返回上一次的 `done`

- **现象**：`MigrateModelRoot` 在预检阶段失败（目标不可写 / 空间不足 / 目标在源内）时，
  调用返回了错误，但随后 `GetMigrationState`（API-6.12 的轮询兜底）仍返回上一次迁移
  留下的 `done`。前端若靠轮询（事件丢失、或用户切页回来重挂载）就会把一次失败的迁移
  显示成「迁移完成」，进而以为模型已经在新目录。
- **复现条件**：先成功迁移一次（状态停在 `done`），再对一个不可写的目标调用
  `MigrateModelRoot`，然后 `GetMigrationState`。
- **根因**：`RunMigration` 的预检失败分支只 `migrating.Store(false)` 并返回一个临时构造的
  `MigrateState`，既没有写 `m.migState`，也没有走 `setMigratePhase`（不广播、不触发
  `OnMigrate`）。对外状态因此停留在历史值。
- **临时处置**：预检失败改走 `m.setMigratePhase(contract.MigrateFailed, err)`，
  状态与事件都收敛到 `failed` + `ERR_MIGRATE_PRECHECK`。
  回归锁定：`TestMigratePreflightFailureKeepsOldRootAndReportsFailed`（L2）、
  `TestServiceMigratePreflightFailureVisibleInState`（L3 薄壳 + 轮询接口）。
- **状态**：已解决

## E07-02 校验完成之后的「取消」被静默忽略，用户点了取消却把目录切走

- **现象**：迁移进入 `verifying` 后点「取消迁移」，界面上仍推进到 `switching` → `done`，
  旧目录还被按策略删掉了。用户的理解是「我取消了」，实际是一次完成的迁移。
- **复现条件**：在 `verifying` 阶段调用 `CancelMigration`。
- **根因**：只有 `copyTree` 逐文件检查了 `ctx.Done()`；`verifyCopied`、`switchRoot`、
  cleaning 都不看 ctx。校验一结束就没有任何取消检查点，取消请求被吞掉。
- **临时处置**：在 `verifying` 与 `switching` 之间加**最后一个安全取消点**
  （此时源目录完好、什么都没改，回滚零成本）：`ctx.Err() != nil` 即回滚。
  回归锁定：`TestMigrateCancelDuringVerifyingRollsBack`。
- **状态**：已解决

## E07-03 已切换成功后取消，会连旧目录一起删掉

- **现象**：`switching` 之后（`cleaning` 阶段）取消，收尾仍执行 `removeDirAsync(plan.From)`，
  旧目录被删。此时新目录已生效，功能上没错，但对「刚刚按了取消」的用户而言，
  数据仍在的那份副本被删掉是一次不可逆的意外。
- **复现条件**：`switching` 阶段触发取消。
- **根因**：`cleaning` 分支只区分 `copyOnly`，没考虑「迁移已被取消」。
- **临时处置**：`cleaning` 阶段若 `ctx.Err() != nil` 则保留源目录（双份共存），
  只记录日志、不删数据；宁可多占空间也不删掉唯一可用副本。
  回归锁定：`TestMigrateCancelDuringCleaningCompletesButKeepsSource`、
  `TestMigrateCancelAfterSwitchKeepsSourceDir`。
- **状态**：已解决

## E07-04 `switching` 中途失败会在目标目录留下「一半搬过去」的残骸

- **现象**：`switchRoot` 逐个把 staging 的顶层条目 rename 到目标根。若第 N 个条目失败，
  前 N-1 个已经落在目标目录里，随后 `rollback` 只删 staging —— 目标目录里留着
  `models/` 之类的半成品（DB 与生效根已回到旧目录，所以**不会**出现「两处都不完整」，
  但目标目录脏了，下次迁移还要靠改名让位去覆盖）。
- **复现条件**：切换阶段的 rename 失败（跨设备、权限变化、外部占用）。
- **根因**：`switchRoot` 没有记录已移动条目，失败时无法撤销已完成的 rename。
- **临时处置**：记录 `moved`，失败时（含「目标同名条目改名让位失败」这一支）逐个搬回
  staging，再由 `rollback` 删除 staging，使目标目录恢复到迁移前状态。
- **状态**：已解决（代码级；见 E07-06 的注入测试缺口）

## E07-05 回滚时底层细节码盖掉阶段码，前端只能显示「未知错误」

- **现象**：`switching` 阶段落库失败（库被锁 / 盘被拔）时，回滚状态里的 `lastError.code`
  是 `ERR_DB_WRITE`，而前端只登记了 `ERR_MIGRATE_*` 文案，用户看到的是兜底英文短句，
  读不到「切换失败，已回滚到原目录」这个真正有用的结论。
- **复现条件**：迁移过程中数据库不可写。
- **根因**：`errDTO(apierr.From(cause, code))` 中 `apierr.From` 对已是 `*apierr.Err`
  的错误**原样返回**，于是传入的阶段码被忽略。
- **临时处置**：新增 `phaseErrDTO(cause, code)`，强制把 DTO 的 `Code` 设为阶段码，
  原因保留在 `message` 里。回归锁定：`TestMigrateSwitchFailureRollsBackToOldRoot`
  断言 `lastError.code == ERR_MIGRATE_SWITCH`。
- **状态**：已解决

## E07-06 切换阶段「rename 半途失败」缺少自动化注入测试

- **现象**：E07-04 的修复（搬回已移动条目）目前只有代码与人工推演，没有确定性测试。
- **复现条件**：需要让 `switchRoot` 循环里的第二个 `os.Rename` 失败。
- **根因**：`os.Rename` 在 Windows 上对「普通文件 / 目录（即便非空）」都很宽容，
  要稳定构造失败必须引入文件系统故障注入层（对本包来说是为测试改生产结构）；
  现有 `switchRoot` 里可注入的失败点（落库失败）已被
  `TestMigrateSwitchFailureRollsBackToOldRoot` 覆盖，但它在第一个 rename 之前就返回了，
  走不到「已移动条目搬回」的分支。
- **临时处置**：保留代码级保证 + 现有三条回滚用例对「目标目录无 staging 残留、
  旧目录逐文件不变」的断言；如后续要做，建议给 `Manager` 加一个
  `renameFunc` 测试缝（默认 `os.Rename`）。
- **状态**：未解决（验证缺口，不影响可用性）

## E07-07 「目标空间不足」只有策略代码，无平台无关的确定性测试

- **现象**：`PlanMigrate` 用 `freeBytes(target) < totalBytes*1.15` 判定不可行，
  但单测里无法稳定造出「目标盘几乎满」的环境。
- **复现条件**：需要一个可用空间小于待迁移体积的挂载点。
- **根因**：磁盘容量不可 mock；本机也难以在测试中挂一个小 loop 设备（Windows）。
- **临时处置**：测试覆盖了同一分支的另一个入口（目标不可写 → `ERR_MIGRATE_PRECHECK`，
  见 `TestMigratePreflightFailureKeepsOldRootAndReportsFailed`）；空间判定的系数
  （`diskOverheadFactor = 1.15`）保持文档口径，靠人工验证。
- **状态**：未解决（验证缺口）

## E07-08 迁移计划不可行时，前端拿不到 `MigratePlan` 明细

- **现象**：`modelssvc.SetModelRoot` 在预检失败时按契约返回 `(plan, *apierr.Err)`；
  前端 `ModelsService.setModelRoot` 捕获后抛出 `AppError`，于是页面只显示错误码文案，
  「待迁移 X、目标只剩 Y」这类关键数字丢失。
- **复现条件**：目标不可写 / 空间不足时点「更改目录」。
- **根因**：错误与计划同时返回时的契约在薄壳层被 `throw` 抹平（前端只保留 code/message）。
- **临时处置**：页面按 `ERR_MIGRATE_PRECHECK` 的 i18n 文案提示「预检未通过，请确认目录可写
  且空间充足」；未改契约（本轮禁止改 `internal/contract`）。若要展示明细，需在薄壳增加
  一个「预检返回结构体（plan + error）」的 DTO，属于契约变更。
- **状态**：已绕过

## E07-09 `navigator.clipboard` 在 WebView 下不可靠，改用 Wails 运行时

- **现象**：解除接管的命令需要一键复制；用浏览器 `navigator.clipboard.writeText` 在
  某些 WebView / 非安全上下文下会 reject（`Undefined` 或权限错误）。
- **复现条件**：打包后（非 dev 服务器）点「复制命令」。
- **根因**：`navigator.clipboard` 需要安全上下文，Wails 本地资源不总是满足。
- **临时处置**：走 `@wailsio/runtime` 的 `Clipboard.SetText`，封装在
  `frontend/src/services/models/PathsService.ts#copyText`，View 不直接碰运行时 API。
- **状态**：已解决
