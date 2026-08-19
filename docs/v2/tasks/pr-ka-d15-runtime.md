# PR-KA-D15 实现规格:KD-K26 收口——Node/Cluster 门面下沉 internal/runtime + 清除根 alias

| 字段 | 值 |
| --- | --- |
| 标题 | `refactor: sink Node and cluster facade into internal/runtime, delete root aliases` |
| 状态 | **Ready** |
| 依赖 | D14 已合(`6e17bdb`)。在 `v2` 分支上做 |
| 设计来源 | [kernel-architecture.md](../kernel-architecture.md) :173-191(目标包图)、KD-K26;D14 规格 §7 阶段图 |
| 验收人 | 主 agent |

## 1. 目标

KD-K26 最后一刀。三件事:

- **(a) 编排层下沉**:根包剩余生产文件 `git mv` → 新建 **`internal/runtime`**(package `runtime`):Node 本体、Cluster 门面/lease manager/repairer、cluster Node 方法群、Sim 钩子、recover、health、subscription saga、`session_runtime.go`。survey **编排**留在 Node 上(见 §1.2),不搬进 `internal/survey`。
- **(b) 消费方切直引**:`cmd/server`、`pkg/transport/{ws,grpc,quic}`、`internal/admin`、`internal/cluster/sim` 以及全部外部测试,从 `messageloop.X` 改到 `internal/{runtime,session,protocol,cluster,metrics,occupancy,survey}` 与 `shared`。
- **(c) 根包退出**:删除 `aliases.go` 与根 `marshaler.go`;根上只留 `doc.go`(无导出符号)。模块路径 `github.com/messageloopio/messageloop` 仍有效,但再 `import` 根包拿不到 Node/Session/Disconnect。

**不做**:改协议/行为/常量值;新建空包 `internal/rpc`;把 `recover.go` 并进 `internal/stream`;把 survey 扇出搬进 `internal/survey`(会环);碰 `shared/`、`sdks/`、`config/`、`pkg/topics`、`pkg/redisbroker`、`proxy/`、`protocol/`。

### 1.1 零变化红线

行为零变化;常量/码表值/JSON tag 不变;锁序不变量(D14 §1.1)保持。导出手术仅限 §3.4 列明的机械改名(包装函数换成直引)。禁止借机重构 Node 方法、改 fencing/recover 算法、改 Sim 语义。

### 1.2 未决题裁定(D14 §7 留给本规格)

| 题 | 裁定 | 理由 |
| --- | --- | --- |
| `recover.go` 是否并入 `internal/stream` | **否**。随 Node 进 `internal/runtime` | recover 是 `func (n *Node)` 编排,点名 `*Session`、Hub、`ClusterSessionSnapshot`、ChannelPolicy。`internal/stream` 已是叶子(D11),session 已 import stream;并入会 `stream→session→stream`。可选:把 `positionFrom`/`offsetFrom` 抽到 `internal/stream`(session 已 import stream,可删 D14 副本),Replayer 本体不动 |
| `internal/rpc` | **本刀不建** | 图上预留格。现有 RPC = `Session.handleRPC` + `Node.ProxyRPC` + `proxy/`。空包无进口。以后若要让 Runtime 不再点名 `proxy` 类型再开 |
| `defaults.go` / `marshaler.go` | **拆常量;删 marshaler 转发** | 见 §3.2。marshaler 的权威实现已在 `shared/`;根文件是纯 re-export,清 alias 后删除 |
| survey 编排是否归 `internal/survey` | **否,留 Node** | `internal/survey` 已是叶子类型。编排用 Hub/`Send`/cluster bus;搬过去会 `survey→session` 或 `survey→runtime` 环。更正 D14 §7 原句 |

## 2. 允许改动的文件

### 2.1 git mv → `internal/runtime/`(package `runtime`)

生产:

