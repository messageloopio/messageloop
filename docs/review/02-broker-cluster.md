# 评审任务 02：Broker 与集群层

## 背景

你要评审的项目是 **MessageLoop**（工作目录：`D:/Codes/qiulin/messageloop`），一个用 Go 编写的实时消息平台服务端，通过 WebSocket 和 gRPC 提供 pub/sub 消息能力。先读根目录 `AGENTS.md` 了解构建命令与代码规范。

你是独立评审 agent，没有任何先前上下文。**只做只读评审，不修改任何代码。**

## 评审范围（Broker 与集群层）

- `broker.go`（`Broker` 接口、`Publication`）、`broker_memory.go` 及 `broker_memory_test.go`
- `pkg/redisbroker/` 全部（Redis Streams 历史 + Redis Pub/Sub 实时扇出 + 集群适配实现）：`redis.go`、`pubsub.go`、`history.go`、`epoch.go`、`presence_redis.go`、`cluster_command_bus.go`、`cluster_directory.go`、`cluster_query_store.go` 及全部测试
- 集群控制面（根包）：`cluster.go`、`cluster_commands.go`、`cluster_state.go`、`cluster_resume.go`、`cluster_projection_repair.go` 及对应测试（含 `cluster_redis_integration_test.go`）
- 参考文档：`docs/developer/04-cluster.md`

## 模块职责与关键契约（供定位，需你自行通读验证）

- `Broker` 接口（`broker.go`）：`Start/Subscribe/Unsubscribe/Publish/PublishTransient/History`；内存实现用于单节点，Redis 实现用于集群。
- Redis broker：`Publish` 先 XADD 再 PUBLISH，失败 XDEL 回滚；pub/sub 断线 1s→30s 指数退避重连；`deliverOnce` 按 offset 去重。
- 集群接口（`cluster.go`）：`SessionDirectory`（会话租约 + 快照 CAS）、`ClusterCommandBus`（单播/广播命令）、`ClusterQueryStore`（频道订阅投影）、`ClusterNodeLeaseManager`、`ClusterProjectionRepairer`。
- 命令总线：默认命令等待 5s、handler 执行 10s、claim lease 30s；超时返回 `UnknownFinalState`。
- 会话恢复：先对 session lease CAS，失败返回 `DisconnectStale` 拒绝恢复，不发起 takeover。

## 评审维度

1. **分布式正确性**：会话租约 CAS、takeover、投影修复、epoch 管理的竞态与边界条件；命令总线的去重、超时、lease 续期、panic 恢复。
2. **Redis 交互正确性**：XADD/PUBLISH 顺序与回滚、断线重连后 catch-up 不重不漏、offset 编解码兼容、Stream 长度控制。
3. **两实现语义一致性**：内存 broker 与 Redis broker 在 handler 错误传播、History 语义等方面是否行为一致（接口使用者不应感知差异）。
4. **并发与性能**：锁粒度（如 `deliverMu` 全局串行化投递）、goroutine 泄漏、信号量边界。
5. **错误处理**：错误吞没、超时合理性、重试边界。
6. **测试缺口**：尤其 Redis 相关路径的单元测试覆盖。

## 已知线索（需你独立核实真伪，确认或推翻，并给出证据）

1. `docs/developer/04-cluster.md` 称 `History` 的 `since_offset` 是 exclusive，但实现（内存与 Redis）疑均为 inclusive——文档与代码必有一方错误。
2. `pkg/redisbroker/pubsub.go` 的 `deliverOnce` 用单把 `deliverMu` 串行所有频道的投递去重，疑为多频道吞吐瓶颈。
3. Redis broker `deliverOnce` 用 `_ = b.handler(...)` 吞掉 handler 错误，而内存 broker 会把 handler 错误返回给 `Publish` 调用者——两实现行为不一致。
4. `runPubSub` 与 `catchUpMissed` 疑存在几乎相同的反序列化 + 构造 `Publication` 重复代码块。
5. `pkg/redisbroker/cluster_query_store.go` 无独立单元测试；`cluster_state.go` 的 `clusterNodeLeaseManager` 无单独测试；`catchUpMissed` 无独立测试。
6. 命令总线注释声明无签名/认证、依赖 Redis 网络隔离——评估该信任边界是否在部署文档中有对应说明。
7. `cluster_query_store.go` 等文件疑使用 CRLF 行尾，与仓库其余 LF 不一致。

## 工作流程

1. 先跑 `go build ./...` 和 `go test ./...`（如有 Docker/Redis 环境可跑 `cluster_redis_integration_test.go`，没有则注明跳过）。
2. 通读范围内代码，逐维度评审。
3. 逐条核实"已知线索"：确认（给出决定性证据）或推翻。
4. 补充你自己发现的新问题。

## 输出格式

用中文输出。先给基线测试结果与总体评价（3-5 句），然后逐条 findings：

```
[级别] Critical / Important / Minor
[位置] path:line
[问题] ...
[证据] 关键代码摘录或推理
[修复建议] ...
[置信度] high / medium / low
```

最后单独一节列出"建议补充的测试"。不要贴大段代码，每条 finding 引用不超过 10 行。
