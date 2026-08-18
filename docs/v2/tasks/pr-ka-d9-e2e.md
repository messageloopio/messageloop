# PR-KA-D9 实现规格：双进程黑盒 e2e（真服务器 × 真 SDK）

| 字段 | 值 |
| --- | --- |
| 标题 | `e2e: black-box smoke against a spawned server process via the Go SDK` |
| 状态 | **Ready**（待实现） |
| 依赖 | D8 已合（`326307f`,CI 有 Redis service、sdks/go 测试已进 CI)。在 `v2` 分支上做 |
| 设计来源 | 转正评审 backlog D9：现有 e2e 全是同进程（`pkg/websocket/e2e_test.go` 用 in-process node)，从未验证过 `cmd/server` 真实启动接线 + 真实 SDK 过真实网络 |
| 验收人 | 主 agent |

## 1. 目标

新增一个**双进程**黑盒冒烟 e2e：测试进程 `go build` 出真实 `cmd/server` 二进制、以子进程拉起（独立配置、独立端口），用**真实 Go SDK**(`sdks/go`）过真实 socket 跑核心流程。这是对 D2–D8 所有合同的端到端背书：配置加载、监听器接线、版本门、WS/gRPC 双传输、历史/恢复、admin API——任何一环接错都会在这里炸。

放在 `sdks/go` 模块（它只依赖 `shared`，无 import 环；D8 的 `Test Go SDK module` step 会**自动**把它带进 CI，无需动 ci.yml)。

**场景（一个测试文件，内存 broker 为默认路径）:**

