# PR-05 实现规格：服务端 ping + 可配秒级 idle

| 字段 | 值 |
| --- | --- |
| 标题 | `server: optional server-initiated ping and documented second-scale idle timeout` |
| 状态 | **Accepted**（2026-08-14 主 agent 终验通过，尚未 commit） |
| 依赖 | **PR-01 已合**（Outbound `Ping=17`、Inbound `Pong=14`）。不依赖 04b |
| 设计来源 | [v1.0-platform-gaps.md](../v1.0-platform-gaps.md) 缺口 4、KD-14 |
| 验收人 | 主 agent |

## 1. 目标

服务端能主动发 `Ping`、用独立的 `ping_timeout` 探半开连接（策略 B：到期即 3511，不等 `idle_timeout`）。`idle_timeout` 允许秒级（≥1s）。**默认仍是 idle=300s、不主动 ping**，旧客户端 + 默认配置不断连。

`handlePong` 必须走与 `handlePing` 相同的 10s 节流 `refreshPresence` + `syncClusterSessionState`，否则只回 Pong 的新 SDK 会让 Redis lease / presence TTL 过期。

集群 session lease 按公式随 idle/ping **缩短**，默认路径仍算出 600s。

本 PR **不**实现：Go/TS SDK 处理 Outbound Ping（PR-08）、改默认 idle、Survey、按 user。

## 2. 允许改动的文件

- `config/config.go`、`config/config_test.go`：`heartbeat.ping_interval` / `ping_timeout`；`Validate()` 非 0 则 ≥1s；idle 过短只 Warn
- `config-example.yaml`
- `heartbeat.go`、必要时 `heartbeat_test.go`（新）：双 ticker + 每次 ping 武装 `pingDeadline`
- `node.go`：解析三字段进 `HeartbeatConfig`；`GetHeartbeatConfig()`；`sessionLeaseTTL()`。**不要改 `NewNode` 签名**
- `client.go`：`handleMessage` 增加 `Pong`；`HandleMessage` 入站停 `pingDeadline`；`handlePong`（与 `handlePing` 同节流刷新）；`Client` 加 `pingDeadline` timer
- `cluster_state.go`、`cluster_resume.go`：所有 `defaultClusterSessionLeaseTTL` **写入**改为 `n.sessionLeaseTTL()`（常量可留作「idle=300s 且 ping=0」的期望值）
- `pkg/websocket/handler.go`：读超时按 §6 公式（需要 `GetHeartbeatConfig`）
- `metrics.go`、`metrics_test.go`：`heartbeat_idle_disconnects_total`（3511 时 +1）
- `docs/developer/02-configuration.md`、`docs/protocol.md`（Ping/Pong 双向）、必要时 `docs/developer/04-cluster.md` lease 一句
- 测试：`heartbeat_test.go`、必要时 `client_test.go` / `node_test.go` / `pkg/websocket/handler_test.go`
- `docs/design/tasks/pr-05-heartbeat.md`（完成备注）

禁止：改 proto、改 SDK、改默认 idle=300s、把 `ping_interval` 默认成非 0、git 写操作。不要改 `handlePublish` / `handleSurvey` / presence writer。

## 3. 现状（动手前再读）

- `Heartbeat.IdleTimeout` 空 → 300s；`"0s"` 禁用踢人（`node.go:89-102`）。
- `heartbeatLoop` 只有 `idleTicker`（`heartbeat.go:38-59`）。idle=5s 时最坏约 10s 才查一次。
- 服务端**从不**发 Ping。活动刷新：任意入站改 `lastActivity`（`client.go:310`）；客户端 `Ping` 再 `ResetActivity` + 节流 refresh（`handlePing` 1339–1356）。
- **没有** `handlePong`。Inbound `Pong=14` 会掉进 `default` → `DisconnectBadRequest`。
- Outbound `Ping=17` / Inbound `Pong=14` proto 已冻，不要改号。
- 集群 lease **写死** 600s（`cluster_state.go:20`，`PutSessionLease` / resume CAS）。
- WS 读超时：初值 60s；`idle>0` 则 **覆盖为** `2*idle`；配置了再硬覆盖（`pkg/websocket/handler.go:70-77`）。
- gRPC 流无读 deadline，只靠心跳。
- `TestNewNode_HeartbeatDefaultIdleTimeout`、`TestClient_HandlePing_ThrottlesClusterRefresh` 必须留下。

## 4. 配置

```yaml
server:
  heartbeat:
    idle_timeout: "300s"    # 已有。空/解析失败=300s；"0s"=不因 idle 踢人
    ping_interval: "0s"     # 新增。0/空 = 不主动 ping（旧行为）
    ping_timeout: "3s"      # 新增。仅 ping_interval>0 时有意义
```

```go
type Heartbeat struct {
    IdleTimeout  string `yaml:"idle_timeout"`
    PingInterval string `yaml:"ping_interval"`
    PingTimeout  string `yaml:"ping_timeout"`
}
```

`Validate()`：

