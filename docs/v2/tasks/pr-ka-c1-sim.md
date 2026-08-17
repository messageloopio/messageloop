# PR-KA-C1 实现规格：确定性 fencing 模拟（KD-K20）

| 字段 | 值 |
| --- | --- |
| 标题 | `cluster: deterministic fencing simulator; lock Bind/Evict/Fence` |
| 状态 | **Accepted**（2026-08-17 主 agent 终验通过，尚未 commit） |
| 依赖 | B4 已合（`a0ea543`）。在 `v2` 分支上做 |
| 设计来源 | [kernel-architecture.md](../kernel-architecture.md) Cluster / 状态机、KD-K3、KD-K4、KD-K5、KD-K20、KD-K30 |
| 验收人 | 主 agent |

## 1. 目标

fencing 合同已经落地（A1 CAS、B1 Fence/Detach/Close、B4 无盲写 + OnLeave）。本 PR **不再改生产算法**，只加一套 **进程内、可步进、无 Redis、无 Sleep** 的模拟夹具，把宪法场景锁成回归。

1. `internal/cluster/sim`：共享内存 Directory（真 CAS）、可编排的内存 CommandBus、可选 Clock。
2. 两个 Node 共一份 Directory。incarnation 由脚本指定（`inc-a` / `inc-b`），禁止依赖 `uuid.New`。
3. 默认 RPC = **同步函数调用**（与靶心「单节点 RPC = 函数调用」一致）。丢 Evict / 暂扣命令是显式编排，不是并发碰运气。
4. 下列场景必须绿，且不访问 Redis、不用固定长 `Sleep`：B 抢走后 A ping 不得写回；Bind 后 Evict → 旧节点 Fence 且不得 Unbind；丢掉 Evict 仍无双活；本机 Detach/Attach 指针稳定；死节点 OnLeave 后可 `CAS(nil)`；并发 `CAS(nil)` 只一人赢。

**不是：** FoundationDB / TigerBeetle 级离散事件网络模拟；随机调度 / 属性测试（可留接口，本 PR 不要求）；改 A1 CAS、B1 状态机、B4 HMAC；生产 `IncarnationID` 改 `INCR`（KD-K27 另刀）；命令总线换 Redis Stream；整仓 `internal/*` 搬家；切 admin `server.v2`。

## 2. 允许改动的文件

- `internal/cluster/sim/`（新）：`Clock`、`Directory`（多 session 真 CAS）、`Bus`（同步投递 + Hold/Drop/Flush）、`World`（两节点夹具）
- `cluster_sim_test.go`（根包，新）：宪法场景。根包测试可以调用未导出的 `syncClusterSessionState` / `resumeRemoteSession` / `handleConnect` / `membershipOnce`
- 仅当夹具必须碰未导出字段时：根包 `cluster_sim.go` 放薄封装（例如 `export` 测试用的 `Node` 访问器）。禁止把生产热路径改成「if sim」
- 现有 `fakeSessionDirectory` **不要删**（A1/B4 单测仍用）。不要为了模拟去改 CAS 比较字段
- `docs/v2/tasks/pr-ka-c1-sim.md`（完成备注）
- `docs/developer/04-cluster.md`：加一小节「确定性模拟（测试）」——两段话即可

禁止：改 `protocol/**`、SDK、`pkg/redisbroker` 生产代码、HMAC 规范字节、`client.go` / `hub.go` / `session.go` / `recover.go` / `occupancy.go` / `authorizer.go` / `interest.go` 的业务算法、git commit/push。

允许的生产改动只有一种：为注入 Clock 加可选函数字段（例如 `Node.now func() time.Time`，nil = `time.Now`）。没有注入需求就不要改。

## 3. 现状（动手前再读）

- A1 已有 `TestClusterSessionSync_FencedWhenAnotherOwnerWins`：手写 `directory.lease = B的租约`，再调 A 的 `syncClusterSessionState`。fake 基本是 **单 slot**（`f.lease`），不是多 session 权威 Directory。
- B1：`Session.Fence` 关附件、撤本机订阅、**不** `DeleteSessionLease`、不 Leave。`Detach` 只撕附件，Directory 仍认本 fencing。
- B4：`clusterRepairer.membershipOnce` 首拍只建集合；死 incarnation `DeleteSessionLease`。
- `ClusterOptions.IncarnationID` 空则 `uuid.NewString()`（KD-K27 未做）。模拟里必须显式传入。
- 命令总线生产实现是 Redis Pub/Sub + HMAC。内存 / noop bus **不要求** HMAC（B4 §4.4）。
- 现有集群测试大量 `time.Sleep` / Redis Skip。本 PR 的宪法测试禁止走那条路。

## 4. 夹具类型

包路径：`github.com/messageloopio/messageloop/internal/cluster/sim`

名称可同语义，行为必须如下。

### 4.1 Clock

```go
type Clock struct { /* 单调墙钟 */ }

func NewClock(start time.Time) *Clock
func (c *Clock) Now() time.Time
func (c *Clock) Advance(d time.Duration) // d>0；禁止回拨
```

