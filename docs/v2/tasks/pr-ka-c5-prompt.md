# PR-KA-C5 第三方实现 Prompt（整段复制即可）

把下面「PROMPT」围栏里的全部内容发给实现 agent。不要摘要。做完后把完成报告交回主 agent 做严格验收。

---

````
你是 MessageLoop（Go 实时消息平台）的实现工程师。项目根目录：

D:\Codes\qiulin\messageloop

当前分支应为 `v2`（已含 A0–A4、B1–B4、C1–C4；C4 tip 为 `d99cd86`）。

## 任务

独立实现 **PR-KA-C5**。唯一规格书（必须先通读再改代码）：

`docs/v2/tasks/pr-ka-c5-keyprefix.md`

背景（只读）：`docs/v2/kernel-architecture.md` 舰队 / KD-K31。规格书与设计冲突时 **以规格书为准**。

先读这些现码再动手：

- `pkg/redisbroker/options.go`（9 个前缀默认值常量与 `NewOptions`）
- `pkg/redisbroker/cluster_command_bus.go:42-44`（3 个包级前缀常量，**不**派生自 `ClusterPrefix`）
- `pkg/redisbroker/cluster_command_bus_test.go` 的 4 处硬编码 `ml:cluster:cmd:*` 字面量（规格书 §3.2 已列出）
- `pkg/redisbroker/cluster_directory.go`（SCAN pattern 拼接点、`node_epoch:` 避撞结构）

## 目标（一句话）

全部 Redis 键前缀从 `ml:` 换代为 `ml2:`（KD-K31 独立版本），键结构/语义零变化，不加配置项。

## 硬约束

1. 只许改规格书 §2 路径。
2. 只改前缀**值**；不改任何键的结构、数据类型、TTL、语义，不改 SCAN 的拼接方式。
3. **不**给 `config.RedisConfig` 加前缀配置字段；**不**把命令总线前缀重构为派生自 `ClusterPrefix`。
4. `node_epoch:` 不得落进 `node:` SCAN 范围（避撞结构保持），`TestRedisSessionDirectory_NodeEpochKeyEscapesNodeLeaseScan` 必须仍绿。
5. 生产代码（含注释中的键形）不得再出现 `ml:` 字面量；`docs/design`、`docs/review`、`docs/archive`、`docs/v2/tasks/` 既有规格等历史文档保持原样。
6. proto / SDK / memory broker / C1 sim / HMAC / `client.go` / `hub.go` / `session.go` 零改动。
7. 不做 git commit / tag / push。
8. 测试禁止用固定长 Sleep。多个 `go test` 命令串行执行，不要同时跑两个根目录 `go test ./...`。

## 验证

```bash
grep -rn '"ml:' --include='*.go' .   # 生产+测试代码必须零命中
go test -count=1 ./pkg/redisbroker
go test -count=1 -run "TestSim_|TestClusterCommandBus" .
go test ./...
go test -race . ./pkg/redisbroker
```

Redis 集成测试需要真实 Redis（127.0.0.1:6379，测试用 DB 14；沿用仓库既有 `requireCommandBusRedis` 同款机制）。若环境无 Redis，标 skip 并在报告中写明。

对照规格书 §5 测试表与 §6 清单自检（含新增的「换代隔离」测试）。

## 完成报告

- 改动文件列表
- §6 每条 过/失败 + 证据
- 测试命令与结果（真实输出）
- 偏离（应无）

另外：实现完成后，把实现备注填入规格书 `docs/v2/tasks/pr-ka-c5-keyprefix.md` §8（参照 C4 规格书 §10 的写法）。
````
