# PR-01 实现规格：冻结 v1.0 协议字段

| 字段 | 值 |
| --- | --- |
| 标题 | `proto: add v1.0 recovery, presence, survey, heartbeat, and user destination fields` |
| 状态 | **Accepted**（2026-08-14 主 agent 终验通过） |
| 依赖 | 无。可与 PR-02 并行 |
| 设计来源 | [v1.0-platform-gaps.md](../v1.0-platform-gaps.md) §API / Interface Changes、缺口 1–5 协议提案 |
| 验收人 | 主 agent（实现完成后做严格验收，见文末清单） |

## 1. 目标

一次把 v1.0 要用的 **protobuf 字段号全部写进源 proto 并重新生成代码**。冻号后禁止改号。

本 PR **只加字段与生成物，不改任何运行时行为**。现有测试必须保持全绿。服务端/SDK 可以编译，但 **不得** 开始解析新字段做业务。

## 2. 允许改动的文件

只许改这些路径（生成物算在内）：

- `protocol/client/v1/service.proto`
- `protocol/server/v1/api.proto`
- `protocol/shared/v1/errors.proto`（仅允许在 `Error` 上加注释，列出字符串 code 约定；**不要**改字段）
- `shared/genproto/**`（`task generate-protocol` / `buf generate` 产物）
- `sdks/ts/src/proto/**`（同一条 generate 命令写出）
- `docs/design/tasks/pr-01-protocol.md`（若需回写完成备注）

禁止：改 `client.go`、`hub.go`、`node.go`、`sdks/go/client.go`、`sdks/ts/src/client/**`、配置、测试业务逻辑。禁止新增 RPC。禁止加 `DisconnectUser` / `SubscribeUser`。

若 generate 后 `git diff` 只应出现 proto + genproto + ts proto。

## 3. 现状（以源码为准，实现前再读一遍）

`protocol/client/v1/service.proto`：

- `InboundMessage.oneof` 已用 **3–11**（Connect … Ping）
- `OutboundMessage.oneof` 已用 **3–13**（Error … Pong）
- `Connected` 已用 **1–5**
- `SubscribeAck` 已用 **1**
- `Publish` 已用 **1–5**
- `SurveyRequest` 已用 **1–3**（无 `channel`）
- 已有空 message `Ping {}`、`Pong {}` —— **复用**，不要新建同名类型

`protocol/server/v1/api.proto`：

- `Destination` 已用 **1–2**（sessions, channels）
- `DisconnectRequest` 已用 **1–3**
- `SubscribeRequest` / `UnsubscribeRequest` 已用 **1–2**
- `PresenceInfo` 已用 **1–3**（`client_id` 语义仍是 session ID，本 PR 不改语义）

## 4. 必须写入的字段号（P0 冻结表）

号必须与下表 **完全一致**。多一个、少一个、改号都算失败。

### 4.1 client.v1 `InboundMessage.oneof`

| 字段 | 号 | 类型 |
| --- | --- | --- |
| `presence_query` | **12** | `PresenceQuery` |
| *(reserved)* | **13** | `reserved 13;` 注释：`PresenceStatsQuery` v1.x |
| `pong` | **14** | `Pong`（已有类型；客户端应答服务端 Ping） |

`reserved 13;` 写在 oneof **外面**（protobuf 的 reserved 不能写在 oneof 里）。oneof 内不要占 13。

### 4.2 client.v1 `OutboundMessage.oneof`

| 字段 | 号 | 类型 |
| --- | --- | --- |
| `presence` | **14** | `PresenceSnapshot` |
| `presence_event` | **15** | `PresenceEvent` |
| *(reserved)* | **16** | `reserved 16;` 注释：`PresenceStats` v1.x |
| `ping` | **17** | `Ping`（已有类型；服务端发起） |
| `survey_result` | **18** | **`messageloop.client.v1.SurveyResult`**（本文件新 message，禁止 import server.v1） |

### 4.3 改现有 client messages

```protobuf
message Connected {
  string session_id = 1;
  repeated Subscription subscriptions = 2;
  repeated Publication publications = 3;
  bool resumed = 4;
  string epoch = 5;
  bool recovered = 6;                       // 至少一个频道 recovered=true
  bool truncated = 7;                       // 至少一个频道 truncated
  repeated RecoverResult recover_results = 8;
  repeated PresenceSnapshot presence = 9;
}

message SubscribeAck {
  repeated Subscription subscriptions = 1;
  repeated Publication publications = 2;
  repeated RecoverResult recover_results = 3;
  string epoch = 4;
  repeated PresenceSnapshot presence = 5;
}

message Publish {
  string channel = 1;
  messageloop.shared.v1.Payload payload = 2;
  messageloop.shared.v1.Metadata metadata = 3;
  string token = 4;
  bool transient = 5;
  // v1.x: server ignores this in v1.0. Number frozen.
  string idempotency_key = 6;
}

message SurveyRequest {
  string request_id = 1;
  messageloop.shared.v1.Payload payload = 2;
  messageloop.shared.v1.Metadata metadata = 3;
  string channel = 4;
  int32 timeout_ms = 5;
}
```

### 4.4 新 client messages（必须原样，含字段号）

