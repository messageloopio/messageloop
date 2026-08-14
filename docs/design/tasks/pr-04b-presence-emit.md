# PR-04b 实现规格：打开 Presence `cluster_emit` 门闩

| 字段 | 值 |
| --- | --- |
| 标题 | `server: optional presence cluster_emit on exact channels` |
| 状态 | **Accepted**（2026-08-14 主 agent 终验通过，尚未 commit） |
| 依赖 | **PR-04a 已合**（`deliverPresenceEvent` / `presencePublication` / broadcast 改写 `ml.type=presence`）。舰队必须先全部跑 04a，再把本开关打开 |
| 设计来源 | [v1.0-platform-gaps.md](../v1.0-platform-gaps.md) 缺口 2 集群节、KD-3、KD-16、验收 9 |
| 验收人 | 主 agent |

## 1. 目标

`cluster_emit=true` 时，join/leave 的**唯一**投递路径是：

`PublishTransient(exactCh, presencePublication(evt))` → 各节点 broker handler → 已有 `broadcastPublication` 改写 → `deliverPresenceEvent`。

默认 **`false`**，行为与 04a 完全一致（只本地 `deliverPresenceEvent`）。

**禁止叠用**：`true` 时 `emitPresence` **不得**再调 `deliverPresenceEvent`。内存 broker 的 `PublishTransient` 同步进 handler；Redis 本进程也 `PSubscribe`，offset 0 **不去重**。叠用 = 本节点每人两条 join，对端一条，客户端无法按 ID 去重。

本 PR **不**实现：SDK `OnPresence`、改默认值为 true、Survey、心跳、按 user。

## 2. 允许改动的文件

- `config/config.go`、`config/config_test.go`：`server.presence.cluster_emit`（bool，默认 false）
- `config-example.yaml`：注释掉的示例字段
- `node.go`：`Node` 记住开关；`presenceClusterEmit()`；改 `emitPresence` 分支。**不要改 `NewNode` 签名**
- `hub.go`：**仅** broadcast 改写分支：`excludeSession` 改为事件主体（`evt.Info.SessionId`），保证 Phase 2 仍无 self-join。不要改普通 publication 路径
- `metrics.go` / `metrics_test.go`：`presence_failures_total{op="emit"}`（PublishTransient 失败）。已有 `op` 标签，不必新指标名
- `presence_test.go`：本 PR 必测
- 必要时 `cluster_redis_integration_test.go`（Redis 双投测；无 Redis 则 Skip，模式对齐现有 `MESSAGELOOP_TEST_REDIS_ADDR`）
- `docs/developer/02-configuration.md`、`docs/developer/04-cluster.md`
- `docs/protocol.md` Presence 节「Phase 1 只本节点」补一句门闩
- `docs/design/tasks/pr-04b-presence-emit.md`（完成备注）

禁止：改 proto、改 SDK、改 `channel_policy.go`、改 `client.go` 生产路径、把默认改成 true、git 写操作。

## 3. 现状（动手前再读）

- `emitPresence`（`node.go:1022-1024`）**只**调 `deliverPresenceEvent`。注释写明不要预留 `cluster_emit` 分支——本 PR 就是加这个分支。
- `presencePublication` / `parsePresencePublication` 已在 `presence_event.go`。
- `broadcastPublication` 已识别 `ml.type=presence` 并 `deliverPresenceEvent(ch, evt, "")`。**第三个参数是空字符串**，Phase 2 下加入者已在精确订阅里，会被打上 self-join。本 PR 必须修这一点。
- 内存 `PublishTransient`（`broker_memory.go:178-190`）：无历史，同步调 `Start` 注册的 handler（`node.Run` 里就是 `broadcastPublication`）。
- Redis `PublishTransient`（`pkg/redisbroker/redis.go:292-314`）：只 `PUBLISH`；本进程 `deliverOnce` 对 offset 0 **无条件投递**（`pubsub.go:257-261`）。
- `NewNode(cfg *config.Server)` 已读 `cfg.RequireAuth` / heartbeat / channels。本开关同样从 `cfg` 拷进 `Node` 字段。
- `shouldTrackPresence` 已挡住通配 / ephemeral / `presence=false`。`emitPresence` 只会被这些门闩之后的 writer 调用。

## 4. 配置

