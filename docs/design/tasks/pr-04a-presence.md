# PR-04a 实现规格：Presence 识别与本节点投递（不 emit）

| 字段 | 值 |
| --- | --- |
| 标题 | `server: drop or rewrite ml.type=presence and deliver first-class events locally` |
| 状态 | **Accepted**（2026-08-14 主 agent 终验通过，尚未 commit） |
| 依赖 | **PR-01 已合**（`PresenceQuery` / `PresenceEvent` / `PresenceSnapshot` / Admin `session_id`+`connect_client_id`）。**PR-02 已合**（`ChannelPolicy.Presence` / `LegacyPresenceChannel` / `PresenceSnapshotLimit`）。**PR-03 已合**（同改 `client.go`） |
| 设计来源 | [v1.0-platform-gaps.md](../v1.0-platform-gaps.md) 缺口 2、缺口 6、KD-3、KD-16 |
| 验收人 | 主 agent |

## 1. 目标

订阅精确频道的客户端默认能收到该频道的 join/leave（一等 `PresenceEvent`），并能在 Connect / SubscribeAck 上拿到快照、用 `PresenceQuery` 再拉一次。通配订阅者收到**精确频道**上的事件，自己的 pattern **不**进 PresenceStore、**不**写伴生频道。

本 PR 是 **Phase 1**：`emitPresence` **只**走本节点 `deliverPresenceEvent`。**禁止** `PublishTransient(exactCh, {ml.type=presence})`。混部安全靠两件事：默认不跨节点 emit；`broadcastPublication` 先学会识别 `ml.type=presence` 并改写/丢弃（本阶段生产路径见不到这种帧，单测必须注入）。

本 PR **不**实现：`cluster_emit` 配置与 Phase 2 投递（PR-04b）、Go/TS SDK `OnPresence`、客户端 Survey、服务端 ping、按 user。

## 2. 允许改动的文件

- `presence.go`：`PresenceInfo` 加字段；注释写明 `ClientID == session ID`
- `presence_event.go`：一等信封映射；保留旧 JSON 给 legacy 伴生
- `defaults.go`：新增 `MaxPresenceSnapshotClients = 256`（只加这一项）
- `node.go`：`SetPresenceForSession` 填新字段；新增 `shouldTrackPresence` / `presenceJoin` / `presenceLeave` / `emitPresence` / `deliverPresenceEvent` / `presenceSnapshot` / `presencePublication`。`PublishPresenceJoin/Leave` **保留**，仅供 `legacy_presence_channel=true`
- `hub.go`：`broadcastPublication` 开头识别 `ml.type=presence`；新增收集 presence 收件人（带 `Ephemeral`）的 helper
- `client.go`：所有 presence writer 改走 `shouldTrackPresence`；`Connected` / `SubscribeAck` 填 `presence`；`handleMessage` 增加 `PresenceQuery`。禁止改 `handlePublish` / `handleSurvey` / `handlePing` 的非 presence 逻辑（`refreshPresence` 本身可改）
- `cluster_resume.go`：restore 对 wildcard / ephemeral / `!Presence` 不 `SetPresence`；restore **不**发 join
- `cluster_commands.go`：`handleClusterSubscribe` / `Unsubscribe` 同样门闩；join/leave 走 `presenceJoin` / `presenceLeave`
- `pkg/grpcstream/api_handler.go`：**仅** `GetPresence` 填 `session_id` / `connect_client_id`
- `pkg/topics/matcher.go`：把 `matchCriteria` 导出为 `Match`（一行包装，禁止复制一份新 DSL）
- `metrics.go`、`metrics_test.go`：`presence_failures_total{op}`
- `docs/protocol.md` Presence 节（替换「伴生频道无消费者」）
- `docs/developer/02-configuration.md`：`presence` / `legacy_presence_channel` / `presence_snapshot_limit` 从「尚未读取」改为本 PR 生效
- 测试：`presence_test.go`（新）、`client_fix_test.go`（改 `newPresenceEventObserver` 与 NonEphemeral 断言）、必要时 `cluster_resume_test.go` / `node_test.go` / `pkg/grpcstream/api_handler_test.go` / `pkg/topics/*_test.go`
- `docs/design/tasks/pr-04a-presence.md`（完成备注）