- `node.go`、`session_runtime.go`、`subscription_saga.go`、`health.go`
- `cluster.go`、`cluster_state.go`、`cluster_commands.go`、`cluster_resume.go`、`cluster_repair.go`、`cluster_user_index.go`、`cluster_sim.go`
- `recover.go`

测试(随生产,改 `package runtime`;同包未导出符号继续可访,避免 D14 那种跨包补导出):

- `node_test.go`、`client_test.go`、`client_fix_test.go`、`recover_test.go`、`presence_test.go`、`survey_test.go`、`heartbeat_test.go`、`health_test.go`、`channel_policy_test.go`、`authorizer_test.go`、`occupancy_test.go`、`gap_notice_test.go`、`rpc_timeout_test.go`、`cluster_test.go`、`cluster_state_test.go`、`cluster_resume_test.go`、`cluster_repair_test.go`、`cluster_remote_test.go`、`cluster_offsets_test.go`、`cluster_projection_repair_test.go`、`cluster_user_index_test.go`、`cluster_epoch_test.go`、`metrics_test.go`(根残留的 Node 接线测)、`version_test.go`、`broker_memory_node_test.go`、`publication_test.go`、`testhelpers_test.go`
- 外部测试包改 `package runtime_test` 并迁入:`cluster_redis_integration_test.go`、`cluster_sim_test.go`、`cluster_v1_e2e_test.go`

### 2.2 删除

- `aliases.go`(整文件)
- `marshaler.go`、`marshaler_test.go`(与 `shared/marshaler*.go` 重复)

### 2.3 新建

- `internal/runtime/doc.go`(包注释:KD-K26 收口,PR-KA-D15)
- 根 `doc.go`(见 §3.7)
- `internal/occupancy/defaults.go`:`MaxPresenceSnapshotClients`(从 defaults.go 拆出)
- `internal/survey/defaults.go`:`MaxSurveyAnswerBytes`、`MaxSurveyResultBytes`
- `internal/runtime/defaults.go`:其余常量(`DefaultMaxMessageSize`、`DefaultHeartbeatIdleTimeout`、`MaxRecoveredPublications`、`DefaultShutdownTimeout`)
- 可选:`internal/stream/position.go`(`positionFrom`/`offsetFrom`);若做,删 `internal/session/runtime.go` 里 D14 副本,session/hub 改调 `stream.PositionFrom`/`stream.OffsetFrom`(导出这两个 helper)

### 2.4 删除后的根 `defaults.go`

内容拆光后删除根文件。

### 2.5 消费方 import/限定符(授权改)

- `cmd/server/{main.go,runtime.go,runtime_test.go,config_consistency_test.go}`
- `pkg/transport/ws/*`、`pkg/transport/grpc/{handler.go,client_server.go,transport.go,*_test.go,e2e_test.go,port_integration_test.go}`、`pkg/transport/quic/{handler.go,server.go,transport.go,*_test.go,e2e_test.go}`
- `internal/admin/{admin_server.go,api_handler.go,*_test.go}`
- `internal/cluster/sim/world.go`(及 sim 里仍引根的测试,若有)

### 2.6 文档

`AGENTS.md`、`CLAUDE.md`、`.github/copilot-instructions.md`、`docs/developer/01-architecture.md`、`docs/developer/06-development.md`(模块表)、`docs/v2/README.md`、本规格 §9。`docs/design/` 历史文档不动。

`cmd/server/main.go:136` 注释 `messageloop.ClusterOptions.normalize()` → `cluster.ClusterOptions.Normalize()`(D13 记下的陈旧引用,本刀收)。

## 3. 现状(行号漂移以语义为准)

### 3.1 搬运集 → `internal/runtime`

