# PR-KA-B3 实现规格：流式恢复 + client v2 信封

| 字段 | 值 |
| --- | --- |
| 标题 | `recover: stream replay publications; RecoverComplete; client v2 wire` |
| 状态 | **Accepted**（2026-08-17 主 agent 终验通过，尚未 commit） |
| 依赖 | B2 已合。A0 已冻结 `client.v2` / `shared.v2`。在 `v2` 分支上做 |
| 设计来源 | [kernel-architecture.md](../kernel-architecture.md) Protocol 恢复节、KD-K11、KD-K12、KD-K16、KD-K22、KD-K31 |
| 验收人 | 主 agent |

## 1. 目标

恢复不再把最多 1000 条塞进 `Connected` / `SubscribeAck`。Connect / Subscribe / 重订走 **同一个 Replayer**：先 Ack（无批次），再按频道流式 `Publication(replay=true)`，最后 `RecoverComplete`。

1. 客户端数据面切到 **`client.v2` + `shared.v2`**（A0 已生成）。Admin 仍用 `server.v1`（B3 不切 admin）。
2. `Connected` / `SubscribeAck` **没有** `publications` / `recover_results`。
3. 权威是服务端 Position。`cursor.offset` 缺省 ≠ 从头。从头只要 `fresh=true` 或 StreamEpoch 重置。
4. 恢复失败不撤订阅。
5. Go SDK（及 TS 若有恢复路径）**一条**消费路径：replay 与 live 都走 `Publication` 投递；游标只信 `RecoverComplete.position`。
6. 去掉 B1 对 `Connected` 的 `MaxMessageSize` 豁免（不再一帧 1000 条）。

**不做：** HMAC / NodeRPC Stream / `internal/*`（B4）；改 A1 CAS、A2 `History` gap 算法、A3 `CompileInterest`、A4 Decide、B2 Occupancy 投递面；稠密 seq 中洞（Q8）；不要改 `protocol/**/v1/**`。

## 2. 允许改动的文件

- `recover.go` / `recover_test.go`：Replayer 改为流式 Send；`ChannelRecovery` 不再堆积 `[]Publication` 回给 Ack
- `client.go`：`finishConnect` / `handleSubscribe` 先发无批次的 `Connected`/`SubscribeAck`，再调 Replayer；读 `Subscription.cursor` / `fresh`
- `session.go`：`outboundFrameClass` 把 `RecoverComplete` 标 Control；**删除** `Connected` 出站大小豁免
- `marshaler.go` / 传输：`pkg/websocket`、`pkg/grpcstream/client_server.go`、`pkg/quicstream` 的客户端编解码改 `clientv2`（协商名可仍是 `messageloop` / `messageloop+json` / `messageloop+proto`）
- 根包及 `pkg/*` 所有 `client/v1` **客户端信封** import 改为 `client/v2`（别名建议 `clientpb`）；`shared.v2.Position` / `GapReason` / `Error`
- `sdks/go/`：一条 Publication 路径；删 `applyRecoverResults` / Ack 内嵌批次；`WithRecover` 改为 `cursor`+`fresh`
- `sdks/ts/`：同样删 `applyRecoverResults` / `connected.publications` 批次；消费 `recover_complete` + `publication.replay`
- `docs/developer/01-architecture.md`、`docs/protocol.md` 恢复段（若仍写 Ack 内嵌 1000 条）
- `docs/v2/tasks/pr-ka-b3-recover.md`（完成备注）

禁止：改 `protocol/**/v1/**`、改 A2 `HistoryPage` 语义、改 Redis offset 编码、git commit/push。Admin `server.v1` 可继续 `page.Pubs()`。

## 3. 现状（动手前再读）

- `recoverSubscription`（`recover.go`）已统一 Connect/Subscribe；返回 `ChannelRecovery{Publications, Status, Offset}`。`finishConnect` / `handleSubscribe` 把 `Publications` 塞进 Ack。
- v1 `Subscription` 有标量 `offset`/`epoch`；`offset==0` 被当成从头（`recover.go` ~149）。这与 KD-K22 相反。
- v2 已有：`Subscription.cursor`（`Position`）+ `fresh`；`Message.position` + `replay`；`RecoverComplete`；`Connected` 无 pubs。
- B1 写队列：`Send` 同步等落线。`session.go` 对 `Connected` 豁免 `MaxMessageSize`（注释写明等 B3）。
- Go SDK：`handlePublications` 吃 `Connected`/`SubscribeAck` 批次；`applyRecoverResults` 写游标。
- Occupancy 仍发 v1 `PresenceEvent`（无 gen）。切 v2 后应带上 `OccupancyEvent.Gen`。

