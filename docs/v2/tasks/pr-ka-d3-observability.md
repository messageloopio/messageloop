# PR-KA-D3 实现规格：观测面补齐（六个合同指标）

| 字段 | 值 |
| --- | --- |
| 标题 | `metrics: land contract observability (bind fencing, evict lag, dual activation, live drop, occupancy discard)` |
| 状态 | **Accepted**（2026-08-18 主 agent 终验通过，尚未 commit） |
| 依赖 | D2 已合（`83c7faa`）。在 `v2` 分支上做 |
| 设计来源 | [kernel-architecture.md](../kernel-architecture.md) 观测节（:578-584）与 LiveBus 缓冲满合同（:409）；转正评审架构覆盖路 |
| 验收人 | 主 agent |

## 1. 目标

架构观测节列出而代码尚未落地的合同指标，本 PR 一次补齐。**纯仪表，零行为改动**（不改任何判定/投递/fencing 逻辑，只在既有决策点计数）：

| 指标（Prometheus 名，`messageloop_` 前缀自动加） | 类型 | 埋点（现状行号见 §3) |
| --- | --- | --- |
| `bind_fenced_total` | Counter | CAS-nil 抢权失败（`cluster_state.go` 首登）+ takeover claim 失败（`cluster_resume.go`) |
| `bind_refresh_fail_total` | Counter | 同 fence 续约路径三处 ErrSessionFenced(`cluster_state.go`) |
| `evict_lag` | Histogram（秒） | `requestSessionTakeover` 往返时延（`cluster_resume.go`)，仅真实发往远端旧主时 |
| `session_dual_activation_seconds` | Histogram（秒） | takeover 重叠窗口：CAS 赢 → takeover 收尾（含 KD-K30 死节点旁路） |
| `occupancy_gen_discard_total` | Counter | `node.go:1366` 的迟到 occupancy 丢弃，**替换** `PresenceFailures{op="late"}`（全仓唯一 op="late" 点） |
| `live_drop_total` | Counter | `pkg/redisbroker/pubsub.go` live publication 稠密 seq 跳变检测（复用 C4 `lastSeqs`) |

`live_drop_total` 语义：live 消息 `Seq > 0` 且该频道 `lastSeqs[ch] > 0` 且 `Seq > last+1` 时 `Add(Seq-last-1)`;`lastSeqs` 推进逻辑不变。go-redis 缓冲（1024）满时静默丢的 publication 由此被计数——架构「禁止静默丢而不计数」对 publication 生效。

**明确不做（留 D4，需投递路径改造，单独规格）:** occupancy 事件的丢弃计数（occupancy 无 seq，本 PR 检测不了）；缓冲满时优先丢 occupancy 的策略；频道降级标记。也不做 takeover trace 的结构化追踪（Bind→Evict→Hydrate→Replay 的日志/trace 面）、`cluster_command_bus.go:933` 应答 Publish 失败的 Warn 日志（小项，另行）。

## 2. 允许改动的文件

