# Admin API 的 API Key 鉴权与按 Namespace 权限控制

| 字段 | 值 |
| --- | --- |
| 文档标题 | Admin API Key 鉴权与按 Namespace 权限控制（proxy 托管 / Torchwood Key 复用） |
| 作者 | qiulin + agent 会话设计（四轮迭代收敛） |
| 日期 | 2026-09-16 |
| 状态 | 已实现（dogfooding） |
| 仓库路径 | `docs/design/2026-09-16-admin-api-key-authz.md` |
| 涉及仓库 | messageloop（本仓库）、mlbridge、Torchwood（三仓协同） |
| 阶段前提 | dogfooding：重点验证并补全机制缺口，**不考虑向后兼容**（对齐 KD-K31"无兼容期"先例） |
| 实现还原点 | S1 875c6ad / S2 9531a0f / S3 c6fc04f / S4 8aff6a7 / S5+补遗（分支 feat/admin-api-key-authz） |
| 修订 | 2026-09-16 rethink 复查：修正 4 项（key_id 唯一性 / scope 抹空短路 / fail-fast / 长度门顺序）、补充 2 项（auth_tokens 列表 / JWT 否决留档），见 D26-D29 |
| 实现澄清 | ① 矩阵中 Disconnect 不可见 session 的落地形态是 results **无该键**（强于 false，不泄露存在性；Go map 读取语义等价 false）；② G7 两个指标（admin_auth_requests_total + admin_rpc_total{method,key_id,result∈denied/ok/error}）均已实现；③ T4 落地为 whoami 端点（见 T4 行修订）——原"verify + apikeys.verify scope"方案因 TW 已退役 apikeys scope 而否决；④ **whoami 线上 JSON 为 snake_case**（`key_id/project_id/max_age_seconds`）——TW grpc-gateway 全局 `UseProtoNames: true`，冻结契约的 camelCase 假设错误，mlbridge 已按实际形状解析并加回归测试；⑤ T3 平台级 key 剪裁（`api_keys.project_id NOT NULL` 需迁移，运维暂用静态 token）；⑥ T5 零改动——TW project id 生成规则（`^[a-z][a-z0-9]{0,27}$`）已是 namespace 语法严格子集，加不变量测试防未来放宽 |
| 三仓还原点 | messageloop `feat/admin-api-key-authz`（本分支，已推送）；mlbridge `feat/authenticate-admin` 6e7eb6b+b47aaae；torchwood `feat/admin-key-whoami` 5573ac60（后两者本地未推送） |

---

## 一、现状与问题

### 1.1 Admin API 现状（勘察结论，均可在代码中复核）

服务端 API 指 `messageloop.server.v2.APIService`（8 个 RPC：Publish / Disconnect / Subscribe / Unsubscribe / Survey / GetPresence / GetHistory / GetChannels），监听 `server.grpc_admin.addr`，处理器在 `internal/admin/api_handler.go`，与客户端流共享同一 `Node`。

- **鉴权**是单一静态 Bearer token：`server.grpc_admin.auth_token` + unary 拦截器 `AdminAuthInterceptor`（`pkg/transport/grpc/server.go`），或显式 `allow_insecure`（非 loopback 仅 WARN）。
- **授权**两层、全部节点级全局：能力位闭集 `server.grpc_admin.capabilities`（9 位，`internal/authz/authorizer.go` ClosedCapabilityNames）+ Authorizer 表按 channel 模式裁决。所有调用方共用固定 principal `"admin"`（`internal/runtime/cluster_commands.go` adminPrincipal）。
- 请求中的 `namespace` 字段只是 user→sessions 展开的**参数**，不是授权范围；admin 面对 channel 引用**没有任何 namespace 校验与语法门**。
- 客户端面对照：namespace 约束严密且结构性收敛——`checkNamespace`（`internal/session/namespace.go`，会话边界 fail-closed）+ 单一入口 `precheckNamespace` + census 测试（其注释记载的 "mechanism gap G1, review #4"：散点式检查被证明会漏，已结构性修复）。
- 集群会话租约 `ClusterSessionLease` 带 `Namespace` 字段（本地 hub 与 Redis 目录双路径），session 寻址操作可据此做 namespace 判定。
- 无配置热加载（`Authorizer.ReplaceRules` 无生产调用方），一切配置启动时加载。
- Torchwood（dokploy 栈）：**单项目本身支持多个 API Key**；项目 Key（`sk-...`，哈希落库）带固定 scope（`users.read`、`databases.read/write` 等）；Server API 有 30s 校验缓存。mlbridge 以 ProxyService gRPC 承接 `$authenticate` → Torchwood `tokens:verify`，客户端凭证即 TW access token，会话 namespace = TW project id。

### 1.2 问题

1. 所有 admin 调用方共享同一身份与全部权限；多租户（namespace = 租户）无法给租户侧后端发放仅限本 namespace 的服务端 API 凭证。
2. 静态 token 一发即超管，无法按调用方撤销、按调用方审计。
3. admin 面的授权执行是**散点式**（每个 handler 记得自己查什么）——与客户端面已结构性修复的同类缺口同型，是本设计要补的机制缺口主体。

