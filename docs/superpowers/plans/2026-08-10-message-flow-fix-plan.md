# 消息数据流修复方案(Message Flow Fix Plan)— 2026-08-10

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.
>
> **强制执行顺序: Task 0(全面复核)未通过前,禁止修改任何代码。**

基于两轮深度代码评审(6 域并行审查 + 高危结论逐行核对 + 复核与查漏补缺)产生的修复清单。**本文档是修复实施的唯一依据**;每个修复条目都标注了证据位置(file:line),Task 0 要求执行者逐条复核这些证据后再动手。

- 评审日期: 2026-08-09 ~ 2026-08-10
- 基线: `main`(以执行时 `git rev-parse HEAD` 为准并记录于此)
- 修复原则: 行为最小变更、不引入新依赖、每个修复附带能复现原问题的测试、单机模式行为优先保持不变

**Goal:** 修复消息数据流断点(集群通配符失效、历史恢复漏消息、epoch 跨节点不一致、TS JSON codec 损坏)与转换不一致(Payload 类型坍塌、字段丢失),消除入口与集群模式的安全/可靠性隐患。

**Architecture:** 修复按"Redis broker 集群链路 → 入口安全 → 会话一致性 → Publication 模型扩展"分层推进;除 Task 12(Publication 模型)为破坏性接口变更外,其余均为局部改动,接口与 wire format 保持兼容。

**Tech Stack:** Go 1.x + go-redis/v9 + gorilla/websocket + gRPC + protobuf(protojson);TS SDK 用 @bufbuild/protobuf v2。

## Global Constraints

- 所有 Go 改动通过: `go build ./...`、`go vet ./...`、`go test -race ./...`。
- Redis 集成测试通过环境变量 `MESSAGELOOP_TEST_REDIS_ADDR` 指定;无 Redis 时相关测试 `t.Skipf` 跳过(不算通过,也不算失败)。
- TS SDK 改动通过: `cd sdks/ts && npm test`(jest)。
- proto 变更后必须运行 `task generate-protocol` 重新生成 `shared/genproto/`,并提交生成产物。
- 不改动 `Broker.History` 接口签名(`broker.go:49` 语义为 `offset >= sinceOffset`,所有实现向其对齐)。
- 行号引用以评审基线为准;若代码已漂移,以符号/函数名定位并在 Task 0 复核报告中记录。
- 每个 Task 完成后独立提交(commit message 用英文,遵循 conventional commits)。

## 验证命令(全量收尾必须全绿)

```bash
go build ./...
go vet ./...
go test -race ./...
MESSAGELOOP_TEST_REDIS_ADDR=127.0.0.1:6379 go test -race ./...   # 有 Redis 时
cd sdks/ts && npm test
cd sdks/go && go test ./...
```

---

## Task 0: 全面复核(强制,只读)

**本任务不修改任何代码。** 目的是在执行前确认方案中每条证据与结论在当前代码基线上仍然成立。任何一条被推翻,必须先修订本文档再继续。

**Files:**
- Read: 下列复核清单中引用的全部文件
- Create: `docs/superpowers/plans/2026-08-10-message-flow-fix-plan-review.md`(复核报告)

- [ ] **Step 1: 建立基线**

```bash
git rev-parse HEAD
go build ./... && go vet ./... && go test ./...
cd sdks/ts && npm test
```

预期: 全绿。若有已存在的失败,记录在复核报告中,作为修复前后的对照。

- [ ] **Step 2: 逐条复核下列证据清单**

对每一项: 打开引用位置,确认代码与描述一致,在复核报告中标记 `确认` / `已漂移(记录新位置)` / `推翻(给出证据)`。

| # | 结论 | 证据位置 |
|---|------|----------|
| 0-1 | Redis 模式通配符订阅失效: pubsub 过滤用精确 map 匹配 | `pkg/redisbroker/pubsub.go:55-60`;订阅写入 `pkg/redisbroker/redis.go:56-61`;通配符字面量经 `node.go:289` 传入;hub 分流在 `hub.go:79-84` |
| 0-2 | Redis History exclusive off-by-one | `pkg/redisbroker/history.go:59-66`(`streamStartID` 生成 `"(ts-seq"`);接口语义 `broker.go:49`(`>= sinceOffset`);内存实现 `broker_memory.go:185`;调用方 `client.go:604`(`sub.Offset + 1`) |
| 0-3 | epoch 按节点 UUID | `pkg/redisbroker/redis.go:35`;消费方 `client.go:562-566, 599-612`;截断 `client.go:621`(`MaxRecoveredPublications`) |
| 0-4 | 恢复消息一律 Binary | `client.go:626-630`;对比实时路径 `hub.go:431-438` |
| 0-5 | TS JSON encode 丢 oneof | `sdks/ts/src/transport/codec/json.ts:150-165` |
| 0-6 | TS JSON decode 无 fromJson + 无 snake→camel | `sdks/ts/src/transport/codec/json.ts:167-170` + `client.ts:189-247`;服务端 protojson `UseProtoNames: true` 在 `shared/marshaler.go:88-93` |
| 0-7 | TS 映射表 `survey_reply` 误写 `survey_response` | `sdks/ts/src/transport/codec/json.ts:9-21`;proto 定义 `protocol/client/v1/service.proto` |
| 0-8 | Payload_Json 入 broker 坍塌为 text | `client.go:924-930` + `hub.go:309-324`;broker 模型仅 `([]byte, isText)` |
| 0-9 | Admin gRPC 无 auth_token 时无认证 | `pkg/grpcstream/admin_server.go:10-14`;`config/config.go` 的 Validate 未强制 |
| 0-10 | Admin 操作绕过 ACL | `pkg/grpcstream/api_handler.go:174-190` → `cluster_commands.go:202-220` 直调 AddSubscription,不经 `checkSubscribeACL` |
| 0-11 | 心跳 IdleTimeout 默认 300s 未生效 | `node.go:82-90`(仅配置非空才建 HeartbeatManager);`defaults.go:11` 常量未引用 |
| 0-12 | WS 默认无写超时 | `pkg/websocket/server.go:32-37` DefaultOptions;`pkg/websocket/transport.go:43-45`;`cmd/server/main.go:188-214` |
| 0-13 | 除 Publish 外入站 handler 无认证检查 | `client.go:954, 733, 1050, 1086, 1119, 1143, 1194`;对比 `client.go:849` |
| 0-14 | 匿名模式可凭 SessionId 接管会话 | `client.go:378-416, 460-494` |
| 0-15 | 本地 resume 致 ConnectionsTotal 泄漏 + 绕过 maxConnsPerUser | `client.go:469-530`;`hub.go:681-690` ReplaceSession 无 limit 检查;`client.go:228-243` closeQuiet 不 Dec |
| 0-16 | lease TTL 90s < idle 300s;续约仅 handlePing(10s 节流) | `cluster_state.go:16`;`client.go:1086-1102`;`heartbeat.go` 不续约 |
| 0-17 | CompareAndSwapSessionLease 已实现但无生产调用 | `pkg/redisbroker/cluster_directory.go:80-123`;写入用 `cluster_state.go:211` 无条件 Put |
| 0-18 | XADD 与 PUBLISH 非原子 | `pkg/redisbroker/redis.go:85-108` |
| 0-19 | Pub/Sub 断线重连无回补 | `pkg/redisbroker/pubsub.go:13-33` |
| 0-20 | redisBroker 无 Ready() | `pkg/redisbroker/redis.go:42-53`;对比 `node.go:127-133` 与 `broker_memory.go:68-70` |
| 0-21 | 远程恢复失败留僵尸会话 | `client.go:521-535` 先 AddClient 后恢复订阅;`cluster_resume.go:112-127` 失败只回滚订阅 |
| 0-22 | 指标三处不对称 | `node.go:239-242`(AddClient 失败不回滚);`cluster_resume.go:154-155`(直调 broker.Subscribe 无 ActiveChannels);`cluster_commands.go:156-181`(PublishToSession 不计 MessagesDelivered) |
| 0-23 | hub.removeWildcardSub 恒返回 last=true(影响 Task 3 计数设计) | `hub.go:110-122` |
| 0-24 | survey responseCh 满不丢结果(map 兜底),原高危结论推翻 | `survey.go:96-98, 123-166` |
| 0-25 | 会话 snapshot 不存 Ephemeral、ChannelOffsets 未填充 | `cluster_state.go:63, 274-309`;消费方 `cluster_resume.go:115` |