- 三个字段若非空必须能 `ParseDuration`。
- 解析后非 0 的 `idle_timeout` / `ping_interval` / `ping_timeout` 必须 **≥1s**，否则 error。
- `ping_interval>0` 且 `ping_timeout` 空：按 `ping_interval` 作为 timeout（实现时在 `NewNode` 填，不必 Validate 失败）。
- `ping_interval>0` 且显式 `ping_timeout=0s`：error。
- 若 `idle>0` 且 `idle < ping_interval+ping_timeout`：Warn，不硬拒。

`NewNode` 解析进：

```go
type HeartbeatConfig struct {
    IdleTimeout  time.Duration
    PingInterval time.Duration
    PingTimeout  time.Duration
}
```

空 idle → `DefaultHeartbeatIdleTimeout`（与今日相同）。空 ping_interval → 0。

## 5. 调度（策略 B，必须按此）

`HeartbeatManager.Start`：`IdleTimeout==0 && PingInterval==0` 才直接 return。只禁 idle、开了 ping 也要跑。

`heartbeatLoop`：

1. `idleTicker`：仅 `IdleTimeout>0`，周期 = idle。
2. `pingTicker`：仅 `PingInterval>0`，周期 = `PingInterval`（允许每连接 `0.8~1.2` jitter，避免齐射）。**第一次 ping 在一个 interval 之后**，不要 connect 立刻打。
3. `pingTicker` 到期：发 Outbound `Ping`（新 `id`），然后 **`time.AfterFunc(PingTimeout)` 武装一次性 `pingDeadline`**。已有未停掉的 deadline 先 `Stop`。
4. `pingDeadline` 到期且仍未应答：`close(DisconnectIdleTimeout)`（3511），`heartbeat_idle_disconnects_total` +1。**不等**下一个 ping/idle tick。
5. `idleTicker` 到期且 `now-lastActivity > IdleTimeout`：同样 3511 + 指标。
6. **任意入站**（`HandleMessage` 开头，含 Ping/Pong/业务）：`lastActivity=now`（已有）且 **`Stop` 当前 `pingDeadline`**。Pong 不是唯一解药。

`handlePong`：

- 不要再回 Pong。
- 必须调用与 `handlePing` **同一段**节流 refresh（抽 helper，禁止复制走样）：`refreshPresence` + `syncClusterSessionState`，间隔 `pingClusterRefreshInterval`。

旧客户端忽略 Outbound Ping、仍每 30s 发 Inbound Ping：`ping_interval=0` 时与今日相同。运维打开 `ping_interval` 却不升 SDK → 会被 `ping_timeout` 踢掉，文档写明。

## 6. WebSocket 读超时

```
if idle == 0 && ping_interval == 0:
    readTimeout = 60s
    if configured > 0:
        readTimeout = configured          // 与今日最后一步相同
else:
    floor = max(2*idle if idle>0 else 0,
                3*ping_interval if ping>0 else 0,
                10s)
    readTimeout = floor
    if configured > 0:
        readTimeout = max(configured, floor)   // 配置不得小于探测窗口
```

`idle=0 && ping=0` **禁止**套 10s 地板（否则禁用心跳的连接 10s 被读超时踢掉，违背验收 4）。

gRPC 不要加读 deadline。

## 7. 集群 lease

```
func (n *Node) sessionLeaseTTL() time.Duration {
    idle, ping := ...
    if idle == 0 && ping == 0 {
        return defaultClusterSessionLeaseTTL // 600s，禁用心跳时不要改短
    }
    return max(30s, 2*idle, 3*ping, idle+pingClusterRefreshInterval+10s)
}
```

默认 idle=300s、ping=0 → `max(30, 600, 0, 300+10+10)=600s`。

idle=15s、ping=5s → `max(30, 30, 15, 35)=35s`。

替换这些写入点的 TTL 参数（以及 `ExpiresAt = now+TTL`）：

- `cluster_state.go` `syncClusterSessionState` 的 `PutSessionLease`
- `cluster_state.go` 另一处 `ExpiresAt`（约 294）
- `cluster_resume.go` CAS / `ExpiresAt`（约 70–72）

常量 `defaultClusterSessionLeaseTTL` 留下当「默认配置的期望值」。

## 8. 指标

`messageloop_heartbeat_idle_disconnects_total` Counter。每次因 idle 或 pingDeadline 走 3511 时 +1。`NewMetrics` 注册。冒烟测试。

## 9. 必须存在的测试

