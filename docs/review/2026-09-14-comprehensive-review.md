# MessageLoop 综合架构评审报告（2026-09-14）

评审方式：主会话建立项目画像并按职责切分 7 个模块，7 个只读子代理并行评审（六维度），主会话逐条复核高严重度发现、抽查中严重度发现，并完成横切评审。全程零代码改动；`go build ./...`、`go vet`、`go test ./...` 在评审基线上全部通过。

---

## 1. 项目画像

- **定位**：Go 实时消息平台服务器。客户端经 WebSocket（`:9080`）/ gRPC streaming（`:9090`）/ 可选 QUIC、KCP（UDP + TLS overlay）接入；协议为 protobuf 信封 `InboundMessage`/`OutboundMessage`，JSON 与 protobuf 双编码。
- **多租户（P1）**：频道命名空间化 `ns:topic`；namespace 在连接时由鉴权代理响应（`UserInfo.namespace`）解析，回退静态 `server.namespace`；越权频道操作返回 `NAMESPACE_MISMATCH`，跨 namespace resume 拒绝（3500）。
- **分层**：
  - 传输层 `pkg/transport/{ws,grpc,quic,kcp}`：accept/读写循环、长度前缀帧、TLS、写超时。
  - 会话层 `internal/session`：Session 状态机（Authenticating/Attached/Detached/Closed）、Attachment、双泳道发送队列、本地 resume/takeover、Hub（64 分片，订阅锁 16384 分片）。
  - 运行时 `internal/runtime` Node：中央协调器——授权表（`internal/authz`，PR-KA-A4）、occupancy 代际去重、survey、presence、recover、subscription saga（本地 + 集群两段式，`clusterStepTimeout=2s`）。
  - 数据面 `internal/stream.Broker`：内存实现（分片异步派发）+ Redis 实现（`pkg/redisbroker`，Redis Streams + pub/sub，history、seq gap、Lua 原子 seq 分配）。
  - 控制面（`cluster.enabled`）：node_epoch INCR 分配 incarnation、SessionDirectory、HMAC-SHA256 命令总线、QueryStore、节点租约、ClusterRepairer（投影修复/membership/用户索引）、跨节点 resume。
  - 对外：`proxy/`（`rpc.*` 转发后端 gRPC/HTTP + token 鉴权代理）、`internal/admin`（admin gRPC API + capability 位）、`cmd/server`（lynx 装配，Prometheus /metrics、/health）。
- **部署形态**：单二进制；`broker.type=memory|redis`；集群模式需 Redis + 共享 HMAC key；配置 viper/lynx YAML。
- **规模**：约 4.7 万行非生成 Go（不含 SDK/生成代码）；SDK 独立模块（`sdks/go` 自有 go.mod、`sdks/ts`）。
- **依赖**：gorilla/websocket、grpc-go、quic-go、kcp-go v5、go-redis v9、prometheus、lynx。

## 2. 架构总评

**优点（明显高于平均水准的设计纪律）**：
- 授权收敛为单一 Authorizer 表，deny 语义保守、fail-closed；admin capability 位逐 RPC 落位。
- 集群控制面：HMAC 门（规范编码、常数时间比较、±30s 偏斜、去重窗口大于重放窗口）、全 CAS 租约写入、INCR epoch 拒绝降级启动、命令三态机（claim/续租/终态）、确定性 fencing 仿真器。
- 会话层状态机与 Attachment 分离清晰；Close/Fence 幂等且普遍有身份校验；namespace 守卫几乎覆盖所有入口。
- 数据面：Broker 接口把投递顺序、错误语义、gap 分类写成显式契约；Redis 侧 Lua 原子化；五个话题匹配器共享单一 `matchCriteria` 且一致性测试覆盖 `:`/`**` 语义。
- 测试文化厚实：`go test ./...` 全绿，规格条款（§/KD/PR）与注释一一对应。

