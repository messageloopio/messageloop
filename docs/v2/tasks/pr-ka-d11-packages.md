# PR-KA-D11 实现规格:KD-K26 包重划阶段一——叶子契约下沉 internal/*

| 字段 | 值 |
| --- | --- |
| 标题 | `refactor: sink leaf contracts into internal/{protocol,channel,occupancy,stream}` |
| 状态 | **Ready**(待实现) |
| 依赖 | D10 已合(`6f77006`)。在 `v2` 分支上做 |
| 设计来源 | [kernel-architecture.md](../kernel-architecture.md) :173-191(目标包图)、KD-K26(包按 `internal/*` 重划,不强制 re-export 旧根包符号)、KD-K31(独立版本可破 import) |
| 验收人 | 主 agent |

## 1. 目标

KD-K26 的全量目标是把根包 `package messageloop`(34 个生产文件)拆进 `internal/{runtime,session,protocol,channel,stream,occupancy,authz,rpc,survey,cluster,admin}`。一次 PR 做完整拆分不可评审,本 PR 是**阶段一:机械下沉五个叶子契约组**,它们的外部依赖已全部核实为「只出不进」(见 §3),零行为变化、零接口形状变化。

硬核部分(Session.node 回指针反转、cluster 方法挂 Node、survey 三方耦合、channel_policy↔authorizer 的未导出耦合、transport 改名、grpcstream admin 剥离)**不在本 PR**,留给 D12/D13(§7 有阶段图)。

### 1.1 本 PR 搬五组

| 目标包 | 文件 | 内容 |
| --- | --- | --- |
| `internal/protocol` | `disconnect.go`、`version.go` | `Disconnect` 类型 + 3000-3514 码表;协议世代门 |
| `internal/channel` | `interest.go`(仅此一个) | `CompileInterest`/`CompiledInterest`/`MatchAfterCompile`/`ErrPatternNotRoutable` |
| `internal/occupancy` | `presence.go`、`presence_event.go`、`occupancy.go` | `PresenceInfo`/`PresenceStore`/内存实现、`PresenceEvent` + action 常量、`OccupancyEvent`/`OccupancyGenSource`/`SyntheticLeaveReporter`/`ErrLateOccupancy`(+ `OccupancyHandler`,若定义在 `broker.go` 则随 `broker.go` 走) |
| `internal/stream` | `broker.go`、`publication.go`、`broker_memory.go` + `defaults.go` 中的 `DefaultHistoryLimit` 常量 | `Publication`/`PayloadKind`/`Broker` 接口/`PublicationHandler`/`HistoryPage`/`HistoryGapReason`/`CatchUpGap`/`GapHandler`、`PublicationFromPayloadV2`、`MemoryBroker` |
| (根包) | 新增 `aliases.go` | 过渡 type alias / var / const 转发(见 §1.2) |

依赖方向(已核实,无环):`internal/stream` → `internal/channel`(CompileInterest)+ `internal/occupancy`(OccupancyEvent/OccupancyHandler);`internal/occupancy` 自洽;`internal/channel` → `pkg/topics`;`internal/protocol` 无内部依赖;四包均可引 `config`/`shared`/`pkg/topics`。

### 1.2 根包 alias 过渡(KD-K26 明示允许不 re-export,但本阶段为控 diff 而转发)

新增根文件 `aliases.go`(package messageloop),集中转发五组已下沉符号,文件头注明「PR-KA-D11 过渡转发,阶段三(D13)清除;新代码不准引根 alias」。形式:

- 类型:`type Disconnect = protocol.Disconnect`
- 常量:`const DisconnectStale = protocol.DisconnectStale`(码表整块)
- 函数:`var CompileInterest = channel.CompileInterest`(或等值包装函数)
- 未导出符号:`version.go` 的 `protocolGenerationOK` 搬出后导出为 `protocol.GenerationOK`,根 alias 文件保留同名包装函数,根内调用点零改动

