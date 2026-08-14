# PR-03 实现规格：Subscribe / Connect 共用恢复 + 可见结果

| 字段 | 值 |
| --- | --- |
| 标题 | `server: recover on Subscribe and surface recovered/truncated/failed` |
| 状态 | **Accepted**（2026-08-14 主 agent 终验通过，尚未 commit） |
| 依赖 | **PR-01 已合**（`RecoverResult` / `Connected` 6–8 / `SubscribeAck` 2–4）。**PR-02 已合**（`Node.ChannelPolicy`，`transient_only` → Recover=false） |
| 设计来源 | [v1.0-platform-gaps.md](../v1.0-platform-gaps.md) 缺口 1、KD-2、KD-9 |
| 验收人 | 主 agent |

## 1. 目标

Connect 与 Subscribe **共用**一条恢复实现。成功 / 失败 / 截断 / 跳过必须写进 `RecoverResult`，禁止再 `continue` 吞掉 History 错误。订阅成功与恢复成败解耦（KD-9：History 抖动不能让人进不了房）。

本 PR **不**实现：Presence 快照（`Connected.presence` / `SubscribeAck.presence` 保持空）、Go/TS SDK API、服务端 ping、客户端 Survey、按 user。

## 2. 允许改动的文件

- `recover.go`、`recover_test.go`（新，根包）
- `client.go`：**仅** `handleConnect` 的恢复循环 + `Connected` 填充，以及 `handleSubscribe` 末尾 Ack。禁止改 `handlePublish` / `handleSurvey` / `handlePing` / ACL / presence writer
- `defaults.go`：只改 `MaxRecoveredPublications` 注释（值仍是 1000）
- `metrics.go`、`metrics_test.go`：本 PR 的三个恢复指标
- `client_test.go`、`client_fix_test.go`、必要时 `cluster_resume_test.go` / `cluster_offsets_test.go`
- `docs/protocol.md`（Subscribe recover 语义）
- `docs/developer/02-configuration.md`（`recover` / `recover_limit`「本版本尚未读取」那两行改成已生效）
- `docs/design/tasks/pr-03-recover.md`（完成备注）

禁止：改 proto、改 `channel_policy.go`、改 SDK 业务、`handlePublish`、presence 投递、git 写操作。

## 3. 现状（以当前 main 为准，动手前再读）

`handleConnect`（`client.go` 约 656–805）：

- 先 ACL + `AddSubscription` + presence。
- 然后 **仅当** `sub.Recover && sub.Offset > 0` 调 `broker.History`。
- History 失败：`continue`，客户端只看到空 `Connected.publications`。
- 截断：打日志，**不**设 `truncated`。
- `Connected` 只填 `SessionId/Resumed/Epoch/Publications/Subscriptions`。

`handleSubscribe`（约 1145–1218）：订阅成功后直接 `SubscribeAck{Subscriptions}`，**零恢复**。

`Node.ChannelPolicy(ch)` 已可用。`transient_only` 的 `For()` 已强制 `Recover=false`、`History=false`。

`ClusterSessionSnapshot.ChannelOffsets`：只含「至少投递过一条历史」的频道；缺省表示从未投递过历史（`cluster_state.go:67-75`）。

`isWildcard`：`hub.go:75-77`（含 `*` 即通配）。

`publicationID(channel, offset)`：`hub.go:29-31`，恢复消息必须用这个 ID。

broker epoch：`c.node.broker.(interface{ Epoch() string })`，与现网一致。

## 4. `recover.go` API

```go
type RecoverStatus int

const (
    RecoverSkipped RecoverStatus = iota // 未调 History
    RecoverOK                           // History 成功（含 0 条）
    RecoverTruncated                    // 命中请求级或策略 cap
    RecoverFailed                       // History 返回 error
    RecoverEpochReset                   // epoch 失效，已从头拉且 History 成功
)

type ChannelRecovery struct {
    Channel      string
    Status       RecoverStatus
    Publications []*clientpb.Publication
    Offset       uint64 // 见 §5 游标规则
    Epoch        string
    Err          error
}

type recoverQuota struct {
    remaining int // 本次 Connect 或 Subscribe 请求剩余条数
}

func newRecoverQuota() *recoverQuota {
    return &recoverQuota{remaining: MaxRecoveredPublications}
}

func (n *Node) recoverSubscription(
    ctx context.Context,
    sub *clientpb.Subscription,
    snapshot *ClusterSessionSnapshot, // nil = 非 resume
    quota *recoverQuota,
) ChannelRecovery
```

