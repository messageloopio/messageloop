# PR-KA-C6 实现规格：catch-up 洞的 client-facing GapNotice（live gap 信封）

| 字段 | 值 |
| --- | --- |
| 标题 | `broker: client-facing GapNotice for reconnect catch-up holes` |
| 状态 | **Ready** |
| 依赖 | C5 已合（`1debf5a`）；C4 的稠密 seq 中洞检测已在。在 `v2` 分支上做 |
| 设计来源 | [kernel-architecture.md](../kernel-architecture.md) Gap 合同 / LiveBus / KD-K12、KD-K14 |
| 验收人 | 主 agent |

## 1. 目标

broker 的 Redis Pub/Sub 断线重连后，`catchUpMissed` 补投漏掉的消息；检出洞（C4 中洞 / 既有的尾截）目前只 `catchUpGaps` 计数 + Warn，**客户端无感知**——补投与实时投递同形同 lane，客户端只能自己从 offset 不连续推断。本 PR 兑现架构文档「client-facing live gap 信封」里程碑：

1. catch-up 检出洞时，向该频道的**本地订阅者**（精确 + 命中的通配）发送一个可解释的 `GapNotice` 出站信封，携带 channel、gap_reason、最后已知安全 position。
2. 检测语义不变：中洞靠稠密 seq（C4），尾截靠既有 `checkCatchUpGap`；legacy 断链规则不变（宁可漏报）。
3. 两条 SDK 消费路径都把 `GapNotice` 当一等公民（见 §6 的 TS 坑）。

**不做：** 改检测本身（C4 已落）；改 Replayer 恢复路径（`RecoverComplete` 合同不动）；通配频道的 catch-up 补投（documented limitation，照旧只通知）；跨节点扇出（各节点自己的 catch-up 各自通知本节点订阅者）；live 消息本身的顺序保证；client-facing 通知去抖/聚合策略（每频道每次 catch-up 至多一条，见 §4）。

## 2. 允许改动的文件

- `broker.go`：`CatchUpGap` 类型、`GapHandler`、`Broker.SetGapHandler`、合同注释（顺带修 `pubsub.go` 注释里「see the Broker contract in broker.go」的悬空引用）
- `pkg/redisbroker/pubsub.go`：`checkCatchUpSeqGap` / `checkCatchUpGap` 检出后调 handler；`redis.go`：handler 字段
- `broker_memory.go`：`SetGapHandler` no-op（memory 无 catch-up 概念）
- **所有因此编译失败的 `Broker` fake**（`*_test.go`）：只加 no-op 方法
- `node.go`：接线 `SetGapHandler` + `onGap` 扇出；`hub.go`：仅当现有 recipients helper 不能复用时加最小 helper
- `session.go`：**仅** `outboundFrameClass` 加 `GapNotice` 分类
- `metrics.go` / `metrics_test.go`：`live_gap_notice_total{reason}` counter
- `protocol/client/v2/service.proto`：`GapNotice` message + `OutboundMessage.oneof` 新成员（field 19）；`protocol/shared/v2/types.proto`：`GAP_REASON_REPLAY_TRUNCATED = 6`
- `shared/genproto/**`、`sdks/ts/src/proto/**`：仅 buf 再生产物
- `sdks/go`：`GapNotice` 映射 + 暴露（按现有回调惯例）；`sdks/ts`：converter + client case + 暴露（**必须**，否则未知信封触发 error 回调）
- 测试：`pkg/redisbroker/pubsub_test.go`、`node_test.go` 或新增、`recover_test.go` 如涉及、SDK 测试
- `docs/developer/01-architecture.md`、`04-cluster.md`；`docs/v2/kernel-architecture.md`：仅「Gap 合同」一节
- `docs/v2/tasks/pr-ka-c6-gap-notice.md`（完成备注）

禁止：改 `client.go` / `recover.go` / C1–C5 任何已落合同（HMAC、epoch、命令总线 Stream、稠密 seq 检测规则、键前缀）；改 catch-up 补投机制本身；git commit/push。

## 3. 现状（动手前再读）

