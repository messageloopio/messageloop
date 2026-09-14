# 评审修复实施验收记录（2026-09-14）

分支 `fix/2026-09-14-review-p0`，按 subagent-sequential-implement 流程：4 个实现阶段，每阶段一个子代理、主会话一手验收（读 diff + 亲自跑测试 + 全局一致性检查）后提交还原点。设计底稿：`2026-09-14-comprehensive-review.md`（发现编号）与 `2026-09-14-mechanism-gap.md`（机制编号）。

## 阶段还原点

| 阶段 | Commit | 范围 | 修复项 |
|------|--------|------|--------|
| ① | `93fe291` | internal/session | #1 MarkMetricsCharged 锁重入死锁；#3 陈旧 shell delegate 误杀（新增 `closeIfServingHandoff`，transport 身份比较）；同模式锁审计（唯一真重入点已修，session.go:542 与 node.go:393 核实为锁外豁免） |
| ② | `ae2f531` | internal/session、proxy | #4 SubRefresh 越权 + G1 中央 namespace 预检（`precheckNamespace` 于 `handleMessage` 认证门后；Subscribe 逐频道过滤，Publish/Unsubscribe/SubRefresh/SurveyRequest/PresenceQuery 整体拒绝，行为逐类复刻现状；豁免 RpcRequest/SurveyReply 均经 handler 核实）；#11 RPC protojson `DiscardUnknown` |
| ③ | `e2b690a` | pkg/transport/ws | #2 解压炸弹：`NextReader` + `LimitReader(maxSize+1)` 有界读；G6 安全夹具（16MB 炸弹帧实测 1.4MB 分配，预算 4MB，TotalAlloc 断言） |
| ④ | `0c040c0` | internal/runtime、internal/cluster、pkg/redisbroker、internal/metrics | #14 `SessionLeaseOwnerDeleter` 可选接口 + Lua CAS-DEL（镜像既有 CAS 脚本），`deleteClusterSessionState` 与 repairer onLeave 改原子删除；#8 可观测部分：`cluster_node_lease_renew_failures_total` + 连击跨租约 TTL 时 Warn→Error 升级 + 恢复 Info（自动断开会话为标注的后续任务） |

## 每阶段红-绿证据

- ①：stash 还原缺陷代码后，死锁测试 10s watchdog FAIL、链式 resume 测试 FAIL（E 被杀、hub 移除）；恢复后全绿。
- ②：注释预检调用后，unsubscribe/sub_refresh 拒收子测试 FAIL（纵深防御使既有 5 类保持绿——单层禁用对外不可见，符合设计）；恢复后全绿。
- ③：LimitReader 放大/回退 `io.ReadAll` 两种回归形态均 FAIL（37.5MB 分配超 4MB 预算 / 16.7MB 被正常接受），恢复后全绿。
- ④：核心竞态用例"GET 与 CAS-DEL 之间被 node-b 接管后 stale reader 不删"PASS；fallback、升级阈值(3/4/1)、恢复不回零均有专门用例。

## 终验（主会话执行）

- `go vet ./...` 干净；`go test -count=1 ./...` 全量 22+ 包无 FAIL。
- 改动范围：24 文件，+2046/−22，全部落在 4 个阶段声明的边界内；无 go.mod 变更；阶段间无相互破坏（每阶段验收含前序回归）。
- 机制落地对照：G1（中央预检 + census 测试钉分类）、G3（`*Locked` 惯例修复 + 竞态回归）、G6（安全夹具）已随补丁落地；G2/G4/G5 及 M-2/M-3 仍为独立后续任务。

## 已知边界（如实记录）

1. 阶段④ Lua 脚本未真机执行（本环境无 Redis，`TestClusterRedis_*` SKIP）：结构逐条镜像已上线 CAS 脚本，已补真机集成用例 `TestClusterRedis_DeleteSessionLeaseIfOwner`，需在有 Redis 的 CI 环境跑一次确认 EVALSHA 行为。
2. 评审遗留未修（按行动顺序属"本周"批次）：#5/#6 SDK 断连码重连状态机、#7 无界状态族（G2 机制）、#10/#34 传输共享内核（M-2）、#12 Validate proxy 段（G5）、#13 admin fail-open。
3. #8 的语义部分（续租失败自动断开本机会话）按设计留作后续行为变更任务。
