# 修复任务 05：Broker 与集群层

## 背景

你要修复的项目是 **MessageLoop**（工作目录：`D:/Codes/qiulin/messageloop`），Go 实时消息平台服务端。先读根目录 `AGENTS.md`。

刚完成一轮全项目代码评审，评审发现已经过主 agent 逐条核实（本模块全部确认）。完整方案见 `docs/review/fix-plan.md`。**先读相关代码再动手。**

## 文件归属（严格，多 agent 并行修复）

- 你拥有：`pkg/redisbroker/` 全部、根目录 `broker.go`、`broker_memory.go`、`cluster.go`、`cluster_commands.go`、`cluster_state.go`、`cluster_resume.go`、`cluster_projection_repair.go` 及对应 `_test.go`。
- 禁止修改：根包 `client.go`/`hub.go`/`node.go` 等（服务端核心 agent）、`proxy/`、`pkg/websocket/`、`pkg/grpcstream/`、`pkg/topics/`、`config/`、`cmd/`、`sdks/`、`docs/`。
- 交接项：服务端核心 agent 修复 ephemeral presence 时只改了 `client.go`，**`cluster_resume.go:143` 恢复订阅路径的 presence 登记由你负责**：恢复 ephemeral 订阅时跳过 `presence.Add` 与 join/leave 事件发布（语义见 `docs/protocol.md:166`：ephemeral 订阅不登记在线状态；参照服务端核心 agent 在 `client.go:1050-1056` 的改法，如你先完成可自行读 client.go 对应代码对齐语义）。

## 任务清单

### P1（必修）

1. **集群命令总线断线不重连**（`pkg/redisbroker/cluster_command_bus.go:129-140`）：reader goroutine 只 `for message := range pubsub.Channel()`，pubsub 断开后 Channel 关闭、循环静默退出，节点永久停止处理集群命令直至重启——无日志无指标，发送方只能等 5s 超时拿 UnknownFinalState。修复：仿照 `pubsub.go:14-34` 的 `runPubSubWithRetry` 加指数退避重连；用 stop 标志区分主动 Shutdown 与意外断线；断线打 Warn 日志并计数。补测试：模拟断线→重连→命令仍被执行。
2. **`deliverOnce` 全局锁瓶颈**（`pkg/redisbroker/pubsub.go:163-190`）：单把 `deliverMu` 串行所有频道投递去重，且 handler（含客户端网络写）在临界区内执行——一个慢消费者阻塞所有频道的实时投递与 catch-up。修复：check+record 留在锁内、handler 调用移出锁外（极端交叠下可能重复投递一次，客户端有 offset 幂等兜底），或按频道 hash 分片锁。报告中说明选择及取舍。
3. **handler 错误吞掉 + panic 无 recover**（`pkg/redisbroker/pubsub.go:167-169,187-189`）：两处 `_ = b.handler(...)`，而内存 broker 把 handler 错误返回给 Publish 调用者（`broker_memory.go:139-144`，有测试锁定）；Redis 侧 panic 会炸掉 pubsub 协程。修复：加 recover 转错误日志 + 指标；`broker.go` 的 `Broker` 接口注释明确"异步投递实现不向 Publish 传播投递错误"的契约。
4. **断线 catch-up 无提示消息缺口**（`pkg/redisbroker/pubsub.go:107-156`）：`XRangeN` 上限 = StreamMaxLength（Approx 修剪下流内可略超上限，尾部最新消息被截）；catch-up 期间新消息进 go-redis 默认 100 条缓冲，满则静默丢弃；客户端无 gap 感知。修复：catch-up 后校验最新流 ID 与 lastOffsets 是否断层，有则向客户端发显式 gap 提示；并增大缓冲或 catch-up 期间同时消费 channel。
5. **`waitForReply` 截止时刻竞争**（`pkg/redisbroker/cluster_command_bus.go:232-257`）：`ctx.Done()` 与 reply 通道关闭（go-redis 在 deadline 时关连接）同时就绪时随机选中后者，返回硬错误而非 UnknownFinalState——实测偶发测试失败。修复：`ctx.Done()` 就绪时优先走 `resolveTimedOutCommand`（如先非阻塞检查 `ctx.Err()`）。
6. **takeover 驱逐误删窗口**（`cluster_resume.go:275-277`）：`evictSessionForTakeover` 用无条件 `n.hub.RemoveSession(sessionID)`，LookupSession 与 Remove 之间新连接完成 ReplaceSession 时会误删新会话。改用 `n.hub.RemoveSessionIfMatches(sessionID, client)`（该函数就是为防此场景设计的，`hub.go:496-500`）。

### P2（顺手修）

7. `runPubSub`/`catchUpMissed` 重复反序列化代码块（`pubsub.go:78-97,130-153`）：抽 `messageToPublication(channelName string, redisMsg *redisMessage, offset uint64)`。
8. `lastOffsets` 退订不清理（`pubsub.go:184`，`redis.go:152-175` 的 Unsubscribe 无删除点）：引用计数归零时删除对应条目。
9. `executeHandlerBounded` 超时 handler 继续占信号量槽（`cluster_command_bus.go:603-619`）：已知设计权衡——在函数/配置文档注明"handler 必须响应 ctx 取消，否则占用并发槽"。
10. `clusterSessionSnapshot.ChannelOffsets/BrokerEpoch` 从未填充（`cluster_state.go:286-325` vs 67-68）：填充每频道最后投递 offset 与 epoch，实现跨节点精确续读；若工作量过大，至少在结构体注释明确为未完成功能并在报告中说明。
11. presence `Remove` 与并发 `Add` 竞态（`presence_redis.go:57-72`）：`SCard==0 → DEL index` 与 Add 的 SADD 之间非原子，可产生"在线但不可见"幽灵窗口。用 Lua 脚本原子化。
12. `SendCommand` 不预检目标租约（`cluster_command_bus.go:222-224`）：发送前 `GetNodeLease` 快速失败，避免目标已死白等 5s。

## 测试要求

- 修复前跑 `go build ./... && go test . ./pkg/redisbroker/...` 确认基线（本机有 Redis 则集成测试会真实执行）。
- 回归测试：任务 1（断线重连）、2（跨频道并发投递不互相阻塞）、5（deadline 与通道关闭同就绪必走 UnknownFinalState）、6（LookupSession 返回新 client 时不误删）、11（并发 Add+Remove 后 Get 不丢在线成员）。
- 单元测试补盲（当前零覆盖）：`deliverOnce`（同 offset 双投去重、handler panic 不炸协程）、`redisClusterQueryStore`（Lua 加减/归零 HDEL/空 hash DEL）、`clusterNodeLeaseManager`（续租失败路径）。
- 完成后 `go test -race . ./pkg/redisbroker/...` 全绿。

## 纪律

- 不做 git commit/push。最小改动。
- 完成后返回报告：每条任务处置、改动文件清单、测试结果、遗留问题。
