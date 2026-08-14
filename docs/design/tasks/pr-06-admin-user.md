# PR-06 实现规格：Admin 按 user 投递 / 断开 / 订阅

| 字段 | 值 |
| --- | --- |
| 标题 | `server: publish, disconnect, and subscribe by user_id` |
| 状态 | **Accepted**（2026-08-14 主 agent 终验通过，尚未 commit） |
| 依赖 | **PR-01 已合**（`Destination.users`、`DisconnectRequest.users`、`SubscribeRequest.user_id`、`UnsubscribeRequest.user_id`） |
| 设计来源 | [v1.0-platform-gaps.md](../v1.0-platform-gaps.md) 缺口 3、KD-13 |
| 验收人 | 主 agent |

## 1. 目标

Admin 能按 `user_id` 对用户的**全部 session** 做 Publish / Disconnect / Subscribe / Unsubscribe。展开后复用现有 `PublishToSession` / `DisconnectSession` / `SubscribeSession` / `UnsubscribeSession`，不新增 RPC、不新增 cluster command 类型。

单节点只扫本地 `Hub.SessionsByUser`。集群再并上 Redis user 索引，**展开时必须用 `GetSessionLease`（或本地 `Client.UserID`）校验 UserID**，索引不是权威。

本 PR **不**实现：客户端协议 `PublishToUser`、SDK、Survey、改 proto。

## 2. 允许改动的文件

- `hub.go`：`SessionsByUser(userID) []*Client`（空 userID → 空切片）
- `cluster.go`：`SessionDirectory` 增加 `AddUserSession` / `RemoveUserSession` / `ListUserSessions`
- `cluster_state.go`：`noopSessionDirectory` 空实现；`Put`/`CAS`/`Delete` lease **之后**调 `syncUserIndex`（或 directory 内部做，但 CAS/Delete 不能漏）
- `cluster_resume.go`：CAS 成功路径也要更新索引（若 helper 挂在 directory 方法上则自动覆盖）
- `cluster_user_index.go`（新，根包）：`syncUserIndex`、`expandUserSessions`
- `cluster_user_index_repair.go`（新）：`SCAN` session lease 前缀重建成员键；**不要**挂进频道投影修复循环
- `pkg/redisbroker/cluster_directory.go`：成员键 + Set；TTL 与 session lease 相同
- `pkg/grpcstream/api_handler.go`：读 `users` / `user_id`；空 user → `InvalidArgument`；Publish 的 destination 允许**只有 users**
- `metrics.go` / `metrics_test.go`：`admin_user_fanout` HistogramVec，标签 `op`=`publish`|`disconnect`|`subscribe`|`unsubscribe`
- 所有实现了 `SessionDirectory` 的 fake：补三个方法（`cluster_remote_test.go` `fakeSessionDirectory`、`cluster_test.go` `trackingClusterComponent`、`client_fix_test.go` `countingSessionDirectory`）
- 测试：`hub_test.go` 或 `cluster_user_index_test.go`（新）、`pkg/grpcstream/api_handler_test.go`；必要时 Redis 集成测（无 Redis 则 Skip）
- `docs/developer/03-admin-api.md`：Publish/Disconnect/Subscribe/Unsubscribe 补 users
- `docs/developer/04-cluster.md`：user 索引键与 repair
- `docs/design/tasks/pr-06-admin-user.md`（完成备注）

禁止：改 proto、改 SDK、改 `client.go` 的 handleConnect/Subscribe/Publish 业务、改 `channel_policy.go`、git 写操作。

## 3. 现状（动手前再读）

- Proto 字段已在：`Destination.users=3`、`DisconnectRequest.users=4`、`Subscribe/Unsubscribe.user_id=3`。
- `api_handler.Publish` **今天**把「sessions 与 channels 都空」当失败（约 45 行）。只填 `users` 会被当成无 destination。本 PR 必须改。
- Disconnect/Subscribe/Unsubscribe **完全忽略** `users` / `user_id`。
- `connShard.users` 只服务于 `maxConnsPerUser`，没有 `SessionsByUser`。空 userID 的匿名连接会进 `users[""]`。
- `SessionDirectory` 无 List。lease JSON 有 `UserID`。
- 跨节点 session 操作已通。

## 4. API 语义

空 `user_id` / `users` 里的空字符串 → gRPC `InvalidArgument`，**不扫描**。

