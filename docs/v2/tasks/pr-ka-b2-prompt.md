# PR-KA-B2 第三方实现 Prompt（整段复制即可）

把下面「PROMPT」围栏里的全部内容发给实现 agent。不要摘要。做完后把完成报告交回主 agent 做严格验收。

---

````
你是 MessageLoop（Go 实时消息平台）的实现工程师。项目根目录：

D:\Codes\qiulin\messageloop

当前分支应为 `v2`（已含 A0–A4、B1）。

## 任务

独立实现 **PR-KA-B2**。唯一规格书（必须先通读再改代码）：

`docs/v2/tasks/pr-ka-b2-occupancy.md`

背景（只读）：`docs/v2/kernel-architecture.md` Occupancy 节、KD-K7、KD-K8、KD-K9、KD-K9b。规格书与设计冲突时 **以规格书为准**。

先读这些现码再动手：

- `node.go` `presenceJoin` / `presenceLeave` / `emitPresence` / `deliverPresenceEvent` / `shouldTrackPresence`
- `hub.go` `broadcastPublication` 开头的 `ml.type` 改写与 `Hub.node`
- `presence_event.go` `presencePublication` / `ml.type`
- `config/config.go` `server.presence.cluster_emit`
- `pkg/redisbroker/pubsub.go` 收包循环（只认 publication）
- A3 `interested()` / `CompileInterest`（`im.**` 已能订 `im` + `im.*`）

## 目标（一句话）

Occupancy 事件只走 LiveBus 精确频道 + OccupancyGen；删掉 cluster_emit 和 Hub 对 ml.type 的改写。

## 硬约束

1. 只许改规格书 §2 路径。
2. 禁止 `PublishTransient` 发 occupancy；禁止 `emitPresence` 既 Publish 又本地 deliver（双发）。
3. 删除 `Hub.node` 与 `broadcastPublication` 的 `ml.type` 分支。
4. 删除 `cluster_emit` 热路径；YAML 再写则 Validate 失败。
5. 不要改 v1 proto。gen 放在 LiveBus `OccupancyEvent` 上，不要塞进 v1 `PresenceEvent`。
6. 不要把 Broker 改名为 LiveBus。不要做流式恢复、HMAC、internal/*、不要切 clientv2。
7. 不做 git commit / tag / push。
8. 测试禁止用固定长 Sleep 代替 Ready/Eventually。

## 验证

```bash
go test ./...
go test -race . ./pkg/redisbroker
```

对照规格书 §7 测试表与 §8 清单自检。

## 完成报告

- 改动文件列表
- §8 每条 过/失败 + 证据
- 测试命令与结果
- 偏离（应无）
````
