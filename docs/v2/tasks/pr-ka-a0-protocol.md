# PR-KA-A0 实现规格：冻结独立版本（v2）协议字段

| 字段 | 值 |
| --- | --- |
| 标题 | `proto: add messageloop client/server/shared v2 for independent kernel` |
| 状态 | **Accepted**（2026-08-16 主 agent 终验通过，尚未 commit） |
| 依赖 | 无。可与 PR-KA-A1 并行（无文件交集） |
| 设计来源 | [kernel-architecture.md](../kernel-architecture.md) Protocol、KD-K11、KD-K16、KD-K17、KD-K22、**KD-K31** |
| 验收人 | 主 agent |

## 1. 目标

为独立内核版本一次写入 **v2 protobuf 并 `buf generate`**。冻号后禁止改号。

本 PR **只加 v2 源文件与生成物，不改任何运行时行为**。不得把服务端 / SDK 从 `client/v1` 切到 `v2`。现有测试必须全绿。

**KD-K31**：这是新协议世代，不是在 v1 上加字段。v1 源文件本 PR **不得修改、不得删除**。

## 2. 允许改动的文件

- `protocol/shared/v2/types.proto`（新）
- `protocol/shared/v2/errors.proto`（新）
- `protocol/client/v2/service.proto`（新）
- `protocol/server/v2/api.proto`（新）
- `shared/genproto/shared/v2/**`、`shared/genproto/client/v2/**`、`shared/genproto/server/v2/**`（`task generate-protocol` 产物）
- `sdks/ts/src/proto/**` 下由同一条 generate 写出的 **v2** 文件
- `docs/v2/tasks/pr-ka-a0-protocol.md`（完成备注）

禁止：改 `protocol/**/v1/**`、`client.go`、`hub.go`、`node.go`、`sdks/go/**`（除生成物）、`sdks/ts/src/client/**`、配置、测试业务。禁止新增除 `MessageLoop` / `APIService` 已有形状之外的 RPC。禁止 git commit / tag / push。

`git diff` 只应出现上述 proto + genproto + ts proto。

## 3. 包与 go_package

| proto package | 路径 | `option go_package` |
| --- | --- | --- |
| `messageloop.shared.v2` | `protocol/shared/v2/` | `github.com/messageloopio/messageloop/shared/genproto/shared/v2;sharedv2` |
| `messageloop.client.v2` | `protocol/client/v2/` | `github.com/messageloopio/messageloop/shared/genproto/client/v2;clientv2` |
| `messageloop.server.v2` | `protocol/server/v2/` | `github.com/messageloopio/messageloop/shared/genproto/server/v2;serverv2` |

`server/v2/api.proto` 需要与 v1 相同的 lint ignore（`PACKAGE_DIRECTORY_MATCH`、`FILE_SAME_GO_PACKAGE`）。

client import：`shared/v2/errors.proto`、`shared/v2/types.proto`。

## 4. 必须写入的字段号

号必须与下表 **完全一致**。多一个、少一个、改号都算失败。

### 4.1 `shared.v2.Position`

```
message Position {
  string stream_epoch = 1;
  optional uint64 offset = 2; // 缺省 = unset；禁止用 0 表示 unset 或「从头」
}
```

### 4.2 `shared.v2.GapReason`（enum）

| 名 | 号 |
| --- | --- |
| `GAP_REASON_UNSPECIFIED` | 0 |
| `GAP_REASON_NONE` | 1 |
| `GAP_REASON_HEAD_TRIMMED` | 2 |
| `GAP_REASON_EMPTY_EXPIRED` | 3 |
| `GAP_REASON_EPOCH_RESET` | 4 |

### 4.3 `shared.v2.Payload` / `Metadata`

与 v1 **同形同号**：`content_type=1`，oneof `json=2` / `binary=3` / `text=4`；`Metadata.entries=1`。

### 4.4 `shared.v2.Error`

字段同 v1：`code=1` `type=2` `message=3` `metadata=4`。

`Error` 注释列出本版本字符串 code（不是 enum）：

`AUTH_REQUIRED`, `AUTH_ERROR`, `RATE_LIMITED`, `RPC_TIMEOUT`, `PROXY_ERROR`, `NO_PROXY`, `BAD_REQUEST`, `PERMISSION_DENIED`, `POLICY_DENIED`, `RECOVER_FAILED`, `RECOVER_SKIPPED`, `SURVEY_DISABLED`, `SURVEY_TOO_MANY_SUBSCRIBERS`, `PATTERN_NOT_ROUTABLE`。

**禁止**出现 `ACL_DENIED`。

### 4.5 client.v2 `InboundMessage`