### 1.3 成功标准

- 机制：census 双测试红线（新 RPC / 新请求字段未归类即测试红）；scope 层结构性保证 handler 看不到越界目标。
- 行为：namespace 限定 Key 的越界访问逐格符合 §2.4 拒绝语义矩阵；撤销传播上界 = 缓存 TTL；proxy 宕机 fail-closed 且静态 token 通道不受影响。
- 运营：配置文件与日志均不出现 Key 明文；每条 admin 日志/指标可归因到 key_id。
- dogfooding 不设"逐字节兼容"硬线，但现有测试须通过或被有意更新。

---

## 二、目标设计

### 2.0 全链路（成链后的完整回路）

```
租户后端                     messageloop                mlbridge              Torchwood
   │ Publish(acme:chat.x)      │                          │                     │
   ├─ Bearer sk-tenant ───────►│ authn拦截器: 非静态token → ≥20字符门（proxy 路径专用）          │
   │                           │ resolver: sha256(sk) 缓存命中? ──── 命中: 直接放行（≤TTL 内零下游开销）
   │                           │ ├── miss ──────────────►│ AuthenticateAdmin ─►│ verify(sk)
   │                           │ │                        │ 前缀过滤/映射       │ ← {project:acme, scopes, max_age}
   │                           │◄── AdminIdentityInfo ───┤                     │
   │                           │ 钳制(caps⊆节点上限, ns语法) + 缓存(ttl=min(cfg,max_age))
   │                           │ scope层: 能力表 + ValidateChannel + namespace矩阵改写
   │                           │ handler: 只见 acme: 内的目标（scope-free）
   │◄─ 按 §2.4 矩阵返回 ───────┤                          │                     │
   │                           │  (TW 吊销 sk-tenant → ≤TTL 后全拒)               │
```

### 2.1 身份模型（internal/authz 新增）

```go
// AdminIdentity 是一次 admin API 调用的已认证身份。
type AdminIdentity struct {
    KeyID      string       // TW 侧 Key 显示名；静态 token 为 "static-token"；allow_insecure 为 "insecure"
    Namespaces []string     // ["*"] 或精确列表；空列表 = 拒绝一切（fail-closed）
    Caps       Capability   // 已被节点上限钳制
}
func (i AdminIdentity) Principal() Principal  // UserID = "key:"+KeyID；静态 token/insecure 为 "admin"
func (i AdminIdentity) AllowsNamespace(ns string) bool
func (i AdminIdentity) AllowsChannel(ch string) bool  // topics.NamespaceOf 合法且在范围内；解析失败即 false
```

静态 `auth_tokens` 列表（≥20 字符/把，Validate 强制）→ 任一命中即超管身份（`Namespaces:["*"]`，Caps=节点上限），语义冻结：它是 mlbridge 的生产通道 + 运维 break-glass，不是兼容遗产。`allow_insecure` → `KeyID:"insecure"` 超管。

**正交性原则（整个设计的地基）**：Key 的 project 绑定决定"能在哪"（namespace）；Key 上的标签决定"能做什么"（能力位）。两者永不混在一个字段。platform 级 Key（无 project 绑定）= `["*"]`。

### 2.2 Proxy 契约（protocol/proxy/v2/proxy.proto，纯增量）

```protobuf
service ProxyService {
  // ...现有 8 个 RPC 不动...
  rpc AuthenticateAdmin(AuthenticateAdminRequest) returns (AuthenticateAdminResponse);
}

message AuthenticateAdminRequest {
  string api_key = 1;       // 调用方出示的完整 Key 明文（由 proxy 后端校验）
  string remote_addr = 2;   // 审计用
}

message AdminIdentityInfo {
  string key_id = 1;             // 后端 Key 的唯一 ID（非显示名：allow 列表按 "key:<id>" 匹配，
                                 // 显示名不唯一会让两把 Key 共享 principal 串权限——D26）
  repeated string namespaces = 2;// ["*"] = 全部；空列表 = 拒绝一切（fail-closed）
  repeated string capabilities = 3; // 能力位名称闭集；空 = 零能力
  int64 max_age_seconds = 4;     // 相对字段：本验证结果至多再用多久；0 = 用服务端配置 TTL
}                                  //（相对时间规避两系统时钟偏斜——绝对时间已否决）

message AuthenticateAdminResponse {
  messageloop.shared.v2.Error error = 1;
  AdminIdentityInfo identity = 2;
}
```

`proxy.Proxy` 接口、`GRPCProxy`、`HTTPProxy`（protojson 解析，与 Authenticate 同法）同步实现。**messageloop 不感知 Torchwood，不感知 `sk-` 格式，不规定 Key 格式**——线上契约收到的始终是能力位闭集名（`history.read`…），标签约定完全活在 TW ↔ mlbridge 层。

