# PR-KA-D13 实现规格:KD-K26 阶段三(a)——cluster 契约下沉 internal/cluster + metrics 下沉 internal/metrics

| 字段 | 值 |
| --- | --- |
| 标题 | `refactor: sink cluster contracts into internal/cluster and metrics into internal/metrics` |
| 状态 | **Ready** |
| 依赖 | D12 已合(`d16c1d2`)。在 `v2` 分支上做 |
| 设计来源 | [kernel-architecture.md](../kernel-architecture.md) :173-191(目标包图)、KD-K26;D12 规格 §7 阶段图 |
| 验收人 | 主 agent |

## 1. 目标

KD-K26 阶段三的第一刀。D12 规格 §7 原把「核心拆」记为一个 D13;核实现状后拆成三刀(D13/D14/D15,§7),本 PR 只做第一刀,三件事:

- **(a) cluster 控制面契约下沉**:`cluster.go` 的契约段(Options + 9 个接口 + 命令类型/状态/模型)、`cluster_state.go` 的 DTO/错误/handler/CAS 段、`cluster_epoch.go` 全文、`cluster_user_index.go` 的 `SyncUserIndex` → 新建 **`internal/cluster`** 包(该目录现仅有 `hmac/`、`sim/` 两个子包,根上无 .go 文件,已核实)。含**一项**导出手术(§3.2)。
- **(b) metrics 下沉**:`metrics.go` 全文 `git mv` → 新建 **`internal/metrics`** 叶子包(只依赖 prometheus,已核实)。
- **(c) 消费方切直引**:`pkg/redisbroker`、`internal/cluster/hmac` 生产代码**零根包引用**(门禁);`internal/cluster/sim` 契约面切 `internal/cluster`,门面面(NewNode/NewCluster/NewClusterRepairer 等)留根引用,待 D15。

**Cluster 门面不搬**(`Cluster`/`NewCluster`/`ClusterDependencies`/noop 群/lease manager 实现留根至 D15):`NewCluster` 的 repairer 自动派生(cluster.go:284 调 `NewClusterRepairer(nil, ...)`)与 `clusterComponentName`(cluster.go:411 类型匹配 `*clusterRepairer`)把门面绑在 Node 依赖的 `cluster_repair.go` 上,本刀不拆这个结(§3.4)。session plane、recover、survey、health 全部不动(D14/D15)。

### 1.1 零变化红线

行为零变化;常量/码表值不变;除 §3.2 列明的一项导出手术外,导出符号的名字、签名、语义逐字节等价。proto、`shared/`、`sdks/`、`_examples/`、`config/`、`pkg/topics`、`pkg/transport/`、`internal/{protocol,channel,occupancy,stream,authz,admin}`、`proxy/`、`cmd/server` 零改动。

### 1.2 为什么延续 alias 过渡(防实现方改道)

