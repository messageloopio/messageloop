# MessageLoop 内核架构重设

| 字段 | 值 |
| --- | --- |
| 文档标题 | MessageLoop 内核架构重设 |
| 日期 | 2026-08-16 |
| 状态 | Draft（2026-08-16 按独立评审修订；语义已钉死，尚未批准实施） |
| 仓库路径 | `docs/design/kernel-architecture.md` |
| 独立评审 | [`kernel-architecture-review.md`](kernel-architecture-review.md) |
| 与 v1.0 关系 | **不替代** [`v1.0-platform-gaps.md`](v1.0-platform-gaps.md)。v1.0 是现行代码上的产品闭环；本文是 v1.x / 下一代内核靶心。 |
| 产品定位 | 双向实时 Messaging Platform（IM / Chat Room / Gaming / IoT）。一条连接上完成 pub/sub、恢复、在场、RPC、Survey。 |
| 已落地行为 | 以源码与 [开发者文档](../developer/README.md) 为准。本文是靶心合同，不是现行实现合同。 |

---

## Overview

MessageLoop 的产品判断成立：传输无关、通配订阅、恢复、session takeover、客户端直发、Survey、频道策略。要改的不是部署拓扑，也不是再拆微服务，而是 **模块缝放错了，故事不圆**。

靶心一句话：

> **可恢复的 Session 贴在 Channel 上；Channel 有日志、有在场、有授权；节点之间只交换位置和幂等命令。**

集群对外不可见：客户端只认 `session_id` + `Position`，能连任意节点、在任意节点 resume。单节点与集群是同一个内核，只换适配器。

2026-08-16 修订吸收了独立评审的 critical / major：钉死 Detached 与 Directory 的关系；跨节点改为水化而非「零拷贝」；Occupancy 唯一走 LiveBus；LiveBus 的 pattern 编译规则；History gap 的可检测子集；deny 语言包含；fencing 提前到现行类型上；混部与 proto cap；Open Questions 与 KD 对齐。

---

## Goals & Non-Goals

### Goals

1. Session 成为对象，Connection 是可撕可贴的 Attachment。本机接管不再在分片里换 `*Client` 指针。
2. 协议动词从巨型 `Client` 中抽出；Session 只做状态机、写队列、心跳、限流。
3. 数据面写成完整合同：日志 + 按兴趣的实时总线 + **可检测子集内** 的 gap；memory 与 Redis 对调用方同一套断言。
4. Occupancy 退出业务 Publication；控制事件带 OccupancyGen；跨节点 **只** 走 LiveBus。
5. 授权合成一个求值器：一种通配方言、一条默认叙事、Admin 是 Capability。
6. 集群收成 Membership + Directory（单激活 + fencing）+ NodeRPC；派生索引不当权威；热路径不嵌投影。
7. 客户端回包足够当权威。SDK 只缓存，不编造。旧 SDK 在 cap 协商前继续走 v1.0 Ack 合同。
8. 十二条宪法成为后续改动的门禁。

### Non-Goals

- 把连接终止、频道日志、在场拆成三个网络服务。
- 消息走 Raft / etcd / Kafka；用 K8s Lease 或服务网格粘滞当所有权。
- 抄 Centrifugo 的 `ns:` / `#uid` / `$`。
- 第一天接第三条日志后端（PG / NATS）。
- 完整 ORSWOT Presence。只留 Occupancy 接口缝，第一实现是租约 + OccupancyGen。
- 在 Go 里再造 actor 运行时。
- 用本文替换 v1.0 发版门禁。
- 内置 JWT/JWKS、Admin HTTP、SSE、一次铺齐新语言 SDK。
- **Redis Cluster / Redis 7 sharded PUB/SUB 作为 LiveBus 第一适配器的目标。** 第一适配器是 standalone Redis。Cluster 上的通配缩放另开里程碑。
- v1.0 RC 之前改 Session 对象或换命令总线。RC 前内核只准：文档、proto **加字段 / reserved**、以及「现行类型上删盲写 Put」（A1，见增量路径）。

---

## Diagnosis

结构裂缝仍在，行为裂缝有一部分已在 v1.0 树上补上。两栏分开，避免实现者重复做 PR-03。

### 结构裂缝（仍在，内核要改）

| 裂缝 | 源码锚点 |
| --- | --- |
| 没有 Session 对象；Hub 存 `*Client` | `hub.go` `ReplaceSession` 扫 64 个 `subShard` 换指针 |
| 三种关闭 + 一条失败回滚 | `Client.close` / `closeQuiet` / `evictSessionForTakeover`；`ReplaceSession` 失败后再 `RemoveSubscription`+`presenceLeave` |
| `statusConnecting` 从未赋值 | `client.go` 只用 `statusConnected` / `statusClosed` |
| 授权四套引擎、三种通配 | ACL last-write + 中段 `**`；Policy first-match；Proxy gobwas `*` 跨点；`adminPrincipal = "admin"` |
| Broker 浅缝 | `Epoch()` / `Ready()` type assert；memory `Publish` 不看订阅且同步回传 handler 错误；Redis `PUBLISH` 失败则 `XDel` |
| 通配 `last` 谎言 | `addWildcardSub` 恒 `true`；Redis `wcCounts` 补锅 |
| 全网 `PSubscribe prefix*` | `pkg/redisbroker/pubsub.go` |
| 控制面借用数据面 | `cluster_emit` + `PublishTransient` + `Hub.node` 认 `ml.type` |
| 盲写 Put 否定 CAS | `syncClusterSessionState` → `PutSessionLease` 无条件 SET；ping/pong 每 10s 走这条 |
| CAS 后 takeover 失败不回滚 | `cluster_resume.go`：旧 node lease 仍在则返回错误，lease 已指向新 incarnation。**旧 node lease 已无时允许继续**——这是死节点旁路，保留 |
| 订阅 saga 嵌 Redis 投影 | `node.go` `AddSubscription` 后两步 |
| 命令总线无签名 | `IssuedBy` 仅审计；能写 Redis 就能注入命令 |
| 失败码三套 | `ACL_DENIED` / `PERMISSION_DENIED` / 未接线的 Disconnect 3507；集群字符串不映射 |
| gRPC 无读 deadline；任意 Write 错变 3512 | `pkg/grpcstream/handler.go`；`client.go` `write` |