效果:`cmd/server`、`pkg/websocket`、`pkg/quicstream`、`pkg/grpcstream`、`proxy`、`internal/cluster/*`、根包其余文件 **零改动** 仍编译。

### 1.3 pkg/redisbroker 改引新路径

redisbroker 是五组符号的最重外部消费者(Publication 等 100+ 处)。本 PR 把 redisbroker 对这五组的 import 全部改到新路径(`stream.Publication`、`occupancy.PresenceInfo`、`channel.CompileInterest`、`stream.DefaultHistoryLimit` …),**不允许再经根 alias 引这五组**。cluster 契约(SessionDirectory/ClusterCommand*/lease 类型等)继续引根包——它们本阶段未搬。

### 1.4 不做

- channel_policy.go / authorizer.go(两者经未导出符号 `compilePolicySpec`/`overlay`/`policySpec` 耦合,下沉需要导出手术,D12 做)。
- survey.go、recover.go、cluster 一族、session/hub/client/node、metrics.go、health.go、pool.go(留根,随后续阶段)。
- marshaler.go(纯 re-export,保留到 D13 统一清理)。
- transport 改名 `pkg/transport/{ws,grpc,quic}`、grpcstream admin 面剥 `internal/admin`(D12)。
- 任何接口签名/行为/常量值变化;proto、SDK、`shared/`、`_examples/`、`pkg/topics`、`config/`;git commit / tag / push。

## 2. 允许改动的文件

- 新增:`internal/protocol/`、`internal/channel/`、`internal/occupancy/`、`internal/stream/` 下文件(git mv 自根目录对应文件);根 `aliases.go`
- 修改:`defaults.go`(仅移除 `DefaultHistoryLimit`)、`pkg/redisbroker/*.go`(仅 import 与限定名替换)、`docs/v2/README.md`(状态行)、本规格 §8
- 测试搬运(见 §3.3):仅当测试只依赖被搬符号时随行;否则留根
- `error_codes_test.go`:普查扫描路径适配(见 §3.4)

## 3. 现状(主 agent 已核实,行号以语义为准)

### 3.1 五组文件的出边(全部已 grep 核实)

- `disconnect.go`:仅 `fmt`。`version.go`:仅 `strconv`/`strings`(未导出 `protocolGeneration`/`protocolGenerationOK`)。
- `interest.go`:仅 `pkg/topics`;符号全部已导出。
- `presence.go`/`presence_event.go`/`occupancy.go`:仅 stdlib;`memoryPresenceStore` 未导出但有导出构造器;`SyntheticLeaveReporter`/`OccupancyGenSource` 不引根类型。
- `broker.go`:仅 `sharedv2`+`structpb`;`Broker` 接口体内的 occupancy 类型来自同包 `occupancy.go`(随搬)。
- `broker_memory.go`:外部 `uuid`/`log` + `CompileInterest`(→channel)+ `OccupancyEvent`(→occupancy)+ `DefaultHistoryLimit`(随搬)。
- `DefaultHistoryLimit` 全仓非测试使用点:`broker_memory.go:343`、`pkg/redisbroker/history.go:32`;根包无其它引用(不需要 alias 转发该常量)。

### 3.2 留根不搬的耦合(供 D12/D13,本 PR 不碰)

- `channel_policy.go` ↔ `authorizer.go`:未导出 `compilePolicySpec`/`overlay`/`policySpec`(含未导出字段)互相引用。
- `session.go:378/391/511`、`client.go:519/1317`:Session→Node 上 cluster 方法回指针。
- `recover.go` 消费 `*ClusterSessionSnapshot`;survey 三方耦合(node.go:780 / cluster_commands.go / client.go:1493)。

### 3.3 测试搬运规则

- 纯测试(只依赖被搬符号,不引 Node/Session/Hub)随生产文件搬:`disconnect_test.go`、`version_test.go`、`interest_test.go`、`broker_memory_test.go` 候选——逐个核对后搬,完成报告说明每个的判定。
- `presence_test.go`、`occupancy_test.go`、`authorizer_test.go` 预期引 Node,留根(经 alias 编译);核对结果写报告。

