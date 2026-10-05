# ADR — 架构决策记录

已裁决的架构决策。**评审与重构前先查这里**：与某条 ADR 冲突的候选，除非摩擦真实到值得重开（需在候选卡上明示「contradicts ADR-00XX, worth reopening because …」），否则不再重复裁决。

## 层级关系

- 上位：`docs/v2/kernel-architecture.md`（KD-\* 宪法）> ADR > [CONTEXT.md](../../CONTEXT.md)（地图/现状）
- 来源：多数 ADR 源自 [2026-10-04 架构评审与深化裁决](../design/2026-10-04-architecture-review-decisions.md)（D1-D21，含逐批实现还原点）；ADR 只留决策与不变量，过程性记录在裁决文档。
- 命名：新概念先查 [GLOSSARY.md](../../GLOSSARY.md)。

## 格式

每份 ADR：状态（已接受/已废弃/被 ADR-00XX 取代）、关联（KD-\*/D\*/GLOSSARY 词条）、语境、决策、后果。短；细节回链接。

## 索引

| ADR | 决策 | 源 |
| --- | --- | --- |
| [ADR-0001](0001-transport-seam-error-classification.md) | session.Transport 四方法窄面；对端关闭错误分类归 adapter（ErrPeerGone sentinel） | D11 |
| [ADR-0002](0002-broker-epoch-first-class.md) | `Epoch()` 是 stream.Broker 接口一等能力，禁止 type assertion | D5 |
| [ADR-0003](0003-position-authority-single-construction.md) | Position 游标权威；构造单点 `shared.PositionFrom` | KD-K11/K22 + D6 |
| [ADR-0004](0004-takeover-single-activation.md) | single-activation 仲裁归属 internal/session Takeover 模块（三重身份比较器） | D14 |
| [ADR-0005](0005-node-facade-home-consumer-interfaces.md) | Node 是 facade home 不拆包；消费侧窄接口留在 internal/runtime | KD-K26 + D16/D17/D18 |
| [ADR-0006](0006-single-matcher-adapter.md) | Matcher 单实现（CSTrie）；一个 adapter = 假想 seam，不复活比较族 | D20 |
| [ADR-0007](0007-dual-plane-contract-sinking.md) | 双平面共用契约下沉单点；handler 层保留（通过 deletion test） | D8 |
| [ADR-0008](0008-census-discipline.md) | 手工同步表必须 census 测试钉住 | D2/D3 + G1/G6 |
| [ADR-0009](0009-framing-kit.md) | framing kit 收编写路径与 DISCONNECT 信封；grpc 不入 Writer、ws 不用 | D9 |