本 PR 的宪法场景 **可以不走 Clock**（CAS 不比 TTL）。Clock 必须存在且单测覆盖 Advance。不要为了用上它去改遍生产 `time.Now()`。

### 4.2 Directory

实现 `messageloop.SessionDirectory`，并实现 `ClusterSessionLeaseLister` + `ClusterNodeLeaseLister`。

- session lease 按 `SessionID` 存；node lease 按 `(NodeID, IncarnationID)` 存。
- `CompareAndSwapSessionLease` 的相等谓词与生产一致：`SessionID, NodeID, IncarnationID, LeaseVersion`（见 `fakeLeaseEqual` / `clusterSessionLeaseEqual`）。`expected==nil` 且当前无键 → 成功。
- **禁止**无条件覆盖（没有 Put）。
- `DeleteSessionLease` 必须同步 user 索引（与 Redis directory 相同：有 UserID 则 `RemoveUserSession`）。
- 并发 CAS 在一把锁下原子完成：两个 `CAS(nil)` 恰好一个成功。
- snapshot 按 session 存；本 PR 场景若只用 lease，snapshot 可空实现但接口要满。

### 4.3 Bus

实现 `messageloop.ClusterCommandBus`。

```text
默认：SendCommand 在同一 goroutine 调目标 handler，返回其 Result。
Hold()     之后 SendCommand 只入队，不跑 handler
Flush()    按 FIFO 投递队列；调用方再读结果
DropNext() 下一条 SendCommand 不投递，返回 Status=unknown_final_state
           （或 failed + 可识别 ErrorCode，例如 SIM_DROPPED）
           不得跑 handler
```

- 按 `TargetNodeID` + `TargetIncarnationID` 找 handler。每个 Node 的 `SetHandler` 登记自己。
- 目标未登记：返回 failed（`TARGET_NODE_NOT_ALIVE` 或同等），不 panic。
- **不要** HMAC、不要 Redis、不要后台 goroutine 读循环。`Start`/`Shutdown` no-op 即可。
- `BroadcastCommand`：对每个已登记 incarnation 各 `SendCommand` 一份（新 CommandID）。Hold/Drop 对每份生效规则与单发相同。

### 4.4 World

```go
type World struct {
    Clock *Clock
    Dir   *Directory
    Bus   *Bus
    A, B  *messageloop.Node  // 或薄包装
}

func NewWorld() *World
// A: NodeID=node-a IncarnationID=inc-a
// B: NodeID=node-b IncarnationID=inc-b
// 共享 Dir + Bus；Backend=memory；各挂 memory broker
```

- 两个 Node 必须是真的 `*messageloop.Node`（走现有 `syncClusterSessionState` / `resumeRemoteSession` / `Fence`），不是再写一套状态机。
- 装配 `NewCluster(..., ClusterDependencies{SessionDirectory: Dir, CommandBus: Bus, Repairer: NewClusterRepairer(...)})`。Repairer 用同一 Dir，便于 OnLeave 场景直接调 `membershipOnce`。
- 提供测试助手（名称不限）：在指定节点上 `NewClient` + `ForceTestIDs` + `AddClient` + 首次 `syncClusterSessionState`（CAS nil 占坑）。

根包场景测试用 World；不要在每个测试里复制 80 行装配。

## 5. 宪法场景（必须存在）

全部放在 **无 Redis** 的测试里。禁止 `time.Sleep`。用直接调用 + `Bus.DropNext` / `Hold`+`Flush` + 导出的 `membershipOnce`。

| # | 名字 | 步骤 | 断言 |
| --- | --- | --- | --- |
| 1 | `StealThenPing` | A 占 `sess-1`。B `resumeRemoteSession`（或等价 CAS+1 + takeover）成功。A `syncClusterSessionState` | A 得 `ErrSessionFenced`；Directory 仍是 B；`LeaseVersion` 是 B 的。覆盖 A1 核心，但是 **两节点真 Node + 共享 Dir**，不是手写 `directory.lease=` |
| 2 | `BindThenEvictFences` | A 占着且 Attached。B Bind 成功并发出 takeover。Bus 默认同步投递 | A 的 session `Closed`（Fence，不是 Detached）；Hub 无 A；Directory 仍是 B；A 侧 **没有** `DeleteSessionLease` |
| 3 | `LostEvictNoDual` | 同上但 `DropNext` 掉 Evict | Directory 仍是 B。A 下一次 `syncClusterSessionState` → `ErrSessionFenced`，测试再 `Fence`。任意时刻 `LookupSession("sess-1")` 在 A、B 上至多一个 Attached |
| 4 | `LocalDetachAttach` | 单节点 A：Attached → `Detach` → 新 Attachment `Attach`（本机 resume 路径，或直接调 Session API + 现有 handleConnect 本机分支） | Session 指针与 Hub 条目是同一个；Directory fencing 未换 owner（version 不因本机撕贴而 +1，除非实现本就 same-fence 刷新） |
| 5 | `DeadNodeOnLeave` | A 占 `sess-1`。删 A 的 node lease。`membershipOnce` 两次（首拍 prime） | `sess-1` lease 没了；B `CAS(nil, new, ttl)` 成功。不 Sleep 等 600s |
| 6 | `CasNilOnlyOneWins` | 空 Directory。A、B 对同一 session 同时 `CAS(nil)`（可用两个 goroutine + 夹具 Dir 的锁；或同一测试线程连续两次，第二次必失败） | 恰好一次成功；失败者不得把赢家的 lease 覆盖掉 |