## 4. 信封与游标

运行时 import：

```go
clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
sharedv2 "github.com/messageloopio/messageloop/shared/genproto/shared/v2"
```

内部 History 仍是 `uint64` offset + `HistoryPage`（A2）。只在进出线转换：

```go
func positionFrom(epoch string, offset uint64, set bool) *sharedv2.Position
// set=false → 只填 stream_epoch，offset 缺省（unset）
func offsetFrom(p *sharedv2.Position) (offset uint64, set bool)
```

### 4.1 何时从头（只这两条）

| 条件 | 行为 |
| --- | --- |
| `sub.Fresh == true` | `sinceOffset=0`，忽略 cursor.offset |
| resume 且 snapshot epoch ≠ broker epoch（两边都非空） | 同今日 epoch reset |
| 其它 | **不是**从头 |

禁止：`cursor.offset==0` 或「没带 offset」当从头。非 resume 且 `recover=true` 且 cursor **unset**：当作「无提示」——用服务端已记录的 delivered offset（有则 `since=off+1`），没有则 **Skip**（与 resume 缺 snapshot offset 一样，避免每次重订灌 1000 条）。

非 resume 且 cursor **set**：`sinceOffset = offset+1`（与今日 `sub.Offset>0` 相同）。

### 4.2 帧顺序（每个请求）

```
Connected | SubscribeAck     // 无 publications；SubscribeAck.recover 见下
然后对每个需要 recover 的频道（请求里的顺序）：
    Publication { messages[].replay=true, position }   // 0..N 帧，每帧一条或一小批，禁止再塞满 1000
    RecoverComplete { channel, position, truncated, gap, gap_reason, error? }
```

`SubscribeAck.recover`：

- 本批没有任何 `recover=true` → `NONE`
- 至少一频道会调 History 或已确定 FAILED → 先发 Ack=`PENDING` 再流；若 **全部** History 前就能判定 skip（通配 / 策略 / 无 cursor）且无将流的频道 → `SKIPPED`
- 不要把失败写进 Ack 就结束而不发该频道的 `RecoverComplete`

每频道结束必须有一条 `RecoverComplete`（含 skip / fail / truncated / ok / epoch reset）。skip/fail 把细节放 `error`；`position` 回显权威游标（失败/空批回显 cursor，成功回最后一条的 Position）。

`truncated`：A2 的满批或「空批+gap」。`gap` / `gap_reason` 来自 `HistoryPage`。

恢复失败：**不** `RemoveSubscription`。

### 4.3 写队列

Replayer 对每个 replay / Complete 调 `Session.Send`（已等 done）。不要先把 1000 条塞进 Data 再丢一条 Control Complete（会乱序）。`RecoverComplete` 标 Control 可以，因为 replay 已逐条落线。

每条 replay 出站受 `MaxMessageSize` 约束。一帧 **一条** `Message`（可以 `Publication.messages` 长度 1）。删掉 `isConnectedEnvelope` 豁免。

### 4.4 Occupancy gen

`deliverPresenceEvent` 发出的 v2 `PresenceEvent.gen` = 该事件的 `OccupancyEvent.Gen`（B2 已有）。不要从 v1 字段编造。

## 5. SDK（一条路径）

Go `sdks/go`：

- `Publication` 无论 `replay` 都进 **同一个** `handlePublication` / 用户回调。
- 删 `applyRecoverResults`；游标只在 `RecoverComplete`（及 live `Message.position`）更新。
- `WithRecover`：`recover=true` + 可选 `cursor`；`WithFresh()` 或等价设 `fresh=true`。禁止文档/API 再说「offset 0 = 从头」。
- 等恢复：可按频道等 `RecoverComplete`，不要等 Ack 里的批次。

TS：同样删 `applyRecoverResults` 与 Connected/SubscribeAck 批次投递。

## 6. 必须存在的测试