- [ ] **Step 3: 复核本文档后续 Task 的代码草案**

按符号定位 Task 1-13 中引用的每个函数/结构体,确认草案与真实代码的字段名、签名、import 路径一致;若草案与现状不符,直接修订本文档对应 Task。

- [ ] **Step 4: 写复核报告并等待确认**

复核报告 `2026-08-10-message-flow-fix-plan-review.md` 内容: 基线 commit、每项复核结论、文档修订记录、阻塞项(若有)。**若存在推翻项或阻塞项,停止执行并上报;否则继续 Task 1。**

---

## Task 1: TS SDK JSON codec 修复(P0-4,独立模块可先行)

> Task 0 修订(R1): `name()` **保留** `"messageloop+json"`(草案原为 `"json"`;subprotocol 协商见 `sdks/ts/src/transport/websocket.ts:98-99` 与 `pkg/websocket/handler.go:26-30`,既有断言 `codec.test.ts:12` 依赖);encode 采用**顶层** `toJson(InboundMessageSchema, msg)`(生成代码无实例 `toJson`,Task 0 实测确认);Step 2 预期修正: encode 输出含 `connect` 键但字段为 camelCase,`encoded.connect.client_id` 为 undefined。

**Files:**
- Modify: `sdks/ts/src/transport/codec/json.ts`(全文重写)
- Test: `sdks/ts/test/codec.test.ts`

**Interfaces:**
- Consumes: `@bufbuild/protobuf` v2 的顶层 `fromJson` / `toJson`;生成的 `OutboundMessageSchema`、`InboundMessageSchema`(`sdks/ts/src/proto/client/v1/service_pb.ts`)
- Produces: 不变的 `Codec` 接口(`sdks/ts/src/transport/codec/codec.ts:7-27`): `name()` / `encode(msg)` / `decode(data)` / `useBytes()`;服务端 wire format 为 protojson + `UseProtoNames: true`(snake_case)

- [ ] **Step 1: 写失败测试(先证明现状损坏)**

在 `sdks/ts/test/codec.test.ts` 新增:

```ts
import { create, fromJson } from "@bufbuild/protobuf";
import { InboundMessageSchema, OutboundMessageSchema } from "../src/proto/client/v1/service_pb";

test("JSONCodec encodes connect oneof content", () => {
  const msg = create(InboundMessageSchema, {
    envelope: { case: "connect", value: { clientId: "c1", token: "t" } },
  });
  const encoded = JSON.parse(jsonCodec.encode(msg));
  expect(encoded.connect.client_id).toBe("c1");
});

test("JSONCodec decodes connected with snake_case fields", () => {
  const wire = JSON.stringify({ connected: { session_id: "s1", epoch: "e1", resumed: false } });
  const decoded = jsonCodec.decode(wire) as any;
  expect(decoded.envelope.case).toBe("connected");
  expect(decoded.envelope.value.sessionId).toBe("s1");
  expect(decoded.envelope.value.epoch).toBe("e1");
});

test("JSONCodec decodes survey_reply", () => {
  const wire = JSON.stringify({ survey_reply: { id: "1", payload: { text: "ok" } } });
  const decoded = jsonCodec.decode(wire) as any;
  expect(decoded.envelope.case).toBe("surveyReply");
});
```

- [ ] **Step 2: 运行确认失败**

Run: `cd sdks/ts && npx jest test/codec.test.ts`
Expected: 3 个新用例 FAIL(encode 输出 `connect.clientId`(camelCase)而非 `connect.client_id`——服务端因显式 `json_name = snake_case` + `DiscardUnknown` 静默丢弃;decode 读不到 `sessionId`;`survey_reply` 无法识别)

- [ ] **Step 3: 重写 json.ts**

删除 `JSON_FIELD_TO_CASE`、`CASE_TO_JSON_FIELD`、`transformOutboundMessage`、`transformInboundMessage`、`deepTransform` 五个手写转换,保留 `bigIntReplacer`。新实现(Task 0 已实测确认: 生成代码无实例 `toJson`,用顶层函数):

```ts
import { fromJson, toJson } from "@bufbuild/protobuf";
import { InboundMessage, InboundMessageSchema, OutboundMessageSchema } from "../../proto/client/v1/service_pb";
import type { Codec } from "./codec";

class JSONCodec implements Codec {
  name(): string { return "messageloop+json"; }
  useBytes(): boolean { return false; }

  encode(msg: object): string {
    return JSON.stringify(toJson(InboundMessageSchema, msg as InboundMessage), JSONCodec.bigIntReplacer);
  }

  decode(data: Uint8Array | string) {
    const text = typeof data === "string" ? data : new TextDecoder().decode(data);
    return fromJson(OutboundMessageSchema, JSON.parse(text));
  }

  private static bigIntReplacer(_key: string, value: unknown) {
    return typeof value === "bigint" ? value.toString() : value;
  }
}

export const jsonCodec: Codec = new JSONCodec();
export { JSONCodec };
```

- [ ] **Step 4: 运行确认通过**

Run: `cd sdks/ts && npm test`
Expected: 全部 PASS(含既有用例;若有旧用例断言了损坏行为,按新行为修正断言)

- [ ] **Step 5: 与服务端 wire format 对拍**

取服务端 `shared/marshaler.go` ProtoJSONMarshaler 对同一 `OutboundMessage` 的 JSON 输出作为 golden 样本(可从 Go 测试打印),断言 `fromJson` 能解析且字段完整。

- [ ] **Step 6: Commit**

```bash
git add sdks/ts/src/transport/codec/json.ts sdks/ts/test/codec.test.ts
git commit -m "fix(ts-sdk): rewrite JSON codec with bufbuild fromJson/toJson"
```

---

## Task 2: Redis History inclusive 修复(P0-2)

**Files:**
- Modify: `pkg/redisbroker/history.go:15-16, 27-28, 56-66`
- Test: `pkg/redisbroker/history_test.go:50-83`

**Interfaces:**
- Consumes: `Broker.History(ch string, sinceOffset uint64, limit int)`(`broker.go:49`,语义 `offset >= sinceOffset`)
- Produces: `streamStartID(sinceOffset uint64) string`,非零时返回 inclusive `"ts-seq"`(不改签名)

**关键约束: 不要动 `client.go:604` 的 `sub.Offset + 1`。** 客户端记录最后收到的 offset=N,恢复传 N,client.go 转为 sinceOffset=N+1,History 返回 `>= N+1`——恰好恢复未读消息,无重复无遗漏。

- [ ] **Step 1: 修改既有单测期望为 inclusive**

`history_test.go` 中 `TestStreamStartID` / `TestStreamOffsetFullRoundTrip`: 非零期望值由 `"(ts-seq"` 改为 `"ts-seq"`。运行 `go test ./pkg/redisbroker/ -run 'TestStreamStartID|TestStreamOffsetFullRoundTrip' -v` 确认 FAIL(复现 off-by-one)。

- [ ] **Step 2: 修改实现**

`history.go` `streamStartID`: 非零分支返回 `fmt.Sprintf("%d-%d", ts, seq)`(去掉 `"(" 前缀`);同步更新 `getHistory` 与 `streamStartID` 的注释(由 "start AFTER" 改为 "offset >= sinceOffset,与 broker.go:49 接口语义一致")。

- [ ] **Step 3: 运行确认通过**