`id=1` `time=2`。oneof：

| 字段 | 号 | 类型 |
| --- | --- | --- |
| `connect` | 3 | `Connect` |
| `subscribe` | 4 | `Subscribe` |
| `unsubscribe` | 5 | `Unsubscribe` |
| `publish` | 6 | `Publish` |
| `rpc_request` | 7 | `RpcRequest` |
| `sub_refresh` | 8 | `SubRefresh` |
| `survey_request` | 9 | `SurveyRequest` |
| `survey_reply` | 10 | `SurveyReply` |
| `ping` | 11 | `Ping` |
| `pong` | 12 | `Pong` |
| `presence_query` | 13 | `PresenceQuery` |

无 reserved 空洞。不要为已取消的 PresenceStats 留号。

### 4.6 client.v2 `OutboundMessage`

`id=1` `time=2`。oneof：

| 字段 | 号 | 类型 |
| --- | --- | --- |
| `error` | 3 | `messageloop.shared.v2.Error` |
| `connected` | 4 | `Connected` |
| `subscribe_ack` | 5 | `SubscribeAck` |
| `unsubscribe_ack` | 6 | `UnsubscribeAck` |
| `publish_ack` | 7 | `PublishAck` |
| `publication` | 8 | `Publication` |
| `recover_complete` | 9 | `RecoverComplete` |
| `rpc_reply` | 10 | `RpcReply` |
| `sub_refresh_ack` | 11 | `SubRefreshAck` |
| `survey_request` | 12 | `SurveyRequest` |
| `survey_reply` | 13 | `SurveyReply` |
| `survey_result` | 14 | `SurveyResult` |
| `presence` | 15 | `PresenceSnapshot` |
| `presence_event` | 16 | `PresenceEvent` |
| `ping` | 17 | `Ping` |
| `pong` | 18 | `Pong` |

### 4.7 Connect / Connected / Subscription

`Connect`：

| 字段 | 号 |
| --- | --- |
| `client_id` | 1 |
| `client_type` | 2 |
| `token` | 3 |
| `version` | 4 |
| `subscriptions` | 5 |
| `session_id` | 6 |
| `caps` | 7（`repeated string`） |

`Connected`（**禁止** `publications` / `recover_results` / 聚合 `recovered`）：

| 字段 | 号 |
| --- | --- |
| `session_id` | 1 |
| `subscriptions` | 2（必须带回 `ephemeral`） |
| `resumed` | 3 |
| `stream_epoch` | 4 |
| `accepted_caps` | 5（`repeated string`） |

`Subscription`：

| 字段 | 号 |
| --- | --- |
| `channel` | 1 |
| `ephemeral` | 2 |
| `token` | 3 |
| `recover` | 4 |
| `cursor` | 5（`shared.v2.Position`） |
| `fresh` | 6（bool；显式从头。禁止用 `offset==0` 表示从头） |

**不要**在 Subscription 上保留 v1 的 `offset` / `epoch` 标量。

### 4.8 Subscribe / Ack / RecoverComplete

`Subscribe`：`subscriptions=1`。  
`Unsubscribe`：`subscriptions=1`。  
`UnsubscribeAck`：`subscriptions=1`。

`RecoverState` enum：`RECOVER_STATE_UNSPECIFIED=0` `NONE=1` `PENDING=2` `SKIPPED=3` `FAILED=4`。

`SubscribeAck`：

| 字段 | 号 |
| --- | --- |
| `subscriptions` | 1 |
| `recover` | 2（`RecoverState`；批量里任一频道 pending 则 PENDING） |
| `stream_epoch` | 3 |
| `presence` | 4（`repeated PresenceSnapshot`，仅精确且非 ephemeral） |
| `error` | 5（仅 recover=FAILED 时） |

`RecoverComplete`：

| 字段 | 号 |
| --- | --- |
| `channel` | 1 |
| `position` | 2（`Position`） |
| `truncated` | 3 |
| `gap` | 4（bool） |
| `gap_reason` | 5（`GapReason`） |
| `error` | 6（可选；SKIPPED/FAILED 细节） |

**禁止**定义 `RecoverResult`。

### 4.9 Publish / Message / Publication

`Publish`：`channel=1` `payload=2` `metadata=3` `token=4` `transient=5` `idempotency_key=6`。

`PublishAck`：`id=1` `position=2`（`Position`；transient / 无历史 → `offset` 缺省）。**不要** `uint64 offset`。

`Message`：`id=1` `channel=2` `position=3`（`Position`）`payload=4` `metadata=5` `replay=6`（bool）。

`Publication`：`messages=1`。

### 4.10 RPC / Survey / SubRefresh / Presence / Ping

