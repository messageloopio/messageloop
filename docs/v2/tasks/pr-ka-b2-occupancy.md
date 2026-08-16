# PR-KA-B2 实现规格：Occupancy 只走 LiveBus + OccupancyGen

| 字段 | 值 |
| --- | --- |
| 标题 | `occupancy: live-bus only, OccupancyGen, drop Hub.node and cluster_emit` |
| 状态 | **Accepted**（2026-08-17 主 agent 终验通过，尚未 commit） |
| 依赖 | B1 已合（Session 稳定）。A3 LiveBus 编译已在。在 `v2` 分支上做 |
| 设计来源 | [kernel-architecture.md](../kernel-architecture.md) Occupancy、KD-K7、KD-K8、KD-K9、KD-K9b、KD-K31 |
| 验收人 | 主 agent |

## 1. 目标

Occupancy 事件退出业务 Publication / `ml.type` 改写 / `cluster_emit` 双路径。跨节点 **只** 走 LiveBus（精确频道），本节点 Interest（含 `im.**` 编译）的节点才能收到。

1. `Join` / `Leave` 各取一个单调 **OccupancyGen**；事件带 gen。接收方 `gen <= last_applied[ch][session]` 丢弃。
2. 存完 store 之后 **只** `PublishOccupancy(exactCh, evt)`。禁止 `PublishTransient`、禁止并行再 `deliverPresenceEvent`、禁止 NodeRPC 广播。
3. 删 `server.presence.cluster_emit`。删 `Hub.node` 以及 `broadcastPublication` 里对 `ml.type=presence` 的改写。
4. 通配者不进 store；靠 Coverage 收被命中精确频道的事件（今日 `presenceRecipients` + A3 Interest）。
5. 自己不收 self-join/leave；快照可含自己。

**不做：** 流式恢复（B3）；HMAC / `internal/*`（B4）；把 Broker 改名为 LiveBus；切 `clientv2` 信封；改 A1 CAS、A2 gap、A3 `CompileInterest`、A4 Decide；上 ORSWOT；为 Occupancy Bind 频道；Redis Cluster / sharded PUB/SUB。

## 2. 允许改动的文件

- `occupancy.go`（新）+ `occupancy_test.go`（新）：`OccupancyEvent`、`OccupancyGen` 发号、`last_applied` 过滤、Join/Leave 合同
- `presence.go` / `presence_event.go` / `presence_test.go`：store 仍可留；删 `presencePublication` 作为 **live 路径**；`ml.type` 不再进 `broadcastPublication`
- `node.go`：`presenceJoin` / `presenceLeave` / `emitPresence` 收成 Join+PublishOccupancy；删 `presenceClusterEmit*`
- `hub.go`：删 `node` 回指针与 presence 改写分支；`broadcastPublication` 只扇 Publication
- `broker.go`、`broker_memory.go`、`broker_memory_test.go`：新增 `PublishOccupancy`（**不**写历史）；`Start` 或单独 setter 登记 occupancy handler
- `pkg/redisbroker/redis.go`、`pubsub.go`、`message.go` 及必要测试：live 信封增加 occupancy 类型；`interested()` 后走 occupancy handler，**不得**当 Publication 进 `deliverOnce`
- `pkg/redisbroker/presence_redis.go`：仅当 Join/Leave 要 `INCR` gen 或 Get 发现 TTL 蒸发要合成 leave
- `config/config.go`、`config/config_test.go`、`config-example.yaml`：删 `server.presence.cluster_emit`（YAML 再写则 Validate 失败，与 A4 旧键同策略）
- `cluster_v1_e2e_test.go`、`docs/developer/01-architecture.md`、`docs/developer/02-configuration.md`、`docs/developer/04-cluster.md`：去掉 cluster_emit 叙事
- 所有因删 `Hub.node` / `cluster_emit` / `ml.type` 改写而编译失败的测试
- `docs/v2/tasks/pr-ka-b2-occupancy.md`（完成备注）