**总体风险**：风险不在架构方向，集中在四类工程缝隙——
1. **并发缝隙**：持锁重入死锁、resume 链路 delegate 生命周期、broker 差分非原子、fence 回滚窗口（多数窗口窄但后果重）。
2. **无界状态**：occupancy 去重表、presence gens、用户索引 set、通配 lastOffsets、命令 inbox 流——实体生命周期结束时缺少统一清理钩子，长期运行必然兑现为内存/Redis 膨胀。
3. **复制式一致性**：四个传输各持一份心跳/断连信封/证书逻辑，已实际漂移出 WriteTimeout 语义、超长帧反馈、close-code 三处不一致；Go/TS SDK 语义漂移同理。
4. **协议消费端缺口**：服务端定义了完整的 3000-3514 断连码语义，但两个 SDK 都不消费（无限重连、TS 连码值都丢弃）。

## 3. 复核记录

- **高严重度 7 条：全部逐条打开代码核实，全部确认**（M1-1、M1-2、M2-1、M3-1、M5-1、M7-1、M7-2；M3-1 附加缓解注记，维持高）。
- **中严重度抽查 6 条**：M1-3 SubRefresh 越权、M6-1 protojson 严格解析、M5-4 WriteTimeout 语义、M5-2 GracefulStop、M4-3 liveDesired 差分、M6-2 Validate 无 proxy 覆盖——前 3 条亲自核实确认；后 3 条逻辑链核实成立、涉及第三方库行为处标注待验证。
- **降级/校准**：M3-2 命令终态未签名（子代理自标待复核，维持"中+待验证"：需 Redis 写权限 + ms 级时序窗口，属信任边界不一致而非直接注入）；M2 ReplaceRules ghost presence（生产代码无调用方，降为低/待验证）；M5 WS close 帧与在途写交错（RFC 层面瑕疵，维持低/待验证）。
- **剔除**：无完全误报条目；子代理报告质量高，未发现虚构代码引用。

## 4. 确认的发现清单（按 影响 × 修复成本 排序）

### P0 — 高影响、低成本（建议立即修复）

| # | 发现 | 位置 | 说明 / 建议 |
|---|------|------|-------------|
| 1 | **[可靠性][高] MarkMetricsCharged 持写锁调用 TransportLabel 自死锁** | `internal/session/client.go:127`、`session.go:825-828` | AddClient 与 Close 竞态时，`MarkMetricsCharged` 在持 `mu.Lock()` 下调用 `TransportLabel()`（内部 `RLock`），RWMutex 不可重入 → goroutine 永久阻塞，会话全部操作死锁、连接僵死。修复：持锁分支直接 `MetricsTransportLabel(c.protocol)`。 |
| 2 | **[安全性][高] WS permessage-deflate 解压炸弹** | `pkg/transport/ws/handler.go:39,75-77` | `SetReadLimit` 作用于压缩后字节（gorilla 语义），解压输出无界（DEFLATE 极限约 1032:1）：64KB 恶意帧可在服务端缓冲约 64MB。示例配置 `compression: true` 默认开启。修复：解压侧 `io.LimitReader` 限长 = MaxMessageSize。 |
| 3 | **[功能性][高] 陈旧 shell 的 delegate 分支误杀健康会话** | `internal/session/session.go:793-795`、`client.go:519,541-543` | delegate 只写不清；第二次本地 resume 时 Detach 关闭旧 transport → 旧 shell 的 deferred `closeFromAttachment` 命中 delegate 分支绕过附件身份比较 → 对仍在新连接上服务的健康会话整体 `Close`。链式 resume 后客户端"重连成功即掉线"。修复：delegate 分支同样比较 `att` 身份。 |
| 4 | **[安全性][中] handleSubRefresh 缺 namespace 校验与订阅覆盖门** | `internal/session/client.go:1503-1527` | 全部频道处理器中唯一没有 `checkNamespace` 的入口（subscribe/unsubscribe/survey/presence 均有）。可伪造跨租户频道的 presence leave 广播，并向 proxy 后端泄漏跨 ns 频道名；ACL 请求不带身份。修复：循环头加 `checkNamespace(ch)` + `alreadySubscribed` 门控。 |

### P1 — 高影响、中等成本

