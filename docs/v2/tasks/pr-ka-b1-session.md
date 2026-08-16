# PR-KA-B1 实现规格：Session / Attachment、写队列、状态机

| 字段 | 值 |
| --- | --- |
| 标题 | `session: stable Session object, attachable transport, send queue` |
| 状态 | **Accepted**（2026-08-16 主 agent 终验通过，尚未 commit） |
| 依赖 | A4 已合。在 `v2` 分支上做 |
| 设计来源 | [kernel-architecture.md](../kernel-architecture.md) Session Plane、KD-K2、KD-K3、KD-K5、KD-K6、KD-K31 |
| 验收人 | 主 agent |

## 1. 目标

把「Hub 里存 `*Client`、本机 resume 扫 64 个 `subShard` 换指针」改成 **Session 对象稳定、Attachment 可撕可贴**。

1. Hub 持有 `*Session`。本机接管 **不** 扫订阅分片、**不** 重建通配 matcher。
2. 状态机钉死：`Authenticating | Attached | Detached | Closed`。`Detached` **只** 用于本机交接窗口。
3. 三条关闭收成三个动词：`Close`（真走）、`Fence`（被抢）、`Detach`（本机撕附件）。删除 `close` / `closeQuiet` / `evictSessionForTakeover` 三套平行语义。
4. Session 上一条写队列（Control / Data）。下一帧优先 Control。满与写失败按码表映射。
5. `statusConnecting` 从未赋值的洞补上：Connect 完成前必须是 `Authenticating`。

**不做：** Occupancy 换 LiveBus / 删 `Hub.node` / 删 `cluster_emit`（B2）；流式 `RecoverComplete` 消费（B3）；HMAC / `internal/*` 包重划（B4）；切运行时到 `clientv2` proto；抽出 `ChannelService`；改 A1 fencing CAS、A2 gap、A3 live 编译、A4 Decide。

## 2. 允许改动的文件

- `session.go`（新）+ `session_test.go`（新）：`Session`、`Attachment`、`SessionState`、写队列、`Attach` / `Detach` / `Fence` / `Close`
- `client.go`：把现行 `Client` 收成 `Session`（或 `type Client = Session` 过渡）；`handleConnect` 本机 resume 改 `Attach`；删 `closeQuiet`；`write` 走队列
- `hub.go`：`sessions map[string]*Session`；**删除** `ReplaceSession`（及其扫 shard / 重建 matcher）；`Subscriber` 持 `*Session`
- `node.go`：`NewClient` / `AddClient` / `RemoveSession*` 签名随类型走；不改订阅 saga 算法
- `cluster_resume.go`：`evictSessionForTakeover` → `Session.Fence`（不 Leave、不 Unbind）
- `cluster_commands.go`：takeover 命令走 `Fence`
- `heartbeat.go`：心跳挂在 Session 上（不改 idle/ping 数值合同）
- `transport.go` 与 `pkg/websocket/transport.go`、`pkg/grpcstream/transport.go`、`pkg/quicstream/transport.go`：只为「传输不再做第二层有界队列」——gRPC `sendCh` 不得再当 64 深的第二缓冲（改为深度 1 的 handoff，或由 Session writer 直接 `Write`）
- **所有**因 `*Client` → `*Session` / `ReplaceSession` 删除而编译失败的测试（根包、`pkg/*`）。SDK `sdks/go` **不要**改业务，除非类型导出断裂（本 PR 不要导出新的客户端 SDK API）
- `docs/developer/01-architecture.md` Client/Hub/接管段
- `docs/v2/tasks/pr-ka-b1-session.md`（完成备注）

禁止：改 proto、A1 CAS 算法、A2 History/gap、A3 `pubsub.go`、Authorizer 求值、Occupancy 投递面、git commit/push。

## 3. 现状（动手前再读）

- `Hub.sessions map[string]*Client`。本机 resume：`handleConnect` 复制 `subscribedChannels` 到 **新** `*Client`，`old.closeQuiet()`，再 `ReplaceSession`：
  - 换 `sessions` 与 `connShards` 指针
  - **扫 64 个 `subShard`** 把 `sub.Client = newClient`
  - 通配：按 `sessionID:` 前缀拆 matcher 再 Subscribe 新指针（`hub.go` `ReplaceSession`）
