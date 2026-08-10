# 可观测性指南

## 1. 概述

MessageLoop 服务器在进程内提供三类可观测性出口：

- **管理面 HTTP 服务器**：监听 `server.http.addr`（默认为 `127.0.0.1:8080`，为空时回退到该值，见[《配置参考》](configuration.md)的 server 节），只挂载两个端点：
  - `GET /health` —— 健康检查，返回 JSON 状态；
  - `GET /metrics` —— Prometheus 指标，文本格式。
- **结构化日志**：由 lynx 框架输出（`cmd/server/main.go` 中 `app.SetLogger(zap.MustNewLogger(app))`，日志后端为 zap），核心包通过 `github.com/lynx-go/x/log` 写入带上下文的键值对日志。
- **客户端可见的断连码（Disconnect codes）**：连接被服务端关闭时，传输层会把数值代码与原因文本交给客户端（见[《客户端协议参考》](../protocol.md)），是客户端侧可观测性的主要来源。

管理面 HTTP 服务器与面向客户端的监听器（`transport.websocket.addr`、`transport.grpc.addr`）相互独立，不承载任何业务流量。

## 2. 健康检查

端点：`GET /health`（`health.go` 中的 `Node.HealthHandler`，由 `cmd/server/main.go` 挂载）。

响应为 JSON，始终包含三个字段：

| 字段 | 取值 | 含义 |
| --- | --- | --- |
| `status` | `ok` / `not ready` | 整体状态；任一子检查失败即为 `not ready` |
| `broker` | `ready` / `not ready` / `not applicable` | broker 就绪状态。内存 broker 实现 `Ready()` 通道，启动后关闭；Redis broker 不实现该接口，恒为 `not applicable` |
| `redis` | `ok` / `unreachable` / `not applicable` | 集群模式下的 Redis 连通性探测结果；非集群模式为 `not applicable` |

状态码语义：

- **200**：全部检查通过；
- **503**（`Service Unavailable`）：内存 broker 尚未就绪，或集群模式下 Redis 连通性探测失败。

细节：

- 集群模式下，若 broker 实现了 `Ping`，`cmd/server/main.go` 会将其注入为健康检查探针（`node.SetHealthCheck(pinger.Ping)`）；每次探测受 `healthCheckTimeout`（2 秒）约束，Redis 黑洞等故障不会让端点无限挂起；
- 健康检查不区分就绪（readiness）与存活（liveness），单一端点同时承担两种语义；
- 响应 `Content-Type` 为 `application/json`。

示例：

```bash
curl -s http://127.0.0.1:8080/health
```

单机 + 内存 broker 的典型响应：

```json
{"status":"ok","broker":"ready","redis":"not applicable"}
```

集群模式（Redis broker 正常）的典型响应：

```json
{"status":"ok","broker":"not applicable","redis":"ok"}
```

## 3. 指标

端点：`GET /metrics`，由 `promhttp.HandlerFor(reg, promhttp.HandlerOpts{})` 暴露，Prometheus 文本格式。

采集范围说明（`cmd/server/main.go`）：

- 指标注册在进程内新建的 `prometheus.NewRegistry()` 上，**只包含** `messageloop.Metrics` 定义的指标，不含 Go runtime 与 process 默认采集器（`go_*`、`process_*` 等）；
- `node.SetMetrics(metrics)` 后，`Node` 与 `Hub` 在运行路径中更新指标；集群模式下指标对象同时注入 Redis 命令总线与投影修复器；
- 全部指标以 `messageloop` 为命名空间，**均无标签**。

快速查看示例：

```bash
curl -s http://127.0.0.1:8080/metrics | grep '^messageloop_'
```

### 3.1 连接与订阅

