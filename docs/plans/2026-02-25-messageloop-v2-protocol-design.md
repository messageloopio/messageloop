# MessageLoop v2 协议重设计方案

## 设计目标

完全重新设计 MessageLoop 双向实时消息流平台协议，解决当前 v1 协议的核心痛点：

1. **性能问题**：消除 CloudEvents 转换开销，减少内存分配和 GC 压力
2. **简化复杂度**：去除 CloudEvents 包装，降低开发者学习曲线
3. **增强灵活性**：支持可扩展的元数据类型，消除 string-only 限制
4. **清晰分层**：传输层与应用层职责明确分离

## 核心设计原则

- **完全推倒重来**：不考虑向后兼容性，创建全新的 v2 协议
- **分层架构**：清晰区分传输层（Frame）和应用层（Message）
- **传输解耦**：协议可运行在任何双向流上（WebSocket/gRPC/TCP/QUIC）
- **双栈编码**：同时支持 JSON 和 Protobuf，保留扩展其他编码的接口
- **At-least-once QoS**：提供可靠的消息送达保证和去重机制
- **服务端控制**：服务端生成 ID 并控制超时策略

## 整体架构

### 三层分层设计

```
┌──────────────────────────────────────────────────┐
│              应用层 (Application)                 │
│  - Connect, Subscribe, Publish, RpcCall, ...    │
│  - 业务逻辑，不关心传输细节                        │
└──────────────────────────────────────────────────┘
                      ↕
┌──────────────────────────────────────────────────┐
│              帧层 (Frame Layer)                   │
│  - frame_id, session_id, timestamp              │
│  - ACK/NACK, 去重, flags                        │
│  - encoding 协商                                 │
└──────────────────────────────────────────────────┘
                      ↕
┌──────────────────────────────────────────────────┐
│            传输层 (Transport)                     │
│  - WebSocket / gRPC Stream / TCP / QUIC         │
│  - 纯字节流，不理解协议语义                        │
└──────────────────────────────────────────────────┘
```

**职责划分**：
- **传输层**：负责可靠的双向字节流传输和连接管理
- **帧层**：负责消息边界、ID 生成、ACK、去重、编码协商
- **应用层**：纯业务逻辑，定义 Pub/Sub、RPC、Subscribe 等操作

---

## 帧层设计（Frame Layer）

### Frame 结构

```protobuf
message Frame {
  // 服务端生成的全局唯一 ID（雪花算法或 UUID）
  string frame_id = 1;

  // 会话 ID（服务端在 Connect 时生成）
  string session_id = 2;

  // 客户端递增序列号（用于去重和顺序保证）
  uint64 client_seq = 3;

  // Unix 毫秒时间戳（服务端生成）
  int64 timestamp = 4;

  // Frame 类型标志位
  uint32 flags = 5;

  // 编码类型
  Encoding encoding = 6;

  // 应用层消息（编码后的字节）
  bytes payload = 10;
}

enum Encoding {
  ENCODING_UNSPECIFIED = 0;
  ENCODING_JSON = 1;
  ENCODING_PROTOBUF = 2;
  ENCODING_MSGPACK = 3;    // 扩展预留
  ENCODING_CBOR = 4;       // 扩展预留
}

enum FrameFlags {
  FLAG_NONE = 0x00;
  FLAG_ACK_REQUIRED = 0x01;    // 需要 ACK 确认
  FLAG_IS_ACK = 0x02;          // 这是一个 ACK 响应
  FLAG_IS_NACK = 0x04;         // 这是一个 NACK（否定确认）
  FLAG_COMPRESSED = 0x08;      // Payload 已压缩（gzip）
  FLAG_RETRANSMIT = 0x10;      // 这是重发的消息
  FLAG_PRIORITY = 0x20;        // 优先级标志（高优先级）
}
```

### Frame 层职责