- `RpcRequest`：`channel=1` `method=2` `payload=3` `metadata=4`
- `RpcReply`：`request_id=1` `payload=2` `metadata=3` `error=4`
- `SubRefresh`：`channels=1`；`SubRefreshAck` 空
- `SurveyRequest`：`request_id=1` `payload=2` `metadata=3` `channel=4` `timeout_ms=5`
- `SurveyReply`：`request_id=1` `payload=2` `metadata=3` `error=4`
- `SurveyResult`：`request_id=1` `channel=2` `answers=3` `error=4`（定义在 `client.v2`，禁止引用 `server.v2.SurveyResult`）
- `SurveyAnswer`：`session_id=1` `payload=2` `metadata=3` `error=4`
- `PresenceQuery`：`channel=1`
- `PresenceInfo`：`session_id=1` `user_id=2` `client_id=3`（**仅** Connect.client_id / 设备）`connected_at=4`
- `PresenceSnapshot`：`channel=1` `clients=2` `truncated=3` `occupancy=4`
- `PresenceEvent`：`channel=1` `action=2` `info=3` `gen=4`（`uint64` OccupancyGen）
- `Ping {}` `Pong {}`

### 4.11 server.v2 `APIService`

RPC 集合与 v1 相同：`Publish` `Disconnect` `Subscribe` `Unsubscribe` `Survey` `GetPresence` `GetHistory` `GetChannels`。

与 v1 的 **故意差异**：

- `PresenceInfo`：**不要**「`client_id` 实际是 session」双义。形状与 client.v2 相同（`session_id=1` `user_id=2` `client_id=3` `connected_at=4`）。
- `GetHistoryRequest`：`channel=1` `since=2`（`Position`）`limit=3`。不要 `since_offset`。
- `HistoryPublication`：`position=1` `payload=2` `time=3` `id=4` `metadata=5`。不要 `is_text` / 裸 `offset`。
- `Destination` / Disconnect / Subscribe 已含 `users`：`sessions=1` `channels=2` `users=3`；Disconnect `sessions=1` `code=2` `reason=3` `users=4`；Subscribe/Unsubscribe `session_id=1` `channels=2` `user_id=3`。
- Survey 消息同 v1 号（`request_id=1` `channel=2` `payload=3` `metadata=4` `timeout_ms=5`；Result `session_id=1` …）。

## 5. 生成

```bash
task generate-protocol
```

无 task 时读 `Taskfile.yml` 的 `generate-protocol`，在仓库根执行 `buf generate`。

## 6. 必须存在的检查（可用测试或脚本，至少手检写进报告）

1. `protocol/**/v1/**` 的 `git diff` 为空。
2. 生成了 `shared/genproto/client/v2/*.go`、`shared/genproto/shared/v2/*.go`、`shared/genproto/server/v2/*.go`。
3. `Connected` 生成类型 **没有** `Publications` / `RecoverResults` 字段。
4. `Subscription` 生成类型 **没有** 标量 `Offset` / `Epoch`；有 `Cursor`（`*Position`）与 `Fresh`。
5. `PresenceEvent` 有 `Gen`。
6. `Error` 注释不含 `ACL_DENIED`。
7. `go build ./...` 与 `go test ./...` 全绿（运行时仍用 v1）。

## 7. 验收清单

1. 字段号与 §4 逐表一致。
2. 只改 §2 路径。
3. 未改运行时、未切 import。
4. v1 proto 未动。
5. generate 成功且生成物入库。
6. `go test ./...` 绿。

## 8. 完成报告

- 改动文件列表
- generate 命令与是否成功
- §7 逐条：过/失败 + 证据
- 任何偏离（应无）

## 9. 实现备注（完成后由实现者填写）

- 2026-08-16 已实现：新增 `protocol/{shared,client,server}/v2/*.proto`（4 个源文件），运行 `task generate-protocol`（`buf generate`）成功，生成 Go（`shared/genproto/**/v2/*.pb.go`、`*_grpc.pb.go`）与 TS（`sdks/ts/src/proto/**/v2/*_pb.ts`）。
- 字段号与 §4 完全一致；`Connected` 无 publications / recover_results；未定义 `RecoverResult`；`Subscription` 为 `cursor`（`Position`）+ `fresh`；`PresenceInfo.client_id` 为设备/端；`Error` 注释无 `ACL_DENIED`。
- `openapiv2` 插件会顺带重写 v1 swagger.json（与本次改动无关的既有漂移），已 `git checkout` 还原，未纳入改动。
- `go build ./...`、`go test ./...` 全绿；`sdks/ts` `tsc --noEmit` 通过。v1 proto 与运行时零改动。