与 D11/D12 一致:根 `aliases.go` 是**唯一**过渡点,新增 `--- internal/cluster ---` / `--- internal/metrics ---` 两组转发,根包其余文件(生产与在包测试)、`cmd/server`、外部测试包(`cluster_redis_integration_test.go`、`cluster_sim_test.go`)**零改动**。不允许把根包文件改成直引 internal/*(那是 D15 清除 alias 时的活,现在做只会制造无关 churn 并扩大 blast radius)。`aliases.go:11` 的「新代码不准引根 alias」指**新写**的代码;既有根包代码经 alias 不动。

## 2. 允许改动的文件

- 新建 `internal/cluster/`:`doc.go`(包注释)、`contracts.go`(§3.1 表 A 段)、`state.go`(表 B 段)、`user_index.go`(`SyncUserIndex`)。
- git mv:`cluster_epoch.go` → `internal/cluster/epoch.go`;`metrics.go` → `internal/metrics/metrics.go`。
- 修改(仅删除已搬出段落,其余逐字节不变):`cluster.go`、`cluster_state.go`、`cluster_user_index.go`。
- 修改(仅新增):根 `aliases.go`(两组转发 + 一个未导出包装,§3.2/§3.7)。
- import/限定名切换:`pkg/redisbroker/{redis.go, cluster_command_bus.go, cluster_directory.go, cluster_query_store.go}` 及其同包测试(含 `cluster_command_bus_test.go`);`internal/cluster/hmac/{hmac.go, hmac_test.go}`;`internal/cluster/sim/{bus.go, bus_test.go, directory.go, directory_test.go, world.go}`。
- 测试拆分:`cluster_epoch_test.go`、`metrics_test.go`、`cluster_user_index_test.go` 各拆出随迁部分(§3.6);新增 `internal/cluster/{epoch_test.go, user_index_test.go}`、`internal/metrics/metrics_test.go`。授权`TestNoUUIDIncarnationInProductionSource` 的源码扫描清单加 `internal/cluster/epoch.go`(§3.6)。
- 文档路径引用更新:`CLAUDE.md`(:129-136 一带)、`docs/developer/01-architecture.md`(:53-54、:260、:502 一带)中 `metrics.go`/`cluster.go` 的路径表述(`cluster.go` 仍在根,仅 metrics 与新包需改)。
- `docs/v2/README.md` 状态行、本规格 §9。

## 3. 现状(主 agent 已核实;行号漂移以语义为准)

### 3.1 搬运集 A → `internal/cluster`(package `cluster`)

表 A(自 `cluster.go`,:55-220 契约段 + Options):

| 符号 | 种类 | 行号 | 备注 |
| --- | --- | --- | --- |
| `ClusterOptions` + `normalize` | struct + method | :13-53 | 零根包依赖;**必须同搬**——`AllocateNodeIncarnation` 签名引用它(§3.2) |
| `ClusterLifecycle` | interface | :56-59 | |
| `SessionDirectory` | interface | :71-89 | 引用表 B 的 lease/snapshot DTO,同搬 |
| `ClusterCommandBus` | interface | :92-97 | 引用 `ClusterCommandHandler`(表 B),同搬 |
| `ClusterNodeProjection` | struct | :100-103 | |
| `ClusterQueryStore` | interface | :106-116 | |
| `ClusterNodeLeaseManager` | marker interface | :119-121 | 实现留根(§3.4),接口先就位 |
| `ClusterRepairer` | marker interface | :128-130 | 同上(`clusterRepairer` 实现 Node 依赖,留根) |
| `ClusterSessionLeaseLister` | interface | :136-138 | |
| `ClusterNodeLeaseLister` | interface | :143-145 | |
| `ClusterCommandType` + 6 常量 | string 枚举 | :148-163 | 值逐字节不变 |
| `ClusterCommandStatus` + 5 常量 | string 枚举 | :166-179 | 同上 |
| `ClusterCommand` | struct | :182-201 | |
| `ClusterCommandResult` | struct | :204-220 | |

表 B(自 `cluster_state.go`):

| 符号 | 种类 | 行号 | 备注 |
| --- | --- | --- | --- |
| `ErrClusterCommandUnsupported`、`ErrSessionFenced` | error 变量 | :27-34 | 留根侧消费点(session.go:603、client.go:1318、cluster_state.go:296-331、noop bus)经 alias 零改动 |
| `ClusterNodeLease` | struct | :37-43 | 纯 JSON DTO,零依赖 |
| `ClusterSessionLease` | struct | :45-57 | 同上 |
| `ClusterSubscriptionSnapshot` | struct | :59-63 | 同上 |
| `ClusterSessionSnapshot` | struct | :65-89 | 同上 |
| `ClusterChannelInfo` | struct | :92-95 | 同上 |
| `ClusterCommandHandler` | func 类型 | :97-98 | 签名 `(ctx, *ClusterCommand) (*ClusterCommandResult, error)`,引用均在搬运集 |
| `SessionStateCompareAndSwapper` | interface | :100-112 | 文档注释随行 |

`cluster_epoch.go` **全文**(:1-113):`NodeEpochAllocator`、`FormatNodeEpoch`、`ParseNodeEpoch`、`NodeEpochNewer`、`MemoryNodeEpochAllocator`、`NewMemoryNodeEpochAllocator`、`sharedMemoryNodeEpochAllocator`、`allocateNodeIncarnation`。import 仅 stdlib,零根包依赖。

`cluster_user_index.go` 的 `SyncUserIndex`(:22-57):签名 `(ctx, directory SessionDirectory, oldLease, newLease *ClusterSessionLease, ttl time.Duration) error`,只引搬运集类型;根包生产代码**零调用点**(仅 cluster_user_index_test.go 与 redisbroker 消费)。

搬运集**不引用** `cluster_state.go:13-25` 的 5 个 default 常量(逐一比对已核实),常量整体留根,无需手术。

### 3.2 导出手术(唯一一项)

`allocateNodeIncarnation`(cluster_epoch.go:97,自由函数,签名 `(options ClusterOptions, directory SessionDirectory) (string, error)`)→ `AllocateNodeIncarnation`。唯一调用点是留根的 `NewCluster`(cluster.go:276);在 `aliases.go` 放**未导出包装**(D11 `protocolGenerationOK` 先例),`cluster.go` 逐字节不变:

```go
func allocateNodeIncarnation(options ClusterOptions, directory SessionDirectory) (string, error) {
	return cluster.AllocateNodeIncarnation(options, directory)
}
```

### 3.3 搬运集 B → `internal/metrics`(package `metrics`)

`metrics.go` 全文(:1-266):`MetricsTransportLabel`(:9)、`Metrics`(:21,27 个 prometheus 字段)、`NewMetrics`(:58)。import 仅 `github.com/prometheus/client_golang/prometheus`,零根包引用,零手术,整文件 `git mv`。

### 3.4 留根清单(防越搬,本刀全不动)

- `cluster.go`:`ClusterDependencies`(:223-233)、`Cluster`(:236-245)、`clusterStartRollbackTimeout`(:247)、`NewCluster`(:250-294)、`Enabled/NodeID/IncarnationID/Backend`(:297-327)、`Start/Shutdown/components`(:333-393)、`clusterComponentName`(:397-416)、`noopClusterComponent`(:418-421)、5 个 noop 声明(:423-427)。
- `cluster_state.go`:5 个 default 常量(:13-25)、noop 方法集(:114-196)、`Cluster` accessors(:199-220)、全部 Node 方法(:223-469)、`ClusterNodeLeaseManagerConfig` + `NewClusterNodeLeaseManager` + `clusterNodeLeaseManager`(:471-578)。
- 全文留根:`cluster_commands.go`、`cluster_resume.go`、`cluster_repair.go`、`cluster_sim.go`、`cluster_user_index.go` 的 `Node.ExpandUserSessions`/`Node.ObserveAdminUserFanout`(:67-108)。
- 绊脚石记录(D15 处理):`NewCluster`:284 的 repairer 自动派生、`clusterComponentName`:411 的 `*clusterRepairer` 类型匹配。

### 3.5 消费方切换清单(已核实全集)

- **pkg/redisbroker**(生产:redis.go:15、cluster_command_bus.go:34、cluster_directory.go:11、cluster_query_store.go:11 的根 import):消费契约接口/DTO + `ClusterCommandStatus*` 常量 + `FormatNodeEpoch` + `SyncUserIndex`(cluster_directory.go:438)+ `Metrics`/`NewMetrics`(`SetMetrics` 注入),**全部在搬运集**,一刀切到 `internal/cluster` + `internal/metrics`;切后生产代码零根包引用。测试 `cluster_command_bus_test.go` 用 `ClusterCommandDisconnect`(:202)等,同切。
- **internal/cluster/hmac**:hmac.go 只引 `ClusterCommand`/`ClusterCommandResult`(:63/75/98/108/127/143);hmac_test.go 另用 Type/Status 常量。全切。
- **internal/cluster/sim**:`bus.go`、`bus_test.go`、`directory.go`、`directory_test.go` 纯契约,全切;`clock.go`/`clock_test.go` 不引根包,不动;`world.go` 契约面只有 `ClusterRepairer`(:88 一带)切 `internal/cluster`,门面面(`NewNode`/`NewClient`/`Session`/`JSONMarshaler`/`Transport`/`Disconnect`/`NewCluster`/`ClusterOptions`/`ClusterDependencies`/`NewClusterRepairer`/`ClusterRepairerConfig`)继续引根包(双 import,D15 再收)。
- **cmd/server**:`main.go:54`(`NewMetrics`)、:76/:216(`SetMetrics(*messageloop.Metrics)` 断言)、:185(`NodeEpochAllocator`)、:193(`FormatNodeEpoch`)经 alias **零改动**。
- **transports / internal/admin / proxy**:对搬运集 grep 零命中,无感。

### 3.6 测试面(逐文件判定,已核实)

- `cluster_epoch_test.go` **拆**:迁 `internal/cluster/epoch_test.go`——`TestFormatNodeEpoch`(:13)、`TestParseNodeEpoch`(:21)、`TestNodeEpochNewer`(:38)、`TestMemoryNodeEpochAllocator`(:52);留根——3 个 `TestNewCluster_*`(:74/82/97,测门面)+ `TestNoUUIDIncarnationInProductionSource`(:116,按相对路径 `os.ReadFile("cluster.go")` 等做源码扫描,依赖包运行目录=仓库根,必须留根;**授权**把扫描清单扩上 `internal/cluster/epoch.go`,保住「incarnation 禁 UUID」守护的覆盖语义)。留根测试里的 `ParseNodeEpoch` 引用(:90/:92)经 alias 零改动。
- `metrics_test.go` **拆**:迁 `internal/metrics/metrics_test.go`——`TestMetricsTransportLabel`(:55)+ 全部 `*Registered` 注册表测试(:68/97/120/157/181/200/246/270 一带);留根——`TestMetrics_ConnectionsTotal_TransportLabels`(:18-53,用 `NewNode`/`NewClient`/`WithProtocol` 测接线,不是 Metrics 本体)。
- `cluster_user_index_test.go` **拆**:迁 `internal/cluster/user_index_test.go`——`TestSyncUserIndex_MigratesOnUserChange`(:16)、`TestSyncUserIndex_DeleteRemovesMembership`(:45);两者依赖 `fakeSessionDirectory`(定义在留根的 cluster_remote_test.go:13),**授权**把该 fake 复制进新测试文件。留根——`TestExpandUserSessions_*`(:73/106)、`TestClusterUserIndexRepairer_RebuildsMemberships`(:140)。
- 其余根测试**零改动**(在包测试经 alias;外部测试包 `cluster_redis_integration_test.go`、`cluster_sim_test.go` 经 alias 零改动)。
- 迁出测试在新包内为同包测试(package `cluster` / `metrics`),除 package/import 行外逐字节不变。**测试函数总数前后一致**,完成报告给出迁出/留根计数表。

### 3.7 alias 新增内容(aliases.go 唯一被授权的新增)

`--- internal/cluster ---` 组:§3.1 表 A/B 全部类型逐个 type alias;`ClusterCommandType*`×6、`ClusterCommandStatus*`×5 常量逐形状转发;`ErrClusterCommandUnsupported`、`ErrSessionFenced`、`FormatNodeEpoch`、`ParseNodeEpoch`、`NodeEpochNewer`、`NewMemoryNodeEpochAllocator`、`SyncUserIndex` var 转发;外加 §3.2 的未导出包装。`--- internal/metrics ---` 组:`Metrics` type alias + `NewMetrics`、`MetricsTransportLabel` var 转发。

### 3.8 遮蔽与命名

- 新包名 `cluster` 与根包 `Cluster` 类型不冲突(不同包)。redisbroker/sim 文件若已有占用 `cluster` 标识符的局部名,import 用 `clusterpkg` 别名(D12 `channelpkg` 先例),全文件统一。
- `internal/cluster` 是 `hmac`/`sim` 的父包,import 方向只能是子包 → 父包,父包不准引子包(本刀无此需求)。

## 4. 组织原则

1. `cluster_epoch.go`、`metrics.go` 两枚整文件 `git mv`;部分搬出用「复制到新文件 + 删除原文件对应段落」。除 package/import 行、§3.2 手术、redisbroker/hmac/sim 的限定名替换外,搬动内容逐字节不变。
2. `aliases.go` 是唯一过渡点(§1.2);新位置代码(`internal/cluster`、`internal/metrics`)**零根包引用**;redisbroker/hmac/sim 直引 `internal/*`。
3. 常量/码表值逐字节不变;JSON tag 不变(DTO 是 Redis 序列形状)。
4. 新包各加包注释(`Package cluster ...` / `Package metrics ...`),内容点明 KD-K26 阶段三(a)来源。

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
# redisbroker/hmac 生产代码零根包引用
grep -rn '"github.com/messageloopio/messageloop"' pkg/redisbroker internal/cluster/hmac --include="*.go" | grep -v _test   # 应为空
# 新位置零根包引用
grep -rn '"github.com/messageloopio/messageloop"' internal/cluster/*.go internal/metrics/*.go   # 应为空
# sim 契约面切净(门面符号 NewNode/NewCluster/NewClusterRepairer/ClusterDependencies 等不在此列)
grep -rn 'messageloop\.\(ClusterCommand\|SessionDirectory\|ClusterSessionLease\|ClusterNodeLease\|ClusterSessionSnapshot\|ClusterSubscriptionSnapshot\|ClusterChannelInfo\|ClusterNodeProjection\|SessionStateCompareAndSwapper\|ClusterCommandHandler\|ClusterCommandStatus\|ClusterCommandType\|ClusterRepairer\|Cluster.*Lister\)' internal/cluster/sim --include="*.go"   # 应为空
# 红线目录零改动
git diff --name-only -- shared sdks _examples config pkg/topics pkg/transport internal/protocol internal/channel internal/occupancy internal/stream internal/authz internal/admin proxy cmd/server   # 应为空
# 根包生产文件:除 cluster.go / cluster_state.go / cluster_user_index.go / aliases.go 外零改动
git diff --name-only -- '*.go' ':!*_test.go'   # 应只命中上述 4 个 + 新包文件 + redisbroker/hmac/sim
```

串行跑测试,绝不并发两个根目录 `go test`;Redis 真实实例(127.0.0.1:6379,DB 14)。

## 6. 验收清单

1. 三件事全做完;两枚 `git mv` R 识别;部分搬出内容除授权手术与 package/import/限定名外逐字节不变。
2. 导出手术仅 `allocateNodeIncarnation` → `AllocateNodeIncarnation` 一项;5 个 default 常量留根未动;DTO 的 JSON tag 逐字节不变。
3. 门禁 grep 四条全空/符合预期;redisbroker、hmac 生产代码零根包引用。
4. `aliases.go` 只新增 §3.7 两组 + 一个包装;根包其余生产文件、`cmd/server`、transports、internal/admin、proxy 零改动。
5. 测试拆分三处与 §3.6 一致,测试函数总数前后一致;`TestNoUUIDIncarnationInProductionSource` 扫描清单含 `internal/cluster/epoch.go`。
6. C1 sim、错误码普查、全链、SDK/TS/chatroom、lint 0 issues 全绿。
7. `CLAUDE.md` 与 `docs/developer/01-architecture.md` 路径引用更新;未碰 §1.1 红线;无 churn、.go 保持 CRLF、无 git 操作。

## 7. 阶段图(更新)

- **D11(已合)**:叶子契约下沉 + 根 alias 过渡。
- **D12(已合)**:authz/channel 下沉;transport 改名;admin 剥离。
- **D13(本 PR)**:cluster 控制面契约 + epoch + `SyncUserIndex` 下沉 `internal/cluster`;metrics 下沉 `internal/metrics`;redisbroker/hmac 切断根包依赖。
- **D14**:session plane 下沉 `internal/session`(session.go/client.go/heartbeat.go/hub.go/pool.go/transport.go;`Session.node` 依赖反转为注入接口,核实面:~30 个 Node 方法 + 7 个字段访问);Sim 钩子归位。
- **D15**:Node 本体 + cluster Node 方法 + Cluster 门面/lease manager + recover + survey + health → `internal/runtime`(survey 注册表 → `internal/survey`;`recover.go` 与 `internal/stream` 合流与否、`internal/rpc` 用途、`defaults.go`/`marshaler.go` 归位,均在 D15 规格定);清除根 alias;根包退出或只剩空壳。

## 8. 完成报告

- 改动文件列表(git mv 映射 / 新增 / 修改分组)
- §6 每条 过/失败 + 证据
- 导出手术逐符号清单(名字前→后)
- 测试拆分逐个判定表 + 前后总数
- §5 命令真实输出(含四条门禁 grep)
- 偏离(应无)

## 9. 实现备注(实现方填)