| 文件 | 约行 | 内容 | 迁后注意 |
| --- | --- | --- | --- |
| node.go | 1430 | `Node`、`NewNode`、Hub/Broker/Presence/Cluster/Proxy/Survey 编排 | `newHub(...)` → `session.NewHub(...)`;`newPresenceEvent`/`marshalPresenceEvent` → `occupancy.NewPresenceEvent`/`occupancy.MarshalPresenceEvent` |
| session_runtime.go | 140 | `nodeRuntime`、`NewClient`、根 helper 副本(`index`/`isWildcard`/`publicationID`/`broadcastParallelLimit`/`pingClusterRefreshInterval`) | 与 Node 同包,副本可删——改调 `session` 已导出符号或把仍需的未导出 helper 留在 runtime 包内一份 |
| subscription_saga.go | 27 | `runSubSaga` | 仅 Node 用,整件迁 |
| health.go | 66 | `HealthHandler` | 整件迁 |
| cluster.go | 190 | `Cluster`/`NewCluster`/`ClusterDependencies`/noop 组件 | `allocateNodeIncarnation(...)` → `cluster.AllocateNodeIncarnation(...)` |
| cluster_state.go | 423 | Node 的 lease/snapshot 同步 + 留根常量/lease manager | 整件迁 |
| cluster_commands.go | 322 | Node 命令处理 | 整件迁 |
| cluster_resume.go | 258 | resume/hydrate/takeover | 整件迁 |
| cluster_repair.go | 360 | `clusterRepairer`/`NewClusterRepairer` | 整件迁;`SimMembershipOnce` 的 `*clusterRepairer` 断言依赖同包 |
| cluster_user_index.go | 52 | `ExpandUserSessions`/`ObserveAdminUserFanout` | 整件迁;`SyncUserIndex` 已在 `internal/cluster` |
| cluster_sim.go | 33 | `SimSyncClusterSessionState`/`SimResumeRemoteSession`/`SimMembershipOnce` | **必须随 Node**(包装未导出方法);`world.go` 继续走这三条导出缝 |
| recover.go | 473 | Recover 编排 | 整件迁,不进 stream |

`internal/cluster` 已有契约/DTO/epoch/`SyncUserIndex`。门面(`Cluster`/`NewCluster`/`ClusterDependencies`/`NewClusterRepairer`/`NewClusterNodeLeaseManager`)进 runtime,契约继续在 `internal/cluster`。runtime import cluster,cluster **不准** import runtime。

### 3.2 常量拆分

从根 `defaults.go` 拆出,值逐字节不变:

| 常量 | 新家 | 现生产消费者 |
| --- | --- | --- |
| `DefaultMaxMessageSize` | `internal/runtime` | `node.go` `MaxMessageSize` |
| `DefaultHeartbeatIdleTimeout` | `internal/runtime` | `node.go` 心跳回退 |
| `MaxRecoveredPublications` | `internal/runtime` | `recover.go` |
| `DefaultShutdownTimeout` | `internal/runtime` | `node.go` `Shutdown` |
| `MaxPresenceSnapshotClients` | `internal/occupancy` | `node.go` 快照上限;**`internal/admin/api_handler.go:403`** |
| `MaxSurveyAnswerBytes` / `MaxSurveyResultBytes` | `internal/survey` | `node.go` `buildClientSurveyResult` |

`DefaultHistoryLimit` 已在 `internal/stream`,不要合并(同是 1000,语义不同:页长 vs 单次 recover 配额)。

admin 已会 import occupancy(或本刀加上),**禁止**为这个常量让 occupancy import runtime。

### 3.3 删除 aliases.go

根生产调用点在搬走后必须先改直引,再删文件:

| 根包装 | 迁入后写成 |
| --- | --- |
| `newHub(...)` | `session.NewHub(...)` |
| `newPresenceEvent` / `marshalPresenceEvent` | `occupancy.NewPresenceEvent` / `occupancy.MarshalPresenceEvent` |
| `allocateNodeIncarnation` | `cluster.AllocateNodeIncarnation` |
| 类型短名 `Disconnect`/`Publication`/`Metrics`/`Survey`/`Session`/`Hub`/`ChannelPolicy`/… | 经 import 用包限定,或在 **runtime 包内** 做本地 type alias(D14 `internal/session/runtime.go` 先例)。本地 alias **不是** 根 alias,允许,目的是迁入文件体少改 |

