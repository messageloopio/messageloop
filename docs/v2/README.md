# MessageLoop 独立版本（v2）

本目录是 **不向后兼容** 的独立大版本设计与实施规格。旧树的产品/协议合同仍在 [docs/design](../design/README.md)（v1.0）与 [docs/protocol.md](../protocol.md)。

| 文档 | 状态 | 说明 |
| --- | --- | --- |
| [内核架构重设](kernel-architecture.md) | Draft | 靶心：Session/Attachment、四平面、fencing、宪法、KD-K31 |
| [独立评审](kernel-architecture-review.md) | Review | 2026-08-16 两路评审；修订对照见文首 |
| [PR-KA-A0 实现规格](tasks/pr-ka-a0-protocol.md) | Accepted | 冻结 client/server/shared **v2** proto；[prompt](tasks/pr-ka-a0-prompt.md) |
| [PR-KA-A1 实现规格](tasks/pr-ka-a1-fencing.md) | Accepted | 续约 same-fence CAS、删盲写 Put、resume 回滚；[prompt](tasks/pr-ka-a1-prompt.md) |
| [PR-KA-A2 实现规格](tasks/pr-ka-a2-history.md) | Accepted | History gap 页、Publish 成功合同、memory Interest；[prompt](tasks/pr-ka-a2-prompt.md) |
| [PR-KA-A3 实现规格](tasks/pr-ka-a3-livebus.md) | Accepted | Interest 编译、去掉 `PSubscribe *`；[prompt](tasks/pr-ka-a3-prompt.md) |
| [PR-KA-A4 实现规格](tasks/pr-ka-a4-authorizer.md) | Accepted | Authorizer 一张表、语言包含、Capability 闭集；[prompt](tasks/pr-ka-a4-prompt.md) |
| [PR-KA-B1 实现规格](tasks/pr-ka-b1-session.md) | Accepted | Session/Attachment、写队列、状态机；[prompt](tasks/pr-ka-b1-prompt.md) |
| [PR-KA-B2 实现规格](tasks/pr-ka-b2-occupancy.md) | Accepted | Occupancy 只走 LiveBus + OccupancyGen；[prompt](tasks/pr-ka-b2-prompt.md) |
| [PR-KA-B3 实现规格](tasks/pr-ka-b3-recover.md) | Accepted | 流式恢复、client v2 信封、SDK 一条消费路径；[prompt](tasks/pr-ka-b3-prompt.md) |
| [PR-KA-B4 实现规格](tasks/pr-ka-b4-noderpc.md) | Accepted | NodeRPC HMAC、拒绝未签名、repair 合一、范围化 `internal/cluster`；[prompt](tasks/pr-ka-b4-prompt.md) |
| [PR-KA-C1 实现规格](tasks/pr-ka-c1-sim.md) | Accepted | 确定性 fencing 模拟（KD-K20）；[prompt](tasks/pr-ka-c1-prompt.md) |
| [PR-KA-C2 实现规格](tasks/pr-ka-c2-epoch.md) | Accepted | `node_epoch` 只准 INCR，禁止 UUID incarnation；[prompt](tasks/pr-ka-c2-prompt.md) |
| [PR-KA-C3 实现规格](tasks/pr-ka-c3-stream.md) | Accepted | NodeRPC 请求改 Redis Stream + consumer group；[prompt](tasks/pr-ka-c3-prompt.md) |
| [PR-KA-C4 实现规格](tasks/pr-ka-c4-dense-seq.md) | Accepted | History 稠密 seq、真中洞检测（Q8）；[prompt](tasks/pr-ka-c4-prompt.md) |
| [PR-KA-C5 实现规格](tasks/pr-ka-c5-keyprefix.md) | Accepted | Redis 键前缀换代 `ml:` → `ml2:`（KD-K31）；[prompt](tasks/pr-ka-c5-prompt.md) |
| [PR-KA-C6 实现规格](tasks/pr-ka-c6-gap-notice.md) | Accepted | catch-up 洞的 client-facing GapNotice；[prompt](tasks/pr-ka-c6-prompt.md) |
| [PR-KA-D1 实现规格](tasks/pr-ka-d1-graduation-docs.md) | Accepted | 转正收口：文档对齐 v2 + 删死代码；[prompt](tasks/pr-ka-d1-prompt.md) |
| [PR-KA-D2 实现规格](tasks/pr-ka-d2-version-gate.md) | Accepted | 握手版本门：Connect.version 世代校验 + SDK 默认 2.0.0；[prompt](tasks/pr-ka-d2-prompt.md) |
| [PR-KA-D3 实现规格](tasks/pr-ka-d3-observability.md) | Accepted | 观测面补齐：六个合同指标（纯仪表）；[prompt](tasks/pr-ka-d3-prompt.md) |
| [PR-KA-D4 实现规格](tasks/pr-ka-d4-buffer-full.md) | Accepted | LiveBus 缓冲满：occupancy 优先丢 + 频道降级标记；[prompt](tasks/pr-ka-d4-prompt.md) |
| [PR-KA-D5 实现规格](tasks/pr-ka-d5-proxy-v2.md) | Accepted | proxy 升 v2 + 拆 v1 桥 + 删死 v1 proto；[prompt](tasks/pr-ka-d5-prompt.md) |
| [PR-KA-D6 实现规格](tasks/pr-ka-d6-admin-v2.md) | Ready | admin 切 server/v2 + 删 shared/v1 收尾 + backlog 两条；[prompt](tasks/pr-ka-d6-prompt.md) |

第三方实现：复制对应 `tasks/pr-ka-*-prompt.md` 围栏全文；规格与靶心冲突时以该步规格为准。
