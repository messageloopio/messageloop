# PR-KA-A3 实现规格：Interest 编译与去掉 `PSubscribe *`

| 字段 | 值 |
| --- | --- |
| 标题 | `broker: compile live interest; drop PSubscribe prefix*` |
| 状态 | **Accepted**（2026-08-16 主 agent 终验通过，尚未 commit） |
| 依赖 | A2 已合（memory 已有 matcher Interest）。在 `v2` 分支上做 |
| 设计来源 | [kernel-architecture.md](../kernel-architecture.md) LiveBus 编译、KD-K13、KD-K13b、KD-K31、Q7→拒绝 |
| 验收人 | 主 agent |

## 1. 目标

Redis 实时总线按 **本节点 Interest** 订阅，禁止再 `PSubscribe(PubSubPrefix+"*")` 收全集群再本地丢。

1. 抽出 **一份** `CompileInterest(key)`：精确频道 / 可编译前缀 pattern / 拒绝。
2. Redis `runPubSub` 只订编译结果（`SUBSCRIBE` 精确 + `PSUBSCRIBE` 前缀 glob）。
3. Interest 变化时增删 Redis 订阅；重连后按当前 Interest 重建。
4. 不可路由 pattern：`Subscribe` 失败；客户端该频道 **软失败** `PATTERN_NOT_ROUTABLE`，不断连。
5. 收到消息后仍用 `topics.Match` 丢掉 Redis glob 过匹配（`*` 跨点）。

**不做：** Occupancy 换控制通道（B2）；流式恢复（B3）；HMAC/Stream 命令总线（B4）；Redis Cluster / sharded PUB/SUB；把 Broker 改名为 LiveBus。

## 2. 允许改动的文件

- `interest.go`（新，根包）+ `interest_test.go`：`CompileInterest`、`ErrPatternNotRoutable`
- `broker_memory.go`：`Subscribe` 先 `CompileInterest`（不可路由则 error）
- `broker_memory_test.go`：不可路由 Subscribe 失败
- `pkg/redisbroker/redis.go`：Subscribe/Unsubscribe 在首次/末次时通知 live 层
- `pkg/redisbroker/pubsub.go`：去掉 `PSubscribe(prefix+"*")`；按 Interest 动态订；重连重建
- `pkg/redisbroker/pubsub_test.go` 及必要 redis 测试
- `client.go`：**仅** Connect/Subscribe 把 `ErrPatternNotRoutable` 打成信封 Error，不断连、不回滚其它频道
- `client_test.go` / `client_fix_test.go`：不可路由软失败
- `pkg/topics`：仅当 Compile 要复用 `ValidateTopic` / `Match`（不要改 matcher 语义）
- `metrics.go`：可选 `live_interest_patterns` / 订阅数 gauge
- `docs/developer/01-architecture.md` Redis 实时段
- `docs/v2/tasks/pr-ka-a3-livebus.md`（完成备注）

禁止：改 proto、A1 fencing、A2 gap 算法、`hub.go` 扇出、`PSubscribe(prefix+"*")` 作为兜底留下、git commit/push。

## 3. 现状（动手前再读）

- `runPubSub`：`PSubscribe(ctx, b.opts.PubSubPrefix+"*")`，收包后 `interested()` 过滤（`pubsub.go:106`）。
- Redis `Subscribe` 已对通配做 matcher + refcount（A2 memory 对齐）。
- `Publish` 发到 `PubSubPrefix+精确频道`（不要改频道名）。
- `Ready()` 等第一次 `Receive` 成功后关闭。
- `handleSubscribe`：`AddSubscription` 失败会回滚本请求已加频道并 `return err`（可能变 INTERNAL / 断连）。A3 必须改成不可路由时 **continue + Error 信封**。
- `handleConnect` 里 `AddSubscription` 失败会走 connect 错误路径。不可路由应跳过该频道并在 Connected/后续 Error 中可见，**不断开整次 Connect**。

## 4. `CompileInterest`

放在根包 `interest.go`，memory 与 Redis 共用。

```go
var ErrPatternNotRoutable = errors.New("pattern is not routable on the live bus")

type CompiledInterest struct {
    // Exact is the concrete channel name (no pubsub prefix). Empty if none.
    Exact string
    // Pattern is the Redis glob WITHOUT prefix, or empty.
    // Example: key "im.**" → Pattern "im.*"
    Pattern string
    // AlsoExact is an extra exact subscribe (for trailing ** zero-segment).
    // Example: "im.**" → AlsoExact "im"
    AlsoExact string
}

func CompileInterest(key string) (CompiledInterest, error)
```

规则（顺序固定）：

