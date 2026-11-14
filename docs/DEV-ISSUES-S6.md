# S6 开发问题记录（模型下载与模型仓库域）

本文件记录 S6（`FR-05` 模型来源与切换中的仓库/下载部分 + `FR-07` 模型目录与迁移）实现过程中
实际踩到的坑。格式固定，便于后续接手的人快速判断「是不是已知问题」。

状态取值：`未解决` / `已绕过` / `已解决` / `改设计`。

---

## S6-01 huggingface.co 不可达，清单主源不能放在 HF

- **现象**：按 `docs/ARCHITECTURE.md` §4.6.2 的「HTTPS + 可选镜像」实现时，若把 HF 作为主源，
  清单拉取在目标网络环境下必然超时，`ListAvailable` 每次都走兜底。
- **复现条件**：把 `MANDARYN_MODEL_CATALOG` 指向 `https://huggingface.co/...`。
- **根因**：该域名在目标网络不可达；`github.com` 与 `hf-mirror.com` 可达。
- **临时处置**：默认主源改为 GitHub Releases 目录
  （`https://github.com/<owner>/<repo>/releases/download/<tag>`），默认镜像为
  `https://hf-mirror.com/...`；两者都可以用 `MANDARYN_MODEL_CATALOG` /
  `MANDARYN_MODEL_MIRRORS` 覆盖（见 `internal/services/modelssvc/modelssvc.go`）。
  镜像改写规则单一可预测：**取文件 URL 的 path 部分接到镜像基址后**（`mirrorURL`）。
- **状态**：已解决（设计层面）

## S6-02 SOCKS5 依赖：`golang.org/x/net/proxy` 可直接使用，无需降级

- **现象**：任务书要求「先探测能否拉取 SOCKS5 依赖，拉不到就降级为明确错误码」。
- **复现条件**：导入 `golang.org/x/net/proxy` 后 `go build`。
- **根因**：`go.mod` 里 `golang.org/x/net v0.62.0` 已是（间接）依赖，`go.sum` 也存在完整
  模块哈希，因此直接 import 即可编译，无需改 `go.mod`（本项目禁止改）。
- **临时处置**：直接实现 SOCKS5（`proxy.FromURL` + `proxy.ContextDialer`），保留
  `ERR_DOWNLOAD_PROXY` 处理「dialer 构造失败」「host/port 非法」「密文解密失败」。
- **状态**：已解决

## S6-03 ETag 变化后「整包重下」会失败：必须清掉断点里的旧 ETag

- **现象**：续传时服务端内容已变（`If-Range` 不匹配）→ 服务端回 `200` 整包 →
  下载器按设计丢弃分片重下，但重下仍然失败，最终 `ERR_DOWNLOAD_NETWORK`。
- **复现条件**：先下完部分分片并记录 ETag=v1，随后远端换成 v2，再点继续下载。
- **根因**：重下时 `If-Range` 仍带断点里保存的 v1，服务端继续回整包，形成死循环，
  直到 `maxFileAttempts` 耗尽。
- **临时处置**：`errNeedRestart` 分支除了删分片、清 `partsDone`，还要 `setETag("")`。
  单测 `TestDownloadETagChangedRestartsWholeFile` 断言「先带旧 ETag 续传 → 再从 byte 0 重下」。
- **状态**：已解决

## S6-04 Windows 上 `os.Rename` 不能覆盖已存在目录

- **现象**：重复下载 / 升级同一个包时，`staging → models/<id>@<ver>` 的原子 rename 直接报错。
- **复现条件**：目标目录已存在时执行下载或离线导入。
- **根因**：`os.Rename` 在 Windows 上对目录不做「替换」语义（POSIX 才可能）。
- **临时处置**：`commitDir` 先把已存在的目标改名成 `.trash-<name>-<ts>` 让位，再 rename；
  清理走异步（删不掉也不影响可用性）。同一逻辑在迁移的 `switchRoot` 里复用。
- **状态**：已解决

## S6-05 并发拉清单会撞 `manifest.cache.json` 的 rename

- **现象**：3 个 bundle 同时开始下载时日志出现
  `write manifest cache failed: filex: rename: ... Access is denied.`
- **复现条件**：`go test -run TestDownloadConcurrencyLimit`（多个 `Download` 同时触发 `Fetch`）。
- **根因**：`httpx`/`filex.WriteAtomic` 的 `.tmp → 目标` rename 在 Windows 上遇到目标文件
  正在被另一个 rename 使用时失败；并发 `Fetch` 各自写一次缓存。
- **临时处置**：`Manager.fetchMu` 串行化 `Fetch`（清单拉取本来就是低频操作）。
  顺带避免同一个 TTL 窗口内重复打远端。
- **状态**：已解决

## S6-06 `go vet` 拒绝非恒定格式串

- **现象**：`internal/models/store.go`、`catalog.go` 里 `WithDetail(someString)` 触发
  `non-constant format string in call to ...WithDetail`。
- **复现条件**：`go vet ./internal/models/...`
- **根因**：`*apierr.Err.WithDetail` 是 printf 风格函数，project 的 vet 配置禁止非常量格式串。
- **临时处置**：统一改成 `WithDetail("%s", detail)`。
- **状态**：已解决