`protocolGenerationOK` 只剩 `version_test.go`:改 `protocol.GenerationOK`。

消费方(cmd/server、transports、admin、sim、外部测试)按 §3.5 改限定符,不准再出现 `messageloop.Node` 等。

### 3.4 迁入文件允许的改动

除 package/import 行与 §3.3 三处包装换成直引外,逻辑逐字节不变。允许:

- 为消掉根短名,在 `internal/runtime` 加一份本地 alias 文件(例如 `aliases_local.go`),**仅本包可见**,不要再导出一套给外人。
- `session_runtime.go` 的 `index`/`isWildcard`/`publicationID`/`broadcastParallelLimit`/`pingClusterRefreshInterval`:若与 session 包重复且 session 已导出或可改为调用 session,删根副本;否则留在 runtime 包(node.go/recover.go 仍用短名)。
- 可选 `positionFrom`/`offsetFrom` → `internal/stream` 并导出;`session` 删 D14 副本。

禁止:改 Node 方法签名、改 saga 步、改 Sim 三条函数的语义。

### 3.5 消费方切换(已核实全集)

根 import `"github.com/messageloopio/messageloop"` 的非测试生产点:

| 位置 | 现用根符号 | 改后 |
| --- | --- | --- |
| `cmd/server/main.go` | `NewNode`、`NewMetrics`、`NewCluster`、`ClusterOptions`/`ClusterDependencies`、`NodeEpochAllocator`、`NewClusterNodeLeaseManager`、`ClusterNodeLeaseManagerConfig`、`NewClusterRepairer`、`ClusterRepairerConfig`、`*Node`、`*Metrics`、`*Cluster`、`Broker`、`NewMemoryBroker`、`MemoryBrokerOptions`、`SetHealthCheck`、`Run`/`Shutdown`、`SetupProxy`、`GetHeartbeatConfig`、`HealthHandler` | `runtime.NewNode`/`NewCluster`/…;`metrics.NewMetrics`;`cluster.ClusterOptions`/`NodeEpochAllocator` 等契约;`stream.Broker`/`NewMemoryBroker`/`MemoryBrokerOptions` |
| `cmd/server/runtime.go` | `*messageloop.Node` | `*runtime.Node` |
| `pkg/transport/ws/{handler,server,transport}.go` | `*Node`、`NewClient`、`WithProtocol`、`MakeOutboundMessage`、`Marshaler`/`JSONMarshaler`/`ProtobufMarshaler`/`ProtoJSONMarshaler`、`Disconnect`、`Transport`(接口断言) | `*runtime.Node`、`runtime.NewClient`、`session.WithProtocol`/`MakeOutboundMessage`/`Transport`、`shared.*` marshaler、`protocol.Disconnect` |
| `pkg/transport/grpc/{handler,client_server,transport}.go` | 同上 | 同上 |
| `pkg/transport/quic/{handler,server,transport}.go` | 同上 | 同上 |
| `internal/admin/admin_server.go` | `*Node` | `*runtime.Node` |
| `internal/admin/api_handler.go` | `*Node`、`MaxPresenceSnapshotClients`；方法面:`PublishToSession`、`AdminCanPublish`、`ChannelPolicy`、`Publish`/`PublishTransient`、`AdminCapabilities`、`AdminDecide`、`CountMatchingSubscribers`、`Survey`、`DisconnectSession`、`SubscribeSession`、`UnsubscribeSession`、`ExpandUserSessions`、`ObserveAdminUserFanout`、`Presence`、`Broker`、`Channels` | `*runtime.Node`(方法名不动)、`occupancy.MaxPresenceSnapshotClients`；`Broker` 类型改 `stream.Broker` |
| `internal/cluster/sim/world.go` | `NewNode`/`NewClient`/`NewCluster`/`NewClusterRepairer`/`ClusterOptions`/`ClusterDependencies`/`ClusterRepairerConfig`/`JSONMarshaler`/`Session`/`SimMembershipOnce` 等 | `runtime.*` + `cluster.*` 契约 + `shared.JSONMarshaler` + `session.Session` |