禁止：改 proto、改 `channel_policy.go` 求值、改 SDK 业务、实现 `cluster_emit`、git 写操作。

## 3. 现状（以当前 main 为准，动手前再读）

- Store：`PresenceInfo{ClientID, UserID, ConnectedAt}`。`SetPresenceForSession` 把 `ClientID` 设成 **session ID**（`node.go:227-235`）。
- join/leave：所有 writer `go PublishPresenceJoin/Leave` → `presenceChannel(ch)=ch+"/__presence"` → `PublishTransient`（`node.go:888-929`）。通配会变成 `a.**/__presence`，`ValidateTopic` 拒绝，打 Warn + `PresencePublishFailures`。
- `docs/protocol.md` 原文：「`__presence` currently has **no consumer**」。
- ephemeral 已跳过 store/事件；**通配没有跳过**。`restoreSessionSubscriptions`（`cluster_resume.go:146-150`）对每个非 ephemeral **含 pattern** 都 `SetPresence`。
- `broadcastPublication`（`hub.go:329-448`）把任何 transient 编成 `OutboundMessage.publication`，且 `MessagesDelivered++`。`GetMatchingSubscribers` 丢掉 `Ephemeral`。
- `ChannelPolicy.Presence` 默认 true，**订阅路径尚未读取**。
- `newPresenceEventObserver`（`client_fix_test.go:1104`）订的是伴生频道、数的是 `publication`。本 PR 之后默认路径不再写伴生，**必须改这个 helper**，否则 NonEphemeral 控制测试会假红/假绿。
- `HandleMessage` 对未知 envelope 是 `DisconnectBadRequest`（`client.go:371-373`）。不加 `PresenceQuery` 分支，查询会断连。

## 4. 核心 API

```go
const (
    PresenceMetaTypeKey   = "ml.type"
    PresenceMetaTypeValue = "presence"
    PresenceActionJoin    = "join"
    PresenceActionLeave   = "leave"
)

// PresenceInfo 增加（JSON 缺省兼容旧 Redis 键）：
//   SessionID       string `json:"session_id,omitempty"`        // 正式 session
//   ConnectClientID string `json:"connect_client_id,omitempty"` // Connect.client_id
// ClientID 本版本仍等于 session ID，不要改 PresenceStore 方法签名。

func (n *Node) shouldTrackPresence(ch string, ephemeral bool) bool {
    return !ephemeral && !isWildcard(ch) && n.ChannelPolicy(ch).Presence
}

func (n *Node) presenceJoin(ctx context.Context, ch string, c *Client)
func (n *Node) presenceLeave(ctx context.Context, ch, sessionID, userID string, ephemeral bool)

func (n *Node) emitPresence(ch string, evt *clientpb.PresenceEvent, excludeSession string)
func (n *Node) deliverPresenceEvent(ch string, evt *clientpb.PresenceEvent, excludeSession string)

func (n *Node) presenceSnapshot(ctx context.Context, ch string) *clientpb.PresenceSnapshot

func presencePublication(evt *clientpb.PresenceEvent) *Publication // 给 broadcast 改写 / PR-04b；本 PR 的 emit 不得调用 PublishTransient(它)

func (c *Client) sessionCoversChannel(ch string) bool
```

`topics.Match(pattern, topic string) bool`：`return matchCriteria(pattern, topic)`。

## 5. 算法

### 5.1 是否跟踪

任一条成立则 **不** `Add` / `Remove` / join / leave / 快照：

1. `ephemeral == true`
2. `isWildcard(ch)`（含 `*` 即通配，`hub.go:75-77`）
3. `!n.ChannelPolicy(ch).Presence`

restore / cluster subscribe / refresh / close / unsubscribe **共用**这扇门。

### 5.2 `presenceJoin`（self-join 规范顺序）

仅当 `shouldTrackPresence(ch, c 在该频道的 ephemeral)`：

