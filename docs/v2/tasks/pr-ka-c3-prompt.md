# PR-KA-C3 第三方实现 Prompt（整段复制即可）

把下面「PROMPT」围栏里的全部内容发给实现 agent。不要摘要。做完后把完成报告交回主 agent 做严格验收。

---

````
你是 MessageLoop（Go 实时消息平台）的实现工程师。项目根目录：

D:\Codes\qiulin\messageloop

当前分支应为 `v2`（已含 A0–A4、B1–B4、C1–C2；C2 tip 为 `698040a`）。

## 任务

独立实现 **PR-KA-C3**。唯一规格书（必须先通读再改代码）：

`docs/v2/tasks/pr-ka-c3-stream.md`

背景（只读）：`docs/v2/kernel-architecture.md` NodeRPC、KD-K6、KD-K29、KD-K31。规格书与设计冲突时 **以规格书为准**。

先读这些现码再动手：

- `pkg/redisbroker/cluster_command_bus.go` `SendCommand`（今日 `PUBLISH req:`）、`runCommandReader`、`handleMessage`、`publishCommandResult`
- B4 HMAC：`SignCommand` 在 PUBLISH 前；`handleMessage` 先验签再 claim
- 去重键 `ml:cluster:cmd:state:` 与 reply Pub/Sub
- `pkg/redisbroker/cluster_command_bus_test.go` 现有 HMAC / 往返测试
- C1 `internal/cluster/sim`（不要改成 Stream）

## 目标（一句话）

请求改走 Redis Stream + 每 incarnation 一个 `inbox` consumer group；应答仍 Pub/Sub；HMAC 与去重保持。

## 硬约束

1. 只许改规格书 §2 路径。
2. **不要**再 `PUBLISH`/`SUBSCRIBE` `ml:cluster:cmd:req:`。应答 `reply_channel` 仍是 Pub/Sub。
3. **不要**改 HMAC 规范字节、密钥配置、CAS、C2 epoch、C1 场景。
4. HMAC 拒绝也必须 `XACK`，且不写受害者 state 键。
5. 没有「旧节点仍收 Pub/Sub 请求」窗口。不要同时订 req 通道和 Stream。
6. Stream 键不要放在 `ml:cluster:node:` 下。
7. 不做 git commit / tag / push。
8. 测试禁止用固定长 Sleep。不要同时跑两个根目录 `go test ./...`。

## 验证

```bash
go test -count=1 ./pkg/redisbroker
go test -count=1 -run "TestSim_|TestClusterCommandBus" .
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