Run: `go test ./pkg/redisbroker/ -v`
Expected: PASS

- [ ] **Step 4: 补 Redis 集成测试(有 Redis 时)**

在 `pkg/redisbroker/history_test.go` 新增(沿用同包 `requireCommandBusRedis` 的 `MESSAGELOOP_TEST_REDIS_ADDR` skip 模式,见 `pkg/redisbroker/cluster_command_bus_test.go:183-212`;Task 0 修订 R2 确认此参照):

```go
func TestRedisBroker_History_InclusiveSinceOffset(t *testing.T) {
    // skip if MESSAGELOOP_TEST_REDIS_ADDR unset
    // publish 3 条到同一 channel,记录 offset o1<o2<o3
    // History(ch, o2, 0) 必须返回 o2 与 o3(修复前只返回 o3)
    // History(ch, 0, 0) 必须返回全部 3 条(回归保护)
}
```

- [ ] **Step 5: 行为变更记录**

Admin `GetHistory(channel, since_offset=N)` 由 exclusive 变 inclusive(返回含 N 本身)。写入文末"行为变更清单"。

- [ ] **Step 6: Commit**

```bash
git add pkg/redisbroker/history.go pkg/redisbroker/history_test.go
git commit -m "fix(redisbroker): make History sinceOffset inclusive per Broker contract"
```

---

## Task 3: Redis 模式通配符订阅修复(P0-1)

**Files:**
- Modify: `pkg/redisbroker/redis.go:18-69`(`redisBroker` 结构体、`New`、`Subscribe`、`Unsubscribe`)
- Modify: `pkg/redisbroker/pubsub.go:50-60`(`runPubSub` 过滤逻辑)
- Test: 新增 `pkg/redisbroker/pubsub_test.go`

**Interfaces:**
- Consumes: `github.com/messageloopio/messageloop/pkg/topics` 的 `Matcher` 接口与 `NewCSTrieMatcher()`(hub.go 已在用,线程安全)
- Produces: `Broker.Subscribe/Unsubscribe` 签名不变(`broker.go:31-36`);内部新增引用计数语义

**关键设计约束(来自复核项 0-23):** hub 的 `removeWildcardSub` 恒返回 `last=true`(`hub.go:110-122`),node saga 会在**任一**通配符订阅者离开时调用 `broker.Unsubscribe(pattern)`。因此 broker 层必须对所有 channel(精确+通配符)做引用计数,计数归零才真正退订。

- [ ] **Step 1: 写失败测试**

新增 `pkg/redisbroker/pubsub_test.go`,直接构造 broker 并测试内部过滤函数(把过滤判断抽成方法 `interested(channel string) bool` 以便单测):

```go
func TestRedisBroker_Interested_Wildcard(t *testing.T) {
    b := newTestRedisBroker() // 直接构造 &redisBroker{...}(不连 Redis),Task 0 修订 R3 确认
    _ = b.Subscribe("forex.*")
    if !b.interested("forex.eur") {
        t.Fatal("wildcard pattern should match concrete channel")
    }
    if b.interested("stocks.us") {
        t.Fatal("unrelated channel should not match")
    }
}

func TestRedisBroker_Unsubscribe_RefCount(t *testing.T) {
    b := newTestRedisBroker()
    _ = b.Subscribe("forex.*")
    _ = b.Subscribe("forex.*") // 第二个订阅者
    _ = b.Unsubscribe("forex.*")
    if !b.interested("forex.eur") {
        t.Fatal("pattern must stay subscribed while refcount > 0")
    }
    _ = b.Unsubscribe("forex.*")
    if b.interested("forex.eur") {
        t.Fatal("pattern must be removed when refcount reaches 0")
    }
}
```

Run: `go test ./pkg/redisbroker/ -run TestRedisBroker -v`
Expected: FAIL(`interested` 尚不存在 / 精确匹配失败)

- [ ] **Step 2: 实现**

`redis.go` 结构体与构造函数:

```go
import "github.com/messageloopio/messageloop/pkg/topics"

type redisBroker struct {
    // ... 保留既有字段 ...
    subMu      sync.RWMutex
    subscribed map[string]int                  // 精确 channel -> 引用计数
    wcCounts   map[string]int                  // 通配符 pattern -> 引用计数
    wcHandles  map[string]topics.Subscription  // pattern -> matcher 订阅句柄
    matcher    topics.Matcher
}

func New(cfg config.RedisConfig) messageloop.Broker {
    opts := NewOptions(cfg)
    return &redisBroker{
        client:     newRedisClient(opts),
        opts:       opts,
        subscribed: make(map[string]int),
        wcCounts:   make(map[string]int),
        wcHandles:  make(map[string]topics.Subscription),
        matcher:    topics.NewCSTrieMatcher(),
        // epoch 字段由 Task 4 处理
    }
}
```

`isWildcard` 判定与 hub 保持一致(`strings.Contains(ch, "*")`,见 `hub.go:75-77`,Task 0 确认是否有可复用的导出函数,没有则在 redisbroker 内写私有副本)。

```go
func (b *redisBroker) Subscribe(ch string) error {
    b.subMu.Lock()
    defer b.subMu.Unlock()
    if isWildcardChannel(ch) {
        b.wcCounts[ch]++
        if b.wcCounts[ch] == 1 {
            sub, err := b.matcher.Subscribe(ch, ch) // 以 pattern 字符串自身作 Subscriber
            if err != nil {
                delete(b.wcCounts, ch)
                return err
            }
            b.wcHandles[ch] = sub
        }
        return nil
    }
    b.subscribed[ch]++
    return nil
}

func (b *redisBroker) Unsubscribe(ch string) error {
    b.subMu.Lock()
    defer b.subMu.Unlock()
    if isWildcardChannel(ch) {
        if b.wcCounts[ch] > 0 {
            b.wcCounts[ch]--
            if b.wcCounts[ch] == 0 {
                delete(b.wcCounts, ch)
                if sub, ok := b.wcHandles[ch]; ok {
                    _ = b.matcher.Unsubscribe(sub)
                    delete(b.wcHandles, ch)
                }
            }
        }
        return nil
    }
    if b.subscribed[ch] > 0 {
        b.subscribed[ch]--
        if b.subscribed[ch] == 0 {
            delete(b.subscribed, ch)
        }
    }
    return nil
}

// interested reports whether this node wants messages for channel.
func (b *redisBroker) interested(channel string) bool {
    b.subMu.RLock()
    defer b.subMu.RUnlock()
    if b.subscribed[channel] > 0 {
        return true
    }
    return len(b.matcher.Lookup(channel)) > 0
}
```

`pubsub.go:55-60` 过滤改为:

```go
if !b.interested(channelName) {
    continue
}
```

注意: `matcher.Lookup` 为 lock-free 实现(`pkg/topics/cstrie.go` 全文复核,Task 0 确认无内部锁),`subMu.RLock` 内调用无锁序倒置风险。

- [ ] **Step 3: 运行确认通过**

Run: `go test -race ./pkg/redisbroker/ -v`
Expected: PASS

- [ ] **Step 4: 补 Redis 集成测试**

`pubsub_test.go` 追加(env 守卫同 Task 2,`requireCommandBusRedis`):

```go
func TestRedisBroker_WildcardReceivesPublication_Redis(t *testing.T) {
    // 真实 Redis: brokerA.Subscribe("forex.*"), brokerB.Publish("forex.eur", ...)
    // 断言 brokerA 的 handler 在超时内收到 channel == "forex.eur" 的 Publication
    // 再 Unsubscribe 两次(计数归零),Publish 第二条,断言不再收到
}
```

- [ ] **Step 5: 回归确认精确订阅不受影响**

运行 `MESSAGELOOP_TEST_REDIS_ADDR=... go test -race ./pkg/redisbroker/ ./...` 中相关用例,确认 `publish_transient_test.go` 等既有测试仍绿。

- [ ] **Step 6: Commit**