禁止：改 proto（v1 不加字段；v2 `PresenceEvent.gen` 已冻结，本 PR **不**切运行时到 v2）、SDK 业务、A1/A2/A3/A4 热路径、git commit/push。

## 3. 现状（动手前再读）

- `presenceJoin` / `presenceLeave`：`shouldTrackPresence` 后门 store，再 `emitPresence`。
- `emitPresence`：`cluster_emit=false`（默认）只 `deliverPresenceEvent`（本机）；`true` 则 `broker.PublishTransient(ch, presencePublication(evt))`，**不再**本地 deliver。Hub `broadcastPublication` 认 `Metadata["ml.type"]=="presence"` 改写成 `deliverPresenceEvent`。两路径禁止叠加（会双发）。
- `Hub.node` 只为这条改写存在。
- Redis live 只处理 `messageTypePublication`（`pubsub.go`）；A3 `interested()` 已按编译 Interest 过滤。
- 通配 / ephemeral / `Presence=false` 已不进 store（`shouldTrackPresence`）。
- v1 `clientpb.PresenceEvent` **没有** `gen`。v2 有 `gen=4`。运行时仍是 v1 信封。

## 4. 类型

根包（建议 `occupancy.go`）：

```go
// OccupancyEvent is the LiveBus payload for join/leave. Gen is node-to-node
// ordering (KD-K8). It is NOT added to the v1 client PresenceEvent (A0 froze
// v1; v2 already has gen for a later runtime switch).
type OccupancyEvent struct {
    Event *clientpb.PresenceEvent // channel, action, info
    Gen   uint64
}

// ErrLateOccupancy is returned (and counted) when a receiver drops an event
// because gen <= last_applied[ch][session].
var ErrLateOccupancy = errors.New("late occupancy event")
```

`Broker` 增加（`broker.go`）：

```go
// OccupancyHandler is invoked for live occupancy events. It must not be the
// publication handler. Errors are logged; they never fail Join/Leave.
type OccupancyHandler func(channel string, evt OccupancyEvent) error

// PublishOccupancy fans an occupancy event on the live bus for exact channel
// ch. It never writes Stream/history. Delivery follows Interest (exact or
// compiled pattern). Handler errors do not fail the call (KD-K14).
PublishOccupancy(ch string, evt OccupancyEvent) error
```

`Start` 可加 `occupancy OccupancyHandler` 参数，或 `SetOccupancyHandler`。memory 与 Redis 都要接到。现有 `PublicationHandler` 不得再收到 occupancy。

OccupancyGen 发号：

| 适配器 | 发号 |
| --- | --- |
| memory | 每频道进程内 `uint64`，Join/Leave/合成 leave 各 `+1` |
| Redis | `INCR` 键（建议 `ml:occ:gen:{ch}`，可走现有 prefix）。禁止随机 UUID |

比较：同一 `(channel, subjectSessionID)` 上 `gen` 全序。`gen==0` 视为非法，丢弃。

## 5. 算法

### 5.1 Join / Leave（替换 `emitPresence`）

```
presenceJoin(ch, session):
    if !shouldTrackPresence: return
    store.Add(...)            // 失败：warn + 指标，不撤订阅（现行）
    gen = nextOccupancyGen(ch)
    evt = OccupancyEvent{Event: PresenceEvent{...join...}, Gen: gen}
    _ = broker.PublishOccupancy(ch, evt)   // 失败：warn + 指标，不撤订阅

presenceLeave: 同结构，action=leave，store.Remove
```

禁止：

- `PublishTransient` / `Node.PublishTransient`
- `presencePublication` + `ml.type`
- `emitPresence` 里再调 `deliverPresenceEvent`（会与 LiveBus 回环双发）
- 通配 / ephemeral 走这条路径（`shouldTrackPresence` 已挡）

### 5.2 接收（本机与跨节点同一条）