### 行为裂缝（v1.0 已补，内核换实现时不要回退）

| 已补 | 锚点 | 内核怎么接 |
| --- | --- | --- |
| Connect / Subscribe 共用 `recoverSubscription` | `recover.go` | 同一 Replayer；再加 gap 与流式消费 |
| Resume 信 `ChannelOffsets`，缺则 skip | `cluster_state.go` + `recordDeliveredOffsets` | 服务端 Position 权威；`docs/developer/04-cluster.md` §4.4「未填充」已落后源码 |
| 一等 `presence_event` + `cluster_emit` 两阶段 | `node.go` `emitPresence` | 换成 LiveBus，混部仍要两阶段 |
| `sessionCoversChannel` | `client.go` | 升为 Coverage |
| 客户端 Survey 异步 + 门闩 | `handleSurvey` | 人数估计来源钉死为 Coverage |
| 服务端 ping + `pingDeadline` | `heartbeat.go` | 续约改为 same-fence Bind，禁止再 Put |

生成元仍是四个：没有 Session 对象；没有单一 Authorizer；数据面合同没写完；控制面借用数据面并盲写所有权。

---

## Domain Glossary

一词一义。协议、存储、指标、日志、SDK 使用同一套名字。

| 词 | 是 | 不是 |
| --- | --- | --- |
| **User** | 鉴权给出的应用身份 | 路由键 |
| **Session** | 服务端颁发的可恢复逻辑连接（进程内对象） | 套接字、现行 `*Client` |
| **SessionDoc** | Session 的可水化快照（跨节点拷贝） | 进程内 `Subs` map 本身 |
| **Attachment** | 一次 WS / gRPC / QUIC 附着 | 会话本身 |
| **Client** | 客户端自报的端 / 设备 ID | Presence 主键、Go 会话类型 |
| **Channel** | 分层寻址名（`.` + `*` / 末尾 `**`） | 房间类型、namespace |
| **Stream** | 频道上的消息日志（可关） | 实时总线 |
| **Position** | `{stream_epoch, offset?}`，`offset` 缺省 = unset | `offset==0` 不再表示「从头」 |
| **Occupancy** | 频道上的在场租约集合 | 一种 Publication |
| **Interest** | 本节点对精确频道或 **可编译 pattern** 的引用计数 | 客户端订阅 |
| **Coverage** | 会话的精确订阅 ∪ 通配命中某精确频道 | 只认精确键的 `hasSubscription` |
| **StreamEpoch** | 日志世代 | 节点世代 |
| **OccupancyGen** | 每频道单调世代（见发号规则） | 日志 offset |
| **Fencing** | `(node_id, node_epoch, version)` | StreamEpoch |
| **Capability** | Admin / 后端特权位的闭集 | 假用户 `"admin"` |
| **Directory** | session → (incarnation, fencing) | 频道列表、在场集合 |
| **Membership** | 哪些 incarnation 还活着 | 某个 session 在哪 |

### 三把时钟：发号规则

| 钟 | 发号 | 何时 +1 | 比较 |
| --- | --- | --- | --- |
| **StreamEpoch** | memory：进程启动 UUID；Redis：`ml:broker:epoch` SET NX | 日志世代失效时（memory 重启；Redis 删 key 后重建） | 字符串相等则同一世代 |
| **OccupancyGen** | 每频道一个计数器。memory：进程内 `uint64`；Redis：`INCR ml:occ:gen:{ch}` | 每次 Join、Leave、合成 leave 各取一个新值 | 迟到事件 `gen <= last_applied[ch][session]` 丢弃 |
| **Fencing.node_epoch** | **只准** Redis `INCR ml:cluster:node_epoch:{node_id}`（单节点 memory：进程内计数器）。禁止本地随机 UUID 当 epoch | 进程启动取一次 | 全序：更大的 epoch 更新 |
| **Fencing.version** | 会话本地 + Directory | **仅 Bind 抢权时 +1**。续约不变 | 与 node_id、node_epoch 一起比 |

### 四个问句，四份集合

| 问句 | 只看 |
| --- | --- |
| 这条 Publication 该不该给这个会话？ | **Coverage** |
| 谁在这个频道里（在场）？ | **Occupancy** |
| 本节点要不要拆这条总线消息？ | **Interest** |
| 这个 Session 此刻归谁？ | **Directory** |

禁止用 A 的集合回答 B 的问句。

---

## Target Architecture

部署拓扑不变：相同节点 + 数据面（memory 或 standalone Redis）+ 可选控制面（默认 Redis）。不拆微服务。不在第一天做 edge/core。

```
                 Attachment (WS | gRPC | QUIC)
                           │ attach / detach
                 ┌─────────▼─────────┐
                 │      Session       │
                 │  写队列分 Control/ │
                 │  Data；心跳；限流   │
                 └─────────┬─────────┘
                           │ verb
          ┌────────────────┼────────────────┐
          ▼                ▼                ▼
   ChannelService     Authorizer        Cluster
   订阅 / 扇出          一种通配           Membership
   Replayer            一个 Decide        Directory
          │            Capability         NodeRPC
          ├─ StreamLog
          ├─ LiveBus     （精确 + 可编译 pattern）
          └─ Occupancy   （事件只走 LiveBus）
```

硬规则：

- `session` 不准 import `cluster` 实现。Cluster 通过 Directory / RPC 回调。
- Transport 不准认识 Channel / Occupancy。
- 注册表不准持有 `node` 回指针，不准识别 `ml.type`。
- 订阅热路径不准写集群投影。
- Session **单激活**；Stream **无家**；Occupancy **不为频道做 Directory Bind**。Occupancy 事件的跨节点投递复用 LiveBus 的 Interest，不另建家。

