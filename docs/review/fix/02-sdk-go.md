# 修复任务 02：Go SDK

## 背景

你要修复的项目是 **MessageLoop**（工作目录：`D:/Codes/qiulin/messageloop`），Go 实时消息平台，你的范围是其 Go SDK（`sdks/go/`，终端实时客户端 WebSocket/gRPC 双传输 + 后端 Proxy 服务器骨架）。

刚完成一轮全项目代码评审，评审发现已经过主 agent 逐条核实。完整方案见 `docs/review/fix-plan.md`（可读）。**先读 `sdks/go/` 下相关代码再动手。**

## 文件归属（严格）

- 你拥有：`sdks/go/` 全部。
- 禁止修改：仓库其他任何目录（服务端根包、TS SDK、docs/ 由其他 agent 并行处理）。
- 注意：`sdks/go` 通过 `replace` 引用 `../../shared`，但你不得修改 `shared/`——如需协议侧改动，在报告中提出而非实施。

## 任务清单

### P0（必修）

1. **`handleConnected` 与 `Close()` 竞态 → `close(nil)` panic**（`client.go:296-308,830-838`）：`handleConnected` 在 `c.mu.Unlock()` 后 `select { case <-ch: default: close(ch) }`；`Close()` 把 `c.connectedCh = nil` 后，在途 Connected 消息取到 nil channel → `close(nil)` panic，进程崩溃。修复：close 前判 nil，或 `Close()` 后置已关闭哨兵 channel。补并发回归测试（连接建立瞬间并发 `Close()`，`-race` 跑）。

### P1（必修）

2. **PingTimeout 零实现**（`options.go:57-60,154-158`；`client.go:1074-1078`；`websocket.go:97`）：`PingTimeout` 字段/默认值/setter 俱全但无任何读取点；`handlePong` 空实现；WS 读无 deadline——半开连接（NAT 超时等）永不发现，自动重连永不触发。实现 pong 超时：pingLoop 记录最近 ping/pong 时间，超时主动 `Close()` transport 触发重连流程。先写一个当前会失败的测试再实现（TDD）。TS SDK 的对应实现在 `sdks/ts/src/client/client.ts:467-471` 可参考语义（但注意它的超时处理有 bug，别照抄 close 逻辑）。
3. **SubRefresh/Survey 缺失**（`client.go:249-281`）：`handleMessage` switch 缺 `SubRefreshAck`/`SurveyRequest`/`SurveyReply` 三个 case；服务端已完整实现（根 `client.go:346-351,1175-1287`），cluster survey 依赖客户端应答。至少实现：收到 `SurveyRequest` 提供用户回调 + `SendSurveyReply` API（默认无 handler 时回 echo 应答，与服务端语义一致）；`SubRefresh` 可提供发送 API + ack 路由。补"收到这三种信封不崩溃"的基线测试。
4. **ephemeral 不支持**（`client.go:187-191,437-443,470-476,870-918` 六处硬编码 `Ephemeral: false`）：协议（`service.proto:74`）与服务端均支持逐订阅 ephemeral。增加 `SubscribeWith(channel string, opts ...SubscribeOption)` 或等价变体，透传到全部构造 `Subscription` 的位置；默认行为不变。
5. **`Connect()` 失败/重试不清理**（`client.go:198-215`）：发送失败/超时/ctx 取消三条路径直接 return，不 close transport、不停已启动的 `receiveLoop`（`client.go:203` gen=0）；重试复用同一 transport → 双 gen-0 receiveLoop 并存重复投递。修复：失败统一 close transport 并终止对应 receiveLoop；重试推进 generation 并用新 transport。对照 `reconnect()`（`client.go:787-801`）的正确清理模式。
6. **自动重连后手动 `Connect()` 必然挂起**（`client.go:203,724,288-290`）：`Connect()` 固定 gen=0 启动 receiveLoop，而 `reconnect()` 每次 `generation.Add(1)`；发生过重连后手动 Connect 收到的 Connected 因代次不匹配被永久丢弃，阻塞至 30s 超时。修复：`Connect()` 同样推进 generation 并用新 transport。
7. **`HandlerImpl.RPC/Authenticate` 对 `(nil,nil)` 无防护**（`proxy.go:246,296-298`）：自定义 handler 返回 nil response 时 `resp.Error`/`resp.UserInfo.ToProto()` 解引用 panic——gRPC handler 内 panic 直接崩掉 proxy 进程。加 nil 判定返回 `status.Error(codes.Internal, ...)`，补测试。
8. **resumed 会话不回写订阅列表**（`client.go:310-317`）：`handleConnected` 在 `resumed=true` 时跳过把服务端下发的权威订阅列表写回本地 `subscriptions`（服务端无条件返回完整列表，根 `client.go:684-693`）；跨进程恢复（集群快照）的频道下次重连会丢失。改为无条件以服务端列表为准写回。
9. **`Unsubscribe` 不清 `channelOffsets`**（`client.go:356-362,93-94`）：退订再订阅同一频道后，重连携带旧 offset+`Recover:true`，收到退订期间的历史消息（重复投递）。退订时同步 `delete(channelOffsets, ch)`。
10. **`OnSubscribed/OnUnsubscribed` 丢参数**（`proxy.go:88-98,142-148,348-369`）：接口只有 `ctx`，而 `proxy.proto:102-116` 的请求含 `session_id/channel/username` 且服务端真实填充（根 `client.go:1064-1071,1123-1131`）。接口改为 `OnSubscribed(ctx context.Context, sessionID, channel, username string)`（Unsubscribed 同理）——这是 breaking change，同步更新默认实现、示例与测试。

### P2（随 P1 顺手修，均为小改动）

11. `Build*` 构造器吞 `ToPayload` 错误（`client.go:933,947,1008`）：无法改 public 签名，至少在文档注释标注该限制。
12. 回调 handler 字段无锁读写竞态（`client.go:79-83,609-631` vs `330/375/415-417`）：setter 与读取处加锁（可用独立 `handlerMu`）。
13. `RPC` 无默认超时（`client.go:575-605`）：增加 `WithRPCTimeout` 选项并默认施加超时 ctx。
14. `NewProxyServer` 零值 `Insecure=false` 与服务端 TLS 拨号不匹配（`proxy.go:397-409`）：`Insecure=false` 且未配置 TLS 时启动即报错，或文档显著标注。

## 测试要求

- 修复前先跑 `cd sdks/go && go build ./... && go test ./...`（含 `-race`）确认基线。
- 每条 P0/P1 修复配回归测试（沿用现有 `fakeTransport` 模式）：重点为 1（并发 panic）、2（pong 超时触发重连）、5/6（Connect 失败清理与代次）、7（nil 防护不 panic）、8（resumed 写回）、9（退订清 offset）。
- 完成后 `go test -race ./...` 全绿。

## 纪律

- 不做 git commit/push。最小改动，不顺手重构无关代码。
- 协议语义基准：服务端根包 `client.go` 与 `protocol/client/v1/service.proto`。
- 完成后返回报告：每条任务处置（已修/未修+原因）、改动文件清单、测试结果、遗留问题（尤其 breaking change 清单）。