| 指标名 | 类型 | 标签 | 含义 |
| --- | --- | --- | --- |
| `messageloop_connections_total` | gauge | 无 | 当前活跃客户端连接数。`Node.AddClient` 成功后 +1（`node.go`），会话关闭时 -1（`client.go` `close`） |
| `messageloop_subscriptions_total` | gauge | 无 | 当前活跃频道订阅数（含通配订阅）。订阅/退订时增减（`node.go`），会话恢复（resume）路径同样维护（`cluster_resume.go`） |
| `messageloop_active_channels` | gauge | 无 | 当前至少有一个订阅者的频道数。频道首个订阅者加入时 +1，最后一名退出时 -1（`node.go`） |

### 3.2 发布与投递

| 指标名 | 类型 | 标签 | 含义 |
| --- | --- | --- | --- |
| `messageloop_messages_published_total` | counter | 无 | 发布成功累计数。`Node.Publish` 与 `Node.PublishTransient`（presence 事件等）成功时 +1 |
| `messageloop_messages_delivered_total` | counter | 无 | 实时投递给订阅者成功累计数（`hub.go` 广播路径）。注意：**不计入历史恢复（recovery）投递**，仅统计实时广播 |
| `messageloop_delivery_failures_total` | counter | 无 | 投递失败累计数（死信）。广播时 `Client.Send` 返回错误即 +1（`hub.go`） |

### 3.3 时长直方图

| 指标名 | 类型 | 标签 | 含义 |
| --- | --- | --- | --- |
| `messageloop_message_publish_duration_seconds` | histogram | 无 | 发布耗时，覆盖 `Publish` 与 `PublishTransient` 两条路径；桶配置为 Prometheus 默认桶（`DefBuckets`） |
| `messageloop_rpc_duration_seconds` | histogram | 无 | RPC 请求处理耗时（代理往返，`client.go` `handleRPC`），即客户端发起 RPC 到收到代理响应的时长；桶配置为默认桶 |

### 3.4 集群指标

仅在集群模式（Redis 控制面）下会被更新，非集群部署时恒为 0：

| 指标名 | 类型 | 标签 | 含义 |
| --- | --- | --- | --- |
| `messageloop_cluster_command_dedupe_hits_total` | counter | 无 | 集群命令去重命中次数（`pkg/redisbroker/cluster_command_bus.go`） |
| `messageloop_cluster_command_timeouts_total` | counter | 无 | 集群命令等待应答超时次数（同一文件） |
| `messageloop_cluster_command_unknown_final_state_total` | counter | 无 | 集群命令进入 `unknown_final_state` 的次数（同一文件） |
| `messageloop_cluster_projection_repairs_total` | counter | 无 | 投影修复（projection repair）成功的轮次（`cluster_projection_repair.go`） |
| `messageloop_cluster_projection_repair_failures_total` | counter | 无 | 投影修复失败的轮次（同一文件） |

## 4. 日志

### 4.1 日志框架与级别

- 日志由 lynx 框架管理：`app.SetLogger(zap.MustNewLogger(app))`，后端为 zap（`cmd/server/main.go`）；
- 命令行参数 `--log-level` 控制日志级别，默认 `info`（`cmd/server/main.go` 的 `WithSetFlagsFunc`）；
- 核心包使用 `github.com/lynx-go/x/log`，输出结构化键值对日志；WebSocket 处理器会把 `client_id` 注入日志上下文（`pkg/websocket/handler.go`），单条连接的生命周期日志可据此串联。

### 4.2 常见日志事件

