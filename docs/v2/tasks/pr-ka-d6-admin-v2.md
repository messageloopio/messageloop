# PR-KA-D6 实现规格：admin 切 server/v2 + 清除 shared/v1（移除 v1 收尾）

| 字段 | 值 |
| --- | --- |
| 标题 | `admin: serve APIService from server/v2, delete server/v1 and shared/v1` |
| 状态 | **Ready**（待实现） |
| 依赖 | D5 已合（`847c32a`,proxy 已升 v2、桥已拆）。在 `v2` 分支上做 |
| 设计来源 | 转正评审协议/SDK 路（admin 面二选一决策：切 server/v2);KD-K31 |
| 验收人 | 主 agent |

## 1. 目标

移除 v1 收尾。D5 之后 shared/v1 只剩 admin 白名单；本 PR 把 admin 面切到已生成但零引用的 `server/v2`，删光剩余 v1，并顺手收两条 backlog。

1. **admin 切 server/v2**:`pkg/grpcstream/api_handler.go` 与 `admin_server.go` 换 `genproto/server/v2`(go 别名 `serverv2`)。8 个 RPC 中 5 个是纯机械换（Publish/Disconnect/Subscribe/Unsubscribe/Survey/GetChannels——Survey 的 sharedpb→sharedv2 机械换）;**3 处形状重做**:
   - `PresenceInfo`(v2: `session_id=1, user_id=2, client_id=3, connected_at=4`):`SessionId = firstNonEmpty(info.SessionID, info.ClientID)`;`ClientId = info.ConnectClientID`（真正的 Connect.client_id，语义修正）;v1 的 `connect_client_id` 兜底字段消失。
   - `GetHistoryRequest.since` 从 `uint64 since_offset` 变 `shared.v2.Position`:`since == nil` → 从头（limit 内）;`since.offset` 有值 → `History(channel, offset, limit)`;`since.stream_epoch` 非空时与 broker 当前 epoch 比对（`recover.go:90-94` 的 `Epoch()` type-assert 范式），不匹配 → `codes.FailedPrecondition`("stream epoch mismatch: history belongs to a previous log generation")。
   - `HistoryPublication`(v2: `position=1, payload=2, time=3, id=4, metadata=5`):`Position{StreamEpoch: pub.Epoch, Offset: &pub.Offset}`（按 proto optional 形态）;`payload = pub.PayloadProtoV2()`;`is_text` 字段消失（v2 Payload oneof 自带 kind);`metadata` 由 map 改 `sharedv2.Metadata{Entries: ...}`。
2. **删 admin 桥与 twin**:`adminPayloadV2`(api_handler.go:493-506）删除（输入已是 v2，直通）;`payloadBytes`(:509）切 sharedv2 + `PublicationFromPayloadV2`;`broker.go` `PayloadProto` v1 变体（:54 起）与 `publication.go` `PublicationFromPayload` v1 变体（:15 起）删除（最后的使用者就是 admin)。
3. **删除 v1 残余**:`protocol/server/v1/`、`protocol/shared/v1/`、`shared/genproto/server/v1/`、`shared/genproto/shared/v1/`、`sdks/ts/src/proto/server/v1/`、`sdks/ts/src/proto/shared/v1/` 及相关 swagger。此后**全仓零 v1 proto**。
4. **跟随切换**:`cluster_redis_integration_test.go:20` → server/v2;`_examples/chatroom/internal/chatroom/admin.go` → server/v2;`pkg/grpcstream` 三个测试文件 → v2;`publication_test.go` 的 v1 twin 用例删除（v2 twin 已有覆盖）。
5. **backlog 顺手收（允许，小）**:
   - `_examples/chatroom/cmd/e2e/main.go:438` 编译错：`WithRecover(n, string)` → `WithRecover(&sharedv2.Position{...})` 新签名（B3 遗留），修到 `go build ./...`（在 chatroom 模块内）通过。
   - `TestRedisBroker_LiveSubscription_OccupancyNotInterested` flake：定位根因（负载相关时序），用 Eventually/同步原语修掉，禁止固定长 Sleep;5 连跑验证。
6. **文档**:`docs/developer/03-admin-api.md` 全篇改 v2(import 路径、PresenceInfo/GetHistory/HistoryPublication 字段表）;`docs/protocol.md` 文首（:3）与 Admin 节（:545-548）的「admin 保留 server.v1」表述改为「admin 同为 server/v2」(D1/D2 写的旧决策记录反转）;`docs/developer/README.md:26`、`04-cluster.md:396` 的 `server.v1` 提及；`05-observability.md` 如涉及。
7. `task generate-protocol` 重新生成；无关 churn 照 D2/D5 先例还原。

**不做：** 错误码收口（SURVEY_FAILED/ACL_DENIED 等是另一条 backlog,D7)；改 capability 门禁逻辑；改 `Broker.History` 签名；proto 字段号新增/改动（server/v2 proto **已冻结**，本 PR 不改 proto 文件只改引用；若实现中发现 server/v2 形状无法表达必要语义，停下来报告，不要自作主张改 proto);TS SDK 手写代码；git commit / tag / push。

## 2. 允许改动的文件

- `pkg/grpcstream/api_handler.go`、`admin_server.go`、`api_handler_test.go`、`integration_test.go`、`port_integration_test.go`、`server_test.go`
- `broker.go`（删 v1 twin)、`publication.go`（删 v1 twin)、`publication_test.go`（删 v1 twin 用例）
- 删除：`protocol/server/v1/`、`protocol/shared/v1/`、`shared/genproto/server/v1/`、`shared/genproto/shared/v1/`、`sdks/ts/src/proto/server/v1/`、`sdks/ts/src/proto/shared/v1/` + 相关 swagger
- `cluster_redis_integration_test.go`、`_examples/chatroom/internal/chatroom/admin.go`、`_examples/chatroom/cmd/e2e/main.go`
- `pkg/redisbroker/pubsub_test.go`(flake 修复；若根因在 pubsub.go 生产代码，须报告主 agent 确认后再动）
- `docs/developer/03-admin-api.md`、`docs/protocol.md`、`docs/developer/README.md`、`docs/developer/04-cluster.md`、`docs/developer/05-observability.md`（如涉及）
- `docs/v2/tasks/pr-ka-d6-admin-v2.md`(§8 实现备注）

禁止：见 §1「不做」;`client.go`/`node.go`/`hub.go`/`session.go` 零改动。

## 3. 现状（动手前再读）

### 3.1 server/v1 ↔ server/v2 差异全量

两 proto `diff` 只有四处：package/import/go_package;`PresenceInfo`(5 字段带历史兼容注释 → 4 字段语义修正）;`GetHistoryRequest.since_offset` → `since Position`;`HistoryPublication`(offset/is_text/map metadata → Position/Metadata)。其余逐字段相同（已核对，2026-08-18)。server/v2 生成物已存在（`shared/genproto/server/v2`,go 包名 `serverv2`)，当前零引用。

### 3.2 admin handler 现状

`api_handler.go`:8 RPC 实现（:27/:153/:211/:253/:296/:366/:428/:467);GetPresence :383-395 的 PresenceInfo 填充（含 legacy fallback 注释）;GetHistory :428-464(`h.node.Broker().History(req.Channel, req.SinceOffset, int(req.Limit))`);`adminPayloadV2` :493-506;`payloadBytes` :509-515;`admin_server.go:21` `RegisterAPIServiceServer`。capability 门禁（`requireAdminCaps` + `AdminDecide`）保持原样。