```
onOccupancy(ch, evt):
    if evt.Gen == 0 or evt.Event == nil: drop
    sid = evt.Event.Info.SessionId
    if evt.Gen <= lastApplied[ch][sid]: count late; return
    lastApplied[ch][sid] = evt.Gen
    deliverPresenceEvent(ch, evt.Event, exclude=sid)  // 自己不收 self-join/leave
```

`deliverPresenceEvent` / `presenceRecipients` 可留：按 Coverage 扇到本机 Session。**Hub 不再认识 occupancy。**

memory：`PublishOccupancy` 仅当 `interested(ch)` 调 handler（与 A2 Publication 一致）。handler 即 `onOccupancy`。同步调用即可。

Redis：`PUBLISH PubSubPrefix+ch`，payload 类型 **不是** `pub`。`runPubSub` 在 `interested()` 之后分支：occupancy → occupancy handler，**不要** `deliverOnce`（occupancy 无 stream offset）。A3 编译订阅已保证：只订 `chat.1` 的节点收不到 `im.room.1` 的 occupancy；订 `im.**` 的节点能收到 `im.room.1`。

本节点发、本节点收：memory 同步 handler；Redis 走 PUBLISH 回环（与今日 `cluster_emit=true` 相同）。**不要**再本地 deliver 一次。

### 5.3 迟到与合成 leave

- 乱序 / 重放：`last_applied` 丢弃。
- 显式 Leave 必须新 gen（大于该 session 上次 Join）。
- TTL 蒸发（Redis member 键没了）：若 `Get` / 刷新路径已经修剪幽灵成员，对该 session **合成**一条 leave 并取新 gen、`PublishOccupancy`。B2 **不**新开 SCAN 循环；只挂在已有 Get/refresh 修剪点。memory store 无 TTL 则无合成。

### 5.4 删除旧路径

- 配置：`server.presence.cluster_emit` 出现（非默认 false 空块）→ `Validate` 失败，文案含 `cluster_emit is removed`。
- `Hub.node` 字段删除；`NewNode` 不再赋值。
- `broadcastPublication` 开头的 `ml.type` 分支删除。注入的「当聊天收 presence」测试改成：occupancy **不得**出现在 Publication handler。
- `presencePublication` / `parsePresencePublication`：live 路径禁用。若 `legacy_presence_channel` 仍往 `ch/__presence` 发 JSON，可保留为独立遗留钩子，但 **不得**再给 first-class 事件套 `ml.type`。

## 6. 接入

- `Node.Run` / broker `Start`：登记 occupancy handler → `onOccupancy`。
- 心跳续 presence TTL：仍 `store.Add` 刷新；**不**新发 join（避免 gen 噪声）。除非实现能区分「从未在 store」与「刷新」。
- Fence（B1）：仍不 Leave。真走 `Close` 仍 Leave。
- Admin `GetPresence`：仍读 store + Capability（A4），不走 Coverage。

## 7. 必须存在的测试

1. **单路径**：Join 后本机 Coverage 订阅者收到恰好 **一条** `PresenceEvent`（join）。用 spy 数 `PublishOccupancy` 与 `deliverPresenceEvent` / 客户端信封。禁止双发。
2. **不进 Publication**：Join 不得调用 publication handler，不得出现 `ml.type=presence` 的 `Publication`。
3. **通配 Coverage**：Session 只订 `im.**`，精确 `im.room.1` 上另一人 Join → 收到 join；store 无 `im.**` 成员。
4. **跨节点（Redis）**：节点 A 订 `im.**`，节点 B `PublishOccupancy`/`presenceJoin` `im.room.1` → A 的 handler/客户端收到；A 订的不是该树时（如只订 `chat.1`）收不到。复用 A3 live 测试的 Redis 辅助。无 Redis 则 Skip 并写明。
5. **OccupancyGen**：同一 session Join 再 Leave，leave.gen > join.gen。重放旧 join（更小 gen）不投递（`last_applied`）。
6. **self**：加入者自己收不到 join；快照 `Get` 含自己。
7. **store 失败不撤订阅**：`Add` 失败仍保持 hub 订阅（现行合同）。
8. **无 `Hub.node`**：编译即可；`hub.go` 无 `node *Node`。
9. **无 `cluster_emit`**：`Validate` 拒绝该键；仓库生产代码无 `presenceClusterEmit`。
10. 改写后的原 `cluster_emit` 单发 / 通配收精确 join 测试仍表达同一语义（现在是默认路径）。
11. `go test ./...`；`go test -race . ./pkg/redisbroker`。

