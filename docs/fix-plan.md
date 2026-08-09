# 修复方案(Code Review Fix Plan)— 修订版 v2

基于两轮深度代码评审(`go test -race` 实测复现 3 处失败 + 逐行源码核实)与一轮独立文档审查(审查意见 A1-A6、25 项遗漏核对、方案可行性结论)产生的修复清单。**本文档是修复实施的唯一依据**;修复完成后须通过顶部"验证命令"全绿验证。

- 评审日期: 2026-08-09
- 文档修订: v2(已并入独立审查全部意见)
- 基线: `main` @ 879bb63
- 修复原则: 行为最小变更、不引入新依赖、每个修复附带测试(能复现原问题)

## 验证命令

```bash
go build ./...
go vet ./...
go test -race ./...
go test -race ./pkg/topics/ -bench 'BenchmarkMultithreaded16Thread9010(InvertedBitmap|OptimizedInvertedBitmap)$' -benchtime=1x
```

**注意**:
- `go test -race ./...` 中 `TestClusterRedis_SurveyAggregatesAcrossNodes` 需要 Redis 可用(通过环境变量 `MESSAGELOOP_TEST_REDIS_ADDR` 指定,无 Redis 时该测试 `t.Skipf` 跳过,不算通过)。
- 与仓库 `Taskfile.yml` 对齐:`go test -race ./...`、`go vet ./...`、`go build ./...` 一致;可加 `task lint`(golangci-lint)兜底。

## 修复前已知失败(须在修复后全部转绿)

| 用例 | 竞态双方 | 位置 | 对应修复条目 |
|------|----------|------|--------------|
| TestMemoryBroker_Publish_ConcurrentSafe | `Publish` 读 `b.handler` ↔ `Start` 写 `b.handler` | broker_memory.go:103 / :58 | P0-6 |
| TestClusterRedis_SurveyAggregatesAcrossNodes | BroadcastCommand 各 goroutine 写共享 `Metadata` map ↔ 主 goroutine 读 | cluster_command_bus.go:167 / :233 | P0-1 |
| BenchmarkMultithreaded16Thread9010OptimizedInvertedBitmap | `subPos`/`deletedPositions` 锁外读写 | optimized_inverted_bitmap.go:77-87 | P0-7 |

---

# P0 — 崩溃与安全(必须修复,按编号顺序执行)

## P0-1 集群命令 Metadata map 深拷贝

- **文件**: `pkg/redisbroker/cluster_command_bus.go`
- **问题**: `BroadcastCommand`(:239)`copyCommand := *cmd` 只复制结构体,`Metadata` map 被所有 goroutine 共享;`SendCommand`(:167)`cmd.Metadata[clusterCommandReplyKey] = replyChannel` 并发写同一 map → `concurrent map writes` 进程崩溃(已 -race 复现)。
- **修复**:
  1. `BroadcastCommand` goroutine 内深拷贝 Metadata:`copyCommand.Metadata = make(map[string]string, len(cmd.Metadata)); for k, v := range cmd.Metadata { copyCommand.Metadata[k] = v }`。
  2. `SendCommand` 收到回复后校验 `result.CommandID == cmd.CommandID`,不匹配则记录日志并继续等待直到超时(纵深防御;replyChannel 每次唯一,跨命令串线在现有代码下不可能)。
- **测试**: `TestClusterRedis_SurveyAggregatesAcrossNodes` 在 -race 下转绿。

## P0-2 gRPC Transport 关闭路径 TOCTOU / 并发 Send(含默认配置路径)

- **文件**: `pkg/grpcstream/transport.go`
- **问题**: ①`Close`(:94)`close(t.sendCh)` 与仍在发送的 goroutine 竞争 → send-on-closed-channel panic;②`writeError`(:110)直接 `t.stream.Send` 与 worker(:130)`SendMsg` 并发(grpc-go 禁止同流并发 Send);③**默认配置(`writeTimeout<=0`,config.yaml 未配置时)下 `sendWithTimeout`(:64-66)绕过 worker 直发 `SendMsg`,大扇出并发 WriteMany 时与 worker/Close 并发 Send 竞态仍在**。
- **修复**:
  1. 删除 `Close` 中的 `close(t.sendCh)`;worker 改为经 `closeCh` 退出(`for { select { case req := <-t.sendCh: ...; case <-t.closeCh: return } }`)。
  2. `sendWithTimeout` **无论 `writeTimeout` 是否大于 0 一律经 sendCh 排队**(`<=0` 时使用默认超时,如 10s),消除直发路径的并发 Send。
  3. 发送前检查 `closed`(同一把 mu 下),已关闭直接返回错误。
  4. `writeError` 的错误帧改经 sendCh 排队发送;`Close` 不持锁执行发送。
  5. `Close` 幂等性由 `closeOnce` 保持。
- **测试**: 新增并发测试:多 goroutine 持续 `WriteMany` 同时 `Close`,断言不 panic;默认配置(不设 writeTimeout)下同样覆盖(-race 运行);回归 `pkg/grpcstream/e2e_test.go`。

## P0-3 JSON payload 发布被 structpb 文本格式破坏(全部 5 处)

