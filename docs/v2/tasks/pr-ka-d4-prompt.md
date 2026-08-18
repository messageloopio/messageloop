# PR-KA-D4 第三方实现 Prompt（整段复制即可）

把下面「PROMPT」围栏里的全部内容发给实现 agent。不要摘要。做完后把完成报告交回主 agent 做严格验收。

---

````
你是 MessageLoop（Go 实时消息平台）的实现工程师。项目根目录：

D:\Codes\qiulin\messageloop

当前分支应为 `v2`（已含 A0–C6、D1–D3；D3 tip 为 `daf22a8`）。

## 任务

独立实现 **PR-KA-D4**(LiveBus 缓冲满语义：occupancy 优先丢 + 频道降级标记）。唯一规格书（必须先通读再动手）:

`docs/v2/tasks/pr-ka-d4-buffer-full.md`

背景（只读）:`docs/v2/kernel-architecture.md` :409 缓冲满合同、:360 Publish 成功合同；`docs/v2/tasks/pr-ka-d3-observability.md`(live_drop_total 已落地的部分）。规格书与设计冲突时**以规格书为准**。

先读这些现码再动手：

- `pkg/redisbroker/pubsub.go` 全文件（消费循环、`dispatch`/`dispatchOccupancy`、`deliverOnce` 锁序、`noteLiveSeqGap`、`setActivePubSub`、worker 池）
- `pkg/redisbroker/redis.go`(`redisBroker` 字段、`SetMetrics` 范式）
- `metrics.go` / `metrics_test.go`(Gauge 注册与测试模式）
- `pkg/redisbroker/pubsub_test.go`(D3 新增的 live drop 测试，本 PR 要扩）

## 目标（一句话）

投递受压时优先丢 occupancy（计数 + 降级标记 + Warn),publication 保持反压，降级频道在下次成功入队时恢复；零行为语义改动。

## 硬约束

1. 只许改规格书 §2 路径。
2. `dispatch`（publication）阻塞发送语义零改动；只在 `dispatchOccupancy` 引入非阻塞丢弃。
3. 锁序保持 `deliverMu → subMu`，不得出现反向获取；新增锁须在实现备注里论证。
4. `live_drop_total` 不得双计（occupancy 无 seq;publication 不在 dispatch 点丢）。
5. 不做降级标记的消费方（不反向影响 Interest/发布判定）；不动 go-redis 缓冲与 socket 读取层。
6. 测试禁止固定长 Sleep；串行执行，绝不并发两个根目录 `go test`;Redis 测试用真实 Redis(127.0.0.1:6379,DB 14，沿用 `requireCommandBusRedis` guard)。
7. 不做 git commit / tag / push；不产生格式 churn（终验对照 `git diff --numstat` 与 `--ignore-all-space --ignore-cr-at-eol`)。

## 验证

按规格书 §5 逐条执行并贴真实输出（含 `go test -count=1 ./...` 全量与 grep 门禁）。

## 完成报告

- 改动文件列表
- §6 每条 过/失败 + 证据
- 测试命令与结果（真实输出）
- 偏离（应无）

另外：实现完成后，把实现备注填入规格书 `docs/v2/tasks/pr-ka-d4-buffer-full.md` §8。
````
