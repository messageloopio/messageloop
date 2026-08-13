# 评审任务 04：Topic 匹配与协议层

## 背景

你要评审的项目是 **MessageLoop**（工作目录：`D:/Codes/qiulin/messageloop`），一个用 Go 编写的实时消息平台服务端，通过 WebSocket 和 gRPC 提供 pub/sub 消息能力。先读根目录 `AGENTS.md` 了解构建命令与代码规范。

你是独立评审 agent，没有任何先前上下文。**只做只读评审，不修改任何代码。**

## 评审范围（Topic 匹配与协议层）

- `pkg/topics/`：topic 订阅/匹配引擎，统一 `Matcher` 接口（`Subscribe/Unsubscribe/Lookup`），支持单级通配符 `*`。5 种实现：`cstrie.go`（无锁 CAS 字典树，Hub 默认使用）、`trie.go`（RWMutex 字典树）、`naive.go`（哈希表全扫描）、`inverted_bitmap.go`、`optimized_inverted_bitmap.go`（roaring 位图）及全部测试/基准。
- `protocol/`：protobuf 定义。`client/v1`（双向流 envelope）、`server/v1`（admin API）、`proxy/v1`（业务代理钩子）、`shared/v1`（Payload/Metadata/Error）、`event/v1`。
- `shared/`：独立 Go module（根 `go.mod` 用 `replace` 指向 `./shared`），buf 生成代码 + `Marshaler`（`shared/marshaler.go`：JSON/Protobuf/ProtoJSON）。
- `buf.yaml`、`buf.gen.yaml`：代码生成配置（Go、gRPC、gateway、OpenAPIv2、TS）。

## 模块职责与关键契约（供定位，需你自行通读验证）

- `Matcher` 接口（`pkg/topics/matcher.go`）：`Subscription` 含 `ID uint32`（仅位图实现使用）；`Subscriber` 为空接口别名。
- cs-trie：lock-free，`unsafe.Pointer` + CAS，iNode/cNode/tNode（tombstone）路径拷贝与压缩，CAS 失败递归重试。
- 其余 matcher：单把 `sync.RWMutex` 粗粒度锁。
- 上层使用方：`hub.go`（`NewCSTrieMatcher`）与 `pkg/redisbroker/redis.go`。
- `Marshaler` 接口（`shared/marshaler.go`）：`Marshal/MarshalAppend/Unmarshal/Name`。

## 评审维度

1. **并发正确性**：cs-trie 的 CAS 逻辑（tombstone、clean/contract/toCompressed 收缩）是否有 ABA、活锁、栈溢出（递归重试）风险；位图实现的锁覆盖。
2. **匹配语义一致性**：5 种实现在重复订阅、空分段、空 topic、通配符边界上的行为是否一致（`throughput_test.go` 以 naive 为参考做采样校验）。
3. **接口设计**：`Subscription.ID` 仅位图使用带来的语义不一致；`Subscriber` 空接口的类型安全。
4. **协议定义质量**：proto 字段编号、兼容性、`go_package` 选项与实际生成目录的一致性、`Error` 无错误码枚举的影响。
5. **Marshaler 正确性**：三种实现的边界行为、错误信息可区分性。
6. **测试与基准质量**：并发正确性断言（而非仅基准）、基准本身是否正确测量目标实现。

## 已知线索（需你独立核实真伪，确认或推翻，并给出证据）

1. `optimized_inverted_bitmap.go` 的 `Unsubscribe` 疑只清理实际 constituent 的位图，未清理尾部 `empty` constituent 的索引，已删除 subscriber 的位疑残留。
2. cs-trie 的 `Subscribe/Unsubscribe/Lookup` 在 CAS 失败时递归自调用，极端竞争下疑有栈溢出风险。
3. cs-trie 对重复订阅返回 `true` 但不更新，而 trie/naive 幂等写入——语义不一致。
4. `naive.go` 的 `Unsubscribe` 用 `for range + continue` 实现删除（可直接 `delete`）；`naive.go` 的 `topicMatches` 与 `inverted_bitmap.go` 的 `matchCriteria` 疑逻辑重复。
5. `naive_test.go` 的多线程基准疑误用 `NewTrieMatcher()`；`Unsubscribe` 基准疑重复卸载同一 ID 导致 inverted bitmap 无限 append `deletedPositions`；`TestThroughput` 疑无断言。
6. 多个 `.proto` 的 `go_package` 写为 `.../genproto/...`，而实际生成目录为 `shared/genproto/...`——核实是否一致、是否影响生成。
7. `shared/marshaler.go` 的 `MarshalTypeError` 与 `UnmarshalTypeError` 疑返回完全相同的错误字符串；`ProtoJSONMarshaler.Name()` 疑返回 `"json"` 与 `JSONMarshaler` 冲突。
8. 测试缺口：cs-trie/trie/naive 无并发正确性断言（仅基准）；空分段/空 topic 行为仅 optimized bitmap 有测试；`Marshaler.MarshalAppend` 无测试。

## 工作流程

1. 先跑 `go build ./...` 和 `go test ./pkg/topics/... ./shared/...` 确认基线（基准可用 `-benchtime=10x` 快速验证不 panic）。
2. 通读范围内代码，逐维度评审。
3. 逐条核实"已知线索"：确认（给出决定性证据）或推翻。
4. 补充你自己发现的新问题。

## 输出格式

用中文输出。先给基线测试结果与总体评价（3-5 句），然后逐条 findings：

```
[级别] Critical / Important / Minor
[位置] path:line
[问题] ...
[证据] 关键代码摘录或推理
[修复建议] ...
[置信度] high / medium / low
```

最后单独一节列出"建议补充的测试"。不要贴大段代码，每条 finding 引用不超过 10 行。
