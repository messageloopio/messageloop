# PR-KA-D8 实现规格：CI 修复（v2 分支 + Redis + 子模块 + 工具链固定）

| 字段 | 值 |
| --- | --- |
| 标题 | `ci: cover the v2 branch, real Redis, submodules and TS SDK` |
| 状态 | **Accepted**（2026-08-18 主 agent 终验通过，尚未 commit；push 后 `gh run watch` 实跑计入验收收尾） |
| 依赖 | D7 已合（`2635cf1`)。在 `v2` 分支上做 |
| 设计来源 | 转正评审 backlog D8；D7 终验残留（swagger 旧码表，根因是 buf 工具链未固定） |
| 验收人 | 主 agent |

## 1. 目标

CI 反映 v2 的真实验证面。现状（`.github/workflows/ci.yml`）：只监听 `main`、无 Redis（集成测试全部 skip、空心绿）、根 `go test ./...` 够不到独立模块（`shared/`、`sdks/go/`、`_examples/chatroom/`)、TS SDK 零覆盖、buf 版本 CI(v1.63.0）与本机（1.65.0）不一致导致生成物不可复现。

1. **触发面**:`push` 与 `pull_request` 分支加 `v2`（保留 `main`)。
2. **Redis service**:build-test job 挂 `redis:7` service（映射 `127.0.0.1:6379`，无密码）。测试 helper(`pkg/redisbroker/cluster_command_bus_test.go:209` `requireCommandBusRedis`、根包 `cluster_redis_integration_test.go`）默认打 `127.0.0.1:6379`、空密码、DB 14，与 service 默认形态直接吻合；Redis 缺席时测试 skip，所以加 service 后 CI 必须能在日志里看到集成测试真跑（非 SKIP)。
3. **子模块 job**:setup-go 后 `cd shared && go test ./...`、`cd sdks/go && go test ./...`、`cd _examples/chatroom && go build ./...`(chatroom e2e 需真实服务器，CI 只 build)。
4. **TS job**:`actions/setup-node`（版本见 §3.3)+ `npm ci` + `npm run build` + `npx jest`（在 `sdks/ts`)。
5. **buf 工具链固定（顺带收 D7 残留）**：选定一个版本同时写进 CI 与 `Taskfile.yml` 的 `init`。决策规则：先用 CI 现钉的 **v1.63.0** 在本机跑 `buf generate`(`go run github.com/bufbuild/buf/cmd/buf@v1.63.0 generate` 即可，不必 go install)，对当前工作树零 diff（D7 注释同步后的 `errors.*` churn 除外——那是正常产物）→ 钉 v1.63.0；若有 diff，改用本机 **v1.65.0** 全量重新生成并把全部刷新产物纳入本 PR（三个 swagger 的内嵌码表会随之一并刷新为 19 码定稿，D7 残留关闭），CI 与 Taskfile 钉 v1.65.0。**验收时 CI 的 `buf generate && git diff --exit-code` 必须以所钉版本零 diff 通过。**
6. lint job 保持不动。

**不做：** 改任何 Go 源码 / proto / 测试；release 流水线（ldflags version 是另一条 backlog)；npm publish / docker 发布；branch protection 规则（那是 GitHub 设置，不在仓库文件里）;git commit / tag / push。

## 2. 允许改动的文件

- `.github/workflows/ci.yml`
- `Taskfile.yml`（仅 buf 版本钉）
- 仅当 §1.5 走了「重新生成」分支：`shared/genproto/**`、`sdks/ts/src/proto/**` 的全量刷新
- `docs/developer/06-development.md`（如涉及 CI/工具链描述）
- `docs/v2/tasks/pr-ka-d8-ci.md`(§8 实现备注）

禁止：Go 源码、proto 文件、测试文件、SDK 手写代码。

## 3. 现状（动手前再核对）

### 3.1 ci.yml 现状

单 workflow 两 job:`build-and-test`(checkout → setup-go(go-version-file: go.mod)→ build → vet → `go install buf@v1.63.0` + `buf generate` + `git diff --exit-code` → `go test -race -coverprofile ./...` → PR 时传 coverage）与 `lint`(golangci-lint-action@v6,version v2.12.2)。触发仅 `main`。

### 3.2 模块与 Go 版本

根 `go 1.26.5`;`shared`、`sdks/go` 为 `go 1.25.5`;`_examples/chatroom` 为 `go 1.26.5`。setup-go 用根 go.mod(1.26.5）可覆盖全部子模块。`sdks/ts` 有 `package-lock.json`（用 `npm ci`)。

### 3.3 本机工具链

node v24.11.1、buf 1.65.0;CI 钉 buf v1.63.0。`buf.gen.yaml` 插件全部 remote + 固定版本（go:v1.36.10、gateway:v2.27.4、grpc:v1.6.0、openapiv2:v2.27.3、es:v2.10.0)。D6/D7 观察到本机 1.65.0 重新生成会产生 swagger 定义重排与 pb.go EOL 漂移——§1.5 的决策规则就是为消除这种「本机与 CI 各说各话」。

### 3.4 Redis 测试形态

helper 无 Redis 时 `t.Skipf`；有 service 时真跑。CI 上可用 `go test -v -run TestClusterCommandBus ./pkg/redisbroker | grep -c SKIP` 之类确认非空心（或看 `-v` 输出 PASS 而非 SKIP)。

### 3.5 CI 无法在本地完整验证

workflow 的正确性最终只能在 GitHub 上证实。本 PR 验收分两段：实现方本地逐条复跑每个 step 的命令（等同 CI 内容）；主 agent 在 push 后用 `gh run watch` 看 v2 上的真实运行，红了就按本规格 fix-forward（属本 PR 范围）。

## 4. 测试