- **文件**: `client.go:735`、`client.go:984`、`client.go:1030`、`pkg/grpcstream/api_handler.go:35`、`api_handler.go:263`
- **问题**: `p.Json.String()` 对 `*structpb.Struct` 返回 protobuf 文本格式(`fields:{...}`)而非 JSON;JSON 客户端发布/管理端发布/survey 载荷全部损坏。
- **修复**: 统一替换为 `json.Marshal(p.Json.AsMap())`(错误时返回 error)。为收敛,抽取包级 helper(如 `func marshalJSONStruct(s *structpb.Struct) ([]byte, error)`),5 处全部走 helper。
- **测试**: 新增单测:构造 `Payload_Json`,断言输出可由 `json.Unmarshal` 解析且内容正确(覆盖 handlePublish 与 admin publish 两路径)。

## P0-4 SDK `RPC` 与 `Close` 并发双重 close panic

- **文件**: `sdks/go/client.go`
- **问题**: `RPC`(:487-492)defer 无条件 `close(ch)`;`Close`(:754-759)先 `delete` 再 `close(ch)`。Close 后 pending RPC 的 defer 再次 close → `close of closed channel` panic 崩掉调用方进程。
- **修复(采用方案 2)**: 每个 pending RPC 的 channel 关闭统一由 `sync.Once` 封装(如 `type rpcChan struct { ch chan *clientpb.OutboundMessage; once sync.Once }`,`close()` 方法内 `once.Do`)。`RPC` 的 defer 与 `Close` 均调用该 `close()`。**不用"只 delete 不 close"方案**:RPC 的 select 监听调用方 ctx,`Close` 仅 cancel 内部 `c.ctx`(:735),删通道不关闭会导致 RPC 永久挂起。
- **测试**: 新增并发测试:goroutine 中发起 RPC,主 goroutine 立即 `Close`,断言不 panic 且 RPC 返回错误而非挂起。

## P0-5 Connect 消息:ACL 绕过 + 订阅数上限 + 无锁写 Client 字段(三合一,与 P1-2 紧邻实施)

- **文件**: `client.go handleConnect`(:326-332、:394、:444-461)
- **问题**: ①connect 订阅循环直接 `AddSubscription`,无 `FindProxy`/`CanSubscribe` 检查、无 `MaxSubscriptionsPerClient` 限制(`handleSubscribe` 有完整 ACL);②继承块 `c.user/c.client/c.session/c.subscribedChannels`(:326-332)与 `c.user = authResp.UserInfo.ID`(:394)无锁写,与 heartbeat `close()`、`evictSessionForTakeover`(cluster_resume.go:161-176 锁内读同一字段)竞态。
- **修复**:
  1. 抽取共享 helper(如 `func (c *Client) checkSubscribeACL(ctx, in, channel, token string) error`),与 `handleSubscribe` 复用;connect 循环中逐频道执行,拒绝时发送 per-channel error 并跳过该频道(不断开连接)。
  2. 订阅数上限:循环前校验 `len(connect.Subscriptions)` 与已继承的 `subscribedChannels`(resumed 场景,client.go:329)之和不超过 `MaxSubscriptionsPerClient`,超出返回 `DisconnectChannelLimit`。
  3. 继承块与 `c.user` 赋值包进 `c.mu.Lock()`(先取 `oldSession.mu` 拷贝、释放后再取 `c.mu`,无嵌套)。
  4. 鉴权语义注意:`resumeRemoteSession` 仍在鉴权前设置 `c.session`(供 `authReq.SessionID` 使用),但**无副作用状态字段**(user/client/subscribedChannels)在鉴权前不写入——takeover 与状态继承(即 P1-2)移到鉴权成功后执行。
- **测试**: 新增:denyAll ACL 下 connect 携带被禁频道订阅,断言订阅未生效、连接未断开;并发 connect 与 close 无竞态(-race);订阅上限含继承数生效。

## P0-6 memory broker `b.handler` 无锁读写

- **文件**: `broker_memory.go`
- **问题**: `Start`(:58)写 `b.handler`,`Publish`(:103)无锁读 → 数据竞态(已复现)。
- **修复**: `b.handler` 改为 `atomic.Pointer[PublicationHandler]`(`Start` 用 `Store`,`Publish` 用 `Load` 判空)。
- **测试**: `TestMemoryBroker_Publish_ConcurrentSafe` 转绿。

## P0-7 bitmap 匹配器 `subPos`/`deletedPositions` 锁外读写(两个 matcher)

- **文件**: `pkg/topics/inverted_bitmap.go:32,59`、`pkg/topics/optimized_inverted_bitmap.go:77-87`
- **问题**: `inverted_bitmap.go:32` 锁外读 `b.subPos`(锁内 :59 自增);optimized 版(:77-87)在 `b.mu.Lock()` 之前直接读写 `deletedPositions` 与 `subPos` → 并发 Subscribe 撞 position,订阅互相覆盖/误删;已 -race 复现(optimized 基准 FAIL)。inverted 版同模式,一并修复。
- **修复**: 两个 matcher 的 `subPos` 读取、自增与 `deletedPositions` 的全部读写移入 `b.mu.Lock()` 临界区(参照 inverted_bitmap.go:35-60 已正确部分)。
- **测试**: `go test -race ./pkg/topics/ -bench 'BenchmarkMultithreaded16Thread9010(InvertedBitmap|OptimizedInvertedBitmap)$' -benchtime=1x` 转绿;新增并发 Subscribe 正确性测试(position 不重复、订阅不互相覆盖)。

