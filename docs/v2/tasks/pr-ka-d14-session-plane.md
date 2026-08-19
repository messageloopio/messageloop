# PR-KA-D14 实现规格:KD-K26 阶段三(b)——session plane 下沉 internal/session + Session.node 依赖反转

| 字段 | 值 |
| --- | --- |
| 标题 | `refactor: sink session plane into internal/session, invert Session→Node behind Runtime` |
| 状态 | **Ready** |
| 依赖 | D13 已合(`a2aa7a2`)。在 `v2` 分支上做 |
| 设计来源 | [kernel-architecture.md](../kernel-architecture.md) :150-191(Session Plane、包图、硬规则)、KD-K26;D13 规格 §7 阶段图 |
| 验收人 | 主 agent |

## 1. 目标

KD-K26 阶段三第二刀(最重的一刀),三件事:

- **(a) session plane 下沉**:`session.go`、`client.go`、`heartbeat.go`、`hub.go`、`pool.go`、`transport.go` 六文件 `git mv` → 新建 **`internal/session`**;`survey.go` `git mv` → 新建 **`internal/survey`**(纯叶包,为 `Runtime.Survey` 的签名让路,§3.5)。
- **(b) `Session.node` 依赖反转**:Session 的 `node *Node` 字段换成 `rt Runtime`(`Runtime` 接口定义在 internal/session,§3.2 全文);根包新增 `session_runtime.go` 适配器(`nodeRuntime` 以导出方法委托 Node 的未导出方法,**Node 本体零导出手术**)与 `NewClient` 薄包装(签名不变,§3.4)。
- **(c) alias 过渡延续**:根 `aliases.go` 加 `--- internal/session ---` / `--- internal/survey ---` 两组转发;transports、cmd/server、internal/admin、sim、redisbroker、留根测试**零改动**。

**不做**:Node 本体、cluster 编排(Node 方法群)、recover、health、defaults、marshaler、subscription_saga 留根(D15);Sim 钩子(cluster_sim.go)留根——它包装的是 Node 未导出方法,只能随 Node 走(D13 阶段图原记在 D14,本规格更正为 D15);`Client = Session` 别名随 session.go 迁,根包自留一条同形 alias。

### 1.1 零变化红线

行为零变化;常量/码表值/JSON tag 不变;除 §3.3 列明的导出手术与 §4 列明的机械改写外,符号名字、签名、语义逐字节等价。**锁序不变量**(现状,迁移后必须逐字保持):订阅 saga 以 `subLock(ch)`(node.go 分片锁)在外 → Hub 分片锁 → `Session.mu` 在最内;`TrackChannel`/`UntrackChannel`/`ForceTrackChannel`(§3.3)封装的是最内层 `Session.mu`,外层锁序不变。proto、`shared/`、`sdks/`、`config/`、`pkg/topics`、`pkg/transport/`、`internal/{protocol,channel,occupancy,stream,authz,admin,cluster,metrics}`、`proxy/`、`cmd/server`、`pkg/redisbroker` 零改动。

### 1.2 迁移文件的直引规则