### 2.3 认证解析器（internal/admin/auth.go）

```go
type adminAuthResolver struct {
    findProxy   func() proxy.Proxy        // admin_auth 指派的代理（显式，非路由 glob）
    ceiling     Capability                // 节点能力上限
    ttl         time.Duration             // server.grpc_admin.admin_auth_cache_ttl，默认 30s
    negativeTTL time.Duration             // 常量 5s，不配置化
    // 缓存 key = sha256(presented)，值只存 identity+过期时刻，永不存 Key 明文
    // 容量 1024（正负合一）随机驱逐；同 Key 并发去重（~25 行 in-flight map，不引依赖）
    // 熔断：连续 5 次 proxy 错误 → 30s 快速失败（D27，proxy 宕机不得挂满 rpcTimeout）
}
```

拦截器流程（顺序有讲究：静态 token 比较在长度门**之前**，短静态 token 不被误杀——D28）：

1. 取凭证：`authorization: Bearer <key>` 优先，缺失看 `x-api-key`；
2. 无凭证 → 静态 token 已配？`Unauthenticated`；`allow_insecure`？注入 insecure 超管身份放行；
3. 有凭证 → 先比静态 tokens 列表（逐个常数时间，本地零开销），命中即超管身份；
4. 未命中 → 有 `admin_auth` 指派代理？无 → `Unauthenticated`；有 → 长度门（< 20 字符直接拒 + 负缓存，不触 proxy）→ `Verify`：缓存命中直接返回；miss 去重后调 proxy；成功 → 钳制后入缓存，TTL = `min(配置 TTL, max_age)` 下限 1s；明确拒绝 → 负缓存 5s；**代理错误（含超时/Unimplemented）= fail-closed 且 fail-fast**：错误结果进 2s 短负缓存，连续 5 次错误熔断 30s（熔断期内直接 `Unauthenticated` 快速失败，消息注明 proxy 不可用）——否则 proxy 宕机时每条调用挂满 rpcTimeout（默认 30s），fail-closed 退化为 DoS 放大（D27）。

**钳制规则**（信任边界）：`capabilities` 逐名映射闭集，未知名 WARN 丢弃（版本偏斜容忍），全部未知 → 零能力；`namespaces` 逐个过 `topics.ValidateNamespace`，非法项 WARN 丢弃，`"*"` 只允许单例，清洗后为空 → 零范围身份 + WARN。**上限之内的授予是 proxy 的权限，上限本身永远是服务端配置的。**

**信任分析**：被攻破的 mlbridge 可伪造 `["*"]`+全能力——但它本就持有静态超管 token，妥协面未扩大；钳制的意义是防误配，不是防妥协。

### 2.4 授权执行链：scope 层 + 拒绝语义矩阵

`internal/admin/scope.go`：RPC 进 handler 之前的单一 choke point——能力表求值（`map[方法]func(req) Capability`，条件位如 Publish 带 sessions 才要 session.act 由表内函数读请求得出）+ 请求遍历改写（channel 列表过滤越界项、session 列表按租约 namespace 抹为不存在、namespace 参数越界整体拒、全部 channel 引用过 `topics.ValidateChannel`）。**handler 对 scope 零感知**：不可见语义由结构保证——handler 收到的请求里根本没有越界目标。

拒绝语义矩阵（行为契约；原则一句话：**命名参数越界拒绝（显式、可诊断），ID 寻址的数据越界不可见（不泄露存在性）**）：

| 载体 | 越界时的行为 | 对齐的现有语义 |
|---|---|---|
| namespace 参数（users/user_id 非空时的 req.namespace） | 整个 RPC `PermissionDenied` | 与 capabilities 缺位相同的请求级拒绝 |
| channel 引用（Publish.channels） | 该条计 failed（partial-success 继续） | 与 AdminCanPublish 拒绝一致 |
| channel 引用（Subscribe/Unsubscribe.channels） | `results[ch] = false` | 与现有 per-channel 结果一致 |
| channel 引用（Survey/GetPresence/GetHistory 单 channel） | RPC `PermissionDenied` | 与现有 ACL 拒绝一致 |
| session ID 引用（sessions/session_id） | 不可见：publish 跳过、disconnect/subscribe 结果 false | 与"session 不存在"一致——不向租户 Key 泄露其他租户 session 存在性 |
| 整个请求只剩越界目标（如 Subscribe 仅带一个跨 ns 的 session_id，被抹后为空） | **scope 层短路合成 not-found 响应**（Subscribe/Unsubscribe：results[每个 channel]=false；Publish：计 attempted+failed），不进 handler | 若进 handler 会触发"session_id/user_id 不得同时为空"的 InvalidArgument——既破坏 not-found 语义又泄露"被特殊处理过"（rethink 修正 2） |

**全局面语法门（收紧）**：所有 admin 调用方（含静态 token/`["*"]` 身份）的 channel 引用一律过 `ValidateChannel`，不合法 → `InvalidArgument`——admin 面与客户端面共享同一 channel 语法（`ns:topic`），死通道从静默浪费变成显式报错。