```
cmd/server
internal/runtime
internal/session
internal/protocol
internal/channel           含 Interest
internal/stream            StreamLog + LiveBus
internal/occupancy
internal/authz
internal/rpc
internal/survey
internal/cluster
internal/admin
pkg/transport/{ws,grpc,quic}
pkg/topics
shared/
```

允许一段时间根包 re-export 旧名字（KD-K26）。

---

## Session Plane

### 对象

```text
Session
  ID, User, Client, Fencing
  Subs: channel → SubState{ephemeral, position, token}
  Out: SendQueue{Control, Data}
  State: Authenticating | Attached | Detached | Closed

Attachment
  Transport + Marshaler + 读循环

SessionDoc          // 跨节点唯一合法拷贝
  session_id, user, client, authenticated, protocol
  subscriptions[] {channel, ephemeral, token_ref}
  positions[]     {channel, Position}   // 仅精确频道、曾成功投递历史
  stream_epoch
```

**本机接管**：Detach 旧附件 + Attach 新附件，`Subs` 指针稳定。禁止扫分片换 `*Client`。

**跨节点 resume**：Directory 只存 fencing。新节点 `Hydrate(SessionDoc)` 得到新 Session 对象，一次登记 Coverage / Interest / Occupancy（按 shouldTrack）。写队列 **丢弃**，靠 replay 补数据。这不是零拷贝，也不是 grain 迁移。

### 状态机（钉死）

`Detached` **只**表示：本进程仍持有 Session 对象、Directory **仍认本 fencing**、附件已撕。被抢节点 **不准** 进入 Detached。

| 状态 | Directory | 附件 | 含义 |
| --- | --- | --- | --- |
| Authenticating | 尚未 Bind 成功 | 有 | Connect 进行中 |
| Attached | 本 fencing 是 owner | 有 | 正常服务 |
| Detached | 本 fencing 仍是 owner | 无 | 仅本机交接窗口 |
| Closed | 不得再 Unbind 别人 | 无 | 终态 |

### 事件 × 状态

| 事件 | Directory | Occupancy | 指标 | 下一状态 |
| --- | --- | --- | --- | --- |
| 本机接管：旧附件 | 仍占（同一 fencing，version 可 +1 后仍是自己） | 不 Leave | 过户，不 Dec 两次 | 旧：Closed（对象丢弃）；新：Attached |
| 跨节点：新节点 Bind 成功 | 新 fencing 成为唯一 owner | 暂不改（旧节点尚未 Fence） | 新连接稍后 Inc | 新：Authenticating→Attached（Hydrate 后） |
| 跨节点：旧节点收到 Evict 或写路径 fence | **不许 Unbind** | 不 Leave | 不 Dec 新 owner | **直接 Closed**（Fence） |
| 真走 / 空闲 / 客户端关 | Unbind | Leave | Dec | Closed |
| Bind 失败（冲突） | 不动别人 | 不 Join | 不 Inc | 本连接 Closed |
| 本机 `Attach` 失败（旧已 Detach） | Unbind + 清本地 | Leave（按真走） | Dec | Closed — 避免「Directory 占着、没附件」的空洞。这是现行 `ReplaceSession` 失败回滚的归宿 |

空闲踢人、客户端关闭、限额、鉴权失败：走 **真走**（Leave + Unbind），不是 Fence。

Fence 之后旧对象上的 Publish / Subscribe / Survey / 集群命令一律硬失败或丢弃，**不等** Evict 送到。写路径每次关键副作用前比 fencing（至少 Bind 续约失败、以及出站前一次检查）。不是只有 takeover 命令才比 version。

### 写队列（同一张表）

单条流无法字节插队。Control 优先 = **下一帧选择 Control**。

| 项 | 值 |
| --- | --- |
| Control 深度 | 32 帧（Ping/Pong/Ack/Disconnect/RecoverComplete） |
| Data 深度 | 256 帧（Publication / replay） |
| 单帧上限 | `MaxMessageSize`（默认 64 KiB）出站同样适用；恢复流按帧切，禁止再把 1000 条塞进一帧 |
| Data 满 | 3512 SlowConsumer，不断开后丢数据 |
| Control 满 | 视为对端已死，3512，关附件 |
| 写超时 | 默认 10s，三条传输同一配置 |
| 读 / 探活 | 由同一 Heartbeat 配置推导；gRPC 必须有等价探活（idle/ping），不得只靠无限 `Recv` |

错误映射：

| 传输现象 | 码 |
| --- | --- |
| `io.EOF`、gRPC `Canceled`/`Unavailable`（对端取消）、WS close 1000/1001、QUIC 应用关 | `peer_closed`（Disconnect 3000，不是 3512） |
| 写超时、Data 队列满、写阻塞超过 write_timeout | `slow_consumer`（3512） |
| idle / 未应答 ping | `idle`（3511） |
| 本进程主动 Drain | `force_no_reconnect`（3503） |

### 心跳与 Directory 续约

默认 idle 300s，服务端 ping 默认关。未应答 ping 在 `ping_timeout` 断开（策略 B）。

只回 Pong 必须续 Occupancy 租约，以及 Directory：**`Bind(session, sameFencing)` 或 no-op**。禁止 `PutSessionLease` 无条件 SET。Bind 返回 fence → 本地 Fence → Closed。

验收：节点 B CAS/Bind 抢走后，节点 A 的 ping 续约 **不得** 把 lease 写回 A。

---

## Protocol

信封模型保留。传输协商保留。

### Connect

```
Connect  { token, session_id?, caps[], subscriptions[] }
Connected { session_id, stream_epoch, accepted_caps, resume, recover_results? }
```

匿名忽略 `session_id`。Resume 仅当鉴权成功。

`Connect.subscriptions` 失败策略保留 KD-9：**单频道授权失败 = 该频道软失败，其余仍订**；恢复失败不撤订阅。整批回滚只发生在 Bind / Hydrate 失败（硬失败）。

