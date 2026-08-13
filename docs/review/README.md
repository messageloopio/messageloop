# MessageLoop 分模块代码评审

本目录包含对 messageloop 项目进行分模块评审的 prompt。每个文件是一份**自包含**的评审任务书，可直接分派给一个独立的 coding agent 执行（评审 agent 没有任何项目背景，prompt 中已包含所需上下文）。

## 执行方式

**方式 A（推荐）：总控接力。** 把 `00-orchestrator.md` 的完整内容交给一个具备子代理分派能力的 coding agent，它会并行分派 8 个子代理执行各模块评审并原样收齐报告。

**方式 B：手动分派。** 自行按编号把各模块 prompt 分派给不同的 coding agent（可全部并行，模块间无依赖）。

两种方式的共同约定：

- 评审 agent 被要求**只读评审，不修改代码**，输出统一格式的 findings（见各 prompt 末尾的输出格式约定）。
- 各模块评审完成后，把评审报告交给主 agent 做**核实与汇总**：逐条对照源码验证真伪、去重、按严重程度排序，形成最终修改方案。

## 模块划分

| 文件 | 模块 | 范围 |
|---|---|---|
| `01-core-session.md` | 核心会话层 | `client.go`、`hub.go`、`node.go`、`presence*.go`、`heartbeat.go`、`disconnect.go`、`survey.go`、`subscription_saga.go`、`acl.go` 等根包核心文件 |
| `02-broker-cluster.md` | Broker 与集群层 | `broker*.go`、`pkg/redisbroker/`、`cluster*.go` |
| `03-proxy-transport.md` | Proxy 与传输层 | `proxy/`、`pkg/websocket/`、`pkg/grpcstream/` |
| `04-topics-protocol.md` | Topic 匹配与协议层 | `pkg/topics/`、`protocol/`、`shared/`、buf 配置 |
| `05-config-startup.md` | 配置、启动与可观测性 | `config/`、`cmd/server/`、`metrics.go`、`health.go`、YAML、CI |
| `06-sdk-go.md` | Go SDK | `sdks/go/` |
| `07-sdk-ts.md` | TypeScript SDK | `sdks/ts/` |
| `08-consistency-docs.md` | 跨模块一致性与文档 | 协议契约对等性、文档与代码一致性、跨模块语义对齐 |

## 统一的输出格式约定

所有评审 agent 都被要求按如下格式输出每条 finding，便于后续核实汇总：

- **严重级别**：Critical（正确性/数据丢失/安全）/ Important（健壮性/并发/资源泄漏）/ Minor（可读性/一致性/小改进）
- **位置**：`path:line`
- **问题描述** + **证据**（关键代码摘录或复现推理）
- **修复建议**
- **置信度**：high / medium / low（low 表示需要主 agent 重点核实）

## 评审基线

评审开始前要求 agent 先跑 `go build ./...` 和 `go test ./...`（TS SDK 跑 `npm test`）确认基线是否全绿，findings 中区分"基线已红"与"代码评审发现"两类问题。
