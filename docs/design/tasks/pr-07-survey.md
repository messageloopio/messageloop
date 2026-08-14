# PR-07 实现规格：客户端发起 Survey

| 字段 | 值 |
| --- | --- |
| 标题 | `server: honor client SurveyRequest as channel-scoped Node.Survey` |
| 状态 | **Accepted**（2026-08-14 主 agent 终验通过） |
| 依赖 | **PR-01 已合**（`SurveyRequest.channel/timeout_ms`、Outbound `survey_result=18`）。**PR-02 已合**（`ChannelPolicy.Survey` 默认 false）。**PR-04a 已合**（`sessionCoversChannel`） |
| 设计来源 | [v1.0-platform-gaps.md](../v1.0-platform-gaps.md) 缺口 5、KD-5、KD-6、KD-15 |
| 验收人 | 主 agent |

## 1. 目标

Inbound `SurveyRequest` 变成真正的「向频道订阅者发问并异步收回 `SurveyResult`」。废除 echo。**禁止在读循环里 `Survey.Wait`**（KD-15：发起方自己的 Reply 是同一连接下一条入站）。

默认安全：策略 `survey=false` + `CanSurvey` 默认 deny。Admin `Node.Survey` **不加**订阅者上限、不走 `CanSurvey`。

本 PR **不**实现：Go/TS SDK `Survey()`（PR-08/09）、改 proto、改 Admin Survey RPC。

## 2. 允许改动的文件

- `client.go`：重写 `handleSurvey`；in-flight + 每 session limiter；**禁止**改 `handlePublish` / presence / ping 业务
- `acl.go`：`CanSurvey`；`ACLRule.AllowSurvey`；`aclEntry` 解析
- `config/config.go`、`config/config_test.go`：`ACLRule.AllowSurvey`；`NewNode` 拷规则时带上（`node.go` 组装 ACL 处）
- `node.go`：`sendSurveyRequest` 填 `channel`；`countMatchingSubscribers`；**不要改** Admin 调用的 `Survey` 签名与「无订阅者上限」
- `cluster_commands.go`：`handleClusterSurveyCommand` 识别 `count_only`
- `survey.go` / `defaults.go`：单条/整包体积上限常量与截断
- `metrics.go` / `metrics_test.go`：可选 `survey_client_total{result}`；至少要有失败路径可观测（可复用现有或新 CounterVec）
- `survey_test.go`：**改写 echo**。禁止再把 inbound SurveyRequest 喂回 `HandleMessage`
- `cluster_redis_integration_test.go`：客户端/集群 Survey 测试按 outbound request_id 再 Reply
- `config-example.yaml`、`docs/developer/02-configuration.md`、`docs/protocol.md`
- `docs/design/tasks/pr-07-survey.md`（完成备注）

禁止：改 proto、改 SDK、改 Admin Survey 入口语义、git 写操作。不要改 `NewNode` 签名。

## 3. 现状（动手前再读）

- `handleSurvey`（`client.go:1448`）存 `lastSurveyRequestID` 然后 **echo SurveyReply**。不调 `Node.Survey`。
- Admin：`Node.Survey` → `localSurvey` + `BroadcastCommand(ClusterCommandSurvey)`。已有 expected session、`maxActiveSurveys=1000`、`surveySendTimeout=10s`。
- `sendSurveyRequest` **不填 channel**（`node.go:901-907`）。
- `ChannelPolicy.Survey` 默认 false，订阅路径尚未读取。
- `sessionCoversChannel` 已在 PR-04a。
- Proto：`SurveyRequest.channel=4`、`timeout_ms=5`；Outbound `survey_result=18`；`SurveyAnswer` **没有** `user_id` 字段（不要改 proto）。需要时放进 `answer.metadata.entries["user_id"]`。
- `survey_test.go` / Redis 集成测依赖 echo。本 PR 必须改写成：读 outbound `SurveyRequest` → inbound `SurveyReply`。

## 4. `handleSurvey`（必须按此顺序，同步部分）

全部失败用**顶层 Error**（不断连、不撤销订阅），然后 return。成功的同步路径：**不 Wait**，return nil，worker 稍后 `Send(SurveyResult)`。

