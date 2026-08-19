# PR-KA-D13 第三方实现 Prompt(整段复制即可)

把下面「PROMPT」围栏里的全部内容发给实现 agent。不要摘要。做完后把完成报告交回主 agent 做严格验收。

---

````
你是 MessageLoop(Go 实时消息平台)的实现工程师,由主 agent 委派的子代理。项目根目录:

D:\Codes\qiulin\messageloop

当前分支应为 `v2`(D12 已合,tip `d16c1d2`)。工作区应为干净状态。

## 任务

独立实现 **PR-KA-D13**(KD-K26 阶段三(a):cluster 契约下沉 internal/cluster + metrics 下沉 internal/metrics)。唯一规格书(必须先通读再动手):

`docs/v2/tasks/pr-ka-d13-cluster-contracts.md`

规格书 §3 已含全部现状核实(搬运集逐符号清单、唯一导出手术、留根清单、消费方全集、测试逐文件判定),动手前按 §3 核对现码;行号若漂移,以语义定位为准。

## 目标(一句话)

cluster 控制面契约群 + epoch + `SyncUserIndex` → 新建 `internal/cluster`;`metrics.go` → 新建 `internal/metrics`;redisbroker/hmac/sim 契约面切直引;根包经 `aliases.go` 新增两组转发过渡,根包其余文件与 cmd/server 零改动。

## 硬约束

1. 只许改规格书 §2 路径。proto、`shared/`、`sdks/`、`config/`、`pkg/topics`、`pkg/transport/`、`internal/{protocol,channel,occupancy,stream,authz,admin}`、`proxy/`、`cmd/server` 零改动。
2. `cluster_epoch.go`、`metrics.go` 两枚整文件 `git mv`;部分搬出用「复制 + 删除原段落」。除 §3.2 授权导出手术、package/import 行、redisbroker/hmac/sim 限定名替换外,搬动内容逐字节不变。常量/码表值与 DTO 的 JSON tag 不变。
3. 导出手术仅限 `allocateNodeIncarnation` → `AllocateNodeIncarnation` 一项;`aliases.go` 放未导出包装(D11 `protocolGenerationOK` 先例),`cluster.go` 等留根文件逐字节不变(只删已搬出段落)。
4. `aliases.go` 只新增 §3.7 两组转发 + 一个包装;**不准**把根包既有文件改成直引 internal/*(§1.2);新位置代码(internal/cluster、internal/metrics)零根包引用。
5. 测试拆分按 §3.6:迁出测试为新包同包测试,除 package/import 行外逐字节不变;留根测试零改动;`TestNoUUIDIncarnationInProductionSource` 扫描清单授权加 `internal/cluster/epoch.go`。
6. 测试串行,绝不并发两个根目录 `go test`;Redis 用真实实例(127.0.0.1:6379,DB 14)。
7. 不做 git commit / tag / push;不产生格式 churn;工作区 .go 文件保持 CRLF。
8. `golangci-lint run ./...` 必须保持 0 issues。

## 验证

按规格书 §5 逐条执行并贴真实输出(含四条门禁 grep)。

## 完成报告(作为你的最终回复全文返回,不要只写进文件)

- 改动文件列表(git mv 映射 / 新增 / 修改分组)
- §6 每条 过/失败 + 证据
- 导出手术逐符号清单(名字前→后)
- 测试拆分逐个判定表 + 前后总数
- 测试命令与结果(真实输出)
- 偏离(应无)

另外:实现完成后,把实现备注填入规格书 `docs/v2/tasks/pr-ka-d13-cluster-contracts.md` §9。

完成后把完成报告全文作为最终回复返回给主 agent。
````