迁入 internal/session 的生产代码**直引** internal/* 与 shared/proxy/config,不准经根 alias:`protocol.Disconnect*`、`channel.ChannelPolicy`/`DefaultChannelPolicy`、`authz.*`、`occupancy.*`、`stream.Publication*`、`cluster.ErrSessionFenced`/`cluster.ClusterSessionSnapshot`/`cluster.ClusterSubscriptionSnapshot`、`metrics.Metrics`、`shared.Marshaler`/`shared.ProtoJSONMarshaler` 等。三个未导出包装触点按 §3.5 处理。

## 2. 允许改动的文件

- git mv → `internal/session/`:`session.go`、`client.go`、`heartbeat.go`、`hub.go`、`pool.go`、`transport.go`;测试随迁:`session_test.go`、`hub_test.go`(§3.6)。
- git mv → `internal/survey/`:`survey.go`。
- 新建:`internal/session/runtime.go`(`Runtime` 接口 + `RestoreFailure` + 包注释,可拆 doc.go);`internal/session/fake_runtime_test.go`(§3.6 授权的测试桩);根 `session_runtime.go`(`nodeRuntime` 适配器 + 编译期断言 + `NewClient` 薄包装,§3.4)。
- 修改(仅新增):根 `aliases.go`(session/survey 两组 + `newHub` var 转发,§3.7)。
- 机械改写(§4 规则,逻辑零变化):`node.go`(5 个 Hub 方法名 + 3 处 presence 触点换 `ConnectedAt()`)、`cluster_state.go`(identity 快照块)、`cluster_commands.go`(:299-301)、`cluster_resume.go`(:127-144 AdoptIdentity、:206-208/:261/:283 track/untrack、:247/:253/:278 Hub 方法名)、`recover_test.go`(:724-726)、`presence_test.go`(:499/:507)。
- 文档路径引用更新:`AGENTS.md`、`CLAUDE.md`、`.github/copilot-instructions.md`、`docs/developer/01-architecture.md`。
- `docs/v2/README.md` 状态行、本规格 §9。

## 3. 现状(主 agent 已核实;行号漂移以语义为准)

### 3.1 搬运集(7 枚 git mv)

| 文件 | 行数 | 内容 | 外部耦合 |
| --- | --- | --- | --- |
| session.go | 775 | SessionState、Attachment、Session、sendQueue、Attach/Detach/Fence/Close、writerLoop | `clusterEvictRollbackTimeout` 常量在 cluster_resume.go:17 定义、唯一使用点在 session.go:386——**常量随迁**(留根文件无引用,已核实);`isPeerClosedError` 引 gorilla/websocket + grpc codes,随行 |
| client.go | 1730 | NewClient、verb 处理器群(handleConnect/handleRPC/handlePublish/handleSubscribe/handleUnsubscribe/handlePing/Pong/handleSubRefresh/handleSurvey/handleSurveyReply/handlePresenceQuery)、MakeOutboundMessage、MarshalJSONStruct、ClientInfo | `protocolGenerationOK`(client.go:261)改直引 `protocol.GenerationOK`;`SystemMethodAuthenticate`(:234)随迁,根 alias 转发(§3.5) |
| heartbeat.go | 173 | HeartbeatConfig、HeartbeatManager、armPingDeadline、disconnectHeartbeatTimeout | Node 触点仅 `c.node.metrics.HeartbeatIdleDisconnects`(:166)→ `rt.Metrics()` |
| hub.go | 801 | Hub、Subscriber、ChannelInfo、broadcastPublication、注册表 | `client.node.metrics` 4 处(:400-435)→ `client.rt.Metrics()`;6 项导出手术(§3.3) |
| pool.go | 13 | bytesPool | 仅 session.go/session_test.go 用,随迁 |
| transport.go | 8 | Transport 接口 | Disconnect 改直引 internal/protocol |
| survey.go | 191 | SurveyResult、Survey、NewSurvey | import 仅 context/sync/time,零根包依赖,整件迁 |

### 3.2 Runtime 接口全文(internal/session/runtime.go)

```go
// Runtime 是 Session 对节点编排层的依赖缝(KD-K26 阶段三(b),PR-KA-D14)。
// 访问器每次调用时读取,容忍 SetMetrics 等晚期注入;编排方法由根包
// nodeRuntime 适配器委托到 *Node(含未导出方法)。
type Runtime interface {
	// 装配访问器
	Hub() *Hub
	Metrics() *metrics.Metrics
	Presence() occupancy.PresenceStore
	Authorizer() *authz.Authorizer
	Limits() config.Limits
	RequireAuth() bool
	Heartbeat() *HeartbeatManager

	// 连接与订阅编排
	AddClient(c *Session) error
	AddSubscription(ctx context.Context, ch string, sub Subscriber) error
	RemoveSubscription(ch string, c *Session) error

	// 发布与频道策略
	Publish(ch string, pub *stream.Publication) (uint64, error)
	PublishTransient(ch string, pub *stream.Publication) error
	ChannelPolicy(ch string) channel.ChannelPolicy
	MaxMessageSize() int

	// 身份
	UserPrincipal(userID string) authz.Principal

	// presence 编排
	ShouldTrackPresence(ch string, ephemeral bool) bool
	PresenceJoin(ctx context.Context, ch string, c *Session)
	PresenceLeave(ctx context.Context, ch, sessionID, userID string, ephemeral bool)
	PresenceSnapshot(ctx context.Context, ch string) *clientpb.PresenceSnapshot

	// survey 编排
	Survey(ctx context.Context, channel string, payload []byte, timeout time.Duration) ([]*survey.SurveyResult, error)
	AddSurveyResponse(ctx context.Context, sessionID, requestID string, payload []byte, err error)
	CountMatchingSubscribers(ctx context.Context, ch string) (int, error)
	BuildClientSurveyResult(requestID, channel string, results []*survey.SurveyResult) *clientpb.SurveyResult

	// proxy
	FindProxy(channel, method string) proxy.Proxy
	ProxyRPC(ctx context.Context, channel, method string, req *proxy.RPCProxyRequest) (*proxy.RPCProxyResponse, error)
	GetRPCTimeout() time.Duration

	// cluster 编排
	SyncClusterSessionState(ctx context.Context, c *Session) error
	DeleteClusterSessionState(ctx context.Context, sessionID string) error
	AdjustClusterChannelSubscriptionsTimeout(channel string, delta int64)
	ResumeRemoteSession(ctx context.Context, c *Session, sessionID string) (*cluster.ClusterSessionSnapshot, bool, error)
	RestoreSessionSubscriptions(ctx context.Context, c *Session, subs []cluster.ClusterSubscriptionSnapshot) []RestoreFailure
	RestoreLocalSubscription(ctx context.Context, ch string, sub Subscriber) error
	RemoveLocalSubscriptionOnly(ch string, s *Session, updateMetrics bool) (bool, error)

	// recovery 编排
	StreamEpoch() string
	RecoverState(c *Session, subs []*clientpb.Subscription, snapshot *cluster.ClusterSessionSnapshot) clientpb.RecoverState
	StreamRecoveries(ctx context.Context, c *Session, in *clientpb.InboundMessage, subs []*clientpb.Subscription, snapshot *cluster.ClusterSessionSnapshot, path string)
}

// RestoreFailure 是一频道订阅恢复失败的结果(映射根包未导出的
// clusterRestoreFailure,跨包接口不能暴露未导出类型)。
type RestoreFailure struct {
	Channel string
	Err     error
}
```

方法 ↔ Node 实现点对照(适配器逐条委托):AddClient node.go:378 / AddSubscription :393 / RemoveSubscription :495 / Publish :617 / PublishTransient :646 / ChannelPolicy :318 / MaxMessageSize :772 / UserPrincipal←userPrincipal :323 / ShouldTrackPresence←shouldTrackPresence :1212 / PresenceJoin←presenceJoin :1222 / PresenceLeave←presenceLeave :1261 / PresenceSnapshot←presenceSnapshot :1528 / Survey :780 / AddSurveyResponse :1147 / CountMatchingSubscribers←countMatchingSubscribers :961 / BuildClientSurveyResult←buildClientSurveyResult :1014 / FindProxy :688 / ProxyRPC :704 / GetRPCTimeout :713 / SyncClusterSessionState←syncClusterSessionState cluster_state.go:182 / DeleteClusterSessionState←deleteClusterSessionState :267 / AdjustClusterChannelSubscriptionsTimeout←adjustClusterChannelSubscriptionsTimeout cluster_resume.go:25 / ResumeRemoteSession←resumeRemoteSession :33 / RestoreSessionSubscriptions←restoreSessionSubscriptions :197(+ RestoreFailure 映射)/ RestoreLocalSubscription←restoreLocalSubscription :238 / RemoveLocalSubscriptionOnly←removeLocalSubscriptionOnly :273 / StreamEpoch←streamEpoch recover.go:92 / RecoverState←recoverState :113 / StreamRecoveries←streamRecoveries :138。

### 3.3 导出手术清单(全授权集,除此无他)

Session 侧 7 项(封死根包对 Session 未导出成员的全部触点,逐触点已核实):

1. `ConnectedAt() time.Time` —— node.go:291、:1247、:1277(presence info 构造)。
2. `SnapshotIdentity() IdentitySnapshot` —— cluster_state.go:302-336(lease/snapshot 构造的读锁区)、cluster_commands.go:299-301。新导出类型 `IdentitySnapshot{SessionID, UserID, ClientID, Protocol string; Authenticated bool; ConnectedAt, LastActivity time.Time; LeaseVersion uint64}`,RLock 快照。
3. `SubscribedChannels() []string` —— node.go:359-360、cluster_state.go:326-327。RLock 拷贝。
4. `TrackChannel(ch string) (closed bool)` —— mu.Lock;state==SessionClosed 时不写、报 true;否则写 subscribedChannels。对应 node.go:419-435(track saga 步,含 closed 中止)。
5. `ForceTrackChannel(ch string)` —— mu.Lock 无条件写。对应 cluster_resume.go:261、node.go:528-531(rollback 重挂)。
6. `UntrackChannel(ch string)` —— mu.Lock 删。对应 node.go:523-526、cluster_resume.go:206-208、:283。
7. `AdoptIdentity(sessionID, userID, clientID string, subscriptions []string, leaseVersion uint64)` —— mu.Lock 下换 ID 三元组 + 重建 subscribedChannels + 写 clusterLeaseVersion。对应 cluster_resume.go:127-144 整段(resume 接管;最敏感写操作,验收用例 = client_fix_test.go:652/:713 的 resume 场景)。

Hub 侧 6 项(未导出方法被留根 Node/测试调用):`newHub` → `NewHub`(:60)、`add` → `Add`(:379 调用点)、`addSub` → `AddSub`(node.go:409、cluster_resume.go:247)、`removeSub` → `RemoveSub`(node.go:413/:510/:517、cluster_resume.go:253/:278)、`broadcastPublication` → `BroadcastPublication`(node.go:202、recover_test.go:724-726、presence_test.go:499/:507)、`presenceRecipients` → `PresenceRecipients`(node.go:1405)。根 `aliases.go` 放 `var newHub = session.NewHub` 未导出转发,node.go:80 调用点零改动;其余方法名在调用点机械改名。

### 3.4 适配器与薄包装(根 `session_runtime.go`,新文件)

- `type nodeRuntime struct{ n *Node }` + 37 个导出方法逐条委托 §3.2 对照表;`RestoreSessionSubscriptions` 做 `[]clusterRestoreFailure` → `[]session.RestoreFailure` 逐条映射(字段 channel/err → Channel/Err)。
- 编译期断言 `var _ session.Runtime = nodeRuntime{}`(或 `(*nodeRuntime)(nil)`,以实现形态为准)。
- `NewClient` 薄包装保持旧签名逐字不变:`func NewClient(ctx context.Context, node *Node, t Transport, marshaler Marshaler, opts ...ClientOption) (*Session, ClientCloseFunc, error)`,体内一行 `return session.NewClient(ctx, nodeRuntime{node}, t, marshaler, opts...)`。transports 三处调用点(ws/handler.go:53、grpc/handler.go:27、quic/handler.go:44)零改动。
- internal/session 的 `NewClient(ctx, rt Runtime, t Transport, m shared.Marshaler, opts ...ClientOption)`:函数体除 `node` 换 `rt`、心跳启动改 `rt.Heartbeat()`(nil 守卫等价)外逐字节不变(client.go:25-66 已逐行核实)。

### 3.5 杂项迁徙

- `clusterEvictRollbackTimeout`(cluster_resume.go:17 定义、唯一使用 session.go:386):**常量随迁** internal/session(保持未导出),cluster_resume.go 删定义。
- `SystemMethodAuthenticate`(client.go:234)随迁;根 alias `const SystemMethodAuthenticate = session.SystemMethodAuthenticate`,引用它的 5 个测试文件零改动。
- `protocolGenerationOK`:迁入文件改直引 `protocol.GenerationOK`(client.go:261 一处);aliases.go 既有包装保留(version_test.go:105/:108 留根引用)。
- `internal/survey`:整件迁;node.go 的 survey 编排(NewSurvey/AddExpectedSession/Wait/Close/Payload/ID/Channel/AddResponse/IsExpectedSession 与 `surveys map[string]*Survey` 字段)留根,经 alias 零改动。

### 3.6 测试面

- **必迁(未导出符号耦合,唯二)**:`session_test.go`(:146-147 `sess.out.tryEnqueue`、:177-187 getBuffer/putBuffer/queuedFrame、:439/:444 outboundFrameClass)与 `hub_test.go`(20+ 处 `newHub`)随迁为 internal/session 包内测试。
- **授权新建 `internal/session/fake_runtime_test.go`**:`fakeRuntime` 实现 Runtime,默认安全 no-op(FindProxy 返 nil、Metrics 返真实 `metrics.NewMetrics(prometheus.NewRegistry())`、ChannelPolicy 返 `channel.DefaultChannelPolicy()`、MaxMessageSize 返 0 或测试注入值、编排方法返 nil),被测路径需要的字段可注入。随迁测试的 8 处 `NewClient(ctx, node, ...)` 构造改写为 fakeRuntime 装配,**断言逐字不变**;hub_test 的 1 处 NewNode/NewClient 用例同理适配。
- **留根零改动**:其余全部测试文件(经根 alias + NewClient 薄包装;已核实 client_test/client_fix/presence/recover/rpc_timeout/survey/node/cluster_*/heartbeat/channel_policy/gap_notice/occupancy/testhelpers 对 session 未导出符号零命中)。唯二例外是 §3.3 授权的机械改名:recover_test.go:724-726、presence_test.go:499/:507 的 `.broadcastPublication(` → `.BroadcastPublication(`。
- 测试函数总数前后一致(session_test 8+ 个、hub_test 20+ 个全数随迁),完成报告给计数表。