- catch-up 链路：`runPubSub` 重建订阅后**同步**执行 `catchUpMissed`（`pubsub.go:397-413`）→ `deliverOnce` → `dispatch`（16 worker 异步）→ handler（`node.go:198-200` 注册，`hub.broadcastPublication`）。补投与实时投递**不可区分**（`replay=false`，无来源标记）。
- 检测点：`checkCatchUpSeqGap`（中洞，`pubsub.go:548-563`）与 `checkCatchUpGap`（尾截，`pubsub.go:574-593`），都只做 `catchUpGaps.Add(1)` + Warn；broker 层**不知道**本地订阅者。
- 扇出先例：Occupancy——broker `SetOccupancyHandler` 第二管道，node 侧 `deliverPresenceEvent`（`node.go:1398`）用 `hub.presenceRecipients(ch)`（`hub.go:575`）扇出**非 Publication** 信封。C6 沿用同一模式。
- 出站信封：`OutboundMessage.oneof` 现有 field 3–18 共 16 个成员（`service.proto:37-58`）；`GapReason` 枚举现有 0–5（含 C4 的 MIDDLE）。
- lane 分类：`session.go:698-717 outboundFrameClass`，Control（深 32）/ Data（深 256）双车道，新信封默认落 Data。
- **TS SDK 坑**：未知 envelope → `converters.ts:446` 返回 error → `client.ts:546` `notifyError("Unknown message type")`；Go SDK 无 default 分支，静默忽略。SDK 侧 `gap` 字段目前都不消费。
- 指标：`catchUpGaps` 是 broker 内部 atomic，未接 Prometheus；`recovery_gap_total` 只服务 Replayer 路径。
- 时序：dispatch 管道异步，GapNotice **不**保证与补投消息的严格相对顺序；通知靠携带的 position 自描述。

## 4. 设计

### 4.1 broker 层

```go
// CatchUpGap describes one hole detected during reconnect catch-up (C6).
type CatchUpGap struct {
    Channel      string
    Reason       HistoryGapReason // HistoryGapMiddle 或 HistoryGapReplayTruncated
    LastGoodSeq  uint64           // 中洞：洞前最后连续条目的稠密 seq；0 = 未知
    LastGoodOffset uint64         // 对应的 ts<<20|seq offset；0 = 未知
}

type GapHandler func(gap CatchUpGap)

// SetGapHandler registers the catch-up gap handler (C6); nil disables
// client notification (detection counters/logging are unaffected).
SetGapHandler(handler GapHandler)
```

- `broker.go` 的 `HistoryGapReason` 追加 `HistoryGapReplayTruncated`（iota 追加，不动现有值序）。
- `checkCatchUpSeqGap`：计数 + Warn 照旧；**每频道每次 catch-up 至多调一次** handler（第一个洞胜出，`LastGoodSeq/Offset` = 洞前最后连续条目）。
- `checkCatchUpGap`（尾截）：handler 以 `HistoryGapReplayTruncated` 调一次，`LastGoodOffset` = deliveredTail。
- handler 为 nil：零行为变化。handler panic 不得炸 catch-up（recover 包住，记日志）。
- memory broker：no-op。

### 4.2 node / hub 层

- `node.go`：`broker.SetGapHandler(n.onGap)` 接线（与 `SetOccupancyHandler` 并列）。
- `onGap`：取该频道本地订阅者（精确 + 通配命中，复用/仿照 `presenceRecipients`），构造 `OutboundMessage_GapNotice` 逐 Session `Send`；打 `live_gap_notice_total{reason}`。
- 无本地订阅者：不发、不计 notice 指标（broker 内部计数照旧）。
- broker epoch 填入 `Position.stream_epoch`，`offset` = `LastGoodOffset`（0 则 offset 缺省——Position.offset 是 optional）。

### 4.3 proto / wire

```proto
// GapNotice: 重连 catch-up 检出洞（C6）。position = 最后已知安全位置。
message GapNotice {
  string channel = 1;
  messageloop.shared.v2.Position position = 2;
  messageloop.shared.v2.GapReason gap_reason = 3;
}
```

