# ADR-0008：手工同步表必须 census 测试钉住

- 状态：已接受（2026-10-04，落地于 commit `2ab08d9`）
- 关联：D2 / D3、G1 / G6（admin-api-key-authz 的 census 哲学）、[CONTEXT.md §六](../../CONTEXT.md)

## 语境

编译器不管的手工同步结构是静默漂移的温床。本仓现存三类：capability 名字双表（`authz.ClosedCapabilityNames` ↔ `config.CapabilityNames`，受 config→proxy 依赖方向所迫无法单源，见 ADR-0005 关联的例外边）、`proxy.UserInfo` ↔ proxypb ↔ sdks/go 三方镜像、serverapi 的 rpcScope 注册表（8 RPC × proto 字段 × 能力位）。

## 决策

1. **凡新增「两份以上必须同步的结构」（镜像 struct、名字表、字段登记表），同一 PR 落 census 测试**：断言键集/字段集相等（反射或逐字段），把注释里的「手工同步义务」升格为红线。
2. census 测试落在**能同时看见两表**的最小位置；跨 module（sdks/go）在模块内自钉，且遵守该模块的依赖纪律（SDK 侧纯 stdlib）。
3. 结构性单源化仍然优先——census 是补偿不是消除；单源化的障碍（依赖方向）变化时应回头消表。

## 后果

- 现行三条 census：`TestCapabilityNameCensus`（authz 包）、UserInfo 镜像 census（proxy 与 sdks/go 各一）、rpcScope 三重 census（serverapi，含 stale 表项拒绝）。
- 加 proto 字段/能力名/镜像字段而 census 红，是**预期行为**不是测试误报——补齐同步而不是绕过断言。