transport 的 `Close(Disconnect)` 与 `session.Transport` 同一底层类型(`protocol.Disconnect`),改限定符即可。

`_examples/chatroom` **只引 sdks/shared**,零改。`pkg/redisbroker` 生产代码 D13 已切,零改。

### 3.6 测试面

- **同包测试**随 §2.1 迁 `internal/runtime`、`package runtime`。除 package/import、根短名改本地 alias/限定符、`ReadFile` 相对路径外,断言逐字不变。
- **外部测试**(`package messageloop_test` 的三份 cluster e2e/sim/redis)迁入后 `package runtime_test`,import `internal/runtime` 等。
- **cwd 注意**:`go test ./internal/runtime` 的工作目录是该包目录。今日根测试里 `os.ReadFile("node.go")` / `"internal/session/hub.go"` / `"cmd/server/main.go"` 以仓库根为 cwd。迁后改成相对 `internal/runtime` 的路径,例如 `node.go`、`../session/hub.go`、`../../cmd/server/main.go`、`../cluster/epoch.go`。逐文件改:
  - `occupancy_test.go` `TestOccupancy_NoForbiddenProductionRemnants`
  - `cluster_epoch_test.go` `TestNoUUIDIncarnationInProductionSource`
- **`error_codes_test.go`**:模块级码表普查,读 `protocol/shared/v2/errors.proto` 与源文件。**留在仓库根**、改 `package messageloop_test`(对着只剩 doc.go 的根包做外部测试即可),路径仍相对仓库根;或迁入 runtime 并改所有 ReadFile。二选一,完成报告写明。推荐留根,少动路径。
- **`marshaler_test.go`**:删除(与 `shared/marshaler_test.go` 重复)。
- transport / admin / cmd/server / sim 测试:只改 import 与限定符,文件不搬。
- 测试函数总数前后一致(删掉的只有 marshaler_test 里与 shared 重复的那些;`shared` 侧已覆盖)。完成报告给「迁入 runtime / 留根 error_codes / 删除 marshaler」三列计数。

### 3.7 根包收口

根目录仅:

```go
// Package messageloop is the module root after PR-KA-D15 (KD-K26).
// Production types live in internal/runtime, internal/session, and the
// other internal/* packages. This package exports nothing.
package messageloop
```

禁止再放 alias。`go list` / `go test .`(若只剩 error_codes 外部测试)必须通过。

### 3.8 遮蔽与命名

- 包名 `runtime` 与标准库 `runtime` 冲突时,标准库用 `stdruntime` 别名,本包文件统一。
- `cmd/server/runtime.go` 文件名与包 `main` 不冲突;import 用 `mlruntime "…/internal/runtime"` 或 `noderuntime`,全文件统一。推荐 `mlruntime` 仅当 `runtime` 标识符已被占用;否则 `import "…/internal/runtime"` + `runtime.NewNode`。
- world.go 已有局部名 `cluster` 与 `clusterpkg`(D13);门面改 `runtime.NewCluster`,契约继续 `clusterpkg`。

## 4. 组织原则

