# PR-KA-D2 实现规格：握手版本门（Connect.version 世代校验）

| 字段 | 值 |
| --- | --- |
| 标题 | `server: gate Connect on protocol generation; SDKs default Version 2.0.0` |
| 状态 | **Accepted**（2026-08-17 主 agent 终验通过，尚未 commit） |
| 依赖 | D1 已合（`11bfa04`）。在 `v2` 分支上做 |
| 设计来源 | 转正评审协议/SDK 路结论（版本门未实现且危险）；[kernel-architecture.md](../kernel-architecture.md) KD-K31 |
| 验收人 | 主 agent |

## 1. 目标

v1↔v2 信封字段号已重排（如 outbound 9 在 v1 是 `rpc_reply`、v2 是 `recover_complete`），但服务端**从不读** `Connect.version`（`client.go` `handleConnect` 无校验），旧客户端连上来会被静默错解。本 PR 加版本门：

1. `handleConnect` 在通过 closed/authenticated 检查之后、**staging session ID 之前**校验 `connect.version`：解析主世代号（首个 `.` 前的十进制整数），**只接受世代 2**（`"2"`、`"2.0.0"`、`"2.1.3"` 均合法）。
2. 拒绝路径（空串、非数字、`"1.0.0"`、`"3.0.0"` 等）：先回 `OutboundMessage Error{ code: "VERSION_UNSUPPORTED", type: "version_error", message }`，再以新增的 `DisconnectUnsupportedVersion`（code **3514**,reason `unsupported version`）断开。fail-closed:v1 客户端字段号错位导致 version 读不出来时同样落空被拒——这正是目的。
3. 双 SDK 默认 `Version` 从 `"1.0.0"` 改为 `"2.0.0"`。
4. 文档同步：`docs/protocol.md` 的 Connect 节写明版本门语义与新错误码/断开码；`protocol/shared/v2/errors.proto` 的 well-known 码注释清单补 `VERSION_UNSUPPORTED` 并重新生成（buf 已验证可用）；`AGENTS.md` 的断开码区间表述 3000-3513 → 3000-3514。

**不做：** WS 子协议加版本（`messageloop+proto` 保持不变——消息级版本门对 WS/gRPC/QUIC 三个传输一致生效，子协议改名会破坏既有代理/基础设施，评审 NOTE 留待转正后单独决策）;caps 协商语义（`accepted_caps` 维持回显）;`Connected` 加字段；任何恢复/历史/fencing 行为；错误码收口（ACL_DENIED 等是另一条 backlog，不在本 PR)。

## 2. 允许改动的文件

