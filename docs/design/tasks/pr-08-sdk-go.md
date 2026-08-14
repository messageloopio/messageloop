# PR-08 实现规格：Go SDK v1.0 API

| 字段 | 值 |
| --- | --- |
| 标题 | `sdks/go: recover options, presence handlers, Survey(), and server ping pong` |
| 状态 | **Accepted**（2026-08-14 主 agent 终验通过） |
| 依赖 | **PR-01–PR-07 已合**（proto 字段、服务端 recover / presence / survey / server ping 已落地）。本 PR 只改 Go SDK |
| 设计来源 | [v1.0-platform-gaps.md](../v1.0-platform-gaps.md) 缺口 1 SDK、缺口 2 SDK、缺口 4 SDK、缺口 5 SDK；PR Plan PR-08 |
| 验收人 | 主 agent |

## 1. 目标

Go SDK 能用上已合入的服务端 v1.0 能力：

1. `SubscribeWith(..., WithRecover(offset, epoch))`；`SubscribeAck.publications` 与 `Connected.publications` 走同一投递路径。
2. `OnPresence` / `OnPresenceSnapshot` / `Presence(ctx, channel)`。
3. `Survey(ctx, channel, payload, timeout)` 发起频道级调查并收回答案。
4. 收到服务端 Outbound `Ping` 立即回 Inbound `Pong`（同一 `id`），否则运维打开 `ping_interval` 会把未升级客户端踢掉。

保持旧 `OnSurvey(requestID, req)` 签名可用。默认无 handler 时仍 echo 请求 payload（这是 **SDK 应答侧** 行为，与服务端已废除的 inbound echo 无关）。

本 PR **不**实现：TS SDK（PR-09）、改 proto、改服务端、改 proxy 后端、新 example 应用。

## 2. 允许改动的文件

- `sdks/go/client.go`：`Client` 接口、`handleMessage`、`handleConnected`、`handleSubscribeAck`、Survey / Presence / server Ping
- `sdks/go/presence.go`（新，类型 + 转换）和/或把类型放 `client.go`；不要把 proto 类型直接暴露成公共 API
- 必要时 `sdks/go/survey.go`（`SurveyAnswer` 等）
- `sdks/go/client_test.go`、`sdks/go/fix_regression_test.go`、必要时新 `sdks/go/*_test.go`
- `sdks/go/README.md`、`sdks/go/MIGRATION_GUIDE.md`
- `docs/developer/07-sdk-go.md`（补 recover / presence / Survey / 服务端 Ping；删掉已过时的「PingTimeout 未实施」「handlePong 空实现」）
- `docs/design/tasks/pr-08-sdk-go.md`（完成备注）

禁止：改 proto、改 `shared/` 生成物、改服务端 `*.go`（根包 / `pkg/` / `cmd/`）、改 `sdks/ts/**`、改 `sdks/go/proxy.go` 业务、git 写操作。不要改 `defaultOptions` 的 `PingInterval=30s` / `PingTimeout=10s`。

## 3. 现状（动手前再读）

`sdks/go/client.go`：

- `SubscribeWith` 只有 `WithEphemeral`、`WithSubscriptionToken`。**没有** `WithRecover`。`Subscribe(channels...)` 发出的 `Subscription` 不带 `recover`。
- 重连 `resumeSubscriptions` 已经设 `Recover=true` + offset + epoch（不要拆）。
- `handleConnected` 会把 `Connected.publications` 交给 `OnMessage` 并更新 offset。**忽略** `Connected.presence`、`recover_results`。
- `handleSubscribeAck` **只**更新本地订阅表。丢掉 `publications` / `presence` / `recover_results`。
- `handleMessage` **没有** `PresenceEvent`、`Presence`（snapshot oneof=14）、`SurveyResult`、`Ping` 分支。Outbound Ping 被静默丢弃。
- `OnSurvey` + `SendSurveyReply` 已有。无 handler 时 echo。`Survey()` 发起 API **不存在**。注释仍写「SDK does not initiate surveys」。
- 客户端自己的 ping 循环已实现：发 Inbound Ping，用 Outbound Pong 续期；`PingTimeout` 到期关 transport。`docs/developer/07-sdk-go.md` 还写着「PingTimeout 未强制实施」「handlePong 为空」——本 PR 改文档。

服务端（只读，不要改）：

