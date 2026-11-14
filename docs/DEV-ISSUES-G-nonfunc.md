# DEV-ISSUES — G/非功能验收留证

> Wave G 第四项：把 ROADMAP 14.3「非功能验收」里**能在本机留证**的条目变成可复核的证据。
> 三项依赖真实流式引擎（FFD/SLD 时延）与真实语音链路（回放精度长会话）的条目另见分域记录。

---

## 1. 隐私验收（离线零出站 + 日志无正文）—— **已留证**

命令：

```bash
go test -count=1 -v -run "TestAudit|TestGate|TestPipe" ./internal/privacy/
```

结果（全部 PASS）：

```
TestAuditPassesWithNoRecords              无出站记录时审计通过（不误报）
TestAuditFlagsRealOutboundWhileOffline    离线期真出站 → 必须被判违规（反向用例）
TestAuditIgnoresBlockedAttempts           被闸门拦下的尝试不算违规
TestAuditFlagsPending                     停在 pending 的出站也要暴露
TestAuditOnlineRealOutboundNotViolation   在线模式下的正常出站不算违规
TestAuditFilterByKindAndWindow            按类型与时间窗过滤
TestAuditCanceledContext                  取消的上下文不卡死
TestAuditNilRecorder                      无 recorder 时不 panic
TestGateEnvOffline                        MANDARYN_OFFLINE=1 → 拒绝
TestGateUserOffline                       用户开关 → 拒绝
TestGateEnvOrUser                         两者取或
TestGateCheckRejectsWithErrorCode         拒绝时给 ERR_OFFLINE_ENFORCED
TestGateAllowsOnline                      在线放行
TestPipeOfflineMakesZeroRequests          离线时**零请求**（httptest 断言）
```

**结论**：离线零出站是**机制级**保证（发请求前拒绝，且用 httptest 断言零请求），不是「约定俗成」。
反向用例（`TestAuditFlagsRealOutboundWhileOffline`）证明审计不是永真。

日志侧：`TestRedact*` 系列覆盖正文、声纹向量、密钥的脱敏；诊断包在不安全时整包丢弃日志（`TestBuildBundleDropsAllLogsWhenUnsafe`）。

**仍需人工**：真机抓包（Wireshark）与真实日志人工审计 —— 属外部工具验收，本机自动化替代见上。

---

## 2. 迁移回滚验收 —— **已留证**

命令：

```bash
go test -count=1 -v -run "TestMigrate" ./internal/models/
```

**14 个用例全部 PASS**。口径是「整棵目录相对路径 → sha256 快照」对比，证明失败/取消后**旧根逐文件不变**，
覆盖：预检失败不写对外状态、`verifying` 后取消、切换成功后取消不删旧目录、`switching` 半途残骸回搬、
`bundles` 注册表迁移前后逐字段一致。

（详见 `DEV-ISSUES-E.md` E07 系列。）

---

## 3. 预算限流验收（暂停后自动恢复，录制不中断）—— **已留证**

```bash
go test -count=1 -v -run "TestBudgetResumeFromP|TestBudgetHysteresis" ./internal/perf/
```

```
TestBudgetResumeFromP     PASS   P 档（暂停推理）在额度恢复后自动回到工作档
TestBudgetHysteresisNoFlap PASS  滞回：降档 3s 确认、升档 15s 且留 10% 余量，不抖动
```

「录制不中断」由设计保证并有反射测试守护：`BudgetState` 上**不存在**任何停止录制的方法/字段，
`Recorder.SetBudgetOverlay` 只叠加 `throttled`/`throttleLevel`/`suspended` 标记与时间轴打点。

**仍需人工**：真实压测（跑满 CPU 的外部进程）观察全程音频连续性 —— 属长时人工验收。

---

## 4. 可靠性：崩溃恢复可回放 —— **已留证**

```bash
go test -count=1 -v -run "TestRecover|TestRepairWAV" ./internal/recording/
```

