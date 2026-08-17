# PR-KA-D3 第三方实现 Prompt（整段复制即可）

把下面「PROMPT」围栏里的全部内容发给实现 agent。不要摘要。做完后把完成报告交回主 agent 做严格验收。

---

````
你是 MessageLoop（Go 实时消息平台）的实现工程师。项目根目录：

D:\Codes\qiulin\messageloop

当前分支应为 `v2`（已含 A0–C6、D1、D2；D2 tip 为 `83c7faa`）。

## 任务

独立实现 **PR-KA-D3**（观测面补齐：六个合同指标）。唯一规格书（必须先通读再动手）：

`docs/v2/tasks/pr-ka-d3-observability.md`

背景（只读）：`docs/v2/kernel-architecture.md` 观测节（:578-584）与 LiveBus 缓冲满合同（:409）。规格书与设计冲突时**以规格书为准**。

先读这些现码再动手：

- `metrics.go` / `metrics_test.go`（Metrics 结构体与注册/测试模式）
- `cluster_state.go:249-300`（`syncClusterSessionState` 四个 ErrSessionFenced 点的埋点分配，规格书 §3.1）
- `cluster_resume.go:40-160`（takeover CAS、`requestSessionTakeover`、KD-K30 旁路）
- `node.go:1360-1370`（迟到 occupancy 计数点）
- `pkg/redisbroker/pubsub.go`（消费循环 publication 分支、`lastSeqs` 推进 :683-684、catch-up 基线 :507-560）
- `pkg/redisbroker/cluster_command_bus.go:115,158,1033`（`SetMetrics`/`getMetrics` nil 容忍范式）
- `cmd/server/main.go`（broker 与 Node metrics 的接线点）
- `docs/developer/05-observability.md`（现有指标表风格）

## 目标（一句话）

落地架构观测节剩余的六个合同指标（bind_fenced_total / bind_refresh_fail_total / evict_lag / session_dual_activation_seconds / occupancy_gen_discard_total / live_drop_total），纯仪表零行为改动。

## 硬约束

1. 只许改规格书 §2 路径；fencing/恢复/投递的判定逻辑零改动（diff 只多 metrics 调用与计时变量）。
2. **不做** D4 的三项：occupancy 丢弃计数、缓冲满优先丢 occupancy、频道降级标记。
3. 指标名与架构观测节逐字一致（`messageloop_` 前缀由注册加）；均无 label。
4. 迟到 occupancy 的 `PresenceFailures{op="late"}` 全仓只此一处，替换为 `occupancy_gen_discard_total` 后全仓零 `"late"` 残留；`PresenceFailures` vec 保留。
5. 测试禁止固定长 Sleep（用 Eventually/轮询）；测试串行，绝不并发两个根目录 `go test`；Redis 测试用真实 Redis（127.0.0.1:6379，DB 14，沿用既有 guard）。
6. 不做 git commit / tag / push；不产生格式 churn。

## 验证

按规格书 §5 逐条执行并贴真实输出（含 `go test -count=1 ./...` 全量与各 grep 门禁）。

## 完成报告

- 改动文件列表
- §6 每条 过/失败 + 证据
- 测试命令与结果（真实输出）
- 偏离（应无）

另外：实现完成后，把实现备注填入规格书 `docs/v2/tasks/pr-ka-d3-observability.md` §8。
````