场景 2 的 takeover 走现有 `requestSessionTakeover` / `ClusterCommandTakeover` → 目标 `Fence`。若同步 Bus 已让这条路径通，不要再复制一套 evict。

场景 4 若走完整 `handleConnect` 太重：允许只测 `Session.Detach` + `Attach` + Directory 仍是 A。须断言 **不是** Fence（状态是 Detached 再 Attached，Directory 未 Unbind）。

已有 A1 单测保留。C1 是两节点夹具上的同一合同，不是删旧测试。

## 6. 接入

- `go test ./internal/cluster/sim` 不依赖 Redis，必须绿。
- `go test -run 'TestSim_' .`（或你们给场景的前缀）不依赖 Redis，必须绿。
- 生产 `cmd/server` **不**装配 sim。
- 不要加 `session_dual_activation_seconds` 等观测指标，除非场景 3 已经有现成计数器可读。双活用 `LookupSession` + `Session.State()`（或等价导出）断言即可。

## 7. 必须存在的测试

1. `Directory`：`CAS(nil)` 成功；二次 `CAS(nil)` 失败且原值不变；same-fence（version 相同）刷新成功；version 不同失败。
2. `Bus`：默认同步，handler 跑一次并返回；`DropNext` 后 handler 零次；`Hold` 时 handler 零次，`Flush` 后跑一次。
3. `Clock`：`Advance(5s)` 后 `Now` +5s；`Advance(0)` 或负数不回拨（返回 error 或 no-op，选一种并测）。
4. §5 表 1–6 各一条（可 `t.Run`）。
5. 宪法测试源码不含 `time.Sleep(`。
6. `go test ./internal/cluster/sim ./...`；`go test -race . ./internal/cluster/sim`。

## 8. 验收清单

1. 存在 `internal/cluster/sim`，生产热路径未改 fencing / HMAC / recover。
2. 两节点共享 Directory；B 抢权后 A ping 不得写回。
3. Evict 到达 → 旧节点 Fence，Directory 仍是新 owner。
4. Evict 丢失 → 无双活；旧节点随后 Fence。
5. 死节点 OnLeave 后 `CAS(nil)` 成功。
6. 无 Redis、无 Sleep 跑宪法测试。
7. 未做 Stream / `node_epoch` INCR / 整仓搬家。
8. 测试命令绿。

## 9. 完成报告

- 文件列表
- §8 逐条证据
- 测试命令与结果
- 偏离（应无）

## 10. 实现备注（完成后填写）

- 新增 `internal/cluster/sim`（`clock.go` / `directory.go` / `bus.go` / `world.go` + 三个单测文件）。`Directory` 共享内存实现，session lease 按 `SessionID`、node lease 按 `(NodeID, IncarnationID)` 存，CAS 谓词与生产一致且在一把锁下原子完成；`DeleteSessionLease` 同步 user 索引并记录调用（`DeletedSessionLeases`）供「fenced 不得解绑」断言；`DeleteNodeLease` 为夹具助手（生产接口没有该方法，用于不等 TTL 地演死节点）。`Bus` 默认同步投递，`Hold`/`Flush`（FIFO、返回结果）/`DropNext`（`unknown_final_state` + `SIM_DROPPED`）显式编排；`Register(nodeID, incarnationID, handler)` 按目标 incarnation 路由，`SetHandler` 仅作 fallback（共享总线无法从 `SetHandler` 推出身份），未登记目标返回 failed/`TARGET_NODE_NOT_ALIVE`。`Clock.Advance` 对非正 delta 返回 error、不回拨。
- 根包 `cluster_sim.go` 只放三个薄封装（`SimSyncClusterSessionState` / `SimResumeRemoteSession` / `SimMembershipOnce`），未改任何生产热路径，未注入 Clock（§4.1 允许宪法场景不走 Clock；CAS 不比 TTL，场景 5 用真实墙钟写 lease 的 `ExpiresAt`）。
- 场景测试在根包 `cluster_sim_test.go`，用 **external test package**（`package messageloop_test`）：sim 包 import 根包，internal test package 再 import sim 会构成 Go 禁止的测试 import cycle；外部测试包经由 `cluster_sim.go` 的导出封装访问未导出路径（仓内已有 `cluster_v1_e2e_test.go` 同此前例）。
- 场景 3 依赖 KD-K30 死节点旁路：World 默认不注册 node lease，Evict 丢失后 `GetNodeLease` 为 nil，B 的 CAS 保留、Directory 归 B；场景 5 由测试显式 `PutNodeLease` 两个 incarnation。
- 未动 A1 CAS 谓词、B1 Fence/Detach、B4 HMAC、`fakeSessionDirectory`、Redis、Stream、`internal/*` 布局；`cmd/server` 不装配 sim。
