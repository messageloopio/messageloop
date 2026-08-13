# 总控 Prompt：MessageLoop 全项目分模块评审（接力协调用）

> 使用方式：把本文件的全部内容作为 prompt 交给一个具备子代理（subagent）分派能力的 coding agent。它是"接力协调者"，负责把 8 份模块评审任务分派给子代理并行执行并收齐报告。**它自己不负责评审代码，也不要修改任何代码。**

---

## 你的角色

你是评审协调者。项目根目录是 `D:/Codes/qiulin/messageloop`（Go 实时消息平台，pub/sub over WebSocket/gRPC）。

仓库中已准备好 8 份自包含的模块评审任务书，位于 `docs/review/`：

- `docs/review/01-core-session.md` — 核心会话层（client/hub/node/presence/heartbeat/survey/acl 等根包文件）
- `docs/review/02-broker-cluster.md` — Broker 与集群层（broker、pkg/redisbroker、cluster*.go）
- `docs/review/03-proxy-transport.md` — Proxy 与传输层（proxy/、pkg/websocket/、pkg/grpcstream/）
- `docs/review/04-topics-protocol.md` — Topic 匹配与协议层（pkg/topics/、protocol/、shared/、buf 配置）
- `docs/review/05-config-startup.md` — 配置、启动与可观测性（config/、cmd/server/、metrics、health、CI）
- `docs/review/06-sdk-go.md` — Go SDK（sdks/go/）
- `docs/review/07-sdk-ts.md` — TypeScript SDK（sdks/ts/）
- `docs/review/08-consistency-docs.md` — 跨模块一致性与文档（协议三方对齐、文档真实性、仓库卫生）

## 执行步骤

1. **逐一读取**上述 8 个文件，每份文件就是一份完整的子代理任务书（包含背景、范围、评审维度、待核实线索、输出格式约定）。
2. **为每份任务书分派一个子代理**，把该文件的完整内容作为子代理的 prompt，并追加一句：`项目根目录为 D:/Codes/qiulin/messageloop，所有路径以此为基准。你的评审报告请直接完整返回，不要写入文件。`
   - 8 个子代理之间**没有依赖，全部并行分派**。
   - 子代理类型选用具备代码读取与 shell 执行能力的类型（需要能跑 `go build`/`go test`/`npm test`）。
3. **等待全部子代理完成**。如果有子代理失败或超时，用相同 prompt 重试一次；仍失败则在汇总中标记该模块"评审未完成"。
4. **汇总返回**，格式见下。

## 纪律要求（重要）

- 你和子代理都**只读评审，严禁修改仓库中任何文件**（允许的唯一写入是 `go test`/`npm` 产生的缓存类副产物）。
- **不要**对子代理的报告做删减、改写、去重或二次判断——原样搬运。核实与汇总由下游的主 agent 负责。
- 如果某个子代理没有按约定格式输出（缺少级别/位置/证据/置信度字段），要求它补充完整后再收。
- 记录每个模块的基线测试结果（build/test 是否通过），单独列出。

## 最终返回格式

```
# MessageLoop 评审汇总（原始报告集）

## 基线状态
- 模块01: build OK / test OK（或具体失败信息）
- ...（8 个模块逐一列出）

## 模块 01：核心会话层
<子代理报告原文，完整搬运>

## 模块 02：Broker 与集群层
<子代理报告原文，完整搬运>

...（依此类推至模块 08）

## 未完成模块
<如有，列出模块与原因；无则写"无">
```

收到这份汇总后，主 agent 会逐条核实 findings 的真伪、去重、按严重程度排序，形成最终修改方案。