### 能力协商（闭集）

| cap | 含义 | 服务端未协商时 |
| --- | --- | --- |
| `recover.v1` | Ack 内嵌 publications + `RecoverResult`（现行 v1.0） | 默认；旧 SDK |
| `recover.v2` | 流式 `Publication{replay}` + `RecoverComplete{position,truncated,gap}` | 不发流；走 v1 |
| `recover.gap` | 理解 `gap` / `gap_reason` | 字段可加，旧客户端忽略 |
| `occupancy.gen` | 理解 PresenceEvent.gen | 仍投递事件，旧客户端忽略 gen |
| `server_ping` | 理解 Outbound Ping | `ping_interval` 必须保持 0，否则会误杀 |

未列出的 cap 忽略。服务端 `accepted_caps` 是交集。

### 恢复

```
SubscribeAck      { ok, recover: pending | skipped | failed }   // v2
Publication       { replay=true, position }                    // v2
RecoverComplete   { position, truncated, gap, gap_reason }     // v2

// v1 并存
RecoverResult     { recovered, truncated, offset, epoch, error }
Connected/SubscribeAck.publications
```

- 权威是服务端为该 Session 记下的 Position。客户端 offset 只是提示。
- Resume / 重订 / 动态订阅同一 Replayer。
- 「从头」= 显式 `fresh=true` 或 StreamEpoch 重置。**不是** `offset==0`。
- Position wire（新 message，字段号在 A0 冻结）：

```
message Position {
  string stream_epoch = 1;
  optional uint64 offset = 2; // 缺省 = unset
}
```

- `gap` 语义见 Stream 节。v1 客户端：空批必须继续带 `recovered`/`truncated`（v1.0 合同），**禁止**靠把 `publications` 留空假装追上同时又宣称回包即权威。
- 恢复失败不撤订阅。

### 失败两层

| 层 | 形态 | 典型 |
| --- | --- | --- |
| 软失败 | 信封 `Error`，连接还在 | 授权、限流、恢复失败、策略拒绝、Survey 门 |
| 硬失败 | `Disconnect` | 鉴权门、限额、空闲、慢消费者、fencing 失败、Bind/Hydrate 失败 |

一份码表。权限软失败统一 `PERMISSION_DENIED`（type=`acl_error` 或 `policy_error`）。废弃并行的 `ACL_DENIED` 作为新合同（旧码可映射一个版本）。集群内部状态不泄漏：对客户端是 `DisconnectStale` 或再 resume。

文档无触发点的 Disconnect 码不写进新合同。

### 回包即权威

对 **accepted_caps 内** 的客户端：频道列表含 ephemeral、Position、recover 结果、resumed 必须可从回包重建。SDK 只缓存。

---

## Channel Plane

```text
Subscribe(session, spec) → Sub
Unsubscribe(session, ch)
Publish(session, msg) → Position
Fanout(ch, env)                      // 只被 LiveBus 调
```

`history=false` / `TransientOnly` / 客户端 `transient`：ChannelService **改走 Live**，Ack offset 视为无 Position（unset），不返回「拒绝」。这与现行 `handlePublish` 一致。`Node.Publish` 管理面若要求写历史而策略禁止 → 软错误 `ErrHistoryDisabled`（现行行为保留）。

跨节点 Hydrate **一次**登记 Coverage/Interest/Occupancy，禁止逐频道 saga。Interest 引用计数仍按频道/pattern 聚合。

### StreamLog

```text
Position { stream_epoch, offset? }

StreamLog
  Append(ch, msg) → Position
  History(ch, from Position, n) → { messages, truncated, gap, gap_reason, first_retained? }
```

**Publish 成功 = 日志已接受（若本应写日志）。** 投递失败不否定发布。禁止 Redis 在 `PUBLISH` 失败后 `XDel` 已写入的 Stream 条目。memory 不得把 handler 错误从 `Append` 返回。

验收测试（KD-K14）：

1. handler panic / 本地 Fanout 失败后，`Append` 仍返回 Position。
2. Redis `PUBLISH` 失败不得 `XDel`。
3. `SetInterest` 为 false 时 memory **不得**调 handler。
4. 同一 Recover 断言在 memory 与 Redis 上对 `gap` 的 **可检测子集** 一致。

### Gap 合同（可检测子集）

`ts<<20|seq` **不是**稠密序号。第一实现 **不**承诺检测「中间被 XDEL 掉的单洞」。

| `gap_reason` | 何时为真 |
| --- | --- |
| `none` | 从 `from`（或 unset=头）读到的是连续保留前缀 |
| `head_trimmed` | `from` 已设置，且 `first_retained > from`（memory：环最旧 > from；Redis：适配器持久化的 `first_retained` / trim generation，在 MAXLEN 与 TTL 滑动时更新） |
| `empty_expired` | `from` 已设置，流为空，且曾有过 `first_retained`（整段蒸发）。从未发布过的频道：`gap=false`，空批，unset 游标 |
| `epoch_reset` | StreamEpoch 与 `from.stream_epoch` 不同 |

禁止：`RecoverOK ∧ 空批 ∧ gap=none` 用在 `from` 已设置且适配器无法证明「保留区仍覆盖 from」的时候。证明不了就标 `head_trimmed` 或 `empty_expired`，宁可假阳性。

后续可选：Stream 条目旁存稠密 `seq`，再承诺中洞。那是独立里程碑，不是第一刀。

### LiveBus

```text
LiveBus
  Publish(ch, env)           // 永不入日志；ch 必须是精确频道
  SetInterest(key, on)       // key = 精确频道或可编译 pattern
  OnMessage(func(ch, env))
```

`Publish` 投递给：**对该精确频道有 Interest，或持有匹配该频道的可编译 pattern Interest** 的节点。然后节点内按 Coverage 扇出到 Session。

#### Pattern → Redis 编译（冻结）

只允许两种 Interest key：