Node 侧（签名原地替换，编译器即迁移清单，无兼容变体）：`AdminCanPublish(p, ch)` / `AdminCanSubscribe(p, ch)` / `SubscribeSession` / `UnsubscribeSession` / `AdminDecide` 全部收 principal 入参；新增 `AdminSessionNamespace(ctx, sessionID) (string, bool)`（本地 hub 优先，未命中查集群目录）。GetChannels 按身份 namespace 过滤。**deny 不可打洞**对一切身份成立：Key 的 channel 照常过全局 Authorizer 表。per-Key 差异化细粒度（可选高级用法）：`Principal.UserID = "key:<id>"` 复用 `allow_publish` 等 allow 列表，零新机制。

### 2.5 机制缺口清单与结构性补全（本设计的核心增量）

| # | 缺口（怎么漏） | 补全机制（结构上怎么防） |
|---|---|---|
| G1 | 执行点散布：8 RPC × 4 类 namespace 载体靠每个 handler 记得 | 请求遍历式 scope 层（§2.4），handler scope-free |
| G2 | 覆盖面漂移：walker 也会漏 | 双层 census 测试：① 反射枚举 APIServiceServer 全部方法，断言每个都在 walker 分类表；② 反射枚举全部请求消息字段，断言每个被处理或在显式豁免清单 |
| G3 | 隐式激活：admin 认证靠 glob 路由会被现有 `method:"*"` 无声接管 | proxy 条目显式 `admin_auth: true` 唯一指派；两处指派 = Validate 报错 |
| G4 | channel 语法无门：admin 面接受任意 topic 串 | 全局面 ValidateChannel（§2.4 末） |
| G5 | fail-open 启动门：allow_insecure + 非 loopback 仅 WARN | fail-closed 启动：非 loopback 绑定 + allow_insecure → Validate 报错（admin HTTP 同规则对齐） |
| G6 | 能力位检查散布：requireAdminCaps 靠每个 handler 记得 | 声明式能力表集中于 scope 层，handler 内检查全部删除；census ① 覆盖 |
| G7 | 身份不可归因 | key_id 贯穿：context logger 每条 admin 日志带 key_id；指标 `admin_rpc_total{method,key_id,result}` + `admin_auth_requests_total{verifier,key_id,result}`；跨 Key 同 key_id 首现 WARN |
| G8 | 时钟偏斜（绝对过期时间在两系统间有偏斜） | 相对字段 `max_age_seconds`（§2.2） |
| G9 | Key 喷射打 proxy | 入口长度门（≥20 字符）+ 负缓存 5s + 容量 1024 随机驱逐 + 同 Key 去重，四层限幅 |

（G1/G2 是客户端面 "mechanism gap G1, review #4" 先例的 admin 面移植——同类缺口用同类结构性答案。）

### 2.6 Torchwood 标签机制：服务前缀自定义 scope

TW 的 scope 从固定枚举升级为**带语法的自由字符串**，一条规则通吃所有未来服务：

```
scope     := <service> "." <name>
service   := [a-z][a-z0-9-]{1,31}     # 服务命名空间，≥2 字符
name      := [a-z0-9_.-]{1,40}        # 语义归服务自己解释
总长 ≤ 64；小写；Key 创建/编辑时校验
```

- TW 内建 scope（无前缀，如 `users.read`）保持原样、不可被自定义覆盖——**无前缀 = TW 自己的，有前缀 = 服务的**，命名天然不冲突。
- TW 只做语法校验和存储，不解释服务 scope 语义；UI 自由输入 + 常用建议列表。
- **耦合是命名约定，不是代码表**：mlbridge 无翻译表，只做前缀过滤（约十行）：`caps = [strip("messageloop.", s) for s in scopes if hasPrefix(s, "messageloop.")]`。
- 三层知识边界最小：**TW 存任意合法语法；mlbridge 只认 `messageloop.` 前缀；messageloop 只认闭集名。**

messageloop 标签清单（= `messageloop.` + 能力位闭集名，一一对应，不多不少）：

| 标签 | 控制什么 |
|---|---|
| `messageloop.session.act` | 代会话定向操作（向 session 投递 / 断开 / 代订阅） |
| `messageloop.user.fanout` | 按 user 展开（叠加在 session.act 之上） |
| `messageloop.subscribe.any` | 代订阅越过 allow 列表（deny_all 仍不可打洞） |
| `messageloop.history.read` | GetHistory |
| `messageloop.presence.read` | GetPresence |
| `messageloop.channels.list` | GetChannels（结果仍按身份 namespace 过滤） |
| `messageloop.survey.bypass_gate` | Survey 绕过人口门控 |
| `messageloop.presence.large_snapshot` | 完整 presence 快照（默认按上限截断） |

