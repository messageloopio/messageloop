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

历史设计（已归档实现记录）见 [docs/archive](../archive/) 与 [docs/superpowers/specs](../superpowers/specs/)。