## S6-07 `GOOS=linux` + `CGO_ENABLED=1` 交叉编译在本机不可用

- **现象**：验收命令里的 `GOOS=linux go build ./internal/models/...` 失败，报错全在
  `runtime/cgo`（`sys/mman.h` / `grp.h` 缺失、`sigset_t` 未定义等）。
- **复现条件**：任何 `GOOS=linux` 且 `CGO_ENABLED=1` 的构建。
- **根因**：`runtime/cgo` 需要目标平台的 C 工具链，本机只有 MSYS2 UCRT64（Windows 目标）；
  报错与 `internal/models` 自身代码无关。
- **临时处置**：用 `CGO_ENABLED=0 GOOS=linux go build ./internal/models/...` 验证 Linux 平台代码
  （`fs_unix.go` 的 `syscall.Statfs` 等）。`GOOS=darwin` + `CGO_ENABLED=1` 可以通过。
  真正的 Linux 产物按 `ADR-01` 在 Linux 机器 / 容器里构建。
- **状态**：已绕过（环境限制）

## S6-08 错误码表里没有 `ERR_DOWNLOAD_PRECHECK`

- **现象**：任务书要求预检失败返回 `ERR_DOWNLOAD_DISK_FULL` / `ERR_DOWNLOAD_PRECHECK`，
  但 `internal/apierr` 只登记了 `ERR_DOWNLOAD_NETWORK` / `_CHECKSUM` / `_DISK_FULL` / `_PROXY`。
- **复现条件**：读 `internal/apierr/apierr.go` 的常量表。
- **根因**：`ERR_DOWNLOAD_PRECHECK` 未在 DOC-03 §5 登记；且本任务禁止改 `apierr`。
- **临时处置**：预检的两种失败复用既有语义——空间不足 → `ERR_DOWNLOAD_DISK_FULL`；
  目标不可写 → `ERR_PATH_UNWRITABLE`（`docs/ARCHITECTURE.md` §4.8 的既有码）。
  **未新增错误码**，避免出现「码表里有、前端 i18n 没有」的悬挂。
- **状态**：改设计（若产品坚持要 precheck 专属码，需同步 DOC-03 §5 + 前端 `errors.*`）

## S6-09 API-6.1 的 `(list, ERR_DOWNLOAD_NETWORK)` 双返回会丢列表

- **现象**：`ListAvailable` 远端刷新失败时按文档返回清单 + 错误；但 Wails 的 Promise 语义下
  reject 会丢弃返回值，前端拿不到「缓存/内置兜底」的内容，页面变空。
- **复现条件**：断网调用 `ListAvailable`。
- **根因**：跨端契约把「降级内容」和「降级原因」塞进同一个返回里，而错误通道不携带数据。
- **临时处置**：兜底内容以 `fallbackTTL=30s` 进内存缓存 —— 前端在收到
  `ERR_DOWNLOAD_NETWORK` 后**再调用一次** `ListAvailable`，30s 内不再触发网络，直接拿到内容
  且无错误。单测 `TestCatalogFallsBackToBuiltin` 覆盖「第二次调用无错误」。
- **状态**：未解决（契约层缺陷：建议后续把「清单来源」作为字段放进返回，而不是靠错误码表达）

## S6-10 `InstalledBundle.SizeBytes` 与 `BundleInfo.SizeBytes` 含义不同

- **现象**：已装包的 `SizeBytes` 比清单里的 `sizeBytes` 大几百字节，前端若直接对比会显示「不一致」。
- **复现条件**：`ListInstalled` 与 `ListAvailable` 并排展示。
- **根因**：已装包用 `dirSize` 统计**磁盘占用**（含包内 `manifest.json`），清单里是**下载体积**。
- **临时处置**：保留磁盘占用语义（`GetStoreStats.TotalBytes` 需要真实占用）；
  在 `scanInstalled` 里注明，前端文案用「占用」而不是「大小」。单测按 `>= payload` 断言。
- **状态**：改设计（已文档化）

## S6-11 「校验失败」结论必须粘住，且行不存在时要补插

- **现象**：`Verify` 返回 `OK=false` 后，`ListInstalled` 又显示 `installed`。
- **复现条件**：手工放入（或离线导入）的包 → 篡改文件 → `Verify` → 再列。
- **根因**：两个独立缺陷叠加：① 扫描只看文件系统存在性，没合并 TBL-08 的 `state`；
  ② `markBundleState` 只做 `UPDATE`，而手工/离线装的包没有对应行，`RowsAffected=0` 静默失败。
- **临时处置**：`bundleStateMap` 读回 `state`+`verified_at` 并在扫描时合并（`corrupt` 粘住直到
  重新校验通过）；`markBundleState` 在 `RowsAffected==0` 时补插一行。
- **状态**：已解决

## S6-12 异步清理的 `.trash-*` 会落在源目录的**父目录**

- **现象**：迁移完成后模型的旧目录不是就地消失，而是父目录里出现 `.trash-modelroot-<ts>`，
  少数情况下（文件被占用）会残留一段时间。