**两个刻意不存在的标签**：① 没有 `messageloop.publish`——channel 发布不受能力位门控（Publish 只有 sessions/users 目标才查 caps），其控制单元就是 namespace 范围 + Authorizer 规则；租户"只发消息"的 Key = project 绑定 + 零标签，天然成立；按 Key 细分发布权限用 allow_publish 列表的 `key:<id>` 集成。② 没有 `messageloop.all` 通配——显式勾选，通配毁审计。零 messageloop 标签的 Key 合法：认证通过、全 RPC PermissionDenied（Key 可能同时服务其他系统）。

**Key 复用裁决**：复用 TW 的 Key **体系**（类型、`sk-` 格式、哈希落库、生命周期），通过"一用途一 Key"避免复用 Key **实例**（爆炸半径控制；TW 单项目多 Key 已具备，零机制成本）。**mlbridge 自身的 admin 调用永久走静态 token**（D25，依赖环分析见 §3）。

### 2.7 配置最终形态

```yaml
server:
  grpc_admin:
    addr: ":9091"
    auth_tokens:                    # 运维/mlbridge 通道：超管列表，语义冻结。列表形态让轮换无空窗：
      - "..."                       # 加新 → 滚动重启 → 删旧（rethink 补充 5）。每把 ≥20 字符（Validate 强制）
    admin_auth_cache_ttl: "30s"    # 正缓存 TTL
    capabilities: [...]            # 节点上限（能力位闭集名）
    allow_insecure: false          # 非 loopback 绑定时配 true = Validate 报错（G5）
proxy:
  - name: mlbridge
    admin_auth: true               # G3：显式指派，全配置唯一
    grpc: {...}
    routes: [...]                  # 只服务客户端面，与 admin 认证解耦
```

envconfig 增加 `admin_auth_cache_ttl` 一行映射。非 TW 部署的独立 messageloop：无 `admin_auth` 指派则 Key 认证功能不存在，静态 token 照常。

---

## 三、关键决策及理由

### 3.1 四轮演进与被否方案（考古价值：为什么不是别的）

| 版本 | 方案 | 结局 | 被否理由 |
|---|---|---|---|
| v1 | messageloop 静态配置 API Key（YAML 存 SHA-256 哈希 + `cmd/mlkey` 生成工具） | **推翻** | 凭证权威（Torchwood）与凭证存储（messageloop YAML）分裂两处；每把 Key 的发放/撤销都要改配置重启，租户自助走不通 |
| v2 | Key 校验托管 proxy（mlbridge→TW），`$authenticate_admin` 走 glob 路由 | 部分推翻 | 托管方向正确（保留）；glob 路由会被现有 `method:"*"` 无声接管（G3）；`expires_at_unix` 绝对时间有时钟偏斜（G8）；mlbridge 翻译表多余（v4 命名约定替代） |
| v3 | 机制缺口结构化（scope 层、census 双测试、签名替换、fail-closed 启动门） | 保留为骨架 | — |
| v4 | 复用 TW Key 体系 + 服务前缀自定义标签 + 三仓协同 | **当前定稿** | — |

### 3.2 最终决策表