| # | 发现 | 位置 | 说明 / 建议 |
|---|------|------|-------------|
| 5 | **[功能性][高] 两 SDK 无视"禁止重连"断连码** | `sdks/go/client.go:541-543,1836+`、`sdks/ts/src/client/client.ts:807+` | 服务端 3503（勿重连）/3514（需升级 SDK）/3500（跨租户 resume 拒绝）语义被当成暂时故障无限重试（Go 默认无限次）。修复：重连前检查 `DisconnectError.Code`；3503/3514 停止并上报，3500 丢弃 SessionId 降级全新会话。 |
| 6 | **[功能性][高] TS SDK 丢失断连码与原因** | `sdks/ts/src/transport/websocket.ts:88` | `onclose` 只透传数字 code，`reason` 从不读取，客户端侧无 `DisconnectError` 对象——无法区分 3511/3514/3503，与 Go SDK 公开语义分叉。与 #5 一并修。 |
| 7 | **[可靠性][高] occupancy 去重表 lastApplied 只写不删（无界增长族旗舰）** | `internal/runtime/node.go:1357-1374` | 每个 (channel, session) 占用事件（含跨节点）永久 +1 条目，无任何删除路径、无指标。同类：`occGens`/memoryPresenceStore `gens`（node.go:1334-1338、occupancy/presence.go:66-76）、集群用户索引 set 只增不减（cluster_directory.go:328-340）、通配订阅 lastOffsets（pubsub.go:532-543）、死亡 incarnation inbox 流（cluster_command_bus.go:674）。修复：见机制建议 M-1。 |
| 8 | **[可靠性][高] 节点租约续租失败静默 → 对等节点误删活会话 fencing → resume 永久丢失** | `internal/runtime/cluster_state.go:446-448`、`cluster_repair.go:296-376`、`cluster_resume.go:42-44` | 节点与 Redis 间断连超过租约 TTL（进程存活、客户端全在线，memory broker 下无感知）→ 续租仅 Warn → 对等节点 membership SCAN 判死 → onLeave 无条件删除其全部会话租约 → 客户端重连时 `lease==nil` 按全新会话处理，服务端会话快照/订阅状态丢失。缓解：客户端 cursor + recover 仍可从 history 补发。修复：续租连续失败升级（指标+断开本机会话）；onLeave 删除前复核 owner（原子 CAS-DEL，同 #14）。 |

### P2 — 中严重度