```bash
git add pkg/redisbroker/redis.go pkg/redisbroker/pubsub.go pkg/redisbroker/pubsub_test.go
git commit -m "fix(redisbroker): support wildcard subscription interest with refcounting"
```

---

## Task 4: 集群级 epoch(P0-3)

**Files:**
- Modify: `pkg/redisbroker/redis.go:28-53`(`New`、`Start`)、`pkg/redisbroker/redis.go` 的 `Epoch()` 方法
- Modify: `pkg/redisbroker/options.go:30-52`(新增 `EpochKey` 选项)
- Test: `pkg/redisbroker/` 新增 epoch 测试(可并入 `pubsub_test.go` 或新建 `epoch_test.go`)

**Interfaces:**
- Consumes: `client.go:562-566` 通过 `interface{ Epoch() string }` 读取;`client.go:599-612` 比较逻辑不变
- Produces: `Epoch() string` 语义变为"集群级 epoch";新选项 `Options.EpochKey`,默认 `"ml:broker:epoch"`

**方案选择(已权衡,见评审):** 采用 **Redis 固定 key(SET NX)** 方案——节点重启 epoch 不变、持久化部署下可增量恢复。`run_id` 方案不采用(持久化部署下 Redis 重启会无意义地触发全量恢复)。

- [ ] **Step 1: 写失败测试**

```go
func TestRedisBroker_Epoch_SharedAcrossNodes(t *testing.T) {
    // env 守卫;连接同一 Redis 构造两个 broker 并 Start
    // 断言 a.Epoch() == b.Epoch() 且非空
}

func TestRedisBroker_Epoch_PersistedAcrossRestart(t *testing.T) {
    // Start broker A,记录 epoch,关闭;Start broker B,断言 epoch 相同
}
```

Expected: FAIL(现状各自 UUID 不同)

- [ ] **Step 2: 实现**

`options.go` 新增:

```go
// EpochKey is the Redis key holding the cluster-wide broker epoch.
EpochKey string // default "ml:broker:epoch"
```

`redis.go`:

```go
func (b *redisBroker) Start(ctx context.Context, handler messageloop.PublicationHandler) error {
    b.handler = handler
    pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
    defer cancel()
    if err := b.client.Ping(pingCtx).Err(); err != nil {
        return fmt.Errorf("redis broker: connect: %w", err)
    }
    if err := b.initEpoch(ctx); err != nil {
        return fmt.Errorf("redis broker: init epoch: %w", err)
    }
    defer func() { _ = b.client.Close() }()
    return b.runPubSubWithRetry(ctx)
}

func (b *redisBroker) initEpoch(ctx context.Context) error {
    c, cancel := context.WithTimeout(ctx, 5*time.Second)
    defer cancel()
    if _, err := b.client.SetNX(c, b.opts.EpochKey, uuid.NewString(), 0).Result(); err != nil {
        return err
    }
    epoch, err := b.client.Get(c, b.opts.EpochKey).Result()
    if err != nil {
        return err
    }
    b.epoch = epoch
    return nil
}
```

`New()` 中删除 `epoch: uuid.NewString()`(epoch 在 `Start` 中初始化)。`Epoch()` 加防御: epoch 未初始化时返回空串的调用方(`client.go:605`)会走全量恢复,安全;Task 0 复核 `node.go:112-135` 时序确认: `Start` 内 `initEpoch` 先于 `runPubSubWithRetry` 执行,而 Task 11b 的 `Ready()` 在 PSubscribe 确认后关闭 → **Ready 关闭时 epoch 必然已就绪,无需额外显式同步**(修订 R4)。

- [ ] **Step 3: 运行确认通过**

Run: `MESSAGELOOP_TEST_REDIS_ADDR=... go test -race ./pkg/redisbroker/ -run TestRedisBroker_Epoch -v`
Expected: PASS

- [ ] **Step 4: 并发竞争测试**

多 goroutine 同时 `initEpoch`,断言所有读者拿到同一值(SET NX 语义保证,测试做回归保护)。

- [ ] **Step 5: 行为变更记录**

升级后所有客户端旧 epoch 不匹配,**首次重连全量恢复一次**(安全保守,`client.go:601-603` 设计意图)。若运维清空 stream,应同时删除 epoch key——写入文档"行为变更清单"与部署文档运维注意事项(`docs/deployment.md` 不存在时并入 `docs/protocol.md`,修订 R4)。

- [ ] **Step 6: Commit**

```bash
git add pkg/redisbroker/redis.go pkg/redisbroker/options.go pkg/redisbroker/epoch_test.go docs/deployment.md
git commit -m "fix(redisbroker): use cluster-wide epoch stored in Redis"
```

---

## Task 5: Admin gRPC 强制认证(P0-新增)

**Files:**
- Modify: `config/config.go`(admin 配置结构与 `Validate()`)
- Modify: `pkg/grpcstream/admin_server.go:10-14`(如需)
- Modify: `configs/test.yaml`、相关测试配置
- Test: `config/config_test.go`

**Interfaces:**
- Consumes: `pkg/grpcstream/server.go:80-103` 的 interceptor 挂载逻辑(仅 `AdminAuthToken != ""` 时)
- Produces: 配置字段 `server.grpc_admin.allow_insecure`(bool,默认 false);`Validate()` 新规则

- [ ] **Step 1: 写失败测试**

`config/config_test.go`(无 `minimalValidConfig` helper,内联构造;修订 R5):

```go
func TestValidate_AdminRequiresAuthToken(t *testing.T) {
    cfg := &Config{
        Transport: Transport{WebSocket: WebSocketTransport{Addr: ":9080"}},
        Server:    Server{GRPCAdmin: GRPCAdmin{Addr: "127.0.0.1:9091"}},
    }
    if err := cfg.Validate(); err == nil {
        t.Fatal("empty admin auth token must fail validation")
    }
    cfg.Server.GRPCAdmin.AllowInsecure = true
    if err := cfg.Validate(); err != nil {
        t.Fatalf("allow_insecure must bypass the check: %v", err)
    }
}
```

- [ ] **Step 2: 实现**

`config.go` admin 配置结构新增 `AllowInsecure bool \`yaml:"allow_insecure"\``;`Validate()` 追加: admin 启用且 `AuthToken == "" && !AllowInsecure` → 返回错误,错误信息明确提示设置 `auth_token` 或显式 `allow_insecure: true`。`admin_server.go` 在 `AllowInsecure` 生效时启动日志 WARN 一条("admin gRPC running WITHOUT authentication")。

- [ ] **Step 3: 修通既有配置与测试**

`configs/test.yaml` 及各测试内联配置: 补 `auth_token` 或 `allow_insecure: true`,使 `go test ./...` 全绿。

- [ ] **Step 4: Commit**

```bash
git add config/config.go config/config_test.go configs/test.yaml pkg/grpcstream/admin_server.go
git commit -m "feat(config): require admin gRPC auth token unless allow_insecure"
```

---

## Task 6: 心跳 IdleTimeout 默认值生效(P0-新增)

**Files:**
- Modify: `node.go:82-90`
- Test: `node_test.go`

**Interfaces:**
- Consumes: `defaults.go:11` `DefaultHeartbeatIdleTimeout = 300 * time.Second`(已存在未被引用)
- Produces: `NewNode` 行为: 未配置心跳时按默认值启用 idle 检测

- [ ] **Step 1: 写失败测试**

`node_test.go`: 以空 `Heartbeat.IdleTimeout` 构造 Node,断言 heartbeat manager 存在且 idle timeout 为 300s(具体可观测字段 Task 0 复核 `heartbeat.go` 结构后确定,例如 `node.heartbeat != nil` 或暴露的 config 取值)。

- [ ] **Step 2: 实现**

