# PR-KA-D1 实现规格：转正收口——文档对齐 + 死代码清理

| 字段 | 值 |
| --- | --- |
| 标题 | `docs/code: align public docs with v2 behavior and drop dead sendQueue.enqueue` |
| 状态 | **Ready**（待实现） |
| 依赖 | C6 已合（`08e8a4c`）；转正评审（2026-08-17 四路）结论。在 `v2` 分支上做 |
| 设计来源 | 转正评审报告（SHOULD-FIX 清单）；[kernel-architecture.md](../kernel-architecture.md) |
| 验收人 | 主 agent |

## 1. 目标

转正评审认定代码/规格/测试三轴全过，但公共文档与 v2 行为有漂移。本 PR 做一次性收口，共 6 项（每项的现状行号见 §3）：

1. **`docs/protocol.md` 对齐 v2 信封**：OutboundMessage 表补 `gap_notice` 行（C6 已上线，文档漏收）；Heartbeat 节去掉 "in v1.0" 漂移措辞；文首加版本定位（客户端面 = 独立 v2 协议，管理面保留 `server.v1` 是 PR-KA-B3 明示接受的决策）。
2. **`README.md` 清 ACL 残留**：特性行与 proxy 表行的 "ACL" 改为 Authorizer 表述；「Limits And Built-In ACL」整节换成 `server.authorizer` 示例与语义；「Enable The Distributed Control Plane」的集群示例 YAML 补 HMAC 密钥（现状示例照抄会启动失败）。
3. **`docs/developer/02-configuration.md` 补 HMAC 密钥文档**：cluster 节字段表 + YAML 块补 `cluster.hmac_key` / `cluster.hmac_key_file`；校验清单补 HMAC 启动硬门；顺手修 :46 的断链（`#serverauthorizer-节` 锚点不存在，授权表实际在 `## server 节` 内）。
4. **新增 `configs/cluster-example.yaml`**：最小可跑的集群配置示例，含 `hmac_key_file` 占位与密钥生成/要求注释（≥32 字节、文件尾单换行会被裁剪、两源互斥）。
5. **`docs/v2/kernel-architecture.md` 残留清扫**：「三把时钟」表三个 `ml:` 键形改 `ml2:`（C5 落地时规格限定只改舰队节，此处补收）；Document History 补一行 A0–C6 落地记录。
6. **删死代码 `sendQueue.enqueue`**（`session.go:208-231`）：零调用者（唯一入队路径是 `Session.enqueue` → `tryEnqueue`），文档注释与实现矛盾，且 `for q.closed` 分支在 `defer Unlock` 之上手动 `Unlock`，一旦被调用即 double-unlock panic。`notFull` cond 保留（`tryEnqueue`/`dequeue`/`close` 仍在用）。

**不做：** 任何行为/语义改动；proto、SDK、测试（删死代码不需要改测试——`grep` 确认零调用）；改 `docs/design`、`docs/review`、`docs/archive`、`docs/v2/tasks/` 下既有规格（历史文档保持原样）；Q9（仓库形态）;CI 与 release 工程；顺手重构（`PayloadProto` 重复、`compilePattern` 重复等评审 NOTE 项一律不碰）。

## 2. 允许改动的文件

- `session.go`：仅删除第 208–231 行（doc 注释 + `enqueue` 方法）
- `docs/protocol.md`
- `README.md`
- `docs/developer/02-configuration.md`
- `configs/cluster-example.yaml`（新增）
- `docs/v2/kernel-architecture.md`：仅「三把时钟」表三个单元格 + Document History 追加一行
- `docs/v2/tasks/pr-ka-d1-graduation-docs.md`（§7 实现备注）

禁止：改任何 `.go` 生产逻辑（除 §1.6 的删除）、测试文件、proto、SDK、`docs/v2/README.md` 与增量表（主 agent 负责）；git commit / tag / push。

## 3. 现状（动手前再读）

### 3.1 `docs/protocol.md`