1. **ID 生成**：服务端为每个 Frame 生成全局唯一 `frame_id`
2. **去重依据**：`session_id + client_seq` 组合唯一标识一个客户端消息
3. **ACK 机制**：通过 `FLAG_ACK_REQUIRED` 和 `FLAG_IS_ACK` 实现确认
4. **编码协商**：支持同一连接混合使用不同编码
5. **扩展能力**：通过 flags 支持压缩、优先级等特性

---

## 应用层设计（Message Layer）

### Message 封装

```protobuf
message Message {
  oneof payload {
    // 连接管理
    Connect connect = 1;
    Connected connected = 2;
    Disconnect disconnect = 3;
    Ping ping = 4;
    Pong pong = 5;

    // 订阅管理
    Subscribe subscribe = 10;
    Unsubscribe unsubscribe = 11;
    SubscribeResult subscribe_result = 12;

    // 发布/订阅
    Publish publish = 20;
    PublishResult publish_result = 21;
    Publication publication = 22;

    // RPC
    RpcCall rpc_call = 30;
    RpcReply rpc_reply = 31;

    // 历史回放
    HistoryRequest history_request = 40;
    HistoryReply history_reply = 41;

    // Survey（服务端发起）
    SurveyRequest survey_request = 50;
    SurveyResponse survey_response = 51;

    // 错误
    Error error = 99;
  }
}
```

### 核心消息类型

#### 1. 连接管理

```protobuf
message Connect {
  string client_id = 1;                    // 客户端标识（可选）
  string token = 2;                         // 认证令牌
  string client_type = 3;                   // 客户端类型
  map<string, bytes> metadata = 4;          // 客户端元数据
  repeated Encoding preferred_encodings = 5; // 请求的编码格式
  bool recover_subscriptions = 6;           // 是否恢复之前的订阅
}

message Connected {
  string session_id = 1;                    // 服务端分配的会话 ID
  Encoding encoding = 2;                    // 服务端选择的编码
  string server_version = 3;                // 服务端信息
  repeated string recovered_channels = 4;   // 恢复的订阅列表
  map<string, bytes> metadata = 5;          // 服务端元数据
}

message Disconnect {
  DisconnectCode code = 1;                  // 断开原因码
  string reason = 2;                         // 人类可读的原因
  bool reconnect_allowed = 3;                // 是否允许重连
}

enum DisconnectCode {
  DISCONNECT_NORMAL = 0;
  DISCONNECT_UNAUTHORIZED = 1;
  DISCONNECT_TOKEN_EXPIRED = 2;
  DISCONNECT_KICKED = 3;
  DISCONNECT_PROTOCOL_ERROR = 10;
  DISCONNECT_VERSION_MISMATCH = 11;
  DISCONNECT_TOO_MANY_ERRORS = 12;
  DISCONNECT_IDLE_TIMEOUT = 20;
  DISCONNECT_CONNECTION_LIMIT = 21;
  DISCONNECT_RATE_LIMIT = 22;
  DISCONNECT_SERVER_SHUTDOWN = 30;
  DISCONNECT_SERVER_RESTART = 31;
  DISCONNECT_INTERNAL_ERROR = 32;
}
```

#### 2. 订阅管理

```protobuf
message Subscribe {
  repeated string channels = 1;        // 订阅的通道列表（支持通配符 *）
  bool with_history = 2;                // 是否需要历史消息
  StreamPosition since = 3;             // 历史消息起始位置
  int32 history_limit = 4;              // 历史消息数量限制
}

message SubscribeResult {
  repeated string subscribed = 1;       // 成功订阅的通道
  repeated ChannelError failed = 2;     // 失败的通道及原因
  repeated Publication history = 3;     // 历史消息（如果请求）
}

message StreamPosition {
  uint64 offset = 1;                    // 偏移量
  string epoch = 2;                     // 纪元（用于检测流重置）
}
```

#### 3. 发布/订阅