| 测试 | 内容 |
| --- | --- |
| YAML 合法 | workflow 解析通过（actionlint 或等价） |
| 本地复跑 | CI 每条命令在本机逐一执行且绿（含钉版 buf 的 regen 零 diff、带 Redis 的 `go test -race ./...`、子模块、TS) |
| 集成非空心 | 带 Redis 跑时 `go test -v` 输出中集群/Redis 用例为 PASS 而非 SKIP |
| 真实运行 | push 后 `gh run watch` 在 v2 分支全绿（主 agent 执行，计入验收） |

## 5. 验证

```bash
# 逐条本地复跑（顺序执行，不并发根目录 go test）
go build ./... && go vet ./...
go run github.com/bufbuild/buf/cmd/buf@v1.63.0 generate && git status --short   # §1.5 决策点
go test -race ./...                # 真实 Redis 在 127.0.0.1:6379
go test -v -run "TestClusterCommandBus_CreateGroup" ./pkg/redisbroker 2>&1 | grep -E "PASS|SKIP"   # 须 PASS
cd shared && go test ./...
cd sdks/go && go test ./...
cd _examples/chatroom && go build ./...
cd sdks/ts && npm ci && npm run build && npx jest
# YAML 检查
npx --yes actionlint -oneline .github/workflows/ci.yml || python -c "import yaml,sys; yaml.safe_load(open('.github/workflows/ci.yml'))"
```

## 6. 验收清单

1. ci.yml 触发含 `main` + `v2`(push 与 PR)。
2. build-test job 有 `redis:7` service(6379，无密码），集成测试在 CI 语义下不再 skip。
3. 子模块三件套（shared test、sdks/go test、chatroom build）与 TS(npm ci/build/jest）进入 CI。
4. buf 版本在 CI 与 Taskfile 钉同一版本；以该版本 `buf generate` 对工作树零 diff;§1.5 若走重新生成分支，swagger 内嵌码表已是 19 码定稿（grep 验证 `SURVEY_FAILED` 在三个 swagger 中）。
5. 本地复跑全绿；YAML 合法；push 后 GitHub 真实运行绿（主 agent 用 gh 验证）。
6. 未碰 §2 禁止项；无 git 操作。

## 7. 完成报告

- 改动文件列表（新增/删除/修改分组）
- §6 每条 过/失败 + 证据（§1.5 走了哪个分支、为什么）
- 测试命令与结果（真实输出）
- 偏离（应无）

## 8. 实现备注（实现方填）

- **§1.5 决策走了「重新生成」分支**：`go run github.com/bufbuild/buf/cmd/buf@v1.63.0 generate` 对工作树产生真实 diff（`service.swagger.json` 675 行、`api.swagger.json` 213 行、`proxy.swagger.json` 65 行变更；两个 `.pb.go` 为纯 EOL 漂移），非零 diff，故按规则改用本机 **v1.65.0** 全量重生成。两个版本的 swagger 输出 blob 一致（proxy.swagger.json 均得 `1a3a51d`），说明 diff 全部来自 D7 的 swagger 旧码表残留而非版本行为差。钉版定为 **v1.65.0**（CI 与 Taskfile `init` 同步）。
- 重生成产物：三个 swagger 的内嵌码表刷新为 19 码定稿（`SURVEY_FAILED` 已在三个 swagger 中各出现 1 次）；`sdks/ts/src/proto` 无变化；`service.pb.go`/`types.pb.go` 为 LF/CRLF 行尾漂移，内容经 git 规范化后与 HEAD 相同，已 `git checkout --` 还原以守住仓库 CRLF 工作树约定（`.gitattributes`: `*.go text eol=crlf`）。
- 钉版可复现性：本机 `buf generate`（1.65.0）二次运行 sha256 逐字节一致（幂等）；CI 的 `buf generate && git diff --exit-code` 在产物入库后零 diff。
- workflow 结构：`build-and-test` 挂 `redis:7` service（`ports: 6379:6379`，无密码，与 `requireCommandBusRedis` 默认 `127.0.0.1:6379`/空密码/DB14 直接吻合）；子模块三件套以 `working-directory` 分步执行；TS 独立 `ts-sdk` job（setup-node@v4，Node 24.11.1，npm cache 指向 `sdks/ts/package-lock.json`），`npm ci`/`npm run build`/`npx jest` 经 job 级 `defaults.run.working-directory: sdks/ts` 执行；lint job 未动。
- 本地复跑（§5）全部通过，集成测试 `-v` 输出为 `--- PASS` 而非 SKIP（证据见完成报告）。
- 未触碰任何 Go 源码 / proto / 测试 / SDK 手写代码；无 git 写操作。


### 主 agent 终验备注（2026-08-18）

- §1.5 决策亲验：`go run buf@v1.63.0 generate` 对实现方刷新后的三个 swagger 逐字节一致（sha256 校验 OK）——证实 diff 源自 D7 残留而非版本行为差，钉 v1.65.0 结论成立。
- pb.go「修改」幻影查明：`service.pb.go`/`types.pb.go` 在 regen 后 `git status` 显示 M 但 `git diff` 为空（EOL stat 缓存假象，blob 本为 LF）；对 CI 的 `git diff --exit-code` 无影响。
- 三个 swagger 已含 19 码定稿（`SURVEY_FAILED` 各 1 次，`ACL_DENIED` 零命中），D7 残留关闭。
- 主 agent 亲跑全绿：build/vet、全量 `go test -race ./...` 11/11（真实 Redis）、shared/sdks-go 模块测试、TS npm ci/build/jest 83/83、chatroom build、workflow YAML 解析（三 job）。
- 收尾项：push 后 `gh run watch` 验证 v2 上真实运行，红了按 §3.5 fix-forward。
