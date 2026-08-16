# PR-KA-A2 实现规格：History gap、Publish 成功合同、memory Interest

| 字段 | 值 |
| --- | --- |
| 标题 | `broker: history gap page, keep stream on pubsub fail, memory interest` |
| 状态 | **Accepted**（2026-08-16 主 agent 终验通过，尚未 commit） |
| 依赖 | 无运行时依赖 A0。建议在已合入 A1 的 `v2` 分支上做 |
| 设计来源 | [kernel-architecture.md](../kernel-architecture.md) StreamLog / Gap 合同、KD-K12、KD-K14、KD-K31 |
| 验收人 | 主 agent |

## 1. 目标

把数据面合同落到现行 `Broker`（还不拆 Session、不切 v2 信封、不改 Redis `PSubscribe *`）：

1. `History` 返回带 **可检测 gap** 的页，不再只给 `[]*Publication`。
2. Redis `XADD` 成功后 **禁止** 因 `PUBLISH` 失败 `XDel`；`Publish` 返回已分配 offset 且 **error=nil**。
3. memory `Publish` / `PublishTransient`：**先**写入（若需要）**再**按 Interest 调 handler；handler 错误/panic **不得**否定 Publish。
4. memory Interest 与 Redis `interested()` 同语义：精确计数 **或** 通配 matcher 命中。

**不做：** LiveBus 编译 / 去掉 `PSubscribe *`（A3）；稠密 seq 中洞（Q8）；流式 `RecoverComplete`（B3）；切运行时到 `clientv2`。

## 2. 允许改动的文件

- `broker.go`：`History` 签名、`HistoryPage` / `HistoryGapReason`
- `broker_memory.go`、`broker_memory_test.go`
- `pkg/redisbroker/redis.go`（Publish 去掉 XDel；Subscribe 已有 matcher，勿改 PSubscribe）
- `pkg/redisbroker/history.go`、`history_test.go`
- `pkg/redisbroker/options.go`（仅 retained 键前缀，如需要）
- `pkg/redisbroker/publish_transient_test.go` 及因签名编译失败的 redis 测试
- `recover.go`：消费 `HistoryPage`；空批+gap → 不得标 RecoverOK
- `recover_test.go`
- `pkg/grpcstream/api_handler.go`：`GetHistory` 只取 `Publications`（Admin v1 无 gap 字段）
- `metrics.go` / `metrics_test.go`：可选 `recovery_gap_total{reason}`
- **所有因此编译失败的 `Broker` fake**（`*_test.go`）：只改 `History` 签名与返回
- `docs/developer/01-architecture.md` Broker / History 段（若写了旧签名或「PUBLISH 失败则 XDel」）
- `docs/v2/tasks/pr-ka-a2-history.md`（完成备注）

禁止：改 proto、SDK 业务、`hub.go` 扇出算法、`PSubscribe` 模式、A1 fencing、git commit/push。

## 3. 现状（动手前再读）

- `Broker.History(ch, since, limit) ([]*Publication, error)`：`since` 含等；空切片不是 error。
- memory `Publish`：写环后 **同步** 调 handler，把 handler error 返回给调用方；**不看** `subs`。`Publish("im.room.1")` 在只 `Subscribe("im.**")` 时仍会 handler。
- Redis `interested()`：精确 `subscribed[ch]` 或 `matcher.Lookup(ch)`（`redis.go`）。
- Redis `Publish`：`XADD` 后 `PUBLISH` 失败则 `XDel` 并 return err（`redis.go` 约 274–283）。
- `recover.go`：`len==limit` → Truncated；否则空批也是 RecoverOK。
- Redis offset = `ts<<20|seq`，**不能**用相邻差判断中洞。`checkCatchUpGap` 注释已承认裁头不可检。A2 **不**承诺中洞。

## 4. `HistoryPage`

写在 `broker.go`：