---

# P1 — 消息正确性与安全(按编号顺序执行;P1-2 与 P0-5 紧邻实施)

## P1-1 ReplaceSession 不迁移通配订阅 → 恢复后通配消息丢失

- **文件**: `hub.go:617-628`
- **问题**: 只替换 subShards 的 exact 订阅;`wcSubs`(hub.go:30)与 matcher 中的 `Subscriber` 仍指向被 `closeQuiet` 的旧 Client → 通配消息投递到已关闭连接。
- **修复**: `ReplaceSession` 在 `wcSubsMu` 锁内遍历 `h.wcSubs`,将属于该 sessionID 条目的 `Subscriber.Client` 替换为新 Client;同时 matcher 侧对每条通配模式执行 `Unsubscribe(旧)` + `Subscribe(新)` 重建(cstrie 内部是接口值拷贝,必须重建;无按 Subscriber 更新 API)。
- **测试**: 新增:客户端订阅 `chat.*`,执行 `ReplaceSession`,发布消息,断言新会话收到、旧会话收不到;并断言 `LookupSubscriber` 返回新 Subscriber。

## P1-2 接管先于鉴权执行 → sessionID 可远程杀会话 / 鉴权失败删他人状态

- **文件**: `client.go handleConnect`(:338 在 :347 鉴权之前)、`cluster_resume.go`
- **问题**: ①未认证连接凭 sessionID 即可触发远端 `evictSessionForTakeover`;②鉴权失败后 `close()` 的 `deleteClusterSessionState(c.session)`(:155)删除可能在他节点正常服务的 session 状态。
- **修复**(与 P0-5 同一区域,合并实施):
  1. `resumeRemoteSession` 中的 takeover 与状态继承(写 user/client/subscribedChannels)移到 proxy 鉴权成功之后;鉴权前仅设置 `c.session = connect.SessionId`(供 authReq.SessionID 使用)。
  2. `deleteClusterSessionState` 增加所有权校验:删除前 `GetSessionLease`,仅当 lease 指向本节点(NodeID+IncarnationID 匹配)或 lease 已不存在/过期才删除。
- **测试**: 新增:无效 token + sessionID 连接,断言未发送 takeover 命令(fake bus 验证);鉴权失败后断言远端 lease/snapshot 未被删除。

## P1-3 恢复消息 ID 不稳定且无总量上限

- **文件**: `client.go:464-495`、`hub.go:302,399`
- **问题**: 恢复历史用 `uuid.New()` 生成新 ID,实时投递(hub.go:302/:399)也用 uuid → 同一条消息恢复与实时 ID 不同,客户端无法按 ID 去重。
- **修复**: 实时投递与恢复统一 ID 规则:`fmt.Sprintf("%s-%d", channel, offset)`(channel+offset 全局唯一)。恢复总量上限 1000 条(超出截断并记录日志)。
- **测试**: 新增:发布 N 条,恢复消息 ID 与实时投递 ID 规则一致。

## P1-4 PublishAck.Offset 恒为 0

- **文件**: `node.go:438-448`、`client.go:743-753`
- **问题**: `Node.Publish` 丢弃 broker 返回的 offset。
- **修复**: `Node.Publish` 返回 `(uint64, error)`;`handlePublish` 将 offset 填入 `PublishAck.Offset`。已核实所有调用方兼容(`PublishPresenceJoin/Leave`、`api_handler` 等忽略返回值或 `_ =` 用法均成立)。
- **测试**: 新增:发布后断言 `PublishAck.Offset` 等于 broker 返回的 offset。

## P1-5 流式 offset 编码 `ts*1000+seq` 碰撞

- **文件**: `pkg/redisbroker/history.go:32-34, 66-79`
- **问题**: 同毫秒第 1000+ 条与下毫秒首条 offset 相同,恢复错取/跳过(seq 为 64 位无上限)。
- **修复**: 编码改为 `ts<<20 | seq`(ts = offset>>20, seq = offset&0xFFFFF,seq 上限约 100 万/ms);`sinceOffset+1` 进位在 ms 边界保持正确;注释注明新编码,旧编码 offset 不再兼容(由客户端携带的 epoch 校验兜底)。
- **测试**: 新增 `parseStreamOffset` 往返一致性单测,含 ts 边界与 seq>1000 场景。

## P1-6 requireAuth 且无 auth proxy 时任意 token 通过

- **文件**: `client.go:347-360`
- **问题**: `requireAuth=true` 时仅拦截空 token;`connect.Token != ""` 且无匹配 auth proxy 时跳过认证直接放行,`user=""`。
- **修复**: `requireAuth` 开启且 token 非空但无 auth proxy 时,拒绝连接(返回 `DisconnectInvalidToken` 或启动时校验配置)。
- **测试**: 新增:requireAuth、无 proxy、携带任意 token,断言被拒绝。

## P1-7 ACL 规则顺序敏感(denyAll 可被前置宽松规则绕过)