| 场景 | 日志语句（级别） | 位置 |
| --- | --- | --- |
| WebSocket 监听启动/停止 | `starting websocket server` / `stopping websocket server`（Info，含 `addr`） | `pkg/websocket/server.go` |
| gRPC 监听启动/停止 | `starting gRPC server` / `stopping gRPC server`（Info，含 `name`、`addr`） | `pkg/grpcstream/server.go` |
| WebSocket 升级失败 | `websocket upgrade error`（Error） | `pkg/websocket/handler.go` |
| 客户端会话创建失败 | `create client error`（Error） | `pkg/websocket/handler.go` |
| WebSocket 正常关闭 | `websocket closed normally`（Info） | `pkg/websocket/handler.go` |
| WebSocket 读错误（非正常关闭） | `websocket read error`（Error） | `pkg/websocket/handler.go` |
| 帧解码失败 | `decode client message error`（Error） | `pkg/websocket/handler.go` |
| 消息处理错误（含 Disconnect 码语义） | `handle message error`（Error） | `pkg/websocket/handler.go` |
| 消息收发（调试） | `handling message` / `sending message`（Debug） | `client.go` |
| 广播投递失败 | `send publication error`（Error） | `hub.go` |
| RPC 超时 | `RPC request timeout`（Warn，含 `channel`、`method`、`timeout`、`duration`） | `client.go` |
| 代理错误 | `RPC proxy error`（Warn） | `client.go` |
| 调查请求发送失败/超时 | `failed to send survey request` / `survey request send timed out`（Warn） | `node.go` |
| 调查注册表满载 | `survey registry full, rejecting survey registration`（Warn） | `node.go` |
| 集群：Redis Pub/Sub 断线重连 | `redis pubsub disconnected, retrying`（Warn） | `pkg/redisbroker/pubsub.go` |
| 集群：节点租约续期失败 | `cluster node lease renewal failed`（Warn，含 `node_id`、`incarnation_id`） | `cluster_state.go` |
| 集群：命令应答超时 | `cluster command timed out waiting for reply`（Warn） | `pkg/redisbroker/cluster_command_bus.go` |
| 集群：命令进入未知终态 | `cluster command entered unknown final state`（Warn） | `pkg/redisbroker/cluster_command_bus.go` |
| 集群：投影修复失败 | `cluster projection repair failed`（Warn） | `cluster_projection_repair.go` |
| 关停排空超时 | `shutdown: timed out draining client connections`（Warn） | `node.go` |
| 管理 API 调用 | `server side API Publish/Disconnect/Subscribe/...`（Info） | `pkg/grpcstream/api_handler.go` |

观察建议：以 Debug 级别运行可获得完整的消息收发轨迹，但消息体全文会进入日志（含 payload），生产环境仅在排查时临时开启。

## 5. 断连码参考

`Disconnect` 是携带 `Code` 与 `Reason` 的结构体并实现 `error` 接口（`disconnect.go`）：核心代码以返回错误的方式表达"应断开此连接"，`Client.HandleMessage` 用 `errors.As` 识别后关闭连接，传输层把 Code 与 Reason 交给客户端（WebSocket 用 close 帧的 code/reason，gRPC 流先投递 `DISCONNECT_ERROR` 错误帧）。`Code` 为 0 表示正常关闭（WebSocket 端映射为 1000）。

下表列出全部内置断连码（`disconnect.go`）。"服务端是否触发"标注该码当前在源码中是否有实际触发点：