```protobuf
message RecoverResult {
  string channel = 1;
  bool recovered = 2;     // History 调用成功（含 0 条）
  bool truncated = 3;     // 命中 cap
  uint64 offset = 4;      // 有消息=最后一条；空批=回显 cursor
  string epoch = 5;
  messageloop.shared.v1.Error error = 6; // RECOVER_FAILED / RECOVER_SKIPPED
}

message PresenceQuery {
  string channel = 1; // 精确频道
}

message PresenceInfo {
  string session_id = 1;
  string user_id = 2;
  string client_id = 3;   // Connect.client_id（设备/端），不是 session
  int64 connected_at = 4;
}

message PresenceSnapshot {
  string channel = 1;
  repeated PresenceInfo clients = 2;
  bool truncated = 3;
  int32 occupancy = 4;
}

message PresenceEvent {
  string channel = 1;     // 始终是精确频道
  string action = 2;      // "join" | "leave"
  PresenceInfo info = 3;
  reserved 4;             // occupancy v1.x
}

// 客户端 Survey 汇总。与 server.v1.SurveyResult 同名不同包。
message SurveyResult {
  string request_id = 1;
  string channel = 2;
  repeated SurveyAnswer answers = 3;
  messageloop.shared.v1.Error error = 4;
}

message SurveyAnswer {
  string session_id = 1;
  messageloop.shared.v1.Payload payload = 2;
  messageloop.shared.v1.Metadata metadata = 3;
  messageloop.shared.v1.Error error = 4;
}
```

`Ping` / `Pong` 保持空 message，双向复用。

### 4.5 server.v1（只扩展现有消息）

```protobuf
message Destination {
  repeated string sessions = 1;
  repeated string channels = 2;
  repeated string users = 3;
}

message DisconnectRequest {
  repeated string sessions = 1;
  uint32 code = 2;
  string reason = 3;
  repeated string users = 4;
}

message SubscribeRequest {
  string session_id = 1;
  repeated string channels = 2;
  string user_id = 3;
}

message UnsubscribeRequest {
  string session_id = 1;
  repeated string channels = 2;
  string user_id = 3;
}

message PresenceInfo {
  string client_id = 1;          // 兼容：仍为 session ID
  string user_id = 2;
  int64 connected_at = 3;
  string session_id = 4;         // 与 client_id 相同的正式名
  string connect_client_id = 5;  // Connect.client_id
}
```

不加新 RPC。`PublishResponse` 保持空。

### 4.6 shared.v1.Error 注释（可选但建议）

在 `Error.code` 旁注释，**不改字段号**：

```
// Well-known string codes (not an enum): AUTH_REQUIRED, AUTH_ERROR,
// ACL_DENIED, ACL_ERROR, RATE_LIMITED, RPC_TIMEOUT, PROXY_ERROR,
// BAD_REQUEST, PERMISSION_DENIED, POLICY_DENIED,
// RECOVER_FAILED, RECOVER_SKIPPED.
```

## 5. 生成步骤

仓库用 Task + buf（见 `Taskfile.yml`、`buf.gen.yaml`）：

```bash
# 已装过则跳过 init
task init
task generate-protocol
```

等价：`buf generate`（在仓库根，配置 `buf.gen.yaml`）。

产出必须提交：

- `shared/genproto/client/v1/*`
- `shared/genproto/server/v1/*`
- 若 shared proto 注释变了：`shared/genproto/shared/v1/*`
- `sdks/ts/src/proto/client/v1/service_pb.ts` 等

生成后：

```bash
go build ./...
go test ./...
cd sdks/go && go test ./...
```

TS 侧至少保证 `cd sdks/ts && npx tsc -p tsconfig.json --noEmit` 或现有 `npm test` 不因类型破裂。本 PR 不要改 `sdks/ts/src/client/**` 去消费新字段。

## 6. 明确不做

- 不实现恢复、presence 投递、Survey、心跳、按 user。
- 不改 `handleSurvey` echo。
- 不更新 `docs/protocol.md` 正文（留给 PR-10；proto 注释可以写）。
- 不做 git commit / tag / push（除非调用方另行要求）。

## 7. 验收清单（实现者自检 + 主 agent 终验）

1. `InboundMessage` 无 field 13；文件中有 `reserved 13`。
2. `OutboundMessage` 无 field 16；文件中有 `reserved 16`。
3. `Connected.presence = 9`，`SubscribeAck.presence = 5`。
4. `InboundMessage.pong = 14`，`OutboundMessage.ping = 17`，`survey_result = 18`。
5. `PresenceEvent` 有 `reserved 4`。
6. `Publish.idempotency_key = 6`（或等价 `reserved 6`；优先实字段 + 注释 ignored）。
7. `SurveyRequest.channel = 4`，`timeout_ms = 5`。
8. client.v1 存在 `RecoverResult` / `PresenceQuery` / `PresenceInfo` / `PresenceSnapshot` / `PresenceEvent` / `SurveyResult` / `SurveyAnswer`，字段号与 §4.4 一致。
9. server.v1 `Destination.users=3`，`DisconnectRequest.users=4`，`SubscribeRequest.user_id=3`，`UnsubscribeRequest.user_id=3`，`PresenceInfo.session_id=4`，`connect_client_id=5`。
10. **没有**新的 `service APIService` RPC。
11. `go build ./...` 与 `go test ./...` 全绿（行为不变）。
12. `git diff` 不含 `client.go` / `hub.go` / `sdks/go/client.go` / `sdks/ts/src/client`。

## 8. 完成报告（实现者必须交）

- 改动文件列表
- `buf generate` 是否成功
- 上表 12 条自检结果（过/失败）
- 任何偏离本规格的地方（应无）