- Subscribe/Connect 恢复：`SubscribeAck.publications` + `recover_results`。
- Presence：Outbound `presence_event=15`；`Connected.presence` / `SubscribeAck.presence` 为 repeated 快照；`PresenceQuery` 成功回 **单条** `OutboundMessage.presence=14`（`MakeOutboundMessage(in, ...)`，带入站 `id`）。
- Survey：Inbound `SurveyRequest{channel, timeout_ms, request_id}`；成功异步 `survey_result=18`（回填客户端 `request_id`）；同步失败顶层 Error **带入站 id**；worker 失败（集群 count 超限等）顶层 Error **可能不带回填 id**。每 session 至多 1 个 in-flight 客户端 Survey。
- `SurveyAnswer` proto **没有** `user_id`：在 `metadata.entries["user_id"]`。客户端 `SurveyResult` 与 Admin `server.v1.SurveyResult` **同名不同包**，SDK 只用 `client.v1`。

## 4. Recover

```go
func WithRecover(offset uint64, epoch string) SubscribeOption
```

设置 `Subscription.Recover=true`、`Offset`、`Epoch`。`epoch==""` 且 `offset==0` 仍发 `recover=true`（新鲜 Subscribe 从头恢复，由服务端按策略决定；resume 缺 offset 服务端会 Skipped，不是 SDK 的事）。

`handleSubscribeAck` 必须：

1. 保留今日对 `subscriptions` 的本地状态更新。
2. 把 `ack.Publications` 交给与 `handleConnected` **同一条** `wrapPublicationToMessages` + `OnMessage` 路径，并按 message offset 更新 `channelOffsets`。
3. 对每个 `ack.RecoverResults`：若 `offset>0`，`channelOffsets[channel]=offset`（空批回显 cursor，禁止用 0 抹掉已知位置）。

不要新增 `OnRecover` 回调。恢复消息就是 `OnMessage`。

`Subscribe` / `SubscribeWith` 仍是发完即返回（不 Wait Ack）。现有 fire-and-forget 语义不变。

## 5. Presence

公共类型（不要直接 `return *clientpb.PresenceEvent`）：

```go
type PresenceInfo struct {
    SessionID   string
    UserID      string
    ClientID    string
    ConnectedAt int64
}

type PresenceEvent struct {
    Channel string // 始终精确频道
    Action  string // "join" | "leave"
    Info    PresenceInfo
}

type PresenceSnapshot struct {
    Channel   string
    Clients   []PresenceInfo
    Truncated bool
    Occupancy int32
}
```

API：

```go
OnPresence(fn func(PresenceEvent))
OnPresenceSnapshot(fn func(PresenceSnapshot))
Presence(ctx context.Context, channel string) (*PresenceSnapshot, error)
```

- Outbound `presence_event` → `OnPresence`。未知 `action` 仍投递，不要丢。
- `Connected.presence` 与 `SubscribeAck.presence` 每条快照调一次 `OnPresenceSnapshot`（在 `OnConnected` / 订阅状态写回之后）。
- `Presence` 发 Inbound `PresenceQuery{channel}`。成功：匹配 **同一入站 id** 的 `OutboundMessage.presence`（oneof 14），返回快照，并再调一次 `OnPresenceSnapshot`。失败：同一 id 的顶层 Error → `error`（至少带上 code/message）。空 channel 可本地拒或交给服务端。
- 未连接 → `not connected`。
- 禁止在 `OnMessage` / `OnPresence` / `OnSurvey*` 回调里同步调用 `Presence`（与 RPC 一样会卡死读循环）。文档写明。

`Presence` 用与 `RPC` 相同的 pending 模式（按 inbound `id`）。超时/取消/Close 必须清 pending，不得泄漏。

## 6. Survey 发起

```go
type SurveyAnswer struct {
    SessionID string
    UserID    string // metadata.entries["user_id"]；没有则空
    Payload   *Message
    Error     error  // 该条 SURVEY_ANSWER_TOO_LARGE / SURVEY_FAILED 等
}

func (c *client) Survey(ctx context.Context, channel string, payload *Message, timeout time.Duration) ([]SurveyAnswer, error)
```

