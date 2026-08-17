# PR-KA-C3 实现规格：NodeRPC 请求改走 Redis Stream（KD-K6）

| 字段 | 值 |
| --- | --- |
| 标题 | `cluster: deliver NodeRPC requests on Redis Stream + consumer group` |
| 状态 | **Accepted**（2026-08-17 主 agent 终验通过，尚未 commit） |
| 依赖 | C2 已合（`698040a`）。HMAC 已在（B4）。在 `v2` 分支上做 |
| 设计来源 | [kernel-architecture.md](../kernel-architecture.md) NodeRPC、KD-K6、KD-K29、KD-K31 |
| 验收人 | 主 agent |

## 1. 目标

集群命令的 **请求** 不再走会丢的 Pub/Sub。每个 incarnation 一条 Redis Stream + 一个 consumer group：至少一次投递，幂等靠现有 `command_id` 去重。HMAC 硬门保持。

1. `SendCommand` 对目标 inbox **`XADD`**，禁止再 `PUBLISH` 到 `ml:cluster:cmd:req:`。
2. 接收循环 **`XREADGROUP`**（`>` 新消息）+ **`XAUTOCLAIM`** 认领超时 pending。禁止 `SUBSCRIBE` 请求通道。
3. 处理完（含 HMAC 拒绝）必须 **`XACK`**，避免毒消息永远重投。
4. **应答仍走现有 Pub/Sub `reply_channel`**（发送方在等；丢应答已映射 `unknown_final_state`）。本 PR 不把应答改成 Stream。
5. 没有「一边 SUBSCRIBE 一边 XREAD」的过渡窗口（KD-K31）。

**不做：** 改 HMAC 规范字节 / 密钥配置；改 A1 CAS、C2 `node_epoch` 发号、C1 sim 总线（仍是进程内函数调用）；改 Occupancy / LiveBus / History Stream；`ml2:` 键前缀换代；整仓 `internal/*` 搬家；把 broker 数据面 `XADD` 历史流改掉。

## 2. 允许改动的文件

- `pkg/redisbroker/cluster_command_bus.go` 及 `cluster_command_bus_test.go`：发送 `XADD`、接收 `XREADGROUP`/`XAUTOCLAIM`/`XACK`；删请求路径的 `SUBSCRIBE`/`PUBLISH`
- `pkg/redisbroker/options.go`：仅当要导出 stream 前缀常量（可继续用包内常量）
- `docs/developer/04-cluster.md`、`docs/deployment.md`：请求通道从 Pub/Sub 改为 Stream
- `docs/v2/tasks/pr-ka-c3-stream.md`（完成备注）
- 因构造 / 读循环签名微调而必须改的测试装配（`cluster_redis_integration_test.go` 等）

禁止：改 `internal/cluster/hmac`、`cluster_epoch.go`、`internal/cluster/sim` 宪法语义、`client.go` / `hub.go` / `session.go` / `recover.go`、git commit/push。

## 3. 现状（动手前再读）

- `SendCommand`：填 ID / 去重 / `targetAlive` → `SUBSCRIBE reply` → `SignCommand` → **`PUBLISH req:{node}:{inc}`** → `waitForReply`。
- `Start`：`SUBSCRIBE` 自己的 `req:` 通道，`runCommandReader` 收 JSON，`handleMessage` 先验签再 claim。
- 去重：`ml:cluster:cmd:state:{commandID}`，终态 TTL 10 分钟。handler timeout 10s；claim 租约 30s。
- HMAC：未签名 / 坏签 / 偏斜 / 空 id 不 claim、不写 state。
- 应答：`publishCommandResult` `PUBLISH reply_channel`，带 `SignResult`。
- 内存 / C1 `sim.Bus`：函数调用，无 Redis。

Pub/Sub 丢 Evict 是 C1「Lost Evict」能脚本化的原因；生产 Redis 总线上要至少一次，靠 Stream + 去重，而不是再丢。

## 4. 键与消费者