```protobuf
message Publish {
  string channel = 1;                   // 目标通道
  bytes data = 2;                       // 消息数据
  string type = 3;                      // 消息类型（用户自定义）
  map<string, bytes> metadata = 4;      // 应用层元数据
  bool persist = 5;                     // 是否持久化到历史
}

message Publication {
  string channel = 1;                   // 来源通道
  bytes data = 2;                       // 消息数据
  string type = 3;                      // 消息类型
  map<string, bytes> metadata = 4;      // 元数据
  StreamPosition position = 5;          // 流位置（用于恢复）
  string publisher_id = 6;              // 发布者信息（可选）
}
```

#### 4. RPC 调用

```protobuf
message RpcCall {
  string channel = 1;                   // 目标通道
  string method = 2;                     // 方法名
  bytes data = 3;                       // 请求数据
  map<string, bytes> metadata = 4;      // 元数据
}

message RpcReply {
  bytes data = 1;                       // 响应数据
  map<string, bytes> metadata = 2;      // 元数据
  bool success = 3;                     // 是否成功
  Error error = 4;                      // 错误信息（如果失败）
}
```

#### 5. 错误处理

```protobuf
message Error {
  ErrorCode code = 1;                   // 错误码
  string message = 2;                    // 错误消息
  bool fatal = 3;                       // 是否致命（需要断开连接）
  map<string, string> context = 4;      // 额外上下文
}

enum ErrorCode {
  ERROR_UNKNOWN = 0;
  ERROR_UNAUTHORIZED = 1;
  ERROR_PERMISSION_DENIED = 2;
  ERROR_INVALID_CHANNEL = 3;
  ERROR_CHANNEL_LIMIT = 4;
  ERROR_RATE_LIMIT = 5;
  ERROR_INVALID_MESSAGE = 6;
  ERROR_RPC_TIMEOUT = 7;
  ERROR_RPC_NOT_FOUND = 8;
  ERROR_INTERNAL = 9;
  ERROR_SERVICE_UNAVAILABLE = 10;
}
```

**关键设计**：
- 所有消息使用 `map<string, bytes>` 元数据，支持任意类型
- 清晰的请求/响应配对（Publish/PublishResult, RpcCall/RpcReply）
- 错误分层：可恢复错误（Error 消息）vs 致命错误（Disconnect）

---

## ACK 和去重机制（At-least-once）

### ACK 流程

```
客户端                                    服务端
  │                                        │
  │  Frame {                               │
  │    client_seq: 100                     │
  │    flags: FLAG_ACK_REQUIRED            │
  │    payload: Publish(...)               │
  │  }                                     │
  ├────────────────────────────────────────>│
  │                                        │ [处理消息]
  │                                        │ [记录 session_id+100]
  │                                        │
  │            Frame {                     │
  │              client_seq: 100 (回显)    │
  │              flags: FLAG_IS_ACK        │
  │              payload: PublishResult    │
  │            }                           │
  │<────────────────────────────────────────┤
  │                                        │
  [收到 ACK，清除本地重发队列]               │
```

### 去重实现

**服务端去重缓存**：
- Key: `session_id + client_seq`
- Value: 已处理的消息结果（用于返回重复 ACK）
- TTL: 5 分钟

**重发逻辑**：
- 客户端超时未收到 ACK（5 秒），重发消息并设置 `FLAG_RETRANSMIT`
- 服务端检查去重缓存，如已处理则返回缓存的 ACK
- 最多重试 3 次，使用指数退避（5s → 10s → 20s）

**需要 ACK 的消息**：
- Publish
- Subscribe/Unsubscribe
- RpcCall
- HistoryRequest

**不需要 ACK 的消息**：
- Ping/Pong（心跳本身就是确认）
- Publication（服务端推送）
- SurveyResponse

---

## 错误处理和断开连接

### 分层错误处理

**可恢复错误**（返回 Error 消息或 NACK）：
- 无效通道名
- 权限不足
- 速率限制
- RPC 超时
- RPC 方法不存在

**致命错误**（发送 Disconnect + 关闭连接）：
- 认证失败
- 协议版本不匹配
- 格式错误过多
- 心跳超时
- 服务器主动踢出
- 服务器关闭/重启

### 断开连接流程