| 测试 | 断言 |
| --- | --- |
| `TestHeartbeat_IdleTimeoutDisconnects` | `idle=5s`，客户端不发帧 → 约 5s+ 后 close code=3511，指标 +1 |
| `TestHeartbeat_PingTimeoutFiresBeforeIdle` | `ping_interval=2s, ping_timeout=1s, idle=5s`，客户端不回 Pong（吞掉 Outbound Ping）→ **Ping 后约 1s** 即 3511，不必等到 idle=5s；指标 +1 |
| `TestHeartbeat_PingDeadlineNotWaitNextTick` | 同上：deadline 在 timeout 触发，不是等下一个 2s tick（可用 fake clock 或量时间：断开应 < 2s+interval 余量，且 ≥ timeout） |
| `TestHeartbeat_DefaultNoServerPing` | 默认配置：不发 Outbound Ping；`TestNewNode_HeartbeatDefaultIdleTimeout` 仍绿 |
| `TestHeartbeat_IdleAndPingDisabledKeepsConnection` | `idle=0s` 且 `ping_interval=0`，不配 read_timeout。静默 ≥2s（单测不必真等 15s）不断连；`GetHeartbeatIdleTimeout()==0` |
| `TestHeartbeat_PongRefreshesPresenceAndLease` | `ping_interval>0`，客户端只回 Pong。`handlePong` 触发与 `handlePing` 相同的节流 refresh（可用计数 fake presence / directory）。第二次 Pong 在 10s 窗口内不重复 |
| `TestHeartbeat_AnyInboundCancelsPingDeadline` | 发出 Ping 后客户端发 Publish/业务帧（不是 Pong）→ deadline 取消，不被 3511 |
| `TestHeartbeat_SessionLeaseTTLFormula` | 默认 → 600s；idle=15s ping=5s → 35s；idle=0 ping=0 → 600s |
| `TestHeartbeat_ValidateRejectsSubSecond` | `idle_timeout=500ms` 或 `ping_interval=200ms` → `Validate` error |
| `TestHeartbeat_ReadTimeoutFloorWhenProbing` | 单测公式或 handler：idle=15s ping=5s、未配 read_timeout → ≥ max(30s, 15s, 10s)=30s；idle=0 ping=0 未配 → 60s |

现有 `TestClient_HandlePing_ThrottlesClusterRefresh`、`TestClientSession_HandleMessage_Ping` 必须绿。

## 10. 文档

`02-configuration.md` heartbeat 表：

- `idle_timeout`：允许秒级，≥1s 或 0；默认 300s
- `ping_interval` / `ping_timeout`：默认关；打开后旧客户端必须升级，否则 `ping_timeout` 踢人
- lease 公式与「默认仍 600s」

`protocol.md`：Ping/Pong 双向；任一方收到 Ping 立即回 Pong（同 id）；入站刷新活动；服务端 Pong 续 lease/presence。

## 11. 验收清单

1. 默认：idle=300s，无服务端 Ping，旧路径不断连。
2. `idle=5s` 静默 → 3511。
3. 未应答服务端 Ping → `ping_timeout` 内 3511，不等 idle。
4. `idle=0` 且 `ping=0`：心跳不踢；WS 读超时仍 60s（未配时）。
5. `handlePong` 与 `handlePing` 共用节流 refresh。
6. lease 默认 600s，短 idle 可短于 600s；禁用心跳不改短 lease。
7. 无 proto 变更；`NewNode` 签名不变。
8. `go test -count=1 . ./config/... ./pkg/websocket/...` 与 `go test -race -count=1 .` 绿。

## 12. 完成报告

- 文件列表
- `heartbeatLoop` / `handlePong` / `sessionLeaseTTL` / WS 读超时 文件:行
- §9 每个测试：过/失败
- §11 八条 + 证据
- 偏离与理由

## 13. 实现备注（落地后填写）

（实现者补 2–6 条。）

1. **调度**：`heartbeatLoop` 用「idleTicker + 可 Reset 的 pingTimer」而非双 Ticker——ping 需要 0.8~1.2 抖动，Reset 前先重新计算 jitter。抖动函数挂在 `HeartbeatManager.jitter` 字段（非包级变量），测试可在 `Start` 之前钉死为恒等函数，避免并发写读竞态。
2. **pingDeadline 防御**：`armPingDeadline` 的回调里用「指针比较 + status 检查」二重守卫——新 ping 替换旧 deadline、任意入站 Stop 后旧回调不得触发；`disconnectHeartbeatTimeout` 再用 `atomic.Bool` CAS 保证 pingDeadline 与 idleTicker 同时命中时 metric 只 +1。`close()` 不依赖 status 检查之外的清理，closeQuiet/takeover 路径天然安全。
3. **读超时公式**：抽 `heartbeatReadTimeout(idle, ping, configured)` 纯函数（`pkg/websocket/handler.go`），§6 的「idle=0 && ping=0 禁止 10s 地板」由单测 `TestHeartbeat_ReadTimeoutFloorWhenProbing` 钉死。
4. **已知既有失败**：`TestWebSocket_MultiClientBroadcast` 在 PR-04a（dc3fede）后就已损坏——订阅者读到的第一个帧是 presence `join` 事件而非 publication（本 PR 基线验证：pristine main 同样失败）。不在 PR-05 改动清单内，未修。
5. **lease 公式**：`sessionLeaseTTL()` 读 `HeartbeatConfig`；默认（300s/0）→ 600s，与 `defaultClusterSessionLeaseTTL` 一致；idle=15s/ping=5s → 35s。
6. **指标**：`messageloop_heartbeat_idle_disconnects_total` 由 `disconnectHeartbeatTimeout` 统一 +1（idle 与 pingDeadline 两条路径共用），冒烟测试在 `metrics_test.go`。