1. **起服**：测试从 `sdks/go` 定位仓根（`../..`),`go build -o <tmp>/server ./cmd/server`(`cmd.Dir` = 仓根；Windows 注意 `.exe`)；生成临时配置（空闲端口用 `net.Listen(":0")` 抢到后释放再写进配置）；子进程启动；轮询 `http://<http addr>/health` 就绪（超时 15s 失败即报）。
2. **WS 全流程**:SDK 建 WS 客户端 → connect(`version` 由 SDK 默认带 "2.0.0"，顺带背书 D2 版本门）→ subscribe `e2e.chat` → 第二个 SDK 客户端（或 admin Publish）发消息 → 断言收到且 payload 逐字节一致。
3. **历史/恢复**：发布 2 条带 `add_history` 的消息后，新客户端 `SubscribeWith(..., WithFresh())` 或等效恢复订阅 → 断言按序回放。
4. **gRPC 流传输**:SDK 走 gRPC transport 重复一遍 connect/subscribe/publish 收消息（证明第二传输接线）。
5. **admin gRPC 冒烟**：带 `authorization: Bearer <token>` 调 `messageloop.server.v2.APIService/GetChannels` 与 `GetPresence`(genproto 在 `shared` 模块，sdks/go 已依赖）→ 断言频道在列、至少一个在线会话。
6. **Redis 变体（可选但 CI 必跑）**：参考 `requireCommandBusRedis` 的 env 探测（`MESSAGELOOP_TEST_REDIS_ADDR`，默认 `127.0.0.1:6379`,ping 不通则 skip）再起一份 `broker.type: redis` 的配置，重跑场景 2（证明 Stream/PubSub 真实路径）。CI 的 build-and-test job 有 redis service，会真跑。

**硬要求**：总耗时 < 60s；全部同步用轮询/就绪信号，禁止固定长 Sleep；子进程必须 `t.Cleanup` 杀掉（即使断言失败）；端口零硬编码冲突（抢占式分配）。

**不做：** QUIC 传输（TLS 证书装配超冒烟范围）；chatroom e2e 进 CI（另一条）；性能/压测；改任何服务端源码——**冒烟若炸出真实 bug，停下来报告主 agent，不许顺手修**;ci.yml 改动（覆盖自动生效）;git commit / tag / push。

## 2. 允许改动的文件

- `sdks/go/e2e_process_test.go`（新增；如确需拆分可加一个 `e2e_process_helpers_test.go`)
- `docs/developer/06-development.md`（测试一节加两行说明本 e2e 的存在与运行方式）
- `docs/v2/tasks/pr-ka-d9-e2e.md`(§8 实现备注）

禁止：服务端/根包任何源码；SDK 生产代码（`sdks/go/*.go` 非测试文件）;ci.yml;proto/生成物。

## 3. 现状（动手前再核对）

- 服务器入口 `cmd/server`(flag 走 pflag,`--config` 指定配置文件；`main.go:124`)。配置键形见 `configs/test.yaml`:`server.http.addr`(health 挂 `/health`,`main.go:353`)/`server.grpc_admin.addr`(+`auth_token`)/`transport.websocket.addr`+`path`/`transport.grpc.addr`/`broker.type`(memory|redis)+`broker.redis.*`。
- SDK 客户端 API:`sdks/go/client.go`（连接选项、`SubscribeWith`、`WithFresh`/`WithRecover` 见 :972 一带；WS 与 gRPC transport 的拨号方式读 SDK 现码，不要想当然）。
- admin 合同:`docs/developer/03-admin-api.md`(D6 后已是 server/v2；认证头 `authorization: Bearer <token>`)。
- 版本门：D2 起 Connect 只认世代 2,SDK 默认 `"2.0.0"`——e2e 里不用显式传。
- 内存 broker 支持 History/恢复（C 系列全程用内存 broker 测的）。
- CI 接线事实：D8 后 `.github/workflows/ci.yml` 的 `build-and-test` job 挂 `redis:7`(127.0.0.1:6379 无密码）且含 `Test Go SDK module` step(`working-directory: sdks/go`,`go test ./...`)——新测试落在这个模块即自动进 CI，且 Redis 变体在 CI 真跑、本地无 Redis 时 skip。
- Windows 本地 + Linux CI 双平台：二进制名、路径分隔、`go build` 的 `cmd.Dir` 都要处理。

## 4. 测试

| 测试 | 内容 |
| --- | --- |
| e2e 本体 | 上述 6 场景全绿（内存路径必跑，Redis 变体有 Redis 则跑） |
| 负向自检 | 故意把配置里的 admin token 改错一次（本地手动，不留码），确认 admin 冒烟真的会失败——证明断言不是空转 |
| 回归 | `cd sdks/go && go test -count=1 .` 全绿；根仓 `./...` 不受影响（新文件只在 sdks/go) |

## 5. 验证

```bash
cd sdks/go && go test -count=1 -v -run TestE2EProcess .   # 贴真实输出；含 Redis 变体的 PASS（非 SKIP)
cd sdks/go && go test -count=1 .                          # 整模块绿
go build ./...                                            # 仓根不受影响
# 重复运行稳定性
cd sdks/go && go test -count=3 -run TestE2EProcess .      # 3 连过
```

## 6. 验收清单

1. 双进程真实成立：服务器是 `go build` 出的 `cmd/server` 子进程，客户端是 SDK 过 socket（报告须贴出子进程 PID/端口证据或等价日志）。
2. 六场景齐：WS 全流程、历史回放、gRPC 传输、admin GetChannels/GetPresence、Redis 变体（本地可 skip,CI 真跑）。
3. 无固定长 Sleep；子进程清理可靠（`ps` 无残留）；端口不硬编码。
4. §5 全绿（含 3 连跑）；仓根测试不受影响。
5. 未碰 §2 禁止项（特别：服务端源码零改动；若冒烟炸出 bug 已按 §1 停手报告）;`git diff --numstat` 与 `--ignore-all-space --ignore-cr-at-eol` 一致；无 git 操作。

## 7. 完成报告

- 改动文件列表（新增/删除/修改分组）
- §6 每条 过/失败 + 证据
- 测试命令与结果（真实输出）
- 偏离（应无）

## 8. 实现备注（实现方填）

（留空）
