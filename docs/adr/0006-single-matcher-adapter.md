# ADR-0006：Matcher 单实现（CSTrie）——一个 adapter = 假想 seam

- 状态：已接受（2026-10-04，落地于 commit `2ab08d9`）
- 关联：D20、KD-K14、GLOSSARY「namespace」

## 语境

`pkg/topics.Matcher` 曾有五个实现：naive / trie / cstrie / inverted_bitmap / optimized_inverted_bitmap。后四者仅测试与基准引用，生产三处（hub、memoryBroker、redisBroker）只用 CSTrie。接口注释自认 duplicate-subscription 语义各实现不同，`Subscription.ID` 字段仅 bitmap 系有意义——一个 adapter = 假想 seam，四个 adapter 是维护税。

## 决策

1. 退役四族（~1,300 行含测试）与跨实现 consistency_test、五族对比基准。
2. `Matcher` 接口契约统一为单一语义：**幂等 per (topic, Subscriber)**；`Subscription.ID` 字段删除。
3. 基准快照留档于裁决文档附二（删前 `BenchmarkPopulate`：CSTrie 235µs/153KB/3784allocs，与 Trie 227µs 同档，无性能回退）。

## 后果

- 生产零改动（本就只用 CSTrie）；`ns:topic` 语法与 lock-free 匹配行为不变。
- 未来要加第二个 Matcher 实现（例如为某部署形态的权衡），必须带着**契约测试**来证明等价，不复活「比较族」——没有第二个 adapter 之前，接口上的任何抽象都是假想 seam。
