# 修复任务 06：Topics 匹配与协议层

## 背景

你要修复的项目是 **MessageLoop**（工作目录：`D:/Codes/qiulin/messageloop`），Go 实时消息平台服务端。先读根目录 `AGENTS.md`。

刚完成一轮全项目代码评审，评审发现已经过主 agent 逐条核实（位图两个 bug 均有探针复现）。完整方案见 `docs/review/fix-plan.md`。**先读相关代码再动手。**

## 文件归属（严格，多 agent 并行修复）

- 你拥有：`pkg/topics/` 全部、`protocol/` 全部 `.proto`、`shared/`（含 `shared/marshaler.go`）、`buf.yaml`、`buf.gen.yaml`、`shared/genproto/`（重新生成产物）。
- 禁止修改：根包、`proxy/`、`pkg/websocket/`、`pkg/grpcstream/`、`pkg/redisbroker/`、`config/`、`cmd/`、`sdks/`、`docs/`。
- 注意：`shared/` 是独立 Go module，测试要在 `shared/` 目录内跑。

## 任务清单

### P1（必修）

1. **optimized bitmap `Unsubscribe` 残留位误投递**（`pkg/topics/optimized_inverted_bitmap.go:110-124`）：订阅 `"a"`（maxConstituents=3）会在 `bitmaps[1][""]`、`bitmaps[2][""]` 填充 pos；`Unsubscribe` 只清理实际 constituent（`i == len(constituents)` 时 break），尾部 `empty` 位残留。pos 回收给更长订阅后产生误匹配（已复现：`Subscribe("a")→Unsubscribe→Subscribe("b.c.d")→Lookup("b.c")` 误中）。修复：清理循环扩展到 `maxConstituents`，`i < len(constituents)` 清 `bitmaps[i][constituents[i]]`，`i >= len(constituents)` 清 `bitmaps[i][empty]`。**先写复现序列的回归测试（当前会红）再修**。
2. **位图 matcher 重复 `Unsubscribe` pos 别名**（`pkg/topics/inverted_bitmap.go:69-77`、`optimized_inverted_bitmap.go:110-124`）：`Unsubscribe` 无条件 append `deletedPositions`，同一 Subscription 卸载两次 → 同一 pos 入队两次 → 后续两个订阅共享 ID、互相覆盖（已复现）。修复：回收前检查 `subscribers[sub.ID]` 存在（且确为该 subscriber）再回收；同时在 `Matcher` 接口（`pkg/topics/matcher.go`）文档中约定 Unsubscribe 幂等性要求。
3. **空分段/空 topic 语义统一**（`optimized_inverted_bitmap.go:73-80,132-138`）：optimized 拒绝 `ErrBadTopic`，而 naive/trie/cs-trie/inverted_bitmap 都接受含空分段的 topic——注释声称"为一致而拒绝"但事实相反，`TestOptimizedInvertedBitmapMatcherRejectsEmptySegments` 固化了不一致。修复方向（推荐）：五种实现统一**拒绝**显式空分段与空 topic（含 trie/cs-trie 的 `""` 分支路径），更新对应测试与注释；如评估后认为工作量/兼容性风险大，可反方向统一为接受，但必须在报告中论证选择。
4. **重复订阅语义文档化**（`inverted_bitmap.go:30-67` 等）：位图是多重订阅（每次分配新 pos），其余三种按 Subscriber 幂等；`Subscription.ID` 仅位图使用。在 `Matcher` 接口与 `Subscription` 的文档注释中明确语义差异（不要求改行为统一）。
5. **4 个 `.proto` 的 `go_package` 修正**（`protocol/client/v1/service.proto:8`、`server/v1/api.proto:10`、`proxy/v1/proxy.proto:9`、`event/v1/events.proto:6`）：均为 `.../messageloop/genproto/...`，实际生成路径是 `shared/genproto/...`（`buf.gen.yaml:5`）。当前靠 `paths=source_relative` 碰巧能工作，未来跨 proto import 必然编译失败。改为 `github.com/messageloopio/messageloop/shared/genproto/<pkg>/v1;<alias>`，然后 `task generate-protocol`（或等效 buf 命令）重新生成，确认 `go build ./...` 与 `cd shared && go build ./...` 通过、全仓导入路径无变化需求。

### P2（顺手修）

6. cs-trie CAS 失败重试改有界循环（`cstrie.go:168,231,302`）：递归改循环 + 上限（如 1000 次后 `runtime.Gosched()`），消除理论栈溢出风险。
7. `cleanParent` 重试参数错位（`cstrie.go:421-423`）：重试参数旋转后恒 no-op——保持原参数序重试，或删除该无效重试（惰性清理已兜底），注释说明。
8. `shared/marshaler.go`：① `MarshalTypeError`/`UnmarshalTypeError`（141-152）错误串完全相同——包含 `%T` 与操作名；② `ProtoJSONMarshaler.Name()`（126-128）改 `"protojson"` 并加入 `Marshalers` 列表（131-134）。
9. `naive.go`：`Unsubscribe` 的 for-range-continue 改直接 `delete` + 清理空 topic map（30-43）；`topicMatches`（70-87）与 `inverted_bitmap.go:103-119` 的 `matchCriteria` 抽共享函数。
10. `Subscriber` 接口约束（`pkg/topics/matcher.go:10`）：文档注明必须 comparable，或 `Subscribe` 内 `reflect.TypeOf(sub).Comparable()` 校验并返回错误（五实现统一）；`optimized_inverted_bitmap.go:111` 的 `Unsubscribe(nil)` 加防护。
11. `protocol/client/v1/service.proto` 的显式 `json_name` 注解（`UseProtoNames:true` 下是死代码）：删除冗余注解，保持与 server/proxy proto 风格一致；删除后重新生成确认 wire 行为不变。

## 测试要求

- 修复前跑 `go test ./pkg/topics/... && cd shared && go build ./...` 确认基线。
- 回归测试：任务 1（stale-empty 复现序列）、2（重复卸载后 ID 唯一、无覆盖）、3（五实现对 `""`/`"a."`/`.a`/`"a..b"` 行为一致的差分测试，以 naive 为基准）。
- 补盲：cs-trie/trie/naive 的并发正确性断言（复用 `utils_test.go` 的 `testMatcherConcurrentSubscribe`，当前仅位图有）；`shared/marshaler` 单测（该 module 零测试文件：三实现 × Marshal/MarshalAppend/Unmarshal/Name + 错误可区分性 + Name 唯一性）。
- 完成后 `go test -race ./pkg/topics/...` 全绿；`cd shared && go test ./...` 全绿。

## 纪律

- 不做 git commit/push。最小改动。
- `protocol/` 与 `shared/genproto/` 变更需在报告中明确列出（其他 agent 依赖这些产物）。
- 完成后返回报告：每条任务处置、改动文件清单、测试结果、行为变更（空分段语义方向选择及论证）、遗留问题。