`node.go` 中配置解析处: `IdleTimeout == ""` 时回落到 `DefaultHeartbeatIdleTimeout`(注意类型: 配置是字符串时长还是 `time.Duration`,Task 0 复核 `config.go` 与 `node.go:82-90` 的解析方式,保持一致的解析路径)。若该配置块还控制 ping interval 等其他字段,空配置时整组按默认。

- [ ] **Step 3: 回归**

Run: `go test -race ./...`
Expected: 全绿;重点确认既有测试中没有依赖"无心跳 manager"行为的用例,若有,显式配置关闭或修正用例。

- [ ] **Step 4: 行为变更记录**

未配置心跳的部署此前**完全没有** idle 断连,修复后 300s 无活动连接会被断开(`DisconnectIdleTimeout`)。补充(修订 R6): `pkg/websocket/handler.go:66-69` 的 WS read deadline 未配置心跳时由 60s 变 600s(2×IdleTimeout)。均写入"行为变更清单"。

- [ ] **Step 5: Commit**

```bash
git add node.go node_test.go
git commit -m "fix(node): apply default heartbeat idle timeout when unconfigured"
```

---

## Task 7: WebSocket 默认写超时(P1-6a)

**Files:**
- Modify: `pkg/websocket/server.go:32-37`(`DefaultOptions`)
- Modify: `cmd/server/main.go:188-214`(`newWebSocketServer` 配置解析)
- Test: `pkg/websocket/transport_test.go`

**Interfaces:**
- Consumes: `pkg/websocket/transport.go:43-45`(已支持 `writeTimeout > 0` 时设 deadline,无需改 transport)
- Produces: 默认值 `DefaultWSWriteTimeout = 10 * time.Second`(与 gRPC `pkg/grpcstream/transport.go:19` 对齐)

- [ ] **Step 1: 写失败测试**

`transport_test.go`: 构造 `DefaultOptions()`,断言 `WriteTimeout == 10*time.Second`;阻塞写超时用例(修订 R7: 现有 `transport_test.go` 无 conn mock,若阻塞写用例成本过高可仅保留默认值断言 + 参考 `pkg/grpcstream/transport_test.go:192` 的阻塞写模式)。

- [ ] **Step 2: 实现**

`DefaultOptions()` 增加 `WriteTimeout: DefaultWSWriteTimeout`;`main.go` 中配置为空时保留该默认值(现状是未配置即覆盖为 0,改为仅当配置显式给出时才覆盖,或在解析后 `if wsOpts.WriteTimeout == 0 { wsOpts.WriteTimeout = DefaultWSWriteTimeout }`——选不破坏显式 `0` 语义最少的方案;允许配置 `"0"` 显式关闭需文档说明)。

- [ ] **Step 3: 行为变更记录**

慢消费者由"无限阻塞广播"变为"10s 写超时后 `Send` 失败,连接被 `DisconnectSlowConsumer` 关闭,广播继续"。写入"行为变更清单"。

- [ ] **Step 4: Commit**

```bash
git add pkg/websocket/server.go pkg/websocket/transport_test.go cmd/server/main.go
git commit -m "fix(websocket): default 10s write timeout to protect broadcast from slow consumers"
```

---

## Task 8: 入站消息统一认证守卫(P1-6b)

**Files:**
- Modify: `client.go:325-348`(`handleMessage` 分发入口)
- Test: `client_test.go`

**Interfaces:**
- Consumes: `c.Authenticated()`(Task 0 复核其定义与 `requireAuth=false` 时的语义——必须确认: 匿名模式下 Connect 成功后 `Authenticated()` 返回 true,否则会误伤匿名模式)
- Produces: 不变

- [ ] **Step 1: 写失败测试**

`client_test.go` 对每种非 Connect envelope 各加一个未认证用例(参照既有 `TestClientSession_HandleMessage_Publish_BeforeAuth` 的构造方式):

```go
func TestClientSession_HandleMessage_Subscribe_BeforeAuth(t *testing.T) { /* 期望 DisconnectInvalidToken 或 BAD_REQUEST */ }
func TestClientSession_HandleMessage_RPC_BeforeAuth(t *testing.T)         { /* 同上 */ }
func TestClientSession_HandleMessage_Unsubscribe_BeforeAuth(t *testing.T) { /* 同上 */ }
func TestClientSession_HandleMessage_Ping_BeforeAuth(t *testing.T)        { /* 同上 */ }
func TestClientSession_HandleMessage_SubRefresh_BeforeAuth(t *testing.T)  { /* 同上 */ }
```

Expected: FAIL(现状均放行)

- [ ] **Step 2: 实现**

`handleMessage` 中,在 `switch` 之前(或 switch 的 Connect 分支之后的统一位置)加:

```go
if _, isConnect := in.Envelope.(*clientpb.InboundMessage_Connect); !isConnect && !c.Authenticated() {
    return DisconnectInvalidToken
}
```

同时在 `switch` 末尾补 `default` 分支: 未知/空 envelope 返回 `DisconnectBadRequest`(消除静默丢弃,`client.go:325-348`)。

- [ ] **Step 3: 回归**

Run: `go test -race ./...`
Expected: 全绿;重点排查既有测试中"未 Connect 直接发 Ping/Subscribe"的用例并修正。

- [ ] **Step 4: Commit**

```bash
git add client.go client_test.go
git commit -m "fix(client): require authentication for all non-connect inbound messages"
```

---

## Task 9: 匿名会话接管禁止 + 本地 resume 指标/限流修复(P1-6c/d)

**Files:**
- Modify: `client.go:378-416, 454-530`(handleConnect 的 resume/takeover 分支)
- Modify: `hub.go:681-690`(`ReplaceSession`)、`client.go:228-243`(`closeQuiet`)/指标转移逻辑
- Test: `client_test.go`、`hub_test.go`

**Interfaces:**
- Consumes: `node.go:71` `requireAuth`;`client.go` 的 `metricsCharged` 字段;`hub.go` 的 `addWithLimit`/`maxConnsPerUser`
- Produces: `ReplaceSession` 增加 limit 校验语义(签名 Task 0 复核后确定,可能新增 error 返回)

- [ ] **Step 1: 写失败测试**

```go
func TestClientSession_AnonymousResumeRejected(t *testing.T) {
    // requireAuth=false;client A connect 得 session S;断开(或不断开)
    // client B 携带 Connect.SessionId=S 连接
    // 断言 B 不接管 S:要么按新会话处理(忽略 SessionId),要么收到断开错误
    // 具体断言语义由 Step 2 的方案决定并记录
}

func TestClientSession_LocalResume_MetricsBalanced(t *testing.T) {
    // 正常认证 resume 成功后,ConnectionsTotal 净增 1(不是 2)
}

func TestHub_ReplaceSession_EnforcesMaxConnsPerUser(t *testing.T) {
    // 新 user 已达上限时 ReplaceSession 返回错误
}
```

- [ ] **Step 2: 实现(方案已定: 禁止匿名 resume)**

`handleConnect`: 仅当认证实际发生(`requireAuth=true` 且 token 校验通过)时才进入 resume/takeover 分支;`requireAuth=false` 时忽略 `connect.SessionId`(按新会话处理)并 WARN 日志一条。指标修复(二选一,Task 0 复核代码后选改动小的):
- 方案 A(推荐): 本地 resume 时把旧 client 的 `metricsCharged` 转移给新 client,旧 client `closeQuiet` 不再 Dec(本来也不 Dec),新 client 正常 Dec——收支平衡。
- 方案 B: 本地 resume 也走 `AddClient`(自动 Inc + limit 检查),旧 client 改为正常 close 路径 Dec。

`hub.go ReplaceSession`: 替换前对新 user 执行与 `addWithLimit` 相同的 `maxConnsPerUser` 检查,超限返回 error,`handleConnect` 据此断开新连接。

- [ ] **Step 3: 回归 + 行为变更记录**

匿名模式断线后不再保留会话/订阅/offset(此前可接管)。写入"行为变更清单"。

- [ ] **Step 4: Commit**

