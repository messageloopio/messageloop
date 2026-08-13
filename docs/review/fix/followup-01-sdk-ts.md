# 继续修复：工作流 01 TypeScript SDK（第 2 轮）

背景：你此前完成了 MessageLoop（`D:/Codes/qiulin/messageloop`）TS SDK 的修复。深度验收结论：P0/P1 全部正确落地，但有 2 项必修 + 若干建议修。范围仍限于 `sdks/ts/`，禁止 git 写操作。

## 必修

1. **重新删除 `src/proto/v1/service_pb.ts`**。验收发现：你此前已删除该旧布局残留文件（git status 曾显示 `D`），但后续环节中它被恢复且内容与 HEAD 一致，当前工作区仍存在。确认全 src 无引用后 `rm sdks/ts/src/proto/v1/service_pb.ts`（该目录应整体消失）。若目录下只剩这一个文件，连同空目录一并清理。
2. **订阅回写去条件**（`src/client/client.ts:224-228`）：当前 "connected" 分支用 `if (serverSubs.length > 0)` 条件回写本地订阅集，服务端返回空列表时本地会残留已退订频道。改为**无条件以服务端 `subscriptions` 为准**：清空本地集合并按服务端列表重建（对齐 Go SDK `sdks/go/client.go:436-447` 的语义）。注意与同一分支的 `publications` 投递、`resubscribeAllChannels` 的交互，补一个回归测试：服务端返回的列表不含某本地频道时，重连后该频道不再被 resubscribe。

## 建议修（低成本则做，否则报告中说明）

3. `pnpm-lock.yaml` 仍引用 `@grpc/grpc-js` 与旧 peerDependencies——重新生成 lockfile 或手工移除该条目（你之前已从 package.json 删除该依赖）。
4. legacy `onMessage` 与 `addMessageHandler` 双套 API 并存、混用重复投递——在 README 或类型注释中明确推荐用法（统一为 add 系列，onXxx 标注为便捷别名），不要求改行为。

## 验收标准

- `npm test` 全绿（含新回归测试）、`npm run build` 通过。
- 返回报告：每条处置、改动文件、测试结果。
