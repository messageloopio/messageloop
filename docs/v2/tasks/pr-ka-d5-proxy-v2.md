# PR-KA-D5 实现规格：proxy 协议升 v2 + 拆 v1 桥 + 删死 v1 proto

| 字段 | 值 |
| --- | --- |
| 标题 | `proxy: rev proxy protocol to v2 (shared/v2 types), drop the v1 payload bridge, delete dead v1 protos` |
| 状态 | **Ready**（待实现） |
| 依赖 | D4 已合（`f021ebe`）。在 `v2` 分支上做。后续 D6:admin 切 server/v2 + 删 server/v1/shared/v1 |
| 设计来源 | 转正评审协议/SDK 路（移除 v1 面）;KD-K31 |
| 验收人 | 主 agent |

## 1. 目标

移除 v1 的第一步。已探明：`client/v1`、`event/v1` 全死；`shared/v1` 的 Payload/Metadata/Error 与 v2 **字段号逐位相同**(wire 兼容）;proxy 合约（`protocol/proxy/v1`）是 v1 的活依赖之一；admin(server/v1）依赖 shared/v1，留给 D6。

1. **新增 `protocol/proxy/v2`**：复刻 `proxy/v1/proxy.proto`,`package messageloop.proxy.v2`,import 换 `shared/v2/errors.proto` + `shared/v2/types.proto`,`go_package .../genproto/proxy/v2;proxyv2pb`。消息、字段号、8 个 RPC **一字不动**——wire 完全兼容（shared Payload/Metadata/Error v1↔v2 同号）,this is a codegen-level rev, not a wire change。
2. **`proxy/` 包切 v2**(`proxy.go`/`http.go`/`grpc.go`):`RPCProxyRequest/Response.Payload`、`Error` 等字段类型变 sharedv2。HTTP proxy 的 protojson 输出字段名不变，对后端零 wire 影响。
3. **拆桥**:`client.go` 删 `payloadV2toV1`(:832-846)、`payloadV1toV2`(:850-864)、`sharedErrorV2`(:818-827);RPC 路径（:924 请求、:1004 应答）原生 v2 直通。**有意的行为改善**:`sharedErrorV2` 今天丢 `Error.metadata`，拆桥后 metadata 透传——写进规格，补一个透传测试。
4. **SDK 跟随**:`sdks/go/proxy.go` 切 proxy/v2，删 `payloadV1toV2`/`payloadV2toV1`/`errorV2toV1`(:85-137，同样丢 metadata 的问题随之消失）;`sdks/go/example/proxyserver`、`_examples/chatroom/cmd/backend` 跟随。
5. **删死 proto**:`protocol/client/v1`、`protocol/event/v1` 整目录 + 其全部生成物（`shared/genproto/client/v1`、`shared/genproto/event/`、`sdks/ts/src/proto/client/v1`、`sdks/ts/src/proto/event/`、相关 swagger)。
6. **shared/v1 收缩到白名单**（其余全切 sharedv2，类型逐字段相同，纯机械）:
   - 允许残留（D6 清）：`protocol/server/v1` 的 import;`broker.go` `PayloadProto` v1 变体、`publication.go` `PublicationFromPayload` v1 变体、`pkg/grpcstream/api_handler.go`（含 `adminPayloadV2`）及其测试——**admin 专用**。
   - 必须切走：`client_fix_test.go`、`node_test.go`、`rpc_timeout_test.go`、`marshaler_test.go`、`proxy/*_test.go` 等非 admin 引用。
7. `task generate-protocol` 全量重新生成；纯排序 churn(v1/proxy swagger 等无关文件）照 D2 先例 `git checkout --` 还原。

**不做：** admin 切 server/v2、删 `protocol/server/v1` / `shared/v1` / 上述 admin 白名单（全部是 D6)；改 proto 字段号/消息形状；TS SDK 非生成物源码；`hub.go`/`session.go`/broker 逻辑/cluster;`docs/design`、`docs/review`、`docs/archive`、`docs/v2/tasks/` 既有规格；git commit / tag / push。

## 2. 允许改动的文件

- 新增：`protocol/proxy/v2/proxy.proto` + 其生成物
- 删除：`protocol/client/v1/`、`protocol/event/v1/`、`protocol/proxy/v1/`（被 v2 取代）、`shared/genproto/client/v1/`、`shared/genproto/event/`、`shared/genproto/proxy/v1/`、`sdks/ts/src/proto/client/v1/`、`sdks/ts/src/proto/event/`、`sdks/ts/src/proto/proxy/v1/`、上述目录相关 swagger
- `client.go`（删三个桥函数 + RPC 路径直通）、`proxy/proxy.go`、`proxy/http.go`、`proxy/grpc.go`
- `broker.go`/`publication.go`：**仅当** v1 twin 有非 admin 调用者时才动（预期没有——admin 专用，本 PR 不动）
- `sdks/go/proxy.go`、`sdks/go/example/proxyserver/main.go`、`_examples/chatroom/cmd/backend/main.go`（及 chatroom 内其他 proxy/v1 引用）
- 测试：`proxy/http_test.go`、`proxy/transport_test.go`、`sdks/go/proxy_test.go`、`sdks/go/fix_regression_test.go`、`client_fix_test.go`、`node_test.go`、`rpc_timeout_test.go`、`marshaler_test.go`（机械切 sharedv2)
- 文档：`docs/developer/06-development.md`(codegen 路径，:144 已是死链一并修）、`docs/developer/07-sdk-go.md` proxy 节；新增一行说明见 §6.6
- `docs/v2/tasks/pr-ka-d5-proxy-v2.md`(§8 实现备注）

禁止：见 §1「不做」。

## 3. 现状（动手前再读）

### 3.1 桥与调用点

`client.go:818-864` 三个桥函数；唯一调用点 :924(RPC 请求 → proxy）与 :1004(proxy 应答 → 客户端）。`sharedErrorV2` 丢 `metadata`（两版 proto 都有 field 4 `google.protobuf.Struct`)。

### 3.2 proxy 合约

`protocol/proxy/v1/proxy.proto`:`ProxyService` 8 RPC(RPC/Authenticate/SubscribeAcl/PublishAcl/OnConnected/OnSubscribed/OnUnsubscribed/OnDisconnected);import shared/v1 errors+types。`proxy/proxy.go:52` `RPCProxyRequest.Payload *sharedpb.Payload` 等。HTTP 变体走 protojson（字段名 v1/v2 相同）。

### 3.3 死 proto 确认

`client/v1`、`event/v1` 在**非生成代码**中零引用（已全仓 grep)。`event/v1` 只有 `PublishEvent` 一个消息，文档残留提及在 `docs/developer/06-development.md:157`、`CLAUDE.md`——CLAUDE.md 的提及顺手删掉（允许改 CLAUDE.md 的这一处）。

### 3.4 内部模型早已中立

根包 `Publication`(`broker.go:27-50`）是纯 Go struct(Payload []byte + Kind),Broker 接口不带 proto 类型；Redis 格式是 JSON envelope + 原始字节（`pkg/redisbroker/message.go:26-39`),**不**marshal 任何 proto——本 PR 对 Redis 数据格式零影响。

### 3.5 buf

`buf.yaml` module 覆盖 `protocol/` 全目录；`task generate-protocol` = `buf generate`(remote plugins，本机已验证可用）。生成物落 `shared/genproto/`、`sdks/ts/src/proto/`。

## 4. 测试

| 测试 | 内容 |
| --- | --- |
| RPC metadata 透传（新） | proxy 返回带 `metadata` 的 Error，客户端收到的 Error 含同一 metadata（拆桥前会丢） |
| proxy 合约机械切换 | `proxy/http_test.go`/`transport_test.go` 全绿（类型换 v2，断言语义不变） |
| SDK proxy | `sdks/go` 测试全绿；`fix_regression_test.go` 同步 |
| 回归 | 全仓 `./...`、TS jest 全绿 |

## 5. 验证

```bash
task generate-protocol && git status --short   # 只应有预期生成物差异；churn 还原
go build ./...
go test -count=1 ./proxy ./pkg/grpcstream .
go test -count=1 ./...          # 串行；真实 Redis
cd sdks/go && go test -count=1 ./...
cd sdks/ts && npx jest
grep -rn "genproto/client/v1\|genproto/event" --include="*.go" --include="*.ts" . | grep -v node_modules   # 零命中
grep -rn "protocol/client/v1\|protocol/event" --include="*.go" --include="*.ts" --include="*.md" . | grep -v node_modules | grep -v "docs/design\|docs/review\|docs/archive\|docs/v2/tasks"   # 零命中
grep -rn "genproto/shared/v1" --include="*.go" . | grep -v genproto   # 只剩 admin 白名单：broker.go、publication.go、pkg/grpcstream/*（生产+测试）
```

## 6. 验收清单

1. `protocol/proxy/v2` 与 v1 逐字段一致（diff 只有 package/import/go_package);buf 生成物齐全（Go+TS+swagger);proxy/v1 整目录及其生成物删除。
2. `client.go` 三桥删除、RPC 路径原生 v2;Error.metadata 透传有新测试且绿。
3. `proxy/`、`sdks/go/proxy.go`(+example)、chatroom backend 切 v2;SDK 三个桥函数删除。
4. `client/v1`、`event/v1` proto 与全部生成物删除，grep 门禁零命中；CLAUDE.md/06-development.md 的死引用清掉。
5. shared/v1 残留仅在 §1.6 白名单；非 admin 测试全部切 sharedv2。
6. 全量测试链（§5）绿；生成物无 churn;TS SDK 非生成物零改动。
7. 未碰 §2 禁止项；无格式 churn;无 git 操作。

## 7. 完成报告

- 改动文件列表（新增/删除/修改分组）
- §6 每条 过/失败 + 证据
- 测试命令与真实输出
- 偏离（应无）

## 8. 实现备注（实现方填）

（空）