| # | 发现 | 位置 | 说明 |
|---|------|------|------|
| 9 | [可靠性][中] gRPC GracefulStop 可被持流客户端拖延，关停固定多等 5s 并告警 | `pkg/transport/grpc/server.go:146-169` | transport.Close 不终结流；建议 GracefulStop + 兜底 `Stop` 定时器。 |
| 10 | [功能性][中] WriteTimeout=0 语义四传输不一致（WS 真禁用 / QUIC、KCP 静默回落 10s / gRPC 恒 ≥10s） | `ws/server.go:33-36`、`quic/server.go:66`、`kcp/server.go:68`、`grpc/transport.go:116` | 同一配置旋钮行为相反；见机制建议 M-2。 |
| 11 | [可靠性][中] RPC 代理用严格 protojson（无 DiscardUnknown），后端加任意字段即全量 RPC 失败 | `proxy/http.go:149-153` | 同文件其余 5 处均 `DiscardUnknown:true`。一行修复 + 回归测试。 |
| 12 | [可靠性][中] config.Validate 完全不覆盖 proxy 段（负 timeout 启动通过、运行期全挂） | `config/config.go`（Validate 无 `c.Proxy` 引用） | 补：timeout>0、endpoint 非空、http/grpc 互斥、name 唯一。 |
| 13 | [安全][中] admin gRPC 库路径鉴权 fail-open（空 token 且未声明 allow_insecure 时不装拦截器也不告警） | `internal/admin/admin_server.go:21-25` | Validate 挡住主程序，库调用方裸暴露；改为默认拒绝。 |
| 14 | [可靠性][中] DeleteSessionLease 读-删非原子，竞态窗口可删除跨节点接管后新 owner 的租约 | `pkg/redisbroker/cluster_directory.go:260-274` | 提供 Lua `CASDeleteSessionLease(sessionID, expectedOwner)`，三个调用点全部改用（与 #8 一并修）。 |
| 15 | [功能][中] 内存 broker 首次 Publish 与末次 Unsubscribe 并发：history 条目丢失且 offset 复用 | `internal/stream/broker_memory.go:288-295` | reclaim 落入 ring 创建窗口；同频道两条消息共用 offset 1。 |
| 16 | [功能][中] liveDesired 差分与入队非原子：并发订阅/退订乱序后频道实时投递静默失效（直至重连） | `pkg/redisbroker/redis.go:253-257` | ops 序列号化或 diff+enqueue 同临界区。 |
| 17 | [可靠性][中] pubsub 重连退避从不复位：累计断连后永久钉在 30s | `pkg/redisbroker/pubsub.go:427-446` | 成功存活超阈值后复位 backoff。 |
| 18 | [性能][中] 每次 Publish 固定追加 XINFO+SET 两次串行 RTT（发布路径 3-4 RTT） | `pkg/redisbroker/redis.go:424,441-463` | first-entry 维护折进 Lua 或按 trim 证据刷新。 |
| 19 | [可靠性][中] Fence 回滚窗口内并发 Close 被吞：会话复活到已死 transport 上 | `internal/session/session.go:367-433` | 引入"fencing 中"状态；回滚前探测 transport 存活。 |
| 20 | [可靠性][中] Close 的 presence 清理用无超时 Background ctx 同步阻塞 read loop | `internal/session/session.go:484-504` | 与同函数 5s proxy 超时不一致；统一短超时（见机制 M-4）。 |
| 21 | [安全][中] 未认证连接失败路径触发 OnDisconnected 且携带客户端注入的 session ID | `internal/session/client.go:322-327` | 认证前 staged ID 仅存局部变量；未完成认证跳过 disconnect 通知。 |
| 22 | [安全][中][待验证] 命令终态去重存储未签名、发送方去重命中不验签 | `pkg/redisbroker/cluster_command_bus.go:455-460,828-844` | Redis 写权限可将命令"抑制为已成功"；终态落盘前签名、命中后验签。 |
| 23 | [功能][中] RecoverComplete 的 Error/Truncated 字段两 SDK 静默丢弃 | `sdks/go/client.go:753-762`、`sdks/ts/src/client/client.ts:421-431` | 恢复失败对应用不可见，resume 以错误游标继续丢数据。 |
| 24 | [功能][中] TS SDK 非恢复型重连双重 recover 请求 + 客户端不去重 | `sdks/ts/src/client/client.ts:334-337,894-918` | 删除 resubscribeAllChannels（对齐 Go 靠 Connect.subscriptions）或按 (channel,offset) 去重。 |
| 25 | [功能][中] TS SDK 订阅登记客户端先行且不随 ack 收敛，被拒频道每次重连重试 | `sdks/ts/src/client/client.ts:1080-1085` | 以 SubscribeAck/Connected 回写为准（对齐 Go）。 |
| 26 | [可靠性][中] TS presence() 无超时，服务端丢回复时 Promise 永久挂起 | `sdks/ts/src/client/client.ts:1223-1241` | 套用 rpcTimeout。 |
| 27 | [安全][中][待复核] KCP 无握手/cookie 校验：任意 UDP 首包创建会话 + TLS 握手（资源放大 DoS 面） | `pkg/transport/kcp/server.go:113-122` | 补每源地址限速/并发握手上限，部署文档强制 UDP 防护。 |
| 28 | [性能][中] onOccupancy 单一全局 occMu 串行化全节点占用事件（与 hub 16384 分片设计不一致） | `internal/runtime/node.go:55,1357` | 按 channel 哈希分片。 |
| 29 | [可靠性][中] nextOccupancyGen 对 Redis INCR 用无超时 Background ctx | `internal/runtime/node.go:1320-1331` | 沿用调用方 ctx + 短超时。 |
| 30 | [性能][中] HTTP 后端客户端零值 Transport：每主机 2 条空闲连接、无 HTTP/2 | `proxy/http.go:92-96` | 调大 MaxIdleConnsPerHost + ForceAttemptHTTP2。 |
| 31 | [性能][中] localSurvey 每订阅者 2 个无上限 goroutine | `internal/runtime/node.go:837-845,1106-1110` | 复用 presence fan-out 的 64 并发上限。 |
| 32 | [性能][中] buildClientSurveyResult 每追加一答案全量 proto Marshal（O(N²)） | `internal/runtime/node.go:1016-1028,1075-1081` | 增量累计 `proto.Size(answer)`。 |
| 33 | [可靠性][中] UpdateFirstRetained 非原子：retained 标记 stale-low 抑制 HeadTrimmed 漏报 | `pkg/redisbroker/redis.go:424,441-464` [待验证] | 标记只许单向前进（Lua 比较旧值）。 |
| 34 | [可维护性][中] 传输层关键逻辑 3-4 份副本（heartbeatReadTimeout×3、断连信封×3、自签证书×2） | `ws/quic/kcp handler.go`、`grpc/quic/kcp transport.go` | 见机制 M-2；已实际漂移出 #10、超长帧反馈、close-code 三处不一致。 |
| 35 | [安全][中] 通配符匹配的具体频道在 lastOffsets/lastSeqs 永久累积（#7 家族实例） | `pkg/redisbroker/pubsub.go:532-543` | interested 为通配时跳过记录。 |

