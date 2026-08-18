# PR-KA-D7 实现规格：错误码收口（一份码表）

| 字段 | 值 |
| --- | --- |
| 标题 | `errors: fold off-table codes into one well-known table` |
| 状态 | **Ready**（待实现） |
| 依赖 | D6 已合（`e303c3d`，全仓零 v1 proto）。在 `v2` 分支上做 |
| 设计来源 | 转正评审 backlog D7；`docs/v2/kernel-architecture.md:327`（一份码表、不保留 `ACL_DENIED`）；KD-K31 |
| 验收人 | 主 agent |

## 1. 目标

服务端发给客户端/Admin 的 `Error.code` 全部落入 `protocol/shared/v2/errors.proto` 注释里的 well-known 码表；表外码要么换入表内码，要么把表补齐。一份码表，两个载体（proto 注释 + `docs/protocol.md`）逐字一致。

**换名（2 个码，4 处发射点）：**

1. `ACL_DENIED` → `PERMISSION_DENIED`（`type=acl_error` 不变）。kernel-architecture.md:327 已裁定「权限软失败只有 `PERMISSION_DENIED`，不保留 `ACL_DENIED`」；A4 规格里的「ACL_DENIED 可继续用于静态拒绝」是延期决定，本 PR 收口。改 `client.go:764`（subscribe 静态拒）与 `client.go:990`（publish 静态拒）。
2. `ACL_ERROR` → `PROXY_ERROR`（`type=proxy_error`）。语义就是「ACL proxy 调用失败」，与 `client.go:927` RPC proxy 失败的 `PROXY_ERROR` 同类。改 `client.go:785`（subscribe）与 `client.go:1015`（publish）。

**入表（4 个码，发射点不动）：**

3. `INTERNAL_ERROR`（`type=server_error`）— 通用服务端错误，SDK survey fallback 码表（`sdks/go/client.go:1738`、`sdks/ts/src/client/client.ts:52`）已依赖，必须保留。
4. `SURVEY_FAILED`（`type=survey_error`）— per-answer 码（`node.go:1039`、`pkg/grpcstream/api_handler.go:202`）。
5. `SURVEY_ANSWER_TOO_LARGE`（`type=survey_error`）— per-answer 码（`node.go:1064`）。
6. `DISCONNECT_ERROR`（`type=transport_error`）— transport 层断连前发的 Error（`pkg/grpcstream/transport.go:156`、`pkg/quicstream/transport.go:112`），已在 protocol.md 表里，补进 proto 注释。

**码表定稿（19 个，写进 errors.proto 注释，分组标注）：**

- 认证/版本：`AUTH_REQUIRED`、`AUTH_ERROR`、`VERSION_UNSUPPORTED`
- 请求/权限：`BAD_REQUEST`、`PERMISSION_DENIED`、`POLICY_DENIED`、`PATTERN_NOT_ROUTABLE`、`RATE_LIMITED`
- 代理/RPC：`NO_PROXY`、`PROXY_ERROR`、`RPC_TIMEOUT`
- 恢复：`RECOVER_FAILED`、`RECOVER_SKIPPED`
- survey 顶层：`SURVEY_DISABLED`、`SURVEY_TOO_MANY_SUBSCRIBERS`
- survey per-answer：`SURVEY_FAILED`、`SURVEY_ANSWER_TOO_LARGE`
- 服务端/传输：`INTERNAL_ERROR`、`DISCONNECT_ERROR`

**文档对齐：** `docs/protocol.md` 错误码表（:501-515）与示例（:492 `ACL_DENIED` → `PERMISSION_DENIED`）改成与 proto 注释同一份表；`docs/developer/02-configuration.md:155` 的 `ACL_DENIED` 提及改 `PERMISSION_DENIED`。