### 3.4 error_codes_test 注意

`error_codes_test.go` 是全仓错误码普查(按 emission site 扫描生产文件)。`disconnect.go` 搬到 `internal/protocol/disconnect.go` 后,若测试硬编码了文件路径/目录清单,需适配为新路径;普查断言的码表内容不变。

## 4. 算法(本 PR 无算法,只有组织原则)

1. 一律 `git mv` 保持文件历史;搬动文件除 `package` 行与 `import` 块(外加 `version.go` 的 `GenerationOK` 导出名)外**逐字节不变**。
2. alias 集中一个文件,不散落;alias 文件本身加包注释说明过渡性质。
3. 包名:`protocol`/`channel`/`occupancy`/`stream`;doc comment 仿照旧文件头,标注目标架构出处(kernel-architecture.md :173-191)。

## 5. 验证命令

```bash
go build ./...
go test -count=1 -run "TestSim_|TestCluster|TestResume|TestClientFix|TestSession" .
go test -count=1 ./pkg/redisbroker ./internal/...
go test -count=1 ./...
cd sdks/go && go test -count=1 .
cd sdks/ts && npx jest
cd _examples/chatroom && go build ./...
golangci-lint run ./...
```

门禁 grep:

```bash
# redisbroker 不再经根包引五组符号:其 import 中根包只剩 cluster 契约用途
grep -rn "messageloop\.\(Publication\|PayloadKind\|Broker\|HistoryPage\|CatchUpGap\|PresenceInfo\|PresenceStore\|OccupancyEvent\|CompileInterest\|MatchAfterCompile\|DefaultHistoryLimit\)" pkg/redisbroker --include="*.go"   # 应为空
# 根包外零改动
git diff --name-only -- cmd/ pkg/websocket pkg/quicstream pkg/grpcstream proxy config shared sdks _examples   # 应为空
```

串行跑测试,绝不并发两个根目录 `go test`;Redis 用真实实例(127.0.0.1:6379,DB 14)。

## 6. 验收清单

1. 五组文件全部 `git mv` 到位;除 package/import/§1.2 允许的导出名外逐字节不变(`git diff -M` 核对 rename 相似度)。
2. 接口形状零变化:`Broker`/`PresenceStore`/`OccupancyGenSource`/`SyntheticLeaveReporter`/`CompiledInterest`/`Disconnect` 签名与码表值逐字节等价。
3. 根 `aliases.go` 唯一转发点;`cmd/server`/三包 transport/grpcstream/proxy/internal/cluster/* 零改动编译通过。
4. redisbroker 五组符号只引新路径(§5 grep 为空);cluster 契约仍引根包。
5. C1 sim 六场景、错误码普查、全量测试链、SDK/TS/chatroom、lint 0 issues 全绿。
6. 未碰 §1.4 禁止项;无格式 churn(`.go` 全 CRLF);无 git 操作。

## 7. 阶段图(供后续 PR 索引,本 PR 只做阶段一)

- **D11(本 PR)**:叶子契约下沉 + 根 alias 过渡 + redisbroker 改引。
- **D12**:`channel_policy`+`authorizer` 导出手术后下 `internal/channel`/`internal/authz`;transport 改名 `pkg/transport/{ws,grpc,quic}`;grpcstream admin 面剥 `internal/admin`。
- **D13**:session/runtime/cluster/survey 核心拆(Session.node 依赖反转、Sim 钩子归位、recover 与 stream 合流),清除根 alias,根包退出或只剩空壳。

## 8. 完成报告

- 改动文件列表(git mv 映射 / 新增 / 修改分组)
- §6 每条 过/失败 + 证据
- 测试搬运逐个判定表(搬/留 + 理由)
- §5 命令真实输出
- 偏离(应无)

## 9. 实现备注(实现方填)

(留空)
