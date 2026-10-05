# ADR-0003：Position 游标权威与单点构造

- 状态：已接受（2026-10-04，落地于 commit `2ab08d9`）
- 关联：KD-K11 / KD-K22、D6、D7、GLOSSARY「Position」、事故 7ce8784（TS SDK fresh implies recover）

## 语境

Position 是恢复契约的游标权威：`{stream_epoch, offset?}`。铁律：「从头」只经显式 `fresh=true` 或 StreamEpoch 重置，**绝不 offset==0**；offset 可以不设（transient / fresh / unknown）。曾经两处字节级重复的 `positionFrom`（internal/runtime/recover.go 与 internal/session/runtime.go），且 entry 语义（fresh 仅在 Recover=true 时有意义、缺 offset → skip、epoch 失配 → reset）散落 session 客户端路径——TS SDK "fresh implies recover" 事故（7ce8784）正是契约散落的直接产物。

## 决策

1. Position 构造单点：**`shared.PositionFrom`**（独立 shared module 的 position.go）。选 shared 而非 recover 模块：协议级函数，session 与 runtime 两侧零新增依赖方向。
2. entry 语义裁决归 **recover 模块**（internal/runtime/recover.go）：`recoveryCursor`（fresh→从头；resume 双方 epoch 非空且不等→epochReset；缺 offset→skip；非 resume cursor→offset+1；deliveredOffset 兜底）与 `SnapshotRecoverySubs`（快照独有频道合成 Recover:true）。
3. ack-before-stream 顺序**留在 session**——那是协议帧序，不是 recover 语义，移动即错位。

## 后果

- 5 个生产调用点全经 `shared.PositionFrom`；「从头重放恒空」类事故获得单一归属地。
- 改 Position 语义只动 recover 模块 + shared 单点；任何新的 position 构造点出现即违此 ADR。