`resume := snapshot != nil`。不要把 `resume` 再单独当参数（避免与 snapshot 不一致）。

把 `*Publication` 转成 `*clientpb.Publication` 的逻辑从 `handleConnect` 抽到本文件（一条 broker pub → 一个 `Publication{Messages:[{Id, Channel, Offset, Payload, Metadata}]}`）。

## 5. 算法（必须按此顺序）

当前 broker epoch 记为 `currentEpoch`（无 Epoch() 则为 `""`）。

### 5.1 Skip 门（不调用 History）→ `RecoverSkipped`

任一条成立即 skip：

1. `sub == nil` 或 `sub.Channel == ""`
2. `!sub.Recover`
3. `isWildcard(sub.Channel)`
4. `pol := n.ChannelPolicy(sub.Channel)` 且（`!pol.Recover` **或** `!pol.History` **或** `pol.TransientOnly`）
5. **resume 且** `snapshot.ChannelOffsets` **没有** `sub.Channel`（即使客户端 `offset>0` 也不用客户端 offset 当 resume 续读；缺快照 offset = 从未投递，禁止倒流）

`RecoverResult`：

- `recovered=false`
- `offset=cursor`（见下；skip 时 cursor 定义：resume 缺 key 则为 0；非 resume 为 `sub.Offset`）
- `epoch=currentEpoch`
- **仅当** `sub.Recover==true` 时设 `error.code=RECOVER_SKIPPED`、`type=recover_error`
- `sub.Recover==false`：有 RecoverResult，但 **无** error

### 5.2 游标 `cursor` 与 `sinceOffset`

**resume（`snapshot != nil`）且 `ChannelOffsets[ch]` 存在：**

| 条件 | Status 倾向 | cursor | sinceOffset |
| --- | --- | --- | --- |
| `snapshot.BrokerEpoch == currentEpoch` 或任一侧 epoch 为空且不能判定失效 | 继续拉 | `ChannelOffsets[ch]` | `cursor+1` |
| 两边 epoch 都非空且不相等 | 将走 EpochReset | `0` | `0` |

**非 resume：**

| 条件 | Status 倾向 | cursor | sinceOffset |
| --- | --- | --- | --- |
| `currentEpoch != "" && sub.Epoch != "" && sub.Epoch != currentEpoch` | EpochReset | `0` | `0` |
| `currentEpoch != "" && sub.Epoch == ""` | EpochReset（与现网「无 epoch 则从头」一致，打 Warn） | `0` | `0` |
| 否则 `sub.Offset == 0` | 从头（**仅非 resume**，KD-2） | `0` | `0` |
| 否则 | 续读 | `sub.Offset` | `sub.Offset+1` |

禁止：resume + 缺 `ChannelOffsets[ch]` 走进 KD-2 从头（那是 §5.1 第 5 条 Skip）。

### 5.3 limit 与配额

```
limit = MaxRecoveredPublications
if pol.RecoverLimit > 0 && pol.RecoverLimit < limit {
    limit = pol.RecoverLimit
}
if quota.remaining < limit {
    limit = quota.remaining
}
```

`limit == 0`：不再调 History，本频道 `RecoverTruncated`，`recovered=true`，`truncated=true`，`offset=cursor`，空 publications。`recovery_truncated_total{path}` +1。

`History(ch, sinceOffset, limit)`（必须传算出的 limit，禁止再传 `0` 依赖 DefaultHistoryLimit 双封顶搞混请求级配额）。

- error → `RecoverFailed`，`recovered=false`，`error.code=RECOVER_FAILED` `type=recover_error`，`offset=cursor`。**订阅已成功，不要回滚。**
- `len(pubs) == limit`（且 limit>0）→ `RecoverTruncated`（保守视为后面还有）
- 否则若走了 epoch 重置 → `RecoverEpochReset`，当作成功可见（`recovered=true`，无 error）
- 否则 → `RecoverOK`（0 条也是 OK）

交付 ≥1 条后：`quota.remaining -= len(pubs)`。

### 5.4 `RecoverResult.offset`

| 情况 | offset |
| --- | --- |
| 交付了 ≥1 条（含截断） | **最后一条已交付 publication 的 Offset** |
| 空成功 / EpochReset 空批 | **回显 cursor**。禁止用 0 抹掉非 0 cursor |
| Skipped / Failed / 配额耗尽截断 | 回显 cursor |

### 5.5 指标与日志

每次 `recoverSubscription` 结束打一条日志：`channel`, `status`, `count`, `truncated`, `error`。

指标（`metrics.go`，namespace `messageloop`）：

