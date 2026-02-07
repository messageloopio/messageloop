# CloudEvents SDK 迁移指南

## 概述

MessageLoop Go SDK 已经更新为使用 `github.com/cloudevents/sdk-go/v2` 的 `cloudevents.Event` 类型，而不是原始的 protobuf 类型 `pb.CloudEvent`。这样可以提供更好的 CloudEvents 标准兼容性和更友好的 API。

## 主要变更

### 1. 依赖更新

已添加 `github.com/cloudevents/sdk-go/v2` 依赖到 `go.mod`。

### 2. API 变更

#### Client 接口

```go
// 之前
Publish(channel string, event *pb.CloudEvent) error
RPC(ctx context.Context, channel, method string, req, resp *pb.CloudEvent) error
OnMessage(fn func([]*Message))

// 现在
Publish(channel string, event *cloudevents.Event) error
RPC(ctx context.Context, channel, method string, req, resp *cloudevents.Event) error
OnMessage(fn func([]*cloudevents.Event))
```

#### Message 结构体

```go
// 之前
type Message struct {
    ID          string
    Channel     string
    Offset      uint64
    Event       *pb.CloudEvent
    Data        []byte
    ContentType string
}

// 现在
type Message struct {
    ID          string
    Channel     string
    Offset      uint64
    Event       *cloudevents.Event  // 类型已更改
    Data        []byte
    ContentType string
}
```

#### 辅助函数

```go
// 之前
NewCloudEvent(id, source, eventType string, data []byte) *pb.CloudEvent
NewTextCloudEvent(id, source, eventType, textData string) *pb.CloudEvent
SetEventAttribute(event *pb.CloudEvent, key, value string)

// 现在
NewCloudEvent(id, source, eventType string, data []byte) *cloudevents.Event
NewTextCloudEvent(id, source, eventType, textData string) *cloudevents.Event
SetEventAttribute(event *cloudevents.Event, key, value string)
```

### 3. 新增转换函数

SDK 内部会自动处理 `pb.CloudEvent` 和 `cloudevents.Event` 之间的转换。如果你需要手动转换，可以使用以下函数：

```go
// pb.CloudEvent -> cloudevents.Event
// 使用官方 SDK: github.com/cloudevents/sdk-go/binding/format/protobuf/v2
func PbToCloudEvent(pbEvent *pb.CloudEvent) (*cloudevents.Event, error)

// cloudevents.Event -> pb.CloudEvent
// 使用官方 SDK: github.com/cloudevents/sdk-go/binding/format/protobuf/v2
func CloudEventToPb(event *cloudevents.Event) (*pb.CloudEvent, error)
```

**注意**：这两个函数内部使用 CloudEvents 官方 SDK 的 `format.FromProto` 和 `format.ToProto` 方法进行转换，确保与 CloudEvents 规范完全兼容。

## 迁移步骤

### 1. 更新导入

```go
// 之前
import (
    pb "github.com/cloudevents/sdk-go/binding/format/protobuf/v2/pb"
    messageloopgo "github.com/messageloopio/messageloop/sdks/go"
)

// 现在
import (
    cloudevents "github.com/cloudevents/sdk-go/v2"
    messageloopgo "github.com/messageloopio/messageloop/sdks/go"
)
```

### 2. 更新事件创建代码

```go
// 之前
event := messageloopgo.NewCloudEvent(
    "msg-123",
    "/client/example",
    "chat.message",
    []byte("Hello, MessageLoop!"),
)

// 现在（代码保持不变，但返回类型已更改）
event := messageloopgo.NewCloudEvent(
    "msg-123",
    "/client/example",
    "chat.message",
    []byte("Hello, MessageLoop!"),
)
```

### 3. 更新 RPC 调用

```go
// 之前
var resp pb.CloudEvent
err = client.RPC(ctx, "user.service", "GetUser", req, &resp)
if err != nil {
    return err
}
log.Printf("Response: %s", resp.GetTextData())

// 现在
resp := cloudevents.NewEvent()
err = client.RPC(ctx, "user.service", "GetUser", req, &resp)
if err != nil {
    return err
}
log.Printf("Response: %s", resp.Data())
```

### 4. 更新消息处理

```go
// 之前
client.OnMessage(func(messages []*messageloopgo.Message) {
    for _, msg := range messages {
        pbEvent := msg.Event  // *pb.CloudEvent
        id := pbEvent.GetId()
        source := pbEvent.GetSource()
        data := msg.Data      // 原始数据
        channel := msg.Channel // 消息来源通道
    }
})

// 现在
client.OnMessage(func(events []*cloudevents.Event) {
    for _, event := range events {
        // 直接使用 cloudevents.Event
        id := event.ID()
        source := event.Source()
        eventType := event.Type()
        data := event.Data()  // []byte
        
        // 注意：如果需要通道、偏移量等元信息，需要通过扩展属性获取
        // 或者联系我们添加这些信息到 CloudEvent 的扩展中
    }
})
```

## CloudEvents API 对比

### 访问事件属性

```go
// pb.CloudEvent
id := pbEvent.GetId()
source := pbEvent.GetSource()
type := pbEvent.GetType()
specVersion := pbEvent.GetSpecVersion()

// cloudevents.Event
id := event.ID()
source := event.Source()
type := event.Type()
specVersion := event.SpecVersion()
```

### 访问数据

```go
// pb.CloudEvent
textData := pbEvent.GetTextData()
binaryData := pbEvent.GetBinaryData()

// cloudevents.Event
data := event.Data()  // []byte
```

### 设置扩展属性

```go
// pb.CloudEvent
messageloopgo.SetEventAttribute(pbEvent, "mykey", "myvalue")

// cloudevents.Event
event.SetExtension("mykey", "myvalue")
// 或使用辅助函数
messageloopgo.SetEventAttribute(event, "mykey", "myvalue")
```

## 兼容性

- SDK 内部自动处理所有类型转换
- 服务器端无需更改，仍然使用 protobuf 通信
- 所有现有功能保持不变

## 优势

1. **标准兼容**: 完全符合 CloudEvents 规范
2. **更好的 API**: 使用标准的 CloudEvents SDK API
3. **生态系统**: 可以与其他 CloudEvents 工具和库集成
4. **类型安全**: 更好的类型检查和 IDE 支持