```go
type HistoryGapReason int

const (
    HistoryGapNone HistoryGapReason = iota
    HistoryGapHeadTrimmed
    HistoryGapEmptyExpired
)

type HistoryPage struct {
    Publications  []*Publication
    Truncated     bool            // len(Publications)==limit 且 limit>0
    Gap           bool            // GapReason != None
    GapReason     HistoryGapReason
    FirstRetained uint64          // 0 = 未知 / 从未发布
}

func (p *HistoryPage) Pubs() []*Publication {
    if p == nil {
        return nil
    }
    return p.Publications
}
```

`Broker.History` 改为：

```go
History(ch string, sinceOffset uint64, limit int) (*HistoryPage, error)
```

`limit<=0` 仍用 `DefaultHistoryLimit`。error 仅传输/存储失败；空页不是 error。

**不要**在 `History` 里做 epoch_reset（那是 `recover.go` 已有逻辑）。

## 5. Gap 判定（memory 与 Redis 同一张表）

`sinceOffset == 0`：视为从头读。`GapReason=None`（除非实现想标 epoch，本 PR 不要）。

`sinceOffset > 0`：

| 条件 | GapReason |
| --- | --- |
| 没有任何保留条目，且无法证明「保留区仍覆盖 since」 | `EmptyExpired`（宁可假阳性：从未发布 + 客户端乱填 offset 也标这个） |
| 有条目，且 `FirstRetained > sinceOffset` | `HeadTrimmed` |
| 有条目，且 `FirstRetained <= sinceOffset`（或 FirstRetained==0 但第一条 offset <= since） | `None` |

`Gap = (GapReason != None)`。  
`Truncated = limit>0 && len(Publications)==limit`。

禁止：`sinceOffset>0` ∧ 空批 ∧ `GapReason==None`（这就是「假装追上」）。

## 6. memory 实现

### 6.1 Interest

与 Redis 对齐：

- 精确：`subs[ch]++` / `--`（今日已有）。
- 通配（`strings.Contains(ch,"*")`，与 hub `isWildcard` 一致）：refcount + `topics.NewCSTrieMatcher()`。`Subscribe` 首次挂 pattern，`Unsubscribe` 计到 0 摘掉。
- `interested(concrete)`：`subs[concrete]>0` **或** `len(matcher.Lookup(concrete))>0`。
- `Publish` / `PublishTransient`：写完（transient 不写环）后 **仅当** `interested(ch)` 才调 handler。

### 6.2 Publish 合同

1. ValidateTopic。
2. 写环、分配 offset（与今日相同）。
3. `defer recover` 包住 handler：panic 记日志，**仍** `return offset, nil`。
4. handler 返回 error：记日志，**仍** `return offset, nil`。
5. 无 handler 或未 interested：`return offset, nil`。

`PublishTransient`：不写环；未 interested 则不调 handler，return nil；handler 错/panic 也 return nil（可打日志）。

### 6.3 History + 水位

- `FirstRetained` = 环最旧条目的 Offset（`count>0`）；`count==0` 则为 0。
- 频道对象在 `nextOff>0` 时 **不要**因「无订阅且 count==0」删掉（否则丢「曾经发过」）。无订阅且 `nextOff==0 && count==0` 才删。
- `sinceOffset>0` 且 `count==0` → `EmptyExpired`。
- `sinceOffset>0` 且 `count>0` 且最旧 Offset `> sinceOffset` → `HeadTrimmed`。
- 无 history 对象且 `sinceOffset>0` → 空页 + `EmptyExpired`。
- 无 history 对象且 `sinceOffset==0` → 空页 + `None`。

## 7. Redis 实现

### 7.1 去掉 XDel

`Publish`：`XADD` 成功并算出 offset 后，`PUBLISH` 失败只 Warn/Error 日志，**不** `XDel`，`return offset, nil`。

`XADD` 自己失败：仍 return 0, err。

### 7.2 `first_retained`