| 名 | 类型 | 标签 |
| --- | --- | --- |
| `recovery_total` | CounterVec | `path`=`connect`\|`subscribe`，`result`=`ok`\|`truncated`\|`failed`\|`skipped` |
| `recovery_publications` | HistogramVec | `path` |
| `recovery_truncated_total` | CounterVec | `path` |

`path` 由调用方传入 helper（加参数 `path string`，或 `recoverSubscription` 返回后由 `handleConnect`/`handleSubscribe` 记账）。`RecoverEpochReset` 记 `result=ok`。`RecoverTruncated` 同时 `recovery_total{truncated}` 与 `recovery_truncated_total`。

`NewMetrics` 必须注册。`metrics_test.go` 补注册冒烟。

## 6. 接到 `handleConnect`

保持现有：鉴权、resume、`AddClient`、ACL、`AddSubscription`、presence。**删掉** 726–792 内联 History。

在订阅循环之后、`Send(Connected)` 之前：

1. `quota := newRecoverQuota()`
2. 恢复集合 = **有序 union**：
   - 本轮 Connect 里 **ACL 通过且已 AddSubscription** 的 `connect.Subscriptions`（保持请求顺序）
   - 再加上 `resumeSnapshot.Subscriptions` 里尚未出现的 `Channel`（只对 `resumeSnapshot != nil`）
3. 对集合中每个频道构造 `*clientpb.Subscription`：
   - 来自 Connect 的用客户端那条（含 Recover/Offset/Epoch）
   - 仅来自快照的：`{Channel: ch, Recover: true, Offset: 0}`（真正续读靠 `ChannelOffsets`，缺 key 会 Skip）
4. 逐个 `recoverSubscription`；把 publications **追加**到现有 `pubs` 切片（顺序：先请求频道，再快照独有频道）
5. `Connected`：

```go
Connected{
    SessionId:      c.SessionID(),
    Resumed:        resumed,          // 语义不变：会话接管成功
    Epoch:          currentEpoch,
    Publications:   pubs,             // 兼容旧客户端
    Subscriptions:  c.subscriptionList(),
    Recovered:      any recovered==true,
    Truncated:      any truncated==true,
    RecoverResults: results,          // 每个恢复集合频道一条
    // Presence: 不要填（PR-04）
}
```

ACL 拒绝的频道：不进恢复集合，不进 `recover_results`。

## 7. 接到 `handleSubscribe`

订阅循环（ACL / limit / saga / presence / OnSubscribed）**保持**。对 **ACL 通过且 saga 成功** 的每个 `ch`（含 alreadySubscribed 的 re-subscribe）调用同一 helper。

- 同一 `Subscribe` 请求共用一个 `quota`
- re-subscribe + `recover=true` = **合法 catch-up**，不要当 no-op
- ACL 拒绝的频道：现网已发 Error 信封并 `continue`，不进 Ack 的 `Subscriptions`，也不恢复

```go
SubscribeAck{
    Subscriptions:  subs,           // 现网：成功订阅的列表
    Publications:   pubs,
    RecoverResults: results,
    Epoch:          currentEpoch,
    // Presence: 不要填
}
```

`SubscribeAck.recovered` 没有单独 bool（proto 没有）；可见性全在 `recover_results`。

## 8. 兼容性

| 客户端 | 行为 |
| --- | --- |
| 不设 `recover` | Skip，result 无 error；与今日 Subscribe 一致 |
| 只读 `SubscribeAck.subscriptions` | 订阅仍成功；恢复消息被忽略（旧客户端本就不消费） |
| 旧 Connect 只读 `publications` | 仍填充；另外多了 `recover_results` / `recovered` / `truncated`，旧客户端忽略 |
| 新鲜 Connect/Subscribe `recover=true, offset=0` | **行为变化**：今日跳过，本 PR 从头拉（非 resume） |
| resume + `recover=true, offset=0` 且无 `ChannelOffsets` | **必须 Skip**，禁止倒 1000 条 |

`TestNode_Connect_RecoveryCap` 用 `Offset: 1`，续读语义不变，但必须 **补断言** `truncated==true` 且 `recover_results` 非空。不要删这个测试。

## 9. 必须存在的测试

放在 `recover_test.go` 和/或 `client_test.go`。对旧代码会红的路径要覆盖。