### P3 — 低严重度（择机处理，完整列表）

包括：Authorizer 未知 Action 默认放行（fail-open，closed set 下暂无可利用面）；Run 中 broker 启动错误可被 ctx.Done 吞掉；RemoveSubscription 回滚失败静默；AddSubscription cluster 步骤未加 clusterStepTimeout（与 Remove 不对称）；Gauge 命名 `*_total` 违反 Prometheus 惯例；finishRecovery 丢 RecoverComplete 发送错误；ChannelPolicy 每请求全表扫描；Publish 原地修改调用方 Publication（跨频道策略串扰）；onGap 串行 fan-out；isWildcard/index 跨包三份复制；同一 session id 并发双 connect Attach 失败即 Close 全会话 [待验证]；OnConnected 传 ClientId 其余传 userID 的语义不一致；DrainAll 每会话一个 goroutine 无上限；Connect 路径 presence 快照串行逐频道；QUIC `MaxIncomingUniStreams:-1` 实为 0；内部错误文本下发客户端；KCP 会话 ctx 不随连接取消；KCP Close 固定 sleep 100ms；读循环错误分类/日志级别四传输不一；`Disconnect{}`(code 0) 收尾各传输表现不一；3510 码值未定义但注释宣称覆盖；BroadcastCommand SCAN 后逐 key GET（N+1）且不滤过期租约；命令 claim 续租与终态写入竞态（终态 TTL 可被缩回 30s）；非法 JSON 命令信封静默丢弃；sim Bus 未实现 exclude_self；ExpiresAt 零值租约永生；CAS 失败用错误字符串匹配；Survey 本地/远端串行各等满超时；同 node_id 双进程无检测 [文档化接受风险]；Cluster Shutdown 后 Start 假成功；命令 stream key 永不清理（#7 家族实例）；seq 计数器被逐出后向后跳变无告警 [待验证]；head-trim 误报 Middle；per-publication HistorySize>全局 maxLen 时每次重连假阳性 ReplayTruncated；PublishTransient 两实现错误语义不一致；内存 broker PublishTransient 无 ring 竞窗；applyLiveOp 断连虚报已确认；内存 broker Start 无防重入；`stored := *pub` 浅拷贝共享 Metadata map；broker 层不强制 ns: 语法（纵深防御）；redis client 不支持 ACL Username/TLS；options.go 非法时长静默忽略 + StreamApproximate 死分支；`stream` 局部变量遮蔽包名；chShard 每次分配 fnv 哈希器；两 broker 可观测性不对齐（memory 零计数器、redis 三计数器未接 Prometheus）；isLoopbackAddr 不解析主机名（"localhost" 误告警）；QUIC/KCP 构造失败时 gRPC listener 泄漏；Survey 无 capability 位且 bypass_gate 在默认集；grpc_admin 无 TLS 时 token 明文无告警；两套通配符方言并存（proxy routes glob 跨段 vs authorizer 单段）；CapabilityNames 手工双份维护；CSTrie 热路径分配偏多；Go SDK Connect 超时引爆后台重连；Go SDK 断连码注释缺 3514；仓库根目录 `380` 空文件未忽略（`nul`/`*.exe` 已忽略）。