键：`opts.StreamPrefix + "retained:" + ch`，值为十进制 `uint64` 字符串。每次成功 `XADD` 后：

1. `XINFO STREAM` 读 first-entry id → `parseStreamOffset`，`SET` retained。
2. 失败则：若 retained 不存在，`SET` 为本条 offset。
3. `Expire` retained，TTL 与该次 stream `Expire` 相同。

### 7.3 History

1. 照旧 `XRangeN` 取条目。
2. `GET retained`；没有则用本批第一条 offset（若有）。
3. 按 §5 填 `HistoryPage`。
4. `sinceOffset>0` 且 0 条 → `EmptyExpired`（stream 过期或从未有：假阳性允许）。

不要改 `runPubSub` 的 `PSubscribe prefix*`。`checkCatchUpGap` 可留作内部指标，不是本 PR 的 History 合同。

## 8. `recover.go`

```
page, err := n.broker.History(...)
if err != nil → RecoverFailed（同今日）
pubs := page.Pubs()
// 填 Publications / Offset 同今日
if page != nil && page.Gap && len(pubs) == 0:
    res.Status = RecoverTruncated   // 未追上；offset 回显 cursor
else if len(pubs) == limit:
    RecoverTruncated
else if epochReset:
    RecoverEpochReset
else:
    RecoverOK
```

有消息且 gap（头裁但仍读到更新的）：按条数走 Truncated/OK，另打 gap 指标。不要当 Failed。

可选：`RecoveryTotal` 或新 counter `recovery_gap_total{reason=head_trimmed|empty_expired}`。

## 9. 调用方

- `api_handler.go` `GetHistory`：用 `page.Pubs()`，忽略 gap（Admin v1 无该字段）。
- 所有 `Broker` fake：返回 `(*HistoryPage, error)`。空历史用 `&HistoryPage{}` 或 `(nil, nil)`（调用方必须 `Pubs()`）。
- 现有 `History(...)` 测试改为看 `page.Pubs()`，并补 gap 断言（§10）。

## 10. 必须存在的测试

1. **memory handler 失败不否定 Publish**：handler return error 或 panic，`Publish` 仍返回非 0 offset、err=nil；`History` 能读到该条。
2. **memory 无 Interest 不调 handler**：未 Subscribe 时 Publish / PublishTransient，handler 计数为 0；写历史的 Publish 仍进环。
3. **memory 通配 Interest**：`Subscribe("forex.*")` 后 `Publish("forex.eur")` 调 handler；`Publish("stocks.us")` 不调。
4. **memory 头裁**：HistorySize=2，发 offset 1,2,3；`History(ch, 1, 0)` → 不含 1，`GapReason=HeadTrimmed`，`FirstRetained==2`。
5. **memory 空+since**：发过再让环空不了就用「无对象 + since>0」或 count=0+nextOff>0 → `EmptyExpired`，空批。
6. **memory since=0 未发布**：空页、`GapReason=None`。
7. **Redis 不 XDel**（有 Redis）：注入或用可失败的 pubsub 不是必须；**源码断言**：`Publish` 函数体在 `PUBLISH` 失败分支 **零** `XDel`。另：`XADD` 成功路径在 PUBLISH 失败时仍 `return offset, nil`（可用 miniredis / 关 pubsub 的集成，若环境无 Redis：单测 spy client 或只做源码+memory 合同，Redis 测标 skip）。优先 `pkg/redisbroker` 里已有 redis 测试辅助。
8. **Redis History since>0 空流**：`EmptyExpired`。
9. **recover 空批+gap**：fake History 返回空页 `Gap=true, EmptyExpired`，`recoverSubscription` → `RecoverTruncated`，不是 RecoverOK。
10. 现有 broker / recover / channel_policy History 测试全绿。

禁止用固定长 Sleep 代替同步点。

## 11. 验收清单

