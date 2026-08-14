# 设计文档

本目录存放尚未（或刚刚）落入实现的产品/协议设计。已落地的行为以源码和 [开发者文档](../developer/README.md) 为准。

| 文档 | 状态 | 说明 |
| --- | --- | --- |
| [v1.0 功能缺口设计](v1.0-platform-gaps.md) | Approved | Subscribe 恢复、Presence、按 user、心跳、客户端 Survey、通配 presence、频道策略；含 Key Decisions、排期、PR Plan |
| [产品 ROADMAP](../../ROADMAP.md) | Approved | v0.2 → v1.0 → v1.x 能力地图与排期摘要 |
| [PR-01 实现规格](tasks/pr-01-protocol.md) | Accepted | 冻结 v1.0 proto 字段号；[第三方 prompt](tasks/pr-01-prompt.md) |
| [PR-02 实现规格](tasks/pr-02-channel-policy.md) | Accepted | 频道前缀策略引擎；[第三方 prompt](tasks/pr-02-prompt.md) |
| [PR-03 实现规格](tasks/pr-03-recover.md) | Accepted | Subscribe/Connect 共用恢复；[第三方 prompt](tasks/pr-03-prompt.md) |
| [PR-04a 实现规格](tasks/pr-04a-presence.md) | Accepted | Presence 本节点一等事件（不 emit）；[第三方 prompt](tasks/pr-04a-prompt.md) |
| [PR-04b 实现规格](tasks/pr-04b-presence-emit.md) | Accepted | Presence `cluster_emit` 门闩；[第三方 prompt](tasks/pr-04b-prompt.md) |
| [PR-05 实现规格](tasks/pr-05-heartbeat.md) | Accepted | 服务端 ping + 秒级 idle；[第三方 prompt](tasks/pr-05-prompt.md) |
| [PR-06 实现规格](tasks/pr-06-admin-user.md) | Accepted | Admin 按 user 投递/断开/订阅；[第三方 prompt](tasks/pr-06-prompt.md) |
| [PR-07 实现规格](tasks/pr-07-survey.md) | Accepted | 客户端发起 Survey；[第三方 prompt](tasks/pr-07-prompt.md) |
| [PR-08 实现规格](tasks/pr-08-sdk-go.md) | Accepted | Go SDK v1.0 API；[第三方 prompt](tasks/pr-08-prompt.md) |
| [PR-09 实现规格](tasks/pr-09-sdk-ts.md) | Accepted | TypeScript SDK v1.0 API；[第三方 prompt](tasks/pr-09-prompt.md) |
| [PR-10 实现规格](tasks/pr-10-docs.md) | Accepted | 文档对齐 + 集群 e2e；[第三方 prompt](tasks/pr-10-prompt.md) |

历史设计（已归档实现记录）见 [docs/archive](../archive/) 与 [docs/superpowers/specs](../superpowers/specs/)。
