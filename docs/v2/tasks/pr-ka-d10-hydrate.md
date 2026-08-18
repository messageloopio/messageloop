# PR-KA-D10 实现规格：Hydrate 去 saga + 集群写路径原子性收口

| 字段 | 值 |
| --- | --- |
| 标题 | `cluster: soft-fail hydrate without saga, atomic lease+snapshot write, epoch-wired takeover skip` |
| 状态 | **Ready**（待实现） |
| 依赖 | D9 已合（`8eab8a8`)。在 `v2` 分支上做 |
| 设计来源 | 转正评审 backlog D10；`docs/v2/kernel-architecture.md` :348（禁止逐频道 saga）、:292（单频道软失败哲学）、:610 宪法 5（没有盲写）、KD-K27/C2 延期授权（`pr-ka-c2-epoch.md:76`) |
| 验收人 | 主 agent |

## 1. 目标

收口集群恢复的三处评审点名项。

### 1.1 Hydrate 去 saga（逐频道软失败）

现状（`cluster_resume.go:171-211` + `client.go:523-535`)：远端 resume 的订阅恢复逐频道执行，任一频道失败 → saga 回滚已成功频道 → 删 hub 注册 + 删 lease/snapshot → 客户端吃 `DisconnectStale`(3502)，会话**永久不可恢复**。回滚自身还吞错，可留 broker 订阅泄漏（无自愈）。

改为**逐频道软失败**（kernel-architecture.md:292 同构）:

- `restoreSessionSubscriptions` 不再整体返回 error：某频道 restore/presence 失败 → 该频道不恢复、记入失败列表、继续其余频道；**不做回滚**（成功频道保留）。
- `client.go:523-535` 的删库式硬回滚删除：恢复段不再触发 `hub.RemoveSession` + `deleteClusterSessionState` + 3502。会话以部分订阅存活。
- 有失败频道时，在 `Connected` 发出**之后**向客户端逐频道发顶层 Error 信封：`code=RECOVER_FAILED`、`type=recover_error`、`metadata.entries["channel"]=<ch>`(D7 码表内码，**无 proto 变更**)。客户端按既有顶层错误路径获知哪些频道没恢复，自行重订。
- **显式决策**：hydrate **不**重新过 Authorizer/ACL——恢复是已授权会话的延续（现状即不过，本 PR 把空白写成决策，写进 `docs/developer/04-cluster.md`)。

### 1.2 lease CAS + snapshot 原子写（消灭盲写窗口）

现状（`cluster_state.go:255-313`):`syncClusterSessionState` 两处「`CompareAndSwapSessionLease` 成功 → 裸 `PutSessionSnapshot`」(:277、:312)。两步不原子：A 的 in-flight 刷新可使其旧 snapshot 盲落在 B 抢租之后（窗口一）；抢租成功到首个新快照落盘之间快照还是上一代（窗口二）。评审宪法 5「没有盲写」当时只收到 lease(A1),snapshot 是漏网同款。

修法（探查报告方案 1，最小接口变更）:

- 新**可选**接口（名称建议 `SessionStateCompareAndSwapper`，置于 `cluster_state.go` 或 directory 接口旁）:
  `CompareAndSwapSessionState(ctx, expected, desired *ClusterSessionLease, snapshot *ClusterSessionSnapshot, leaseTTL, snapshotTTL time.Duration) (bool, error)`。
  语义 = 现有 CAS 四字段谓词（expected 为 nil 时要求键不存在）**+ 成功时同一原子步写 snapshot**;`ok=false` 时什么都不写。
