# 设计:RPC 一致性 hash 路由与服务注册发现(v1.1)

| | |
| --- | --- |
| 状态 | **待评审**(提案已 Accepted,开发启动以本文档评审通过为准) |
| 日期 | 2026-10-15 |
| 提案 | [docs/proposals/2026-10-15-rpc-consistent-hash-routing.md](../proposals/2026-10-15-rpc-consistent-hash-routing.md)(§10 拍板记录) |
| 排期 | v1.1:2026-11-16 启动(v1.0 发版 2026-11-14 之后),见 §8 |

---

## 0. 输入:已确认决策(提案 §10)

| # | 决策 |
| --- | --- |
| Q0 | 一致性 hash 的目标是** proxy 后端(战斗服)的多实例**,不是 messageloop 节点 |
| Q1 | 路由键默认 `channel`,可配 `metadata:<field>` |
| Q2 | 一期纯一致性 hash;二期 sticky 键目录(`routing.strategy` 预留) |
| Q3 | MVP redis 后端(复用既有 Redis);consul 紧随 |
| Q4 | gRPC 后端先行;HTTP keyed 路由二期 |
| Q5 | `sdks/go` 不内置 Registrar,保持依赖最小;注册方示例进文档 |
| Q6 | messageloop 节点自身不注册进 registry |
| Q7 | 列 v1.1 |
| 补充 | **协同修改 lynx-go/lynx**:Picker 支持路由 key,一致性 hash 上游化为 `KeyedPicker`(§3 给上游规格) |
| 评审补充(2026-10-15) | RPC 的实例选择策略**配置化**:`consistent_hash`(默认)/ `round_robin` / `random`,同一 service 型后端既覆盖有状态(定向)也覆盖无状态(轮转/随机)后端;详见 §4.3 |

## 1. 目标与非目标

**目标**

1. 同一路由 key 的 RPC 在全部 messageloop 节点上确定性路由到**同一后端实例**——纯函数 `f(service, key, snapshot) → instance`,零协调、零节点间流量。
2. 后端实例增减/宕机后路由自动收敛(依赖 registry watch + TTL)。
3. 复用 lynx `contrib/registry` 契约与 redis/consul 后端;一致性 hash 选实例上游化。
4. **静态 `endpoint` 代理、客户端线协议、proto、session→Node 链路全部零改动**。
5. 实例选择策略配置化:有状态后端(战斗服)用 `consistent_hash` 定向,无状态后端用 `round_robin` / `random`——service 型后端同时是这两类负载的服务发现多实例形态。

**非目标**

- 场景状态的迁移与所有权:实例死亡即丢失,恢复是战斗服的责任(提案 Q2 已拍板)。
- sticky 键目录(二期,本期只在配置 schema 预留 `strategy` 值域)。
- HTTP 型后端的 keyed 路由、consul 后端(紧随 MVP,不阻塞)。
- messageloop 节点自注册、`sdks/go` 内置 Registrar(Q5/Q6)。
- pub/sub 数据面:Publication 仍走 broker,与本设计无关。

## 2. 总体架构

```mermaid
flowchart LR
  subgraph battle["战斗服集群(注册方)"]
    B1[B1 实例] -->|Registrar<br/>心跳 10s / TTL 30s / 排水注销| REG[(registry<br/>Redis: ml2/registry/)]
    B2[B2 实例] -->|Register| REG
    B3[B3 实例] -->|Register| REG
  end
  subgraph ml["messageloop 节点 ×N(发现方)"]
    SESS[session.handleRPC] -->|ProxyRPC| NODE[Node.FindProxy]
    NODE -->|Router.Match(glob 不变)| SP[proxy.ServiceProxy]
    SP -->|GetAll(缓存/watch 驱动)| RSLV[registry.Resolver]
    RSLV -->|watch + 轮询兜底| REG
    SP -->|1. Pick(key, 快照)| PICK[KeyedPicker<br/>Rendezvous]
    SP -->|2. connFor(inst)| POOL[per-instance<br/>grpc.ClientConn]
    POOL -->|ProxyService.RPC| B2
  end
```