```go
// config/config.go
type Server struct {
    // ...
    Presence Presence `yaml:"presence" json:"presence" mapstructure:"presence"`
}

// Presence is the process-wide presence control-plane switch.
// It is not a channel policy (those stay under server.channels).
type Presence struct {
    // ClusterEmit, when true, publishes first-class presence events
    // through the broker so other nodes can rewrite them. Default false.
    // Turn on only after every node is on PR-04a+.
    ClusterEmit bool `yaml:"cluster_emit" json:"cluster_emit" mapstructure:"cluster_emit"`
}
```

零值 = false。`Validate()` 不必为 bool 加规则。不要放进 `server.channels`。

`config-example.yaml` / `02-configuration.md`：

```yaml
server:
  presence:
    cluster_emit: false   # 全舰队升级到 PR-04a 后再改为 true
```

文档必须写清：混部旧节点会把 `ml.type=presence` 编成 `publication`（聊天）。

`NewNode`：

```go
if cfg != nil {
    node.presenceClusterEmitFlag = cfg.Presence.ClusterEmit
}
```

字段名自定。提供：

```go
func (n *Node) presenceClusterEmit() bool
```

`cluster_emit=true` 时启动打一条 **Warn**（`NewNode` 或 `Run` 一次即可）：所有节点必须已是 04a+。

## 5. `emitPresence`（必须按此）

```go
func (n *Node) emitPresence(ch string, evt *clientpb.PresenceEvent, excludeSession string) {
    if n.presenceClusterEmit() {
        if isWildcard(ch) || evt == nil {
            return
        }
        pub := presencePublication(evt)
        if pub == nil {
            if n.metrics != nil {
                n.metrics.PresenceFailures.WithLabelValues("emit").Inc()
            }
            return
        }
        if err := n.PublishTransient(ch, pub); err != nil {
            log.WarnContext(context.Background(), "failed to emit presence",
                err, "channel", ch)
            if n.metrics != nil {
                n.metrics.PresenceFailures.WithLabelValues("emit").Inc()
            }
        }
        return
    }
    n.deliverPresenceEvent(ch, evt, excludeSession)
}
```

硬约束：

1. `true` 分支 **零** `deliverPresenceEvent`。
2. `false` 分支 **零** `PublishTransient`（与 04a 相同）。
3. 发布的是**精确业务频道** `ch`，不是 `ch/__presence`。Metadata 必须是 `presencePublication` 那套（`ml.type=presence` + protojson）。
4. 通配 pattern 即使被误调也不 `PublishTransient`。
5. `legacy_presence_channel` 仍走 `PublishPresenceJoin/Leave`（04a），与本开关无关。不要把伴生和 first-class emit 混在一个 `PublishTransient` 里。

## 6. broadcast 改写：排除事件主体

`hub.go` 现有：

```go
n.deliverPresenceEvent(evt.Channel, evt, "")
```

改为：

```go
exclude := ""
if evt.Info != nil {
    exclude = evt.Info.GetSessionId()
}
n.deliverPresenceEvent(evt.Channel, evt, exclude)
```

这样 Phase 2 下加入者 / 离开者（`Info.SessionId`）不会收到自己的事件。`TestPresence_BroadcastPresenceNotPublication` 注入的事件 `SessionId=sess-x`，观察者是别人，仍然会收到。

04a 本地路径继续传 `excludeSession`，行为不变。

## 7. 必须存在的测试

| 测试 | 断言 |
| --- | --- |
| `TestPresence_ClusterEmitDefaultLocalOnly` | 默认（或显式 `false`）+ `countingBroker`：join **不** `PublishTransient` 精确频道；A 仍收到恰好 1 条 `presence_event`（本地 deliver） |
| `TestPresence_ClusterEmitMemoryExactlyOne` | `cluster_emit=true`，默认内存 broker，`node.Run`。A、C 订 `chat.room.1`，B Subscribe 同一频道。A、C **各恰好 1** 条 join；B **0** 条 self-join；`MessagesDelivered` 不因这些事件增加 |
| `TestPresence_ClusterEmitMemoryNoDoublePath` | 同上夹具。若有人叠用，A 会收到 2 条。本测试就是「恰好 1」。可再断言没有 `publication` |
| `TestPresence_ClusterEmitWildcardStillLocalCovered` | `cluster_emit=true`，A 订 `chat.**`，B 加入 `chat.room.1`。A 仍收到恰好 1 条 `{channel=chat.room.1}`（经 broker→rewrite→matcher，不是 `PublishTransient("chat.**")`） |
| `TestPresence_ClusterEmitFalseUnaffected` | 现有 `TestPresence_JoinEventAndSnapshot` 继续绿（本 PR 不要改坏默认路径） |
| `TestPresence_ClusterEmitRedisExactlyOne` | **有 Redis 才跑**（skip 对齐 `cluster_redis_integration_test.go`：`MESSAGELOOP_TEST_REDIS_ADDR`，连不上 Skip）。两个 Node 共享同一 Redis broker，A 在 node1、C 在 node2 订同一精确频道，B 在 node1 Subscribe。A 与 C **各恰好 1** 条；B 0 条。证明跨节点且无双投 |