1. **Connected 无批次**：带 `recover=true` 的 Connect，第一帧 `Connected` 的 publications 字段不存在/为空；随后有 `replay=true` 的 Publication，再 `RecoverComplete`。
2. **Subscribe 同序**：动态 Subscribe 同样：Ack 无 pubs → replay → Complete。
3. **fresh**：`fresh=true` 从 offset 1 起回放；仅 `cursor.offset=0` 且 `fresh=false` **不是**从头（Skip 或从服务端游标续，断言与「灌全历史」不同）。
4. **失败不撤订**：History error → `RecoverComplete.error` 且 `recover=FAILED` 语义，hub 仍有该订阅。
5. **空批+gap**：A2 的 EmptyExpired / HeadTrimmed 出现在 `RecoverComplete.gap` / `gap_reason`，且不得假装 OK 追上。
6. **写大小**：超 `MaxMessageSize` 的单条 replay 不得靠 Connected 豁免混过；`Connected` 本身不再携带历史。
7. **SDK**：Go（及 TS 若改了）单测：replay Publication 与 live Publication 打到同一回调；`RecoverComplete` 更新游标；不再读 `recoverResults`。
8. 既有 recover 表测试（skip 通配 / 策略 / resume 无 offset）改断言 `RecoverComplete`，语义保持。
9. `go test ./...`；`go test ./sdks/go`；若改了 TS：`cd sdks/ts && npm test`。
10. `go test -race . ./sdks/go`。

禁止固定长 Sleep 代替 Send done / Eventually。

## 7. 验收清单

1. 运行时客户端信封是 `client.v2`；`Connected`/`SubscribeAck` 无内嵌恢复批次。
2. 每个 `recover=true` 的精确频道恰好一条 `RecoverComplete`。
3. `fresh` / epoch 重置才从头；`offset==0` 不再当从头。
4. 恢复失败不撤订阅。
5. Go SDK 一条 Publication 消费路径。
6. 无 `Connected` MaxMessageSize 豁免。
7. 未改 A1/A2/A3/A4/B2 热路径算法。
8. 测试命令绿。

## 8. 完成报告

- 文件列表
- §7 逐条证据
- 测试命令与结果
- 偏离（应无）

## 9. 实现备注（完成后填写）

（实现者填写）

### 9.1 文件列表

**服务端（本 session 续做，主 agent 已抽查的流式恢复不在此列）**

- `client_fix_test.go`：`TestClientSession_HandleMessage_Connect_ACLDeniedSubscription` / `TestClient_ConnectWithUnroutableSubscription_SoftFail` 补 v2 presence 独立信封断言（error → Connected → Presence 三帧）
- `cluster_redis_integration_test.go`：`TestClusterRedis_RemoteResumeTakeover` 改为扫描 Connected 信封（presence/RecoverComplete 落在其后）；新增 `messagesSnapshot()` 访问器
- `pkg/websocket/handler_test.go`：客户端信封 import 切 `client/v2` + `shared/v2`；`Message.Offset` → `Message.Position`
- `pkg/websocket/integration_test.go`：import 切 `client/v2`
- `pkg/quicstream/e2e_test.go`：import 切 `client/v2` + `shared/v2`（`sharedpb` → `sharedv2`）
- `shared/marshaler_test.go`：客户端信封 import 切 `client/v2`

**Go SDK（`sdks/go`，独立 module，replace 走现有 `./../../shared`）**

- `client.go`：import 切 `client/v2`/`shared/v2`；`handleConnected` 读 `stream_epoch`、删 `GetPublications`/`GetRecoverResults`/`GetPresence` 批次；`handleSubscribeAck` 删批次；删 `applyRecoverResults`；`handlePublication` 单一路径（replay 与 live 同回调，仅 live 从 `Message.position` 更新游标）；新增 `handleRecoverComplete`（`RecoverComplete.position` 写游标，unset 不动）；`resumeSubscriptions` 用 `Cursor: Position(epoch, offset)`；`handlePublishAck` 读 `Position`；`WithRecover(cursor *sharedv2.Position)` + 新增 `WithFresh()`；receiveLoop 加 `RecoverComplete` 分支、`Presence` 无 pending 时投给 `OnPresenceSnapshot`；`SendSurveyReply`/`Survey`/`rejectPendingPresence`/`BuildErrorMessage` 换 `sharedv2`
- `message.go`：import 切 v2；`wrapPublicationToMessages` 改为 `messageFromEnv`（读 `position`）；新增 `posOffset` / `Position()` 辅助；`ReceivedMessage` 增加 `OffsetSet`/`Replay`/`Position`
- `presence.go` / `survey.go` / `websocket.go` / `grpc.go` / `quic.go` / `disconnect.go`：import 切 v2
- `mux.go`：`RPCResponse.Error` 随 SDK 类型切 `sharedv2.Error`
- `proxy.go`：SDK 面（`RPCResponse`/`AuthenticateResponse`）切 `sharedv2`；proxy wire（`protocol/proxy/v1` 仍说 shared.v1）新增 `payloadV1toV2`/`payloadV2toV1`/`errorV2toV1` 桥接
- `example/proxyserver/main.go`：同样桥接
- `pr08_test.go`：`TestSDK_SubscribeWithRecover` 改 cursor/fresh 断言；`TestSDK_SubscribeAckPublications` → `TestSDK_RecoverySinglePath`（replay/live 同回调、`RecoverComplete` 写游标、不读 recoverResults）；`TestSDK_PresenceSnapshotOnConnected` → `TestSDK_PresenceSnapshotPushedAfterConnected`（独立 Presence 信封 + unset position 不建游标）
- `proxy_test.go`：payload 辅助拆 v1/v2 两个版本
- `token_ack_test.go` / `client_test.go` / `fix_regression_test.go` / `disconnect_error_test.go` / `quic_test.go`：import 切 v2；`PublishAck{Id, Position}` 断言
- `README.md` / `MIGRATION_GUIDE.md`：恢复段改写（cursor/fresh，删「offset 0 = 从头」）