1. 未连接 → `not connected`。
2. 生成 `request_id`（uuid）。`timeout>0` 则 `timeout_ms=timeout.Milliseconds()`，否则发 `0`（服务端用策略上限）。
3. 发 Inbound `SurveyRequest{request_id, channel, payload, timeout_ms}`。
4. **Wait 的是 SDK 调用方 goroutine，不是 receive loop。** 结果由 `handleMessage` 填 pending。
5. 完成条件（先到先得）：
   - `SurveyResult.request_id` 匹配 → 转成 `[]SurveyAnswer`。`SurveyResult.error` 非空则整个调用返回该 error（answers 仍可附带）。
   - 顶层 Error 的 `OutboundMessage.id` 等于本次入站 id → 返回该 error。
   - 顶层 Error **没有** 可匹配 id、但 code 属于 survey 拒绝码（`SURVEY_DISABLED` / `SURVEY_TOO_MANY_SUBSCRIBERS` / `BAD_REQUEST` / `PERMISSION_DENIED` / `RATE_LIMITED` / `INTERNAL_ERROR`），且当前 **恰好一个** in-flight `Survey()`：交给它。服务端 worker 失败可能不带回填 id；每 session 服务端也只允许 1 个 in-flight。
6. `ctx` 取消/超时：返回 `ctx.Err()`，删 pending；迟到的 `SurveyResult` 丢弃。
7. `Close` / 断连：pending Survey 失败，与 RPC/PublishAck 一样。

把 `Survey` 加到 `Client` 接口。

### 应答侧兼容

```go
OnSurvey(fn func(requestID string, req *Message) (*Message, error)) // 签名不得改
OnSurveyRequest(fn func(requestID, channel string, req *Message) (*Message, error))
```

收到 Outbound `SurveyRequest`：

1. 若设了 `OnSurveyRequest` → 用它（带 `req.Channel`）。
2. 否则若设了 `OnSurvey` → 用它（忽略 channel，旧应用继续工作）。
3. 否则 echo payload（`SendSurveyReply`）。更新「mirroring the server's own default」这类过时注释。

现有 `TestClientSurveyRequestDefaultEcho`、`TestClientSurveyCustomHandlerAndSubRefresh` 必须继续绿。可改注释，不要改断言语义。

禁止在 `OnSurvey*` 里同步调 `Survey()`。

## 7. 服务端 Ping

`handleMessage` 增加 `OutboundMessage_Ping`：

1. 立刻发 Inbound `Pong`，`id` = 该条 Outbound 的 `id`（可空则仍发 Pong）。
2. 视作连接存活：走与今日 `handlePong` 相同的 `lastPong` + `pongCh`，避免「服务端在 ping、客户端自己的 PingTimeout 却把连接掐了」。

不要改客户端主动 ping 循环的默认间隔。不要把 `PingInterval` 默认改成 0。

旧客户端忽略 Outbound Ping——那是 **不升级** 的行为。本 PR 之后的 SDK 必须回 Pong。

## 8. 必须存在的测试

全部用现有 `fakeTransport`（`client_test.go`），**禁止**依赖本机 Redis / 跑起来的 server。

| 测试 | 断言 |
| --- | --- |
| `TestSDK_SubscribeWithRecover` | `SubscribeWith(ch, WithRecover(7, "ep"))` 发出的 `Subscription` 为 `recover=true, offset=7, epoch=ep` |
| `TestSDK_SubscribeAckPublications` | 推一条带 `publications` 的 `SubscribeAck` → `OnMessage` 收到对应 payload，offset 写入 `channelOffsets` |
| `TestSDK_PresenceEvent` | Outbound `PresenceEvent{action=join}` → `OnPresence` 收到 channel/session/user/client |
| `TestSDK_PresenceSnapshotOnConnected` | `Connected.presence` → `OnPresenceSnapshot` 一次 |
| `TestSDK_PresenceQuery` | `Presence(ctx, ch)` 发出 `PresenceQuery`；推回同 id 的 `Outbound.presence`；返回值 Occupancy/Clients 正确 |
| `TestSDK_PresenceQueryDenied` | 同 id 顶层 `PERMISSION_DENIED` → `Presence` 返回 error |
| `TestSDK_SurveyRoundTrip` | `Survey` 发出带 channel / request_id 的 `SurveyRequest`；推回 `SurveyResult`（同一 request_id，含 user_id metadata）→ 返回对应 `SurveyAnswer` |
| `TestSDK_SurveyTopError` | 同 id 顶层 `SURVEY_DISABLED` → `Survey` 返回 error，不挂死 |
| `TestSDK_OnSurveyCompat` | 只设旧 `OnSurvey`：Outbound SurveyRequest 仍产生 SurveyReply（现有 echo/handler 测保持绿即可，若已覆盖可引用） |
| `TestSDK_OnSurveyRequestChannel` | `OnSurveyRequest` 收到 outbound `channel`；Reply 的 request_id 正确 |
| `TestSDK_ServerPingPong` | 推 Outbound `Ping`（带 id）→ 发出 Inbound `Pong` 且 id 相同 |
| `TestSDK_ServerPingKeepsAlive` | 开很短 `PingTimeout`；只推服务端 Ping、不推 Pong：连接在超时窗口内 **不被** 客户端自己掐掉 |

