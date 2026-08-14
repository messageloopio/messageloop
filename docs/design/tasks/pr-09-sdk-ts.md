# PR-09 实现规格：TypeScript SDK v1.0 API

| 字段 | 值 |
| --- | --- |
| 标题 | `sdks/ts: recover, presence, client survey, and pong for server ping` |
| 状态 | **Accepted**（2026-08-15 主 agent 终验通过） |
| 依赖 | **PR-01–PR-08 已合**。服务端与 Go SDK 已落地；本 PR 只改 TypeScript SDK 与其文档 |
| 设计来源 | [v1.0-platform-gaps.md](../v1.0-platform-gaps.md) 缺口 1/2/4/5 的 SDK 小节；PR Plan PR-09。语义与 [PR-08 Go SDK](pr-08-sdk-go.md) **对齐** |
| 验收人 | 主 agent |

## 1. 目标

TypeScript SDK（`@messageloop/sdk`）能用上已合入的服务端 v1.0 能力，并与 Go SDK 行为对照：

1. `subscribe({ channel, recover, offset, epoch })`；`SubscribeAck.publications` 与 `Connected.publications` 走同一投递路径。
2. `onPresence` / `onPresenceSnapshot` / `presence(channel)`。
3. `survey(channel, payload, timeoutMs?)` 发起频道级调查。
4. 收到服务端 Outbound `Ping` 立即回 Inbound `Pong`（同一 `id`），并当作存活证据。

保持旧 `onSurvey(requestId, request)` 签名可用。默认无 handler 时仍 echo 请求 payload（SDK 应答侧；与服务端已废除的 inbound echo 无关）。

重连后的 Subscribe（`resubscribeAllChannels`，session **未** resumed）必须带 `recover=true` + 已记录的 offset/epoch。今日这条路径不带 recover。

本 PR **不**实现：gRPC 传输、改 proto 源、改服务端、改 Go SDK、新 example 应用。

## 2. 允许改动的文件

- `sdks/ts/src/client/client.ts`、`types.ts`、必要时 `options.ts`、`index.ts`
- `sdks/ts/src/message/converters.ts`、`index.ts`：`parseOutboundMessage` 新 case；`createSubscribeMessage` 带 recover；`createPongMessage` / `createPresenceQueryMessage` / `createSurveyRequestMessage`
- 必要时 `sdks/ts/src/message/message.ts`（不要把 proto 类型做成公共 API）
- `sdks/ts/src/index.ts`：导出新类型与构造函数
- `sdks/ts/test/*.test.ts`（新 `pr09.test.ts` 亦可）
- `sdks/ts/README.md`
- `docs/developer/08-sdk-ts.md`（删掉「不支持 Survey」；补 recover / presence / survey / 服务端 Ping）
- `docs/design/tasks/pr-09-sdk-ts.md`（完成备注）

禁止：改 `protocol/**`、`shared/**`、服务端 `*.go`、`sdks/go/**`、`sdks/ts/src/proto/**`（生成物已含字段，不要手改）、git 写操作。不要改默认 `pingInterval=30000` / `pingTimeout=10000` / `autoReconnect=true`。

## 3. 现状（动手前再读）

`sdks/ts/src/client/client.ts`：

- `subscribe(...ChannelOrSpec[])` 只认 `channel` + `token`。`createSubscribeMessage` **不设** `recover`/`offset`/`epoch`。
- 重连 `connect()` 在 Connect 上带 `recover: isReconnecting`（保留）。未 resumed 时 `resubscribeAllChannels()` 再发 Subscribe，**不带 recover**。
- `handleMessage` 的 `connected` 会投递 `publications`。**没有** `subscribeAck` 分支 → Ack 里的恢复消息 / presence 快照被丢掉。
- `parseOutboundMessage` 不认识 `presence` / `presenceEvent` / `ping` / `surveyResult`；未知信封变成 `type:"error"`。
- `onSurvey` + 默认 echo 已有。无 `survey()` 发起。无 Presence API。Outbound Ping 被静默丢弃（或当成 unknown error）。
- `08-sdk-ts.md` 仍写「不支持 Survey」——本 PR 改文档。

对照表（写进测试注释即可）：