```bash
git add client.go hub.go client_test.go hub_test.go
git commit -m "fix(client): disable anonymous session takeover; balance metrics and conn limit on resume"
```

---

## Task 10: 会话 lease TTL 与 CAS 抢占(P1-7)

**Files:**
- Modify: `cluster_state.go:16`(TTL 常量)、`cluster_state.go:202-215`(`syncClusterSessionState`)
- Modify: `cluster_resume.go:34-88`(`resumeRemoteSession` 使用 CAS)
- Modify: `client.go:521-535`(本地 resume 的 lease 更新也走 CAS,视复核结论)
- Test: `cluster_remote_test.go`、`cluster_resume_test.go`

**Interfaces:**
- Consumes: `SessionDirectory.CompareAndSwapSessionLease`(`cluster.go:68` 接口已有,`cluster_directory.go:80-123` 已实现)
- Produces: TTL 常量 `defaultClusterSessionLeaseTTL = 600 * time.Second`(= 2 × `DefaultHeartbeatIdleTimeout`)

- [ ] **Step 1: 写失败测试**

```go
func TestResumeRemoteSession_UsesCAS(t *testing.T) {
    // fakeSessionDirectory 记录 CompareAndSwapSessionLease 调用
    // 断言 resumeRemoteSession 以旧 lease version 为期望值调用 CAS
}

func TestResumeRemoteSession_CASConflictAborts(t *testing.T) {
    // fake CAS 返回 false(另一节点已接管)
    // 断言 resume 失败、不执行 takeover、不恢复订阅,新连接被断开
}
```

Expected: FAIL(现状无 CAS 调用)

- [ ] **Step 2: 实现**

1. `cluster_state.go:16`: `defaultClusterSessionLeaseTTL = 600 * time.Second`,注释写明取值依据(必须覆盖 `DefaultHeartbeatIdleTimeout=300s` 并留一倍余量;续约触发点仅 `handlePing` 且 10s 节流,见 `client.go:1086-1102`)。
2. `resumeRemoteSession`: 读取旧 lease 后,以 `lease.LeaseVersion` 为 expected、新版本号(`+1`,注意 `cluster_state.go:256-258` 的 0→1 fallback)调用 `CompareAndSwapSessionLease`;CAS 失败 → 中止恢复,向新连接返回断开(复用 `disconnect.go` 中合适的 code,Task 0 复核后确定,如 `DisconnectStale`)。
3. 续约路径(`syncClusterSessionState`)保持无条件 `PutSessionLease`(拥有者自身续约,版本已是独占的);Task 0 复核确认此判断成立,若续约也可能与并发 resume 冲突,则续约同样改 CAS。
4. 保持 10s 续约节流不变(防 Redis 写放大);600s TTL 已覆盖节流窗口。

- [ ] **Step 3: 集成回归**

`cluster_remote_test.go` 的 fake 目录目前 CAS 恒 true——改为按版本号真实模拟,并跑 `MESSAGELOOP_TEST_REDIS_ADDR=... go test -race -run TestClusterRedis ./...`。

- [ ] **Step 4: 行为变更记录**

lease 90s→600s: 节点崩溃后其会话被其他节点接管的时间变长(此前最快 90s)。这是用"接管延迟"换"存活连接不被误接管",写入"行为变更清单"。

- [ ] **Step 5: Commit**

```bash
git add cluster_state.go cluster_resume.go client.go cluster_remote_test.go cluster_resume_test.go
git commit -m "fix(cluster): extend session lease TTL and use CAS for cross-node resume"
```

---

## Task 11: Redis 可靠性三件套(P1-8: PUBLISH 补偿 + Ready 信号 + 重连回补)

**Files:**
- Modify: `pkg/redisbroker/redis.go:73-129`(`Publish` 失败补偿、`Ready()`)
- Modify: `pkg/redisbroker/pubsub.go:13-80`(重连回补、per-channel offset 跟踪)
- Test: `pkg/redisbroker/pubsub_test.go` / 新增 `ready_test.go`

**Interfaces:**
- Consumes: `node.go:127-133` 的 `interface{ Ready() <-chan struct{} }` 等待逻辑(已存在,无需改 node)
- Produces: `redisBroker.Ready() <-chan struct{}`(新增,与 `broker_memory.go:68-70` 对齐)

### 11a. PUBLISH 失败补偿(原子性)

**方案决策:** 不采用 Lua 脚本——stream key(`ml:stream:<ch>`)与 pubsub key(`ml:pubsub:<ch>`)在 Redis Cluster 下不同 slot 会失败,而加 hash tag 会改变既有 key 命名、需要数据迁移。采用**最小变更的补偿方案**: PUBLISH 失败时 XDEL 回滚 stream 条目并返回错误;XDEL 再失败则 ERROR 日志 + 指标(残留历史可接受,客户端恢复仍能拿到)。

- [ ] **Step 1: 写失败测试**(修订 R8: 仓库无 miniredis 依赖、`redisBroker.client` 为具体类型 `*redis.Client` 不可 stub,改为真实 Redis 集成测试 + go-redis Hook(`client.AddHook`)注入 PUBLISH 失败,env 守卫同 Task 2)

```go
func TestRedisBroker_Publish_PubSubFailureRollsBackStream(t *testing.T) {
    // requireCommandBusRedis 获取配置;broker.client.AddHook 对 *redis.PublishCmd 返回错误
    // 断言: Publish 返回 error;XADD 的条目已被 XDEL(stream 长度不增)
}
```

- [ ] **Step 2: 实现**

`redis.go` `Publish` 的 PUBLISH 失败分支:

```go
if err := b.client.Publish(ctx, b.opts.PubSubPrefix+ch, pubSubData).Err(); err != nil {
    if delErr := b.client.XDel(ctx, stream, id).Err(); delErr != nil {
        log.ErrorContext(ctx, "failed to roll back stream entry after pubsub failure",
            "stream", stream, "id", id, "error", delErr)
    }
    return 0, err
}
```

- [ ] **Step 3: 运行确认通过** → commit 11a。

### 11b. Ready() 信号

- [ ] **Step 1: 写失败测试**

```go
func TestRedisBroker_Ready_ClosesAfterSubscribe(t *testing.T) {
    // Start 后 select 等待 Ready() 在超时内关闭
    // Start 前 Ready() 不得关闭
}
```

- [ ] **Step 2: 实现**

`redisBroker` 新增 `readyCh chan struct{}`(`New` 中创建)与 `readyOnce sync.Once`;`Ready()` 返回 `b.readyCh`;`runPubSub` 中 PSubscribe 成功(**go-redis v9.18 用 `pubsub.Receive(ctx)` 等待订阅确认**,Task 0 修订 R9 确认)后 `readyOnce.Do(func() { close(b.readyCh) })`。注意 `runPubSubWithRetry` 重连场景: readyCh 只关一次,重连不重置(Node.Run 只需启动就绪语义)。

- [ ] **Step 3: 验证 Node.Run 阻塞语义**

测试: 用一个 PSubscribe 延迟完成的 fake/慢 Redis,断言 `Node.Run` 在 ready 前不返回。

- [ ] **Step 4: Commit 11b。**

> 行为变化(修订 R9): Redis broker 新增 `Ready()` 后,`health.go:33-49` 的 `healthReadyBroker` 自动生效——就绪前健康检查返回 503(此前恒 not applicable);同步更新 `docs/developer/05-observability.md:24` 的表述。

### 11c. Pub/Sub 断线重连回补

- [ ] **Step 1: 写失败测试**

```go
func TestRedisBroker_Reconnect_CatchesUpMissedMessages(t *testing.T) {
    // broker consumer 运行中收到 offset o1;模拟 pubsub 断开(停 consumer)
    // producer 发布 o2, o3;consumer 重连
    // 断言 consumer handler 收到 o2, o3(回补),且与后续实时消息无重复
}
```

- [ ] **Step 2: 实现**