1. `SetPresenceForSession`（填 `ClientID=session`、`SessionID=session`、`ConnectClientID=c.ClientID()`、`UserID`、`ConnectedAt`）。失败：Warn + `presence_failures_total{op="store"}`，**不**回滚订阅。
2. `emitPresence(ch, joinEvent, excludeSession=c.SessionID())` —— 加入者**不**收 self-join。
3. 若 `ChannelPolicy(ch).LegacyPresenceChannel`：再 `go n.PublishPresenceJoin(...)`（旧伴生 JSON，只精确频道；`shouldTrack` 已排除通配）。

`joinEvent`：

```text
PresenceEvent{
  channel: ch,          // 永远是精确频道
  action:  "join",
  info:    {session_id, user_id, client_id=Connect.client_id, connected_at},
}
```

### 5.3 `presenceLeave`

仅当 `shouldTrackPresence`：

1. `presence.Remove(ch, sessionID)`。失败同样 Warn + `{op="store"}`。
2. `emitPresence(ch, leaveEvent, excludeSession=sessionID)` —— 离开者自己不收 leave。
3. legacy 同上，`PublishPresenceLeave`。

### 5.4 `emitPresence`（Phase 1 唯一路径）

```go
func (n *Node) emitPresence(ch string, evt *clientpb.PresenceEvent, excludeSession string) {
    n.deliverPresenceEvent(ch, evt, excludeSession)
}
```

**禁止**在本函数里 `PublishTransient`。不要预留 `if cluster_emit` 分支——那是 PR-04b。

### 5.5 `deliverPresenceEvent`

1. 精确分片：读 `subShard.subs[ch]`，看 `Subscriber.Ephemeral`。
2. 通配：`hub.matcher.Lookup(ch)` 得到 `Subscriber`。
3. 按 `sessionID` 去重；`Ephemeral == true` 跳过；`sessionID == excludeSession` 跳过。
4. `Send` `OutboundMessage.presence_event`（**不是** `publication`）。
5. **不** `MessagesDelivered++`。Send 失败：`presence_failures_total{op="deliver"}`（可同时 `DeliveryFailures++`，不要用 `MessagesDelivered`）。
6. 扇出可复用 `broadcastParallelLimit` / 阈值 8 的现有节奏，但必须是独立循环，不能先编成 publication 再改。

不要调用 `GetMatchingSubscribers`（它丢掉 ephemeral）。

### 5.6 `broadcastPublication` 改写/丢弃

在编 `OutboundMessage.publication` **之前**：

```go
if pub != nil && pub.Metadata[PresenceMetaTypeKey] == PresenceMetaTypeValue {
    evt := parsePresencePublication(pub) // protojson → clientpb.PresenceEvent
    if evt == nil {
        // 无法解析：丢弃。绝不能当聊天发出。
        // presence_failures_total{op="rewrite"}
        return nil
    }
    if evt.Channel == "" {
        evt.Channel = ch
    }
    n.deliverPresenceEvent(evt.Channel, evt, "")
    return nil
}
```

`presencePublication`（本 PR 实现、单测注入；emit **不用**）：

```go
Payload: protojson(PresenceEvent), Kind: JSON,
Metadata: { "ml.type": "presence" }
```

普通消息路径完全不动。

### 5.7 快照

```
limit = MaxPresenceSnapshotClients           // 256
if pol.PresenceSnapshotLimit > 0 {
    limit = pol.PresenceSnapshotLimit        // 策略可抬高或压低
}
clients = store.Get(ch) 按 session_id 排序
occupancy = len(全部)
if len(clients) > limit {
    clients = clients[:limit]
    truncated = true
}
```

`Get` 失败：空快照 + Warn + `{op="store"}`，订阅仍成功。

通配 / ephemeral / `!Presence`：**不要**往 `Connected.presence` / `SubscribeAck.presence` 塞该频道条目（「快照为空」= 省略，不报错）。

### 5.8 `sessionCoversChannel`

```go
if c.hasSubscription(ch) { return true }
for _, pattern := range 持锁拷贝的 subscribedChannels {
    if isWildcard(pattern) && topics.Match(pattern, ch) {
        return true
    }
}
return false
```