| # | 分叉点 | 裁决 | 理由（含否掉的选项） |
|---|---|---|---|
| D1 | 覆盖哪个面 | 仅 Admin gRPC API | 客户端面 token→auth-proxy 模式天然支持任意凭据（含 API Key），proxy 侧实现即可；admin HTTP 只有 /health 与 /metrics，无 namespace 概念 |
| D2′ | Key 校验在哪 | proxy 托管（mlbridge→TW 闭环） | 见 §3.1 v1 被否理由；与客户端凭证同构，messageloop 零凭证存储 |
| D5 | namespaces 缺省语义 | fail-closed：proxy 返回空 = 零范围 | namespace 是新维度，本仓库对它的一贯哲学是 fail-closed（NAMESPACE_MISMATCH / NAMESPACE_REQUIRED） |
| D6 | 越界拒绝形态 | 参数拒绝、数据不可见（§2.4 矩阵） | 对齐现有同类拒绝语义 + 不泄露存在性；统一"全 RPC 拒"会破坏 Publish 的 partial-success 契约 |
| D8 | Key 与 Authorizer allow 列表关系 | `Principal.UserID = "key:<id>"` 复用 allow 列表 | "每 Key 独立规则表"违背 KD-K10（一张表、一个 Decide）；复用零新机制，纯增量 |
| D14 | 复用 Authenticate 还是新 RPC | 新增 `AuthenticateAdmin` | 响应契约不同构（多 namespace + 能力名 + key_id + max_age 在 UserInfo 无处安放）；复用需 client_type 魔法值分支 |
| D15 | 缓存与撤销上界 | 正缓存 TTL 配置化（默认 30s）+ max_age 钳制；负缓存固定 5s | 撤销上界 = TTL 是文档承诺；负缓存防坏 Key 喷射。否"每次都验"（延迟/压力）与"长 TTL"（撤销失控） |
| D16 | proxy 不可用时 | fail-closed，静态 token 不受影响 | 与客户端认证 fail-closed 一致；静态 token 是设计内的兜底通道。fail-open = 撤销失效 |
| D17 | 能力/范围信任钳制 | 服务端上限永远钳制 proxy 授予 | proxy 决定"你是谁"，服务端配置决定"任何人最多能做什么"；防误配（防妥协不指望它，mlbridge 本持超管 token） |
| D18′ | 功能开关 | proxy 条目显式 `admin_auth: true` | v2 的路由即开关有 G3 隐式激活问题；显式指派唯一、可校验 |
| D20 | Key 类型：新建 vs 复用 TW | 复用体系 + 一用途一 Key 实例隔离 | 两套 Key 体系是 dogfooding 负债；"messageloop 专用 Key 类型"会把词汇表泄漏进 TW 模型 |
| D21 | 标签：内建枚举 vs 自定义 | 自定义 scope + `<service>.<name>` 语法 + 命名约定 | 耦合从代码表降为命名约定，mlbridge 零配置，未来服务直接接入；TW 对服务语义无知 |
| D22 | 标签形态 | `messageloop.` + 能力位闭集名，精确匹配 | 与闭集一一对应，messageloop 契约零改动；`messageloop.all` 通配毁审计 |
| D23 | 要不要 `messageloop.publish` | 不要 | 发布的控制单元是 namespace 范围 + Authorizer 规则（含 per-Key allow_publish 集成），加标签 = 同一控制点两个机制 |
| D24 | 零标签 Key | 合法，认证通过、全 RPC 拒绝 | Key 可同时服务其他系统；也给了"先发 Key 后授权"的运营节奏 |
| D25 | mlbridge 自身 Key 化 | 不，永久静态 token | 依赖环：mlbridge 是认证路径组件，Key 化它 = 它宕机时连它自己的 admin 调用也认证不了 |
| D13 | proto 改动面 | 仅 proxy.proto 增量；server api.proto / SDK / 集群协议零改动 | 身份走 metadata，授权走服务端配置；改 server proto 波及四端 SDK，无必要 |
| D26 | key_id 用显示名还是唯一 ID | 唯一 ID | allow 列表按 `"key:<id>"` 匹配 principal，显示名是用户自由文本、不唯一——两把同名 Key 会共享 principal 串权限；日志/指标也用唯一 ID 保证可归因（rethink 修正 1） |
| D27 | proxy 错误路径的失败速度 | fail-closed **且 fail-fast**：错误进 2s 短负缓存，连续 5 错熔断 30s | 只 fail-closed 不 fail-fast 时，proxy 宕机 = 每条 Key 型调用挂满 rpcTimeout（默认 30s），时间维度上 fail-closed 退化为 DoS 放大（rethink 修正 3） |
| D28 | 静态 token 形态与拦截器门序 | `auth_tokens` 列表（轮换无空窗：加新→滚动→删旧）+ 每把 ≥20 字符进 Validate + 长度门仅作用于 proxy 路径 | 单值 token 轮换必有"改配置→重启"期间新旧不同步的断连窗口，而静态 token 是 mlbridge 生命线；长度门若排在静态 token 比较之前会误杀短 token（rethink 修正 4 / 补充 5） |
| D29 | 自包含凭证（JWT/JWKS 本地验签） | 否决，留档 | 本地验签无网络依赖是真实优点，但撤销语义弱化为等 exp、为 messageloop 引入 JWKS 新机制、且 TW opaque Key 复用是 D20 既定裁决；proxy 可用性顾虑已由 D27 熔断缓解 |

no-compat 简化（dogfooding 专属）：旧拦截器直接删除（不留 Deprecated）；Node 方法签名原地替换（不留 For 变体）；允许 G4/G5 收紧现行为。

---

## 四、分步实施计划

### messageloop（两步，Step 1 一次合入——authn 与 authz 分离上线在任何中间态都是越权窗口）

- **Step 1（核心全链路）**：proxy.proto 契约 + 生成物 + 双实现与契约测试 → `authz.AdminIdentity` → `internal/admin/auth.go`（resolver/拦截器/钳制/缓存/去重/形检）→ `admin_auth` 指派与 Validate（含 G5 启动门）→ `internal/admin/scope.go`（能力表 + 请求遍历）→ census 双测试 → Node 签名替换 + `AdminSessionNamespace` → GetChannels 过滤 → G7 指标 → 删 `AdminAuthInterceptor`。
  验证：`go build ./... && go test ./...`；census 双测试；矩阵集成测试；手工 grpcurl 双 header 冒烟。
- **Step 2（栈接线 + 文档）**：dokploy 栈 mlbridge 指派、文档（`docs/developer/02-configuration.md` / `03-admin-api.md` / `01-architecture.md`、dokploy README、config-example.yaml）。
  注意：`docs/developer/` 存在并行改版，合入前先对齐。

### Torchwood（T1"多 Key per project"已具备；硬前置收敛为五项）