1. `channel==""` 或 `isWildcard(channel)` → `BAD_REQUEST` / `request_error`（禁止对 pattern 发起）。
2. `!sessionCoversChannel(ch)` → `PERMISSION_DENIED` / `acl_error`。
3. `!ChannelPolicy(ch).Survey` → `SURVEY_DISABLED` / `policy_error`。
4. `acl != nil && !acl.CanSurvey(ch, user)` → `PERMISSION_DENIED` / `acl_error`。
5. 该 session 已有 in-flight 客户端 Survey → `RATE_LIMITED` / `rate_limit`。
6. 每 session limiter（1/s，burst 1）超限 → `RATE_LIMITED`。
7. `timeout = clamp(req.TimeoutMs, 100ms, min(policy.MaxSurveyTimeout || 5s, 10s))`。`TimeoutMs<=0` 用策略上限（默认 5s）。
8. 本地 `len(GetMatchingSubscribers(ch)) > MaxSurveySubscribers`（策略可覆盖，默认 256）→ 同步 `SURVEY_TOO_MANY_SUBSCRIBERS` / `survey_error`。**零条** outbound SurveyRequest。
9. 标记 in-flight，**`go` worker**，return。

Worker：

1. `n.countMatchingSubscribers(ctx, ch)`。`> MaxSurveySubscribers` → `Send` 顶层 Error 或 `SurveyResult{error=SURVEY_TOO_MANY_SUBSCRIBERS}`（选一种并在全测试一致；推荐顶层 Error 与同步路径相同），**零下发**。
2. 否则 `Node.Survey`（现有聚合，含本节点 + 集群）。
3. 截断超大 answer / 整包。
4. `Send(Outbound SurveyResult{request_id, channel, answers})`。发起方若也在订阅列表，其 Reply 由已空闲的读循环进 `handleSurveyReply`。
5. `defer` 清 in-flight。

`request_id`：客户端应带；空则服务端生成 uuid，结果里回填同一 id。

## 5. `CanSurvey`（默认拒绝）

与 `CanSubscribe` 同结构（denyAll 短路、最后一条带名单的规则胜出），但：

```go
allowed := false // 与 CanSubscribe 相反
if entry.allowSurvey != nil {
    allowed = entry.wildcardSurvey || entry.allowSurvey[userID]
}
```

- 无规则匹配 → **拒绝**
- 规则只写了 `allow_subscribe`、没写 `allow_survey` → **不打开**
- `deny_all` → 拒绝
- **无 SurveyAcl proxy**（v1.0）
- Admin `Node.Survey` **不**调用 `CanSurvey`

`config.ACLRule` 与根包 `ACLRule` 都加 `AllowSurvey []string`。`NewNode` 拷规则时带上。

## 6. 集群 `count_only`

```go
func (n *Node) countMatchingSubscribers(ctx context.Context, ch string) (int, error)
```

- 无 cluster：`len(GetMatchingSubscribers(ch))`
- 有 cluster：本地计数 + `BroadcastCommand(ClusterCommandSurvey, Metadata["count_only"]="true", exclude_self=true)`，各节点**只**返回本地 matching 数，**不**调 `localSurvey`

`handleClusterSurveyCommand`：见 `count_only` 则 `result.Metadata["count"]=strconv.Itoa(n)`，return。计数与发送之间允许微小 TOCTOU，文档接受。

Admin `Node.Survey` 路径**不要**加这道门。

## 7. 下发与体积

`sendSurveyRequest` 必须填 `SurveyRequest.Channel = survey.Channel()`。

```
MaxSurveyAnswerBytes = 4096
MaxSurveyResultBytes = 256 * 1024
```

单条 payload 超限：该条 `error.code=SURVEY_ANSWER_TOO_LARGE`、payload 清空。整包编码后超限：后续 answer 改 error、不再附 payload。`user_id` 放 `metadata.entries["user_id"]`（proto 无该字段）。

## 8. 必须存在的测试

