# 任务书 02：跨节点精确续读（ChannelOffsets）+ payload 转换抽取 + metrics transport label

## 角色

你是 MessageLoop（Go 实时消息平台，项目根 `D:/Codes/qiulin/messageloop`）的实现工程师。本任务实现 backlog 的 B4 + A1 + A3 三项（决策已定，见 `docs/review/backlog.md`）。

**前置依赖**：本任务必须在任务书 01（topics-validation）完成合入后再启动——01 改 `hub.go` 订阅入口，本任务改 `hub.go` 投递路径与 metrics 调用点、及 `client.go` 恢复路径，同文件不能并行。

## 文件归属（只许改这些）

- `cluster_state.go`、`cluster_resume.go`、`cluster_commands.go`（如需）
- `hub.go`（仅投递/广播路径的 offset 簿记）
- `client.go`（恢复路径消费 + A1 调用点）
- `broker.go`、`broker_memory.go`（如簿记需落在 broker 投递路径）
- `pkg/grpcstream/api_handler.go`（仅 A1 调用点收敛）
- 根包新增一个小文件（如 `publication.go`）放 A1 抽取的共享函数
- `metrics.go` 及 `ConnectionsTotal` 的所有 Inc/Dec 调用点（A3）
- 相关 `*_test.go`

如确需越出清单，必须在报告中显著标注并给出理由。

## 任务 1（B4）：填充 `ClusterSessionSnapshot.ChannelOffsets`

现状：

- `cluster_state.go:67-73` 预留了 `ChannelOffsets map[string]uint64` 字段，注释明确标注 NOT populated——hub 不记录每频道最后投递 offset，跨节点 resume 只能回退到客户端自报 offset（`client.go:727-741`，用 `sub.Offset+1` 做 `History` 恢复），客户端自报 offset 不可信/可能缺失。`BrokerEpoch`（cluster_state.go:74-77）已填充，可参照其链路。
- 投递路径：deliverOnce worker 池（上一轮修复引入，同频道严格保序）——先查 `hub.go`/`broker_memory.go` 找到每会话每频道实际投递成功的位置。

要求：

1. **先调查后设计**：确认是否存在现成的每会话每频道"最后投递 offset"簿记；若无，在投递成功路径增加轻量簿记（每 session 每 channel 一个 uint64，随退订/断连清理，注意 ephemeral presence 对称清理的既有模式——参考上一轮 `LookupSubscriber` 回读的做法）。簿记必须便宜：不得在每消息热路径引入锁竞争，优先挂在已有的分片锁/subShard 内。
2. 快照填充：写快照（cluster_state.go 的 snapshot 链路）时带上 `ChannelOffsets`。
3. 恢复消费：跨节点 resume（cluster_resume.go + client.go 恢复路径）优先使用服务端记录的 `ChannelOffsets[ch]+1`，缺失时才回退客户端自报 offset；`BrokerEpoch` 校验逻辑保持既有语义（epoch 变化强制全量恢复）。
4. 对称清理：会话迁移回滚（`evictSessionForTakeover` 回滚路径，参考上一轮修复）、会话关闭、退订时簿记同步清理。
5. 更新 `cluster_state.go:67-73` 的注释（移除 NOT populated 标注，说明簿记来源）。

## 任务 2（A1）：payload→Publication 重复转换抽取

现状：同一套 `sharedpb.Payload` oneof → `messageloop.Publication`（含 `PayloadKind` 判定与 JSON `MarshalJSONStruct` 错误处理）的转换逻辑至少重复在两处：`pkg/grpcstream/api_handler.go:35-59` 与 `:283-297`、`client.go`（约 1070-1090、1370、1420 三处 oneof 分支——以实际代码为准）。

要求：

1. 在根包抽取共享函数（如 `PublicationFromPayload(id string, md map[string]string, p *sharedpb.Payload) (*Publication, error)`，签名以三处调用点的实际需要为准），放新文件（如 `publication.go`）。
2. 所有调用点收敛到共享函数，行为完全等价（特别是 JSON 变体的错误处理路径）。
3. 不追求消灭所有重复——只收敛语义相同的三处以上重复，签名不适配的调用点保持原样并在报告中说明。

## 任务 3（A3）：`connections_total` 增加 `transport` label

现状：`metrics.go` 全部指标无 label，无法按传输区分 ws/grpc 连接数。

要求：

1. `ConnectionsTotal` 由 `prometheus.Gauge` 改为 `*prometheus.GaugeVec`，加 `transport` label（取值 `ws` / `grpc`）。
2. 找到所有 `ConnectionsTotal` 的 Inc/Dec 调用点，按连接来源传入正确 label 值；调用点若无法直接知道 transport 类型，溯源到 transport 构造处（WS 在 `pkg/websocket/`，gRPC 在 `pkg/grpcstream/`——**这两个目录不在归属内**，若 label 来源需要在 transport 构造处传参，优先在根包调用点用现有信息判定；确需改 pkg 内文件时在报告中说明，不要改）。
3. 只改这一个指标，不扩散到 `messages_delivered_total` 等其他指标。
4. 文档提及该指标处（`docs/developer/` 可观测性文档）同步。

## 测试要求

- B4：簿记账 unit 测试；快照含 ChannelOffsets 的 round-trip 测试；resume 优先服务端 offset 的测试（伪造服务端 offset 与客户端自报不一致的场景）；清理路径测试。
- A1：等价性测试（三变体 + JSON 错误路径）。
- A3：label 值正确性测试（ws/grpc 两路径的 Inc/Dec）。
- 通过：`go build ./... && go test -race -count=1 . ./pkg/grpcstream/...`。

## 纪律

- 不做任何 git 写操作；改动最小化；注释与实际行为同步。
- 报告格式：完成项清单（file:line 证据）、设计取舍（簿记位置与成本）、行为变更显著标注、测试验证方式与结果、遗留问题。