| RPC | 展开 | 之后 |
| --- | --- | --- |
| Publish `destination.users` | 每个 user → session 列表 | 与 `destination.sessions` **并集**（去重）后走现有 `PublishToSession`。channel 可空 |
| Disconnect `users` | 同上 | 与 `sessions` 并集后走 `DisconnectSession`。`results` 按 session |
| Subscribe / Unsubscribe `user_id` | 一个 user → sessions | 与 `session_id` 并集。两者都空 → `InvalidArgument`。对每个 session×channel 调现有方法。`results` 键保持今日习惯（按 channel）；多 session 时 **任一 session 成功则该 channel 为 true**（写进完成备注） |

Publish 失败语义不变：best-effort，全失败才 `Internal`。session 不存在仍跳过、不记失败（与今日 sessions 相同）。

展开：

```
func (n *Node) expandUserSessions(ctx, userID string) []string {
    if userID == "" { return nil }
    seen := map[string]struct{}{}
    for _, c := range n.hub.SessionsByUser(userID) {
        if c.UserID() == userID { seen[c.SessionID()] = struct{}{} }
    }
    if n.ClusterEnabled() {
        ids, _ := n.clusterSessionDirectory().ListUserSessions(ctx, userID)
        for _, sid := range ids {
            lease, err := directory.GetSessionLease(ctx, sid)
            if err != nil || lease == nil || lease.UserID != userID { continue }
            seen[sid] = struct{}{}
        }
    }
    return sorted(seen)
}
```

**禁止**索引 miss 时全集群 SCAN。索引陈旧靠 repair。

`SessionsByUser("")` 必须返回空，即使 shard 里有匿名连接。

## 5. 集群索引

```go
AddUserSession(ctx, userID, sessionID string, ttl time.Duration) error
RemoveUserSession(ctx, userID, sessionID string) error
ListUserSessions(ctx, userID string) ([]string, error)
```

noop / fake：Add/Remove 成功；List 返回空（或 fake 可记录，便于单测）。

Redis 键（对齐 presence 成员+索引）：

- `ml:cluster:user:member:{userID}:{sessionID}` — 值随意（`"1"`），TTL = **该次写入的 session lease TTL**（`n.sessionLeaseTTL()`）
- `ml:cluster:user:sessions:{userID}` — Set

`syncUserIndex(ctx, dir, oldLease, newLease, ttl)`：

- `newLease==nil`（Delete）：`RemoveUserSession(old.UserID, sid)`
- Put/CAS 成功：`old==nil` 或 user 相同 → `AddUserSession`（刷新 TTL）；user 变了 → `Remove` 旧 + `Add` 新
- 空 UserID：只 Remove（匿名不进索引）

**必须**在 directory 的 `PutSessionLease` / `CompareAndSwapSessionLease`（成功时）/ `DeleteSessionLease` 里调用，或在 Node 封装层、保证 resume CAS 也走到。推荐：**Redis directory 内部**在写完 lease 后维护索引（Delete 时若不知 user，先 GET lease 再 Remove）。Node 层再包一层也可以，但测 CAS。

Repair：周期 `SCAN ml:cluster:session:lease:*`（复用 `scanKeys` 若可导出，或 redis directory 新方法），读 lease JSON，对非空 UserID `AddUserSession`。不要并进频道投影修复。集群未启用则不跑。

## 6. 指标

`messageloop_admin_user_fanout` HistogramVec，标签 `op`。每次按 user 展开后 `Observe(len(sessions))`。注册 + 冒烟。

## 7. 必须存在的测试

| 测试 | 断言 |
| --- | --- |
| `TestHub_SessionsByUser` | 两 session 同 user → 返回 2；空 userID → 空；另一 user 不混入 |
| `TestAdmin_PublishDestinationUsers` | 单节点两客户端同 user，Publish `users=[U]`（**不填 sessions/channels**）两端都收到 publication |
| `TestAdmin_DisconnectUsers` | Disconnect `users=[U]` 两 session 均断开，`results` 两 key true |
| `TestAdmin_EmptyUserInvalidArgument` | `users=[""]` 或 Subscribe `user_id=""` 且 `session_id=""` → `InvalidArgument`，不扫 hub |
| `TestAdmin_SubscribeByUser` | Subscribe `user_id=U` + channel → 该 user 本地 session 进入 hub 订阅 |
| `TestExpandUserSessions_SkipsMismatchedLease` | 索引列出 sid 但 lease.UserID≠U → 不进展开结果、不 Close |
| `TestSyncUserIndex_MigratesOnUserChange` | old U1 → new U2：List U1 不再含 sid，List U2 含 sid |
| `TestAdmin_PublishUsersNoCluster` | `cluster.enabled=false` 只靠本地 SessionsByUser |