| 项 | 值 |
| --- | --- |
| 请求 Stream | `ml:cluster:cmd:stream:{nodeID}:{incarnationID}` |
| Consumer group | 固定名 `inbox`（每条 stream 一个 group） |
| Consumer name | 本进程 `incarnationID`（或 `inbox`；同一 incarnation 只许一个活消费者） |
| 条目字段 | 单一 field `payload` = 今日命令 JSON（含 `Signature`） |
| 应答 | **不变**：`ml:cluster:cmd:reply:{uuid}` Pub/Sub |
| 去重键 | **不变**：`ml:cluster:cmd:state:{commandID}` |

- **不要** 使用 `ml:cluster:node:` 前缀（C2 SCAN 会误伤）。
- `XADD` 用近似 `MAXLEN`（建议 `~ 10000`，可调变量），避免 inbox 无限涨。
- `Start`：`XGROUP CREATE ... MKSTREAM`；group 已存在则忽略 `BUSYGROUP`。
- 读：`XREADGROUP GROUP inbox {consumer} COUNT n BLOCK ms STREAMS {key} >`
- 崩溃重投：周期 `XAUTOCLAIM`（idle ≥ `clusterCommandClaimLeaseTTL`，默认 30s）把 pending 拉回本消费者，再走同一 `handleMessage`。去重保证副作用至多一次成功执行。
- `XACK`：在 `handleMessage` **返回之后**（含 HMAC 拒绝、无 handler、成功、失败）。未 ACK 的才进 pending。

## 5. 算法

### 5.1 发送

```
SendCommand:  // 去重 / targetAlive / 签 与今日相同
    SignCommand
    XADD stream:{targetNode}:{targetInc}  payload=json(cmd)  MAXLEN ~
    waitForReply   // 仍 SUBSCRIBE reply_channel
```

禁止：`PUBLISH` 到 `cmd:req:`；先 XADD 再补签。

### 5.2 接收

```
Start:
    XGROUP CREATE MKSTREAM inbox
    loop: XREADGROUP >
          并行/有界地 handleMessage + XACK
          偶尔 XAUTOCLAIM idle pending
handleMessage:  // 与 B4 相同
    unmarshal → VerifyCommand → 失败则 return（仍 XACK）
    claim / handler / SignResult / PUBLISH reply
```

HMAC 失败 **仍 XACK**。否则毒消息会每 30s 重投打爆指标。仍 **不** 把失败写成受害者的 state 键。

读循环重连 / 有界并发（128）/ handler 10s timeout：语义对齐今日，实现可从 pubsub reader 迁过来。

### 5.3 删除旧请求 Pub/Sub

- 生产代码不再 `SUBSCRIBE`/`PUBLISH` `ml:cluster:cmd:req:*`。
- 常量 `clusterCommandRequestPrefix` 可删，或仅留测试负断言。
- 包注释与 `04-cluster.md` / `deployment.md` 改为：请求 = Stream + group；应答 = Pub/Sub；HMAC 仍是硬门。

## 6. 接入

- `NewClusterCommandBus(...)` 签名保持（cfg, nodeID, inc, hmacKey）。
- `cmd/server` / C2 接线不用改，除非构造函数增加可选 stream 调参（默认即可）。
- C1 `internal/cluster/sim` **不**实现 Stream。
- 无 Redis 的 HMAC 包测试仍绿。

## 7. 必须存在的测试

1. **往返**：现有 signed round-trip / HMAC 拒绝用例（unsigned、坏签、skew、空 id、伪造应答）在 Stream 接收路径上仍绿。handler 计数与 state 键断言不变。
2. **请求不是 PUBLISH**：spy 或源码级：`SendCommand` 成功路径调用 `XAdd`，不 `Publish` 到 `cmd:req:`。
3. **ACK**：一条命令处理完后，该 stream 上该 group 的 pending 不含此 ID（`XPENDING` / `XACK` 后为空）。
4. **HMAC 拒绝也 ACK**：注入未签名 JSON 到 stream（`XADD`）→ handler 零次、无 state 键、条目被 ACK（随后 `XAUTOCLAIM` 不应再投给 handler）。
5. **至少一次**：`XADD` 后不 ACK，模拟崩溃；`XAUTOCLAIM`（可把 idle 调到测试可测的短值）再投一次；去重使 handler 业务至多成功一次（第二次走 in_progress / 已终态）。
6. **无请求 SUBSCRIBE**：bus `Start` 后，进程不为 `cmd:req:` 建 Pub/Sub 订阅（读源或断言 `pubsub` 字段只服务 reply）。
7. C1 `go test -run 'TestSim_' .` 仍绿。
8. `go test ./pkg/redisbroker`；`go test ./...`；`go test -race . ./pkg/redisbroker`。