### 3.3 shared/v1 残留清单（D5 终验实测）

`broker.go`、`publication.go`、`publication_test.go`、`pkg/grpcstream/{api_handler,api_handler_test,integration_test}.go`、`_examples/chatroom/internal/chatroom/admin.go`、生成物 `server/v1`。D6 后必须零残留（生成物随删除走）。

### 3.4 Epoch 访问器

`recover.go:90-94`:`epochBroker` interface assert → `Epoch() string`;redisBroker(:546）与 memoryBroker(:383）均实现。GetHistory 的 epoch 比对照此。

### 3.5 backlog 两条

- chatroom e2e:`WithRecover(cursor *sharedv2.Position)`(`sdks/go/client.go:972`)；调用点传的是 `(number, string)` 旧签名。修到 chatroom 模块 `go build ./...` 通过即可，不追求跑通 e2e。
- flake:`TestRedisBroker_LiveSubscription_OccupancyNotInterested`（全仓跑负载下偶发 0.04s 超时失败，单跑稳过）。先读测试找时序假设（大概率是固定窗口等待 occupancy 未到达），改 Eventually/同步。

## 4. 测试

| 测试 | 内容 |
| --- | --- |
| admin 8 RPC | 既有 `api_handler_test.go` 等切 v2 后全绿（断言语义不变，字段名按 v2) |
| PresenceInfo 语义（新/改） | `session_id`/`client_id` 各归其位（client_id = Connect.client_id，不再是 session) |
| GetHistory Position（新/改） | since nil/仅 offset/epoch 匹配/epoch 不匹配（FailedPrecondition）四路 |
| 回归 | 全仓 `./...`、Go SDK、TS jest 全绿;chatroom 模块 build 通过;flake 5 连过 |

## 5. 验证

```bash
task generate-protocol && git status --short
go build ./...
go test -count=1 ./pkg/grpcstream ./proxy .
go test -count=1 ./...                                   # 串行；真实 Redis
cd sdks/go && go test -count=1 ./...
cd sdks/ts && npx jest
cd _examples/chatroom && go build ./...
go test -count=5 -run TestRedisBroker_LiveSubscription_OccupancyNotInterested ./pkg/redisbroker
grep -rn "genproto/server/v1\|genproto/shared/v1\|protocol/server/v1\|protocol/shared/v1\|server\.v1" --include="*.go" --include="*.ts" --include="*.md" --include="*.proto" . | grep -v node_modules | grep -v "docs/design\|docs/review\|docs/archive\|docs/v2/tasks"   # 零命中
ls protocol/ shared/genproto/ sdks/ts/src/proto/          # 无 v1 目录
```

## 6. 验收清单

1. admin 面 serve server/v2;3 处形状重做符合 §1.1 语义（PresenceInfo 语义修正、since Position 四路、HistoryPublication Position/Metadata);capability 门禁零改动。
2. `adminPayloadV2` 与两个 v1 twin 删除；`payloadBytes` 切 v2。
3. 全仓零 v1 proto/生成物（§5 grep 门禁零命中，`ls` 无 v1 目录）;server/v1、shared/v1 删除干净。
4. cluster_redis_integration_test、chatroom admin 切 v2;chatroom e2e 编译修复（chatroom 模块 build 绿）。
5. flake 修复有根因说明，5 连跑绿；未引入固定长 Sleep。
6. 文档四处同步（03-admin-api.md 全篇 v2、protocol.md 两处、developer/README、04-cluster)。
7. §5 全链绿；生成物无 churn；未碰 §2 禁止项；无格式 churn；无 git 操作。

## 7. 完成报告

- 改动文件列表（新增/删除/修改分组）
- §6 每条 过/失败 + 证据
- 测试命令与真实输出
- 偏离（应无）

## 8. 实现备注（实现方填）

（空）