- `metrics.go`（6 个字段 + `NewMetrics` 注册）、`metrics_test.go`
- `cluster_state.go`、`cluster_resume.go`：仅加 `n.metrics.*` 调用（含必要的计时变量），判定逻辑零改动
- `node.go`：仅 :1366 一处替换
- `pkg/redisbroker/pubsub.go`:live 跳变检测；`pkg/redisbroker/redis.go`（或定义 broker 的文件）:`SetMetrics(*messageloop.Metrics)` 方法，照 `cluster_command_bus.go:158` `SetMetrics`/`getMetrics` nil 容忍范式
- `cmd/server/main.go`：把 Node 的 metrics 接线到 redis broker（仅一行级）
- 相关测试文件（`cluster_state_test.go`、`cluster_resume_test.go`、`cluster_remote_test.go`、`pkg/redisbroker/pubsub_test.go` 等，按需）
- `docs/developer/05-observability.md`：补 6 个指标（该文档同时漏收 `cluster_command_hmac_reject_total` / `recovery_gap_total` / `live_gap_notice_total`——一并补齐这四个已存在指标的条目）
- `docs/v2/tasks/pr-ka-d3-observability.md`(§8 实现备注）

禁止：改 fencing/恢复/投递语义；改 proto、SDK、`hub.go`、`session.go`、`internal/cluster/*`;D4 的三项投递改造；`docs/v2/README.md` 与增量表（主 agent 负责）;git commit / tag / push。

## 3. 现状（动手前再读）

### 3.1 fencing 埋点

`cluster_state.go` `syncClusterSessionState`(:249 起）:
- :267-273 首登 CAS(nil→desired),`!ok` → `ErrSessionFenced` → **`bind_fenced_total`**
- :277-280 directory  fencing 与本地不符 → `ErrSessionFenced` → **`bind_refresh_fail_total`**
- :289-291 directory version 更新 → `ErrSessionFenced` → **`bind_refresh_fail_total`**
- :293-298 同 fence 续约 CAS,`!ok` → `ErrSessionFenced` → **`bind_refresh_fail_total`**

`cluster_resume.go` `resumeRemoteSession`(:40 起）::71-77 takeover CAS claim,`!claimed` → `DisconnectStale` → **`bind_fenced_total`**。

### 3.2 takeover 重叠窗口

`resumeRemoteSession`::71 CAS 赢 → :80 `requestSessionTakeover`(:134-157,SendCommand 往返，成功/SESSION_NOT_FOUND 视为完成）→ KD-K30 死节点旁路（:81-101,GetNodeLease 判死后继续）。`evict_lag` = `requestSessionTakeover` 单次调用耗时；`session_dual_activation_seconds` = 从 :71 之前起算到 takeover 分支收尾（含旁路）的整段。无远端旧主（`lease.NodeID` 空/同节点）不 Observe 两者。

### 3.3 迟到 occupancy

`node.go:1366`:`n.metrics.PresenceFailures.WithLabelValues("late").Inc()` —— 全仓唯一 `op="late"` 点（已 grep 确认）。替换为 `n.metrics.OccupancyGenDiscards.Inc()`;`metrics_test.go:73-80` 的对应断言同步改。`PresenceFailures` vec 本身保留（其他 op 在用）。

### 3.4 live drop 检测点

`pkg/redisbroker/pubsub.go` 消费循环 `case messageTypePublication`(:455 附近）→ `deliverOnce`。`b.lastSeqs` 已存在并在 :683-684 对 `Seq > 0` 推进（C4);catch-up 回放也写同一 map(:535-538),live 检测与之同临界区、无新锁。检测放 live publication 分支、推进之前。legacy 条目（Seq==0）不检测（C4 break-chain 同款语义）。重连后 catch-up 重置基线，天然不错报。

### 3.5 broker 接 metrics 的范式

`cluster_command_bus.go:115,158,1033`:`SetMetrics(*messageloop.Metrics)` + nil 容忍 `getMetrics()`。redisBroker 照此加一个；`cmd/server/main.go` 在 broker 构造后接线（参照 :197 命令总线接线与 Node metrics 的来源）。

### 3.6 观测文档

`docs/developer/05-observability.md` 目前未收：`bind_fenced_total`、`bind_refresh_fail_total`、`evict_lag`、`session_dual_activation_seconds`、`occupancy_gen_discard_total`、`live_drop_total`（本 PR 新增），以及已存在的 `cluster_command_hmac_reject_total`、`recovery_gap_total`、`live_gap_notice_total`（B4/C2/C6 遗留漏收）。一并补齐，口径与 `metrics.go` 注册名一致（`messageloop_` 前缀）。

## 4. 测试

| 测试 | 内容 |
| --- | --- |
| metrics 注册 | `metrics_test.go` 既有模式扩 6 个（注册不 panic、可 Inc/Observe) |
| fencing 计数 | `cluster_state_test.go` 等：构造三种 refresh-fenced 场景与 CAS 抢权失败场景，断言对应计数器 +1（用 `testutil.ToFloat64`) |
| takeover 计时 | takeover 成功后两个 histogram 各有 1 次观察（`testutil.CollectAndCount` 或读 SampleCount)；无远端旧主时为 0 |
| live drop | `pubsub_test.go`（真实 Redis)：正常连发无误报；构造 seq 跳变（直接 XADD 跳过若干 seq 再触发投递）断言 `live_drop_total` 增量 == 跳过条数；legacy Seq==0 不计 |
| 迟到 occupancy | 原 `op="late"` 断言改断 `occupancy_gen_discard_total` |

测试禁止固定长 Sleep；异步断言用 Eventually/轮询。

## 5. 验证

```bash
go build ./...
go test -count=1 ./pkg/redisbroker
go test -count=1 -run "TestSim_|TestClusterCommandBus|TestMetrics|TestSyncClusterSessionState|TestResume" .
go test -count=1 ./...          # 串行；真实 Redis(127.0.0.1:6379)
grep -n "bind_fenced_total\|bind_refresh_fail_total\|evict_lag\|session_dual_activation_seconds\|occupancy_gen_discard_total\|live_drop_total" metrics.go docs/developer/05-observability.md
grep -rn '"late"' --include="*.go" . | grep -v genproto   # 零命中
grep -n "cluster_command_hmac_reject_total\|recovery_gap_total\|live_gap_notice_total" docs/developer/05-observability.md
```

## 6. 验收清单

1. 六个合同指标按 §1 表格的语义与埋点落地，命名与架构观测节逐字一致；`PresenceFailures{op="late"}` 已被替换且全仓零 `"late"` 残留。
2. 纯仪表：fencing/恢复/投递判定逻辑零行为改动（diff 里只多 metrics 调用与计时变量）。
3. live drop 只在 live publication 且 Seq 跳变时计数，数值 == 跳过条数；legacy/重连后无误报（有测试）。
4. broker `SetMetrics` nil 容忍；未接线（memory broker/单节点）路径不 panic。
5. 05-observability.md 补齐本 PR 6 个 + 历史漏收 3 个指标，名字与注册一致。
6. §5 命令全绿；无格式 churn；未碰 §2 禁止项；无 git 操作。