| Code | 名称 | Reason | 含义 | 服务端触发 | 客户端建议处理 |
| --- | --- | --- | --- | --- | --- |
| 3000 | `DisconnectConnectionClosed` | `connection closed` | 连接关闭且无服务端建议：可能是干净断开，也可能网络中断，服务端无法区分 | 是（`client.go`） | 按需重连；如反复出现，先排查网络稳定性 |
| 3500 | `DisconnectInvalidToken` | `invalid token` | token 无效，或 `require_auth` 开启且未携带 token | 是（`client.go`） | 修复/刷新 token 后重连；不要无限重试 |
| 3501 | `DisconnectBadRequest` | `bad request` | 协议帧格式错误（如已鉴权再发 Connect） | 是（`client.go`） | 检查 SDK 版本与协议兼容性，属客户端 bug |
| 3502 | `DisconnectStale` | `stale` | 连接在配置的间隔内未完成鉴权 | 否（保留定义） | 连接后尽快发送带 token 的 Connect |
| 3503 | `DisconnectForceNoReconnect` | `force disconnect` | 服务端要求不要重连（如关停排空 `DrainAll`） | 是（`node.go`） | 停止重连，等待外部恢复信号 |
| 3504 | `DisconnectConnectionLimit` | `connection limit` | 超过每用户连接数上限（`limits.max_connections_per_user`） | 是（`hub.go`） | 先断开该用户的旧连接；检查客户端是否泄漏连接 |
| 3505 | `DisconnectChannelLimit` | `channel limit` | 超过每客户端订阅数上限（`limits.max_subscriptions_per_client`） | 是（`client.go`） | 收敛订阅数；检查订阅清理逻辑 |
| 3506 | `DisconnectInappropriateProtocol` | `inappropriate protocol` | 传输无法承载的数据（如 JSON 客户端收到二进制频道） | 否（保留定义） | 频道数据格式与客户端编码不匹配，属集成错误 |
| 3507 | `DisconnectPermissionDenied` | `permission denied` | 权限不足 | 否（保留定义） | 检查 token 权限与 ACL 规则 |
| 3508 | `DisconnectNotAvailable` | `not available` | 服务端无法处理该消息类型 | 否（保留定义） | 检查客户端使用的消息类型是否被支持 |
| 3509 | `DisconnectTooManyErrors` | `too many errors` | 客户端产生过多错误 | 否（保留定义） | 修复客户端侧持续报错的根本原因 |
| 3511 | `DisconnectIdleTimeout` | `idle timeout` | 心跳检测到超时未活动（`server.heartbeat.idle_timeout`，默认 300 秒） | 是（`heartbeat.go`） | 定期发送 Ping 保活（见[《客户端协议参考》](../protocol.md)的心跳小节） |
| 3512 | `DisconnectSlowConsumer` | `slow consumer` | 客户端消费速度跟不上，写入失败触发 | 是（`client.go`） | 加快消费或增加缓冲；客户端能力与频道吞吐不匹配 |

说明：

- 3510 在代码中未定义；
- 收到 35xx 终态码时，除 3503 外均可在修正根因后重连；重连策略与错误展示的协议细节见 [../protocol.md](../protocol.md) 的 Error Codes 与 Disconnect Codes 章节；
- 服务端目前**没有**按断连码输出的指标，按码统计断连数需要从客户端侧聚合断连事件（SDK 在收到断开通知时可上报，见 [sdk-go.md](sdk-go.md)、[sdk-ts.md](sdk-ts.md)）。

## 6. 监控建议

### 6.1 关键告警项

| 告警项 | 依据指标/信号 | 建议阈值与说明 |
| --- | --- | --- |
| 连接数骤降 | `messageloop_connections_total` | 单节点 5 分钟内下降超过 50% 或归零：大概率是节点崩溃、网络分区或关停；配合该节点的 `/health` 与进程监控判定 |
| 投递失败率上升 | `messageloop_delivery_failures_total` 与 `messageloop_messages_delivered_total` 的速率比 | 比率持续 > 1%：慢消费者或传输写失败（参见日志 `send publication error`） |
| RPC 延迟劣化 | `messageloop_rpc_duration_seconds` 的 P99（`histogram_quantile`） | 与 `server.rpc_timeout`（默认 30s）对比；P99 接近超时值即应告警，同时观察 `RPC request timeout` 日志 |
| 发布延迟劣化 | `messageloop_message_publish_duration_seconds` 的 P99 | 内存 broker 下应远小于毫秒级；偏高说明广播扇出大或订阅者写阻塞 |
| 集群节点离线 | 外部探测：每个节点的 `/health`；集群节点数与租约状态见 [cluster.md](cluster.md) | 节点 `/health` 连续 N 次 503 即告警；注意非集群模式下 `/health` 不会探测 Redis，`redis` 字段为 `not applicable` |
| 集群命令健康 | `messageloop_cluster_command_timeouts_total`、`messageloop_cluster_command_unknown_final_state_total` | 两者速率 > 0 即排查：Redis 延迟、命令总线故障、节点租约丢失 |
| 投影修复失败 | `messageloop_cluster_projection_repair_failures_total` | 速率 > 0 即告警：查询投影与真实状态不一致的风险 |
| 心跳超时断连 | 无专用指标：用日志（`idle timeout` 相关）或客户端侧 3511 断连码计数 | 大量 3511 说明客户端未按时 Ping，检查客户端保活逻辑 |

