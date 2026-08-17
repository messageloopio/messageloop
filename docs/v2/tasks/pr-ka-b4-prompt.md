# PR-KA-B4 第三方实现 Prompt（整段复制即可）

把下面「PROMPT」围栏里的全部内容发给实现 agent。不要摘要。做完后把完成报告交回主 agent 做严格验收。

---

````
你是 MessageLoop（Go 实时消息平台）的实现工程师。项目根目录：

D:\Codes\qiulin\messageloop

当前分支应为 `v2`（已含 A0–A4、B1–B3；B3 tip 为 `782061b`）。

## 任务

独立实现 **PR-KA-B4**。唯一规格书（必须先通读再改代码）：

`docs/v2/tasks/pr-ka-b4-noderpc.md`

背景（只读）：`docs/v2/kernel-architecture.md` Cluster / NodeRPC、KD-K4、KD-K6、KD-K26、KD-K29、KD-K31。规格书与设计冲突时 **以规格书为准**。

先读这些现码再动手：

- `pkg/redisbroker/cluster_command_bus.go` 包注释（现写「无签名」）、`SendCommand`、`handleMessage`、`publishCommandResult`
- `cluster.go` `SessionDirectory.PutSessionLease`、`ClusterCommand`、`ClusterDependencies`
- `cluster_projection_repair.go` / `cluster_user_index_repair.go`（两条独立 ticker）
- `pkg/redisbroker/cluster_directory.go` `PutSessionLease` / `CompareAndSwapSessionLease`
- `config/config.go` `ClusterConfig` 与 `Validate` 的 cluster 段
- `cmd/server/main.go` `setupCluster`

## 目标（一句话）

现有 Redis Pub/Sub 命令总线加 HMAC 硬门并拒绝未签名；删掉 `PutSessionLease`；一个 repairer 兼 OnLeave；HMAC 放进 `internal/cluster/hmac`。

## 硬约束

1. 只许改规格书 §2 路径。
2. **不要**把命令总线改成 Redis Stream / consumer group / `XADD`。HMAC 加在现有 PUBLISH 信封上。
3. 密钥只来自 `cluster.hmac_key` 或 `cluster.hmac_key_file`，禁止写入任何 Redis 键或日志。
4. 未签名 / 坏签 / 偏斜命令不得 claim、不得跑 handler。伪造应答不得当 succeeded。
5. 从接口和生产代码删除 `PutSessionLease`。热路径继续走 A1 的 CAS，禁止引回无条件 SET。
6. 不要整仓搬 `internal/*`。不要动 `client.go` / `hub.go` / `session.go` / `recover.go` / `occupancy.go` / `authorizer.go`。只建 `internal/cluster/hmac`（或同包 hmac 文件）。
7. 不要改 proto、SDK、A1 CAS 算法、A2/A3/A4、B1/B2/B3 热路径。
8. 不做 git commit / tag / push。
9. 测试禁止用固定长 Sleep 代替注入时钟 / 导出的 `repairOnce`。
10. **不要**在仓库根同时跑两个 `go test ./...`（会争 Redis，污染 `in_progress` 状态键）。先单独 `go test ./pkg/redisbroker`，再一次 `go test ./...`。

## 验证

```bash
go test ./internal/cluster/hmac ./pkg/redisbroker
go test ./...
go test -race . ./pkg/redisbroker
```

无 Redis 时 bus 集成测试应 Skip；hmac 包与 Validate 测试必须绿。

对照规格书 §7 测试表与 §8 清单自检。

## 完成报告

- 改动文件列表
- §8 每条 过/失败 + 证据
- 测试命令与结果
- 偏离（应无）
````