```
TestRecoverUnfinishedRepairsCrashedSession   未正常结束的会话 → 按实际长度补 WAV 头并置 interrupted
TestRecoverUnfinishedMarksMissingAudioFailed 音频缺失 → failed（不假装可回放）
TestRecoverNoUnfinishedIsNoop                没有残留时零动作
TestRecoverSessionByID                       单会话恢复
TestRepairWAVAfterCrash                      头未回填的 WAV 可被修复
TestRepairWAVTrimsHalfFrame                  半帧尾部被裁掉（避免残帧噪音）
TestRepairWAVRejectsNonWAV                   非 WAV 不硬修
```

---

## 5. 可靠性：连续长时间录制内存无明显增长 —— **本轮新增留证**

ROADMAP 原文这条此前**只有设计意图、没有任何实测**。新增测试
[internal/recording/memory_test.go](../internal/recording/memory_test.go)：

- 用假通道（48kHz 立体声 → 16kHz 单声道，走完整重采样 + 下混 + 落盘路径）连续录制；
- 预热后取 `HeapAlloc` 基线，之后每轮 `runtime.GC()` 再采样；
- 断言「峰值 ≤ 基线 + 8MiB」。

实测输出：

```
基线：堆 1.28 MiB（已录 1534ms）
第 1 轮：堆 1.24 MiB（已录 3.4s）
第 2 轮：堆 1.25 MiB（已录 4.6s）
第 3 轮：堆 1.25 MiB（已录 6.1s）
第 4 轮：堆 1.25 MiB（已录 7.5s）
第 5 轮：堆 1.25 MiB（已录 9.0s）
第 6 轮：堆 1.25 MiB（已录 10.5s）
第 7 轮：堆 1.25 MiB（已录 12.1s）
第 8 轮：堆 1.25 MiB（已录 13.6s）
--- PASS
录制已封存 status=completed durationMs=13652 frames=218438 bytes=436920 dropped=0
```

**结论**：堆在 1.25 MiB 附近**完全持平**，没有随时长增长 —— 与「环形缓冲固定容量 + 每 200ms 刷盘释放」
的设计一致。这也是该设计的关键验证：录制路径上不存在与时长相关的累积结构。

**局限（如实说明）**：本测试用假通道加速，覆盖的是**样本吞吐量**而非真实墙钟时长；
GOGC 与分配器缓存会让小规模采样有噪声，所以用「基线 + 8MiB 容差」而非「零增长」。
若要验收「连续 8 小时真实录制」，仍需实机长跑 —— 本测试只能证明**不存在泄漏结构**，不能替代墙钟长跑。

顺带修掉一个测试自身的显示缺陷：`uint64` 相减在峰低于基线时下溢，日志曾打印出天文数字，
改为有符号差值（`int64(peak)-int64(baseline)`）。

---

## 6. 回放同步精度（288）—— 部分留证，受限于真实语音链路

已有事实（Wave E 交付时实测）：高亮唯一时间基准是 `audio.currentTime`，rAF 每帧二分查找，
切换误差 ≈1 帧（≈17ms），**远优于 ≤80ms 要求**。

**未闭环**：ROADMAP 要求的「长会话下不劣化」需要千句级真实会话。Wave F 已把回放改为分页（200 条/页、
窗口化渲染），理论上长会话不会比短会话差（渲染量与总句数无关），但**尚无千句级实测数据**。
需要真实语音长录音或可播放的种子会话 —— 见 `DEV-ISSUES-F-paged.md` §6 的人工验证三步。

**状态**：待真实长会话后补测，不阻塞。

---

## 7. 边界：哪些非功能验收本机做不了

| 条目 | 为什么做不了 | 需要的条件 |
|---|---|---|
| 时延指标 FFD/SLD | 需要真实流式引擎（Wave G 正在补）+ 真实语音；阈值按 PRD P50/P95 口径 20 次采样 | 流式模型包就位后即可在本机采样 |
| 隐私真机抓包 | 需要外部抓包工具与「跑满各功能」的用例 | Wireshark + 人工流程 |
| 预算压测音频连续性 | 需要外部 CPU 压力进程持续数分钟 | 压测脚本 + 人工听检 |
| 三平台产物 | 需要 macOS / Linux 机器或 CI | 跨平台构建环境 |