## 7. 完成报告

- 改动文件列表
- §6 每条 过/失败 + 证据
- 测试命令与真实输出
- 偏离（应无）

## 8. 实现备注（实现方填）

实现已完成（v2 分支，基于 `2c5ea2d`）。要点：

- **metrics.go**：`Metrics` 增加 `BindFencedTotal` / `BindRefreshFailTotal` / `EvictLag` / `SessionDualActivationSeconds` / `OccupancyGenDiscards` / `LiveDropTotal` 六字段并注册；指标名与架构观测节逐字一致（`messageloop_` 前缀由 `Namespace` 加），均无 label；两个 histogram 用 `DefBuckets`（时长刻度）。
- **cluster_state.go**：`syncClusterSessionState` 四个 ErrSessionFenced 点按 §3.1 分配埋点——首登 CAS(nil) `!ok` → `BindFencedTotal`；fencing 不符 / directory version 更新 / 续约 CAS `!ok` 三处 → `BindRefreshFailTotal`。判定逻辑零改动。
- **cluster_resume.go**：takeover CAS claim `!claimed` → `BindFencedTotal`；`dualActivationStart` 取在 CAS 之前（规格 §3.2「从 :71 之前起算」），`evictStart` 紧贴 `requestSessionTakeover` 调用。`EvictLag` 在调用返回即 Observe（成功/失败都计，它是往返时延）；`SessionDualActivationSeconds` 只在 takeover 分支收尾 Observe（成功路径与 KD-K30 死节点旁路；回滚提前 return 的两条失败路径不 Observe，窗口未形成有效接管）。原 `if err := ...; err != nil` 改为 `takeoverErr := ...; if takeoverErr != nil`，返回值语义不变。
- **node.go**：`onOccupancy` 的 `PresenceFailures{op="late"}` 替换为 `OccupancyGenDiscards.Inc()`；`PresenceFailures` vec 保留。全仓 `"late"` 零残留（grep 门禁验证）。
- **pkg/redisbroker**：`redis.go` 加 `metricsMu`+`metrics` 字段与 `SetMetrics`/`getMetrics`（nil 容忍，照命令总线范式）；`pubsub.go` 消费循环 live publication 分支在 `deliverOnce` 推进 `lastSeqs` 之前调 `noteLiveSeqGap`，读基线用 `deliverMu`+`subMu.RLock`（与 `deliverOnce` 同锁序、无新锁）。语义：`Seq>0 && last>0 && Seq>last+1` 时 `Add(Seq-last-1)`；legacy（Seq==0）与无基线（重连 catch-up 重置后首条）不计。
- **cmd/server/main.go**：`node.SetBroker(broker)` 后经接口断言 `SetMetrics(*messageloop.Metrics)` 接线；memory broker 不实现该接口，自然不接线、不 panic。
- **测试**：`metrics_test.go` 删 `op="late"` 断言并加 `TestMetrics_ContractObservabilityRegistered`（注册名 + Inc/Observe + histogram SampleCount，用 `dto.Metric`）；`cluster_state_test.go` 加首登抢权失败 + 三种 refresh-fenced 子用例；`cluster_resume_test.go` 加 claim fenced、takeover 成功双 histogram 各 1 次、无远端旧主 0 次三个用例；`occupancy_test.go` 加迟到事件计数断言；`pubsub_test.go` 加无 Redis 的 `noteLiveSeqGap` 语义单测（含 nil metrics 不 panic）与真实 Redis 的 `TestRedisBroker_LiveDrop_SeqGapCounted`（连发无误报 → SET seq 计数器跳到 10 后发布 seq 11 断言 +7 → PublishTransient Seq==0 不计）。异步断言全部用 `require.Eventually`，无固定长 Sleep。
- **docs/developer/05-observability.md**：§3.2 收 `live_drop_total`；§3.4 收 `cluster_command_hmac_reject_total`、`bind_fenced_total`、`bind_refresh_fail_total`、`evict_lag`、`session_dual_activation_seconds`；§3.5 收 `recovery_gap_total`、`live_gap_notice_total`、`occupancy_gen_discard_total`；§3 开头的语义标签清单同步补 `reason` 系列。

验证：§5 全部命令实跑通过（`go build ./...`；`go test -count=1 ./pkg/redisbroker`；根目录定向测试；`go test -count=1 ./...` 全量；三条 grep 门禁）。真实 Redis 为本机 Docker 容器 `messageloop-test-redis`（127.0.0.1:6379，测试沿用 `requireCommandBusRedis`，DB 14）。无 git 操作，无格式 churn（改动文件保持各自原有 CRLF/LF 行尾）。
