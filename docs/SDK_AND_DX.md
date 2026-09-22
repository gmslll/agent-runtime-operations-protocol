---
title: SDK、CLI 与开发者接入体验
status: active-design
updated: 2026-09-22
---

# 1. 目标体验

普通开发者应能在 10 分钟内完成一个本地 Agent 的协议接入，包装一个已有 Python Agent 的核心业务改动目标不超过约 30 行：

```text
安装 SDK
→ 生成项目
→ 定义 Manifest
→ 实现 handle_run
→ 本地测试
→ 注册测试环境
```

高可用、Token 校验、幂等、Trace、事件批量、Outbox、健康检查和优雅下线尽量由 SDK 处理。

Quickstart、SDK 和 Conformance 必须可以连接 Reference Control Plane 独立运行，不得要求开发者安装金运 Console。

# 2. SDK 分层

```text
Generated Models
  JSON Schema 生成的数据类型

Protocol Core
  ID、时间、错误、事件、状态机、校验器

Provider SDK
  Agent Server、Run Handler、Event Emitter、Outbox

Consumer SDK
  Run 创建结果、Dispatch Ticket、Direct Client、SSE Client

Registry SDK
  Register、Keepalive、Drain、Watch、Cache

Worker SDK
  Claim、Attempt Lease、Fencing、Session Affinity

Conformance Kit
  Mock Control Plane、测试向量、故障注入
```

# 3. 公共 v0.1 语言

## 3.1 Go

用途：

- Control Plane 和 Console 引用协议类型。
- Go HTTP Agent。
- Gateway/BFF/Worker。
- 契约测试工具。

## 3.2 Python

用途：

- LLM/AI Agent 的主要提供方 SDK。
- Python Web Framework 集成。
- 参考 Agent 和本地开发。
- FastAPI/ASGI 薄封装和十分钟 Quickstart。

## 3.3 TypeScript

用途：

- Web/BFF Streaming Consumer。
- SSE、Schema 和 UI 类型。

公共 v0.1 至少提供 TypeScript Generated Models、SSE Consumer 和 Event Reducer；完整 Node Provider 可以随后实现。所有语言必须使用同一 Schema Source。

# 4. Provider SDK API 示例

Python Distribution 名称固定为 `arop-sdk`，Import Package 为 `arop`；CLI 固定为 `arop`。正式发布前仍需完成 PyPI/npm 占用检查和所有权验证。

```python
from arop import Agent, RunContext

agent = Agent.from_manifest("agent.yaml")

@agent.skill("default")
async def handle_run(ctx: RunContext, input: dict):
    await ctx.progress(percent=10, message="开始处理")
    await ctx.output.delta("answer", "正在生成")
    result = {"text": "最终结果"}
    await ctx.usage("output_tokens", 100, source="agent_reported")
    return result

agent.serve(host="0.0.0.0", port=8080)
```

SDK 自动提供：

- `POST /v1/runs`
- `GET /v1/runs/{run_id}`
- `GET /v1/runs/{run_id}/events`
- `POST /v1/runs/{run_id}/commands`
- `/v1/health/live`
- `/v1/health/ready`
- Run Token 校验。
- Input/Output Schema 校验。
- Attempt Inbox 和幂等。
- Event ID、Sequence 和 Trace。
- Event Outbox 和批量 ACK。
- Deadline、Cancel 和优雅关闭。

# 5. Runtime 注册示例

```python
from arop.registry import RuntimeRegistration

registration = RuntimeRegistration(
    service_id="image-agent-runtime",
    instance_id="image-agent-01",
    manifest="agent.yaml",
    transport="direct",
)

async with registration:
    await agent.serve()
```

Context Manager 负责：

```text
启动注册
→ 获取 Lease 和 Generation
→ 周期 Keepalive
→ 断线重试
→ Lease 失效重新注册
→ SIGTERM 进入 Drain
→ 已有 Run 完成
→ 注销
```

# 6. Worker SDK 示例

```python
worker = Worker.from_config("worker.yaml")

@worker.skill("image.generate", "default")
async def execute(ctx, request):
    return await run_local_agent(request)

await worker.run_forever()
```

Worker SDK 负责：