- **文件**: `acl.go:74-83, 92-101`
- **问题**: 首个命中条目即 return,前置宽松规则可绕过后续 denyAll。
- **修复**: 最坏匹配优先语义:先收集所有命中条目,denyAll 命中即拒绝;否则按最后一个(或最具体)命中条目的 allow 列表判定;行为变更在文档与测试中明确。
- **测试**: 新增:宽松规则在前、denyAll 在后,断言 deny 生效(及反向顺序)。

## P1-8 Survey 响应可伪造

- **文件**: `node.go:733-740`
- **问题**: `AddSurveyResponse` 仅按 requestID 查 survey,任何客户端可注入任意 sessionID 的响应。
- **修复**: `AddSurveyResponse` 校验发起响应的 sessionID 确实是该 survey 的订阅者(localSurvey 发送时记录 sessionID 集合),非订阅者响应丢弃并记录日志。
- **测试**: 新增:非订阅者伪造响应被丢弃。

## P1-9 命令总线无发送方身份校验

- **文件**: `pkg/redisbroker/cluster_command_bus.go:289-375`
- **问题**: 任何能写 Redis 的进程可注入 takeover/disconnect/publish 命令。
- **修复**: 本期实现成本高,采用:①在代码注释与部署文档明确信任边界(Redis 网络隔离为安全前提);②命令载荷增加 `IssuedBy` 字段(发送方 NodeID)便于审计;签名机制列为后续工作。
- **测试**: 无新增测试(仅注释/字段);现有命令总线测试保持通过。
- **状态**: 已实施(2026-08-10 终审补齐):`ClusterCommand` 增加 `IssuedBy` 字段(cluster.go,注释声明"审计用,非安全边界");`SendCommand`/`BroadcastCommand` 填充 `b.nodeID`;目标节点 `handleMessage` 记录 `issued_by` 到日志;`cluster_command_bus.go` 包级注释补充信任边界说明;新增 `TestClusterCommandBus_SendCommandFillsIssuedBy`。

## P1-10 SDK 错误信封不按 ID 路由 pending RPC

- **文件**: `sdks/go/client.go:228-230`
- **问题**: 服务端返回带请求 ID 的 Error 时只走 `handleError`,pending RPC 挂到 ctx 超时。
- **修复**: receiveLoop 对 Error 信封检查 `msg.GetId()`,命中 pending RPC 则投递该错误并 `delete`(配合 P0-4 的 once-close);未命中才走 handleError。
- **测试**: 新增:服务端返回带 ID 的 Error,RPC 立即收到错误而非超时。

## P1-11 SDK proxy HandlerImpl 嵌入覆盖模式不成立

- **文件**: `sdks/go/proxy.go:181, 204, 229, 253, 268`
- **问题**: 显式调用 `h.RPCHandlerImpl.HandleRPC` 等,用户在外部类型上的覆写被绕过(注释宣称可覆写);`:204` `resp.Payload.ToPayload()` 错误被吞。
- **修复**: 改为接口字段分派(如 `type HandlerImpl struct { RPC RPCFunc; ... }`,默认实现可被替换),或通过外部类型方法分发;`ToPayload` 错误记录日志返回错误。
- **测试**: 新增:自定义 handler 覆写生效;payload 转换失败返回错误。

## P1-13 精确+通配双重订阅 → 消息重复投递

- **文件**: `hub.go:374-435`
- **问题**: `broadcastPublication` 先经 subShards 广播 exact 订阅(:376),再经 matcher 广播通配订阅(:381),两路未去重;同一客户端同时订阅 `chat.x`(精确)与 `chat.*`(通配)时收到 2 条重复消息(且两路消息 ID 不同,客户端无法按 ID 去重)。`GetMatchingSubscribers`(:473-495)有去重,广播路径没有。
- **修复**: 广播前按 sessionID 合并去重(将 exact 订阅者与 matcher 命中者合并为 `map[sessionID]*Client` 后统一发送,可复用 `GetMatchingSubscribers` 的合并逻辑;同时消除两路不同 message ID 的问题——合并后单路发送,同一 client 只收一份)。
- **测试**: 新增:客户端同时订阅 `chat.x` 与 `chat.*`,发布 `chat.x`,断言仅收到 1 条。

## P1-12 SDK Message 零值/JSON 空值 Data 断言 panic

- **文件**: `sdks/go/message.go:44-49`
- **问题**: `NewData("application/json", nil)` 后 `AsJSON` 对 nil interface 断言 panic,库代码崩调用方进程。
- **修复**: 类型断言前判 `d.value == nil`,返回空值/空串;`ToPayload`/`String` 同步防护。
- **测试**: 新增:零值 Message 的 `ToPayload`/`String`/`AsJSON` 不 panic。

---

# P2 — 健壮性与加固(按编号顺序执行)

## P2-1 WS 入站消息无默认大小上限

