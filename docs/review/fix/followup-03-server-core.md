# 继续修复：工作流 03 服务端核心（第 2 轮）

背景：你此前完成了 MessageLoop（`D:/Codes/qiulin/messageloop`）服务端核心（根包 `client.go`/`hub.go`/`node.go` 等）的修复。深度验收结论：全部条目正确落地，文档补 3513 由文档 agent 负责。以下为 4 项建议修（无必修，但 1-3 强烈建议）。范围与上次相同，禁止 git 写操作。

## 建议修

1. **ACL 注释修正**（`acl.go:127-139` 附近）：注释声称匹配语义 "consistent with the subscription matcher"，但你实现的 `**` 多段匹配超出了 `pkg/topics` CSTrie 的能力（matcher 只支持单段 `*`）——客户端按 `chat.**` 订阅时 CSTrie 把它当字面量。修正注释，明确说明"ACL 通配语法比订阅 matcher 更宽松：`*` 与 matcher 一致（单段），`**` 仅 ACL 层支持（多段）"。若你评估后认为应反向对齐（ACL 去掉 `**`），在报告中论证而不要直接改行为。
2. **`handleSubRefresh` presence leave 限流**（`client.go:1308-1311` 附近）：ACL 撤销订阅时每频道一个 `go PublishPresenceLeave(...)`，与你在 `handleUnsubscribe` 用的信号量限流（P1-A5）原则不一致。复用同一限流机制。
3. **connect 超限显式回滚**（`client.go:678` 附近）：`handleConnect` 发现订阅数超限 `return DisconnectChannelLimit` 时，此前已成功添加的频道仅依赖后续 `close()` 清理，存在瞬时脏状态。返回前对本轮 `addedChannels`/`addedPresence` 做显式回滚。
4. **补 P1-A2 客户端级回滚测试**：现有 `TestHub_ReplaceSession_FailureKeepsOldSessionIntact` 只覆盖 hub 层不变性。补一个客户端级测试：构造 resume 时 `ReplaceSession` 失败（连接数超限），断言旧会话的订阅、presence、cluster 状态全部按 `client.go:545-571` 的回滚路径清理干净。
5. **行尾统一**：`client_fix_test.go` 工作区为 LF，与仓库统一的 CRLF 不一致——转换为 CRLF（可用 `unix2dos` 或编辑器，勿改动内容）。

## 验收标准

- `go build ./... && go test -race -count=1 .` 全绿。
- 返回报告：每条处置、改动文件、测试结果；条目 1 若选择改行为需显著标注。