现有 SDK 测试必须继续绿（尤其 `TestClientPongTimeoutClosesTransport`、`TestClientPongKeepsConnectionAlive`、resume `Recover=true`、survey echo）。

## 9. 文档

`07-sdk-go.md` + `sdks/go/README.md`：

- `WithRecover`；SubscribeAck 恢复消息走 `OnMessage`。
- `OnPresence` / `OnPresenceSnapshot` / `Presence`。
- `Survey` + `OnSurveyRequest`；旧 `OnSurvey` 仍可用；user_id 在 answer。
- 服务端 Ping → 客户端 Pong。打开 server `ping_interval` 必须用本版本 SDK。
- 纠正 PingTimeout / handlePong 过时描述。
- 写明：不要在收包回调里同步调 `RPC` / `Survey` / `Presence`。

`MIGRATION_GUIDE.md` 加一小节：新增 API；`OnSurvey` 不破坏。

## 10. 验收清单

1. `WithRecover` 发出 recover/offset/epoch；SubscribeAck publications 进 `OnMessage`。
2. Presence 事件 + Connect/Ack 快照 + `Presence()` 查询。
3. `Survey()` 按 request_id 收回 `SurveyResult`；同步顶层错误不挂。
4. 旧 `OnSurvey` 签名与默认 echo 仍绿。
5. Outbound Ping → 同 id Inbound Pong；可当作存活。
6. 无 proto / 服务端 / TS 改动。
7. `cd sdks/go && go test -count=1 ./...` 与 `go test -race -count=1 ./...` 绿。
8. 根包 `go test -count=1 .` 仍绿（本 PR 不该碰到它；回归即可）。

## 11. 完成报告

- 文件列表
- `WithRecover` / `handleSubscribeAck` publications / `OnPresence` / `Presence` / `Survey` / server Ping（文件:行）
- §8 每个测试：过/失败
- §10 八条 + 证据
- `go test` 摘要
- 偏离与理由

## 12. 实现备注（落地后填写）

**已实现（PR-08 完成）**：

- `sdks/go/client.go`：`WithRecover`（§4）、`handleSubscribeAck` publications/recover_results/presence（§4/§5）、`OnPresence` / `OnPresenceSnapshot` / `Presence`（§5，pending 模式按入站 id）、`Survey` / `OnSurveyRequest`（§6，pending 按入站 id + `SurveyResult.request_id`；同 id 顶层 Error + 无 id 拒绝码单 in-flight 兜底）、`handleServerPing`（§7，同 id Pong + `lastPong`/`pongCh` 存活）。`Close` 与断连清理 pending Presence/Survey。
- 新文件 `sdks/go/presence.go`（`PresenceInfo` / `PresenceEvent` / `PresenceSnapshot` + 转换）、`sdks/go/survey.go`（`SurveyAnswer`，`UserID` 读 `metadata.entries["user_id"]`）、`sdks/go/pr08_test.go`（§8 全部 12 个测试）。
- 文档：`docs/developer/07-sdk-go.md`、`sdks/go/README.md`、`sdks/go/MIGRATION_GUIDE.md` 补 recover / presence / Survey / 服务端 Ping，纠正「PingTimeout 未实施」「handlePong 为空」。
- 未动：proto、服务端、`sdks/ts`、`sdks/go/proxy.go`、`PingInterval=30s` / `PingTimeout=10s` 默认值；`OnSurvey` 签名不变。
- 验证：`go test -count=1 ./...`、`go test -race -count=1 ./...`、根包 `go test -count=1 .` 全绿。