禁止固定长 Sleep 代替 Ready / Eventually。

## 8. 验收清单

1. 仓库无 `Hub.node`、无 `cluster_emit` 热路径、无 `broadcastPublication` 认 `ml.type`。
2. Occupancy 不走 `PublishTransient`；Publication handler 看不到 join/leave。
3. `im.**` 的节点能收到 `im.room.1` 的 join；只订无关频道的节点收不到。
4. OccupancyGen 单调；迟到事件丢弃。
5. 本机恰好一条，不双发。
6. 未改 A1/A2/A3/A4 热路径。
7. 测试命令绿。

## 9. 完成报告

- 文件列表
- §8 逐条证据
- 测试命令与结果
- 偏离（应无）

## 10. 实现备注（完成后填写）

**实现者**：B2 工程师（2026-08-17，分支 `v2`）

**改动文件**

- 新增 `occupancy.go` / `occupancy_test.go`：`OccupancyEvent`、`ErrLateOccupancy`、`OccupancyGenSource`、`SyntheticLeaveReporter`；gen==0/nil 丢弃、late sentinel、`§8.1` 源码级抽查测试。
- `broker.go`：`Broker` 增加 `PublishOccupancy` + `SetOccupancyHandler`；新增 `OccupancyHandler` 类型。
- `broker_memory.go` / `broker_memory_test.go`：occupancy handler 登记；`PublishOccupancy` 只按 `interested(ch)` 同步调 handler，不写历史、不进 publication handler；两项单测用 `Ready()` 同步（无固定 Sleep）。
- `presence.go`：`memoryPresenceStore` 实现 `NextOccupancyGen`（每频道进程内 `uint64`）。
- `node.go`：删 `presenceClusterEmitFlag` / `presenceClusterEmit()` / `emitPresence`；`presenceJoin`/`presenceLeave` 存完 store 后 `publishOccupancy(ch, evt)`（只 `PublishOccupancy`，不 `PublishTransient`、不本地 deliver）；新增 `nextOccupancyGen`（presence 适配器发号，无适配器回退节点本地计数）、`onOccupancy`（gen==0/nil 丢、`last_applied[ch][session]` 弃迟到并返回 `ErrLateOccupancy`、`deliverPresenceEvent` 排除 subject）、`onSyntheticLeave`（Redis Get 修剪幽灵成员→合成 leave）；`Run` 登记 occupancy handler + synthetic-leave hook。
- `hub.go`：删 `node *Node` 回指针与 `broadcastPublication` 的 `ml.type` 分支；`NewNode` 不再赋值。
- `presence_event.go`：删 `PresenceMetaTypeKey/Value`、`presencePublication`、`parsePresencePublication`；保留 legacy companion 路径。
- `pkg/redisbroker/message.go`：`messageTypeOccupancy` + `redisOccupancy` 信封（`t:"occupancy"`，不是 `pub`）；`serializeOccupancy`/`deserializeOccupancy`（protojson）。
- `pkg/redisbroker/pubsub.go`：`runPubSub` 在 `interested()` 后按 `Type` 分支——`pub` 走 `deliverOnce`，`occupancy` 走 occupancy handler（不经 `deliverOnce`，无 stream offset）；worker pool 增 occupancy 投递。
- `pkg/redisbroker/redis.go`：`occHandler`、`occupancyFailures`、`SetOccupancyHandler`、`PublishOccupancy`（只 PUBLISH，不写 Stream）。
- `pkg/redisbroker/presence_redis.go`：`NextOccupancyGen` = `INCR ml:presence:occ:gen:<ch>`；`Get` 修剪 TTL 蒸发幽灵时调 `SetSyntheticLeaveHook` 注册的回调。
- `config/config.go`（`ClusterEmit` 改 `*bool` + Validate 拒绝 `cluster_emit is removed`）、`config_test.go`、`config-example.yaml`。
- `cluster_v1_e2e_test.go` / `cluster_redis_integration_test.go`：presence 跨节点测试改写为默认 Occupancy 路径，`time.Sleep` 改为 `require.Never`；wildcard 跨节点 + 无关节点收不到的负断言之锚定。
- 编译修复的 Broker 测试桩：`client_test.go`、`client_fix_test.go`、`cluster_resume_test.go`、`health_test.go`、`node_test.go`、`recover_test.go`、`pkg/grpcstream/api_handler_test.go`、`presence_test.go`（countingBroker 记 occupancy）。
- `metrics_test.go`：presence_failures op 标签 `rewrite`→`gen`/`late`（re编译修复目的之外的指标一致性小改）。
- 文档：`docs/developer/01-architecture.md`、`02-configuration.md`、`04-cluster.md` 去 cluster_emit / ml.type 叙事。