**服务端主动断开**：
1. 服务端发送 Disconnect 消息（包含 code 和 reason）
2. 服务端关闭底层连接（TCP FIN / WebSocket Close）
3. 客户端根据 `reconnect_allowed` 决定是否重连

**客户端主动断开**：
1. 客户端发送 Disconnect 消息
2. 客户端关闭底层连接
3. 服务端清理会话资源

**优雅关闭**（服务器维护）：
1. 服务端发送 Disconnect（code: SERVER_SHUTDOWN, reconnect_allowed: true）
2. 客户端停止发送新消息，等待已发送的 ACK
3. 延迟关闭连接，给客户端准备时间

---

## 编码和传输适配

### 编码器接口

```go
type Encoder interface {
    Type() Encoding
    EncodeFrame(frame *Frame) ([]byte, error)
    DecodeFrame(data []byte) (*Frame, error)
    EncodeMessage(msg *Message) ([]byte, error)
    DecodeMessage(data []byte) (*Message, error)
    MimeType() string
}
```

**内置编码器**：
- **ProtobufEncoder**：最紧凑，最快，适合生产环境
- **JSONEncoder**：人类可读，适合调试和 Web 浏览器
- **扩展接口**：支持 MessagePack、CBOR 等未来格式

### 传输层抽象

```go
type Transport interface {
    WriteFrame(frame *Frame) error
    WriteFrames(frames []*Frame) error  // 批量写入
    ReadFrame() (*Frame, error)
    Close(code DisconnectCode, reason string) error
    RemoteAddr() string
    Encoder() Encoder
}
```

**WebSocket 实现**：
- 通过子协议协商编码（`messageloop+json` / `messageloop+proto`）
- 一个 WebSocket 消息 = 一个 Frame
- JSON 使用 TextMessage，Protobuf 使用 BinaryMessage

**gRPC 实现**：
- 双向流式 RPC：`rpc Connect(stream Frame) returns (stream Frame)`
- 通过 metadata 协商编码
- 使用自定义 RawCodec 避免双重序列化

### 混合编码支持

同一连接可以发送不同编码的 Frame（通过 `frame.encoding` 字段标识）：
- 控制消息使用 JSON（易于调试）
- 数据消息使用 Protobuf（节省带宽）

---

## 关键消息流程

### 1. 连接建立

1. 客户端建立传输连接（WebSocket/gRPC）
2. 客户端发送 Connect 消息（包含 token, preferred_encodings）
3. 服务端通过 proxy 验证 token（调用 `$authenticate` 方法）
4. 服务端生成 session_id，选择编码
5. 服务端返回 Connected 消息（包含 session_id, encoding）
6. 客户端保存 session_id，开始心跳

### 2. 订阅和发布

**订阅流程**：
1. 客户端发送 Subscribe 消息（channels, with_history, since）
2. 服务端检查权限（通过 proxy）
3. 服务端添加到 Hub 订阅表
4. 服务端从 broker 获取历史消息（如果请求）
5. 服务端返回 SubscribeResult（subscribed, history）

**发布流程**：
1. 发布者发送 Publish 消息（channel, data, persist）
2. 服务端去重检查（session_id + client_seq）
3. 服务端写入 broker（Redis/Memory）
4. 服务端查找订阅者，广播 Publication 消息
5. 服务端返回 PublishResult（offset, success）

### 3. RPC 调用

1. 客户端发送 RpcCall 消息（channel, method, data）
2. 服务端路由匹配（proxy router）
3. 服务端应用超时（默认 30s）
4. 服务端转发到后端（HTTP 或 gRPC）
5. 服务端返回 RpcReply（data, success）或超时错误

### 4. 历史回放（断线恢复）

1. 客户端重连后发送 Subscribe（with_history: true, since: {offset, epoch}）
2. 服务端检查 epoch 是否匹配
3. 如果匹配，返回 offset 之后的消息
4. 如果不匹配（流重置），返回 STREAM_RESET 错误
5. 客户端清空本地历史，重新订阅

---

## 实施计划

### 第一阶段：协议定义（Protocol Definition）