1. **精确频道**：standalone `SUBSCRIBE live:{ch}`。将来 Cluster 可用 `SSUBSCRIBE`。
2. **字面前缀 + 末尾单段 `*` 或末尾 `**`**：例如 `im.**`、`im.room.*`。standalone 订 `PSUBSCRIBE live:{literal_prefix}*`，节点上再用 `topics.Match` 丢掉过匹配（Redis `*` 跨点）。`im.**` 还必须额外精确订 `live:im`（`**` 含零段）。

**拒绝**（`SetInterest` 返回错误，订阅软失败）：`*.room`、`im.*.tick`、中段 `*` 后还有字面、裸 `**`。客户端订这些 pattern：`SubscribeAck` 该频道 `PERMISSION_DENIED` 或 `BAD_REQUEST`（`pattern_not_routable`），不断连。

裸 `**` 等于全网 `*`，KD-K13 禁止。需要「订一切」用 Admin Capability，不走客户端通配。

第一适配器 **standalone Redis**。Redis Cluster + sharded PUB/SUB **没有** pattern subscribe，列为非目标。Cluster 上的通配要么拒绝，要么「收该前缀所在平面 + 本地过滤」——另开里程碑，不得声称「加节点后入站 ≈ 本节点兴趣」已在 Cluster 上成立。

缓冲满：指标 `live_drop_total`；该节点对该频道标降级；**禁止静默丢而不计数**。满时优先丢 Occupancy 事件（可靠下一快照补），业务 Publication 满则对该节点 Interest 视为短暂失败（不否定 Append）。

### Occupancy

默认：成员租约 + OccupancyGen。万人频道策略关掉。

```text
Occupancy
  Join(ch, session, user, client) → gen
  Leave(ch, session, gen)
  Snapshot(ch, cap) → {clients[:cap], occupancy, truncated}
```

规则：

- 只登记精确、非 ephemeral、Effects.Occupancy 的订阅。
- 通配者不进 store；通过 Coverage 收被命中精确频道的事件。
- 自己不收 self-join/leave；快照可含自己。
- 过期合成 leave：取新 OccupancyGen，事件带该 gen。
- 事件 **不进 Stream、不是 Publication 信封**。
- 遗留 `ch/__presence` 仅策略打开。

**跨节点唯一投递面：LiveBus。** `Occupancy.Join/Leave` 之后 `LiveBus.Publish(exactCh, PresenceEnv)`。禁止 `PublishTransient` + `ml.type`，禁止并行 NodeRPC 广播，禁止「或」。节点 B 只有 `im.**` 的 Interest 时，靠 LiveBus 的 pattern 编译收到 `im.room.1`。

混部：与 `cluster_emit` 相同纪律。默认关新通道；节点 lease 上报 `occupancy_schema=livebus`；Membership 最小版本未齐之前，新节点仍走本机 `deliverPresenceEvent`（单节点 / 旧舰队）；齐了再开 LiveBus emit。禁止双写业务频道。

---

## Authorizer

一种通配，与 `pkg/topics` **同一函数**（`*` 单段，`**` 仅末段）。取消 ACL 中段 `**`、Proxy gobwas 跨点 `*` 作为授权语言。Proxy 仍可按自己的 glob **选后端**，但 **允许/拒绝** 必须流回 `Decide`，不得短路整层。

```text
Decide(principal, action, channel) → Decision{Allow, Reason, Effects}

principal = User | Admin
action    = SubscribePattern | Publish | Recover | Presence | Survey | 下列 Capability
```

求值：`DenyAll → 显式 deny → Proxy 输入（若路由命中）→ 显式 allow → 默认`。

### 默认叙事

| 类 | 默认 | 例子 |
| --- | --- | --- |
| 投递类 | 允许，用规则收紧 | 订阅、发布、收消息 |
| 放大类 | 拒绝，用规则打开 | 客户端 Survey、按 user 扇出 |
| 控制类 | 拒绝，除非已鉴权 Session | resume、takeover |

频道前缀策略是规则的 **Effects**，不是平行 first-match 引擎。YAML 兼容期：旧 `acl.rules` + `channel_policies` 只读编译进同一张表（pattern → allow/deny/effects）。新配置只写一张表。

### 授权主语

| Action | 主语 |
| --- | --- |
| SubscribePattern | 订的名字。见下方语言包含 |
| Publish | 精确频道。**不要求 Coverage**（KD-K21）。Admin 同规则 |
| Presence / Survey | 精确频道。必须 Coverage，再对该频道授权 |
| Recover | 精确频道 + Effects.Recover；通配 skip |

### SubscribePattern 语言包含（可计算）

`AllowLang(principal)` = 默认允许的语言减去所有对该 principal 生效的 deny 规则（含 DenyAll 与空 allow 列表）。

```
Decide(principal, SubscribePattern, p) = Allow
  iff  L(p) ⊆ AllowLang(principal)
```

`L(p)` 是 matcher 语义下 p 匹配的精确频道集合（无穷集，只做包含判定，不枚举）。

判定（`*` 单段、`**` 仅末段）：

1. 将 p 与每条 deny 规则 d 求交。若 `L(p) ∩ L(d)` 非空且 d 拒绝该 principal → Deny。
2. Proxy 动态拒绝 **不进入** AllowLang（否则 TOCTOU）。Proxy 在订阅当时再跑一次，失败只拒这一次请求。
3. 默认允许时：存在任意会命中 p 的 deny → p 整条拒绝。因此有 `secret.**` DenyAll 时，客户端 `**` / `im.**`（若与 secret 相交）被拒。`im.**` 与 `secret.**` 不相交则仍可订。

表驱动最小集（实现必须锁测试）：

| p | deny | 结果 |
| --- | --- | --- |
| `im.**` | `secret.**` DenyAll | Allow（不相交） |
| `**` | `secret.**` DenyAll | Deny |
| `im.*` | `im.secret` DenyAll | Deny（`im.secret` ∈ L(`im.*`)） |
| `im.room.*` | `im.**` DenyAll | Deny |
| `chat.**` | 无 | Allow（默认） |
| `*.room` | — | `pattern_not_routable`（LiveBus 拒绝，先于 ACL） |

