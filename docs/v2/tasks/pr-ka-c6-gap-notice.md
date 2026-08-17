# PR-KA-C6 实现规格：catch-up 洞的 client-facing GapNotice（live gap 信封）

| 字段 | 值 |
| --- | --- |
| 标题 | `broker: client-facing GapNotice for reconnect catch-up holes` |
| 状态 | **Accepted**（2026-08-17 主 agent 终验通过，尚未 commit） |
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

实现于 `v2` 分支（基线为 C6 规格提交 `98d18c9`，C5 tip `1debf5a` 之上）。真实 Redis（127.0.0.1:6379，测试 DB 14，沿用 `requireCommandBusRedis`）全量实跑，无 skip。

- **broker 合同（`broker.go`）**：`HistoryGapReason` iota 追加 `HistoryGapReplayTruncated`（现有值序不动）；新增 `CatchUpGap{Channel, Reason, LastGoodSeq, LastGoodOffset}`、`GapHandler`、`Broker.SetGapHandler(handler GapHandler)`（按 §4.1 签名，无 error 返回，区别于 `SetOccupancyHandler`）。合同注释写明：nil 关闭通知、检测计数/日志不受影响、panic recover、每频道每次 catch-up 至多一条、memory 为 no-op。
- **Redis broker（`pkg/redisbroker/redis.go` / `pubsub.go`）**：`redisBroker` 加 `gapHandlerMu + gapHandler` 字段与 `SetGapHandler`；`pubsub.go` 加 `notifyGap`（nil 短路 + recover 记 Error，照 `deliver`/`deliverOccupancy` 先例）。`checkCatchUpSeqGap` 加 `baselineOffset` 形参并跟踪 `prevOffset`（洞前最后连续条目的 stream offset；洞开在基线之后时取基线 offset），第一个洞通知一次（`Reason=HistoryGapMiddle`，`LastGoodSeq/Offset`=洞前条目）并返回「已通知」；`checkCatchUpGap`（尾截）加 `alreadyNotified` 形参，检出时计数 + Warn 照旧、仅在未通知过时才以 `HistoryGapReplayTruncated` + `LastGoodOffset=deliveredTail` 通知。检测规则、legacy 断链（seq 0 → prev/prevOffset 归零断链）、`catchUpGaps` 计数语义零改动。pubsub.go 两处「client-facing gap envelope 是 future work」注释同步更新为 C6 语义。
- **memory broker**：`SetGapHandler` no-op（无 catch-up 概念）。
- **fakes**：`Broker` 接口新增方法导致 12 个测试 fake 补 no-op：`client_test.go` fakeHistoryBroker、`client_fix_test.go` fakeEpochHistoryBroker + failStartBroker、`cluster_resume_test.go` evictTestBroker/failSubscribeBroker/failSecondSubscribeBroker、`health_test.go` fakeBrokerNoReady、`node_test.go` failTransientBroker、`recover_test.go` gapHistoryBroker/trimmedHistoryBroker、`presence_test.go` countingBroker、`pkg/grpcstream/api_handler_test.go` failPublishBroker/probeBroker。全部只加一行 `SetGapHandler(GapHandler) {}`，diff 逐行核对无 gofmt 无关 churn（HEAD 若干文件本非 gofmt-clean，已回退重做一次以保 diff 干净）。
- **node/hub（`node.go`，hub.go 零改动）**：`Run` 中 `broker.SetGapHandler(n.onGap)` 与 `SetOccupancyHandler` 并列接线。`onGap` 复用现有 `hub.GetMatchingSubscribers(ch)`（精确 + 通配命中、按 session 去重——ephemeral 不过滤，spec 未要求），构造 `OutboundMessage_GapNotice{channel, position=positionFrom(n.streamEpoch(), LastGoodOffset, LastGoodOffset>0), gap_reason}` 逐 Session `Send`。reason 映射放在 node.go 的 `gapNoticeReasonV2`（**不动 recover.go 的 `gapReasonV2`**：Replayer 路径零改动约束）；指标 label 走 `gapNoticeReasonLabel`（middle/replay_truncated）。无本地订阅者不发不计指标；发送失败 Warn + `DeliveryFailures`，不计 MessagesDelivered。
- **lane / 指标**：`session.go outboundFrameClass` 加 `OutboundMessage_GapNotice` → Control 车道；`metrics.go` 加 `LiveGapNoticeTotal`（`live_gap_notice_total{reason}`）并注册。
- **proto / 再生**：`protocol/client/v2/service.proto` 加 `GapNotice` message（channel/position/gap_reason）+ oneof field 19；`protocol/shared/v2/types.proto` 加 `GAP_REASON_REPLAY_TRUNCATED = 6`。`buf generate`（buf 1.65.0，远端插件）后 diff 仅 `shared/genproto/client/v2/service.pb.go`、`shared/genproto/client/v2/service.swagger.json`、`shared/genproto/shared/v2/types.pb.go`、`sdks/ts/src/proto/client/v2/service_pb.ts`、`sdks/ts/src/proto/shared/v2/types_pb.ts`；openapiv2 插件对 client/v1、proxy/v1、server/v1 三个 swagger 的无关重命名 churn（C4 同款）已 `git checkout` 回退。
- **Go SDK（`sdks/go`）**：新文件 `gap.go`（`GapNotice{Channel, GapReason sharedv2.GapReason, StreamEpoch, Offset, OffsetSet}` + `gapNoticeFromPB`）；`client.go` 加 `gapNoticeHandler` 字段、`OnGapNotice` 回调（Client 接口 + 实现）、`handleMessage` 加 `OutboundMessage_GapNotice` case → `handleGapNotice`。不进消息流、不动 `channelOffsets` cursor、无 handler 静默忽略。
- **TS SDK（`sdks/ts`）**：`client/types.ts` 加 `GapNotice` 接口（`gapReason` 直接用 proto 枚举类型）与 `IClient.onGapNotice`；`message/converters.ts` 加 `gapNoticeFromPB` 与 `parseOutboundMessage` 的 `"gapNotice"` case（union type 同步扩展——**消掉「Unknown message type」路径**）；`client/client.ts` 加 `gapNoticeHandler` 字段、`onGapNotice` 方法、switch case；`client/index.ts`、`src/index.ts` 导出 `GapNotice` 类型。cursor（`channelOffsets`）零触碰。
- **测试**（全部实跑通过，无固定长 Sleep；均用直接调 `catchUpMissed`/`onGap`/`handleMessage` 或 channel 同步点）：
  - `pkg/redisbroker/pubsub_test.go`：`CatchUpMissed_MiddleGapNotified`（恰好一次，`Reason=Middle`、`LastGoodSeq=2`、`LastGoodOffset`=seq2 条目 offset，counter 仍 +1）、`CatchUpMissed_ReplayTruncatedNotified`（发布后缩 `StreamMaxLength=2` 再直接调 catch-up，`Reason=ReplayTruncated`、`LastGoodOffset`=回放尾部，counter +1）、`CatchUpMissed_OneNoticePerChannelPerCatchUp`（同一次 catch-up 同时中洞+尾截：counter +2、通知恰好一条且中洞胜出）、`CatchUpMissed_GapHandlerPanicContained`（panic recover，补投与计数照常）、`CatchUpMissed_LegacyBaselineNoNotification`（注册 handler 的 legacy 基线零通知）。nil handler 零行为变化由既有 `CatchUpMissed_MiddleGapCounted` / `CatchUpGapDetected` / `CatchUpMissed_LegacyBaselineNoFalsePositive`（均未设 handler、只断言计数）覆盖。
  - 根包新文件 `gap_notice_test.go`：`OnGapFansOutGapNotice`（精确订阅者收到且 wire 上 channel/gap_reason=MIDDLE/position.offset+stream_epoch 正确；`gap.**` 通配订阅者收到；无关订阅者收不到；非 Publication；`live_gap_notice_total{middle}` +1）、`OnGapReplayTruncated`（REPLAY_TRUNCATED 映射；offset=0 → wire 上 offset 缺省而非 0）、`OnGapNoLocalSubscribers`（无订阅者不发不计指标）。
  - `session_test.go` `OutboundFrameClass_GapNotice`（Control 车道；Publication 仍 Data）；`metrics_test.go` `LiveGapNoticeRegistered`。
  - `sdks/go/gap_test.go`：回调可达、字段映射正确、OnError 不触发、cursor 不动；无 handler 静默忽略 + unset offset 不落成 0。
  - `sdks/ts/test/gapnotice.test.ts`：converter 映射（含 unset offset → undefined）、client 分发到 onGapNotice、onError 不触发、cursor 不动、无 handler 静默忽略。