**§8 逐条证据**

1. 仓库无 `Hub.node`、无 `cluster_emit` 热路径、无 `broadcastPublication` 认 `ml.type`：`TestOccupancy_NoForbiddenProductionRemnants` 读 `hub.go`（无 `node *Node`/`ml.type`）、`node.go`（无 `presenceClusterEmit`/`emitPresence`）、`presence_event.go`（无 `ml.type`/`presencePublication`）。人工核对 `git grep` 无残留。
2. Occupancy 不走 `PublishTransient`；Publication handler 看不到 join/leave：`TestPresence_OccupancyNotPublication`（transientChannels 为空）、`TestMemoryBroker_PublishOccupancy_NeverPublication`、`TestRedisBroker_LiveSubscription_OccupancyFollowsInterest`（occupancy 到 occupation handler，pub handler 收不到）。
3. `im.**` 节点收 `im.room.1` 的 join；只订无关频道收不到：`TestPresence_OccupancyWildcardAcrossNodes`（Redis，A 订 `im.**` 收到 exact join；nodeC 只订 `chat.1` 经 `require.Never` 断言收不到）、`TestRedisBroker_LiveSubscription_OccupancyNotInterested`。
4. OccupancyGen 单调；迟到事件丢弃：`TestPresence_OccupancyGenOrderingAndDedupe`（leave.gen>join.gen；重放旧 join 返回 `ErrLateOccupancy` 且不投递）、`TestRedisPresenceStore_NextOccupancyGenIncr`、`TestMemoryBroker_..._InterestGate`。
5. 本机恰好一条不双发：`TestPresence_OccupancySinglePathExactlyOne`（transportA/C 各一条 join，joiner 无，PublishOccupancy 恰一次）+ Redis 版 `TestPresence_OccupancyAcrossRedisExactlyOne` 的 `require.Never`。
6. 未改 A1/A2/A3/A4 热路径：改动仅命中规格书 §2 允许路径；`git status` 无 `authorizer.go`/`interest.go`/`recover.go`/`cluster.go`/`channel_policy.go`。
7. 测试命令绿：见下。

**测试命令与结果**

```
go test ./...            → 全绿（含 Redis 集成，本机 127.0.0.1:6379 可用）
go test -race . ./pkg/redisbroker → 全绿
go build ./... / go vet ./...     → 无输出
```

**偏离（应无）**

无功能偏离。两处范围说明：`pkg/redisbroker/presence_redis_test.go`、`broker_memory_test.go` 为 §2 允许的「必要测试」；`metrics_test.go` 的 `rewrite`→`gen`/`late` 标签序列更新服务于「删 rewrite 路径」的剩余引用（不属编译失败项，属 §2 意图内的一致性改动）。`docs/developer/05-observability.md` 等规格书 §2 未列出的历史文档未改（约束 1）。
