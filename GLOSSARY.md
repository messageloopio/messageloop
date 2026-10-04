# GLOSSARY

本仓的 load-bearing 域词。评审与设计文档（如
[架构评审与深化裁决](docs/design/2026-10-04-architecture-review-decisions.md)）按此命名；
代码重构起名前先查这里，概念不在册就先补这里。

## Session

可恢复的逻辑连接（KD-K2）：状态机 Authenticating → Attached → Detached → Closed。
Hub 在本节点生命周期内持有 Session 指针；本地 resume 只换 Attachment，绝不换 Session 对象。
`Client` 是 `Session` 的过渡别名。**Avoid**: "connection"（指传输层时用 transport）。

## Attachment

一次传输绑定：Transport + Marshaler + 协议名（ws/grpc/quic/kcp）。它不拥有订阅、
fencing 或 Occupancy——那些属于 Session。Attach 换绑、Detach 撕离。

## delegate / shell（本地 resume 后的身份）

本地 resume 后，临时的 Authenticating 连接对象变成**只读循环壳（shell）**，
其读写与关闭请求经 `delegate` 指针路由到被恢复的 Session（后者骑着交接来的
transport 继续服务）。裁决规则见 `internal/session/takeover.go`（Takeover 模块）。

## Takeover

本地 resume 的交接执行与身份仲裁模块（`internal/session/takeover.go`）：
谁在何时可以关掉谁。三重身份比较器——① delegate 路由（壳不自行其是）；
② attachment 指针身份（被替换的 attachment 属于已脱离连接，读循环死掉不得
拆会话）；③ transport 身份（被恢复会话的新 attachment 是包着交接 transport 的
新对象，链式 resume 重绑后旧壳失语）。single-activation 硬不变量在此归属。

## Position

恢复契约的游标权威（KD-K11/KD-K22）：`{stream_epoch, offset?}`。
"从头"只经显式 `fresh=true` 或 StreamEpoch 重置，绝不 offset==0；
offset 可以不设（transient/fresh/unknown），由 `shared.PositionFrom` 单点构造。
关联：RecoverState、DeliveredOffset、BrokerEpoch（集群快照侧）。

## 其余高频词（既有约定，此处只登记）

- **Hub / Subscriber**：分片本地注册表；Subscriber 记录携带 `DeliveredOffset`（服务端记录的投递位点）与 `Ephemeral`。
- **Node / Principal / scope / capability**：运行时协调者与 Server API 鉴权三元组（见 admin-api-key-authz 设计 §2.4）。
- **Lease**：`ClusterSessionLease`（会话所有权，fencing/CAS）与 `ClusterNodeLease`（成员）；
  epoch 三义——`StreamEpoch`（日志代）≠ `BrokerEpoch`（快照字段）≠ `node_epoch`/IncarnationID（进程代）。
- **namespace**：`ns:topic` 多租户语法（`pkg/topics`）；proxy 授予的会话 namespace、静态 `server.namespace` 回退、身份的 namespace scope 三者有别。
