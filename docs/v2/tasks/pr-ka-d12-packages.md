# PR-KA-D12 实现规格:KD-K26 阶段二——authz/channel 下沉 + transport 改名 + admin 剥离

| 字段 | 值 |
| --- | --- |
| 标题 | `refactor: sink authz+channel_policy, rename transports to pkg/transport/*, split admin into internal/admin` |
| 状态 | **Ready**(待实现) |
| 依赖 | D11 已合(`7dc4ee3`)。在 `v2` 分支上做 |
| 设计来源 | [kernel-architecture.md](../kernel-architecture.md) :173-191(目标包图)、KD-K26;D11 规格 §7 阶段图 |
| 验收人 | 主 agent |

## 1. 目标

KD-K26 阶段二,三件事(相互依赖顺序 a → c,b 独立):

- **(a) authz/channel 下沉**:`channel_policy.go` → `internal/channel`,`authorizer.go` → `internal/authz`(含最小导出手术,§3.1)。
- **(b) transport 改名**:`pkg/websocket` → `pkg/transport/ws`、`pkg/grpcstream` → `pkg/transport/grpc`、`pkg/quicstream` → `pkg/transport/quic`(纯 git mv + import 路径更新,§3.4)。
- **(c) admin 剥离**:`pkg/grpcstream` 的 admin 面(`admin_server.go`+`api_handler.go`+admin 测试)→ `internal/admin`;共享 gRPC 底座(`server.go`/`codec.go`)留在 `pkg/transport/grpc` 供回引(§3.3)。

**health.go 不搬**(保留为 Node 方法,admin HTTP mux 组装留 cmd/server;D13 随 runtime 再议)。`DefaultAdminCapabilities` 随 `Capability` 归 internal/authz。session/runtime/cluster/survey 核心拆仍在 D13。

### 1.1 零变化红线

行为零变化;常量/码表值不变;除 §3.1/§3.3 列明的导出手术外,导出符号的名字、签名、语义逐字节等价。proto、`shared/`、`sdks/`、`_examples/`、`config/`、`pkg/topics`、`internal/cluster/`、`internal/{protocol,occupancy,stream}`(D11 已就位)零改动。`pkg/redisbroker` 零改动(它不引 authz/channel_policy,已核实)。

## 2. 允许改动的文件

- git mv:`channel_policy.go`→`internal/channel/`;`authorizer.go`→`internal/authz/`;`pkg/websocket`→`pkg/transport/ws`;`pkg/quicstream`→`pkg/transport/quic`;`pkg/grpcstream`→`pkg/transport/grpc`(client 面+底座);`pkg/transport/grpc/admin_server.go`+`api_handler.go`→`internal/admin/`
- 修改:根 `aliases.go`(加 authz/channel 转发);`cmd/server/main.go`、`cmd/server/runtime.go`(import 路径与限定符);`cluster_redis_integration_test.go`(admin 引用改 `internal/admin`);三包及 admin 的测试文件(随行/拆分,§3.5);`survey_test.go`(自带 `userPrincipal` 副本)
- 测试搬运/拆分:`authorizer_test.go`→`internal/authz/`;`channel_policy_test.go` 纯 policy 前半(:26-158)→`internal/authz/`(它们实际测 `NewAuthorizer`),Node 依赖部分留根;grpcstream `server_test.go` 拆两半,`api_handler_test.go` 与 admin 侧 integration 测试 →`internal/admin/`
- 文档路径引用更新:`AGENTS.md`、`CLAUDE.md`、`.github/copilot-instructions.md`、`docs/developer/01-architecture.md`(`docs/design/` 历史文档不动)
- `docs/v2/README.md` 状态行、本规格 §9

## 3. 现状(主 agent 已核实;行号漂移以语义为准)

### 3.1 (a) 导出手术最小集

`channel_policy.go` → `internal/channel`:

- 已导出随行:`ChannelPolicy`、`DefaultChannelPolicy`、`ErrHistoryDisabled`。
- **导出手术**:未导出 `compiledPolicySpec`(struct,14 字段)→ 导出**不透明类型** `CompiledPolicySpec`(字段保持未导出——已核实 authorizer 只整体传递/嵌入,从不逐字段访问);`compilePolicySpec` → `CompilePolicySpec`;`overlay` → `Overlay`。