- `ReplaceSession` 失败时旧 transport 已 `closeQuiet`，回滚靠 `RemoveSubscription` + `presenceLeave`（现行 `client.go` ~580）。这是第四条关闭路径。
- `Client.close`：presence Leave + 撤订阅 + `RemoveSessionIfMatches` + Unbind（`deleteClusterSessionState`）+ `transport.Close`。
- `closeQuiet`：只关 transport，不 Leave、不撤订阅。被替换的旧对象变 `statusClosed`，但 Hub 已指向新对象。
- `evictSessionForTakeover`：撤本地订阅、不 `presenceLeave`、`RemoveSessionIfMatches`。被抢应用这条。
- `statusConnecting` 已定义，**从未赋值**。对象出生即零值，Connect 成功后才像 connected。
- 出站：`Client.write` 同步 `transport.Write`；失败一律 `DisconnectSlowConsumer`（3512）。gRPC 另有 `sendCh` 深 64。任意 Write 错变 3512 是诊断里的裂缝。

## 4. 类型

根包新增（名字必须用这些）：

```go
type SessionState uint8

const (
    SessionAuthenticating SessionState = iota + 1
    SessionAttached
    SessionDetached
    SessionClosed
)

// Attachment is one transport binding: the socket plus how bytes are framed.
// It does not own subscriptions, fencing, or Occupancy.
type Attachment struct {
    Transport Transport
    Marshaler Marshaler
    Protocol  string // "ws" | "grpc" | "quic"
}

// Session is the recoverable logical connection (KD-K2). Hub holds this
// pointer for the lifetime of the session on this node.
type Session struct {
    // existing Client fields that are session state: id, user, clientID,
    // fencing/lease version, subscribedChannels, heartbeat, limiters, ...
    // plus:
    state      SessionState
    attachment *Attachment
    out        *sendQueue
}

func (s *Session) State() SessionState
func (s *Session) Attach(att *Attachment) error   // Detached|Authenticating → Attached
func (s *Session) Detach(reason Disconnect)       // Attached → Detached; close old att; drop queue
func (s *Session) Fence(reason Disconnect) error  // → Closed; no Leave; no Unbind
func (s *Session) Close(reason Disconnect) error  // 真走：Leave + Unbind + Closed
```

`NewClient(...)` 可以留作构造函数名，但必须返回 `*Session`（`type Client = Session` 允许，仅作别名）。Hub / Subscriber / Lookup 的静态类型必须是 `*Session`。

`Subscriber`：

```go
type Subscriber struct {
    Session   *Session
    Ephemeral bool
}
```

若为减少测试 churn 暂留 `Client *Session` 字段，必须同时提供 `Session` 字段或访问器，并且 **matcher / subShard 存的是 Session 指针**。本 PR 验收看指针稳定，不看字段拼写。

## 5. 状态机

`Detached` = 本进程仍持有 Session、Directory **仍认本 fencing**、附件已撕。被抢节点 **不准** 进 Detached。

| 事件 | 下一状态 | Directory | Occupancy | Interest / Hub 订阅 |
| --- | --- | --- | --- | --- |
| 新生连接（未 Connect） | Authenticating | 未 Bind | 无 | 无 |
| Connect 成功（新 session） | Attached | Bind | 随后按 shouldTrack Join | 按订阅登记 |
| 本机 resume：Attach 成功 | 同一对象 Attached | 仍占（same-fence；version 可 +1 后仍是自己） | **不** Leave | **指针不动** |
| 本机 resume：Detach 后 Attach 失败 | Closed | Unbind | Leave（按真走） | 撤掉。禁止「Directory 占着、没附件」 |
| 跨节点：本节点被 Evict / 写路径 `ErrSessionFenced` | Closed（`Fence`） | **不许 Unbind** | **不** Leave | 撤本地 Coverage/Interest（进程里对象没了） |
| 真走 / 空闲 / 客户端关 / 限额 / 鉴权失败 | Closed（`Close`） | Unbind | Leave | 撤掉 |
| Bind 失败 | 本连接 Closed | 不动别人 | 不 Join | 无 |

`Authenticating` 必须在 `NewClient` / `Accept` 时赋上，不得再靠零值。

Connect 完成（发出 `Connected`）时必须是 `Attached`。

## 6. 本机接管（硬验收）

禁止再走「新 `*Client` + `ReplaceSession` 扫 shard」。

```
existing := hub.LookupSession(sessionID)  // 已在本节点
// 鉴权成功且允许 resume 之后：
existing.Detach(Disconnect{})             // 关旧附件；不 Leave、不撤订阅
if err := existing.Attach(newAtt); err != nil {
    _ = existing.Close(DisconnectInternal) // §5：空洞走真走
    return err
}
// 新连接上那个临时 Authenticating Session 不得进 Hub，直接丢弃（关其 transport）
```

约束：