### 6.2 生产注意事项

- **绑定地址**：`server.http.addr` 默认为 `127.0.0.1:8080`，仅暴露 `/health` 与 `/metrics`。该 HTTP 面**无鉴权**，若绑定到非回环地址，必须置于私有网络或防火墙之后，否则指标与健康状态会对外泄露（参见 [../deployment.md](../deployment.md) 的 Health And Metrics 与 Production Checklist 章节）；
- **抓取间隔**：推荐 10–15 秒，不小于 5 秒。直方图桶为 Prometheus 默认桶，P99 类告警需要足够的历史样本；
- **registry 不含 Go runtime / process 指标**：如需 GC、协程数等运行时信号，请另行通过 `net/http/pprof` 或外部 exporter 采集，本端点不提供；
- **多节点部署**：每节点独立暴露指标，聚合与告警请按 `node_id`（或实例标签）区分；集群相关指标只在集群模式下有意义（见 §3.4）；
- 健康检查探针的 Redis 探测超时固定为 2 秒（`healthCheckTimeout`），请勿把 `/health` 用作 Redis 延迟的监控手段。

## 7. 故障排查指引

| 症状 | 检查步骤 | 可能根因 |
| --- | --- | --- |
| 客户端频繁断连 | 1) 客户端侧记录断连码（§5）；2) 以 Debug 级别查服务端日志；3) 检查 `messageloop_connections_total` 是否抖动 | 3511 心跳超时（客户端未 Ping）；3512 慢消费者（频道吞吐超客户端消费能力）；3504/3505 连接/订阅超限；3500 token 失效或 `require_auth` 配置 |
| RPC 超时 | 1) 查 `messageloop_rpc_duration_seconds` P99 与 `RPC request timeout` 日志；2) 核对 `server.rpc_timeout` 与各代理 `proxy[].timeout`（三层超时见[《架构指南》](architecture.md)与[《配置参考》](configuration.md)）；3) 直接调用代理后端测量延迟 | 代理后端变慢或不可达；超时配置过短；路由未命中（此时表现为 echo 回显而非超时，见 [../protocol.md](../protocol.md)） |
| 集群不生效 | 1) 核对 `cluster.enabled`、`cluster.node_id`、`broker.type=redis`（集群要求 Redis broker，配置校验见[《配置参考》](configuration.md)）；2) 查 `messageloop_cluster_command_timeouts_total` 与 `cluster node lease renewal failed` 日志；3) 调 `/health` 看 `redis` 字段；4) 确认各节点 `node_id` 唯一 | Redis 不可达/延迟高；`node_id` 冲突导致租约抖动；命令总线故障（见 [cluster.md](cluster.md) 与 [../deployment.md](../deployment.md) 的多节点章节） |
| 消息丢失或延迟 | 1) 对比 `messageloop_messages_published_total` 与 `messageloop_messages_delivered_total` 速率；2) 查 `messageloop_delivery_failures_total` 与 `send publication error` 日志；3) 检查 `messageloop_active_channels` 是否符合预期 | 慢消费者写阻塞（3512）；订阅未建立（查 `messageloop_subscriptions_total`）；通配订阅匹配问题（见[《架构指南》](architecture.md)的 topic matcher 章节） |
| `/health` 返回 503 | 1) 读响应体：`broker` 字段为 `not ready` 还是 `redis` 字段为 `unreachable`；2) 前者等 broker 就绪（启动早期正常），后者检查 Redis 连通性 | 启动阶段 broker 未就绪；集群模式下 Redis 不可达（探测 2 秒超时） |
| 管理 API 或监控端无响应 | 1) 确认 `server.http.addr` 端口可达（默认 `127.0.0.1:8080`，仅回环）；2) 确认与客户端监听端口区分开（见 [../deployment.md](../deployment.md) 的 Listener Model） | 端口未绑定或绑定到回环导致外部不可达；进程崩溃（配合 `messageloop_connections_total` 归零确认） |