**关键文件**：
- `protocol/v2/frame.proto` - Frame 层定义
- `protocol/v2/message.proto` - Message 层定义
- `protocol/v2/error.proto` - 错误和断开码定义

**任务**：
1. 编写 Protobuf 定义文件
2. 生成 Go 代码：`task generate-protocol`
3. 创建编码器接口和实现：
   - `protocol/v2/encoder.go` - Encoder 接口
   - `protocol/v2/encoder_json.go` - JSON 编码器
   - `protocol/v2/encoder_protobuf.go` - Protobuf 编码器

### 第二阶段：帧层实现（Frame Layer）

**关键文件**：
- `frame.go` - Frame 处理逻辑
- `frame_ack.go` - ACK 和去重机制
- `frame_dedup.go` - 去重缓存实现

**任务**：
1. 实现 Frame ID 生成（雪花算法或 UUID）
2. 实现 ACK/NACK 机制
3. 实现去重缓存（TTL 5 分钟）
4. 实现客户端重试逻辑

### 第三阶段：传输层适配（Transport Adapters）

**关键文件**：
- `transport.go` - Transport 接口定义
- `pkg/websocket/transport_v2.go` - WebSocket 传输实现
- `pkg/grpcstream/transport_v2.go` - gRPC 传输实现

**任务**：
1. 实现 Transport 接口
2. WebSocket 编码协商（子协议）
3. gRPC 编码协商（metadata）
4. 传输层测试（单元测试）

### 第四阶段：应用层实现（Application Layer）

**关键文件**：
- `client_v2.go` - Client 实现
- `node_v2.go` - Node 实现
- `handler_connect.go` - Connect 处理
- `handler_subscribe.go` - Subscribe 处理
- `handler_publish.go` - Publish 处理
- `handler_rpc.go` - RPC 处理

**任务**：
1. 重写 Client 使用新协议
2. 重写 Node 核心逻辑
3. 实现各消息类型的处理器
4. 集成现有 Hub 和 Broker

### 第五阶段：错误处理和断开连接（Error Handling）

**关键文件**：
- `disconnect.go` - Disconnect 处理
- `error_handler.go` - 错误分类和处理
- `reconnect.go` - 重连策略

**任务**：
1. 实现错误分类逻辑（可恢复 vs 致命）
2. 实现优雅断开流程
3. 实现客户端重连策略
4. 集成现有 Disconnect 代码

### 第六阶段：SDK 更新（Client SDKs）

**关键文件**：
- `sdks/go/v2/` - Go SDK v2
- `sdks/ts/src/v2/` - TypeScript SDK v2

**任务**：
1. Go SDK：实现 v2 协议支持
2. TypeScript SDK：实现 v2 协议支持
3. 编写 SDK 示例和文档
4. 向后兼容性测试（v1 和 v2 共存）

### 第七阶段：测试和验证（Testing & Validation）

**关键文件**：
- `protocol/v2/encoder_test.go` - 编码器测试
- `frame_test.go` - Frame 层测试
- `client_v2_test.go` - 客户端测试
- `test-e2e/v2/` - E2E 测试

**任务**：
1. 单元测试（覆盖率 > 80%）
2. 集成测试（订阅、发布、RPC）
3. E2E 测试（完整流程）
4. 性能测试（对比 v1 协议）
5. 压力测试（高并发、大量订阅）

---

## 验证方法

### 1. 单元测试

```bash
# 运行所有测试
go test ./...

# 运行 v2 协议测试
go test ./protocol/v2/...
go test ./frame_test.go
go test ./client_v2_test.go

# 测试覆盖率
go test -cover ./...
```

### 2. 编码器验证

```bash
# 测试 JSON 和 Protobuf 编码的互操作性
go test -v ./protocol/v2/encoder_test.go -run TestEncoderRoundtrip
```

### 3. 传输层验证

```bash
# WebSocket 传输测试
go test -v ./pkg/websocket/transport_v2_test.go

# gRPC 传输测试
go test -v ./pkg/grpcstream/transport_v2_test.go
```