| 项 | 内容 | 必需性 |
|---|---|---|
| T2 | 自定义 scope 机制（§2.6 语法校验、内建保护、UI） | 必需 |
| T3 | platform 级 Key（无 project 绑定，`["*"]` 唯一来源） | 必需 |
| T4 | **whoami 端点**（实现裁决，替代原 verify+scope 方案）：`GET /v1/server/api-keys/whoami`，调用方 X-API-Key 即被查询的 key 本身 → 200 `{keyId, name, projectId（空=平台级）, scopes[], maxAgeSeconds}`；无效/禁用/过期 → 401。零新 scope——TW 的 apikeys scope 已退役（防自铸提权），"知道 key 明文"即查询授权，零信息泄露；复用现有每请求读库校验（撤销立即生效，优于原方案） | 必需 |
| T5 | project id 创建时 namespace 语法门（`[a-z0-9-]{1,32}` 首尾非 `-`） | 必需（否则签出的 namespace 在 messageloop 清洗为空 → 零范围 Key，源头该源头拦） |
| T6 | per-Key 撤销收紧：whoami 响应的 `maxAgeSeconds` 由 TW 服务端按 key 既有 `expire_at` 计算（`max(0, expire_at-now)`，规避时钟偏斜——G8）；未设过期 = 0（用 mlbridge/messageloop 默认 TTL）。零 schema 变更 | 必需 |
| T7/T8 | 审计进 analytics、last_used_at 节流聚合、控制台 Key 管理 UI（CLI 先行） | 运营 |

### mlbridge

| 项 | 内容 |
|---|---|
| M1 | 实现 `ProxyService.AuthenticateAdmin`（TW verify + 前缀过滤十行 + max_age 策略） |
| M2 | ~~供给 Key 增加 `apikeys.verify` scope~~（随 T4 whoami 裁决取消——mlbridge 供给 Key 不参与 admin key 校验） |
| M3 | mlbridge 自身 admin 调用继续走静态 token（D25，守住依赖环） |
| M4 | `mlkey-smoke` 子命令：拿一把 Key 跑 dogfooding 打靶清单，输出 Key 完整标签面供审计 |

### 联合发版顺序（dogfooding，一个栈版本内）

1. TW 先行（T2-T6，纯增量，老消费方无感）；
2. messageloop + mlbridge 同版（proto 契约两端一起；mlbridge 配置加 `admin_auth: true`）；
3. 签发首批 Key 打靶：platform Key 一把（运维工具）、每 project 一把（租户后端）。

### Dogfooding 打靶清单（每项对应一个机制承诺）

1. 撤销传播上界：TW 吊销 → 实测 ≤ TTL+ε 内全拒；
2. proxy 宕机：停 mlbridge → 缓存过期后 Key 型调用全拒、静态 token 照常；
3. 跨 namespace 打靶：acme 的 Key 打满矩阵（对 beta 的 channel/ns 参数/session）→ 逐格符合，重点"session 不可见 = false 而非报错"；
4. 能力钳制：全 scope Key + 节点子集上限 → 生效 = 交集；
5. census 红线：故意加假 RPC 不分类 → 测试红（机制自证演示）；
6. Key 喷射：一万随机 Key → proxy 只收到四层限幅后的量；
7. 语法门：静态 token 用无 namespace channel → InvalidArgument；
8. 多 Key 实例隔离：同 project 零标签 Key → 能认证、全拒、不影响另一把；
9. 熔断：停 mlbridge → 5 连错后调用秒拒（不挂 30s）；恢复后熔断自动退出；
10. token 轮换：auth_tokens 加新删旧滚动重启，mlbridge 全程不断连。

---

## 五、风险与对策

| 风险 | 对策 |
|---|---|
| admin API 新增 proxy 可用性依赖 | 影响面有界：仅 Key 型调用方、仅缓存过期边界；静态 token 通道永不经过 proxy；D27 熔断保证 proxy 宕机时快速失败而非挂起；proxy 本就是客户端连接 tier-1 依赖，未引入新依赖等级 |
| 静态 token 泄露/轮换 | 泄露爆炸半径 = 超管（既知代价，靠 dokploy env/file 秘密管理）；轮换由 auth_tokens 列表消除空窗（D28） |
| 执行点遗漏（8 RPC × 载体） | 结构性防漏：scope 层 + census 双测试（G1/G2），不靠 reviewer 眼力 |
| 钳制/清洗逻辑越权 | 逐条单测 + 集成断言"proxy 返回超上限 caps → 生效 = 上限" |
| Key 明文泄露 | 拦截器内存瞬时；缓存键 sha256、值不含明文；日志/指标只 key_id；对 proxy 传输按 proxy 配置走 TLS |
| 坏 Key 喷射 DoS | G9 四层限幅 |
| 撤销延迟争议 | 文档承诺上界 = TTL；敏感 Key 由 TW per-Key max_age 收紧至秒级 |
| 版本偏斜（旧 mlbridge / TW 新能力位） | `admin_auth` 指派才启用；未知闭集名 WARN 丢弃（安全方向偏斜） |
| 运维给同一 Key 混配跨系统标签（实例混用） | 机制不禁止（TW 无法判断用途），靠"一用途一 Key"守则 + 控制台提示 + mlkey-smoke 打印完整标签面；多 Key 机制把代价降到"多发一把 Key" |
| 存量 project id 违反 namespace 语法 | T5 拦新；存量违规 Key 会被清洗为零范围 + WARN，dogfooding 期一次性脚本排查 |
| 静态 token 行为回归 | 现有测试通过或有意更新；静态 token 语义冻结为超管，不扩展限权（要限权就发 Key） |

