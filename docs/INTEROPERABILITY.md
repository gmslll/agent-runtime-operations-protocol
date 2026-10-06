---
title: 外部标准互操作设计
status: active-design
updated: 2026-09-22
---

# 1. 目标

本项目必须能够进入现有 Agent 生态，而不是要求外部系统放弃 MCP、A2A、ARD、CloudEvents 或 OpenTelemetry。

互操作采用“保留原协议语义、在运行层增加关系”的原则：

- 外部标准对象保持其原始含义。
- 本项目只增加运行、调度、治理和审计关系。
- Adapter 不得静默丢失身份、状态、产物、取消或错误语义。
- 无法无损映射时必须保留原始对象引用，并标记映射等级。

# 2. A2A

A2A 是独立 Agent 系统之间的互操作边界。本项目同时支持：

- 把已发布 AgentVersion 导出为 A2A Agent Card。
- 把外部 A2A Agent Card 导入为受治理的 AgentDefinition 候选。
- 通过 A2A Adapter 调用外部 Agent。
- 把本项目 Runtime 暴露为 A2A Server。

AROP v1 的目标协议线固定为 A2A 1.0，首个公开 `v1.0.0-rc.N` 的兼容 Fixture 固定到 A2A v1.0.1 Release。取消独立公共 v0.1；后续补丁版本可以进入 Compatibility Matrix，但不得在不更新 Fixture 和报告的情况下仅写 `latest`。

映射等级固定为：

| 等级 | 含义 |
| --- | --- |
| `exact` | 字段和语义可无损往返 |
| `extended` | 依赖 AROP 或 A2A Extension 才能保留完整语义 |
| `lossy` | 可以执行转换，但必须报告丢失的字段或状态 |
| `unsupported` | 当前 Adapter 不允许转换 |

发生 `extended` 或 `lossy` 映射时必须保留原始对象引用和源协议版本，禁止静默丢失信息。

## 2.1 对象映射

| 本项目 | A2A | 规则 |
| --- | --- | --- |
| AgentDefinition | Agent Card identity/description | 一个稳定业务能力对应一个发布身份 |
| AgentVersion | Agent Card 版本或扩展元数据 | 不覆盖 A2A 原生字段 |
| AgentSkill | Agent Card Skill | Skill ID 和输入输出描述可映射 |
| Run | Task | 生命周期相同时可以一对一 |
| Attempt | 本项目扩展关系 | A2A Task 重试不直接等同于物理 Attempt |
| Message | Message | 保留 Role、Part 和 Media Type |
| AssetRef/ResultRef | Artifact/Part 引用 | 大文件仍使用受控引用 |
| Event | Task 状态、消息、Artifact 更新 | Adapter 分配本项目 Event ID 和 Sequence |
| conversation_ref | context/session reference | 只传通用引用，不传本机或渠道私有 ID |

## 2.2 生命周期规则

- A2A Task 是外部协议真值；Adapter 不得伪造不存在的 A2A 状态。
- 本项目 Run 可以包含一个 A2A Task，也可以在编排场景关联多个 A2A Task。
- 每次对外实际投递仍创建 Attempt，以支持 Fencing、Trace 和重试审计。
- Cancel 只有在收到对端确认或终态后才能转换为已取消。
- A2A Artifact 必须在终态 Snapshot 或 ResultRef 中可恢复。

# 3. MCP

MCP 是 Agent 与工具、资源、提示和上下文能力之间的协议，不是本项目 Registry 或 Run Protocol 的替代品。

支持方式：

- Manifest 可以声明 Agent 运行所需的 MCP 能力类别和逻辑依赖。
- Secret、Token 和实际 MCP Endpoint 不写入公共 Manifest。
- Runtime 通过部署配置或凭据代理解析具体 MCP Server。
- MCP Tool 调用可关联当前 `run_id`、`attempt_id`、`traceparent` 和审计记录。
- MCP 长任务结果可以映射为 Step/Tool Event，但不得泄露模型隐藏推理。

本项目不重新定义 MCP JSON-RPC 消息、Tool Schema、Resource 或 Prompt。

# 4. ARD 与 AI Catalog

外部发现使用开放发现格式；本地运行路由继续使用 Runtime Registry。

## 4.1 导出

Control Plane 或独立 Publisher 可以把已公开的 AgentDefinition 导出为：

- ARD Entry。
- AI Catalog Entry。
- A2A Agent Card URL。
- MCP Server Card URL（如果该资源本身是 MCP Server）。

导出内容只包含公开元数据和公开入口，不包含：

- RuntimeInstance 列表。
- 内网 Endpoint。
- Lease、容量和健康细节。
- Deployment Credential。
- 未授权 Skill 或内部标签。

## 4.2 导入

外部发现结果进入本地目录前必须经过：

```text
获取原始文档
→ 验证来源、签名和 Schema
→ 安全扫描和信任策略
→ 创建本地候选定义
→ 管理员审核和授权
→ 建立受控 Adapter/Deployment
```