### 4. E2E 测试

```bash
# 启动测试服务器（v2 协议）
go run cmd/server/main.go --config-dir ./configs --protocol-version v2

# 运行 E2E 测试
cd sdks/ts
npm run test:e2e:v2

# Go SDK 测试
cd sdks/go/example/v2
go run main.go
```

### 5. 性能基准测试

```bash
# 对比 v1 和 v2 性能
go test -bench=. -benchmem ./benchmark/

# 测试指标：
# - 消息吞吐量（messages/sec）
# - 延迟（p50, p95, p99）
# - 内存分配（allocs/op）
# - CPU 使用率
```

### 6. 兼容性测试

```bash
# v1 客户端连接 v2 服务器（应该被拒绝）
# v2 客户端连接 v1 服务器（应该被拒绝）
# v1 和 v2 服务器同时运行（不同端口）
```

### 7. 手动验证

**WebSocket 客户端**：
```javascript
// 浏览器控制台
const ws = new WebSocket('ws://localhost:9080/v2/ws', 'messageloop+json');
ws.onopen = () => {
  ws.send(JSON.stringify({
    client_seq: 1,
    flags: 1, // FLAG_ACK_REQUIRED
    encoding: 1, // ENCODING_JSON
    payload: btoa(JSON.stringify({
      connect: {
        token: "test-token",
        client_type: "browser"
      }
    }))
  }));
};
ws.onmessage = (e) => console.log('Received:', JSON.parse(e.data));
```

**gRPC 客户端**：
```bash
# 使用 grpcurl 测试
grpcurl -plaintext \
  -d '{"encoding": 1}' \
  localhost:9090 messageloop.v2.MessageLoop/Connect
```

---

## 性能优化预期

相比 v1 协议，v2 预期改进：

1. **内存分配减少 60%**：消除 CloudEvents 转换，直接使用 Protobuf 结构
2. **延迟降低 30-40%**：减少序列化/反序列化次数
3. **吞吐量提升 50%**：更紧凑的帧结构，减少网络开销
4. **GC 压力降低**：减少临时对象分配

---

## 迁移路径（后续阶段）

虽然 v2 是完全推倒重来，但可以提供迁移工具：

1. **双协议支持**：服务器同时支持 v1 和 v2（不同端点）
2. **客户端库并存**：v1 和 v2 SDK 可以同时使用
3. **数据迁移工具**：从 v1 历史数据迁移到 v2 格式
4. **监控和日志**：区分 v1 和 v2 流量，逐步切换

---

## 风险和缓解措施

| 风险 | 影响 | 缓解措施 |
|------|------|----------|
| 协议设计缺陷 | 高 | 充分的设计评审和原型验证 |
| 性能不达预期 | 中 | 早期性能基准测试，迭代优化 |
| 客户端迁移困难 | 中 | 提供详细文档和迁移指南 |
| 向后兼容性问题 | 低 | v2 完全独立，不影响 v1 |
| 测试覆盖不足 | 高 | 强制 80% 测试覆盖率 |

---

## 时间线（参考）

注意：不提供具体时间估算，仅列出阶段顺序。

1. 协议定义和 Protobuf 生成
2. 帧层和编码器实现
3. 传输层适配器
4. 应用层核心逻辑
5. 错误处理和连接管理
6. SDK 更新
7. 测试和验证
8. 性能优化
9. 文档和示例

---

## 总结

MessageLoop v2 协议通过以下核心改进解决 v1 的所有痛点：

✅ **性能提升**：消除 CloudEvents 转换，减少 60% 内存分配
✅ **简化设计**：清晰的三层架构，职责明确
✅ **灵活扩展**：`map<string, bytes>` 元数据，可插拔编码器
✅ **可靠传输**：At-least-once QoS，去重和重试机制
✅ **传输无关**：可运行在任何双向流上
✅ **双栈编码**：同时支持 JSON 和 Protobuf，易于调试和生产部署

这是一个完全重新设计的协议，为 MessageLoop 的长期发展奠定坚实基础。