| 能力 | Go（PR-08） | TS（本 PR） |
| --- | --- | --- |
| 恢复订阅 | `SubscribeWith(ch, WithRecover(off, epoch))` | `subscribe({ channel, recover: true, offset, epoch })` |
| 恢复投递 | `SubscribeAck.publications` → `OnMessage` | 同上 → `onMessage` / `addMessageHandler` |
| Presence 事件 | `OnPresence` | `onPresence` |
| Presence 快照 | `OnPresenceSnapshot` | `onPresenceSnapshot` |
| Presence 查询 | `Presence(ctx, ch)` | `presence(ch): Promise<PresenceSnapshot>` |
| 发起 Survey | `Survey(ctx, ch, payload, timeout)` | `survey(ch, payload, timeoutMs?): Promise<SurveyAnswer[]>` |
| 应答 Survey | `OnSurvey` / `OnSurveyRequest` | `onSurvey` / `onSurveyRequest` |
| 服务端 Ping | 同 id Inbound Pong + lastPong | 同 id Pong + 清 `pingTimeoutTimer` |
| user_id | `metadata.entries["user_id"]` | 同 |

## 4. Recover

扩展 `SubscriptionSpec`（`types.ts` 与 `converters.ts` 必须一致，不要两份漂移）：

```ts
interface SubscriptionSpec {
  channel: string;
  token?: string;
  recover?: boolean;
  offset?: bigint; // 或 number；发出去是 proto uint64。选一种并在全文件一致，推荐 bigint（已有 channelOffsets）
  epoch?: string;
}
```

`createSubscribeMessage`：spec 上 `recover===true` 时写 `recover=true`、`offset`、`epoch`。`offset=0n` / `epoch=""` 仍发 `recover=true`（新鲜 Subscribe 从头，由服务端策略决定）。

`subscribe(...)` 把 spec 原样交给 `createSubscribeMessage`。字符串频道行为不变（不 recover）。

`handleMessage` 增加 `subscribeAck`：

1. 保留/更新本地 `subscribedChannels`（token 回退与 Connected 相同）。
2. `ack.publications` 走与 `Connected.publications` **同一条** `deliverMessages`。
3. 每个 `ack.recoverResults`：若 `offset > 0`，`channelOffsets.set(channel, offset)`。空批（0）不得抹掉已知位置。
4. 每个 `ack.presence` 调一次 `onPresenceSnapshot`（在订阅表写回之后）。

`Connected.recoverResults` 同样写回 cursor（与 Go 对齐）。`Connected.presence` 每条调一次 snapshot 回调。

`resubscribeAllChannels`：对每个已订频道发 `recover: true`、`offset: channelOffsets.get(ch) ?? 0n`、`epoch`（当前 session epoch）。这是「重连 Subscribe 带 recover」。

不要新增 `onRecover`。恢复消息就是 `onMessage`。

## 5. Presence

公共类型（不要直接 export proto）：

```ts
interface PresenceInfo {
  sessionId: string;
  userId: string;
  clientId: string;
  connectedAt: bigint; // 或 number；与 proto int64 对齐即可，文档写清
}

interface PresenceEvent {
  channel: string; // 始终精确频道
  action: string;  // "join" | "leave"；未知 action 仍投递
  info: PresenceInfo;
}

interface PresenceSnapshot {
  channel: string;
  clients: PresenceInfo[];
  truncated: boolean;
  occupancy: number;
}
```

API（加到 `IClient`）：

```ts
onPresence(handler: (event: PresenceEvent) => void): void;
onPresenceSnapshot(handler: (snap: PresenceSnapshot) => void): void;
presence(channel: string): Promise<PresenceSnapshot>;
```

- Outbound `presenceEvent` → `onPresence`。
- `Connected.presence` / `SubscribeAck.presence` → 每条一次 `onPresenceSnapshot`。
- `presence(channel)` 发 Inbound `PresenceQuery{channel}`。成功：匹配 **同一入站 id** 的 `OutboundMessage.presence`（oneof 14），resolve 快照，并再调一次 `onPresenceSnapshot`。失败：同 id 顶层 Error → reject（error 带 `code`）。空 channel 交给服务端。
- 未连接 → reject `not connected`。
- 断连 / `close()` 必须 reject pending Presence，不得泄漏。
- 禁止在 `onMessage` / `onPresence*` / `onSurvey*` 里同步 `await presence()` / `survey()` / `rpc()`。文档写明。

## 6. Survey 发起