1. `Broker.History` 签名为 `(*HistoryPage, error)`。
2. memory / Redis 实现 §5 表；`since>0` 空批不得 `None`。
3. memory 未 interested 不调 handler；通配与精确都对。
4. memory Publish 在 handler 错/panic 后仍成功且历史在。
5. Redis `Publish` 无 XDel-on-pubsub-fail；成功 XADD 后对调用方 err=nil。
6. `recover.go` 空批+gap → Truncated。
7. 热路径零 `PSubscribe` 改动。
8. `go test ./...`；`go test -race ./pkg/redisbroker ./`（根包 recover）。

## 12. 完成报告

- 文件列表
- §11 逐条证据
- 测试命令与结果
- 偏离（应无；Redis 环境 skip 须写明）

## 13. 实现备注（完成后填写）

**状态：已实现（2026-08-16）**

改动文件：

- `broker.go`：`HistoryGapReason` / `HistoryPage`（含 `Pubs()`）、`Broker.History` 签名改为 `(*HistoryPage, error)`；更新投递错误合同注释（memory 不再回传 handler 错误）
- `broker_memory.go`：通配 Interest（refcount + CSTrieMatcher，`isWildcard` 与 hub 一致）、`interested()`、`Publish`/`PublishTransient` 仅 interested 时调 handler 且 handler 错/panic 不否定发布、`Unsubscribe` 仅 `count==0 && nextOff==0` 回收、`History` 返回 gap 页
- `broker_memory_test.go`：既有用例改 `page.Pubs()`；新增 §10.1–10.6 用例与通配 refcount、Truncated 标志用例
- `pkg/redisbroker/redis.go`：`Publish` 去掉 XDel 分支（PUBLISH 失败只 Warn，`return offset, nil`）；新增 `updateFirstRetained`（XINFO first-entry → SET，失败时若键不存在 SET 为本条 offset，TTL 与 stream 相同）
- `pkg/redisbroker/history.go`：`getHistory` 返回 `*HistoryPage`，按 §5 填 gap；`FirstRetained` 取 retained 标记，缺失时回退本批第一条
- `pkg/redisbroker/history_test.go`：既有用例改页；新增 §10.8 与 HeadTrimmed、marker 无 gap 用例
- `pkg/redisbroker/ready_test.go`：旧 `TestRedisBroker_Publish_PubSubFailureRollsBackStream` 按 §10.7 改为 `TestRedisBroker_Publish_PubSubFailureKeepsStream`（failPublishHook 注入，断言 `(offset, nil)` 且 stream 条目保留）——唯一超出 §2 文件清单的改动，系删除旧合同测试的必然结果（见完成报告「偏离」）
- `pkg/redisbroker/publish_transient_test.go`：改 `page.Pubs()`
- `recover.go`：消费 `HistoryPage`；空批+gap → `RecoverTruncated`（offset 回显 cursor）；gap 打 `recovery_gap_total{reason}` 指标
- `recover_test.go`：`countingRecoveryBroker` 签名；新增 `gapHistoryBroker` 与 §10.9 用例
- `pkg/grpcstream/api_handler.go`：`GetHistory` 用 `page.Pubs()`
- `pkg/grpcstream/api_handler_test.go`：fake 签名
- `metrics.go` / `metrics_test.go`：新增 `recovery_gap_total{reason}`
- `channel_policy_test.go`、`node_test.go`、`client_test.go`、`client_fix_test.go`、`health_test.go`、`cluster_resume_test.go`、`cluster_offsets_test.go`、`presence_test.go`：History 调用点/fake 签名
- `pkg/grpcstream/integration_test.go`：改 `page.Pubs()`
- `docs/developer/01-architecture.md`：Broker 表与两实现段落更新

验收清单（§11）逐条证据与测试命令见完成报告；本地用 Docker 起了 Redis 7，`pkg/redisbroker` 集成测试全部真实跑过（含 `TestRedisBroker_Publish_PubSubFailureKeepsStream`），无 skip。
