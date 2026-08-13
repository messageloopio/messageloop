# 任务书 05：HTTP proxy 非 200 错误体改用 protojson

## 角色

你是 MessageLoop（项目根 `D:/Codes/qiulin/messageloop`）的实现工程师。本任务实现 backlog 的 A4（无争议小改动，见 `docs/review/backlog.md` 第二节）。

## 文件归属（只许改这些）

- `proxy/http.go`、`proxy/http_test.go`

其他文件一律不动。

## 任务（A4）：HTTP proxy 非 200 错误体改用 protojson

现状：`proxy/http.go:413-420` 用 `encoding/json` 解析响应体为 `sharedpb.Error`（`notificationErrorResponse`）。proto 消息的 JSON 契约是 protojson（字段名、oneof、数值编码有差异），用 encoding/json 解析存在边界不符。

要求：

1. 改用 `google.golang.org/protobuf/encoding/protojson` 解析错误体（先看根包已有的 protojson 用法保持一致——`shared/marshaler.go` 有现成 marshaler 可参照）。
2. 保持既有回退行为：解析失败时仍回退到裸 body 文本的 `HTTPStatusError`。
3. 在 `proxy/http_test.go` 补测试：protojson 特有编码（如 camelCase 字段名）的错误体能正确解析出 `sharedpb.Error`；回归测试须对旧代码会红（说明验证方式）。

## 测试要求

- 通过：`go build ./... && go test -race ./proxy/...`。

## 纪律

- 不做任何 git 写操作；改动最小化。
- 报告格式：完成项清单（file:line 证据）、行为变更显著标注、测试验证方式与结果、遗留问题。