- `client.go`：仅 `handleConnect` 内加版本门（位置见 §3.1)
- `version.go`（**新增**，根包）：世代常量与解析 helper（如 `const protocolGeneration = 2`、`func protocolGenerationOK(version string) bool`)；也可并入 `client.go`，二选一
- `disconnect.go`：新增 `DisconnectUnsupportedVersion`(3514)，其余不动
- `protocol/shared/v2/errors.proto`：仅注释清单补 `VERSION_UNSUPPORTED`；改后跑 `task generate-protocol`(buf 1.65.0 本机可用），**只保留** `shared/genproto/shared/v2/types.pb.go` 的注释级变化；swagger json 若出现纯排序 churn 一律 `git checkout --` 还原
- 测试：所有构造 `clientpb.Connect{...}` 字面量的 `*_test.go`（现状约 109 处、15 个文件，分布见 §3.3)，以及新增的版本门测试
- `sdks/go/options.go`（默认值）、`sdks/ts/src/client/options.ts`、`sdks/ts/src/client/client.ts`、`sdks/ts/test/client.test.ts`（默认版本断言）
- `sdks/go/MIGRATION_GUIDE.md`：加一行默认版本说明
- `docs/protocol.md`:Connect 节 + 错误码/断开码相关表
- `AGENTS.md`：断开码区间一句
- `docs/v2/tasks/pr-ka-d2-version-gate.md`(§8 实现备注）

禁止：改 proto 字段号/消息形状（只允许 errors.proto 注释）；改 `hub.go`/`session.go`/broker/cluster/sim/HMAC；改 SDK 消费路径；改 `_examples/`;git commit / tag / push。

## 3. 现状（动手前再读）

### 3.1 版本门插入点

`client.go:242` 起 `func (c *Session) handleConnect(...)`:closed 检查 → authenticated 检查 → :259 `originalSessionID := c.session` 开始 staging。**版本门插在 authenticated 检查之后、`originalSessionID` 之前**。拒绝时先 `c.Send` 一条 Error（照 :273-283 AUTH_REQUIRED 分支的 `MakeOutboundMessage` 同款写法），再 `return DisconnectUnsupportedVersion`。被拒连接不得 staging 客户端给的 session ID、不得触发任何认证/takeover 路径。

### 3.2 断开码与错误码

- `disconnect.go:34-113`：内置码 3500-3513，新增 3514 放 `DisconnectInternal` 之后，带 doc 注释（客户端**不应**原地重连，升级 SDK 后再连）。
- `protocol/shared/v2/errors.proto:11-14`:well-known 字符串码注释清单（14 个），补 `VERSION_UNSUPPORTED` 进注释。strings 不是 enum，无 wire 变化。
- 既有 AUTH_REQUIRED 分支（`client.go:273-296`）是「先发 Error 再断开」的现成范式，照抄。

### 3.3 需要补 version 的测试字面量（约 109 处）

`grep -rn "clientpb.Connect{" --include="*.go" .` 分布：根包 `client_fix_test.go`(41)、`client_test.go`(17)、`recover_test.go`(19)、`survey_test.go`(9)、`channel_policy_test.go`(2)、`presence_test.go`(2)、`cluster_*_test.go`/`node_test.go`(9);`pkg/grpcstream`(5)、`pkg/quicstream`(2)、`pkg/websocket`(1)。规则：每个字面量显式加合法 v2 版本（根包建议在 `testhelpers_test.go` 定义 `const testProtocolVersion = "2.0.0"` 统一引用；其他包用 `"2.0.0"` 字面量）。`sdks/go/client.go` 的 3 处 Connect 构造走 `c.opts.Version`，随默认值自动升。

### 3.4 SDK 默认值现状

- `sdks/go/options.go:97` `Version: "1.0.0"` → `"2.0.0"`
- `sdks/ts/src/client/options.ts:53` 与 `sdks/ts/src/client/client.ts:1470` `version: "1.0.0"` → `"2.0.0"`
- `sdks/ts/test/client.test.ts:162` 断言 `"1.0.0"` → `"2.0.0"`

### 3.5 文档现状

- `docs/protocol.md` Connect 节（:96 起）无版本门描述；文档已有错误码/断开码相关章节，补 `VERSION_UNSUPPORTED` 与 3514。
- `AGENTS.md` Error Handling 节：「codes (3000-3513 range)」→ 3000-3514。

## 4. 测试

新增（根包，命名自定）:

| 测试 | 内容 |
| --- | --- |
| 版本门拒绝表驱动 | version ∈ `""` / `"1.0.0"` / `"3.0.0"` / `"abc"` / `"2x"`：断言收到 Error `VERSION_UNSUPPORTED` + `type=version_error`，随后断开码 3514；断言 session 未 staging（未认证、未占用客户端给的 session ID) |
| 版本门放行表驱动 | version ∈ `"2"` / `"2.0.0"` / `"2.1.3"`：正常走完连接（可用既有 helper 断言 Connected) |
| 既有全部测试 | 补 version 后必须全绿，不得靠删断言蒙混 |

## 5. 验证

```bash
go build ./...
go test -count=1 . ./pkg/websocket ./pkg/grpcstream ./pkg/quicstream
go test -count=1 ./...        # 串行；真实 Redis(127.0.0.1:6379）在跑
cd sdks/go && go test -count=1 ./...
cd sdks/ts && npx jest
grep -rn '"1.0.0"' sdks/ --include='*.go' --include='*.ts'   # 零命中（node_modules/dist 除外）
grep -n "VERSION_UNSUPPORTED" protocol/shared/v2/errors.proto docs/protocol.md
grep -n "3514" disconnect.go docs/protocol.md AGENTS.md
git diff --stat -- protocol/ shared/genproto/    # 只允许 errors.proto 注释 + types.pb.go 注释
```

## 6. 验收清单

