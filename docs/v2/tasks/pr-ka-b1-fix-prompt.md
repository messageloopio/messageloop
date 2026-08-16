# PR-KA-B1 打回修复 Prompt（整段复制即可）

把下面「PROMPT」围栏里的全部内容发给**原实现 agent**。不要摘要。只修终验打回的 race，不要重开 B1 范围。做完把完成报告交回主 agent 再验。

---

````
你是 MessageLoop（Go 实时消息平台）的实现工程师。项目根目录：

D:\Codes\qiulin\messageloop

当前分支应为 `v2`。PR-KA-B1 主体已实现，主 agent 终验 **打回**：`go test -race` 在 Connect 与 Close 并发时失败。你只修这一处，然后交回终验。

## 唯一阻塞

`TestClientSession_HandleMessage_Connect_ConcurrentWithClose`（`client_fix_test.go`）在 `-race` 下失败：

- 写：`Session.Close` 持 `s.mu` 执行 `s.attachment = nil`（`session.go` 约 510–511 行）
- 读：`handleConnect` **无锁** 读 `c.attachment.Transport.RemoteAddr()`（`client.go` 313 行）

```go
// client.go handleConnect，组 AuthenticateProxyRequest 时：
RemoteAddr: c.attachment.Transport.RemoteAddr(),
```

`Close` 与 `handleConnect` 可并发（该测试就是这么写的）。B1 把 `attachment` 收成可空指针后，这是新竞态。

对照已正确的写法：`ClientInfo()`（`client.go` 约 812–817）在 `RLock` 下拷出 `att`，nil 则跳过。`handleConnect` 后面拷 `tempAtt` / `att` 再 `Attach` 的路径也已经加锁。只有 313 行漏了。

## 要求

1. 在持 `c.mu`（`RLock` 即可）下拷出 `attachment`（或 `RemoteAddr` 字符串），再组 `AuthenticateProxyRequest`。禁止无锁解引用 `c.attachment`。
2. `attachment == nil` 或 `Transport == nil`：按已关闭处理，返回 `DisconnectConnectionClosed`（或与该函数其它「会话已关」路径一致），不要 panic。
3. 全文件再扫一遍生产路径：`c.attachment` / `s.attachment` 的读必须在 `mu` 下，或先在锁内拷到局部变量。测试里的直接读可以不动。
4. 不要改 B1 合同：不要恢复 `ReplaceSession` / `closeQuiet` / `evictSessionForTakeover`；不要动 A1 CAS、A2 gap、A3 live、A4 Decide；不要改 proto。
5. 允许的文件：`client.go`（必改）；若你发现同类无锁读只在 `session.go` 的 Send/enqueue 路径，可以顺手加锁，但不要借机重构。`client_fix_test.go` 仅当需要补一条「Close 后 Connect 读 RemoteAddr 不 panic」时才动。
6. 不做 git commit / tag / push。

## 验证（必须）

```bash
go test -race . -run TestClientSession_HandleMessage_Connect_ConcurrentWithClose
go test -race -count=1 . ./pkg/websocket ./pkg/grpcstream ./pkg/quicstream
go test -count=1 ./...
```

三条全绿、无 DATA RACE 才算修完。同步点继续用 Eventually / done channel，禁止新增长 Sleep。

## 完成报告

- 改了哪些行（贴 before/after）
- 上面三条测试命令与结果
- 是否还扫到其它无锁 `attachment` 读（有则列出处理）
- 偏离（应无）
````