**关键性质**:路由决策全部落在 `proxy` 包内的新 `Proxy` 实现里。`Router.Match`(`proxy/router.go:75`)、`Node.ProxyRPC`(`internal/runtime/node.go:778`)、session 错误映射(`internal/session/client.go:972-1097`)对 service 型后端**零感知**——它只是又一个 `Proxy`。

**改动面**:

| 层 | 位置 | 改动 |
| --- | --- | --- |
| proxy 包 | `proxy/service.go`(新) | `ServiceProxy`:实现 `Proxy` 全接口,聚合 Resolver + KeyedPicker + conn 池 |
| proxy 包 | `proxy/picker.go`(新,过渡) | 内部 Rendezvous 实现(接口形状与上游规格一致,§3) |
| config | `config/config.go` | `RegistryConfig` 段;`ProxyConfig` 增 `Service`/`Routing` |
| 装配 | `cmd/server/main.go`、`internal/runtime/node.go:734 createProxy` | registry backend + Resolver 构建;`createProxy` 增 service 分支(数行) |
| 上游 | lynx `contrib/registry` | `KeyedPicker` + `RendezvousPicker`(协同 PR,§3) |
| 零改动 | shared/、sdks/、protocol/、session、Server API | — |

## 3. 上游协同:lynx `contrib/registry` KeyedPicker 规格

> 本节是给 lynx-go/lynx 上游 PR 的规格;上游 tag 出来前,本仓 `proxy/picker.go` 按同一规格先行实现,tag 后换薄包装(§7 PR-D)。

```go
// picker.go — 新增(不改既有 Picker 接口)
// KeyedPicker 由调用方提供路由 key,对同一 (key, instances) 集合必须给出稳定映射。
type KeyedPicker interface {
	Pick(key string, instances []Instance) (Instance, error)
}

// RendezvousPicker 返回 Rendezvous(HRW)一致性 hash 实现。
// 映射已冻结,见下方规范;任何变更都破坏跨服务部署,禁止。
func RendezvousPicker() KeyedPicker
```

**映射规范(冻结)**:

- score(instance) = `FNV-1a 64( key + "\x00" + instance.ID )`,选 score 最大者;
- score 并列时按 `instance.ID` 字典序取较小者(冻结 tie-break,保证跨实现一致);
- `len(instances) == 0` → 返回既有哨兵 `registry.ErrNoInstance`;
- 忽略 `Instance.Weight`(v1,与内置 Random/RoundRobin 一致,文档注明);
- FNV-1a 选型:零依赖、跨语言易实现;Rendezvous 对 (key, id) 两两独立打分,对 hash 均匀性不敏感,实例数 <100 时分布足够。

**冻结测试向量**:上游与本仓**双持同一份向量**(固定 key 集 × 固定实例集 → 期望实例),任何一侧改动 hash 编码即红。映射稳定性是跨服务契约(战斗服可能用其他语言重算目录做预分片),不是实现细节。

**不进本 PR 的上游项**:`Resolver` 的 keyed 集成(如 `PickKey(ctx, name, key, filter)`)留给上游后续;本仓自持快照自调 `Pick`,对 Resolver 只用 `GetAll`。

**非键策略零上游依赖**:`round_robin` / `random` 直接复用 lynx 既有 `RoundRobinPicker()` / `RandomPicker()`——只用非键策略的部署不依赖上游 KeyedPicker tag,PR-D 的换接对它们是纯空操作。

**版本路径**:上游 PR → 合并 → `contrib/registry` 出新 tag → 本仓 require 升级。上游节奏不阻塞 v1.1(PR-B 用内部实现先行,PR-D 换接)。

## 4. 组件设计

### 4.1 `ServiceProxy`(`proxy/service.go`)

实现 `proxy.Proxy` 全部接口(`proxy/proxy.go:12`:RPC、Authenticate、SubscribeAcl、PublishAcl、OnConnected、OnSubscribed、OnUnsubscribed、OnDisconnected、AuthenticateAPIKey、Name、Close)。

