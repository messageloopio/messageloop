# ADR-0009：framing kit——写路径与 DISCONNECT 信封单份；grpc 不入 Writer

- 状态：已接受（2026-10-04，落地于 commit `2ab08d9`）
- 关联：D9（批 3 落地）、ADR-0001、[CONTEXT.md §四](../../CONTEXT.md)

## 语境

四个长度前缀传输 adapter 各持两套私有机器：有界写路径（互斥 + 每写 deadline + closed 旗标）与 DISCONNECT_ERROR 信封构造。quic/kcp 的 transport 层近乎 148 行克隆。四个 adapter 满足同一 `session.Transport` seam，seam 本就真实——这是把**实现侧**的重复沉为一份，不新增 seam。

## 决策

1. `pkg/transport/framing` 两件东西：
   - `Writer`：串行帧 + 每写时限 + closed 旗标（`MarkClosed`）+ 断连帧写（1s 上限）；
   - `DisconnectMessage`：DISCONNECT_ERROR 信封的**唯一形状**（数值码放 metadata.disconnect_code，经 `session.MakeOutboundMessage` 构造）。
2. 收编范围是**按模型适配，不是一刀切**：
   - quic / kcp：全量收编（transport.go 收缩至 72-75 行，只剩 conn 类型与关闭语义差异）；
   - grpc：只用 `DisconnectMessage`——其 channel/worker 发送模型与互斥 Writer 不兼容，**有意不入**；
   - ws：不用 kit——gorilla 自带帧协议 + `WriteControl` 原生 close 帧，且 Close 故意不取写锁（防 writerLoop 卡死 Close）。
3. 每 adapter 的 `ErrTransportClosed` 措辞经 `closedErr` 参数保留（测试与日志归因不变）。

## 后果

- adapter 只剩真正的传输差异；新增长度前缀传输（若有）直接复用 Writer。
- 单测已补齐（2026-10-05，8 个用例，此前覆盖全靠 quic/kcp e2e 间接）：closed sentinel 与 MarkClosed 幂等、每帧 deadline 布防/批末清除、DisconnectFrameTimeout 预算上限、**MarkClosed 后断连帧仍可写**的顺序契约（决策 2 的 Close 序列依赖此）、marshal 错误零出线、DisconnectMessage 信封形状、并发帧完整性。
