# 继续修复：工作流 07 配置与启动（第 2 轮）

背景：你此前完成了 MessageLoop（`D:/Codes/qiulin/messageloop`）配置/启动层的修复。深度验收结论：全部条目正确落地、放行。仅 1 项建议修。范围与上次相同，禁止 git 写操作。

## 建议修

1. **行尾统一**：`config/config_test.go` 与 `cmd/server/config_consistency_test.go` 工作区为 LF，与仓库统一的 CRLF 不一致（提交后下次 Git 触碰会产生行尾翻动 diff）。转换为 CRLF（`unix2dos` 或编辑器，勿改动内容）。

另：`stream_approximate: false` 改为 Validate 拒绝已被验收裁定为"可接受的破坏性变更"，无需改代码——但请确认 `config-example.yaml` 及文档对该字段的注释已说明"false 将被拒绝"（若文档 agent 未覆盖，在报告中交接，不要改 docs/）。

## 验收标准

- 转换后 `go test ./config/... ./cmd/...` 全绿；`git diff --stat` 中这两个文件不应出现内容性变更。
- 返回报告：处置、交接项。