```go
type ServiceProxy struct {
	name     string
	service  string            // registry 服务名,与注册方 Instance.Name 一致
	timeout  time.Duration     // 缺省 DefaultRPCTimeout,与 GRPCProxy 同
	routing  RoutingConfig     // §4.3
	resolver *registry.Resolver // 节点单例,装配层注入,Close 不归它管
	keyed    registry.KeyedPicker // 键策略(consistent_hash)非 nil;非键策略为 nil
	unkeyed  registry.Picker   // 非键策略(round_robin/random)下 RPC 与无状态族共用;键策略下为 RoundRobin(无状态族用)
	grpcCfg  *GRPCProxyConfig  // TLS/insecure,同 service 全实例共享
	mu       sync.RWMutex
	conns    map[string]*grpc.ClientConn // key = instance.ID
}
```

方法语义按策略分派:

| 策略 | 方法族 | 实例选择 | 理由 |
| --- | --- | --- | --- |
| `consistent_hash` | `RPC` | `keyed.Pick(extractKey(req), snapshot)` | 场景状态在单实例内存,必须定向,且全节点同映射 |
| `consistent_hash` | Authenticate / SubscribeAcl / PublishAcl / On\* / AuthenticateAPIKey | `unkeyed.Pick(snapshot)`(RoundRobin) | 这些调用无场景粘性,任意实例可答 |
| `round_robin` / `random` | **全部方法(含 RPC)** | `unkeyed.Pick(snapshot)`(策略对应 Picker) | 无状态后端:RPC 与其余调用同构,统一轮转/随机;key 提取整体跳过 |

`RPC` 主路径:`resolver.GetAll(ctx, service, Filter{Protocol: grpc})` 拿快照 → 按策略选实例(键策略:提取 key → `keyed.Pick`;非键策略:`unkeyed.Pick`)→ `connFor(inst)` → 复用 `GRPCProxy.RPC` 的 proto 转换与超时逻辑(抽 `grpcCall` 共享 helper,不复制九份)→ 错误按 §4.5 包装。

`Name()` 返回配置 `name`——`ByName`/`api_auth` 显式指派(`proxy/router.go:93`)照常可用;`api_auth` 与 service 型组合**允许**(AuthenticateAPIKey 属无状态族,轮转语义成立),文档注明。

### 4.2 连接管理

- `connFor(inst)`:RLock 查 `conns[inst.ID]`;未命中 `grpc.NewClient(endpoint, dialOpts...)`。`grpc.NewClient` 本身惰性拨号(`proxy/grpc.go:57` 既有用法),首 RPC 才真正建连,无需预热。
- endpoint 取 `EndpointOf(inst, ProtocolGRPC)`:实例 `Endpoints` 里 `protocol=grpc` 的 `address`(禁止裸 `:8080`,registry 契约已保证)。
- **摘除**:每次 RPC 前对快照做差集——不在最新快照里的实例,Close 其 conn 并从 map 删除。在途 RPC 被 cancel → 按 `PROXY_ERROR` 返回,与 fail-fast 语义一致。不自建 watcher:registry-redis 有 events Pub/Sub 推送 + 15s 轮询兜底,`Resolver` 缓存由其驱动,`GetAll` 命中的即是新近快照(stale 上限 60s)。
- conn 数量 = 实例数,不设额外上限;TLS 配置同 service 全实例共享(`grpc.server_name` 对多实例域名的适用性限制写入配置注释)。

### 4.3 路由键提取

```go
type RoutingConfig struct {
	Strategy string // "consistent_hash"(默认)| "round_robin" | "random" | "sticky"(二期,reserved)
	Key      string // "channel"(默认)| "metadata:<name>";仅键策略(consistent_hash / sticky)使用
}
```

| 策略 | 语义 | 适用 |
| --- | --- | --- |
| `consistent_hash`(默认) | Rendezvous 按键定向,全节点同映射 | 有状态后端(战斗服):同场景同实例 |
| `round_robin` | 节点内轮转(每 messageloop 节点独立计数,不跨节点对齐) | 无状态后端,请求均摊 |
| `random` | 每请求均匀随机 | 同上 |
| `sticky` | 键租约目录,扩容不迁移运行中 key(§4.8) | 二期 reserved,配置即报错提示 |