- `OutboundMessage.oneof` 加 `GapNotice gap_notice = 19;`
- `types.proto` 加 `GAP_REASON_REPLAY_TRUNCATED = 6;`（尾截：回放批被 limit 截断，stream 仍有更新条目未送达；历史里还在，客户端可 recover 补齐）。
- 映射：`HistoryGapMiddle → GAP_REASON_MIDDLE`、`HistoryGapReplayTruncated → GAP_REASON_REPLAY_TRUNCATED`。
- `session.go outboundFrameClass`：`GapNotice` 归 **Control** 车道（小、低频、控制语义）。

### 4.4 SDK

- **Go**：`handleMessage` type switch 加 `GapNotice` case，按现有惯例暴露（如 `OnGapNotice` 回调或 Subscription 事件——读 SDK 现有代码选最贴近的惯用法）；不注册回调时静默忽略亦可，但不得 panic/报错。
- **TS**：`converters.ts` 加 case 映射、`client.ts` switch 加分支并暴露事件；**必须**消掉「Unknown message type」路径。
- SDK 语义：GapNotice 是通知，不动 cursor、不进消息流、不与 replay 消息排序。

## 5. 文档

- `01-architecture.md` / `04-cluster.md`：catch-up 段补 GapNotice 一句；`kernel-architecture.md`「Gap 合同」节把「不下发客户端……独立里程碑」改为已落地（C6）并加一行信封形状。
- broker.go 合同注释写清 GapHandler 语义（修掉 pubsub.go 两处悬空引用）。

## 6. 必须存在的测试

1. **broker 中洞通知**：复用 `CatchUpMissed_MiddleGapCounted` 的装配（直接调 `catchUpMissed`），注册 GapHandler → 恰好一次调用，`Reason=Middle`、`LastGoodSeq/Offset` 为洞前条目；counter 仍 +1。
2. **broker 尾截通知**：批满 + stream 有更新条目 → handler 一次，`Reason=ReplayTruncated`。
3. **nil handler 零行为变化**：不设 handler 时两条检测路径只计数不炸。
4. **handler panic 不炸 catch-up**。
5. **node 扇出**：订阅该频道的 Session 收到 `GapNotice`（wire 上 `gap_reason` 正确、`position.offset` 正确）；未订阅的同节点 Session 收不到；命中该频道的通配订阅者收得到。
6. **legacy 不诬报**：无 seq 基线/条目 → 无通知（既有 `LegacyBaselineNoFalsePositive` 扩展或并列新测试）。
7. **lane**：`outboundFrameClass(GapNotice)` == Control。
8. **指标**：`live_gap_notice_total{reason}` 注册并随扇出 +1。
9. **SDK**：Go 收到 GapNotice 不报错且回调可达；TS converter 单测覆盖新 case（若 TS 有测试设施则跑，否则至少 `tsc`/构建通过）。
10. `go test -count=1 ./pkg/redisbroker`；`go test -count=1 -run "TestSim_|TestClusterCommandBus" .`；`go test ./...`；`go test -race . ./pkg/redisbroker`；`cd sdks/go && go test ./...`。

禁止用固定长 `Sleep` 代替同步点 / Eventually。无 Redis 则 Skip 并写明。

## 7. 验收清单

1. catch-up 中洞与尾截都会给本地订阅者发 `GapNotice`，每频道每次 catch-up 至多一条；nil handler 零行为变化。
2. 信封带 channel / gap_reason / 最后安全 position；检测规则（含 legacy 断链）不变。
3. proto 只加 `GapNotice`（field 19）与 `GAP_REASON_REPLAY_TRUNCATED=6`；再生产物 diff 干净。
4. TS SDK 不再对 GapNotice 走「Unknown message type」；Go SDK 正常消费；两 SDK cursor 语义不变。
5. Replayer 恢复路径（RecoverComplete）零改动；C1–C5 合同零改动。
6. §6 测试命令全绿。

## 8. 完成报告

- 文件列表
- §7 逐条证据
- 测试命令与结果
- 偏离（应无；Redis 环境 skip 须写明）

## 9. 实现备注（完成后填写）

（空）