```ts
interface SurveyAnswer {
  sessionId: string;
  userId: string; // metadata.entries["user_id"]；没有则 ""
  payload?: Message;
  error?: Error;  // 该条 SURVEY_ANSWER_TOO_LARGE / SURVEY_FAILED
}

survey(channel: string, payload: Message | null, timeoutMs?: number): Promise<SurveyAnswer[]>;
```

1. 未连接 → reject。
2. 生成 `requestId`（可用现有 `generateMessageId` 或 uuid）。`timeoutMs>0` 则设 `timeoutMs`，否则发 `0`。
3. 发 Inbound `SurveyRequest{requestId, channel, payload, timeoutMs}`。
4. **Wait 在调用方 Promise**，不要卡住 `handleMessage`。
5. 完成（先到先得）：
   - `SurveyResult.requestId` 匹配 → resolve `SurveyAnswer[]`。`SurveyResult.error` 非空则 reject 该 error（answers 可挂在 `error.answers` 上，或 reject 同时丢 answers；选一种并在测试里固定。推荐：reject 且 `error.answers = answers`）。
   - 顶层 Error 的 envelope `id` 等于本次入站 id → reject。
   - 顶层 Error **没有** 可匹配 id、但 code 属于 `SURVEY_DISABLED` / `SURVEY_TOO_MANY_SUBSCRIBERS` / `BAD_REQUEST` / `PERMISSION_DENIED` / `RATE_LIMITED` / `INTERNAL_ERROR`，且当前 **恰好一个** in-flight `survey()`：交给它。
6. `close` / 断连：pending survey reject。迟到的 `SurveyResult` 丢弃。

### 应答侧兼容

```ts
onSurvey(handler: (requestId: string, request: Message) => Message | Promise<Message>): void; // 签名不得改
onSurveyRequest(handler: (requestId: string, channel: string, request: Message) => Message | Promise<Message>): void;
```

收到 Outbound `SurveyRequest`：

1. 设了 `onSurveyRequest` → 用它（带 `req.channel`）。
2. 否则设了 `onSurvey` → 用它（忽略 channel）。
3. 否则 echo payload。

更新「mirroring the server's own default」这类过时注释。现有 `onSurvey` 测试必须继续绿。

## 7. 服务端 Ping

`parseOutboundMessage` + `handleMessage` 增加 `ping`：

1. 立刻发 Inbound `Pong`，`id` = 该条 Outbound 的 `id`（可空则仍发 Pong）。需要 `createPongMessage(id)`。
2. 视作存活：清掉当前 `pingTimeoutTimer`（与收到 `pong` 相同），避免「服务端在 ping、客户端自己的 pingTimeout 却把连接掐了」。

不要改默认 `pingInterval` / `pingTimeout` / `autoReconnect`。

## 8. 必须存在的测试

全部用 jest + mock `transport.send` / 直接调 `handleMessage`（与现有 `test/client.test.ts`、`protocol.test.ts` 同一风格）。**禁止**依赖本机 Redis / 已启动的 server。

| 测试 | 断言 |
| --- | --- |
| `subscribe with recover` | `subscribe({ channel, recover: true, offset: 7n, epoch: "ep" })` 发出 `recover=true, offset=7, epoch=ep` |
| `subscribeAck publications` | 推带 `publications` 的 `subscribeAck` → `onMessage` 收到 payload，offset 写入 `channelOffsets` |
| `presence event` | Outbound `presenceEvent{action:join}` → `onPresence` 收到 channel/session/user/client |
| `presence snapshot on connected` | `Connected.presence` → `onPresenceSnapshot` 一次 |
| `presence snapshot on subscribeAck` | `SubscribeAck.presence` → `onPresenceSnapshot` 一次 |
| `presence query` | `presence(ch)` 发出 `PresenceQuery`；推回同 id `presence`；返回 Occupancy/Clients；再触发 snapshot |
| `presence query denied` | 同 id 顶层 `PERMISSION_DENIED` → reject |
| `survey round trip` | `survey` 发出带 channel / requestId 的 `SurveyRequest`；推回同 requestId 的 `SurveyResult`（user_id metadata）→ 对应 `SurveyAnswer` |
| `survey top error` | 同 id 顶层 `SURVEY_DISABLED` → reject，不挂死 |
| `onSurvey compat` | 只设旧 `onSurvey`：Outbound SurveyRequest 仍产生 SurveyReply |
| `onSurveyRequest channel` | `onSurveyRequest` 收到 outbound `channel`；Reply 的 requestId 正确 |
| `server ping pong` | 推 Outbound `Ping`（带 id）→ 发出 Inbound `Pong` 且 id 相同 |
| `resubscribe sends recover` | `resubscribeAllChannels`（或模拟未 resumed 重连）发出的 Subscribe 带 `recover=true` + 已存 offset |

