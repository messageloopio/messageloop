# PR-KA-A1 实现规格：续约改为 same-fence CAS，删除盲写 Put

| 字段 | 值 |
| --- | --- |
| 标题 | `cluster: refresh session lease with same-fence CAS; rollback failed takeover` |
| 状态 | **Accepted**（2026-08-16 主 agent 终验通过，尚未 commit） |
| 依赖 | 无。可与 PR-KA-A0 并行。**不**依赖 v2 proto |
| 设计来源 | [kernel-architecture.md](../kernel-architecture.md) Cluster / Bind、KD-K4、KD-K5、KD-K30、KD-K31 |
| 验收人 | 主 agent |

## 1. 目标

现行 `syncClusterSessionState` 对 lease 做无条件 `PutSessionLease`（Redis `SET`），会把已被他节点 CAS 抢走的所有权写回去。本 PR 在 **现行类型** 上改成：

1. 续约 / 订阅 saga 同步 = **same-fence CAS**（`LeaseVersion` 不升）。
2. 首次登记 = **CAS(expected=nil)**，禁止盲写覆盖。
3. 发现 Directory 上的 fencing 已不是自己 → 返回可识别错误，ping/pong 路径 **Fence 断开**（`DisconnectStale`）。
4. `resumeRemoteSession` 在 CAS 成功后 takeover 失败且旧 node lease 仍在时 **回滚 CAS**。

本 PR **不**实现：Session/Attachment 拆分、`node_epoch` INCR、换命令总线、HMAC、v2 proto、Occupancy LiveBus、删 `PutSessionLease` 接口方法本身（测试 fake 可留；**生产热路径禁止调用**）。

独立版本（KD-K31）：不必兼容「旧二进制仍 Put」。本 PR 合入后的节点不得再盲写。

## 2. 允许改动的文件

- `cluster_state.go`：`syncClusterSessionState` 及本 PR 新增的 refresh/bind helper、哨兵错误
- `cluster_resume.go`：`resumeRemoteSession` 失败回滚
- `client.go`：**仅** `throttledClusterRefresh` / ping/pong 对 fenced 错误的处理（断开）。禁止改 handleConnect 鉴权、handlePublish、Survey、presence 业务
- `cluster.go`：仅当需要给 `SessionDirectory` 加注释或导出错误时
- `pkg/redisbroker/cluster_directory.go`：仅当 CAS(nil) 或 refresh 需要修实现 bug；**禁止**把 `PutSessionLease` 改回热路径
- 测试：`cluster_state_test.go`、`cluster_resume_test.go`、`cluster_remote_test.go`、`client_fix_test.go`、必要时 `cluster_test.go` / `cluster_offsets_test.go`
- `docs/developer/04-cluster.md`：lease 续约改为 CAS，点明盲写已删
- `docs/v2/tasks/pr-ka-a1-fencing.md`（完成备注）

禁止：改 proto、SDK、`hub.go` 扇出、`broker.go`、git 写操作。

## 3. 现状（动手前再读）

- `Node.syncClusterSessionState`（`cluster_state.go`）：`PutSessionLease` + `PutSessionSnapshot`。调用方：`AddClient`、订阅 saga 的 `cluster.session`、`throttledClusterRefresh`（ping/pong）。
- `redisSessionDirectory.PutSessionLease`：无条件 `SET`，再 `syncUserIndex`。
- `CompareAndSwapSessionLease`：`WATCH` + `clusterSessionLeaseEqual`（只比 `SessionID, NodeID, IncarnationID, LeaseVersion`）。`expected==nil` 且 key 不存在时应能成功（已有 `left==nil && right==nil`）。
- `resumeRemoteSession`：CAS version+1 后 `requestSessionTakeover`；失败且 `GetNodeLease(旧)` 非 nil 则 **return err，不回滚**。
- `throttledClusterRefresh`：sync 失败只 `Warn`，不断开。
- 死节点旁路（保留，KD-K30）：takeover 失败但旧 node lease 已无 → 继续 resume。

## 4. 错误

在根包导出（或 `cluster_state.go`）：