- `channel`(默认):`req.Channel` **原串**,含 namespace 前缀(如 `acme:battle.scene-42`)。跨 namespace 天然分散;同 namespace 内场景 ID 唯一性由业务保证。不剥前缀——剥离规则(按 `:` 切)会把不同 ns 的同段混到同一实例,而保留原串零成本获得隔离。
- `metadata:<name>`:`req.Metadata.Entries[name]`(`Metadata = map<string,string> entries`,`protocol/shared/v2/types.proto:41`)。取不到或空串 → **fallback 到 channel** + `proxy_service_key_fallback_total` 计数。key 恒非空(channel 为必填字段)。
- key 提取**仅在键策略执行**;`round_robin` / `random` 整体跳过(不读 metadata,无 fallback 计数)。
- 客户端线协议与 proto **零改动**(提案已核实两段 wire 都带 metadata)。

### 4.4 配置 schema

```yaml
registry:
  enabled: true
  backend: redis            # redis(本期)| consul(紧随)
  redis:                    # 缺省逐项继承 broker.redis 的连接参数
    addr: 127.0.0.1:6379
    # password / db / pool_size ...
    key_prefix: "ml2:registry/"

proxy:
  - name: battle
    service: acme-battle    # 发现式后端;与 endpoint 互斥
    timeout: 30s
    routing:
      strategy: consistent_hash  # 默认,可省;round_robin / random;sticky 二期 reserved
      key: channel               # 仅键策略使用,默认 channel;metadata:<name>
    grpc:
      insecure: true
    routes:
      - channel: "acme:battle.*"
        method: "*"
```

校验规则(启动即失败,不留运行期惊喜):

1. `service` 与 `endpoint` 互斥,二者必须恰有其一;
2. 存在任一 service 型 proxy 时 `registry.enabled` 必须为 true,且 `backend` 与其子段匹配;
3. `routing.strategy` ∈ {`consistent_hash`(默认), `round_robin`, `random`, `sticky`};`sticky` 本期报错并提示二期;`routing.key` 语法 ∈ {`channel`, `metadata:<非空>`},**仅键策略允许出现**——非键策略下配 key 直接报错(而非忽略,防配置漂移:写错策略名时早失败);
4. `registry.redis` 缺省继承 `broker.redis` 连接参数,显式项覆盖;registry 用独立 `*redis.Client` 实例(连接池隔离,注册轮询不与数据面抢池);
5. `registry` 与 `cluster.enabled` **解耦**——单节点部署 + 多战斗服实例同样成立。

### 4.5 错误路径

| 场景 | 内部错误 | 客户端可见 | 运维信号 |
| --- | --- | --- | --- |
| 路由没配(既有语义) | `proxy.ErrNoProxyFound` | `NO_PROXY`(不变) | 配置错误 |
| service 无健康实例 | `proxy.ErrNoHealthyInstance`(新哨兵) | **`PROXY_ERROR`**(复用,不新增客户端错误码) | `proxy_service_no_instance_total` + Warn 日志 |
| 实例连接失败 / 在途摘除 | grpc err 包装 | `PROXY_ERROR`(既有路径) | 既有 |
| 后端超时 | 既有 | `RPC_TIMEOUT` | 既有 `RPCDuration` |

不新增客户端可见错误码的理由:客户端对"无实例"无差异化处理;`NO_PROXY` 保留给"配置缺路由"(排障语义不同);部署问题(后端全灭)靠指标与日志区分。

### 4.6 可观测性

| 指标 | 类型 | 标签 |
| --- | --- | --- |
| `proxy_service_instances` | gauge | `service`(快照实例数,含 0) |
| `proxy_service_no_instance_total` | counter | `service` |
| `proxy_service_key_fallback_total` | counter | `service`(仅键策略) |
| `proxy_service_membership_events_total` | counter | `service`, `event`∈{join, leave} |

membership 变化打 Info 日志(instance ID、endpoint、event);RPC 侧沿用既有 `RPCDuration`(`proxy` 标签即配置 name)。

