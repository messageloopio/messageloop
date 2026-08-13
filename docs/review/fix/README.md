# MessageLoop 修复任务书

本目录包含按 `docs/review/fix-plan.md` 切分的 8 份修复任务书。每份自包含，可直接分派给具备写代码能力的 coding agent 执行。

## 执行方式

**方式 A（推荐）：总控接力。** 把 `00-orchestrator.md` 的完整内容交给一个具备子代理分派能力的 coding agent，它会并行分派 8 个子代理执行修复、做整仓终验并收齐报告。

**方式 B：手动分派。** 自行把各任务书分派给不同 coding agent。可全部并行——文件归属已按互斥设计：

| 文件 | 工作流 | 拥有的文件 |
|---|---|---|
| `01-sdk-ts.md` | TypeScript SDK | `sdks/ts/` |
| `02-sdk-go.md` | Go SDK | `sdks/go/` |
| `03-server-core.md` | 服务端核心 | 根包 `client.go`/`hub.go`/`node.go`/`survey.go`/`acl.go` 等 |
| `04-proxy-transport.md` | Proxy 与传输 | `proxy/`、`pkg/websocket/`、`pkg/grpcstream/` |
| `05-broker-cluster.md` | Broker 与集群 | `pkg/redisbroker/`、`broker*.go`、`cluster*.go` |
| `06-topics-protocol.md` | Topics 与协议 | `pkg/topics/`、`protocol/`、`shared/` |
| `07-config-startup.md` | 配置与启动 | `config/`、`cmd/server/`、yaml、`.github/`、`metrics.go` |
| `08-docs.md` | 文档与卫生 | `docs/`、`README.md`、`AGENTS.md`、归档删除 |

## 共同约定

- 所有 agent **禁止 git 写操作**，改动以未提交状态留在工作区供主 agent 审查。
- 每份任务书含测试要求：修复前确认基线、P0/P1 配回归测试、完成后相关包全绿。
- 跨工作流交接项（03↔05、07→08、04→根包、06→全仓）已在任务书中预先写明，由主 agent 最终核实。
- 全部完成后，把汇总报告交回主 agent 做逐条审查与最终验证。

## 第 2 轮：继续修复（深度验收后）

深度验收判定"需补充修复后放行"，以下 followup 任务书分派给**对应的原实现 agent**（可并行，文件归属不变）：

| 文件 | 原工作流 | 内容 |
|---|---|---|
| `followup-01-sdk-ts.md` | 01 TS SDK | 必修×2（重删旧 proto 残留、订阅回写去条件）+ 建议×2 |
| `followup-02-sdk-go.md` | 02 Go SDK | 建议×3（connectErrCh 误判、示例 nil 防护、迁移文档） |
| `followup-03-server-core.md` | 03 服务端核心 | 建议×5（ACL 注释、SubRefresh 限流、超限回滚、A2 测试、行尾） |
| `followup-05-broker-cluster.md` | 05 Broker/集群 | 必修×1（驱逐回滚保留 ephemeral 标志） |
| `followup-07-config-startup.md` | 07 配置/启动 | 建议×1（测试文件行尾 CRLF） |
| `followup-08-docs.md` | 08 文档 | 必修×3（3513 断连码、可观测性、Validate 规则）+ 建议×2 |

无 followup 的工作流（04 Proxy/传输、06 Topics/协议）验收全项放行，无需改动。

**待决策项（不在本轮修复）**：精确频道（不含 `*`）不经 matcher，`"a."` 这类空分段频道仍可订阅成功，与 wildcard 空分段拒绝语义不一致——需产品决策是否在所有订阅入口统一校验，决策后另行排期。

全部 followup 完成后，交回主 agent 做最终复核，然后即可提交。
