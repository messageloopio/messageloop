# PR-KA-D14 第三方实现 Prompt(整段复制即可)

把下面「PROMPT」围栏里的全部内容发给实现 agent。不要摘要。做完后把完成报告交回主 agent 做严格验收。

---

````
你是 MessageLoop(Go 实时消息平台)的实现工程师,由主 agent 委派的子代理。项目根目录:

D:\Codes\qiulin\messageloop

当前分支应为 `v2`(D13 已合,tip `a2aa7a2`)。工作区应为干净状态。

## 任务

独立实现 **PR-KA-D14**(KD-K26 阶段三(b):session plane 下沉 internal/session + `Session.node` 依赖反转为注入的 `Runtime` 接口)。唯一规格书(必须先通读再动手):

`docs/v2/tasks/pr-ka-d14-session-plane.md`

规格书 §3 已含全部现状核实(搬运集、Runtime 接口全文、导出手术全集与逐触点行号、适配器设计、测试逐文件判定),动手前按 §3 核对现码;行号若漂移,以语义定位为准。

## 目标(一句话)

session.go/client.go/heartbeat.go/hub.go/pool.go/transport.go → `internal/session`,survey.go → `internal/survey`;Session 的 `node *Node` 换成 `rt Runtime`(接口在 internal/session,根包 `session_runtime.go` 写 nodeRuntime 适配器 + NewClient 薄包装);aliases.go 加两组转发,transports/cmd/server/留根测试零改动。

## 硬约束

1. 只许改规格书 §2 路径。proto、`shared/`、`sdks/`、`config/`、`pkg/topics`、`pkg/transport/`、`internal/{protocol,channel,occupancy,stream,authz,admin,cluster,metrics}`、`proxy/`、`cmd/server`、`pkg/redisbroker` 零改动。
2. 7 枚整文件 `git mv`;迁入文件除 package/import 行、§4.2 的 `s.node.`→`s.rt.` 机械重写、§3.5 三处杂项外逐字节不变。常量/码表值/JSON tag 不变。
3. 导出手术仅限 §3.3 的 13 项(Session 7 + Hub 6);`Runtime` 接口成员与 §3.2 全文一致;Node 本体零导出手术;`RestoreFailure` 逐字段映射。
4. 锁序不变量(§1.1):`subLock(ch)` 在外 → Hub 分片锁 → `Session.mu` 最内,迁移前后逐字保持;`AdoptIdentity`/`TrackChannel`/`ForceTrackChannel`/`UntrackChannel` 语义与原版逐分支等价。
5. `aliases.go` 只新增 §3.7 两组;新位置生产代码(internal/session、internal/survey)零根包引用、直引 internal/* 与 shared;留根文件除 §2 授权机械改写外逐字节不变。
6. 测试按 §3.6:仅 session_test.go、hub_test.go 随迁(断言逐字不变,仅构造改 fakeRuntime);留根测试除 5 行 `broadcastPublication`→`BroadcastPublication` 机械改名外零改动。
7. 测试串行,绝不并发两个根目录 `go test`;Redis 用真实实例(127.0.0.1:6379,DB 14,Docker 容器 `messageloop-test-redis`,若未运行先 `docker start messageloop-test-redis`)。已知 flake:`go test ./...` 包间并发时 redisbroker 的 DB14 FlushDB 可打到根包 cluster 原子写测试(backlog #8),单包复跑确认非回归即可,不算失败。
8. 不做 git commit / tag / push;不产生格式 churn;工作区 .go 文件保持 CRLF。
9. `golangci-lint run ./...` 必须保持 0 issues。

## 验证

按规格书 §5 逐条执行并贴真实输出(含四条门禁)。

## 完成报告(作为你的最终回复全文返回,不要只写进文件)

- 改动文件列表(git mv 映射 / 新增 / 机械改写分组)
- §6 每条 过/失败 + 证据
- 导出手术逐符号清单(名字前→后)+ Runtime 接口实现对照
- 测试迁移/适配逐个判定表 + 前后总数
- 测试命令与结果(真实输出)
- 偏离(应无)

另外:实现完成后,把实现备注填入规格书 `docs/v2/tasks/pr-ka-d14-session-plane.md` §9。

完成后把完成报告全文作为最终回复返回给主 agent。
````