- **文件**: `pkg/websocket/handler.go:60-62`、`node.go:522-524`、`cmd/server/runtime.go:46`、`config/config.go:53`
- **问题**: `MaxMessageSize=0` 时 WS 无限制(gorilla 默认),gRPC 走原始配置 0=4MB;config 注释"0 = default (64KB)"未实现;`DefaultMaxMessageSize` 死常量。
- **修复**: `Node.MaxMessageSize()` 对 0 回退 `DefaultMaxMessageSize`(64KB);gRPC 侧(runtime.go)同步走 `node.MaxMessageSize()` 保持两传输一致(0=64KB)。行为变更属配置注释承诺,文档注明。
- **测试**: 新增:`MaxMessageSize()` 0 值返回 64KB;WS 超限连接被拒绝。

## P2-2 集群命令:每命令 goroutine 无上限 + 认领后崩溃命令 10 分钟锁死

- **文件**: `pkg/redisbroker/cluster_command_bus.go:94-99`、`:402-440`
- **修复**:
  1. 命令处理 goroutine 加有界信号量(默认 128),满时阻塞处理保证不丢命令。
  2. 认领机制:认领 key 带 owner 心跳/短 TTL;`resolveExistingCommand` 对超时未完成的 pending 允许重新认领。
- **测试**: 新增:认领后不执行(模拟崩溃)的超时再认领测试;现有命令总线测试保持通过。

## P2-3 Survey 无每命令超时 + localSurvey 无写超时(含 survey 注册表防护)

- **文件**: `node.go:574-582`、`survey.go:98`、`pkg/redisbroker/cluster_command_bus.go:343`
- **问题**: 单客户端写阻塞 → localSurvey 永不返回 → 命令 state key 卡 pending、survey 注册表膨胀。
- **修复**:
  1. `localSurvey` 发送 goroutine 使用带超时 ctx(如 `context.WithTimeout(ctx, 10s)`),发送失败记入响应。
  2. 命令 handler 执行加 per-command 超时(10s),超时写终态 `CLUSTER_COMMAND_TIMEOUT`。
  3. `survey.Wait` 对 `timeout<=0` 回退默认超时(5s)。
  4. `registerSurvey` 上限防护:超过阈值(如 1000)拒绝注册并记录(与超时修复配合,常态下不应触发)。
- **测试**: 新增:mock 写阻塞 transport,Survey 超时返回而非挂起;`timeout=0` 防御。

## P2-4 Cluster.Start 部分失败泄漏已启动组件

- **文件**: `cluster.go:249-256`
- **问题**: 第 N 个组件启动失败时,前 N-1 个保持运行,`startErr` 固化导致重试永远失败。
- **修复**: 启动失败时逆序 `Shutdown` 已启动组件并返回聚合错误;重试语义明确(允许重建 Cluster 实例,不修复原地重试)。
- **测试**: 新增:mock 第 3 个组件失败,断言前 2 个被 Shutdown 调用。

## P2-5 健康检查恒返回 ok

- **文件**: `health.go`
- **问题**: 不反映 broker Ready / Redis 连通性 / 集群状态。
- **修复**: `HealthHandler`:broker `Ready()`(若实现)未关闭返回 503;`ClusterEnabled()` 时对 Redis ping(2s 超时)失败返回 503;响应含状态详情。
- **测试**: 新增:broker 未启动返回 503;Redis 不可达返回 503。
- **状态**: 已实施(2026-08-10 终审补齐):`Node` 增加可注入 `HealthCheck func(context.Context) error`(`SetHealthCheck`);`health.go` 在 broker Ready 检查后以 2s 超时调用(cluster 模式且非 nil),失败返回 503 并置 `redis: "unreachable"`;`pkg/redisbroker` 导出 `Ping`,`cmd/server/main.go` 在 broker 创建后类型断言注入;新增 `TestHealthHandler_ClusterEnabled_HealthCheckFailure_Returns503` / `_OK_Returns200` / `_NoHealthCheck_Returns200`。

## P2-6 接管 evict 半途失败产生半 evict 状态

- **文件**: `cluster_resume.go:178-186`
- **问题**: 任一频道 `removeLocalSubscriptionOnly` 失败提前 return,部分频道已移除、session 未从 hub 移除。
- **修复**: 收集所有失败,继续清理其余频道;全部完成后若存在失败,对已移除频道重新订阅回滚并返回聚合错误。
- **测试**: 新增:mock broker 使第 2 个频道 Unsubscribe 失败,断言最终订阅状态一致。

## P2-7 evict 不更新共享频道投影(与 restore 不对称)

- **文件**: `cluster_resume.go`(`evictSessionForTakeover`)、`cluster_resume.go:91-110`(`restoreSessionSubscriptions`)、`cluster_state.go:229`(`adjustClusterChannelSubscriptions`)
- **问题**: 接管转移语义下计数靠"双不增减"巧合持平,失败路径即漂移。
- **修复**: `evictSessionForTakeover` 每移除一频道调用 `adjustClusterChannelSubscriptions(ctx, ch, -1)`;`restoreSessionSubscriptions` 每恢复一频道 `+1`(短超时、失败仅记录不阻断)。
- **测试**: 新增:takeover 后 `Channels()` 订阅数正确。

## P2-8 Disconnect 码语义与文档不一致

