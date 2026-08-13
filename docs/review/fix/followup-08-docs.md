# 继续修复：工作流 08 文档（第 2 轮）

背景：你此前完成了 MessageLoop（`D:/Codes/qiulin/messageloop`）文档批次的修复。深度验收结论：P3 的 17 项中 15 项正确落地，但有文档与代码的新一轮漂移需补。范围与上次相同（`docs/`、`README.md`、`AGENTS.md`、`CLAUDE.md`、`.gitattributes`），禁止 git 写操作。

## 必修

1. **补 3513 断连码**（服务端核心 agent 本轮新增 `DisconnectInternal{Code:3513}`，用于 connect 路径内部错误强制断连，`disconnect.go:109-112`）：
   - `docs/protocol.md:338-356` 断连码表补 3513 行（语义：connect 路径内部错误，连接被强制关闭；触发点 `client.go:775-783` 的 `disconnectOnConnectError`）。
   - `docs/developer/05-observability.md:147-163` 断连码表同步补 3513，"是否触发"标"是"。
   - `AGENTS.md` 与 `docs/developer/06-development.md:170` 中断连码区间 "3000-3512" 的描述更新为含 3513（如 "3000-3513"）。
2. **可观测性文档同步**（配置 agent 本轮已注册 Go/Process collector 并加 node_id label）：
   - `docs/developer/05-observability.md:62` 与 `:190`：删除/改写"registry 不含 Go runtime、process 指标"的表述（`cmd/server/main.go:44-47` 已注册）。
   - `:191` 附近"全部指标均无标签"改为"cluster 启用且配置 node_id 时，指标带 `node_id` 标签；其余指标无标签"。
3. **Validate 规则文档同步**：`docs/developer/02-configuration.md:19` 附近"至少一个传输地址"改为与实际校验一致（`config/config.go:162-170`：WebSocket addr/path 与 gRPC client addr 均必填），并检查该节 Validate 规则清单与 `config.go` 逐条对齐（含 `consumer_group`/`stream_approximate:false` 会被拒绝两条——配置 agent 本轮新增，错误提示见其 Validate 代码）。

## 建议修

4. **`.gitattributes` 完善**：当前仅 `*.go text eol=crlf`，注释声称"除 .go 外其他文件保持 byte-identical"但并未锁定（其他文件仍受用户全局 `core.autocrlf` 影响）。补一条顶层规则（如 `* -text` 或明确列出需锁定的类型），使注释属实；改动后说明对现有工作区的影响（应无翻动）。
5. 检查 `config-example.yaml` 中 `stream_approximate` 的注释是否说明"false 将被 Validate 拒绝"（配置 agent 交接项）；若 yaml 注释归你口径可改，否则在报告中注明。

## 验收标准

- 逐条核对改后表述与引用代码行一致（行号可有小幅漂移，以语义为准）。
- 返回报告：每条处置、改动文件、遗留问题。