跨节点 Redis 测（有 Redis 才跑，Skip 对齐 `MESSAGELOOP_TEST_REDIS_ADDR`）：

| `TestAdmin_DisconnectUsersAcrossNodes` | U 在 nodeA/nodeB 各一 session；Disconnect `users=[U]` 两端都断 |

现有只填 `sessions`/`channels` 的 Admin 测试必须继续绿。

## 8. 文档

`03-admin-api.md`：

- Publish destination 增加 `users`；**只有 users 合法**
- Disconnect `users` 与 sessions 并集
- Subscribe/Unsubscribe `user_id` 与 `session_id` 并集；都空 InvalidArgument
- 展开校验 lease.UserID；空 user 不扫描

`04-cluster.md`：两把键、repair SCAN lease、索引非权威。

## 9. 验收清单

1. 按 user Disconnect 多 session（单节点必测；跨节点有 Redis 则测）。
2. Publish 只填 `destination.users` 能送到各端；不是「无 destination」。
3. 空 user → InvalidArgument。
4. 无 token 仍 Unauthenticated（现有拦截器，勿拆）。
5. 无 cluster 只走本地 SessionsByUser。
6. lease.UserID 不匹配则跳过。
7. resume/auth 换 user 后索引搬迁。
8. 无 proto 变更；`go test -count=1 . ./config/... ./pkg/grpcstream/... ./pkg/redisbroker/...` 与 `go test -race -count=1 .` 绿。

## 10. 完成报告

- 文件列表
- `SessionsByUser` / `expandUserSessions` / `syncUserIndex` / Publish users 分支 文件:行
- SessionDirectory 三个新方法的实现处
- §7 每个测试：过/失败/Skip
- §9 八条 + 证据
- 偏离与理由

## 11. 实现备注（落地后填写）

1. **目录内部维护索引（推荐路线）**：`PutSessionLease` / `CompareAndSwapSessionLease`（成功时）/ `DeleteSessionLease` 都在 Redis directory 内部调 `SyncUserIndex`（根包导出为 `SyncUserIndex`，供 redisbroker 跨包调用），resume 的 CAS 路径天然覆盖。索引写失败为 best-effort：记录 Warn 且不把错误上抛（索引非权威，repair + 展开时 lease 校验兜底），避免一次索引抖动导致 connect/resume 失败。
2. **Put 先 GET 旧 lease**：`PutSessionLease` 写前读一次旧 lease 用于比对 user，这样「resume 后 authUser 换 user」的搬迁（SREM 旧 / SADD 新）在下次 lease 写入时就完成，不需要依赖 repair。
3. **修复独立装配**：`ClusterUserIndexRepairer` 挂在 `ClusterDependencies.UserIndexRepairer` 上，`NewCluster` 在 deps 为空时自动从 SessionDirectory 推导（实现 `ClusterSessionLeaseLister` 才生效）。因 `cmd/server/main.go` 不在本次改动清单内，未在 main 显式装配；目录自身的 `Start` 即会随 cluster 启动。
4. **展开函数在 Node 上**：`ExpandUserSessions`（根包 cluster_user_index.go，跨包给 grpcstream 用故导出）+ `ObserveAdminUserFanout`；API handler 内统一用 `unionSessions` helper（显式 sessions ∪ users 展开，去重排序）处理四个 RPC。
5. **Subscribe/Unsubscribe 多 session 语义**：对每个 channel，任一 session 成功即该 channel 为 true（写进文档 03-admin-api.md）。
6. **测试**：§7 全过；新增两个真实 Redis 跨节点测试（本机 Redis 可用时跑过）：`TestAdmin_DisconnectUsersAcrossNodes`、`TestClusterRedis_ResumeUserChangeMigratesIndex`（§9.1/§9.7 的实据）。`go test -count=1 . ./config/... ./pkg/grpcstream/... ./pkg/redisbroker/...` 与 `go test -race -count=1 .` 全绿。