**不做：** 集群命令总线内部码（`TARGET_NODE_NOT_ALIVE`/`CLUSTER_COMMAND_SEND_FAILED`/`COMMAND_IN_PROGRESS` 等，`pkg/redisbroker/cluster_command_bus.go` 的 `ErrorCode` 字段）——内部协议，不泄漏到客户端信封，不动；SDK/proxy 侧自产码（`SURVEY_REPLY_ERROR`/`UNKNOWN_METHOD`/`AUTH_NOT_IMPLEMENTED` 等）——SDK 实现细节，不动；`Error.type` 取值收口（维持现状）；`client.go` 错误路径的控制流（半打开连接等是评审另一条，不在本 PR）；git commit / tag / push。

## 2. 允许改动的文件

- `protocol/shared/v2/errors.proto`（**仅注释**扩表；wire 零变化，字段/编号不许动）
- 生成物注释 churn：`shared/genproto/shared/v2/errors.pb.go`、`shared/genproto/shared/v2/errors.swagger.json`、`sdks/ts/src/proto/shared/v2/errors_pb.ts`（`task generate-protocol` 产出，仅限本文件注释带来的 diff）
- `client.go`（仅 4 处码字符串换名，控制流/type/message 不动）
- `client_fix_test.go`（:194、:251 断言 `ACL_DENIED` → `PERMISSION_DENIED`，:184 注释同步）
- `pkg/websocket/e2e_test.go`（:162、:218 断言同步）
- `_examples/chatroom/cmd/e2e/main.go`（:518、:520 `ACL_ERROR` → `PROXY_ERROR`）
- `docs/protocol.md`（码表 + 示例）、`docs/developer/02-configuration.md`（:155）
- `docs/v2/tasks/pr-ka-d7-error-codes.md`（§8 实现备注）

禁止：`node.go`/`api_handler.go` 的发射点改动（SURVEY_FAILED 等入表不换名）；`hub.go`/`session.go`；SDK 手写代码；proto wire 变化；capability 门禁逻辑。

## 3. 现状（动手前再核对）

### 3.1 表外码全量（D6 后实测，2026-08-18）

| 码 | 发射点 | 层 | 处置 |
| --- | --- | --- | --- |
| `ACL_DENIED` | `client.go:764`（subscribe）、`client.go:990`（publish） | 顶层信封 `acl_error` | → `PERMISSION_DENIED` |
| `ACL_ERROR` | `client.go:785`（subscribe）、`client.go:1015`（publish） | 顶层信封 `acl_error` | → `PROXY_ERROR`（type 随改 `proxy_error`） |
| `INTERNAL_ERROR` | `client.go:185`（handleMessage 兜底）、`client.go:1473`、`:1484`（survey worker） | 顶层信封 `server_error` | 入表 |
| `SURVEY_FAILED` | `node.go:1039`、`pkg/grpcstream/api_handler.go:202` | per-answer `survey_error` | 入表 |
| `SURVEY_ANSWER_TOO_LARGE` | `node.go:1062-1068`（`surveyTooLargeError`） | per-answer `survey_error` | 入表 |
| `DISCONNECT_ERROR` | `pkg/grpcstream/transport.go:156`、`pkg/quicstream/transport.go:112` | transport `transport_error` | 入表 |

### 3.2 既有表内码（不许动）

`client.go`:265 `VERSION_UNSUPPORTED`、:300/:312 `AUTH_REQUIRED`、:348 `AUTH_ERROR`、:895 `RPC_TIMEOUT`、:909 `NO_PROXY`、:927 `PROXY_ERROR`、:970 `RATE_LIMITED`、:1620 `BAD_REQUEST`、:1631/:1654 `PERMISSION_DENIED`、:1644 `POLICY_DENIED`;`recover.go`:435/:442 `RECOVER_FAILED`/`RECOVER_SKIPPED`;`pkg/websocket/handler.go`:93、`pkg/quicstream/handler.go`:80/:98 `BAD_REQUEST`。

### 3.3 SDK 依赖面（已核查）

