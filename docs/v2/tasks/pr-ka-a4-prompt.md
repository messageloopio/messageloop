# PR-KA-A4 第三方实现 Prompt（整段复制即可）

把下面「PROMPT」围栏里的全部内容发给实现 agent。不要摘要。做完后把完成报告交回主 agent 做严格验收。

---

````
你是 MessageLoop（Go 实时消息平台）的实现工程师。项目根目录：

D:\Codes\qiulin\messageloop

当前分支应为 `v2`（已含 A0–A3）。

## 任务

独立实现 **PR-KA-A4**。唯一规格书（必须先通读再改代码）：

`docs/v2/tasks/pr-ka-a4-authorizer.md`

背景（只读）：`docs/v2/kernel-architecture.md` 的 Authorizer 节、KD-K10、KD-K15、KD-K21、KD-K31。规格书与设计冲突时 **以规格书为准**。

先读这些现码再动手：

- `acl.go` / `acl_test.go`（CanSubscribe last-write-wins、中段 `**`）
- `channel_policy.go`（另一张 first-match 表）
- `client.go` `checkSubscribeACL` / `handlePublish` / `handleSurvey` / `handleRPC`（代理短路、RPC echo）
- `config/config.go` `Server.ACL` / `Server.Channels` / `Validate`
- `node.go` 装配 ACL + ChannelPolicyEngine
- `cluster_commands.go` `AdminCanSubscribe`
- `pkg/grpcstream/api_handler.go` `GetHistory` / `GetPresence`（无 Capability）
- `interest.go` `CompileInterest`（A3，先于 ACL）

## 目标（一句话）

一个 Authorizer.Decide，一张 YAML 表，SubscribePattern 语言包含；Capability 闭集；Proxy 不短路；RPC 无代理返回 NO_PROXY。

## 硬约束

1. 只许改规格书 §2 路径。
2. 禁止保留 `ACLEngine` 热路径、禁止再读 `server.acl` / `server.channels`。
3. deny 不可被更具体的 allow 打洞。中段 `**` 非法。
4. `L(p)∩L(d)` 必须按 §5.2 求交，禁止枚举频道。
5. 代理允许不得越过静态 deny。
6. 无代理 RPC 不得 echo 成 RpcReply。
7. 不做 git commit / tag / push。不要做 Session 拆分、Occupancy LiveBus、流式恢复、HMAC、不要切 clientv2。
8. 测试禁止用固定长 Sleep 代替同步点。旧 last-write-wins / first-match / echo 测试必须改写，不要兼容层保绿。

## 验证

```bash
go test ./...
go test -race . ./config ./pkg/grpcstream
```

对照规格书 §9 测试表与 §10 清单自检。

## 完成报告

- 改动文件列表
- §10 每条 过/失败 + 证据
- 测试命令与结果
- 偏离（应无）
````
