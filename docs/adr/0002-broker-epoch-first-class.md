# ADR-0002：`Epoch()` 是 stream.Broker 接口的一等能力

- 状态：已接受（2026-10-04，落地于 commit `2ab08d9`）
- 关联：D5、KD-K11/KD-K22（Position 契约）、KD-K14（memory ≡ Redis）、GLOSSARY「Position」「Lease」（epoch 三义）

## 语境

`Epoch() string` 曾是匿名接口 `interface{ Epoch() string }`，在 `recover.go`、`cluster_state.go`、`serverapi/api_handler.go` 三处 type assert。后果：一个忘实现 epoch 的 Broker 替身让 **epoch 失配门静默放行**——恢复语义的静默失效 bug 类。三个 producer（memory / redis / 测试替身）皆可实现该能力，后门没有存在理由。

## 决策

1. `Epoch() string` 上 `stream.Broker` 接口本体（现 10 方法）；匿名可选接口断言全删。
2. 契约注释钉在接口上：epoch 为 `""` 的 position 按**未校验**处理，而非视为匹配。
3. epoch 三义不得混用：`StreamEpoch`（日志代，broker 的 Epoch()）≠ `BrokerEpoch`（集群快照字段）≠ `node_epoch`/IncarnationID（进程代，只准 Redis INCR 发号）。

## 后果

- 任何 Broker 替身缺 epoch 即编译失败（当年 14 个测试替身补桩）；失配门不再可能静默放行。
- KD-K14 的 memory ≡ Redis 等价性断言有了单一落点（接口本体）。
- serverapi 取 epoch 走 `Node.StreamEpoch()` 委托，不再本地重实现。