禁止用固定长 `Sleep` 代替 Eventually / 缩短后的 claim TTL。无 Redis 则 Skip 并写明。

## 8. 验收清单

1. 请求只走 Stream + consumer group；生产无 `cmd:req:` PUBLISH/SUBSCRIBE。
2. HMAC 硬门仍在；拒绝后 XACK，不写受害者 state。
3. 应答仍是带签名的 Pub/Sub。
4. 崩溃 pending 可被 XAUTOCLAIM 再投；去重防双执行。
5. 未改 HMAC 字节、CAS、C2 epoch、C1 场景。
6. 测试命令绿。

## 9. 完成报告

- 文件列表
- §8 逐条证据
- 测试命令与结果
- 偏离（应无）

## 10. 实现备注（完成后填写）

实现于 `v2` 分支（基线 `7337c77`）。

- 请求路径：`SendCommand` 改为 `XADD ml:cluster:cmd:stream:{node}:{inc}`（单 field `payload`，近似 `MAXLEN ~ 10000`，可调变量 `clusterCommandStreamMaxLen`）；删除 `clusterCommandRequestPrefix` / `requestChannel` 与请求侧 `PUBLISH`/`SUBSCRIBE`。签名仍在入流之前。
- 接收路径：`Start` 先 `XGROUP CREATE MKSTREAM inbox`（起点 `0`，BUSYGROUP 忽略）；读循环 `XREADGROUP ... >`（`COUNT 32`，`BLOCK 2s`，可调）+ 每轮空闲时 `XAUTOCLAIM`（`MinIdle = clusterCommandClaimLeaseTTL`）认领崩溃 pending；`dispatchStreamMessage` 在 `handleMessage` 返回后一律 `XACK`（含 HMAC 拒绝、无 handler、payload 被截断的墓碑条目）。重试循环在重建组后恢复消费，`disconnects` 计数保留。128 并发信号量、handler 10s deadline、claim 租约/续租语义不变。
- 应答路径完全未动：仍是带 `SignResult` 签名的 Pub/Sub `reply_channel`；去重键 `ml:cluster:cmd:state:` 未动。
- 构造函数 `NewClusterCommandBus(cfg, nodeID, inc, hmacKey)` 签名不变；`cmd/server`、C1 sim、HMAC 包均未改。
- 测试：`publishRawCommand` 改为 `addRawCommand`（XADD 注入）；原 Pub/Sub 重连测试改写为 `TestClusterCommandBus_RecoversAfterConsumerGroupLoss`（`XGroupDestroy` 模拟组丢失）；新增 `SendCommandUsesStreamNotPublish`（XADD 正断言 + 无 `cmd:req:` 发布 + 应答仍 Pub/Sub）、`AcksProcessedCommands`、`HMACRejectStillAcks`、`RedeliversPendingAfterCrash`（缩短 claim TTL 作 XAUTOCLAIM min-idle，无固定长 Sleep）、`NoRequestPubSubSubscription`。密钥卫生测试扩到 stream 条目。拒绝类指标断言改为 `Eventually`（Stream 派发为异步 goroutine）。
- 验证（真实 Redis，DB 14）：`go test -count=1 ./pkg/redisbroker`、`go test -count=1 -run "TestSim_|TestClusterCommandBus" .`、`go test ./...`、`go test -race . ./pkg/redisbroker` 全部通过。
- 偏离：无。