SubRefresh / 规则热更新：对每个已订 **pattern** 整条重算，失败则整条 Unsubscribe + 精确覆盖频道按 shouldTrack Leave。不按精确频道拆 pattern。

### Capability 闭集

| 位 | 允许 |
| --- | --- |
| `presence.large_snapshot` | 快照超过客户端 cap |
| `survey.bypass_gate` | 跳过人数门、CanSurvey、客户端 in-flight |
| `history.read` | GetHistory，主语 = Capability + 频道，**不走 Coverage** |
| `presence.read` | GetPresence，同上 |
| `channels.list` | GetChannels（派生视图，可脏） |
| `session.act` | 按 session 投递 / 断开 / 订阅 |
| `user.fanout` | 按 user 展开后走 `session.act` |
| `subscribe.any` | Admin 代订；**仍不得**把 ephemeral 写成 Occupancy 成员 |
| `pattern.global` | 持有裸 `**` Interest（仅节点内部 / Admin，不给普通客户端） |

Admin GetHistory / GetPresence 无 Session Coverage；可见性 = 位 + 频道 Decide(Admin, Recover|Presence, ch)。未持有对应位 → 软失败，不得旁路。

RPC 无匹配代理 → 软失败 `NO_PROXY`，不再 echo。

---

## Cluster

```text
Membership
  Alive() []Incarnation
  OnLeave(func(Incarnation))

Directory
  Bind(session, fencing) → ok | fence
  Locate(session) → (incarnation, fencing)
  Unbind(session, fencing)            // fencing 不对则 no-op

NodeRPC
  Call(incarnation, cmd) → Result     // 至少一次 + 幂等
```

单节点：Directory = 进程内 map；RPC = 函数调用；Membership = 就我一个。

**唯一硬不变量：** 任意时刻一个 Session 至多一个活 Attachment。

### Bind / Evict

```text
1. Bind(session, newFencing) 成功     // 世界只认新 fencing
2. 至少一次 Evict(old, newFencing)
3. 旧附件 Fence → Closed；丢了 Evict 也无害——再写会被拒
4. 新节点 Hydrate(SessionDoc) + Attach；replay
```

刷新所有权 = `Bind(session, sameFencing)`。**删除** `PutSessionLease`。死节点旁路保留：Locate 到的 incarnation 已不在 Alive，允许直接 Bind，不必 Evict 成功。

### Membership 第一适配器

Redis **没有**可靠的过期推送。第一实现：带抖动的短周期 SCAN（默认 5s±jitter）驱动 `OnLeave`。这是控制面循环，不是热路径，不违反「热路径无 if cluster」。

OnLeave(inc) → 批量作废该 incarnation 名下 session 的 fencing（视为可 Bind），**不必等** 会话 TTL（现 600s）。宽限期 = 一次 SCAN 周期。

### NodeRPC

**正确性先行、换总线随后（KD-K6 分两步）：**

1. **现行 pubsub 上**：fencing + HMAC + 超时 ≥ handler。不换通道也能修双活。
2. **再**迁 Redis Stream + 每 incarnation 一个 consumer group。

HMAC（一步到位写进合同，两步都遵守）：

- 密钥在 **节点配置**（环境 / 文件），**不进 Redis**。新节点随部署注入。
- 共享集群密钥。被盗 = 能伪造命令；仍要求 Redis 网络隔离。HMAC 把「能写 Redis ⇒ 能注入」收成「能写 Redis **且** 持有密钥」。
- 签：`Type, SessionID, TargetIncarnationID, Fencing, Payload-hash, CommandID, IssuedAt`。
- `IssuedAt` 允许 ±30s 时钟偏斜。
- `command_id` 去重 TTL ≥ `max(handler_timeout, survey_timeout)`（默认 ≥ 15s）。
- 新二进制拒绝未签名。Membership `command_schema=hmac` 未齐时，旧节点仍收未签名；齐后拒绝。
- Directory Bind 的 Redis 账号与业务数据面分离（Redis ACL）。HMAC 不保护 Directory 写入；专用账号才是。

Survey / 长调用：意图与应答分开；发送方等待 ≥ handler 超时。人数预检来源 = 各节点 **Coverage** 计数之和（`count_only`），允许偏高，必须声明。不是 Occupancy，不是投影。

### 派生视图

投影与 user→sessions **退出热路径**。user 索引 = 成员键 + TTL，repair **能修剪**。展开按 user：本机 ∪ 索引，每条再查 Directory 且 User 匹配。**一个** SCAN 修复器重建全部派生。

### 混部

节点 lease 上报：`command_schema`、`occupancy_schema`、`kernel_caps`。

| 开关 | 默认 | 打开条件 |
| --- | --- | --- |
| 删盲写 Put（A1） | 新二进制即删 | 旧二进制仍 Put → **在全舰队升级 A1 之前，新节点 Bind 可能被旧 ping 抢回**。因此 A1 是 **旗舰齐步** 或「先关旧节点 ping 续约」。文档标为 **breaking 运维步骤** |
| Occupancy LiveBus emit | 关 | 全部 incarnation `occupancy_schema=livebus` |
| NodeRPC Stream | 关 | 全部 `command_schema=stream`；在此之前 HMAC+pubsub |
| recover.v2 | 按客户端 cap | 旧 SDK 永远 v1 |

跨 schema 的 Survey / Evict：发到旧 incarnation 用旧通道；新节点不向未声明 Stream 的目标发 Stream。

### 观测

- `session_dual_activation_seconds`（目标 0）
- `bind_fenced_total`、`bind_refresh_fail_total`、`evict_lag`
- `recovery_gap_total{reason}`
- `live_drop_total`、`occupancy_gen_discard_total`
- takeover trace：`Bind → Evict → Hydrate → Replay`

### 多区域

