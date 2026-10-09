# ADR-0001：传输 seam 保持四方法窄面，错误分类归 adapter

- 状态：已接受（2026-10-04，落地于 commit `2ab08d9`；2026-10-05 修订决策 2/4：quic 补包装，kcp 理由升格）
- 关联：D11（批 3/批 4 落地）、GLOSSARY「Attachment」、[CONTEXT.md §二/§四](../../CONTEXT.md)

## 语境

ws / grpc / quic / kcp 四个传输 adapter 满足同一个 `session.Transport` seam（4 方法：Write / WriteMany / Close / RemoteAddr）——4 个生产 adapter，seam 真实。曾经的问题：对端关闭的错误形状各 adapter 不同（gorilla 的 close 1000/1001、gRPC 的 Canceled/Unavailable、quic-go/kcp-go 的私有错误），`internal/session/session.go` 反向 import gorilla/websocket 与 grpc status/codes，用 `isPeerClosedError` 嗅探各家的错误形状——seam 缺错误分类能力，Session 替 adapter 收拾，每接一种新传输就要改内核。

## 决策

1. `session.Transport` 保持 4 方法窄面，不为错误分类加方法（D13 同期裁定不加显式 Ping 能力）。
2. 错误分类是 **adapter 的义务**：ws 对 close 1000/1001、grpc 对 Canceled/Unavailable、quic 对**对端发来的 CONNECTION_CLOSE**（写侧形状为 `*quic.ApplicationError` 且 `Remote=true`，2026-10-05 补齐），以 `errors.Join(session.ErrPeerGone, err)` 包装（`errors.Is` 双链可判）。
3. session 侧判定缩为三判：`io.EOF` / `net.ErrClosed` / `session.ErrPeerGone`（transport.go 定义 sentinel）。
4. **kcp 不包装，且没有可包装的东西**：底层 UDP 往消失的对端写不会失败——写路径不存在对端关闭形状；TLS 截断记录只在读侧出现且读循环已优雅退出；kcp 真正的死端信号是读超时（handler 既有注释明示这是心跳域补偿，因 KCP 无传输层 keepalive）。死端检测归读超时/心跳域，是所有权清晰，不是缺失。quic 侧排除项：`Remote=false`（本端关闭，非对端事件）；空闲超时不包（装配刻意设 `MaxIdleTimeout = max(2×idle, 5min)` 让应用层 3511 先触发）；`quic.StreamError` 不包（单流协议不产生 reset，出现即异常，维持 3512 可见性）。

## 后果

- session 删除对 gorilla/websocket 与 grpc status/codes 的反向 import；新传输只需自己包好 ErrPeerGone。
- 分类纪律以表驱动单测钉住（2026-10-05 补齐——此前 ws/grpc 两份也零单测）：ws / grpc / quic 三份 `wrapPeerGone` 各有形状表（正例、nil、非目标形状透传、`errors.Is` 双链、原形状存活）；kcp 无形状无表。
- kcp 的「不对称」是有理由的设计而非债务；若未来出现真实的对端关闭写形状（如更换传输层实现），按决策 2 的模式补 `wrapPeerGone` + 形状表。
