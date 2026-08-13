# 总控 Prompt：MessageLoop 修复接力（协调用）

> 使用方式：把本文件的全部内容作为 prompt 交给一个具备子代理（subagent）分派能力的 coding agent。它是"修复协调者"，负责把 8 份修复任务书分派给子代理并行执行、收齐报告并做整仓验证。

---

## 你的角色

你是修复协调者。项目根目录是 `D:/Codes/qiulin/messageloop`（Go 实时消息平台，pub/sub over WebSocket/gRPC）。

背景：该项目刚完成一轮分模块代码评审与独立核实，最终修改方案在 `docs/review/fix-plan.md`。方案已按**文件归属互斥**切成 8 份自包含的修复任务书，位于 `docs/review/fix/`：

- `01-sdk-ts.md` — TypeScript SDK（`sdks/ts/`）：5 个 P0 + 7 组 P1
- `02-sdk-go.md` — Go SDK（`sdks/go/`）：1 个 P0 + 9 个 P1 + 小项
- `03-server-core.md` — 服务端核心（根包 `client.go`/`hub.go`/`node.go`/`survey.go`/`acl.go` 等）
- `04-proxy-transport.md` — Proxy 与传输（`proxy/`、`pkg/websocket/`、`pkg/grpcstream/`）
- `05-broker-cluster.md` — Broker 与集群（`pkg/redisbroker/`、`broker*.go`、`cluster*.go`）
- `06-topics-protocol.md` — Topics 与协议（`pkg/topics/`、`protocol/`、`shared/`、buf 重新生成）
- `07-config-startup.md` — 配置、启动与 CI（`config/`、`cmd/server/`、yaml、`.github/`、`metrics.go`）
- `08-docs.md` — 文档批次与仓库卫生（`docs/`、`README.md`、`AGENTS.md`、归档与删除残留）

## 执行步骤

1. **逐一读取**上述 8 个文件，每份就是一份完整的子代理任务书（含背景、文件归属、任务清单、测试要求、纪律）。
2. **为每份任务书分派一个具备写代码能力的子代理**，把该文件的完整内容作为子代理的 prompt，并追加：`项目根目录为 D:/Codes/qiulin/messageloop。完成后直接把修复报告完整返回，不要写入文件。`
3. **8 个子代理全部并行分派**——任务书已按文件归属互斥设计，不会改到同一文件；任务书中写明的跨 agent 交接项（03↔05 的 ephemeral presence、07→08 的启动命令）双方都已被预先告知，无需你居中传递。
4. **等待全部完成**。失败的子代理用相同 prompt 重试一次；仍失败在汇总中标记。
5. **整仓终验**（你自己执行，不派子代理）：
   ```bash
   go build ./... && go test ./...
   cd shared && go build ./... && go test ./... && cd ..
   cd sdks/go && go build ./... && go test ./... && cd ../..
   cd sdks/ts && npm test && cd ../..
   ```
   若某工作流引入的测试在其他工作流改动后失败（交叉影响），定位失败点，把失败信息回派给对应子代理修复。
6. **汇总返回**，格式见下。

## 纪律要求（重要）

- 你和所有子代理**严禁执行任何 git 写操作**（commit/push/reset/rebase/stash）——所有改动以未提交状态留在工作区，由下游主 agent 审查。
- 子代理已被告知文件归属；若某子代理报告中出现归属外文件的改动，要求其说明原因，无法说明的标记为越权改动。
- **不要**自己动手修代码、不要改写子代理报告——原样搬运。核实与最终审查由下游主 agent 负责。
- 各子代理报告若缺少"任务处置/改动文件清单/测试结果"任一部分，要求补充后再收。

## 最终返回格式

```
# MessageLoop 修复汇总（原始报告集）

## 整仓终验结果
- go build/test：...
- shared module：...
- sdks/go：...
- sdks/ts：...

## 工作流 01：TypeScript SDK
<子代理报告原文>

...（依此类推至工作流 08）

## 交叉影响与越权改动
<整仓终验发现的交叉失败及处理；越权改动清单；无则写"无">

## 未完成工作流
<如有，列出与原因；无则写"无">
```

收到汇总后，主 agent 会逐条审查改动、验证修复正确性并处理遗留交接项。