Session 与 Stream 同区域。**跨区域 Directory Bind 直接禁止。** 只同步 user→区域。user 索引从第一天成员键 + TTL。

---

## Admin

同一套服务 + Capability，不是平行投递实现。

允许：大快照、bypass Survey 门、按 user 展开。

不允许：第二套 payload 转换；把 ephemeral 写成 Occupancy；未持有 `history.read` / `presence.read` 读数据面。

独立 listener + bearer 是运维边界，不是第二套内核。

---

## Consistency Constitution

1. 一词一义。三把时钟分名、分发号规则。
2. 一问一集合。
3. 一种通配、一个 Decide、一条默认叙事。
4. 一个游标权威。Position 能 unset。Replayer 不分 Connect/Subscribe。
5. 一个所有权协议。只有 Bind / Unbind / Fence。没有盲写。
6. 操作成对。本机交接 Detach；被抢只准 Fence；真走 Leave+Unbind。
7. 控制事件不进 Stream，不进 Publication。Occupancy 跨节点只走 LiveBus。
8. 适配器可替换。memory ≡ Redis 在 **已写明的断言** 上（含 gap 可检测子集，不含 Redis Cluster 通配缩放）。
9. 失败两层。一份码表。
10. 单节点 ≅ 集群。热路径不出现 `if cluster { redis }`。控制面 SCAN 循环可以有。
11. 回给客户端的就是权威——在 **accepted_caps** 内。旧 cap 走 v1.0 合同。
12. 文档与代码同 PR。未实现字段不得写成已实现。

### 对称表

| 动作 | 本地 | 集群 | 失败 |
| --- | --- | --- | --- |
| 新连接 | Attach + Bind | 同左 | Bind 冲突 → 该连接 Closed |
| 本机恢复 | Detach 旧附件，Attach 新的 | Directory 仍是自己 | Attach 失败 → 真走回滚 |
| 跨节点恢复 | Hydrate(SessionDoc) | Bind 新 fencing + Evict | 旧节点 Fence；对话失败无害 |
| 订阅 | Coverage + Interest，经授权 | 不写 Directory | 授权 / 不可路由 pattern：软失败 |
| 发布 | Append 和/或 Live | Live 按 Interest | 日志失败软失败；投递不否定发布 |
| 进场 | Occupancy.Join + LiveBus | 同左 | store 失败不撤订阅，必须可见 |
| 真走 | Leave + Unbind | 同左 | 与 Join 对称 |
| 被抢 | Fence → Closed | 不得 Unbind | 再写被拒 |
| Survey | Coverage ∧ 放大授权 | NodeRPC；人数=Coverage 估计 | 门在发请求前；超时软失败 |
| Admin | 同服务 + Capability | 同 RPC | 不得写出客户端无法解释的状态 |

---

## Incremental Path

v1.0 RC 前：只做文档、proto 加字段、以及 **A1（旗舰齐步）**。Session 拆对象与换总线放到 RC 之后 / v1.x。

每步可独立合并、可回滚（`kernel.<step>` flag，默认关，A1 除外——A1 是删错误 API）。回归测试须对旧代码会红。

| 步 | 内容 | 完成标准 | 混部 |
| --- | --- | --- | --- |
| **A0** | proto：`Position`、`RecoverComplete`、`gap_reason`、PresenceEvent.gen、Connect.caps；字段号冻结 | 生成代码；旧客户端忽略新字段 | 只加字段 |
| **A1** | **现行类型上**删除刷新 Put；续约 same-fence Bind/CAS；CAS 失败且对方仍活则回滚；写路径认 fence | 测试：B CAS 后 A ping 不得抢回；`PutSessionLease` 退出刷新路径 | **齐步升级**或先关旧续约。见上 |
| **A2** | History 可检测 gap；停 XDel-on-PUBLISH-fail；Publish 成功合同；memory 看 Interest | 四条验收测试绿 | 无协议破 |
| **A3** | Interest 自己计数；去掉通配 last 谎言；LiveBus 编译规则（精确 + 前缀 `*`/`**`，拒绝其余） | memory 与 Redis 同一套 interested 断言；不可路由 pattern 有测试 | 旧节点仍 PSubscribe `*`；新节点可先双订 |
| **A4** | Authorizer 一种通配 + 语言包含；Capability 闭集；YAML 兼容编译 | 表驱动 deny 用例全绿 | 旧 YAML 只读编译 |
| **B1** | 本机 Session/Attachment + 写队列 + 状态机 | 本机接管不扫分片换指针；错误映射表有测试 | 对象仍可从 `*Client` 迁 |
| **B2** | Occupancy 事件走 LiveBus；OccupancyGen | `Hub.node` / `cluster_emit` 可关 | lease `occupancy_schema` 齐后开 emit |
| **B3** | `recover.v2` 流式消费（写队列已在） | 新 SDK 一条路径；旧 cap 仍 v1 | cap 协商 |
| **B4** | NodeRPC → Stream；repair 合一；迁 `internal/cluster` | `command_schema=stream` 齐后切；HMAC 已在 A1/总线第一步 | 双通道直到齐 |

确定性模拟（KD-K20）是 B4 之后的独立里程碑，不是 B4 附赠。

---

## Key Decisions

