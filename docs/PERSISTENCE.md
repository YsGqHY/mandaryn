# Mandaryn 持久化实现指南（PERSISTENCE）

| 项 | 值 |
|----|----|
| 文档编号 | DOC-09 |
| 文档版本 | **v1.0.0** |
| 产品基线 | Mandaryn v0.1.0 |
| 状态 | 生效（实现指南） |
| 上游文档 | [ARCHITECTURE.md](./ARCHITECTURE.md)、[DATA-MODEL.md](./DATA-MODEL.md)、[DEVELOPMENT.md](./DEVELOPMENT.md) |
| 下游文档 | — |

> 本文是**后端持久化的实现指南**（怎么加表、怎么接入业务 service、路径切换怎么不出错）。
> 表结构与字段定义属 [DATA-MODEL.md](./DATA-MODEL.md)（DOC-04），本文不重复列出。
> 前端侧持久化铁律见 [frontend/CLAUDE.md](../frontend/CLAUDE.md)。
> 更细的使用范例（含反例）见 `.claude/skills/foundation-persistence/SKILL.md`。

---

## 1. 数据位置

应用所有跨会话状态存到 `~/Library/Application Support/Mandaryn/mandaryn.db`（macOS）/ `%AppData%\Mandaryn\mandaryn.db`（Windows）/ `~/.local/share/Mandaryn/mandaryn.db`（Linux），位置可由用户在「设置 → 数据存储」里改。配置文件 `storage.json` 与默认数据库放在同目录（独立于 db）。

> ⚠️ 当前实现仍使用旧目录名与旧文件名（`AppDirName = "Foundation"`、`DBFileName = "foundation.db"`），
> 以 [ARCHITECTURE.md](./ARCHITECTURE.md) §10.1「应用标识收尾」的现状表为准。本文按目标标识书写。

## 2. 数据根目录覆盖

设置环境变量 `MANDARYN_DATA_DIR=<dir>`（`internal/storage/paths.go` 的 `DataDirEnv`，当前实现为 `FOUNDATION_DATA_DIR`）后，数据根直接用该目录（不再拼 `AppDirName`）；`cryptox` 主密钥放同目录、`logx` 日志写 `<dir>/logs`。用于便携版 / E2E / server 模式隔离数据。

## 3. 两套根目录不可混淆

`dataRoot`（库 / 密钥 / 日志 / 音频）由 `storagesvc` 管；`modelRoot`（默认 `~/.mandaryn`，模型包）由 `modelssvc` 管。二者迁移路径与回滚语义不同，**不要合并**；也**不要**让任何模块自己读环境变量决定路径——统一走 `internal/paths.Resolver`（`ADR-02`），否则数据库 / 密钥 / 日志可能落在不同根目录。

## 4. 驱动与 ORM

`modernc.org/sqlite`（纯 Go，无 CGO 需求）+ `gorm.io/gorm` + `github.com/glebarez/sqlite`（GORM 对接 modernc 的适配）。

## 5. 关键 PRAGMA

WAL / `synchronous=NORMAL` / `busy_timeout=5000` / `cache_size=-64000` / `mmap_size=256MB` / `temp_store=MEMORY` / `foreign_keys=ON`。

## 6. 加新表：零迁移文件

**模型注册中心**是 `internal/storage/models.go` 的 `AllModels` 数组。

```
1. 在 internal/storage/ 写 GORM 结构体（建议新文件 models_<domain>.go 拆开）
2. 把 &YourModel{} 追加到 AllModels
3. 重启 → AutoMigrate 自动建表 / 加列 / 加索引
```

GORM AutoMigrate **不会**做：删列、删表、重命名字段；这些保留旧 schema 不丢数据。需要重命名字段时业务方手写一次性数据迁移钩子。

## 7. 业务 service 接入持久化

```go
// internal/services/<domain>/<domain>.go
type Service struct {
    holder *storage.Holder // 不要 capture *storage.DB —— 切换路径会替换它
}

func (s *Service) DoSomething(ctx context.Context) error {
    return s.holder.Current().GORM.WithContext(ctx).Save(...).Error
}
```

切换路径时（`storagesvc.SetCustomPath`）会原子替换 holder 内的 DB 指针，业务 service 继续从 `holder.Current()` 取永远拿到活跃句柄。

## 8. 表统计与清空

设置页「数据存储」按表展示字节数与行数，并可逐表清空（清空经原生确认对话框）。是否允许清空由 `AllModels` 中每个 `ModelDescriptor` 的 `Clearable` 决定（如 `app_settings` 不可清空，`preferences` 可清空）。

## 9. 前端读取模式

前端不直接访问数据库，一律经 Provider 异步读取：`PreferencesProvider` / `AppSettingsProvider` 在应用启动时读回并 `ready`，之后写入即广播同步。**禁止**前端使用 `localStorage` / `sessionStorage` 保存任何跨会话状态。

## 10. 变更记录

| 日期 | 版本 | 变更 | 作者 |
|------|------|------|------|
| 2026-11-11 | v1.0.0 | 首版：从根 `CLAUDE.md` 迁出持久化实现指南（数据位置、环境变量覆盖、两套根目录、驱动与 PRAGMA、加表三步、Holder 接入、表统计清空、前端读取模式），`CLAUDE.md` 改为引用本文 | Kether |