- OutboundMessage 表 :77-92：无 `gap_notice` 行。proto 侧 `OutboundMessage.gap_notice = 19`（`GapNotice{ channel, position, gap_reason }`,`gap_reason` ∈ `GAP_REASON_MIDDLE` / `GAP_REASON_REPLAY_TRUNCATED`,`position` = 最后已知安全位置，offset 未知则缺省；每频道每次 catch-up 至多一条）。新行追加在表末（`survey_result` 之后，字段号最大）。
- :444 `Heartbeat is **bidirectional** in v1.0:` —— "in v1.0" 是旧代措辞，去掉版本限定。
- :545 管理面节写 `messageloop.server.v1.APIService` —— 事实正确（admin 面仍是 v1），但要标注这是**明示保留**而非疏漏。
- 文首 :1-3 无版本定位。加一行说明：本文描述当前独立版本（KD-K31）的客户端协议，信封定义在 `protocol/client/v2`；管理面 gRPC 仍为 `server.v1`（PR-KA-B3 明示接受）。

### 3.2 `README.md`

- :16 `Built-in ACL rules plus proxy-backed auth and ACL checks` —— `server.acl` 已被 A4 删除且 `Validate()` 拒绝该键。
- :29 proxy 行 `RPC/auth/ACL/lifecycle delegation to HTTP or gRPC backends` —— 同样去 ACL 措辞。
- :142-165「Limits And Built-In ACL」节：`server.acl.rules` 示例 + 4 条行为注释全部失效。换成 `server.authorizer` 示例。准确语义（来自 `config/config.go:125-150` 与 `docs/developer/02-configuration.md:141-153`):
  - `authorizer.default` = 兜底 ChannelPolicySpec；`authorizer.rules[]` 字段：`pattern` / `deny_all` / `allow_subscribe` / `allow_publish` / `allow_survey` + inline 策略字段；
  - allow 名单：未设置 = 不约束；显式空列表 = 拒绝；`"*"` = 任何已认证用户；
  - 无规则命中 → 默认策略（订阅/发布放行，survey 关闭）；
  - 规则按配置顺序求值、后者覆盖前者（不是 first-match）;deny 不可被 allow 打洞。
- :101-128「Enable The Distributed Control Plane」示例：`cluster:` 块缺 HMAC 密钥，照抄启动必失败（`ResolveHMACKey` 硬门）。补 `hmac_key_file`（或 `hmac_key`）占位 + 一行说明；Operational requirements 列表加一条「所有节点共享同一命令总线 HMAC 密钥（≥32 字节，`cluster.hmac_key` 或 `cluster.hmac_key_file` 二选一）」。

### 3.3 `docs/developer/02-configuration.md`

- cluster 节 :350-367:YAML 块与字段表只有 `enabled` / `node_id` / `backend` 三行，缺 HMAC 两键。补：
  - `cluster.hmac_key` | string | 未设置 | 内联 HMAC-SHA256 密钥，**至少 32 字节**；与 `hmac_key_file` 二选一
  - `cluster.hmac_key_file` | string | 未设置 | 密钥文件路径；文件尾**单个**换行（LF 或 CRLF）会被裁剪
  -  enforcement 位置：`ClusterConfig.ResolveHMACKey()`（`config/config.go:64-85`)，启动接线时调用（`cmd/server/main.go:193-196`）——不在 `Validate()` 里，文档措辞要准确（「启动时解析失败即拒绝启动」而非「Validate 拒绝」）。三个报错原文：`only one of hmac_key or hmac_key_file may be set` / `cluster.hmac_key is required when cluster is enabled (or set cluster.hmac_key_file)` / `cluster hmac key must be at least 32 bytes`。
- :46 校验清单第 7 条末尾的 `[server.authorizer 节](#serverauthorizer-节)` 是断链（全文无此标题；授权表在 `## server 节` :141-153）。把锚点改为 `#server-节`（链接文字可保留）。
- 校验清单（:45 附近第 6 条 cluster 前置条件）补一句：启用集群还要求恰好一个 HMAC 密钥源且 ≥32 字节，启动解析失败即拒绝启动（不在 `Validate()`，见 cluster 节）。

### 3.4 `configs/cluster-example.yaml`（新增）