1. 版本门位置正确（staging 之前）、fail-closed；拒绝路径先 Error 后断开，Error 码/型、断开码 3514 符合 §1.2。
2. 世代解析规则与 §1.1 一致（只认 major==2)；新增测试覆盖 §4 表且全绿。
3. 约 109 处既有测试字面量全部显式带 v2 版本；没有为绕过门而删掉的既有断言。
4. 双 SDK 默认 `"2.0.0"`，相关测试断言同步；grep 门禁零 `"1.0.0"`。
5. `DisconnectUnsupportedVersion` 有 doc 注释；errors.proto 注释清单含 `VERSION_UNSUPPORTED` 且生成物同步（swagger churn 已还原）。
6. `docs/protocol.md` 与 `AGENTS.md` 表述更新；`docs/v2/README.md` 与增量表由主 agent 负责。
7. 未改 §2 禁止项；无格式 churn(`git diff --numstat` 对照 `--ignore-all-space --ignore-cr-at-eol`);无 git 操作。

## 7. 完成报告

- 改动文件列表（按生产/测试/SDK/文档分组）
- §6 每条 过/失败 + 证据
- §5 测试命令与真实输出
- 偏离（应无；buf 重新生成若受阻须在此说明）

## 8. 实现备注（实现方填）

- 版本门实现：`version.go`（新增）放 `const protocolGeneration = 2` 与 `protocolGenerationOK(version string) bool`（`strings.Cut` 取首段 + `strconv.Atoi`，fail-closed）；`client.go` `handleConnect` 在 authenticated 检查之后、`originalSessionID` staging 之前插入门，拒绝路径照 AUTH_REQUIRED 范式先 `Send` `Error{VERSION_UNSUPPORTED, version_error}` 再 `return DisconnectUnsupportedVersion`。
- 断开码：`disconnect.go` 在 `DisconnectInternal` 之后新增 `DisconnectUnsupportedVersion`（3514，`unsupported version`），doc 注释明确客户端不应原地重连。
- 测试字面量：根包内部测试（`package messageloop`，9 个文件）统一引用 `testhelpers_test.go` 新增的 `const testProtocolVersion = "2.0.0"`；外部测试包（`cluster_redis_integration_test.go`、`cluster_v1_e2e_test.go`）与 `pkg/grpcstream`、`pkg/quicstream`、`pkg/websocket` 用 `"2.0.0"` 字面量。除 §3.3 列出的 `clientpb.Connect{` 字面量外，`pkg/websocket` 的 `integration_test.go`/`e2e_test.go` 还有 11 处原始 JSON `connect` 帧（不经 proto 字面量），同样补了 `"version": "2.0.0"`——这是首轮测试红掉后补的，规格书分布清单未覆盖这一类。
- 新增测试 `version_test.go`：`TestVersionGate_RejectsUnsupportedVersions`（`""`/`"1.0.0"`/`"3.0.0"`/`"abc"`/`"2x"`，断言 Error 码/型、3514 关闭、未认证、session ID 未 staging、无 Connected 帧）、`TestVersionGate_AcceptsGeneration2`（`"2"`/`"2.0.0"`/`"2.1.3"`）、`TestProtocolGenerationOK`（解析规则单测）。
- buf 生成物取舍：`task generate-protocol`（buf 本机可用，远程插件可达）后，well-known 码注释实际生成到 `shared/genproto/shared/v2/errors.pb.go`（不是规格书预期的 `types.pb.go`——Error 消息定义在 errors.proto，buf 按 proto 文件出 `.pb.go`）以及 `sdks/ts/src/proto/shared/v2/errors_pb.ts`、两个 v2 swagger json 的 description 字段，均为同一注释的同步，已保留；`client/v1`、`proxy/v1`、`server/v1` 三个 swagger json 为纯排序 churn，已 `git checkout --` 还原；`client/v2/service.pb.go`、`shared/v2/types.pb.go` 为无内容 diff 的行尾噪声，同样还原。
- 行尾约定：仓库 `.gitattributes` 为 `*.go text eol=crlf`（工作区 CRLF），批量 sed 改动后已把受触的 .go 文件转回 CRLF，并修复了 3 个原本无 EOF 换行的文件（`node_test.go`、`cluster_resume_test.go`、`testhelpers_test.go`）被误加的末尾 `\r`；`sdks/ts/test/client.test.ts` 仓库内为 LF（`-text`），已保持 LF。终验 `git diff --numstat` 与 `--ignore-all-space --ignore-cr-at-eol` 输出完全一致。
- SDK：双 SDK 默认 Version 升 `"2.0.0"`；`sdks/go/client.go` 三处 Connect 构造走 `c.opts.Version` 自动生效，未动。已知遗留：`sdks/ts/README.md:75` 的默认值表仍写 `"1.0.0"`，不在 §2 允许改动清单内，未改，建议主 agent 后续跟进。