## 5. 机制层建议（同类问题多处出现，补机制而非逐点修）

- **M-1 实体生命周期清理钩子**：lastApplied / gens / occGens / 用户索引 set / 通配 lastOffsets / 命令 stream key 六处同根因——实体（会话、频道、节点 incarnation）消亡时没有统一清理点。建议：会话 Close 与退订路径同步删除 `(ch,sid)` 状态；ClusterRepairer 周期循环扩展为对账者（SMEMBERS 差集 SREM、死 incarnation stream DEL），把"只增不减"类问题一次收口。
- **M-2 传输共享内核**：把 `heartbeatReadTimeout`、断连信封构造、自签证书、（以及建议新增的）写超时语义、超长帧反馈、错误分类下沉到 `pkg/transport/internal` 共享包。四传输已漂移出三处行为不一致，复制式同步是该族问题的根因。
- **M-3 SDK 断连码→重连策略状态机**：两个 SDK 各自实现一套"一切断连皆可重试"。建议在两侧统一实现：`DisconnectError{Code,Reason}` 为一等公民，重连决策由码值驱动（3503/3514 停止上报；3500 降级新会话；其余指数退避），TS 补齐 close reason 读取。
- **M-4 配置单点归一化**：`ParseDuration` 在 config / cmd/server / NewNode / redisbroker options 四层共 14 处各自解析且失败策略不一（Validate 拒绝 / 静默回退 / 静默默认）。建议 config 层一次性归一化为 `time.Duration` 字段，下游只读；Validate 补齐 proxy 段覆盖。
- **M-5 统一短超时 ctx 策略**：Close presence 清理、nextOccupancyGen、onGap 等路径散布无 deadline 的 `context.Background()`。建议封装 `storeCtx(parent)` 之类的短超时助手并作为存储层调用约定。
- **M-6 传输层指标补齐**：四个传输零指标（握手失败/写超时/frame-too-large 仅日志），memory broker 零计数器、redis broker 三计数器未接 Prometheus。复用既有 `SetMetrics` 注入路径收口。

## 6. 建议行动顺序

1. **当天可完成**（每项 ≤ 半天，含回归测试）：#1 死锁、#3 delegate、#4 SubRefresh namespace、#11 protojson、#17 退避复位。
2. **本周**：#2 解压限制、#5+#6 SDK 重连状态机（M-3）、#12 Validate proxy 段、#13 admin fail-open、#8+#14 租约原子化与升级（集群模式部署者优先）。
3. **两周内**：#7 无界状态族（M-1 机制修复）、#34 传输内核下沉（M-2）、#9、#15、#16、#19、#21。
4. **排期**：P2 其余 + M-4/M-5/M-6 机制项；P3 择机。
5. **流程建议**：P0 五项各补一个能复现原缺陷的回归测试（delegate/死锁两项目前零路径覆盖）；为"无界 map"类问题加一条 Prometheus 内存/条目数指标，防再发。

## 7. 横切评审结论（主会话）

- **namespace 隔离**：端到端覆盖良好（连接时解析、五处入口守卫、admin 强制、跨 ns resume 拒绝 3500、broker 键带前缀），唯一缺口即 #4（SubRefresh）；broker 层不强制 `ns:` 属纵深防御建议。
- **resume/recover 跨层流**：本地 takeover → 集群 CAS → snapshot → saga 的设计自洽；弱点集中在租约生命周期（#8、#14、#19）而非数据路径；客户端 cursor 机制提供了最后一道兜底。
- **配置一致性**：文档（config-example.yaml 注释）与实现高度同步（此前评审已修复漂移）；遗留问题是解析责任分散（M-4）与 `*_total` Gauge 命名。
- **仓库卫生**：`nul`、`example.exe`、`server.exe`、`sdks/go/example.exe` 已被 gitignore；`380`（0 字节）为唯一未忽略杂物，可删除。
- **SDK 协议面**：帧编码（4 字节大端长度前缀、WS 子协议、protojson `UseProtoNames`、gRPC RawCodec 名）与 resume/gap 游标语义逐字节/逐字段一致，服务端 Ping→Pong 已正确实现——协议本体健康，缺口在断连码消费端（M-3）。
