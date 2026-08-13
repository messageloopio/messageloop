# 评审任务 08：跨模块一致性与文档

## 背景

你要评审的项目是 **MessageLoop**（工作目录：`D:/Codes/qiulin/messageloop`），一个用 Go 编写的实时消息平台服务端，通过 WebSocket 和 gRPC 双向流提供 pub/sub 消息能力，协议基于 protobuf envelope。先读根目录 `AGENTS.md`、`README.md`、`CLAUDE.md` 了解整体设计。

你是独立评审 agent，没有任何先前上下文。**只做只读评审，不修改任何代码。**

## 评审范围（跨模块一致性与文档）

这是横向评审：不深入单模块内部逻辑，而是核查**模块之间的契约是否对齐**、**文档是否如实描述代码**。范围：

- 协议契约三方对齐：`protocol/`（.proto 定义）↔ 服务端处理（根包 `client.go`、`pkg/grpcstream/api_handler.go`）↔ 两个 SDK（`sdks/go/`、`sdks/ts/src/`）
- 接口多实现语义对齐：`Broker` 的内存实现（`broker_memory.go`）与 Redis 实现（`pkg/redisbroker/`）；`Matcher` 的 5 种实现（`pkg/topics/`）；3 种 `Marshaler`（`shared/marshaler.go`）
- 文档与代码：`docs/developer/01~08`、`docs/protocol.md`、`docs/deployment.md`、`README.md`、`RPC_TIMEOUT.md`、`AGENTS.md`、`CLAUDE.md`、`docs/fix-plan.md`
- 断连码（`disconnect.go` 3000–3512）、错误信封（`sharedpb.Error`）在各层的使用一致性
- 配置项（`config/config.go`）与 `config-example.yaml`、`docs/developer/02-configuration.md` 的三方对齐

## 评审维度

1. **协议特性覆盖矩阵**：列出 `client/v1/service.proto` 定义的全部消息类型与特性（Connect/Resume/Publish/transient/Subscribe/ephemeral/offset/epoch/recover/RPC/SubRefresh/Survey/Ping/Pong），标注服务端、Go SDK、TS SDK 各自是否支持，找出不对等的格子。
2. **断连码与错误语义**：每个 `Disconnect` code 在哪里产生、SDK 如何处理；`sharedpb.Error` 的 code 是否有跨层约定（proto 中无枚举）。
3. **同名概念语义漂移**：如 `History` 的 since_offset inclusive/exclusive、`ephemeral`、`recover`、`offset/epoch` 在不同层的语义是否一致。
4. **文档真实性**：逐份文档抽查关键论断（超时默认值、端口、配置默认值、特性声明）是否与代码一致。
5. **仓库卫生**：`server.exe`（32MB 二进制）、`nul` 文件等不应入库的产物；CRLF/LF 混用；`.gitignore` 覆盖。
6. **AGENTS.md/CLAUDE.md 时效性**：其中描述的架构、命令、约定是否仍与代码一致。

## 已知线索（需你独立核实真伪，确认或推翻，并给出证据）

1. `docs/developer/04-cluster.md` 称 `History` 的 since_offset 为 exclusive，实现疑为 inclusive。
2. `docs/developer/02-configuration.md` 称 heartbeat idle_timeout 为空则禁用心跳，实现疑回退 300s 默认。
3. Go SDK `Subscribe` 疑不支持 ephemeral；两个 SDK 疑都未实现 SubRefresh/Survey；TS SDK 疑声明了 `@grpc/grpc-js` peer dep 但无 gRPC transport。
4. 仓库根目录存在 `server.exe`（32MB）和 `nul` 文件——核实是否被 git 追踪、`.gitignore` 是否应排除。
5. 多个 `.proto` 的 `go_package` 与实际生成目录 `shared/genproto/...` 疑不一致。
6. `docs/fix-plan.md` 疑是一份旧的修复计划——核实其中所列问题是否已修复、该文件是否应归档或删除。
7. `AGENTS.md` 称"Hub uses 64 shards, subscription locks use 16384 shards"——核实常量值是否仍一致。

## 工作流程

1. 先跑 `go build ./... && go test ./...` 确认基线（可选）。
2. 按维度逐项核查，文档部分逐份过。
3. 逐条核实"已知线索"。
4. 补充你自己发现的不一致。

## 输出格式

用中文输出。先给总体评价（3-5 句），然后：

1. **协议特性覆盖矩阵**（表格：特性 × 服务端/Go SDK/TS SDK × 支持情况）
2. 逐条 findings：

```
[级别] Critical / Important / Minor
[位置] path:line（文档问题给文档位置 + 对应代码位置）
[问题] ...
[证据] ...
[修复建议] 改文档还是改代码，给出倾向
[置信度] high / medium / low
```

3. 最后一节列出"建议归档/删除的文档与文件"。不要贴大段内容，每条 finding 引用不超过 10 行。