### 3.7 alias 新增(aliases.go 仅授权的新增)

`--- internal/session ---` 组:类型 alias(`Session`、`Client`、`SessionState`、`Attachment`、`Subscriber`、`Hub`、`ChannelInfo`、`Transport`、`HeartbeatManager`、`HeartbeatConfig`、`ClientOption`、`ClientCloseFunc`、`ClientInfo`、`RestoreFailure`、`Runtime`、`IdentitySnapshot`);const(`SessionAuthenticating`/`SessionAttached`/`SessionDetached`/`SessionClosed`、`SystemMethodAuthenticate`);var(`NewSubscriber`、`NewHeartbeatManager`、`WithProtocol`、`MakeOutboundMessage`、`MarshalJSONStruct`、`ErrSendQueueFull`、`ErrSessionNotAttached`、`ErrOutboundTooLarge`、`newHub` 未导出转发)。`--- internal/survey ---` 组:`Survey`、`SurveyResult` type alias + `NewSurvey` var。**NewClient 不进 aliases.go**(薄包装在 session_runtime.go,§3.4)。

### 3.8 遮蔽与命名

- internal/session 的 Runtime 接口方法 `ChannelPolicy(...)` 与返回类型 `channel.ChannelPolicy` 不冲突(类型经包限定);文件内 import `internal/channel` 若与形参名 `channel string` 冲突,用 `channelpkg` 别名(D12 先例),全文件统一。
- internal/session 若 import `internal/survey` 与接口方法 `Survey(...)` 同名:方法名不占包标识符,合法;如 gofmt/编译器报冲突再议(预期无)。