1. 整文件 `git mv`;除 §3.4 授权改写外逐字节不变。
2. 新位置代码(internal/runtime)零根包引用;直引 internal/* 与 shared/proxy/config。
3. 清 alias 与搬家同一 PR 做完(不再留过渡刀)。实现顺序建议:先 mv + 本地 alias 让 `internal/runtime` 自洽编译 → 再切消费方 → 再删 `aliases.go`/`marshaler.go`/`defaults.go` → 写根 `doc.go`。
4. 层方向:runtime → {session, survey, stream, cluster, occupancy, authz, protocol, metrics, proxy, config}。反向全禁。
5. `.go` 保持 CRLF;不做 commit/tag/push。

## 5. 验证命令

```bash
go build ./...
go test -count=1 -run "TestSim_|TestCluster|TestResume|TestClientFix|TestSession" ./internal/runtime
go test -count=1 ./pkg/redisbroker ./internal/... ./pkg/transport/... ./cmd/server
go test -count=1 ./...
cd sdks/go && go test -count=1 .
cd sdks/ts && npx jest
cd _examples/chatroom && go build ./...
golangci-lint run ./...
```

门禁:

```bash
# 根包零导出(doc.go 除外不应有 type/func/var/const)
rg -n "^(func |type |var |const )" --glob '*.go' . --glob '!*_test.go' --glob '!internal/**' --glob '!cmd/**' --glob '!pkg/**' --glob '!shared/**' --glob '!sdks/**' --glob '!_examples/**' --glob '!config/**' --glob '!proxy/**'
# 应只剩空,或仅 doc.go 无匹配

# 新位置零根包引用
rg -n '"github.com/messageloopio/messageloop"' internal/runtime --glob '*.go'
# 应为空

# 消费方生产代码不再 import 根包
rg -n '"github.com/messageloopio/messageloop"' cmd/server pkg/transport internal/admin internal/cluster/sim --glob '*.go' | rg -v _test
# 应为空(测试可暂时仍引根——但根已无符号,测试也必须切;所以上式对测试同样应空)

# 红线目录零改动
git diff --name-only -- shared sdks _examples config pkg/topics pkg/redisbroker proxy protocol internal/protocol internal/channel internal/authz internal/cluster/hmac
# 应为空(internal/occupancy、internal/survey、internal/stream 仅允许 §2.3 授权的 defaults/position 新文件)

# 无 aliases.go
test ! -f aliases.go
```

串行跑测试;Redis 真实实例(127.0.0.1:6379,DB 14,容器 `messageloop-test-redis`)。已知 flake 同 D14(backlog #8)。

## 6. 验收清单

1. 三件事全做完;生产 git mv 全部 R 识别;根只剩 doc.go(+ 可选 error_codes 外部测试)。
2. `aliases.go`/`marshaler.go`/`defaults.go` 已删;`internal/rpc` 未建;recover 未并进 stream;survey 编排仍在 Node。
3. 门禁五条符合预期;cmd/server、三 transports、admin、sim 生产代码零根 import。
4. 锁序与 Sim 三条函数语义不变;resume/fence/C1 sim 测试绿。
5. 测试函数总数可对账(减去删除的 marshaler_test);`TestNoUUIDIncarnationInProductionSource` 仍扫到 `cluster.go`(现 `internal/runtime/cluster.go`)与 `cmd/server/main.go`。
6. 全链 + lint 0 issues;文档路径更新;CRLF;无 git 写操作。

## 7. 阶段图(终)

- **D11–D13(已合)**:叶子 / authz / cluster 契约 / metrics。
- **D14(已合 `6e17bdb`)**:session plane + Runtime 缝 + survey 叶子。
- **D15(本 PR)**:Node + Cluster 门面 + recover/health/saga/Sim 钩子 → `internal/runtime`;清根 alias;根包空壳。KD-K26 收口。

之后若再开刀,是新主题(例如真把 Replayer 从 Node 抽成接口、或建 `internal/rpc` 隔开 proxy 类型),不再属于 KD-K26。

## 8. 完成报告

- 改动文件列表(git mv 映射 / 删除 / 新增 / 消费方)
- §6 每条 过/失败 + 证据
- 包装函数直引对照(newHub / newPresenceEvent / allocateNodeIncarnation)
- 测试迁/留/删计数表
- §5 命令真实输出(含门禁)
- 偏离(应尽量无;D14 那种跨包未导出漏检不应再出现——同包测试一起走)

## 9. 实现备注(实现方填)

实现于 2026-08-19,v2 分支,基于 `ecabc5a`(D15 规格已合;D14 tip `6e17bdb` 的后继)。工作区未 commit。验证全绿(`go build ./...`、`go test -count=1 ./internal/runtime` 聚焦集、`go test -count=1 ./pkg/redisbroker ./internal/... ./pkg/transport/... ./cmd/server`、`go test -count=1 ./...`、`sdks/go`、`sdks/ts` npx jest 83、`_examples/chatroom` build、`golangci-lint run ./...` 0 issues)。五条门禁符合预期。

**迁入**:12 生产 + 30 同包测试 `package runtime` + 3 外部测试 `package runtime_test`(原 `messageloop_test`)。git mv 初态均为 `R`;随后 package/import/§3.3 包装改直引后工作树显示 `RM`/`AM`。

**常量拆分**:`internal/runtime/defaults.go`(4)、`internal/occupancy/defaults.go`(`MaxPresenceSnapshotClients=256`)、`internal/survey/defaults.go`(2)。runtime 经 `aliases_local.go` 做本包 const 别名,node.go 短名零改;`admin/api_handler.go` 直引 `occupancy.MaxPresenceSnapshotClients`。未抽 `positionFrom`/`offsetFrom` 到 stream(§2.3 可选,跳过;session 保留 D14 副本,recover 随 Node 进 runtime)。

**包装直引**:`node.go` `newHub`→`session.NewHub`;`newPresenceEvent`/`marshalPresenceEvent`→`occupancy.NewPresenceEvent`/`occupancy.MarshalPresenceEvent`;`cluster.go` `allocateNodeIncarnation`→`cluster.AllocateNodeIncarnation`;`version_test.go` `protocolGenerationOK`→`protocol.GenerationOK`。

**消费方**:cmd/server、三 transports、admin、sim/world.go 及全部外部测试不再 import 根包。`cmd/server/main.go` metrics 包别名 `mlmetrics`(局部名 `metrics` 遮蔽);`world.go` 与 `cluster_redis_integration_test.go` 契约面用 `clusterpkg`(局部变量 `cluster` 遮蔽)。`cmd/server/main.go:136` 注释收为 `cluster.ClusterOptions.Normalize()`。

**测试**:迁入 runtime 318(`package runtime` 300 + `runtime_test` 18);留根 `error_codes_test.go` 改 `package messageloop_test`(1);删除 `marshaler_test.go`(19 Test + 3 Benchmark,shared 已覆盖)。`TestNoUUIDIncarnationInProductionSource` 路径改为 `cluster.go` / `../cluster/epoch.go` / `../../cmd/server/main.go`;`TestOccupancy_NoForbiddenProductionRemnants` 改为 `../session/hub.go` 与 `../occupancy/presence_event.go`。

**根包**:只剩 `doc.go`(无导出)+ `error_codes_test.go`。`aliases.go`/`marshaler.go`/`defaults.go` 已删;`internal/rpc` 未建;recover 未并 stream;survey 编排仍在 Node。`session_runtime.go` 的 `index`/`isWildcard`/`publicationID`/`broadcastParallelLimit`/`pingClusterRefreshInterval` 随迁留在 runtime(session 未导出)。Sim 三条导出函数语义未改。

**偏离**:无行为/锁序/Sim 偏离。可选 `internal/stream/position.go` 未做。为消遮蔽加了 `mlmetrics`/`clusterpkg` 两个 import 别名(不改函数体语义)。未改红线目录里仍写着「aliases.go until D15」的陈旧注释(`internal/{session,cluster,metrics,occupancy,survey}` 包注释)。