| 测试 | 断言 |
| --- | --- |
| `TestSubscribe_RecoverFromOffset` | 历史 1..10，Subscribe recover offset=5 epoch=当前 → publications 为 6..10，ID=`ch-6`…，`recovered=true` |
| `TestSubscribe_RecoverHistoryError` | fake History error → 频道仍在 hub，`recovered=false`，`RECOVER_FAILED`，Ack 不是「只有 subscriptions」 |
| `TestConnect_RecoverTruncated` | 2000 条从头（或扩展现有 Cap 测试）→ 最多 1000，`truncated=true`，offset=第 1000 条，指标 +1 |
| `TestSubscribe_RecoverFalse` | recover=false → 不调 History（用计数 fake），result 无 error |
| `TestSubscribe_RecoverWildcardSkipped` | `im.**` recover=true → 订阅成功，`RECOVER_SKIPPED`，History 调用次数 0 |
| `TestSubscribe_RecoverPolicySkipped` | `game.tick.**` transient_only + recover=true → `RECOVER_SKIPPED`，不是 `recovered=true` 空批 |
| `TestConnect_ResumeMissingOffsetSkipped` | resume 快照无 `ChannelOffsets[ch]`，客户端 recover+offset=0 → Skip，Connected.publications **不含**该频道倒流 |
| `TestConnect_ResumeSnapshotChannelNotInConnect` | 快照 `ChannelOffsets[ch]=5`、epoch 匹配，Connect.Subscriptions 未列 ch → 仍从 6 续读 |
| `TestSubscribe_ResubscribeCatchUp` | 已订 ch，再 Subscribe recover offset=5，历史有 6..8 → 收到 6..8 |
| `TestSubscribe_RecoverEmptyEchoesCursor` | recover offset=5，History 空 → `recovered=true`，`offset=5` 不是 0 |

现有 connect 恢复 ID 测试（`channel-offset`）必须继续绿。

## 10. 文档

`docs/protocol.md` Subscribe 表：

- `recover=true` 时 Ack 带 `publications` + `recover_results`
- `recovered` / `truncated` / `RECOVER_FAILED` / `RECOVER_SKIPPED` 含义
- 失败不断连、不撤销订阅
- 通配与 `history=false` / `transient_only` 为 Skipped
- `offset=0` + 非 resume = 从头；resume 缺服务端 offset = Skip

`02-configuration.md`：`recover` / `recover_limit` 从「本版本尚未读取」改为「PR-03：`recoverSubscription` 读取」。

## 11. 验收清单（实现者自检 + 主 agent 终验）

1. `handleConnect` 内联 History 循环已删除，只调 helper。
2. `handleSubscribe` 走同一 helper。
3. History error → `RECOVER_FAILED`，订阅仍在。
4. 请求级 cap 1000，截断可见 + 指标。
5. 通配 / `transient_only` / `history=false` → Skip + `RECOVER_SKIPPED`（仅当客户端要了 recover）。
6. resume 缺 `ChannelOffsets` 不从头。
7. 快照有、Connect 未列的频道仍按 offset 续读。
8. 空批回显 cursor。
9. `Connected.presence` / `SubscribeAck.presence` 未被本 PR 填值。
10. 无 proto 变更；`go test -count=1 . ./config/...` 与 `go test -race -count=1 .` 绿。

## 12. 完成报告

- 文件列表
- helper 与两处调用的文件:行
- 恢复集合 union 的实现位置
- §9 十个测试名 + 是否绿
- §11 十条自检
- 偏离与理由

## 13. 实现备注（PR-03 已完成）

实现报告见任务交付说明；本节只记录后续维护者需要知道的非显而易见决定：

- `recoverSubscription` 增加第 5 个参数 `path string`（`"connect"` / `"subscribe"`），
  用于 `recovery_total{path}` / `recovery_publications{path}` /
  `recovery_truncated_total{path}` 记账（§5.5 明确允许）。
- skip 的 `RECOVER_SKIPPED` 通过 `ChannelRecovery.Err` 携带（仅当客户端
  `recover=true`）；`recover=false` 的 skip 有 `RecoverResult` 但无 error。
- 配额耗尽（`limit==0`）不调 History，直接 `RecoverTruncated` + 空批 +
  `truncated=true`，offset 回显 cursor（§5.3）。
- 空批成功 / EpochReset 空批回显 cursor，禁止用 0 抹掉（§5.4）。
- `fakeHistoryBroker` / `fakeEpochHistoryBroker` 的 `History` 改为遵守
  limit（否则请求级 cap 与截断检测无法用 fake 验证）。
- `TestClient_RemoteResume_FallsBackToClientOffset` 按新语义改写为
  `TestClient_RemoteResume_MissingOffsetSkipped`（resume 缺
  `ChannelOffsets[ch]` 一律 Skip，不再回退客户端 offset；§5.1 第 5 条）。