## 4. 组织原则

1. 7 枚整文件 `git mv`;迁入文件的允许改动仅:package/import 行、`s.node.`/`c.node.` 重写(§4.2)、§3.5 三处、`clusterEvictRollbackTimeout` 定义并入;其余逐字节不变。
2. 重写映射(机械、全覆盖):
   - `s.node.hub.X(` → `s.rt.Hub().X(`;`s.node.metrics.X` → `s.rt.Metrics().X`(nil 守卫形态不变);`s.node.presence.X(` → `s.rt.Presence().X(`;`s.node.authorizer.X(` → `s.rt.Authorizer().X(`;`s.node.limits.X` → `s.rt.Limits().X`;`s.node.requireAuth` → `s.rt.RequireAuth()`;`s.node.heartbeatManager` → `s.rt.Heartbeat()`;`s.node == nil` → `s.rt == nil`。
   - `s.node.<method>(` → `s.rt.<Method>(`(未导出名按 §3.2 对照表首字母大写)。
3. 根包改动仅限 §2 列明;Node 本体零导出手术;新位置代码(internal/session、internal/survey)零根包引用;适配器/薄包装是根包新代码,直引 internal/session(aliases.go:11 既定方针)。
4. 锁序不变量见 §1.1;saga 步内代码除 Session 触点换 §3.3 方法外逐字节不变。