- **文档**：`01-architecture.md` Redis broker 段 catch-up 句改为「检出洞 → SetGapHandler 上报 → node 扇出 GapNotice（C6）」；`04-cluster.md` §8 加「catch-up 洞通知（C6）」一条；`kernel-architecture.md`「Gap 合同」节把「不下发客户端……独立里程碑」改为已落地（C6）并写明信封形状。
- **验证**（串行执行，真实输出）：`go test -count=1 ./pkg/redisbroker` ok 61.989s；`go test -count=1 -run "TestSim_|TestClusterCommandBus" .` ok 0.134s；`go test ./...` 全 ok（根包 76.915s 等 11 包）；`go test -race . ./pkg/redisbroker` ok（79.535s / 66.749s）；`cd sdks/go && go test ./...` ok；`cd sdks/ts && npx jest` 6 suites / 83 tests 全过（含新 gapnotice.test.ts 4 条）；`npm run build`（esm+cjs+types tsc）通过。回退重做 broker.go/redis.go/broker_memory.go/node_test.go/cluster_resume_test.go 后，`go build ./...` + 点名复跑全部新增测试再确认通过。
- **偏离**：无。hub.go 未加 helper（`GetMatchingSubscribers` 直接复用，符合 §2「仅当不能复用时才加」）；`recover.go` / `client.go` / C1–C5 合同零改动（git status 佐证）。
