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
| [PR-KA-C1 实现规格](tasks/pr-ka-c1-sim.md) | Ready | 确定性 fencing 模拟（KD-K20）；[prompt](tasks/pr-ka-c1-prompt.md) |

第三方实现：复制对应 `tasks/pr-ka-*-prompt.md` 围栏全文；规格与靶心冲突时以该步规格为准。