`pubsub.go`:
1. broker 新增 `lastOffsets map[string]uint64`(subMu 保护),`runPubSub` 每投递一条消息记录 `lastOffsets[channel] = offset`(仅当 offset 更大)。
2. `runPubSubWithRetry` 重连成功后、恢复 `PSUBSCRIBE` 之前,对每个 `interested` 的 channel(精确订阅集合;通配符 pattern 无法直接 XRANGE,回补范围限定精确 channel,通配符缺口写入文档已知限制): `XRangeN(stream, streamStartID(lastOffset+1), "+", N)` 补投缺失消息,复用 `getHistory` 的反序列化逻辑。
3. 回补与实时消息的去重: 回补用的起始 offset 来自 `lastOffsets`,严格 `> lastOffset`;`runPubSub` 投递时若 `offset <= lastOffsets[channel]` 则跳过(防重连瞬间 pubsub 与回补重叠)。

- [ ] **Step 3: 限制说明**

`PublishTransient`(presence 事件)不写 stream、offset=0,不可回补——属设计取舍;回补期间投递的消息 `Time` 语义沿用 Task 12 修复后的原始时间。写入文档。

- [ ] **Step 4: Commit 11c。**

```bash
git commit -m "fix(redisbroker): roll back stream on pubsub failure; add Ready signal and reconnect catch-up"
```

---

## Task 12: Publication 模型扩展(P1-5,破坏性接口变更,放最后单独执行)

**Files:**
- Modify: `broker.go:7-14`(Publication 定义)、`broker.go:40,47`(Publish/PublishTransient 签名)
- Modify: `broker_memory.go:112-165`
- Modify: `pkg/redisbroker/message.go:8-15`、`redis.go:73-129`、`history.go:36-52`、`pubsub.go:62-77`
- Modify: `node.go:463-488`(`Node.Publish`/`PublishTransient`)
- Modify: `hub.go:309-324, 431-438`(投递时按原始类型重建 Payload)
- Modify: `client.go:919-940`(handlePublish 不再坍塌 JSON)、`client.go:626-642`(恢复按类型重建)
- Modify: `pkg/grpcstream/api_handler.go:25-107, 230-257`(Admin Publish/GetHistory 透传 id/metadata/content_type;Task 0 修订 R10 更新行号,原 42-100/230-256 已漂移)
- Modify: `protocol/server/v1/api.proto:130-135`(`HistoryPublication` 增加 `id = 5`、`metadata = 6`;随后 `task generate-protocol`)
- Test: `broker_memory_test.go`、`pkg/redisbroker/`、`client_test.go`、`client_fix_test.go`、`pkg/grpcstream/api_handler_test.go`、所有 mock broker

**Interfaces:**
- Consumes: 所有 `Broker.Publish` 调用方: `client.go:940`、`api_handler.go:96`、`node.go:463-488`、presence 事件 `node.go:834-853`
- Produces(新签名):

```go
// PayloadKind identifies the original Payload oneof variant.
type PayloadKind int

const (
    PayloadKindBinary PayloadKind = iota
    PayloadKindText
    PayloadKindJSON
)

type Publication struct {
    Channel     string
    Payload     []byte            // 既有: 载荷字节(JSON kind 时为 JSON 文本)
    Kind        PayloadKind       // 新增: 原始 oneof 类型
    ContentType string            // 新增
    Id          string            // 新增: 发布方消息 id(可空)
    Metadata    map[string]string // 新增(可空)
    IsText      bool              // 保留但标记 deprecated,由 Kind 推导,兼容期保留
    Offset      uint64
    Time        int64
    Epoch       string
}

type Broker interface {
    // ... 其余方法不变 ...
    Publish(ch string, pub *Publication) (uint64, error)           // offset 写回 pub.Offset 并返回
    PublishTransient(ch string, pub *Publication) error
    History(ch string, sinceOffset uint64, limit int) ([]*Publication, error)
}
```

(最终字段集以 Task 0 复核结果微调;`IsText` 是否保留取决于兼容成本,倾向删除并全量改调用方。)

- [ ] **Step 1: 先改接口与两个实现,编译驱动找齐调用方**

修改 `broker.go` 接口 → `go build ./...` 列出全部编译错误 → 逐一修复 memory/redis broker、node、hub、client、api_handler、全部测试与 mock。此步保证"没有遗漏的调用方"。

- [ ] **Step 2: 核心语义实现**

1. `client.go handlePublish`: 按 inbound Payload 的 oneof 设置 `Kind`;`Payload_Json` 的 bytes 仍由 `MarshalJSONStruct` 产出(入口已被 structpb float64 化的精度损失属已知限制,不在本任务解决,写入文档);`ContentType`/`Metadata` 从 inbound 透传;`Id` 取 inbound 消息 id。
2. `hub.go` 投递: 按 `pub.Kind` 重建——`Binary`→`Payload_Binary`;`Text`→`Payload_Text`;`JSON`→`json.Unmarshal` bytes 到 `map[string]any` → `structpb.NewStruct` → `Payload_Json`,失败时降级 `Payload_Text` + WARN。同时透传 `Id`/`Metadata` 到 `clientpb.Message`。
3. `client.go:626-642` 恢复路径: 同 hub 的重建逻辑(抽一个共享 helper,如 `broker.go` 或新文件中 `func (p *Publication) PayloadProto() *sharedpb.Payload`,供 hub/client/api_handler 三处复用)。
4. `api_handler.go`: Publish 把 `pub.Id`/`pub.Metadata`/`pub.Payload.ContentType` 填入 `Publication`;GetHistory 用同一 helper 重建并回填 id/metadata/content_type。
5. `redisMessage` 增加 `kind`、`contentType`、`id`、`metadata`、`time` JSON 字段;`deserializeMessage` 后兼容推断: `kind` 缺失时 `isText==true`→Text、否则 Binary。旧格式 stream 数据可读、旧代码读新数据忽略未知字段——滚动升级安全。
6. `history.go` 回填 `Time`(用存储的 time 而非 `time.Now()`)与 `Epoch`。
7. `api.proto` 的 `HistoryPublication` 增加 `string id = 5; map<string,string> metadata = 6;`(5/6 未使用,Task 0 确认),运行 `task generate-protocol`,提交 `shared/genproto` 产物;`shared/` 是独立 go module,执行时确认根模块引用方式(replace 或版本)同步(修订 R10)。

- [ ] **Step 3: 测试(每类一条,先写后改)**

```go
func TestMemoryBroker_Publish_PreservesKindAndMetadata(t *testing.T)  { /* JSON kind 发布,History 读回 kind/content_type/id/metadata/time 完整 */ }
func TestRedisBroker_Message_BackwardCompat(t *testing.T)             { /* 手工构造仅含 p/isText 的旧格式 JSON,反序列化推断 kind 正确 */ }
func TestClient_Recovery_PreservesPayloadType(t *testing.T)           { /* 发布 text 消息→断线→重连恢复,断言恢复消息是 Payload_Text 而非 Binary(复现 client.go:626 原 bug) */ }
func TestAdmin_GetHistory_ReturnsContentTypeAndId(t *testing.T)       { /* admin publish 带 id/metadata,GetHistory 读回 */ }
```

- [ ] **Step 4: 全量验证**

```bash
go build ./... && go vet ./... && go test -race ./...
MESSAGELOOP_TEST_REDIS_ADDR=... go test -race ./...
cd sdks/go && go test ./...
```

- [ ] **Step 5: 文档**

`docs/protocol.md` 补充 Payload 类型保留语义;"行为变更清单"记录: 订阅端现在会收到 `Payload_Json`(此前坍塌为 text)——对按 text 解析的存量客户端是行为变化;`Broker.Publish/PublishTransient` 签名变更(对外破坏性,若有外部实现者需迁移)。

- [ ] **Step 6: Commit**