- **复现条件**：迁移完成后立刻 `ls` 源目录的父目录。
- **根因**：`removeDirAsync` 用「改名到同盘同父目录再删」的策略保证原子性；删除是异步的，
  Windows 上被占用的文件会让 `RemoveAll` 失败。
- **临时处置**：接受该行为（残留只占空间、不影响可用性，符合 ARCH §4.6.3 cleaning 阶段定义）；
  删除失败只记 `WARN`，不向上抛。启动时的 `Cleanup` 会清掉 `.staging-*`，但**不清** `.trash-*`
  （可能仍被占用）。
- **状态**：已绕过（后续可加「启动时清理超过 24h 的 .trash-*」）

## S6-13 测试里第二轮下载被「内存清单缓存」干扰

- **现象**：ETag 用例里第二轮下载报 `ERR_DOWNLOAD_CHECKSUM`，而内容其实是对的。
- **复现条件**：同一个 `Manager` 先失败一次、改远端内容与摘要、再续传。
- **根因**：`Manager.cat` 在 TTL（10 分钟）内不刷新，`lookup` 仍拿旧 `sha256` 去校验新内容。
- **临时处置**：测试改为**新建 Manager**（模拟「应用重启后续传」），这也更贴近真实场景
  （进程崩溃 → 重启 → 续传）。生产侧若需要「清单已更新但未刷新」，可显式调 `Fetch`。
- **状态**：已解决（测试方法）

## S6-14 迁移回滚测试无法用「删源文件」制造失败

- **现象**：在 `copying` 阶段的进度回调里删掉一个还没复制的源文件，迁移仍然成功（`done`）。
- **复现条件**：`TestMigrateRollbackOnCopyFailure` 的初版写法。
- **根因**：`filepath.WalkDir` 在下降目录时才列举条目；文件在列举之前被删，等于「本来就不存在」，
  不会报错，也不会进校验集合。这其实是**合理行为**，不是缺陷。
- **临时处置**：改成把「已被列举的普通文件」替换成同名目录 —— 复制时对目录做 `io.Copy` 在两个
  平台都会报错，从而稳定触发回滚。
- **状态**：已解决（测试方法）

## S6-15 `newTempID` 用 `/dev/urandom` 在 Windows 上不存在

- **现象**：Windows 上临时目录名退化成固定值，同名 staging 目录会相互干扰。
- **复现条件**：Windows 上连续创建两个临时区。
- **根因**：初版直接 `os.Open("/dev/urandom")`。
- **临时处置**：改用 `crypto/rand`（跨平台）；熵源异常时退化到纳秒时间戳（只用于临时目录名，
  不参与任何安全判定）。
- **状态**：已解决

## S6-16 终态事件早于数据库写入，消费方回读不到行

- **现象**：`TestRegistryPersistedWithDB` 偶发 `downloads rows = 0`。
- **复现条件**：收到 `EV-09` 的 `done` 状态后立刻查 `downloads` 表。
- **根因**：`finishTask` 里先 `emit` 再 `persistDownloadRow`，事件消费方可能抢在前面。
- **临时处置**：调整为「先落库、再发事件」（所有终态都适用）。
- **状态**：已解决

## S6-17 下载终态只能靠事件获取，没有轮询兜底

- **现象**：任务结束后从 `tasks` 表移除，`DownloadState` 的终态（`done`/`failed`）只存在于
  回调发出的 `EV-09` 里；若前端事件丢失，没有 `GetDownloadState` 之类的兜底查询。
- **复现条件**：下载完成时前端没有订阅 `models:download`。
- **根因**：`docs/API.md` §2.6 的 13 个方法里没有「查询下载状态」，只有 API-6.12 的迁移轮询。
- **临时处置**：终态同时写入 `downloads` 表（TBL-09 的 `state` 列），前端可通过
  `ListInstalled`/`ListAvailable` 的 `installed` 标记间接判断成功；失败原因需要靠事件。
- **状态**：未解决（契约缺口：建议后续补一个只读的下载状态查询方法）

## S6-18 `go mod tidy` 会把 `golang.org/x/net` 从 indirect 提升为 direct（未执行）

- **现象**：`go mod tidy -diff` 显示 `golang.org/x/net v0.62.0` 应从 `// indirect` 块移到
  直接依赖块（因为 S6 为 SOCKS5 代理 import 了 `golang.org/x/net/proxy`）。
- **复现条件**：`go mod tidy -diff`
- **根因**：`// indirect` 只是注释标记，**不影响编译**；`go build` / `go test` 在现有 go.mod 下
  完全正常（模块已 require 且 go.sum 有完整哈希）。
- **临时处置**：本任务禁止改 `go.mod` / `go.sum`，因此**不执行 tidy**，保留注释不准确的状态。
  下次允许动依赖清单时再跑一次 `go mod tidy` 即可（纯注释变更，无版本变化）。
  另注：`tidy -diff` 里 go-ole / sherpa-onnx / go-wca 的差异是 S1~S5 的既有改动，与 S6 无关。
- **状态**：已绕过
