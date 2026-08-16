# PR-KA-A3 第三方实现 Prompt（整段复制即可）

把下面「PROMPT」围栏里的全部内容发给实现 agent。不要摘要。做完后把完成报告交回主 agent 做严格验收。

---

````
你是 MessageLoop（Go 实时消息平台）的实现工程师。项目根目录：

D:\Codes\qiulin\messageloop

当前分支应为 `v2`（已含 A0–A2）。

## 任务

独立实现 **PR-KA-A3**。唯一规格书（必须先通读再改代码）：

`docs/v2/tasks/pr-ka-a3-livebus.md`

背景（只读）：`docs/v2/kernel-architecture.md` LiveBus 编译、KD-K13、KD-K13b。规格书与设计冲突时 **以规格书为准**。

先读这些现码再动手：

- `pkg/redisbroker/pubsub.go` `runPubSub`（今日 `PSubscribe(prefix+"*")`）
- `pkg/redisbroker/redis.go` `Subscribe` / `Unsubscribe` / `interested` / `Publish`
- `broker_memory.go` `Subscribe`（A2 已有 matcher）
- `client.go` `handleSubscribe` / `handleConnect` 里 `AddSubscription` 失败路径
- `pkg/topics/matcher.go` `ValidateTopic` / 段匹配

## 目标（一句话）

按 CompileInterest 只订精确频道和可编译前缀 pattern；删除默认 `PSubscribe prefix*`；不可路由 pattern 对客户端软失败、不断连。

## 硬约束

1. 只许改规格书 §2 路径。
2. 热路径禁止 `PSubscribe(..., PubSubPrefix+"*")` 或把 Pattern 编成单独的 `*`。
3. `*.room`、`**`、`*`、`im.*.tick` 必须 `ErrPatternNotRoutable`。
4. `im.**` 必须能收到 `im` 与 `im.x`；`im.room.*` 不得把 `im.room.a.b` 交给 handler。
5. handleSubscribe / handleConnect 对 NotRoutable **不得** return err 断连。
6. 不做 git commit / tag / push。不要做 Occupancy 控制通道、不要切 clientv2。
7. 测试禁止用固定长 Sleep 代替 Ready/Eventually。

## 验证

```bash
go test ./...
go test -race . ./pkg/redisbroker
```

对照规格书 §8 测试表与 §9 清单自检。

## 完成报告

- 改动文件列表
- §9 每条 过/失败 + 证据
- 测试命令与结果
- 偏离（应无）
````