```go
// ErrSessionFenced means Directory no longer recognizes this node's fencing
// for the session. Callers that hold a local attachment must Fence it
// (DisconnectStale) and must not Unbind the new owner's lease.
var ErrSessionFenced = errors.New("session fenced by another owner")
```

`errors.Is` 可识别。不要用字符串包含判断。

## 5. 算法

### 5.1 `syncClusterSessionState`（替换 Put lease）

伪代码，顺序不得改：

```
if !ClusterEnabled || client==nil: return nil
dir = directory
desired = clusterSessionLease(client)    // version 取 client.clusterLeaseVersion，0 当 1；不在这里 +1
snap = clusterSessionSnapshot(client)

current, err = dir.GetSessionLease(ctx, desired.SessionID)
if err != nil: return err

if current == nil:
    ok, err = dir.CompareAndSwapSessionLease(ctx, nil, desired, sessionLeaseTTL())
    if err != nil: return err
    if !ok: return ErrSessionFenced
    return dir.PutSessionSnapshot(...)

if current.NodeID != ClusterNodeID() ||
   current.IncarnationID != ClusterIncarnationID() ||
   current.LeaseVersion != desired.LeaseVersion:
    return ErrSessionFenced

// same fence: refresh TTL / LastActivity / UserID 等；LeaseVersion 不变
ok, err = dir.CompareAndSwapSessionLease(ctx, current, desired, sessionLeaseTTL())
if err != nil: return err
if !ok: return ErrSessionFenced
return dir.PutSessionSnapshot(...)
```

禁止：任何分支调用 `PutSessionLease`。

`desired.LeaseVersion` 必须等于内存里的 `client.clusterLeaseVersion`（0→1），**刷新不得 +1**。升版本只发生在现有 `resumeRemoteSession` 的抢权路径。

### 5.2 ping / pong

`throttledClusterRefresh` 里对 `syncClusterSessionState`：

- `errors.Is(err, ErrSessionFenced)` → 对该 client `close(DisconnectStale)`（或等价 `disconnectHeartbeatTimeout` 同风格的单次 close）。不要 Unbind / 不要 `deleteClusterSessionState`（那会误删新 owner，现有 `deleteClusterSessionState` 已有所有权检查，但 Fence 路径根本不该调它）。
- 其他错误：维持今日 Warn，不断开（避免 Redis 抖一下踢光全员）。

### 5.3 `AddClient` / 订阅 saga

继续调 `syncClusterSessionState`。首次 AddClient：Directory 无 key → §5.1 的 CAS(nil)。被抢后的 saga 同步：§5.1 中间分支 → `ErrSessionFenced` → 今日 saga 会回滚订阅；这是正确的。

### 5.4 `resumeRemoteSession` 回滚

在现有「CAS 成功」之后、「写 client 字段」之前：

```
if 需要对旧 owner 发 takeover:
    err = requestSessionTakeover(ctx, lease)  // lease 仍是 CAS 前的旧记录
    if err != nil:
        nodeLease, lerr = GetNodeLease(旧 NodeID, 旧 IncarnationID)
        if lerr != nil: 仍必须尝试回滚，然后 return lerr
        if nodeLease != nil:  // 旧节点仍活着（非 KD-K30 旁路）
            // 回滚：CAS(desired, originalLease) 把 fencing 还回去
            _, _ = CompareAndSwapSessionLease(ctx, desired, lease, ttl)
            return err
        // nodeLease==nil：死节点，保留新 CAS，继续
```

回滚 CAS 失败：打 Error 日志，仍 `return` 原先 takeover 错误（不要假装 resume 成功）。不要 `DeleteSessionLease` 除非能证明自己仍是 owner 且回滚 CAS 因 key 形状无法恢复——默认只 CAS 回去。

`lease`（CAS 前）与 `desired`（CAS 后）变量不要弄反。

## 6. 必须存在的测试