仓库现仅有 `configs/test.yaml`(E2E 用，无 cluster 块）。新文件最小集：`server.http.addr` / `server.grpc_admin.addr` / `transport.websocket.addr` / `broker.type: redis` + `broker.redis.addr` / `cluster.enabled: true`、`node_id`、`backend: redis`、`hmac_key_file: /path/to/cluster-hmac.key` 占位。注释写清：密钥 ≥32 字节；文件尾单换行会被裁剪；`hmac_key` 与 `hmac_key_file` 二选一；集群内所有节点共用同一密钥。不要写真实密钥进仓库。

### 3.5 `docs/v2/kernel-architecture.md`

- 「三把时钟」表 :123-125 三个单元格：`ml:broker:epoch` → `ml2:broker:epoch`;`INCR ml:occ:gen:{ch}` → `INCR ml2:occ:gen:{ch}`;`INCR ml:cluster:node_epoch:{node_id}` → `INCR ml2:cluster:node_epoch:{node_id}`。只改这三个单元格，表结构与其余文字不动。
- Document History(:751 起）追加一行：`2026-08-17 | A0–C6 全部落地（规格与实现见 tasks/）；三把时钟键形同步 ml2:（C5）`。
- 增量表 D1 行由主 agent 加，实现方不动。

### 3.6 `session.go` 死代码

- :208-231:doc 注释三行 + `func (q *sendQueue) enqueue(frame *queuedFrame) error`。零调用者（`grep -rn "\.enqueue(" --include='*.go'` 仅 `client.go:144`，走的是 `Session.enqueue` → `tryEnqueue`,:235/:758)。
- 删除后 `newSendQueue`(:202-206)、`notFull`(:196,:204,:252,:262,:325)、`sendQueueControlDepth`/`sendQueueDataDepth` 常量全部保留——`tryEnqueue`/`dequeue`/`close` 仍在用。

## 4. 验证

```bash
go build ./...
go test -count=1 .                                    # 根包覆盖 session.go
grep -n "gap_notice" docs/protocol.md                 # 有命中
grep -n "Built-In ACL\|Built-in ACL" README.md        # 零命中
grep -n "hmac_key" docs/developer/02-configuration.md configs/cluster-example.yaml README.md  # 均有命中
grep -n "ml:broker:epoch\|ml:occ:gen\|ml:cluster:node_epoch" docs/v2/kernel-architecture.md   # 零命中
grep -n "func (q \*sendQueue) enqueue" session.go     # 零命中
grep -rn "\.enqueue(" --include="*.go" . | grep -v genproto  # 仅剩 client.go:144(Session.enqueue)
```

不需要跑全仓 `go test ./...`（无行为改动）；若跑了也行，但必须串行、不得与其他根目录测试并发。

## 5. 验收清单

1. `session.go` 仅删 208–231，其余零 diff;`go build ./...` 与 `go test -count=1 .` 绿。
2. `docs/protocol.md`:OutboundMessage 表含 `gap_notice` 行（内容符合 §3.1 语义）;Heartbeat 节无 "v1.0" 措辞；文首与 Admin 节有版本定位/明示保留说明。
3. `README.md`：零 "Built-In ACL"/"Built-in ACL";ACL 节已换成语义准确的 `server.authorizer` 示例（§3.2 五条语义不得写错）；集群示例含 HMAC 密钥占位与共享密钥要求。
4. `02-configuration.md`:cluster 节字段表/YAML 含 `hmac_key`、`hmac_key_file`，语义与三个报错原文准确；:46 断链修复；校验清单补 HMAC 硬门（措辞 = 启动硬门，不是 Validate)。
5. `configs/cluster-example.yaml` 存在、可解析（YAML 合法）、无真实密钥。
6. `kernel-architecture.md`：三把时钟表三个单元格 `ml2:`，其余单元格零 diff;Document History 追加一行。
7. 未改 §2 禁止项；无 git commit/push；无格式churn(`git diff --numstat` 与 `git diff --ignore-all-space --ignore-cr-at-eol --numstat` 行数一致）。

## 6. 完成报告

- 改动文件列表（含新增）
- §5 每条 过/失败 + 证据（grep 输出 / 测试输出）
- 测试命令与结果（真实输出）
- 偏离（应无）

## 7. 实现备注（实现方填）

（空）