- SDK 代码**零引用** `ACL_DENIED`/`ACL_ERROR`（`grep sdks/` 无命中）——换名对 SDK 安全。
- survey fallback 码表含 `INTERNAL_ERROR`（`sdks/go/client.go:1738`、`sdks/ts/src/client/client.ts:52`）——故 `INTERNAL_ERROR` 必须保留入表。
- SDK 测试里的 `INTERNAL_ERROR`（`sdks/go/token_ack_test.go:363`、`proxy_test.go:61`）是 SDK 自产路径，不动。
- SDK 注释提及 `SURVEY_FAILED`（`sdks/go/survey.go:20`、`sdks/ts/src/client/types.ts:128`）——码保留，注释不用动。

### 3.4 测试断言影响面

- `client_fix_test.go:194`、`:251`：断言 `ACL_DENIED`；:184 注释提及。
- `pkg/websocket/e2e_test.go:162`、`:218`：断言 `ACL_DENIED`。
- `_examples/chatroom/cmd/e2e/main.go:518`、`:520`：断言 `ACL_ERROR`。
- `survey_test.go:1343` 断言 `SURVEY_ANSWER_TOO_LARGE`——码保留，不动。

### 3.5 proto 注释即合同

`errors.proto` 的 `Error.code` 注释就是码表载体。本次只加注释行，`task generate-protocol` 后 genproto/TS/swagger 的 diff 应只有该注释的同步；出现任何 wire 层 diff（描述符字节之外的）即停手报告。

## 4. 测试

| 测试 | 内容 |
| --- | --- |
| 换名回归 | `client_fix_test.go` ACL 用例改断 `PERMISSION_DENIED` 后绿；`pkg/websocket` e2e 同步绿 |
| 码表一致（新） | 新增 `error_codes_test.go`（根包）：生产码常量/字符串与 errors.proto 注释表一致的守护测试——枚举 §3.1/§3.2 全部发射点码（允许硬编码清单 + 注释说明同步义务），断言每个都在表内；防止未来再加表外码 |
| 回归 | 全仓 `./...`、Go SDK、TS jest 全绿；chatroom 模块 build 绿 |

## 5. 验证

```bash
task generate-protocol && git status --short   # genproto 仅注释 churn
go build ./...
go test -count=1 -run "TestClientFix|ACL" .
go test -count=1 ./pkg/websocket ./pkg/grpcstream .
go test -count=1 ./...                          # 串行；真实 Redis
cd sdks/go && go test -count=1 ./...
cd sdks/ts && npx jest
cd _examples/chatroom && go build ./...
grep -rn "ACL_DENIED\|ACL_ERROR" --include="*.go" . | grep -v "_test.go\|docs/"   # 生产零命中
grep -rn "ACL_DENIED\|ACL_ERROR" docs/protocol.md docs/developer/02-configuration.md   # 零命中
git diff protocol/ | grep -E "^[+-][^+-]" | grep -v "^[+-]\s*//"   # proto 仅注释行变化，零输出
```

## 6. 验收清单

1. `ACL_DENIED`/`ACL_ERROR` 在生产代码与现行文档中零残留；4 处发射点换名正确（type 随之：ACL_ERROR → `proxy_error`）。
2. errors.proto 注释为 §1 定稿的 19 码分组表；wire 零变化（§5 末条 grep 零输出）；生成物仅注释 churn。
3. `docs/protocol.md` 码表与 proto 注释逐字一致（同 19 码、同 type、示例改 `PERMISSION_DENIED`）；`02-configuration.md:155` 同步。
4. 新增码表守护测试；§5 全链绿。
5. 未碰 §2 禁止项；无格式 churn（`git diff --numstat` 与 `--ignore-all-space --ignore-cr-at-eol` 一致）；无 git 操作。

## 7. 完成报告

- 改动文件列表（新增/删除/修改分组）
- §6 每条 过/失败 + 证据
- 测试命令与结果（真实输出）
- 偏离（应无）

## 8. 实现备注（实现方填）

（留空）