## 5. 验证命令

```bash
go build ./...
go test -count=1 -run "TestSim_|TestCluster|TestResume|TestClientFix|TestSession" .
go test -count=1 ./pkg/redisbroker ./internal/... ./pkg/transport/...
go test -count=1 ./...
cd sdks/go && go test -count=1 .
cd sdks/ts && npx jest
cd _examples/chatroom && go build ./...
golangci-lint run ./...
```

门禁:

```bash
# 新位置零根包引用
grep -rn '"github.com/messageloopio/messageloop"' internal/session internal/survey --include="*.go"   # 应为空
# 编译期接口断言存在
grep -n "session.Runtime" session_runtime.go   # 应有 var _ 断言
# 红线目录零改动
git diff --name-only -- shared sdks _examples config pkg/topics pkg/transport internal/admin internal/cluster internal/metrics internal/protocol internal/channel internal/occupancy internal/stream internal/authz proxy cmd/server pkg/redisbroker   # 应为空
# 根包生产文件改动面白名单
git diff --name-only -- '*.go' ':!*_test.go'   # 应只命中:六个迁移源文件、node.go、cluster_state.go、cluster_commands.go、cluster_resume.go、aliases.go、session_runtime.go(新增)、internal/session、internal/survey
```

串行跑测试,绝不并发两个根目录 `go test`;Redis 真实实例(127.0.0.1:6379,DB 14)。已知 flake:`go test ./...` 包间并发时 redisbroker 的 DB14 FlushDB 可打到根包 cluster 原子写测试(backlog #8 已记);单包复跑确认非回归即可。

## 6. 验收清单

1. 三件事全做完;7 枚 `git mv` R 识别;迁入内容除 §4 授权改写外逐字节不变。
2. 导出手术仅限 §3.3 的 13 项(Session 7 + Hub 6);`Runtime` 接口成员与 §3.2 全文一致;Node 零导出手术;`RestoreFailure` 映射字段逐条对应。
3. 锁序不变量保持(§1.1);`AdoptIdentity`/`TrackChannel` 语义与原版逐分支等价,resume 场景测试(client_fix_test.go:652/:713 一带)绿。
4. 门禁四条符合预期;transports、cmd/server、redisbroker、sim、internal/admin 零改动;留根测试除 5 行机械改名外零改动。
5. 测试拆分与 §3.6 一致,总数前后一致;fakeRuntime 仅为测试文件。
6. 全链验证 + lint 0 issues 全绿;文档四处路径引用更新;未碰红线;无 churn、.go 保持 CRLF、无 git 操作。

## 7. 阶段图(更新)

- **D11(已合)**:叶子契约下沉 + 根 alias 过渡。
- **D12(已合)**:authz/channel 下沉;transport 改名;admin 剥离。
- **D13(已合)**:cluster 契约 + epoch + SyncUserIndex 下沉 internal/cluster;metrics 下沉 internal/metrics。
- **D14(本 PR)**:session plane 下沉 internal/session;`Session.node` → `rt Runtime` 依赖反转;survey.go 下沉 internal/survey。
- **D15**:Node 本体 + cluster Node 方法 + Cluster 门面/lease manager + recover + health → `internal/runtime`;survey 编排(node.go 内)归 internal/survey;Sim 钩子归位(更正:D13 阶段图原记在 D14,钩子包装 Node 未导出方法,只能随 Node 走);`recover.go` 与 internal/stream 合流与否、`internal/rpc` 用途、defaults.go/marshaler.go 归位在 D15 规格定;清除根 alias;根包退出或只剩空壳。

## 8. 完成报告

- 改动文件列表(git mv 映射 / 新增 / 机械改写分组)
- §6 每条 过/失败 + 证据
- 导出手术逐符号清单(名字前→后)+ Runtime 接口实现对照
- 测试迁移/适配逐个判定表 + 前后总数
- §5 命令真实输出(含四条门禁)
- 偏离(应无)

## 9. 实现备注(实现方填)

实现于 2026-08-19,v2 分支,基于 `bc02723`(D14 规格已合;D13 tip `a2aa7a2` 的后继)。验证全绿(`go build ./...`、`go test -count=1 ./...`、SDK/TS/chatroom、`golangci-lint run ./...` 0 issues),四条门禁符合预期,测试函数总数前后一致(session 9 + hub 41 = 50,全数随迁)。

**被迫偏离(需验收人知悉)**:§3.6「留根测试对 session 未导出符号零命中」不实。Session/Hub 下沉后,留根测试与 `node.go` 仍跨包点名了一批未导出方法/字段/类型,Go 不允许在 alias 类型上补方法。按 D13 `normalize` 先例做最小补集,否则无法编译:

1. **Hub 返回类型**:`presenceRecipient` 被 `node.go` 点名字段 `client`/`ephemeral`。导出为 `PresenceRecipient{Client, Ephemeral}`(aliases.go 多一条 type alias,§3.7 原文未列)。`broadcastParallelLimit`/`index`/`isWildcard`/`publicationID`/`pingClusterRefreshInterval` 在根 `session_runtime.go` 留未导出副本(D11 叶子副本先例),`node.go`/`recover.go` 调用点零改名。
2. **`positionFrom`**:定义在留根 `recover.go`,迁入 hub.go/client.go 不能引根。`internal/session/runtime.go` 放逐字节副本。
3. **`defaultSurveyWaitTimeout`**:Go 不能跨包 alias 可变 var。导出 `survey.DefaultSurveyWaitTimeout`;`survey_test.go` 3 行改 `intsurvey.DefaultSurveyWaitTimeout`(局部变量名 `survey` 占包名,import 用 `intsurvey`)。
4. **留根测试跨包触点**(§3.6 漏检):Session 增导出包装 `HasSubscription`/`SubscriptionList`/`Attachment`/`MarkAuthenticated`/`SetUserIDForTest`/`SetClientIDForTest`/`Marshal`/`HandleRPC`/`HandleUnsubscribe`/`ThrottledClusterRefresh`/`SetLastClusterSyncNanoForTest`;Hub 增 `Sessions()`(兼 ReplaceRules 读 `hub.sessions`);HeartbeatManager 增 `SetJitterForTest`。对应测试只改构造/限定名,断言语义不变。`hub_test.go` 同包方法调用随导出手术 `h.add`→`h.Add` 等。
5. **源码扫描**:`occupancy_test.go` `readSource("hub.go")` → `internal/session/hub.go`(文件已搬走,否则 open 失败)。
6. **`newTestClient`**:随 `hub_test.go` 迁走;根 `testhelpers_test.go` 补同名构造,`cluster_user_index_test.go` 零改调用点。
7. **session_test 3 个重测试**(AttachFailure/Fence/Close):迁入后不能 `NewNode`/`NewCluster`。构造改为 `newFakeRuntime()`(字段 `hub`/`presence`/`deletedLease`/`deletedSnapshot` 保持断言 `node.hub`/`directory.deletedLease` 逐字可读);`presenceJoin` 未导出包装保留调用点。

其余按规格执行:

- 9 枚 `git mv` 均被 git 识别为 `R`(6 生产 + survey + 2 测试)。
- `Runtime` 接口成员与 §3.2 全文一致;`var _ session.Runtime = nodeRuntime{}`;`RestoreFailure` 逐条 `channel/err` → `Channel/Err`。
- Session 授权 7 项 + Hub 授权 6 项导出手术到位;`newHub` 根 var 转发,`NewNode` 调用点零改。
- 锁序不变量未动:saga 仍 `subLock` 在外,`TrackChannel`/`UntrackChannel`/`ForceTrackChannel`/`AdoptIdentity` 只封最内 `Session.mu`。
- 新位置零根包引用;红线目录零改动;`.go` 全 CRLF;无 commit/tag/push。