1. `topics.ValidateTopic(key)` 失败 → 原样返回 `ErrBadTopic`（不是 NotRoutable）。
2. 不含 `*`：`Exact=key`。
3. 按 `.` 分段。最后一段必须是 `*` 或 `**`，且 **前面每一段都是字面**（不含 `*`、`**`）。否则 `ErrPatternNotRoutable`。
4. 字面前缀为空（key 为 `*` 或 `**`）→ `ErrPatternNotRoutable`（会退化成 `PSubscribe prefix*`，KD-K13）。
5. 前缀 = 前面字面段用 `.` 拼接。
   - 末段 `*`：`Pattern = prefix+".*"`（Redis glob）。
   - 末段 `**`：`Pattern = prefix+".*"`，`AlsoExact = prefix`。

表驱动（必须写成测试）：

| key | 结果 |
| --- | --- |
| `chat.room.1` | Exact=`chat.room.1` |
| `im.room.*` | Pattern=`im.room.*` |
| `im.**` | Pattern=`im.*`，AlsoExact=`im` |
| `*` | NotRoutable |
| `**` | NotRoutable |
| `*.room` | NotRoutable |
| `im.*.tick` | NotRoutable |
| `a.` / `a..b` | ErrBadTopic |

`MatchAfterCompile(key, concrete) bool`：用 `topics` 的段匹配（与 CSTrie 一致）。Redis 收包后：先 `interested()`，再建议 `Match` 丢掉 glob 过匹配（`im.room.*` 的 Redis 模式也会收到 `im.room.a.b`）。

## 5. Redis live 订阅

### 5.1 禁止

源码中 **不得** 出现 `PSubscribe(..., PubSubPrefix+"*")` 或等价「前缀 + 单独一个 `*`」作为默认订户。  
允许的 PSubscribe 参数必须来自 `CompileInterest` 的 `Pattern`，且 Pattern 以字面前缀开头（`im.*` 可以，单独 `*` 不可以）。

### 5.2 连接与 Ready

`runPubSub`：

1. `Subscribe(ctx, PubSubPrefix+"__live__")` 作控制订户（客户端不得发到 `__live__`；ValidateTopic 允许该名）。用它的确认关闭 `Ready()`。
2. 按当前 `subscribed` / `wcCounts` **重建** 全部 Compile 结果：精确 → `Subscribe(prefix+Exact)`；Pattern → `PSubscribe(prefix+Pattern)`；AlsoExact → `Subscribe(prefix+AlsoExact)`。
3. 进入收包循环。`msg.Channel` 去掉 prefix 得 concrete；忽略 `__live__`；`interested` + 对每个命中的 pattern `Match`；再 `deliverOnce`。

### 5.3 动态增删

Interest 在 `Subscribe`/`Unsubscribe` 的首次/归零时变化。不得在持 `subMu` 时同步打 Redis。用串行队列（chan + `runPubSub` 同 goroutine，或单 worker）执行：

- add：`pubsub.Subscribe` / `PSubscribe`
- remove：仅当该 Exact/Pattern/AlsoExact 的 **refcount 全局为 0**（多个 pattern 可能共享同一 Redis Pattern，例如 `im.**` 与 `im.room.*` 都可能订 `im.room.*` 不同——按编译结果字符串计 Redis 侧 refcount）。

重连：`runPubSubWithRetry` 新连接上重复 §5.2 重建。不要丢内存里的 `subscribed`/`wcCounts`。

### 5.4 发布

不改 `Publish`/`PublishTransient` 的 Redis 频道名（仍 `PubSubPrefix+ch`）。

## 6. memory

`Subscribe` 入口先 `CompileInterest`：NotRoutable / BadTopic 直接 return。通过后再走今日 refcount+matcher。  
精确与可编译通配行为与 A2 测试保持一致。

## 7. 客户端软失败

导出 `ErrPatternNotRoutable`，`errors.Is` 可识别。

`handleSubscribe`：`AddSubscription` 若 `errors.Is(..., ErrPatternNotRoutable)` 或 `topics.ErrBadTopic`：

- 发顶层 Error：`code=PATTERN_NOT_ROUTABLE` 或 `BAD_REQUEST`（BadTopic），`type=request_error`
- `continue`，**不要**回滚其它已成功频道，**不要** `return err`

`handleConnect` 初始订阅同样：该频道跳过（可记 log），Connect 仍成功；不要 `disconnectOnConnectError`。

Admin `SubscribeSession`：返回 error 即可（现有 gRPC 错误路径）。

## 8. 必须存在的测试