### 4.7 生命周期与装配(`cmd/server/main.go`)

1. `registry.enabled` → 构建 redis client(独立实例,§4.4-4)→ `registryredis.New(client, WithKeyPrefix(...))` → `resolver := registry.NewResolver(disc)`(节点单例;ServiceProxy 共享,内部按 service 各自缓存 + watch goroutine);
2. `Node.SetupProxy`(`internal/runtime/node.go:721`)不变;`createProxy`(`:734`)增加分支:`cfg.Service != ""` → `proxy.NewServiceProxy(cfg, resolver)`(resolver 经 `SetupProxy` 参数传入,或装配层预构造注入——取后者,`SetupProxy` 签名加一个 `ServiceResolver` 参数即可,不把 registry 依赖埋进 Node 的构造逻辑);
3. shutdown:既有 `Router.Close()` 链路(`proxy/router.go:137`)会调 `ServiceProxy.Close`(关全部 conn);resolver 由 main 的 defer 关闭。

不进 `Cluster` facade:registry 是 proxy 后端发现,不是集群组件,与会话租约/命令总线无关。

### 4.8 二期预留:sticky 键目录(本期只留缝)

`routing.strategy` 值域本期锁 `{hash}`。sticky 的机制概要(二期细化):Redis 租约目录 `ml2:registry/dir/<service>/<key>` `SET NX` 指向属主实例 + TTL 续约,hash 只做冷启动放置;实例扩容不迁移运行中 key,实例死亡 TTL 过期后重放置——与 v2 kernel「单激活 + Directory」已裁决哲学同构(`docs/v2/kernel-architecture.md:735`)。

## 5. 注册方(战斗服)契约

| 项 | 契约 |
| --- | --- |
| `Instance.Name` | 服务名,与 `proxy.service` 配置一致 |
| `Instance.ID` | 稳定实例标识(pod 名 / 启动生成 UUID);ID 参与 hash,变更即换属主,**运行期不得变** |
| `Endpoints` | `[{Protocol: "grpc", Address: "host:port"}]`(禁止裸端口,registry 契约) |
| `Version` / `Tags` | 版本灰度与环境隔离(`Filter.Tags`);本期 messageloop 不按 Version 分流 |
| 心跳 | lynx Registrar 默认 10s,TTL 30s;graceful shutdown 走排水注销,SIGKILL 兜底 = TTL 过期(≤30s 摘除) |
| 健康语义 | redis 后端**无被动探针**:崩溃即 TTL 摘除;「活而不健」不摘除,由 RPC 超时兜底(提案风险已接受) |

Go 战斗服(lynx 应用)接入为三行级:

```go
reg := registrar.NewRegistrar(registryredis.New(rdb, registryredis.WithKeyPrefix("ml2/registry/")),
    registrar.WithServiceName("acme-battle"),
    registrar.WithAdvertisers(registry.GRPC(grpcServer)),
)
app.Register(reg) // 心跳/readiness/排水注销由 lynx 生命周期托管
```

非 Go / 非 lynx 注册方:按 registry-redis 键布局直写(`instance/{service}/{id}` JSON + `SET EX` + `events/{service}` PUBLISH),或引 lynx 为库;完整示例文档随 PR-C 落 `docs/developer/03-server-api.md`。

## 6. 测试策略

- **单测**:Rendezvous 冻结向量(与上游规格同向量双持);key 提取(channel / metadata 命中 / 空 fallback,仅键策略);策略分派(`round_robin` 下 RPC 与无状态族统一轮转、key 提取跳过;`random` 返回合法实例);`ErrNoHealthyInstance` → `PROXY_ERROR` 映射;配置互斥、strategy 值域与「非键策略配 key 报错」校验。
- **集成**(redistest 探测 127.0.0.1:6379,缺席静默跳过;CI `MESSAGELOOP_TEST_REDIS_REQUIRED=1`):
  - 单节点 + 3 个 fake 实例:同 key 多次请求全落同一实例;扩到 4 实例后仅约 1/4 的 key 换属主(Rendezvous 最小扰动断言);注销 1 实例后其全部 key 漂移到确定的新属主;无实例时全部请求得 `PROXY_ERROR` 且计数器走字。
  - 双 messageloop 节点(复用 `sdks/go` e2e 的真实进程拉起模式):断言两节点对同 key 收敛后选同一实例。