1. **刷新不升版本**：同一 client 两次 `syncClusterSessionState`，第二次成功后 `GetSessionLease.LeaseVersion` 与第一次相同，ExpiresAt 刷新。
2. **B 抢走后 A 不得写回**（核心）：Directory 里放入 B 的 lease（version=N+1，NodeID=B）。A 节点上 client 内存 version=N。A 调 `syncClusterSessionState` → `ErrSessionFenced`，随后 Get 仍是 B 的 lease。
3. **首次创建**：空 Directory，AddClient/sync → lease 存在 version=1。并发两个 CAS(nil) 只有一个成功。
4. **ping fenced 断开**：在测试里让 sync 返回 `ErrSessionFenced`（fake directory 或注入），走 `throttledClusterRefresh` / 导出的处理函数 → client 关闭码 3502。不要依赖 10s 节流：直接调处理分支或把 interval 测时调短。
5. **resume 回滚**：fake：CAS 成功、takeover 返回 error、GetNodeLease 返回非 nil → 最终 lease 回到 CAS 前的 owner；client 得到错误。
6. **死节点不回滚**：takeover 失败、GetNodeLease 返回 nil → lease 保持新 owner，resume 继续（现有旁路）。
7. 现有 cluster resume / remote 测试仍绿；若有断言「sync 使用 Put 次数」的 fake，改为允许 0 次 Put。

禁止用固定 `time.Sleep` 代替同步点。

## 7. 文档

`docs/developer/04-cluster.md`：删「Put 续约」表述；写明续约是 same-fence CAS、抢权才 +version、盲写已删除。点明 ping 发现 fenced 会 3502。不要再写 ChannelOffsets「未填充」（若该节仍错，顺手改对，算本 PR 允许的文档修正）。

## 8. 验收清单

1. `syncClusterSessionState` 源码中 **零** `PutSessionLease` 调用。
2. `grep PutSessionLease` 热路径（`cluster_state.go` / `client.go` / `cluster_resume.go` / `node.go`）无生产调用。测试 fake 实现方法可保留。
3. §6 测试全绿；`go test ./...`；`go test -race ./`（根包，含 cluster）。
4. 刷新不 +version。
5. 活节点 takeover 失败会回滚 CAS。
6. 死节点旁路仍在。
7. fenced ping 断开且不删新 owner 的 lease。

## 9. 完成报告

- 改动文件列表
- §8 逐条证据（文件:行）
- 测试命令与结果
- 偏离（应无）

## 10. 实现备注（完成后填写）

### 完成情况（2026-08-16）

- 实现文件：`cluster_state.go`、`cluster_resume.go`、`client.go`、`cluster.go`（仅注释）。
- 测试：`cluster_state_test.go`（§6.1/6.2/6.3）、`cluster_resume_test.go`（§6.5/6.6 + §5.4 GetNodeLease 错误仍回滚）、`client_fix_test.go`（§6.4 ping fenced 3502）、`cluster_remote_test.go`（fake 改造）。
- 文档：`docs/developer/04-cluster.md`（§4.1/4.3/4.4/4.5；ChannelOffsets/BrokerEpoch 已填充的旧文修正）。
- `syncClusterSessionState` 零 `PutSessionLease`；热路径 grep 无生产调用。
- 一处伪代码解释：§5.1 中间分支的版本比较实现为「NodeID/IncarnationID 不同 → fenced；Directory 版本 > 本地版本 → fenced；本地版本 ≥ Directory 版本 → same-fence CAS（相等时刷新，本地更大时写透本机接管已 +1 的版本，不新建递增）」。若不写透，`handleConnect` 的本机 resume 内存版本 +1 后同步必被 fenced，集群模式本机 resume 将永远失败（§6.7「现有 resume 测试仍绿」要求）。刷新路径从不 +1。
- `noopSessionDirectory.CompareAndSwapSessionLease` 由 `(false,nil)` 改为 `(true,nil)`：noop 后端没有可冲突的远程 Directory，新 sync 依赖 CAS 成功。
- 验证：`go test ./...` 全绿；`go test -race .` 全绿；另起真实 Redis 容器跑通全部 `TestClusterRedis_*` 集成测试（含 `TestClusterRedis_RemoteResumeTakeover`）。