- **文件**: `client.go:662`(未认证 Publish 返回 `DisconnectStale` 3502)、`disconnect.go`(3511/3512 超出 AGENTS.md 声明段)、AGENTS.md
- **问题**: 未认证 Publish 用"stale"码(语义为超时未认证);码段文档过时。
- **修复**: 未认证 Publish 改返回 `DisconnectInvalidToken`(3500);同步更新 AGENTS.md 码段为 3000-3512;**同步更新受影响测试**:`client_test.go:394-395`(断言 3502)与 `disconnect_test.go` 相关断言。
- **测试**: 上述测试更新后全部通过。

## P2-9 SDK 重连泄漏与幽灵 Connected

- **文件**: `sdks/go/client.go:633-720`
- **问题**: `c.transport = trans`(:650)无锁写;Send 失败/超时(:699-719)后新 transport 不关闭;迟到 Connected 改写重连状态。
- **修复**:
  1. `c.transport` 替换在 `c.mu.Lock()` 内;读取侧(Publish/RPC/pingLoop)同步加锁或改 `atomic.Pointer`。
  2. `reconnect()` 失败路径(发送失败、等待超时)先 `trans.Close()` 再返回。
  3. 迟到 Connected 校验引入**连接代数(generation)计数器**:每次 reconnect 创建新 transport 时 `generation++`,Connected 响应回带期望代数,不匹配则丢弃(仅凭 session/epoch 无法区分新旧连接)。
- **测试**: 新增:reconnect 失败后 transport 关闭被调用(无泄漏);迟到 Connected 不重置重连状态。

## P2-10 HTTP proxy 通知方法丢弃后端响应(归因修正)

- **文件**: `proxy/http.go` 的 4 个**通知方法**(OnConnected/OnSubscribed/OnUnsubscribed/OnDisconnected,parseFunc 直接返回 `FromProto...(nil)`):http.go:231-233、261-263、292-294、322-324
- **问题**: 后端返回的 `Error` 字段被吞(与 gRPC 版透传不一致);注意 `Authenticate/SubscribeAcl/PublishAcl/RPC`(http.go:94-100 等)已透传 Error,**不在本次修复范围**。
- **修复**: 4 个通知方法的 parseFunc 解析响应体并透传 Error;调用方(node 侧 `_, _ = p.OnConnected(...)`)保持忽略错误语义不变。
- **测试**: 新增 httptest server 返回错误,断言通知方法返回的错误可被读取(不吞)。

## P2-11 HTTP proxy 请求字段丢失

- **文件**: `proxy/http.go:114-118, 149-152, 183-186`
- **问题**: 手写白名单 map 丢 SessionId/RemoteAddr/UserId。
- **修复**: 补全字段(`SessionId`、`RemoteAddr`、`UserId` 视方法语义);保持现有请求体格式兼容(增量补字段,不整体切换序列化方式)。
- **测试**: 新增:断言请求体包含此前丢失字段。

## P2-12 optimized bitmap 空分段语义不一致(仅拒绝显式空分段,保留填充机制)

- **文件**: `pkg/topics/optimized_inverted_bitmap.go:35-37, 91-93`
- **问题**: 显式空分段(`""`,如 `"a."`/`.a`/`a..b`)与其他 3 个 matcher 语义不一致(`"a"` 能命中 `"a."`)。
- **修复**: `Subscribe`/`Lookup` 入口拒绝含显式空分段的主题(Subscribe 返回 `ErrBadTopic`,Lookup 不匹配);**保留** :91-93 的尾部 padding 与 :139 lookup padding(它们是"短主题匹配长主题"的 AND 语义必需机制,移除会导致 `a` 无法匹配 `a.b`)。
- **测试**: 新增:显式空分段订阅返回 ErrBadTopic;`a` 匹配 `a.b` 的既有语义保持;已核实现有测试/基准无空分段主题,不受影响。

## P2-13 测试质量:复制粘贴错误与无效基准

- **文件**: `pkg/topics/*_test.go`
- **问题**: naive 的多线程基准(:161-217)实际用 `NewTrieMatcher()`;Unsubscribe 基准在同一 id 上重复卸载(首次迭代后 no-op,inverted 版还无限 append deletedPositions);`TestThroughput` 无断言。
- **修复**:
  1. naive 基准改用 `NewNaiveMatcher()`。
  2. Unsubscribe 基准每次迭代生成新 id(或卸载后重新订阅)。
  3. `TestThroughput` 增加与 naive 的正确性对照断言。
- **测试**: 基准可复现真实场景;`TestThroughput` 有断言。

## P2-14 ConnectionsTotal 计量漂移

- **文件**: `node.go:216`、`client.go:160-162`
- **问题**: 仅 `AddClient` 成功时 Inc;`close()` 无条件 Dec(认证失败、resumed 连接、AddClient 失败路径 Dec 无 Inc)。
- **修复**: `Client` 增加 `metricsCharged bool`(AddClient 成功后置位);`close()` 仅在该标志为真时 Dec。
- **测试**: 新增:认证失败连接后 gauge 不为负。

## P2-15 jsonLog 急切求值

- **文件**: `client.go:229, 868`
- **问题**: 关闭 debug 日志时仍对每条消息做一次 protojson.Marshal。
- **修复**: 先检查日志级别(如 `log.Default().Enabled(ctx, slog.LevelDebug)`)再序列化,或改为惰性 `func() any` 参数(若 lynx 支持)。
- **测试**: 无功能断言;基准/单测确认序列化仅在 debug 开启时发生(可用计数验证)。

