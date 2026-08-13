# 继续修复：工作流 02 Go SDK（第 2 轮）

背景：你此前完成了 MessageLoop（`D:/Codes/qiulin/messageloop`）Go SDK 的修复。深度验收结论：全部条目正确落地、**放行**，以下为 3 项建议修（无必修）。范围仍限于 `sdks/go/`，禁止 git 写操作。

## 建议修

1. **`connectErrCh` 关闭后 `Connect` 误返回 nil**（`client.go` Connect 的 select 等待处与 `Close()` 的 `connectErrCh` 关闭处，约 1148 行附近）：`Close()` 关闭 `connectErrCh` 后，正在 `case err := <-c.connectErrCh` 等待的 `Connect` 会收到零值 nil，误报连接成功。修复：select 中检查 `closed` 标志（或 channel 的 ok 值），已关闭时返回明确的 "client closed" 错误。补并发测试（Connect 等待中调用 Close，断言返回非 nil 错误），`-race` 跑。
2. **示例 nil 防护**（`example/proxyserver/main.go:305-358` 附近）：`MyProxyService.RPC/Authenticate` 直接解引用 `resp.Payload/resp.Error/resp.UserInfo`，与你给 `HandlerImpl` 加的防护不一致。示例是用户的第一参照，补上同款 nil 判定。
3. **`MIGRATION_GUIDE.md` 补 breaking changes**：本轮引入的破坏性/行为变更需记录：`LifecycleHandler.OnSubscribed/OnUnsubscribed` 签名变为 `(ctx, sessionID, channel, username)`；`Client` 接口新增 `SubscribeWith/SubRefresh/SendSurveyReply/OnSurvey`；`RPC` 默认 30s 超时；`PingTimeout` 现在会实际关闭 transport 触发重连；`Connect()` 每次推进 generation。按该文件既有格式补一节。

## 验收标准

- `go build ./... && go test -race ./...`（在 `sdks/go/` 内）全绿。
- 返回报告：每条处置（已修/未修+原因）、改动文件、测试结果。