**TS SDK（`sdks/ts`）**

- `src/message/message.ts`：Payload 切 `shared/v2`；`ReceivedMessage` 增 `offsetSet`/`replay`；`protoToReceivedMessage` 读 `position`
- `src/message/converters.ts`：schema 切 `client/v2`+`shared/v2`；`createConnectMessage`/`createSubscribeMessage` 用 `cursor`+`fresh`；`messageToReceived` 读 `position`；`parseOutboundMessage` 加 `recoverComplete`
- `src/client/types.ts`：`SubscriptionSpec.offset/epoch` → `cursor`/`fresh`
- `src/client/client.ts`：删 Connected/SubscribeAck 批次投递与 `applyRecoverResults`；`recoverComplete` 写游标；presence 独立信封（无 pending query 也投 `onPresenceSnapshot`）；`publishAck` 读 `position.offset`；`deliverMessages` 仅 live 消息推进游标；`connect()`/`resubscribeAllChannels()` 用 `cursor`（无记录则不带 cursor）
- `src/transport/*` 四个文件：import 切 `client/v2`（protobuf codec 的 `$typeName` 校验改为 `messageloop.client.v2.InboundMessage`）
- `test/pr09.test.ts`：恢复段改 cursor/fresh/流式断言（replay+RecoverComplete）；connected presence 改独立信封
- `test/regression.test.ts`：`P0-4` 改流式恢复断言；`epoch` → `streamEpoch`
- `test/protocol.test.ts` / `test/codec.test.ts`：import 与字段切 v2（`stream_epoch`、`position`）
- `README.md`：恢复段改写

**文档**

- `docs/developer/01-architecture.md`：写队列 Control 表加 `RecoverComplete`；`handleConnect` §8 改流式恢复；`Subscribe` 行改裸 Ack + Replayer；offset/epoch 语义段改 cursor/fresh；恢复时序图改流式
- `docs/protocol.md`：`connected` 字段表、`publish_ack`、`publication`、`subscribe_ack` 恢复段全部改 v2 流式（`stream_epoch`、`position`、`recover_complete`、`cursor`+`fresh`，删「offset 0 = 从头」）

### 9.2 偏离

无。硬约束逐条核对：

- 未改 `protocol/**/v1/**`（含生成的 `shared/genproto/client/v1`，仅运行时引用切 v2）
- 未动 A2 `HistoryPage`/gap 算法、A3 `CompileInterest`、A4 Decide、B2 Occupancy 投递面、Redis offset 编码
- 未做 HMAC / `internal/*`；admin 仍 `server.v1`（`page.Pubs()` 不变）
- 服务端流式恢复未回退；`Connected`/`SubscribeAck` 无内嵌批次
- 未 commit / push

### 9.3 验证

```bash
go test ./...                                  # 全绿（root 56s 全包 ok）
go test -race .                                # 全绿
cd sdks/go && go test ./...                    # ok
cd sdks/go && go test -race ./...              # ok
cd sdks/ts && npm test                         # 5 suites / 79 tests 全过
```

Windows 上逐条执行；`sdks/go` 在其目录内执行（replace `./../../shared`）。