禁止只用 `hasSubscription`。禁止为 Match 再写一套 glob。

### 5.9 `PresenceQuery`

`handleMessage` 增加分支。频道必须是非空**精确**频道（`isWildcard` 或空 → 顶层 Error `BAD_REQUEST` / `type=request_error`，不断连）。

然后按顺序拒绝（顶层 Error，订阅不受影响）：

| 条件 | code | type |
| --- | --- | --- |
| `!sessionCoversChannel(ch)` | `PERMISSION_DENIED` | `acl_error` |
| `!ChannelPolicy(ch).Presence` | `POLICY_DENIED` | `policy_error` |
| 内置 ACL `!CanSubscribe(ch, user)` | `PERMISSION_DENIED` | `acl_error` |

**禁止**只靠 `CanSubscribe`：ACL `*` 时不能偷看未覆盖的房间。

通过则回 `OutboundMessage.presence`（`PresenceSnapshot`，同一 cap）。v1.0 不分页。

### 5.10 接到现有 writer

| 入口 | 今日 | 本 PR |
| --- | --- | --- |
| `handleConnect` | 非 ephemeral → Add + `go PublishPresenceJoin` | `!already && shouldTrack` → `presenceJoin`；Send 前给**当前已订的每个精确跟踪频道**各一份快照（含 restore 回来的） |
| `handleSubscribe` | 同上 | 同上；re-subscribe **不**再 join，但仍给快照（catch-up） |
| `handleUnsubscribe` / `close` / `handleSubRefresh` 撤销 | 非 ephemeral → Remove + leave | `shouldTrack` 才 `presenceLeave` |
| `refreshPresence` | 跳过 ephemeral | 再跳过 wildcard 与 `!Presence`；Add 时带齐新字段 |
| `restoreSessionSubscriptions` | 非 ephemeral 就 SetPresence | `shouldTrack` 才 SetPresence；**不** emit join |
| `handleClusterSubscribe` | 非 already → Set + join | `shouldTrack` → `presenceJoin` |
| `handleClusterUnsubscribe` | 已订 → Clear + leave | `shouldTrack` → `presenceLeave` |

Connect 的 `addedPresence` 回滚列表只应包含真正 `Add` 过的频道（通配不再进这个切片）。

`Connected` / `SubscribeAck` 在现有 recover 字段之外加 `Presence: snapshots`。PR-03 的 recover 行为不得回退。

### 5.11 Admin `GetPresence`

```go
SessionId:        firstNonEmpty(info.SessionID, info.ClientID)
ConnectClientId:  info.ConnectClientID
ClientId:         info.ClientID   // 仍为 session，兼容旧脚本
UserId / ConnectedAt: 不变
```

旧 Redis JSON 缺新字段：`session_id` fallback 到 `client_id`，`connect_client_id` 为空。

### 5.12 指标

`presence_failures_total` CounterVec，标签 `op` = `deliver` | `store` | `rewrite` | `companion`。

`NewMetrics` 必须注册。伴生发布失败：**继续**打现有 `PresencePublishFailures`，并 `op=companion`。不要删旧指标。

## 6. 兼容性

| 客户端 / 运维 | 行为 |
| --- | --- |
| 旧客户端忽略未知 envelope | 收不到 join/leave push（以前也收不到）；订阅仍成功 |
| 订了 `ch/__presence` 的旧客户端 | 默认**不再**有伴生帧。要兼容：策略 `legacy_presence_channel=true` |
| Admin `PresenceInfo.client_id` | 仍是 session ID |
| 直接调用 `PublishPresenceJoin` 的旧测试 | helper 仍在，语义不变（写伴生） |
| `TestClient_NonEphemeralSubscription_PresenceAndEvents` | **必须改观察者**：订精确频道、数 `presence_event` |

## 7. 必须存在的测试

放在 `presence_test.go` 和/或改现有文件。对旧代码会红的路径要覆盖。