**回滚**：TW 侧纯增量无迁移；栈内摘掉 `admin_auth: true` 即整体下线 Key 认证；messageloop 各步独立 commit revert，无状态迁移、集群协议零改动。

---

## 六、测试策略

1. **proxy 契约**：gRPC + HTTP 双实现的 AuthenticateAdmin 往返（含 protojson 字段名两形态）。
2. **resolver 单测**：命中/过期/max_age 钳制（含已过期与 1s 下限）/负缓存 TTL/并发同 Key 去重（计数断言代理调用 =1）/caps 钳制/namespace 清洗四例/代理错误 fail-closed/**错误 2s 负缓存与 5 连错熔断的进入、快速失败、恢复退出**/容量驱逐/20 字符门（仅 proxy 路径）/**auth_tokens 多值命中与短 token Validate 拒绝**。
3. **census 双测试**：方法分类表完备性；请求字段"已处理或显式豁免"完备性。
4. **矩阵集成**（stub resolver 注入身份）：8 RPC × 载体 × 身份{scoped、`["*"]`、static-token} 逐格断言 §2.4 矩阵，含"跨 ns session 不可见 = false"与"整请求只剩越界目标 → scope 短路合成 not-found 响应（而非 InvalidArgument）"两条泄露负向断言。
5. **撤销传播**：1s TTL 下 verify → stub 翻转拒绝 → 过期后断言拒绝。
6. **Authorizer 交互**：deny_all 对 proxy 身份照绑；allow 列表 `key:<id>` 放行。
7. **回归**：现有 admin/grpc 测试原样通过或有意更新；allow_insecure、静态 token 端到端各一条。
8. **安全负向**：日志与指标断言不含 Key 明文。

---

## 七、明确不做的事

- messageloop 侧 Key 存储/CRUD/轮换/过期管理（Key 生命周期全部归 Torchwood）；
- 静态 `api_keys` 配置、`cmd/mlkey`、Key 哈希存储（v1 遗产，随 D2′ 移除）；
- 配置热加载 / watch（无路由变更热需求）；
- 客户端面（WebSocket/gRPC/QUIC/KCP）鉴权变化——auth-proxy 模式已覆盖；
- admin HTTP（/health、/metrics）鉴权变化；
- per-Key 限流/计量（TW analytics 已承担计量）；
- proxy 不可用时的 fail-open 缓存续期；
- `messageloop.publish` / `messageloop.all` 标签（D22/D23）；
- mlbridge 自身 Key 化（D25）；
- server api.proto / 四端 SDK / 集群协议改动（D13）。

---

## 附：改动面清单（三仓）

| 仓库 | 文件/模块 | 动作 |
|---|---|---|
| messageloop | `protocol/proxy/v2/proxy.proto`（+生成物） | `AuthenticateAdmin` + 三消息 |
| | `proxy/proxy.go`、`grpc.go`、`http.go` | 接口 + 双实现；`ProxyConfig` +`AdminAuth bool` |
| | `internal/authz/identity.go`（新） | `AdminIdentity` |
| | `internal/admin/auth.go`（新） | resolver + 拦截器 + 钳制/缓存/去重/形检 |
| | `internal/admin/scope.go`（新） | 能力表 + 请求遍历 scope 层 |
| | `internal/admin/api_handler.go` | 删散点检查，接改写后请求 |
| | `internal/admin/census_test.go`、`api_key_scope_test.go`（新） | 双 census + 矩阵 |
| | `internal/runtime/cluster_commands.go`、`node.go`、`aliases_local.go` | 签名替换 + `AdminSessionNamespace` |
| | `config/config.go`、`cmd/server/envconfig.go` | `admin_auth_cache_ttl`、`auth_tokens` 列表（替代单值，no-compat 直接替换）、指派校验、G5 启动门、token 最小长度 |
| | `internal/metrics/` | G7 两指标 |
| | `pkg/transport/grpc/server.go` | 删 `AdminAuthInterceptor` |
| | `docs/developer/*`、`docker/dokploy/README.md`、`config-example.yaml` | 文档 |
| mlbridge | AuthenticateAdmin 实现、`apikeys.verify`、mlkey-smoke | M1-M4 |
| Torchwood | 自定义 scope、platform Key、verify 端点、project id 语法门、max_age | T2-T6（必需）/T7-T8（运营） |
