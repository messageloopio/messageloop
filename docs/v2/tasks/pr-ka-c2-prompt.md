# PR-KA-C2 第三方实现 Prompt（整段复制即可）

把下面「PROMPT」围栏里的全部内容发给实现 agent。不要摘要。做完后把完成报告交回主 agent 做严格验收。

---

````
你是 MessageLoop（Go 实时消息平台）的实现工程师。项目根目录：

D:\Codes\qiulin\messageloop

当前分支应为 `v2`（已含 A0–A4、B1–B4、C1；C1 tip 为 `0adfdf3`）。

## 任务

独立实现 **PR-KA-C2**。唯一规格书（必须先通读再改代码）：

`docs/v2/tasks/pr-ka-c2-epoch.md`

背景（只读）：`docs/v2/kernel-architecture.md` 三把时钟、KD-K27。规格书与设计冲突时 **以规格书为准**。

先读这些现码再动手：

- `cluster.go` `ClusterOptions.normalize()`（空 ID → `uuid.NewString()`）
- `cmd/server/main.go` `normalizeClusterOptions` / `setupCluster`（又生成一次 UUID，再拿去建 bus）
- `pkg/redisbroker/cluster_directory.go` 节点租约键 `ml:cluster:node:{node}:{inc}` 与 SCAN 前缀
- `pkg/redisbroker/options.go` `ClusterNodePrefix`
- C1 测试传入的 `IncarnationID: "inc-a"`（必须继续合法）

## 目标（一句话）

生产 incarnation = 单调 `node_epoch`（Redis INCR / memory 计数器），禁止 UUID；测试里显式传入的 ID 不动。

## 硬约束

1. 只许改规格书 §2 路径。
2. **不要**改 CAS 四字段谓词、HMAC 规范字节、命令总线传输（仍是 Pub/Sub）。
3. **不要**把 epoch 键写成 `ml:cluster:node:...` 子键，以免被 `ListNodeLeases` SCAN 吃掉。用 `ml:cluster:node_epoch:{nodeID}`。
4. 不要改 C1 宪法场景语义。不要改 OccupancyGen / StreamEpoch。不要 `ml2:` 换代。
5. 空 ID + redis 且不能 INCR → 启动失败，禁止回落 UUID。
6. 不做 git commit / tag / push。
7. 测试禁止用固定长 Sleep。不要同时跑两个根目录 `go test ./...`。

## 验证

```bash
go test -count=1 ./pkg/redisbroker
go test -count=1 -run "TestSim_|TestNodeEpoch|TestCluster.*Epoch|TestAllocate" .
go test ./...
go test -race . ./pkg/redisbroker
```

对照规格书 §7 测试表与 §8 清单自检。

## 完成报告

- 改动文件列表
- §8 每条 过/失败 + 证据
- 测试命令与结果
- 偏离（应无）
````
