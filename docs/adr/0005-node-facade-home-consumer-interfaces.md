# ADR-0005：Node 是 facade home——不拆包；消费侧窄接口留在 internal/runtime

- 状态：已接受（2026-10-04，落地于 commit `2ab08d9`）
- 关联：KD-K26、D16 / D17 / D18、[CONTEXT.md §三](../../CONTEXT.md)

## 语境

internal/runtime（生产 ~4,900 行）几乎每次评审都会收到两条建议：「拆成多个包」与「给 serverapi / transport 各命名一个小 interface」。前者损 locality：session 面、Server API 面、集群、recover 共享大量私有状态与测试装置，包级拆分把一处改动摊到多包。后者的动机成立（调用方不该看见 38+ 方法），落法需要裁定。

## 决策

1. **internal/runtime 是 facade home（KD-K26），不做包级拆分**；文件级组织（node / recover / cluster_state / serverapi_runtime / …）已足够。
2. **消费侧窄接口留在 internal/runtime 包内命名**：`ServerAPIRuntime`（18 方法，serverapi handler 的真实调用切片），`*Node` 唯一生产 adapter + 编译期断言；不迁移包、不建新包。
3. **transport 对 Node 的依赖保持构造函数形态**（`runtime.NewClient` + `MaxMessageSize` + `GetHeartbeatConfig`）——构造函数已是那个 seam，命名 3 方法小 interface 是仪式（D17）。
4. `session.Runtime`（38 方法）保持现状，不趁机重构；需要窄面时优先：叶子包纯函数（如 survey.ClampTimeout）> Node 方法 > 扩 Runtime。

## 后果

- serverapi 测试用真 `runtime.NewNode`（matrix 从真实 Server API 面进入是测试纪律）；`ServerAPIRuntime` 无测试替身，替身需求真实出现时再议。
- 本 ADR 不冻结 Runtime 的宽度问题本身——往 Runtime 加方法的 PR 仍在加宽全仓最宽的 interface，先过决策 4 的优先级。