1. `CompileInterest` 表：§4 全行。
2. memory `Subscribe("*.room")` / `"**"` → `ErrPatternNotRoutable`；`Subscribe("im.**")` 成功且 Publish `im.room.1` 仍投递。
3. Redis（真实或 miniredis/已有辅助）：
   - 启动后 **没有** 对 `PubSubPrefix+"*"` 的 PSubscribe（可用 hook / 检查 pubsub 模式列表 / 或集成：节点 A 订 `chat.1`，节点 B 订 `other.1`，A 的 handler 不得因 B 的 Publish 被调用——比今日「先收再丢」更严：可用 spy 包一层统计未 interested 仍到达 runPubSub 的次数；最简：源码禁止 `+"*"` 字面量 + 行为测试「只订 chat.1 时 stocks.1 的 Publish 不进 handler」）。
   - `Subscribe("im.**")` 后 Publish `im` 与 `im.x` 进 handler；`stocks` 不进。
   - `Subscribe("im.room.*")` 后 `im.room.a` 进、`im.room.a.b` **不**进（本地 Match）。
4. 重连：断开 activePubSub 后重订，仍只收到有 Interest 的频道（可用现有 disconnect 测试钩子）。
5. 客户端 Subscribe `*.room`：连接仍在，信封 `PATTERN_NOT_ROUTABLE`，hub 无该订阅。
6. Connect 带不可路由频道：Connected 成功，该频道不在订阅列表。
7. `go test ./...`；`go test -race . ./pkg/redisbroker`。

禁止固定长 Sleep 代替同步点（可用 Ready + Eventually）。

## 9. 验收清单

1. 仓库热路径 **零** `PSubscribe(..., prefix+"*")`（`prefix+"__live__"` 的 Subscribe 可以）。
2. `CompileInterest` 表测试全绿。
3. 不可路由：broker Subscribe error + 客户端软失败不断连。
4. `im.**` / `im.room.*` 投递与过滤正确。
5. 重连后 Interest 仍在且未回到订 `*`。
6. 未改 A2 gap / 未 XDel 回归。
7. 测试命令绿。

## 10. 完成报告

- 文件列表
- §9 逐条证据
- 测试结果
- 偏离（应无）

## 11. 实现备注（完成后填写）

**已实现（2026-08-16，PR-KA-A3）**

- 根包新增 `interest.go`：`CompileInterest` / `CompiledInterest` / `ErrPatternNotRoutable` / `MatchAfterCompile`，memory 与 Redis 共用一份。
- `broker_memory.go`：`Subscribe` 入口先 `CompileInterest`，NotRoutable / BadTopic 直接返回；通过后沿用 A2 refcount+matcher 不变。
- `pkg/redisbroker/pubsub.go`：删除 `PSubscribe(PubSubPrefix+"*")`。每条连接先 `Subscribe(prefix+"__live__")` 作控制订户（其 ack 关闭 `Ready()`），再按当前 `subscribed`/`wcCounts` 重建全部编译结果（精确 `Subscribe`、Pattern `PSubscribe`、AlsoExact `Subscribe`）。动态增删走串行队列 `liveOps`（chan，runPubSub 同 goroutine 消费），`Subscribe`/`Unsubscribe` 在首订/归零时重算 desired 集合并 diff 出 add/remove op；add op 带确认握手（消费 goroutine 收到 subscribe/psubscribe ack 才放行调用方，保证「订阅返回后发布必达」，用 `ChannelWithSubscriptions` 观察 ack）。重连重建不丢内存 `subscribed`/`wcCounts`。收包循环：去 prefix、忽略 `__live__`、`interested()`（含 `MatchAfterCompile` 段匹配过滤）后再 `deliverOnce`。
- `pkg/redisbroker/redis.go`：`Subscribe` 先 `CompileInterest`；`interested()` 增加对命中的 pattern 逐一 `MatchAfterCompile` 过滤（`im.room.*` 不收 `im.room.a.b`）。
- `client.go`：`handleSubscribe` 与 `handleConnect` 对 `ErrPatternNotRoutable` / `topics.ErrBadTopic` 发顶层 Error 信封（`PATTERN_NOT_ROUTABLE` / `BAD_REQUEST`，`request_error`），`continue` 不回滚其它频道、不断连；Connect 仍成功且该频道不进订阅列表。
- 测试：§4 表驱动全行（`interest_test.go`）；memory NotRoutable 拒绝 + `im.**` 投递；Redis 编译订阅（只订 `chat.1` 时 `stocks.1` 不进 handler、无 glob 订阅）、`im.**`（`im`/`im.x` 进、`stocks` 不进）、`im.room.*`（`im.room.a` 进、`im.room.a.b` 不进）、重连重建、动态移除；客户端软失败两条（Subscribe / Connect）。全部用 Ready + Eventually / 握手确认，无固定长 Sleep。
- 已知取舍：跨 Redis 的确认握手只覆盖「订阅返回后必达」；断线窗口内的 add op 在重连重建时恢复，实时丢窗与 A2 的 catch-up 局限相同（文档化）。