- 接线用 type-assert（先例：`NodeEpochAllocator`,`cluster_epoch.go:98`):directory 实现该接口 → 走原子写；未实现 → 回退旧两步并在注释中标注非原子。
- Redis 实现（`pkg/redisbroker/cluster_directory.go`)：一条 Lua——读 lease 键、与 expected 序列化比较（nil ↔ 键不存在）、相等则同脚本 `SET lease + SET snapshot`（各自 TTL:600s lease / 24h snapshot 不动）。序列化稳定性在脚本注释里钉死（两键值格式不变，读者零改动）。
- `noopSessionDirectory`(`cluster_state.go:123-133`）与 C1 sim Directory(`internal/cluster/sim/directory.go`，单锁）直接实现该接口；测试 fakes 逐个核对（见 §3.4)。

**非目标（写明，不做）**：窗口二（`resumeRemoteSession` 先读快照后抢租，`cluster_resume.go:47` 的顺序固有）不消除——其危害有界（接管方拿到旧快照 → 恢复起点偏低 → 可重复投递，幂等语义外），留作已记录残余；snapshot 值不加 fencing 戳；两键两 TTL 不变；CAS 四字段谓词形状不变（C1 门禁）。

### 1.3 epoch 接线：同节点旧世代跳过 takeover RPC

C2 延期授权（`pr-ka-c2-epoch.md:76`）的兑现。`resumeRemoteSession` 的 takeover 分支（`cluster_resume.go:86` 附近）：当 `lease.NodeID == n.ClusterNodeID()` 且 `NodeEpochNewer(n.ClusterIncarnationID(), lease.IncarnationID)` 为真——本进程是同 nodeID 的更新世代（INCR 单调保证旧世代已死，必走 KD-K30 旁路）——**跳过** `requestSessionTakeover`，直接用已 CAS 到手的租约进恢复。语义不变（takeover 注定失败后被旁路），省一次 doomed RTT;`ParseNodeEpoch`/`NodeEpochNewer` 从此有生产调用点。`ParseNodeEpoch` 返回 false 的非 epoch ID(`inc-a` 等测试注入）不跳过，行为与现状一致。

**不做：** 命令接收端按世代拒收、Membership 世代归并（评审候选 2/3，不在本 PR);proto 变更；`hub.go`/`session.go`;Fence 的补偿路径（`session.go:338-425`,session 级、另一件事）;ACL 在 hydrate 的重新求值（§1.1 已定为不求值）;git commit / tag / push。

## 2. 允许改动的文件

- `cluster_resume.go`、`cluster_state.go`、`client.go`（仅 finishConnect 恢复段与失败信封）、`cluster_epoch.go`（仅注释/接线说明，函数不动）
- `pkg/redisbroker/cluster_directory.go`(Lua 原子写）
- `internal/cluster/sim/directory.go`（实现新接口）
- 测试：`cluster_resume_test.go`、`cluster_state_test.go`、`cluster_remote_test.go`、`client_fix_test.go`、`cluster_test.go`、`cluster_redis_integration_test.go`、`internal/cluster/sim/directory_test.go`（按涉及面增减用例）
- `docs/developer/04-cluster.md`（恢复语义 + 原子写 + epoch 接线描述）、`docs/v2/tasks/pr-ka-d10-hydrate.md`(§8)
- 新增生产文件如需（如接口独立成文件）须在报告说明

禁止：proto/生成物；`hub.go`/`session.go`;C1 sim 六场景语义（`cluster_sim_test.go` 的断言形状）;CAS 四字段谓词形状；SDK 手写代码。

## 3. 现状（动手前再核对）

### 3.1 resume 调用链

`client.go:390` handleConnect → 远端分支 `client.go:461` → `resumeRemoteSession`(`cluster_resume.go:33-136`:GetSessionLease :39 → GetSessionSnapshot :47（先读后抢）→ CAS 抢租 :75 → 他节点 fencing 时 `requestSessionTakeover` :88，失败且旧 node lease 仍在 → CAS 还权 :143-147，旧 node lease 已失 → KD-K30 旁路 :108)→ `finishConnect`(`client.go:493`:AddClient :513 → 内含 `syncClusterSessionState`;restore :524)。

### 3.2 saga 三写面与兜底

逐频道：`restoreLocalSubscription`（首订触发 broker.Subscribe)、`SetPresenceForSession`(shouldTrackPresence 门，:183)、投影 `adjustClusterChannelSubscriptionsTimeout(+1)`(:190)。失败回滚 `rollbackRestoredSubscriptions`（吞错）。再上层兜底：`client.go:523-535` 删 hub + 删 lease/snapshot + 3502。**snapshot 只被 `resumeRemoteSession` 读**（全仓唯一生产读点）。

### 3.3 snapshot 写路径

生产写点仅 `cluster_state.go:277/:312`。Redis:lease CAS = WATCH+GET+四字段比较+TxPipelined SET(`cluster_directory.go:106-149`);snapshot PUT = 裸 SET(:179-211)。`syncClusterSessionState` 调用方：`AddClient`(node.go:382)、订阅变更（node.go:464/469/563/568)、ping/pong 节流（client.go:1289)、本机 resume 后（client.go:519)、sim 封装。