`authorizer.go` → `internal/authz`(import `internal/channel` + `config` + `pkg/topics`):

- 全部导出符号随行:`Action`+5 常量、`PrincipalKind`+2 常量、`Principal`、`Capability`+9 个 `Cap*`、`ClosedCapabilityNames`、`DefaultAdminCapabilities`、`Decision`、`ErrInvalidRulePattern`、`Authorizer`、`NewAuthorizer`。
- **导出手术**:`(*authorizer).decideSubscribeSkipAllowLists` → `DecideSubscribeSkipAllowLists`,并**加入 `Authorizer` 接口**(调用点 `cluster_commands.go:108` 经 `n.authorizer` 接口直调;仓内唯一实现即该具体类型)。这是本 PR 唯一的接口形状变化,显式授权。
- `isWildcard`:在 internal/authz 放私有副本(先例:`internal/stream/helpers.go`);root 与 stream 的副本不动。

根 `aliases.go` 增加两组转发(type alias / var / const),注释分组 `--- internal/channel ---` / `--- internal/authz ---`;`ChannelPolicy`、`DefaultChannelPolicy`、`ErrHistoryDisabled`、`Authorizer`、`NewAuthorizer`、`Action`、`Principal`、`PrincipalKind`、`Capability`、`Decision` 等全部经 alias,根包与 cmd/server 引用点零改动。

### 3.2 (a) 使用方核对(改引需求)

- redisbroker:**无引用**,零改动。
- `config/config.go`:`CapabilityNames` 手写镜像,注释指向更新为 `internal/authz`(仅注释)。
- 根包 node.go/client.go/cluster_commands.go/recover.go:经 alias 零改动。
- `pkg/grpcstream/api_handler.go` 的 Cap*/Action* 引用:随 (c) 搬入 internal/admin 后**直引** `internal/authz`(新位置代码不准引根 alias,§4.2)。

### 3.3 (c) admin 剥离粘连点

- 共享底座留在 `pkg/transport/grpc`:`server.go`(`Options`/`Server`)、`codec.go`(`RawCodec`/`rawFrame`)。
- **导出手术**:`prepareServer` → `PrepareServer`,`adminAuthInterceptor` → `AdminAuthInterceptor`(`validateOptions` 保持未导出,由 `PrepareServer` 内部调用)。
- `internal/admin` 提供 `PrepareAdminServer(opts grpc.Options, node *messageloop.Node) (*grpc.Server, error)`(现 `admin_server.go:13` 的 23 行薄壳搬入)与 `NewAPIServiceHandler(node)`(现 `api_handler.go:22`)。
- import 方向:`internal/admin` → `pkg/transport/grpc` + 根包(Node/MaxPresenceSnapshotClients)+ `internal/{stream,protocol,occupancy,authz}`(直引)。根包不引 internal/admin。
- 调用点:`cmd/server/runtime.go:55` 的 `PrepareAdminServer` 改指 internal/admin;`cluster_redis_integration_test.go:16,860` 的 `NewAPIServiceHandler` 同。

### 3.4 (b) transport 改名

- 目录与 package 名:`pkg/transport/ws`(package `ws`)、`pkg/transport/grpc`(package `grpc`)、`pkg/transport/quic`(package `quic`)。
- `pkg/transport/grpc` 内 `google.golang.org/grpc` 统一 alias 为 `googlegrpc`(包名冲突)。
- import 更新点(已核实全集):`cmd/server/main.go:17,19`、`cmd/server/runtime.go:8`、`cluster_redis_integration_test.go:16`、三包外部测试包自引 5 处(`websocket/integration_test.go:15`、grpcstream 三个 `grpcstream_test` 文件、`quicstream/e2e_test.go:12`)。调用点限定符:`websocket.NewServer`→`ws.NewServer` 等。
- Taskfile/CI/Dockerfile/go.mod 无路径引用(已核实)。文档引用按 §2 更新四处;`docs/design/` 历史文档不动。

### 3.5 测试面

