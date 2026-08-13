# 任务书 01：Topic 校验全入口统一 + matcher 后缀 `**`

## 角色

你是 MessageLoop（Go 实时消息平台，项目根 `D:/Codes/qiulin/messageloop`）的实现工程师。本任务实现 backlog 的 B1 + B2 两项（决策已定，见 `docs/review/backlog.md` 第一节），目标是 topic 校验语义在所有入口、所有 matcher 实现间完全一致。

## 文件归属（只许改这些，其他文件一律不动）

- `pkg/topics/**`（matcher.go、cstrie.go、trie.go、naive.go、inverted_bitmap.go、optimized_inverted_bitmap.go 及各自测试、consistency_test.go）
- `hub.go`（仅订阅/退订入口与校验相关段落）
- `broker.go`、`broker_memory.go`、`pkg/redisbroker/**`（仅发布入口的频道校验）
- `acl.go`、`acl_test.go`（仅补 `**` 语义对齐的测试；ACL 实现本身已支持 `*`/`**`，预期不改或极小改）
- `docs/protocol.md`（topic 通配规则章节，若有）

如确需越出清单，必须在报告中显著标注并给出理由。

## 任务 1（B1）：精确频道全入口拒绝空分段

现状：五个 matcher 的 `Subscribe` 已通过未导出的 `validTopic`（`pkg/topics/matcher.go:74-83`）统一拒绝空分段/空 topic（`ErrBadTopic`，matcher.go:18）。但**精确频道不经过 matcher**——`hub.go:79-84` 的 `addSub` 对非通配频道直接进 `subShards.addSub`，无校验；发布路径（broker Publish）同样无校验。`"a."`、`"..b"` 这类畸形频道在精确订阅/发布入口可以静默通过。

要求：

1. 在 `pkg/topics` 导出校验函数（建议 `ValidateTopic(topic string) error`，内部复用 `validTopic`，返回 `ErrBadTopic`），供根包与 broker 使用。
2. `hub.go` 精确订阅入口（`addSub` 非通配分支）接入校验，返回显式错误；确认 `client.go` 的 `handleSubscribe` 会把该错误以错误信封回给客户端（错误链路已在，禁止吞错或静默失败）。
3. 发布入口接入校验：找到 broker 发布调用链（memory broker 与 redis broker 的 `Publish`，及 `node.go` 的发布封装），对畸形频道返回显式错误。注意发布方可能是 admin API（`pkg/grpcstream/api_handler.go`）——**该文件不在你的归属内**，若 admin 路径的错误传播需要改动 api_handler.go，只在报告中说明，不要改。
4. `Lookup`（matcher 查询侧）对非法 topic 保持"不匹配任何订阅"即可，无需报错。

## 任务 2（B2）：matcher 支持后缀式 `**`（MQTT 风格）

决策：matcher 支持多段通配 `**`，**仅允许位于模式末尾**；ACL 侧不动（`acl.go` 已支持 `*` 单段 / `**` 多段）。语义：`a.**` 匹配 `a`、`a.b`、`a.b.c`（零段或多段）；裸 `**` 匹配一切。

要求：

1. `ValidateTopic`/`validTopic` 同步允许末尾 `**`；拒绝中间位 `**`（如 `a.**.b`）与分段内嵌 `**`（如 `a**b`——注意 `strings.Contains(ch, "*")` 的通配判定在 `hub.go:75-77`，分段内嵌 `*` 的现有语义是字面匹配，保持不变）。
2. 各实现分工：
   - **CSTrie**（cstrie.go）：Lookup 每层向下递归时，额外检查当前节点的 `**` 分支并收集其订阅者，无需回溯。
   - **trie / naive**：扩展 `matchCriteria`（matcher.go:97 起）或各自匹配逻辑支持末尾 `**`。
   - **inverted_bitmap / optimized**：`**` 之前的字面前缀走位图索引，候选命中后按段校验（含 `**` 尾匹配）。注意不要破坏"短主题匹配长主题"的 AND 语义（matcher.go 注释中提到的尾部 padding 机制，`docs/archive/fix-plan.md:287` 有说明，那个机制是必需的，不能动）。
3. ACL 对齐验证：`acl.go` 的 `*`/`**` 语义与 matcher 是否真一致（特别是 `a.**` 是否匹配 `a` 本身、`*/__presence` 这类模式的覆盖）；在 `acl_test.go` 补测试锁定 parity。发现实质不一致时不改 ACL 行为，在报告中列出。
4. 测试：`consistency_test.go` 增加 `**` 用例，五个实现同一组用例全过；hub 精确入口非法频道拒绝测试；发布入口非法频道拒绝测试。

## 测试要求

- 新增回归测试必须对旧代码会红（先写测试确认红，再实现转绿，报告里说明验证方式）。
- 通过：`go test ./pkg/topics/... ./pkg/redisbroker/...` 与 `go test -race .`（根包）。

## 纪律

- 不做任何 git 写操作；改动最小化，不顺手重构；注释/文档与实际行为同步。
- 报告格式：完成项清单（file:line 证据）、行为变更显著标注、测试验证方式与结果、遗留问题（含越权建议）。