现有测试必须继续绿（token subscribe、publish transient、onSurvey echo、reconnect recover on Connect）。

每个新测试文件顶部用注释贴 §3 对照表（设计要求）。

## 9. 文档

`08-sdk-ts.md` + `sdks/ts/README.md`：

- `SubscriptionSpec.recover/offset/epoch`；SubscribeAck 恢复消息走 `onMessage`。
- `onPresence` / `onPresenceSnapshot` / `presence`。
- `survey` + `onSurveyRequest`；旧 `onSurvey` 仍可用；user_id 在 answer。
- 服务端 Ping → 客户端 Pong。打开 `server.heartbeat.ping_interval` 必须用本版本 SDK。
- 删掉「不支持 Survey」。
- 写明：不要在收包回调里 `await rpc/survey/presence`。
- 与 Go 的命名对照（`WithRecover` ↔ spec 字段；`Survey` ↔ `survey`）。

## 10. 验收清单

1. recover spec 发出 recover/offset/epoch；SubscribeAck publications 进 `onMessage`。
2. Presence 事件 + Connect/Ack 快照 + `presence()` 查询。
3. `survey()` 按 requestId 收回 `SurveyResult`；同步顶层错误不挂。
4. 旧 `onSurvey` 签名与默认 echo 仍绿。
5. Outbound Ping → 同 id Inbound Pong；可当作存活。
6. 未 resumed 重连 Subscribe 带 recover。
7. 无 proto / 服务端 / Go SDK 改动。
8. `cd sdks/ts && npm test` 绿。

## 11. 完成报告

- 文件列表
- `SubscriptionSpec.recover` / `subscribeAck` publications / `onPresence` / `presence` / `survey` / server Ping / `resubscribeAllChannels`（文件:行）
- §8 每个测试：过/失败
- §10 八条 + 证据
- `npm test` 摘要
- 偏离与理由

## 12. 实现备注（落地后填写）

实现完成（2026-08-15）。要点：

- `SubscriptionSpec` 单一来源定义在 `sdks/ts/src/client/types.ts`（含 `recover?/offset?/epoch?`），`message/converters.ts` 以 `export type` 复用同一份定义，避免两份漂移。
- `createSubscribeMessage` 在 `spec.recover === true` 时写 `recover=true` + `offset` + `epoch`；`offset=0n`/`epoch=""` 仍发 `recover=true`（Go `WithRecover` 对齐）。
- `handleMessage` 新增 `subscribeAck`（订阅表写回 → `deliverMessages` → `applyRecoverResults` → 每条 presence 快照一次 `onPresenceSnapshot`）、`presence`、`presenceEvent`、`ping`、`surveyResult` 分支；`Connected` 同样回写 `recoverResults` 并派发 presence 快照。
- `presence()`/`survey()` 的等待都在调用方 Promise（`pendingPresence`/`pendingSurvey` 按入站 id 登记，注册先于发送）；错误信封先按 id 路由给 RPC → presence → survey，无 id 且 code 属于调查拒绝码时仅当恰好一个 in-flight survey 才路由（与 Go 一致）；`close()`/断连 reject 全部挂起项，迟到 `SurveyResult` 丢弃。
- 收到 Outbound `Ping` 立即回同 id Inbound `Pong` 并清 `pingTimeoutTimer`（存活证据）。
- `resubscribeAllChannels` 对每个已订频道发 `recover: true` + `channelOffsets` + 当前 session epoch（未 resumed 重连路径）。
- 公共 API 不直接暴露 proto 类型：`PresenceInfo/PresenceEvent/PresenceSnapshot/SurveyAnswer` 均为 SDK 层封装；`SurveyAnswer.userId` 读 `metadata.entries["user_id"]`。
- 文档：`08-sdk-ts.md` 与 `sdks/ts/README.md` 已删「不支持 Survey」并补齐 recover/presence/survey/服务端 Ping 说明。
- 验证：`cd sdks/ts && npm test` 全绿（5 个 suite / 76 个用例，含既有 token subscribe、publish transient、onSurvey echo、reconnect recover-on-Connect 回归）。