```bash
git add broker.go broker_memory.go node.go hub.go client.go pkg/redisbroker/ pkg/grpcstream/api_handler.go protocol/server/v1/api.proto shared/genproto/ docs/protocol.md
git commit -m "feat(broker)!: preserve payload kind/content_type/id/metadata through Publication model"
```

---

## Task 13: P2 批量(Admin ACL、僵尸会话、指标对称、presence/projection 杂项)

### 13a. Admin 操作纳入 ACL

**Files:** `pkg/grpcstream/api_handler.go:150-220`、`node.go`(SubscribeSession/UnsubscribeSession/PublishToSession)、`acl.go`

- [ ] **Step 1:** 测试: admin Subscribe/Publish 对被 ACL 拒绝的 channel,断言返回拒绝(现状放行)。
- [ ] **Step 2:** 在 `Node.SubscribeSession/UnsubscribeSession` 与 admin Publish 入口调用与 `client.go` 相同的 ACL 检查(Task 0 修订 R11: `acl.go` 的 principal 模型为 **userID 粒度**(`CanSubscribe/CanPublish(channel, userID)`),admin 操作使用固定 admin 身份(如 `"admin"`)并文档化);集群命令路径(`cluster_commands.go:202-220`)信任发起节点已做的检查,不重复校验(避免双标)。
- [ ] **Step 3:** Commit `fix(admin): enforce ACL on admin subscribe/publish operations`。

### 13b. 僵尸会话回滚

**Files:** `client.go:521-535`、`cluster_resume.go:112-127`

- [ ] **Step 1:** 测试: `restoreSessionSubscriptions` 注入中途失败,断言 hub 无该 session、lease/snapshot 已清理(现状残留)。
- [ ] **Step 2:** 失败补偿: 恢复订阅失败时调用 `RemoveSession` + `deleteClusterSessionState`(Task 0 复核这两个方法的真实名称)并断开新连接。
- [ ] **Step 3:** Commit `fix(cluster): roll back session when remote subscription restore fails`。

### 13c. 指标对称三处

**Files:** `node.go:239-242`、`cluster_resume.go:141-167`、`cluster_commands.go:156-181`

- [ ] **Step 1:** 测试三条: AddClient sync 失败后 `ConnectionsTotal` 不增(修订 R12: 现状已"不增"——`node.go:239-242` Inc 仅在成功路径,此条为**回归保护**而非 TDD 失败测试);远程恢复订阅后 `ActiveChannels` 与正常订阅一致;`PublishToSession` 计入 `MessagesDelivered`/`DeliveryFailures`。
- [ ] **Step 2:** 实现: `AddClient` 错误路径 Dec;`restoreLocalSubscription`/`removeLocalSubscriptionOnly` 对称维护 `ActiveChannels`;`handleClusterPublishCommand` 的 `client.Send` 周围补计数(与 `hub.go:349-351` 口径一致)。
- [ ] **Step 3:** Commit `fix(metrics): balance connections/channels/delivery counters on failure and cluster paths`。

### 13d. presence / projection 杂项

**Files:** `pkg/redisbroker/presence_redis.go:40-51`、`cluster_projection_repair.go:108-117`、`node.go:834-853`

- [ ] **Step 1:** presence 索引 TTL 与 member TTL 对齐,`Remove` 后清理空 index;测试覆盖。
- [ ] **Step 2:** projection repair 增加对 `owner:*` 的扫描: node lease 不存在的 owner key 主动删除(不再等 10min TTL);测试覆盖(可 fake directory)。
- [ ] **Step 3:** presence join/leave 发布错误由 `_ =` 改为记录 WARN + 指标;`__presence` 频道无消费者的问题在 `docs/protocol.md` 中说明现状(本方案不新增消费者)。
- [ ] **Step 4:** Commit `fix(cluster): align presence index TTL, reap dead owner projections, log presence publish errors`。

### 13e. 会话 snapshot 补全 Ephemeral

**Files:** `cluster_state.go:274-309`、`cluster_resume.go:115`、`cluster_resume_test.go`

- [ ] **Step 1:** 测试: ephemeral 订阅的会话经 snapshot 跨节点恢复后,断言订阅仍为 ephemeral(不触发 presence join/leave);现状 snapshot 恒写 `Ephemeral: false`,恢复后变性。
- [ ] **Step 2:** `clusterSessionSnapshot` 正确写入每个订阅的 `Ephemeral` 标志(`ClusterSubscriptionSnapshot` 的 `Ephemeral` 字段**已存在**,`cluster_state.go:49-52`,Task 0 修订 R13 确认——只需填充,无需加字段)。`ChannelOffsets` 字段维持不填充(恢复 offset 由客户端携带,写入文档)。
- [ ] **Step 3:** Commit `fix(cluster): preserve ephemeral flag in session snapshots`。

---

## 行为变更清单(Release Notes 素材)

| # | 变更 | 影响面 | 来源 Task |
|---|------|--------|-----------|
| 1 | TS SDK JSON 模式从"基本不可用"修复为正常 | 仅 TS SDK JSON 用户;protobuf 用户零影响 | 1 |
| 2 | Admin `GetHistory` 的 `since_offset` 变 inclusive | Admin API 调用方(做过 -1 补偿的会重复) | 2 |
| 3 | Redis 模式通配符订阅恢复可用 | 集群用户 | 3 |
| 4 | 升级后客户端首次重连全量恢复一次(epoch 变更) | 集群用户,一次性 | 4 |
| 5 | Admin gRPC 必须配置 `auth_token` 或显式 `allow_insecure: true`,否则启动失败 | 运维配置 | 5 |
| 6 | 未配置心跳时启用默认 300s idle 断连;WS read deadline 由 60s 变 600s | 此前无 idle 检测的部署 | 6 |
| 7 | WS 慢消费者 10s 写超时后被断开(此前无限阻塞) | 客户端 | 7 |
| 8 | 未 Connect 的 Subscribe/RPC/Ping 等被拒绝 | 协议违规客户端 | 8 |
| 9 | 匿名模式禁止会话接管,断线丢订阅/offset | 匿名模式用户 | 9 |
| 10 | 会话 lease 90s→600s,节点崩溃后接管延迟变长 | 集群 failover 时间 | 10 |
| 11 | PUBLISH 失败回滚 stream;启动等待 Redis 就绪;重连回补(精确 channel) | 集群可靠性 | 11 |
| 12 | `Payload_Json` 全链路保留(订阅端收到 json 而非 text);id/metadata/content_type 透传;`Broker.Publish` 签名变更(破坏性,`PublishTransient` 不再返回 offset) | 订阅端客户端 + 外部 Broker 实现者 | 12 |
| 13 | Admin 操作受 ACL 约束;指标口径修正 | Admin 调用方 | 13 |

## 已知限制(本方案不解决,需文档化)

1. JSON 大整数(>2^53)在 structpb 入口已损失精度,Task 12 只保证不再二次损失;彻底解决需在 inbound 层保留原始 JSON bytes。
2. 通配符订阅的跨节点重连回补(11c 仅覆盖精确 channel)。
3. `PublishTransient`(presence 事件)不可回补。
4. Go SDK `PingTimeout` 未生效、ack 不关联;TS SDK 重连重复订阅;两 SDK 默认 AutoReconnect 不一致——属 SDK 独立修复项,不在本方案。
5. presence `__presence` 频道无消费者;ACL(`path.Match`)与 topic matcher、proxy router 的通配符语法三方不一致。
6. WS 与 gRPC 对同一协议错误的处理策略不一致(WS 继续循环、gRPC 断流);gRPC 断开时不透传数字 code、队列满时断开原因帧可能丢失;心跳 idle 检测存在 TOCTOU 竞态(`heartbeat.go:49-57`)。属传输层语义统一项,建议单独立项。
7. `hub.removeWildcardSub` 恒返回 `last=true` 导致的 broker Subscribe/Unsubscribe 抖动(Task 3 已用引用计数在 broker 层兜底;hub 层计数语义修正为可选优化,不在本方案)。