“能够发现”不等于“允许调用”。

# 5. CloudEvents

Event Envelope 使用 CloudEvents 兼容属性，并为本项目的 Run/Attempt/Sequence 定义稳定扩展属性。

要求：

- JSON Structured Mode 是基线事件编码。
- HTTP Event Batch 尽量采用 CloudEvents Batch Content Mode。
- HTTP Binary Mode 映射必须有单独 Fixture。
- 扩展属性的 wire name 只能使用一种规范拼法。
- SDK 属性名与 wire name 的转换必须由生成代码或公共映射完成。
- 事件类型使用公共项目的稳定命名空间；实现方专属事件使用独立扩展前缀。

# 6. OpenTelemetry

协议传播 W3C Trace Context，并把运行事实映射为 OpenTelemetry Trace、Metric 和 Log。

建议 Span 层级：

```text
create run
└── dispatch attempt
    ├── invoke agent
    ├── invoke tool / MCP
    ├── produce asset
    └── deliver events
```

要求：

- `run_id` 和 `attempt_id` 用于关联，不替代 Trace ID 和 Span ID。
- Direct、Proxy 和 Worker Pull 必须继续传播原 Trace Context。
- 重试创建新的 Attempt Span，并关联原 Run。
- 高基数业务 ID 默认不作为 Metric Label。
- Prompt、Message、Tool 参数和结果默认不进入 Telemetry，除非显式启用并经过脱敏策略。
- OpenTelemetry GenAI Semantic Conventions 仍在演进时，通过集中映射层适配，不把实验字段固化为核心协议字段。

# 7. OpenAPI、AsyncAPI 与 JSON Schema

- JSON Schema 是数据结构的权威来源。
- OpenAPI 引用相同 Schema，描述同步请求、状态查询和管理接口。
- AsyncAPI 引用相同 Event Schema，描述事件流和批量通道。
- SDK Generated Models 由相同 Schema 生成。
- 外部协议 Adapter 的映射 Fixture 与核心 Fixture 一起版本化。

# 8. Capability Negotiation

实现不得仅用协议版本推断全部能力。Manifest、注册响应和 Dispatch Ticket 必须明确协商：

```text
arop.delivery.direct.v1
arop.delivery.proxy.v1
arop.delivery.worker-pull.v1
arop.streaming.sse.v1
arop.streaming.resume.v1
arop.events.batch.v1
arop.registry.watch.v1
arop.session.sticky.v1
arop.session.portable.v1
arop.interop.a2a-server.v1
arop.interop.a2a-client.v1
arop.interop.ard-export.v1
arop.interop.mcp-dependencies.v1
```

新增可选 Capability 不改变主版本；改变既有 Capability 语义必须升级协议主版本。

# 9. 扩展命名空间

AROP 标准能力使用 `arop.<area>.<capability>.v<major>`。第三方扩展必须使用自己控制的反向域名，例如 `com.example.arop.gpu-scheduling.v1`，不得占用 `arop.*`。

扩展必须声明：

- 唯一命名空间。
- 所有者和文档 URL。
- 版本和稳定性。
- 使用位置和 JSON Schema。
- 未识别时的处理方式。
- 是否包含敏感数据。

核心实现必须忽略不影响安全的未知可选扩展；无法理解且影响授权、签名、幂等或副作用的扩展必须拒绝执行。

# 10. Conformance

互操作测试至少包括：

- Manifest 与 A2A Agent Card 导入导出。
- Run 与 A2A Task 生命周期映射。
- Message、Artifact 和 AssetRef 映射。
- A2A Cancel、Failure 和 Timeout。
- ARD/AI Catalog 导出不泄露 Runtime Metadata。
- CloudEvents Structured、Binary 和 Batch 编码。
- Trace Context 穿过 Direct、Proxy、Pull、A2A 和 MCP Adapter。
- 未知扩展、版本不兼容和映射降级。

测试报告必须指出原协议版本、Adapter 版本、支持范围和已知有损映射。

# 11. 上游规范

发布 Compatibility Matrix 时必须固定实际测试的上游版本。设计入口：

- [A2A Protocol Specification](https://a2a-protocol.org/latest/specification)
- [Model Context Protocol Specification](https://modelcontextprotocol.io/specification/)
- [Agentic Resource Discovery Specification](https://agenticresourcediscovery.org/spec/)
- [CloudEvents Specification](https://github.com/cloudevents/spec)
- [OpenTelemetry Semantic Conventions](https://opentelemetry.io/docs/specs/semconv/)
- [OpenAPI Specification](https://spec.openapis.org/oas/latest.html)
- [AsyncAPI Specification](https://www.asyncapi.com/docs/reference/specification/latest)
- [JSON Schema](https://json-schema.org/specification)

文档链接指向最新入口，Release 的 Compatibility Matrix 必须记录精确版本或 commit，不能只写“latest”。