| ID | 决定 | 理由 |
| --- | --- | --- |
| KD-K1 | 单进程内核 + 可选 Redis，不拆微服务 | 实时多一跳是惩罚 |
| KD-K2 | Session 是对象；本机 Attach/Detach 指针稳定 | 消掉分片换指针 |
| KD-K3 | 唯一硬不变量：Session 单激活 | 其余派生 |
| KD-K4 | 刷新 = same-fence Bind；删除盲写 Put | 现行 Put 否定 CAS |
| KD-K5 | 先 Bind 再 Evict；被抢只准 Fence | 对话失败不得破坏新 owner |
| KD-K6 | NodeRPC 至少一次 + 幂等。先在现行总线补 fencing/HMAC，再迁 Stream | 正确性不绑在换通道上 |
| KD-K7 | Stream 无家；Occupancy 不 Bind 频道 | pub/sub 要无家 |
| KD-K8 | Occupancy = 租约 + OccupancyGen；不上 ORSWOT | 对局/IM 要进出序 |
| KD-K9 | 控制事件禁止进 Publication/Stream | 消灭 cluster_emit |
| KD-K9b | Occupancy 跨节点 **只** 走 LiveBus | 评审 C4：禁止「或」 |
| KD-K10 | 一种通配、一个 Decide、一条默认叙事 | 消灭十个谓词 |
| KD-K11 | 游标权威在服务端 Position；unset ≠ 从头 | 消灭两套权威 |
| KD-K12 | History 报告 **可检测** gap；中洞第一版不承诺 | `ts<<20\|seq` 做不到诚实中洞 |
| KD-K13 | LiveBus：精确 + 可编译前缀 pattern；拒绝其余；禁止裸 `**` | Redis 无法按兴趣订任意 pattern |
| KD-K13b | LiveBus 第一适配器 = standalone Redis；Cluster/sharded 非目标 | SSUBSCRIBE 无 pattern |
| KD-K14 | memory ≡ Redis 在已列验收测试上 | 浅缝变深缝 |
| KD-K15 | Admin = 同核 + Capability 闭集 | 消灭平行协议 |
| KD-K16 | 回包即权威限于 accepted_caps；v1 保持 v1.0 Recover 合同 | 旧字段留空会假装追上 |
| KD-K17 | 失败两层、一份码表 | 消灭三套码 |
| KD-K18 | 默认 idle 300s、服务端 ping 默认关 | 不误杀旧 SDK |
| KD-K19 | 不替代 v1.0；RC 前不拆 Session 对象、不换总线 | 一人 14 周排期 |
| KD-K20 | 确定性模拟独立里程碑 | 锁 fencing |
| KD-K21 | 发布不要求 Coverage；Admin 同规则 | 现行行为；IoT/系统通知 |
| KD-K22 | 恢复「从头」仅 fresh 或 epoch 重置；字段与 v2 同一 proto 小版本加入，流式消费等 B3 | 收口原 Q3 |
| KD-K23 | `history=false` 时 Publish 改 Live，不硬拒客户端 | 对齐 handlePublish |
| KD-K24 | Survey 人数 = Coverage 估计，允许偏高 | 一问一集合 |
| KD-K25 | 跨区域 Directory Bind 禁止 | 不堵死多区域时先划界 |
| KD-K26 | 允许根包 re-export 过渡 | 避免一次改完 import |
| KD-K27 | `node_epoch` 只准 Redis/内存单调 INCR | 禁止随机 UUID 当世代 |
| KD-K28 | A1 是齐步运维步骤 | 旧 Put 会破坏新 Bind |
| KD-K29 | HMAC 密钥在节点配置，不进 Redis | 能写 Redis 不再自动等于能签命令 |
| KD-K30 | 死节点（Alive 无该 incarnation）允许直接 Bind | 保留现行旁路 |

---

## Open Questions

实施不得再把已决 KD 当选择题。仍开放的只有产品/运维偏好：

| # | 问题 | 选项 |
| --- | --- | --- |
| Q6 | A1 齐步方式 | (a) 维护窗口全升；(b) 先配旧节点停 ping 续约再滚新二进制 |
| Q7 | 不可路由 pattern（`*.room`）对客户端 | (a) `BAD_REQUEST` / `pattern_not_routable`（倾向）；(b) 降级为该节点全量收 + 本地过滤（破坏 KD-K13 缩放承诺，须打黄金指标） |
| Q8 | 稠密 seq 旁路（真中洞检测）放哪个里程碑 | 默认 v1.x+，不挡 A2 |

已关闭：原 Q1→KD-K21；Q2→Non-Goals/KD-K8；Q3→KD-K22；Q4→KD-K6；Q5→KD-K26。

---

## What We Steal, What We Refuse

| 来源 | 偷 | 不偷 |
| --- | --- | --- |
| Orleans | 单激活 + Directory | actor 运行时 |
| Phoenix Presence | 在场是带世代的集合 | 控制面改 gossip |
| Centrifugo | Engine 拆分；sharded PUB/SUB 作 **未来** 精确频道选项 | `ns:`；第一天 Cluster |
| Durable Objects | SessionDoc 可水化；Attachment 可撕 | 跨节点零拷贝；频道必须有家 |

拒绝：Raft/Kafka 运消息、服务网格当所有权、K8s Operator 当目录、一切皆 actor、拆连接/频道服务。

机制备选（已否决）：

| 机制 | 采用 | 否决 |
| --- | --- | --- |
| Occupancy 投递 | LiveBus | 全节点 NodeRPC（缩放）；独立兴趣目录当权威（派生装权威） |
| Redis Position | 可检测子集 + first_retained | 在 `ts<<20\|seq` 上假装中洞 |
| 第 1 把正确性刀 | 现行类型上删 Put（A1） | 先拆 Session 对象 |
| 通配缩放 | 可编译前缀；拒绝其余 | 裸 `**`；暗示 `*.room` 能按兴趣缩放 |

优雅三问仍作自检：一段话说明会话/消息/死节点；删控制面内核不变；客户端不知节点名。

---

## Related Documents

- [独立评审](kernel-architecture-review.md)
- [v1.0 功能缺口](v1.0-platform-gaps.md)
- [ROADMAP](../../ROADMAP.md)
- [现行架构](../developer/01-architecture.md) — 现状
- [集群指南](../developer/04-cluster.md) — 现状；§4.4 ChannelOffsets「未填充」落后源码
- [客户端协议](../protocol.md) — 现行 v1.0 合同

---

## Document History

| 日期 | 说明 |
| --- | --- |
| 2026-08-15 | 初稿 |
| 2026-08-16 | 按独立评审修订：状态机、SessionDoc、LiveBus 编译、gap 子集、deny 包含、A1 先行、混部、cap、KD-K21–K30；关闭原 Q1–Q5 |