1. `LookupSession` 在 resume 前后返回 **同一** `*Session` 指针。
2. `subShard` 里该 `sessionID` 的 `Subscriber.Session` 指针不变。
3. 通配 matcher **零** Unsubscribe/Subscribe 只为换指针。可用计数器或 `hub_test` 断言 matcher 条目地址/次数。
4. `ReplaceSession` **删除**。旧测试改写成指针恒等。
5. 跨用户 resume 仍执行 `maxConnsPerUser`（在 `Attach` 或 Node 层查 connShard）。失败则 **旧 Session 保持 Attached**（还没 Detach），新连接 `Close`。为满足这点：**先检查限额，再 Detach**。不要先 `closeQuiet` 再失败回滚。

版本：本机 resume 仍可 `clusterLeaseVersion+1` 后 same-node Bind（A1 已允许 local≥directory）。不要改 CAS 算法。

## 7. 写队列

挂在 **Session** 上，不在三条传输里再做一套策略。

| 项 | 值 |
| --- | --- |
| Control 深度 | 32 |
| Data 深度 | 256 |
| 单帧上限 | `MaxMessageSize`（默认 64KiB），出站同样适用 |
| 下一帧 | 有 Control 先取 Control，否则取 Data |
| Data 满 | `DisconnectSlowConsumer`（3512），关附件（本 PR 按关 Session `Close` 处理，不断开后继续丢） |
| Control 满 | 视为对端已死，3512，`Close` |
| 写超时 | 传输 `write_timeout`，默认 10s |
| Detach | **丢弃** 队列（本机接管靠后续 recover；跨节点同样丢） |

分类（必须按信封，禁止按「是否 Publication」猜）：

**Control：** `Ping`/`Pong`、各类 `*Ack`、`Connected`、`Error`、`Disconnect`、`RecoverComplete`（若已有该信封）、`SubRefreshAck`。

**Data：** `Publication`、`PresenceEvent`、`SurveyRequest`/`SurveyResult`、其它载荷。

`Session.Send` 入队；**一个** writer goroutine 在 Attached 期间排空。`Detach`/`Fence`/`Close` 停 writer。

传输：`Write`/`WriteMany` 保持同步。gRPC 现有深 64 的 `sendCh` 必须改成深度 ≤1 或删掉，避免双缓冲把 3512 推迟到看不见。

### 错误映射（`write` 不得再一律 3512）

| 现象 | 码 |
| --- | --- |
| `io.EOF`；gRPC `Canceled` / `Unavailable`（对端走）；WS close 1000/1001 | `Disconnect` 3000（`peer_closed`）。**不要** 3512 |
| 写超时；Data/Control 队列满；写阻塞超过 write_timeout | 3512 SlowConsumer |
| idle / 未应答 ping | 3511（心跳已有，保持） |
| `ErrSessionFenced` | `Fence` → `DisconnectStale`（3502），已在 A1 |

用 `errors.Is` / gRPC status code，禁止字符串包含。

## 8. 关闭动词

| 动词 | 替代今日 | 副作用 |
| --- | --- | --- |
| `Close(reason)` | `Client.close` | Leave（shouldTrack）+ 撤订阅 + `RemoveSessionIfMatches` + Unbind + 关附件 |
| `Fence(reason)` | `evictSessionForTakeover` | 撤本地订阅与 Hub 条目 + 关附件；**不** `presenceLeave`；**不** `deleteClusterSessionState` / Unbind |
| `Detach(reason)` | `closeQuiet` | 只关附件、停 writer、丢队列；Session 留在 Hub；状态 `Detached`（随即 `Attach` 回 `Attached`） |

`Close` / `Fence` 对已 `Closed` 幂等。`Detach` 对非 `Attached` 是 no-op。

被抢节点只准 `Fence`，不准 `Detach`。

## 9. 必须存在的测试