Redis 测试允许放在 `cluster_redis_integration_test.go`。本地无 Redis 时 Skip **不算失败**；实现者报告里写清跑了还是 Skip。

`TestPresence_BroadcastPresenceNotPublication` 与 `TestPresence_NoCompanionByDefault` 必须继续绿。

## 8. 文档

`02-configuration.md` server 节增加 `server.presence.cluster_emit` 表行：默认 false；全舰队 04a 后再开；旧节点会把 presence 当聊天。

`04-cluster.md` 加一小节「Presence 跨节点」：

- `false`：只本节点 `deliverPresenceEvent`
- `true`：只 `PublishTransient(exact, ml.type=presence)`，对端靠 04a 改写
- 禁止与本地 deliver 叠用的原因（内存同步 handler / Redis offset-0）
- 与 `cluster.enabled` 独立：控制面关着也能靠 Redis broker 扇 presence

`protocol.md` Presence 末句改为：默认只本节点；`server.presence.cluster_emit=true` 后经 broker 跨节点，加入者仍无 self-join。

## 9. 验收清单

1. 默认 `cluster_emit=false`，04a 测试全绿，精确频道无 first-class `PublishTransient`。
2. `true` 时 `emitPresence` 不调用 `deliverPresenceEvent`。
3. 内存 broker：两观察者各 1 条，加入者 0 条。
4. Redis（若可用）：跨节点各 1 条，无双投。
5. 通配订阅者仍能收到精确频道事件（经 rewrite，不是对 pattern 做 PublishTransient）。
6. broadcast 改写排除 `evt.Info.SessionId`。
7. 无 proto 变更；默认仍 false；`NewNode` 签名不变。
8. `go test -count=1 . ./config/...` 与 `go test -race -count=1 .` 绿。

## 10. 完成报告

- 文件列表
- `emitPresence` / `presenceClusterEmit` / rewrite exclude 的文件:行
- §7 每个测试：过 / 失败 / Skip（仅 Redis）
- §9 八条 + 证据
- 偏离与理由

## 11. 实现备注（落地后填写）

（实现者补 2–6 条非显而易见决定。）

1. **Warn 放在 `NewNode`**（不是 `Run`）：`Run` 可能被测试复用同一 node，`NewNode` 每次构建都打一次，不会漏。
2. **Redis 双节点测试故意不开 `cluster.enabled`**：presence emit 只依赖 Redis pub/sub 管道，控制面关着也能扇 presence（顺带锁定 §8「与 `cluster.enabled` 独立」）；也避免节点租约/投影修复的噪音。
3. **Redis 测试的「恰好 1」同步点**：先等 A 收到 C 的 join（异步 pub/sub 投递）再清空 A/C 的 transport，之后 B 的 join 是唯一事件源；末尾再睡 300ms 断言条数不涨，挡住迟到/叠用。
4. **`emitPresence` 用 `n.PublishTransient`（Node 方法）而非 `n.broker.PublishTransient`**：复用 PublishDuration 计时与 MessagesPublished 计数，与 `PublishPresenceJoin/Leave` 的既有习惯一致。
5. **`presencePublication(evt)` 返回 nil 只有一种情况**（protojson.Marshal 失败，理论不可达），仍按规格记 `op=emit` 并 return，不在 PublishTransient 里混入伴生发布。
6. **gofmt 未跑**：目标文件在本仓库本就 CRLF 不洁（stash 前后 `gofmt -l` 输出一致），避免制造全文件 diff。
