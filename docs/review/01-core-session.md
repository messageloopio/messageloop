# 评审任务 01：核心会话层

## 背景

你要评审的项目是 **MessageLoop**（工作目录：`D:/Codes/qiulin/messageloop`），一个用 Go 编写的实时消息平台服务端，通过 WebSocket 和 gRPC 双向流提供 pub/sub 消息能力，协议基于 protobuf envelope（`InboundMessage`/`OutboundMessage`）。先读根目录 `AGENTS.md` 了解构建命令与代码规范。

你是独立评审 agent，没有任何先前上下文。**只做只读评审，不修改任何代码。**

## 评审范围（核心会话层）

根包 `messageloop` 中负责单客户端连接全生命周期与协议消息处理的文件：

- `client.go`（约 43KB，核心协议状态机）及其测试 `client_test.go`、`client_fix_test.go`
- `hub.go`（连接注册表，64 分片）及 `hub_test.go`
- `node.go`（中央协调者，16384 订阅分片锁）及 `node_test.go`
- `presence.go`、`presence_event.go`、`heartbeat.go`、`survey.go` 及 `survey_test.go`
- `disconnect.go`、`acl.go` 及对应测试
- `marshaler.go`（根目录）、`transport.go`、`pool.go`、`defaults.go`
- 参考文档：`docs/developer/01-architecture.md`、`docs/protocol.md`

## 模块职责与关键契约（供定位，需你自行通读验证）

- `Transport` 接口（`transport.go`）：`Write/WriteMany/Close/RemoteAddr`，核心逻辑与传输解耦。
- `Client.HandleMessage(*clientpb.InboundMessage)`：协议入口，分发到 `handleConnect/handlePublish/handleSubscribe/handleRPC/handleSurvey...`。
- `Hub`：会话与订阅索引，`numHubShards=64` 分片；通配订阅走 `pkg/topics.CSTrieMatcher`。
- `Node`：聚合 Hub + Broker + PresenceStore + Cluster + ProxyRouter；订阅变更用 `subscription_saga` 保证原子性（失败逆序回滚），`subLocks[16384]` 按 channel 哈希串行化。
- 断连语义：typed `Disconnect` 错误，code 3000–3512；心跳空闲超时 3511、慢消费 3512。
- 广播：`Hub.broadcastPublication` 订阅者 >8 时用容量 64 的信号量并发投递。

## 评审维度

1. **并发正确性**：`Client.mu`/`Hub` 分片锁/`subLocks` 的锁覆盖是否完整；goroutine 生命周期（心跳、慢消费关闭、presence 异步发布、close 的 16 个 worker）是否有泄漏或竞态；channel 使用是否有 goroutine 泄漏风险。
2. **协议状态机正确性**：Connect/Resume（本地/远程/匿名拒绝）/Publish/Subscribe/RPC/Survey 各路径的状态迁移、错误返回、ack 语义。
3. **错误处理**：`Disconnect` 与非 `Disconnect` 错误的区分、错误是否被吞、回滚路径是否完整。
4. **资源管理**：订阅上限、消息大小上限、慢消费处理、close 清理是否彻底。
5. **代码质量**：超大函数（`handleConnect` 约 327 行）、重复逻辑、命名误导。
6. **测试缺口**：对照测试文件找出未覆盖的关键分支。

## 已知线索（需你独立核实真伪，确认或推翻，并给出证据）

1. `ClientInfo()`（`client.go` 约 754 行）未加锁读取 `c.client/c.session/c.user/c.connectedAt`，疑与 Connect/Close 写操作存在数据竞争。
2. `ACLEngine` 用 `path.Match`（仅支持 `*`），但注释/文档声称支持 `chat.**`，含 `**` 的规则疑不按预期匹配（`acl.go`）。
3. `Hub.GetActiveChannels` 疑把通配模式本身当 channel 列出，且同会话精确+通配订阅会被重复计数。
4. `Hub.ReplaceSession` 先读连接数再写，疑有 TOCTOU 窗口导致超限。
5. `handleConnect` 中 `oldSession.closeQuiet()` 后若 `ReplaceSession` 失败，旧会话疑成僵尸（hub 中残留但 transport 已关）。
6. `statusConnected` 常量声明后疑从未使用，`Client.status` 始终停留在 `statusConnecting`。
7. `subShard.broadcastPublication` 疑仅在测试中被调用，与 `Hub.broadcastPublication` 存在重复实现。
8. `PresenceInfo.ClientID` 疑实际存的是 session ID，命名误导。
9. `Survey.Close` 疑只从 channel drain 一条数据且 channel 从未被 close。
10. 测试缺口：无 `presence_test.go`、`heartbeat_test.go`、`subscription_saga_test.go`；`Node.Shutdown`/`DrainAll` 无单测。

## 工作流程

1. 先跑 `go build ./...` 和 `go test ./...`（跳过需要 Redis 的集成测试可注明）确认基线。
2. 通读范围内代码，逐维度评审。
3. 逐条核实"已知线索"：确认（给出决定性证据）或推翻（说明为什么不是问题）。
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