1. **指针稳定**：本机 resume 后 `LookupSession` 指针 == 旧指针；对已订精确频道与 `im.**`，matcher/subShard 中的 Session 指针不变。可用 `unsafe.Pointer` 或直接 `==`。
2. **零扫描**：`ReplaceSession` 符号不存在。可用 `go/ast` 或干脆编译：调用 `ReplaceSession` 的测试已改写。
3. **先查限额再 Detach**：用户已满时跨用户 resume 失败，**旧** Session 仍 `Attached` 且 transport 未关（改写 `TestHub_ReplaceSession_FailureKeepsOldSessionIntact` / `client_fix_test` 里对应用例）。
4. **Attach 失败走真走**：用可失败的 fake transport 让 `Attach` 失败（Detach 已发生）→ Session `Closed`、Hub 无该 session、有 Leave（若曾 track）。
5. **Fence**：`Fence` 后 Hub 无 session、无订阅；presence store **没有** leave 记录（spy）；Directory / `deleteClusterSessionState` **零**调用（spy）。
6. **真走**：`Close` 后有 leave（track 频道）且 Unbind/delete cluster 被调用。
7. **状态**：`NewClient` 后 `State()==Authenticating`；`Connected` 发出后 `Attached`；`Detach` 窗口可在单测里直接断言。
8. **写队列**：先入 1 条 Data 再入 1 条 Control，spy transport 的 Write 顺序为 Control 然后 Data。Data 填满 256 后再入一条 → 3512。Control 填满 32 后再入 → 3512。
9. **peer_closed**：fake `Write` 返回 `io.EOF` → 断开码 3000，不是 3512。
10. 既有本机 resume / 通配 resume / A1 fenced ping 测试仍绿（改指针恒等后）。
11. `go test ./...`；`go test -race . ./pkg/websocket ./pkg/grpcstream ./pkg/quicstream`。

禁止固定长 Sleep 代替同步点（Ready / Eventually / 队列 ack）。

## 10. 验收清单

1. 仓库无 `ReplaceSession`；本机 resume 不扫 `subShard`、不重建 matcher。
2. Hub 持 `*Session`；resume 前后指针恒等。
3. 三动词：`Close` / `Fence` / `Detach` 副作用符合 §8。
4. `Authenticating` 在 Connect 前被赋值。
5. 写队列深度与 Control 优先；EOF → 3000。
6. 未改 A1 CAS、A2 gap、A3 live、A4 Decide。
7. 测试命令绿。

## 11. 完成报告

- 文件列表
- §10 逐条证据
- 测试命令与结果
- 偏离（应无）

## 12. 实现备注（完成后填写）

（实现者填写）

### 实现摘要（PR-KA-B1，2026-08-16）

- **对象**：`Session`（session.go）持有 `state` / `attachment` / `out`（Control 32 / Data 256 双车道写队列）+ 原 Client 字段；`type Client = Session` 过渡别名。`NewClient` 出生即 `SessionAuthenticating`（不再靠零值）。
- **Hub**：`sessions map[string]*Session`、`Subscriber.Session`；`ReplaceSession` 删除，改为 `PrepareSessionUser`（跨用户 resume 的限额检查 + connShard 迁移，原子、失败零副作用）。本机 resume 不再扫 subShard、不重建 matcher。
- **handleConnect**：本机 resume 走 `existing.Detach → existing.Attach(newAtt)`，临时 Authenticating 会话不进 Hub，变成读循环 shell（`delegate` 委托）；跨用户先 `PrepareSessionUser`（失败旧会话保持 Attached、新连接 3504）。`Attach` 失败 → `existing.Close(DisconnectInternal)` 真走（§5 空洞修复）。
- **三动词**：`Close`（Leave+Unbind+撤订阅+关附件，幂等）、`Fence`（不 Leave、不 Unbind；含部分失败回滚并恢复 Attached）、`Detach`（只关附件停 writer 丢队列；非 Attached no-op）。`close`/`closeQuiet`/`evictSessionForTakeover`/`disconnectFenced` 旧语义删除，takeover 命令与 fenced ping 走 `Fence(DisconnectStale)`。
- **写队列**：`Send` 入队并按信封分类（Control 优先，下一帧选择）；Attach 起唯一 writer 排空，Detach/Fence/Close 停 writer 丢队列。`Send` 等待该帧落线（done channel），调用方同步观察写结果；Authenticating 窗口（无 writer）直写附件（鉴权拒绝信封仍能落线），Detached/Closed 快速失败。Data/Control 满 → `Close(3512)`。错误映射：`io.EOF`/`net.ErrClosed`/WS close 1000/1001/gRPC Canceled/Unavailable → 3000；写超时等 → 3512；`ErrSessionFenced` → Fence 3502。
- **gRPC 传输**：`sendCh` 64 → 1（仅 handoff，不再当第二缓冲）；相关两条 P1-B3/B4 回归测试改写为深度 1 语义。
- **出站大小上限**：`MaxMessageSize` 适用于普通帧；`Connected` 信封豁免（恢复批次仍一帧最多 1000 条，B3 流式恢复再切帧）。
- **附带修复**：`pkg/quicstream/server.go` `s.ln` 的 Start/Close 数据竞争（基线已存在，`go test -race` 门禁需要；改动仅加锁读）。
- **文档**：`docs/developer/01-architecture.md` Client/Hub/接管段改写为 Session/Attachment/三动词叙事。