### 3.4 需逐个核对的 Directory 实现/fake

`noopSessionDirectory`(cluster_state.go:123-133)、sim Directory、`cluster_remote_test.go:112` fake、`client_fix_test.go:974` counting、`cluster_test.go:48` tracking。接口新增方法后这些点都要么实现、要么显式走 fallback——**逐个点名检查，不许靠编译错漏网**（接口不是编译期强制时是运行期分派）。

### 3.5 epoch 现状

`ParseNodeEpoch`/`NodeEpochNewer` 生产零调用（仅测试）。生产在用的是 `NodeEpochAllocator`/`FormatNodeEpoch`/`allocateNodeIncarnation`(cluster_epoch.go:92-113)。takeover 分支在 `cluster_resume.go:86` 一带。

### 3.6 测试覆盖现状（缺口即本 PR 新增测试的位置）

- `cluster_resume_test.go`：回滚/投影/旁路用例全 fake directory，无并发交错、无回滚失败注入。**软失败改造后这些用例的断言语义要跟着改**（不再整体失败、不再删库）。
- C1 sim 六场景只到 `AttachResumed`，从不驱动 restore 段；本 PR 不动 sim 场景语义。
- Redis 集成 `TestClusterRedis_RemoteResumeTakeover` 只覆盖 happy path；本 PR 在其上加「CAS 与 snapshot 同生同灭」断言（抢租后立刻读 snapshot 必为新 owner 视图），并视情补 restore 中途失败的真 Redis 用例（现零覆盖）。

## 4. 测试

| 测试 | 内容 |
| --- | --- |
| 软失败单测（新） | 注入某频道 restore/presence 失败：其余频道保留、会话存活、客户端收到该频道 `RECOVER_FAILED` 信封、无 lease/snapshot 删除 |
| 全失败边界（新） | 全部频道失败：会话仍存活（空订阅）、不 3502 |
| 原子写（新） | Redis：same-fence 刷新与首次登记两路经 Lua 同生同灭；fallback 路径（fake directory 不实现接口）行为同旧 |
| epoch 跳过（新） | 同 nodeID 旧世代 lease 的 resume 不发 takeover 命令（fake bus 断言），非 epoch ID 不跳过 |
| 回归 | C1 sim 六场景原样绿；`cluster_*_test.go` 全绿；全仓 `./...`、SDK、TS jest、chatroom build |

## 5. 验证

```bash
go build ./...
go test -count=1 -run "TestSim_|TestCluster|TestResume|TestClientFix|TestSession" .
go test -count=1 ./pkg/redisbroker ./internal/cluster/...
go test -count=1 ./...            # 串行；真实 Redis
cd sdks/go && go test -count=1 .
cd sdks/ts && npx jest
cd _examples/chatroom && go build ./...
golangci-lint run ./...           # 必须保持 0 issues(D8 已清零)
```

## 6. 验收清单

1. saga 删除：`restoreSessionSubscriptions` 逐频道软失败、无回滚；`client.go:523-535` 硬回滚移除；失败频道以 `RECOVER_FAILED` 信封告知；hydrate 不过 ACL 的决策写进 04-cluster.md。
2. 原子写：新接口 type-assert 接线；Redis 一条 Lua 完成 lease CAS + snapshot SET；两键 TTL 不变；fallback 注释标注非原子；§3.4 每个 Directory 实现点名核对过。
3. epoch：同节点旧世代跳过 takeover RPC;`NodeEpochNewer` 有生产调用点；非 epoch ID 行为不变。
4. C1 sim 六场景与 CAS 四字段谓词形状原样；§5 全链绿（含 lint 0 issues)。
5. 未碰 §2 禁止项；无格式 churn;无 git 操作。

## 7. 完成报告

- 改动文件列表（新增/删除/修改分组）
- §6 每条 过/失败 + 证据
- §3.4 Directory 实现逐点核对表
- 测试命令与结果（真实输出）
- 偏离（应无）

## 8. 实现备注（实现方填）

（留空）