- 长轮询 Claim。
- Attempt Lease 续租。
- Fencing Token 传播。
- 并发槽位。
- Sticky Session 映射。
- Event Outbox。
- Drain 与恢复。

# 7. Consumer SDK

可信后端调用：

```python
run = await control_plane.create_run(
    agent_id="image.generate",
    input={"prompt": "生成白底图"},
    idempotency_key="business-request-123",
)

async for event in run.stream():
    render(event)
```

SDK 根据 Dispatch Ticket 自动处理：

- Direct 调用。
- Proxy/Relay SSE。
- 断线重连。
- Last-Event-ID。
- Ticket 过期和重新 Dispatch。
- 标准错误。

Consumer SDK 不自行从 Registry 选择另一个实例。重新路由必须回到 Control Plane。

# 8. CLI

建议命令：

```bash
arop init --lang python
arop manifest validate agent.yaml
arop schema validate schemas/input.schema.json
arop dev
arop test
arop test --fault duplicate-delivery
arop publish --environment test
arop register --environment test
arop export a2a
arop export ard
arop doctor
```

## 8.1 init

生成：

```text
agent.yaml
schemas/input.schema.json
schemas/output.schema.json
src/agent.py
tests/test_contract.py
.env.example
Dockerfile
```

不得生成真实密钥。

## 8.2 dev

启动：

- 本地 Agent Server。
- Mock Control Plane。
- Event Viewer。
- 热重载。
- 示例调用页面或终端输出。

## 8.3 test

自动验证：

- Manifest 和 Schema。
- 正常 Run。
- 重复 Dispatch。
- Token Audience。
- 流式事件顺序。
- Event 重放。
- Cancel、Timeout。
- Callback/Event Batch 重试。
- Output Snapshot。
- Usage 和 Error。

# 9. Conformance Level

建议认证等级：

| Level | 要求 |
| --- | --- |
| Core Provider | Manifest、Run、Result、Error、Cancel、幂等 |
| Streaming Provider | Core + Event Stream + Resume + Snapshot |
| Managed Runtime | Streaming + Register + Lease + Health + Drain |
| Pull Worker | Core + Claim + Attempt Lease + Fencing |
| Control Plane | Authz、Run Ledger、Dispatch、Event Ledger、Discovery |
| Production | Durable Inbox/Outbox、Trace、安全配置和故障测试 |

Registry Discovery View 可以根据环境要求过滤不满足认证等级的实例。

# 10. 参考适配器

协议仓建议提供：

- Plain HTTP/Webhook Adapter。
- OpenAI-compatible LLM Adapter。
- Python Function Agent。
- Go HTTP Agent。
- Pull Worker Reference。
- cc-connect/Codex Adapter 示例。
- A2A Agent Card/Task Adapter。
- ARD/AI Catalog Exporter。
- MCP Integration Example。
- Reference Control Plane。

Adapter 只负责协议转换，不复制权限和 Control Plane 业务规则。

# 11. Schema 生成策略

推荐单一权威源：

```text
schemas/
→ OpenAPI/AsyncAPI 引用
→ Go Models
→ Python Models
→ TypeScript Models
→ 文档和测试向量
```

生成代码与手写代码分目录：

```text
sdk/go/generated
sdk/go/runtime
sdk/python/generated
sdk/python/runtime
```

CI 必须验证重新生成后无未提交差异。

# 12. SDK 稳定性

- SDK 主版本与 Protocol 主版本对齐。
- SDK 可以支持多个兼容 Protocol Minor Version。
- Deprecated API 至少保留一个公开弃用周期。
- SDK 默认安全设置不能为了“更容易运行”跳过 Token、TLS 或 Schema 校验。
- SDK 错误必须保留协议 Error Code 和 Trace ID。

# 13. 文档要求

每种语言必须提供：

- 十分钟以内的端到端 Quickstart。
- 完整 API Reference。
- Direct、Proxy、Worker 示例。
- Stateful Session 示例。
- 流式与断线恢复示例。
- 副作用幂等示例。
- 生产部署检查表。
- 升级与兼容说明。
- Conformance 等级和公开测试报告生成方式。
- A2A、MCP、ARD、CloudEvents 和 OpenTelemetry 兼容范围。