| 测试 | 断言 |
| --- | --- |
| `TestPresence_JoinEventAndSnapshot` | A 已订 `chat.room.1`。B Subscribe 同一频道。A 收到一条 `PresenceEvent{action=join, channel=chat.room.1, info.session_id=B, info.client_id=B.client}`，**不是** `publication`。B 的 `SubscribeAck.presence` 含 A 与 B。B 的 transport **没有** self-join 事件 |
| `TestPresence_WildcardSubscriberReceivesExactJoin` | A 订 `chat.**`。B 加入 `chat.room.1`。A 收到 `PresenceEvent{channel=chat.room.1, action=join}`。`PresenceStore` 无 `chat.**` 键 |
| `TestPresence_EphemeralNoStoreOrEvent` | ephemeral Subscribe：store 空，无人收到 join（可改现有 `TestClient_EphemeralSubscription_NoPresenceOrEvents`，不要删） |
| `TestPresence_QueryRequiresCoverage` | A 订 `chat.**`，Query `chat.room.1` → 快照。C 未覆盖该频道（即便默认 ACL 放行）→ `PERMISSION_DENIED` / `acl_error`，**不是**快照 |
| `TestPresence_QueryWildcardRejected` | Query `chat.**` → `BAD_REQUEST`，不断连 |
| `TestPresence_PolicyPresenceFalse` | 策略 `presence=false` 的频道：Subscribe 成功、store 空、无事件、Query → `POLICY_DENIED` |
| `TestPresence_SnapshotTruncated` | store 预置 300 人，`presence_snapshot_limit=256`（或默认）。Subscribe 后 `truncated=true`，`occupancy=301`（含自己），`len(clients)<=256`，加入者无 self-join |
| `TestPresence_NoCompanionByDefault` | 默认策略 join/leave：**不**调用 `PublishTransient("ch/__presence")`；`PresencePublishFailures` 不增。用计数 fake broker |
| `TestPresence_LegacyCompanionExactOnly` | 策略 `legacy_presence_channel=true`：精确频道有伴生 transient；通配订阅**仍不**写 `im.**/__presence` |
| `TestPresence_ValidateTopicCompanionStillRejected` | `topics.ValidateTopic("a.**/__presence")` 仍 `ErrBadTopic`；`a.**.b/__presence` 仍拒绝 |
| `TestPresence_BroadcastPresenceNotPublication` | 构造 `Publication{Metadata: {ml.type:presence}, Payload: protojson(event)}` 调 `broadcastPublication`。订阅者收到 `presence_event`，**零**条 `publication`；`MessagesDelivered` 不增 |
| `TestPresence_RestoreWildcardSkipsStore` | restore / `handleClusterSubscribe` 传入通配 pattern：不对 pattern `SetPresence`（可扩现有 `TestNode_RestoreSessionSubscriptions_SkipsPresenceForEphemeral`） |
| `TestPresence_ResubscribeSnapshotNoSecondJoin` | 已订 ch，再 Subscribe：Ack 仍有快照，其他成员**不再**收到第二条 join |
| `TestAdmin_GetPresenceFillsNewFields` | GetPresence：`client_id`==session，`session_id`==session，`connect_client_id`==Connect.client_id |

现有 `TestNode_PublishPresenceJoin_DistinctMessageIDs`、`TestNode_PublishPresenceFailure_IncrementsMetric` 必须继续绿（它们直接打 companion helper）。

## 8. 文档

`docs/protocol.md` Presence 节改成：

- 订了 `C` 就收 `C` 的 `presence_event`，不必再订 `C/__presence`
- 快照在 `connected.presence` / `subscribe_ack.presence`；`truncated` / `occupancy` / cap
- `PresenceQuery` 鉴权：`sessionCoversChannel` + 策略 + ACL；失败 `PERMISSION_DENIED` / `POLICY_DENIED`
- 通配：事件 `channel` 永远是精确频道；pattern 不进 store、无快照、无伴生
- 失败不断连、不撤销订阅
- 默认不写伴生；`legacy_presence_channel` 才写精确 `ch/__presence`
- Phase 1 只本节点投递（不写跨节点 emit）

`02-configuration.md`：`presence` / `legacy_presence_channel` / `presence_snapshot_limit` 改为「PR-04a：`shouldTrackPresence` / 快照 / 伴生门闩读取」。