- `authorizer_test.go`:不引 Node,整体迁 `internal/authz`;其 helper `userPrincipal`(:20)被 `survey_test.go:1397` 复用——迁走后 survey_test.go 自带副本。
- `channel_policy_test.go`:纯 policy 测试(:26-158,6 个,只用 `NewAuthorizer`+config)迁 `internal/authz`;`TestNodePublish_HistoryDisabled` 等 6 个 Node 依赖测试留根(拆文件,root helper `publishPub` 在 testhelpers_test.go)。
- grpcstream 测试:`api_handler_test.go`(~1200 行)与 admin 侧 integration 测试(`integration_test.go` 的 4 个 `TestGRPC_AdminAPI_*`、`port_integration_test.go` admin 半侧)随迁 `internal/admin`;`server_test.go` 拆:client 半留 `pkg/transport/grpc`,admin 半(`TestPrepareAdminServer_*`/`TestAdminAuthInterceptor`)迁。client 面测试(e2e/transport_test)留。迁走的测试**允许**经根 alias 引 Node 等未下沉符号(减少 churn);生产代码必须直引。

## 4. 组织原则

1. 一律 `git mv`;除 §3.1/§3.3 导出手术、package/import 行、§3.4 限定符改名外,搬动文件逐字节不变。
2. **新位置的生产代码(internal/admin、internal/authz、internal/channel)直引 internal/\***;根 alias 仅供尚未迁移的包/测试过渡。
3. 三包同包测试随行零改;外部测试包(`websocket_test` 等)仅 import 路径与别名更新。

## 5. 验证命令

```bash
go build ./...
go test -count=1 -run "TestSim_|TestCluster|TestResume|TestClientFix|TestSession" .
go test -count=1 ./pkg/redisbroker ./internal/... ./pkg/transport/...
go test -count=1 ./...
cd sdks/go && go test -count=1 .
cd sdks/ts && npx jest
cd _examples/chatroom && go build ./...
golangci-lint run ./...
```

门禁 grep:

```bash
# 旧 transport 路径全灭
grep -rn "messageloop/pkg/\(websocket\|grpcstream\|quicstream\)" --include="*.go" .   # 应为空
# 新位置生产代码不引根 alias 中已下沉的符号(authz/channel/stream/protocol/occupancy)
grep -rn "messageloop\.\(Cap\|Action\|Authorizer\|ChannelPolicy\|Publication\|PresenceInfo\|OccupancyEvent\|Disconnect\|CompileInterest\)" internal/admin internal/authz internal/channel --include="*.go" | grep -v _test   # 应为空
# redisbroker 与 shared/SDK 零改动
git diff --name-only -- pkg/redisbroker shared sdks _examples config pkg/topics internal/cluster   # 应为空
```

串行跑测试,绝不并发两个根目录 `go test`;Redis 真实实例(127.0.0.1:6379,DB 14)。

## 6. 验收清单

1. 三件事全做完;git mv 全部 R 识别;除授权手术与限定符改名外逐字节不变。
2. 导出手术仅限 §3.1/§3.3 列明项;`CompiledPolicySpec` 不透明(字段未导出);`Authorizer` 接口只加 `DecideSubscribeSkipAllowLists` 一项。
3. `internal/admin` 生产代码直引 internal/* + 根(Node/defaults),不引根 alias 的已下沉符号(§5 grep 为空)。
4. 旧 transport 路径全灭;`cmd/server` 仅 import/限定符行变化;grpcstream 拆后 client 面(`pkg/transport/grpc`)不引 admin 符号。
5. C1 sim、错误码普查、全链、SDK/TS/chatroom、lint 0 issues 全绿。
6. 文档四处路径引用更新;未碰 §1.1 红线;无 churn、CRLF、无 git 操作。

## 7. 阶段图(更新)

- **D11(已合)**:叶子契约下沉 + 根 alias 过渡。
- **D12(本 PR)**:authz/channel 下沉;transport 改名;admin 剥离。
- **D13**:session/runtime/cluster/survey 核心拆(Session.node 依赖反转、Sim 钩子归位、recover 与 stream 合流、health 归位),清除根 alias,根包退出或只剩空壳。

## 8. 完成报告

- 改动文件列表(git mv 映射 / 新增 / 修改分组)
- §6 每条 过/失败 + 证据
- 导出手术逐符号清单(名字前→后)
- 测试搬运/拆分逐个判定表
- §5 命令真实输出
- 偏离(应无)

## 9. 实现备注(实现方填)

(留空)