- **回归**:静态 `endpoint` 代理与 `api_auth` 指派行为不变。

## 7. PR Plan

| PR | 内容 | 依赖 | 估(人周) |
| --- | --- | --- | --- |
| **PR-0(上游)** | lynx `contrib/registry`:`KeyedPicker` + `RendezvousPicker` + 冻结向量(协同仓库,合并后出 tag) | — | 0.3(并行) |
| **PR-A** | lynx 升级 v1.17.0→v1.18.0(含 contrib/zap)+ `registry` 配置段 + redis backend/Resolver 装配(**无路由行为变化**) | — | 0.5 |
| **PR-B** | `ServiceProxy` + 策略分派(`consistent_hash` 内部 Rendezvous(上游形状)/ `round_robin` / `random`)+ conn 管理 + `channel` 路由键:hash 定向纵切可用 | PR-A | 1.5 |
| **PR-C** | `metadata:<name>` 键 + 指标 + 集成/双节点 e2e + 文档(03-server-api、04-cluster、deployment) | PR-B | 1.0 |
| **PR-D(换接)** | 上游 tag 后改薄包装调 `registry.RendezvousPicker()`,删内部实现(向量测试保留防上游漂移) | PR-0 出 tag | 0.2 |
| PR-E(紧随,不阻塞 v1.1 可用性) | consul 后端 + HTTP 型 keyed 路由 | PR-C | 后续排 |

## 8. 排期(v1.1)

锚点:v1.0 发版 **2026-11-14**;v1.1 开发 **2026-11-16 启动**(W47),1 名熟手,净投入约 **4 人周**(PR-0 上游协同并行,不计入关键路径;PR-D 待上游 tag,可滑入 buffer)。

| 周 | 日历 | 内容 | 里程碑 |
| --- | --- | --- | --- |
| W47 | 11-16 ~ 11-20 | PR-A;PR-0 上游 PR 提出 | **M1** 依赖与装配落地 |
| W48 | 11-23 ~ 11-27 | PR-B(hash 定向单节点可用) | **M2** 纵切可用 |
| W49 | 11-30 ~ 12-04 | PR-C(集群 e2e + 文档;上游 tag 出则 PR-D) | **M3** 集群验收 |
| W50 | 12-07 ~ 12-11 | buffer:hardening、评审遗留、上游换接兜底 | **v1.1 RPC 路由可用(2026-12-11)** |

**发版门禁(v1.1 本特性)**:

- `go test ./...` + 相关包 `-race`;redis 集成测试在 CI REQUIRED 模式跑过;
- 冻结向量双持(本仓 + 上游)一致;上游未出 tag 前 PR-D 不合并亦可发(v1.1 以内部实现为准,PR-D 为纯替换);
- 静态 endpoint 代理、`api_auth`、既有错误码路径零回归;
- `docs/developer/03-server-api.md`、`04-cluster.md`、`docs/deployment.md` 与实现一致(注册方契约、TLS 多实例限制、无被动探针取舍均成文)。

## 9. 设计开放问题(评审时定,不阻塞骨架)

1. **`GetAll` vs 自建 watcher**:§4.2 已决策全走 `Resolver` 缓存(redis 后端有 Pub/Sub 事件驱动);consul 后端(PR-E)是 blocking query,同样由 Resolver 驱动——若实测摘除延迟不可接受,再考虑 Subscribe 驱动主动摘除,缝已留。
2. **`SetupProxy` 传 resolver 的签名形态**:加参数 vs 装配层预构造 map[name]Proxy 注入。倾向加参数(改动最小),评审确认。
3. **同一实例多 Endpoints / 多协议**:契约允许,本期 `Filter{Protocol: "grpc"}` 固定取 grpc;HTTP keyed(PR-E)再放开。