| 测试 | 断言 |
| --- | --- |
| `TestClientSurvey_RoundTrip` | 策略+ACL 打开，A/B 订精确频道。A Survey。B 根据 **outbound** SurveyRequest 回 Reply。A 收到 `SurveyResult` 含 B（及可选 A） |
| `TestClientSurvey_DefaultDisabled` | 默认配置 → `SURVEY_DISABLED`，History/broker 无 SurveyRequest 下发 |
| `TestClientSurvey_NotCovered` | 未覆盖频道 → `PERMISSION_DENIED`，零下发 |
| `TestClientSurvey_TooManyLocal` | 单节点 >256 订阅者 → `SURVEY_TOO_MANY_SUBSCRIBERS`，零 outbound SurveyRequest |
| `TestClientSurvey_CountOnlyCluster` | 有 cluster 时 count_only 命令被发出且 **不** localSurvey（可用 fake bus 记 metadata） |
| `TestClientSurvey_WildcardCoverExact` | 订 `game.**` 可 Survey(`game.room.1`)；Survey(`game.**`) → `BAD_REQUEST` |
| `TestClientSurvey_NoDeadlockSelfAnswer` | 发起方也在订阅列表。worker 跑着时读循环仍能 HandleMessage(SurveyReply + Ping)。结果异步到达 |
| `TestClientSurvey_AnswerTooLarge` | 8KiB 应答 → 该条 `SURVEY_ANSWER_TOO_LARGE`、无 payload |
| `TestClientSurvey_EchoGone` | inbound SurveyRequest **不再**产生 SurveyReply echo |
| `TestACL_CanSurveyDefaultDeny` | 无规则 / 只写 allow_subscribe → false；`allow_survey: ["*"]` → true |

现有 **Admin** Survey 测试必须继续绿（无订阅者上限）。改写依赖 echo 的客户端/集群测试，不要删集群聚合覆盖。

## 9. 文档

`protocol.md`：客户端可发起；Ack 是异步 `survey_result`；失败码；默认关；禁止 pattern；读循环不阻塞。

`02-configuration.md`：`channels.policies[].survey` 改为「PR-07 读取」；ACL `allow_survey`。

## 10. 验收清单

1. echo 已删；无 channel → BAD_REQUEST。
2. 默认 survey 关 + CanSurvey deny。
3. 未覆盖 → PERMISSION_DENIED，零下发。
4. 超订阅者上限：同步或预检拒绝，零 outbound SurveyRequest。
5. 集群 count_only 不下发；Admin 不受此门。
6. handleSurvey 不 Wait；self-answer 不死锁。
7. sendSurveyRequest 带 channel。
8. 无 proto 变更；`go test -count=1 . ./config/...` 与 `go test -race -count=1 .` 绿。

## 11. 完成报告

- 文件列表
- `handleSurvey` / worker / `CanSurvey` / `countMatchingSubscribers` / `count_only` 分支 文件:行
- §8 每个测试：过/失败
- §10 八条 + 证据
- 偏离与理由

## 12. 实现备注（落地后填写）

1. `handleSurvey`（`client.go:1448`）按 §4 顺序同步校验：BAD_REQUEST（空/通配）→ PERMISSION_DENIED（未覆盖）→ SURVEY_DISABLED（策略）→ PERMISSION_DENIED（`CanSurvey`）→ RATE_LIMITED（in-flight / 1s burst-1 limiter）→ 超时钳制 → 本地订阅者数快路径 → `go` worker，return nil，读循环零阻塞。worker（`client.go` `runSurveyWorker`）：集群 count 预检 → `Node.Survey` → 截断 → 异步 `Send(SurveyResult)`，`defer` 清 in-flight。
2. `CanSurvey`（`acl.go`）与 `CanSubscribe` 同结构但默认拒绝：无规则 / 只写 `allow_subscribe` 不打开；`deny_all` 短路；最后一条带 `allow_survey` 名单的规则胜出。Admin `Node.Survey` 不走 `CanSurvey`。
3. `countMatchingSubscribers`（`node.go`）：无 cluster 返回本地 `len(GetMatchingSubscribers)`；有 cluster 时 `BroadcastCommand(ClusterCommandSurvey, count_only=true, exclude_self=true)`，`handleClusterSurveyCommand`（`cluster_commands.go`）见 `count_only` 只回 `Metadata["count"]`，不调 `localSurvey`。失败节点跳过（Warn），TOCTOU 接受。
4. 体积：`MaxSurveyAnswerBytes=4096`、`MaxSurveyResultBytes=256KiB`（`defaults.go`）。超限 answer 转 `SURVEY_ANSWER_TOO_LARGE`、payload 清空；整包以 `proto.Marshal` 编码尺寸衡量，超出后后续 answer 改 error、去 payload（仍超则整体丢弃）。`user_id` 放 `metadata.entries["user_id"]`（proto 无该字段，仅本地 session 可查）。
5. 指标：`messageloop_survey_client_total{result}`（`metrics.go`），result 为 `ok` 或顶层错误码。
6. 偏离：`handleSurvey` 的 payload 转换放在校验之后、worker 之前（§4 未列，取最不阻塞的位置）；集群 count 预检中失败节点被跳过而非拒绝（文档化软门）；`request_id` 空则服务端生成，结果回填同一 id（§4）。