## P2-16 大扇出逐订阅者 goroutine

- **文件**: `hub.go:327-350, 409-433`
- **问题**: >8 订阅者(及通配路径)每订阅者 1 goroutine,万级订阅 = 万级 goroutine。
- **修复**: 有界并发(如 `errgroup.SetLimit(64)` 或信号量)分批发送。
- **测试**: 现有 hub 测试保持通过;无新断言(性能项)。

## P2-17 close() 串行 saga 慢

- **文件**: `client.go:136-140`
- **问题**: 逐频道串行 `RemoveSubscription`,cluster 模式每步 10s 超时(node.go:383/396),千级订阅断开需数小时。
- **修复**: 频道清理并行化(有界并发,如 16),保持 saga 语义(单频道内顺序不变);cluster 步骤超时缩短(10s→2s)。
- **测试**: 新增:多频道 close 完成时间显著缩短(不设严格断言,保证正确性)。
- **状态**: 已实施(2026-08-10 终审补齐):`RemoveSubscription` 的 cluster.session/cluster.channel 两个 commit 步骤 10s→`clusterStepTimeout`(2s 常量,node.go);回滚步骤保持 5s 不变;AddSubscription 的 cluster.session 沿用请求 ctx 不变。

## P2-18 broker_memory 频道历史永不回收

- **文件**: `broker_memory.go`
- **问题**: `Unsubscribe` no-op,`history` map 随频道数无限增长。
- **修复**: memoryBroker 增加订阅计数;`Unsubscribe` 计数到 0 时删除 `subs[ch]`,且**仅当该频道历史为空**(`h.count==0`,在 h.mu 下检查)才删除 `history[ch]` 条目——历史非空时保留供断线恢复(与 P1-3 恢复功能语义一致:最后订阅者退订不应清空历史)。锁顺序:先 b.mu 后 h.mu(Publish 先 b.mu 取引用释放后再 h.mu,无环)。
- **测试**: 新增:退订后历史保留、空历史频道条目被回收;并发 Publish/Unsubscribe 无竞态(-race)。

## P2-19 presence 事件写入 broker 历史

- **文件**: `node.go:748-765`
- **问题**: `PublishPresenceJoin/Leave` 经 `Publish` 写入历史环形缓冲,恢复消息流混入 join/leave。
- **修复**: presence 事件发布路径避开 History(如 Broker 增加 `PublishNoHistory` 方法或在 presence 频道上不记录历史;最小方案:presence 频道发布时不写历史、仅 pubsub)。
- **测试**: 新增:presence 事件不出现在 History 恢复结果中。
- **补充(2026-08-10 最终审查)**: presence 事件经 `PublishTransient` 发布(offset=0),按 P1-3 的 `channel-offset` ID 规则所有事件共用 `channel-0` ID,客户端无法区分。修复:实时广播的 message ID 规则改为 `offset>0` 用 `channel-offset`、`offset==0` 回退 uuid(`publicationMessageID`,hub.go,仅 transient/presence 场景命中);恢复路径保持 `channel-offset`(历史 offset 恒 >0,P1-3 测试不受影响)。
- **状态**: 已实施(含上述补充);新增 `TestNode_PublishPresenceJoin_DistinctMessageIDs`。

## P2-20 WS close 帧 Code=0 非法

- **文件**: `pkg/websocket/transport.go:62`
- **问题**: `Disconnect{}`(Code=0,用于正常关闭)时 `FormatCloseMessage(0, ...)` 违反 RFC 6455,浏览器按 1006 处理、reason 丢失。
- **修复**: Code==0 时回退 `websocket.CloseNormalClosure`(1000)。
- **测试**: 新增:Code=0 关闭时帧内 code 为 1000。

## P2-21 Ping 无节流、每 Ping 2 goroutine + Redis 同步

- **文件**: `client.go:923-945`
- **问题**: `handlePing` 每次派生 refreshPresence + syncClusterSessionState 两个 goroutine(10s 超时),恶意高频 Ping 放大。
- **修复**: 合并为低频刷新(如间隔 5s 内的 Ping 仅刷新 lastActivity,不重复同步);或对控制消息限流。
- **测试**: 新增:连续 Ping 不产生成比例 goroutine/Redis 调用(计数断言)。

## P2-22 history 恢复 epoch 校验空洞

- **文件**: `client.go:467`
- **问题**: 仅 `sub.Epoch != ""` 时校验;老客户端无 epoch 时 broker 重启后的陈旧 offset 被当作有效。
- **修复**: 客户端未携带 epoch 且服务端 broker 有 epoch 时,拒绝恢复或从 0 全量恢复(选择:从 0 恢复并记录日志,保守不丢消息)。
- **测试**: 新增:无 epoch 客户端在 broker 重启后从 0 恢复。

## P2-23 admin API Publish 吞错误 / add_history 空实现

- **文件**: `pkg/grpcstream/api_handler.go:78-83`
- **问题**: `Publish` 错误只 log 恒返回成功;`add_history` 空实现。
- **修复**: Publish 聚合每个 publication 结果返回部分成功语义;`add_history` 未实现则返回显式 `UNIMPLEMENTED` 错误而非静默忽略。
- **测试**: 新增:broker 失败时 admin Publish 返回错误;add_history 返回显式错误。