## 9. 验收清单（实现者自检 + 主 agent 终验）

1. `emitPresence` 只调 `deliverPresenceEvent`，零 `PublishTransient(exact, ml.type=presence)`。
2. 默认路径不再写 `__presence`；无人订伴生时 `PresencePublishFailures` 不因 join/leave 增加。
3. 通配 / ephemeral / `presence=false` 不进 store、不发事件。
4. 通配订阅者能收到精确频道的 `PresenceEvent`。
5. 加入者快照含自己，无 self-join 事件。
6. `broadcastPublication` 遇到 `ml.type=presence` 绝不产出 `publication`。
7. PresenceQuery：未覆盖 → `PERMISSION_DENIED`；通配频道名 → `BAD_REQUEST`；`presence=false` → `POLICY_DENIED`。
8. restore / cluster subscribe 不对 pattern `SetPresence`。
9. `Connected.presence` / `SubscribeAck.presence` 已填；PR-03 recover 字段仍在。
10. 无 proto 变更；无 `cluster_emit`；`go test -count=1 . ./config/... ./pkg/topics/... ./pkg/grpcstream/...` 与 `go test -race -count=1 .` 绿。

## 10. 完成报告

- 文件列表
- `emitPresence` / `deliverPresenceEvent` / `presenceJoin` / `sessionCoversChannel` / Query 分支的文件:行
- writer 门闩改了哪几处
- broadcast 改写位置
- §7 每个测试：过/失败
- §9 十条：过/失败 + 证据
- 偏离与理由

## 11. 实现备注（留给落地后填写）

（实现完成后由实现者补 2–6 条非显而易见决定。）

1. **Hub 持有 node 反引用**：`broadcastPublication` 需要在开头重写/丢弃 `ml.type=presence` 帧并委托给 `deliverPresenceEvent`，而 Hub 原本没有 node 引用；在 `Hub` 加 `node *Node` 字段、`NewNode` 里赋值一行（`node.hub.node = node`）。生产路径见不到这种帧，该分支由单测注入。
2. **presenceJoin 不接收 ephemeral 参数**：按规格签名 `presenceJoin(ctx, ch, c)`，ephemeral 从调用点已入册的 `hub.LookupSubscriber(ch, c)` 现查（三个调用点——handleConnect / handleSubscribe / handleClusterSubscribe——订阅都先于 presenceJoin 完成）。
3. **leave 事件不携带 ephemeral 参数则无法回填 connect_client_id**：`presenceLeave(ch, sessionID, userID, ephemeral)` 没有 client 参数，leave 事件的 `info.client_id`（协议上是 Connect.client_id）在离开者还挂着时经 `hub.LookupSession(sessionID)` 现查；已不在 hub（如 close 竞态）则为空。`connected_at` 同样回填。
4. **presenceRecipients 在 hub.go 返回带 Ephemeral 的收件人列表**：不用 `GetMatchingSubscribers`（丢 ephemeral 标志），精确分片读 `subShard.subs[ch]` + matcher `Lookup(ch)`，按 sessionID 去重，串行阈值 8 / `broadcastParallelLimit` 的独立扇出循环，绝不先编成 publication。
5. **客户端协议里 PresenceInfo.client_id 是 Connect.client_id，不是 session**：`presenceSnapshot` 的客户端映射 `client_id → store.ConnectClientID`（缺省空）、`session_id → store.SessionID`（fallback 旧键 ClientID）；服务端 Admin 侧 `client_id` 才仍是 session。
6. **两个既有测试因行为变更必须适配**：`newPresenceEventObserver` 改为订精确频道、数 `presence_event`（观察者自身成为频道的 tracked 成员，ephemeral/policy 测试的 store 断言相应调整为「仅观察者」或「空」）；`TestGRPC_ClientStream_MultipleSubscribers` 的收件循环跳过先到的 presence_event（订同一频道的同伴 join 现在会推给精确订阅者）。`TestNode_Survey_ConcurrentClients` 的并发 `messages = nil` 与 presence 扇出竞态：`capturingTransport` 加同步的 `resetMessages()`。