## P2-24 admin token 非常量时间比较

- **文件**: `pkg/grpcstream/server.go:100`
- **问题**: token 比较非恒定时间。
- **修复**: `crypto/subtle.ConstantTimeCompare`。
- **测试**: 现有鉴权测试保持通过。

## P2-25 WS HTTP server 无 ReadHeaderTimeout

- **文件**: `pkg/websocket/server.go:59-62`
- **问题**: 无 `ReadHeaderTimeout`/`IdleTimeout`,slowloris 面。
- **修复**: 设置 `ReadHeaderTimeout`(如 10s)与 `IdleTimeout`。
- **测试**: 无新增(配置项)。

## P2-26 全局 codec 注册副作用

- **文件**: `pkg/grpcstream/server.go:33-37`、`sdks/go/grpc.go:46-49`
- **问题**: 两处以 "proto" 名字全局注册 RawCodec,grpc v1.83 下后注册覆盖前者,污染进程内所有 gRPC 连接。
- **修复**: 改为 per-connection 的 `grpc.ForceCodec`(grpcstream 侧用 `grpc.MaxCallRecvMsgSize`/默认 codec 或命名空间隔离;SDK 侧 `WithDefaultCallOptions(grpc.ForceCodec(...))`);最小方案:注册前检查是否已存在同名 codec。
- **测试**: 现有 gRPC 测试保持通过。

## P2-27 presence Get O(N)(标注非 bug)

- **文件**: `presence.go:68-81`、`presence_redis.go:63-109`
- **结论**: 返回全量 presence 是 API 固有语义,非缺陷;仅记录优化方向(缓存/分页)至文档,本期不实施。

---

# 修复顺序与完成定义

1. **P0-1 → P0-7**(每项独立验证;先写失败测试再修复)
2. **P1-1 → P1-12**(P1-2 与 P0-5 同一区域,合并实施、同一提交)
3. **P2-1 → P2-26**(P2-8 需同步改 AGENTS.md;P2-27 仅文档标注)
4. **全量验证**:顶部"验证命令"全部通过;**修复前已知失败表 3 项全部转绿**(含 Redis 可用环境下的集成测试)
5. 更新本文档"验证结果"小节(修复后由独立审查确认)

## 风险与注意

- P0-2 涉及 gRPC 传输并发模型,改动后须回归 `pkg/grpcstream/e2e_test.go` 与 `pkg/websocket/e2e_test.go`。
- P0-5/P1-2 改动 handleConnect 鉴权/订阅区域,须回归全部连接层 e2e 与集成测试;注意 `authReq.SessionID` 在鉴权前使用 connect.SessionId 的语义保持。
- P1-4 改 `Node.Publish` 签名,已核实调用方兼容;改后跑全量 build。
- P2-1 将 WS 默认上限从"无限制"变为 64KB,属行为变更(配置注释承诺);部署文档注明。
- P2-8 码变更影响 `client_test.go:394-395` 与 disconnect 相关测试,一并更新。
- 不引入新依赖;结构体字段仅做内部新增(不加序列化字段)。
- 每项修复保持行为最小变更;验收标准:修复前测试能复现、修复后转绿。

## 验证结果(2026-08-10,全部通过)

- [x] `go build ./...` 通过
- [x] `go vet ./...` 通过
- [x] `go test -race -count=1 ./...` 全绿(根模块 9 包 + sdks/go 独立模块,含 Redis 集成测试)
- [x] bitmap 竞态基准转绿(`-race -benchtime=1x`,Inverted/Optimized 均 PASS)
- [x] 已知失败表 3 项全部转绿(TestMemoryBroker_Publish_ConcurrentSafe、TestClusterRedis_SurveyAggregatesAcrossNodes、BenchmarkMultithreaded16Thread9010OptimizedInvertedBitmap)

### 实施过程说明(供后续审查)

- P0-1~P0-7、P1-1~P1-13、P2-1~P2-26 全部实施;每项"先写复现测试再修复",修复后转绿。
- P2-18 语义经实施中冲突确认最终为:"最后订阅者退订时仅回收**空历史**频道条目,非空历史保留供断线恢复"(与 P1-3 恢复功能一致)。
- 跨代理回归修复:handlePublish 的 `Payload_Text` case 曾被 P0-3 编辑误删,已恢复(client.go:859-862)。
- 修复范围与文档条目一一对应;另有 2 处超出文档范围的小调整(见 git diff):hub.go 的 `RemoveSessionIfMatches`(P0-5 代理,close 只清理自己拥有的 session)、SDK 的 `rpcPending` 投递锁内化(P0-4 代理,receiveLoop send 移入锁内)。
- 终审补齐(2026-08-10):P1-9(`IssuedBy` 审计字段 + 信任边界注释)、P2-5(健康检查 Redis ping)、P2-17(集群步骤超时 10s→2s)、presence 消息 ID 唯一化(offset==0 回退 uuid)四项;每项"先写测试再修复",新增测试见各条目"状态"。
- 未做 git commit(按约定留给用户决定)。
