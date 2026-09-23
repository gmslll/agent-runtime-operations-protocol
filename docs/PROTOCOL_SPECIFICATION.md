---
title: Agent Runtime Operations Protocol v1 规范
status: draft-normative
updated: 2026-09-22
---

# 1. 规范范围

本文定义 Protocol v1 的规范性语义，包括版本、标识符、Manifest、运行对象、状态、消息、错误、时间和传输绑定。

Core 是所有实现的共同语义；Runtime Management、Delivery、Streaming 和 Governance 能力通过 Profile/Extension 与 Capability Negotiation 声明。实现不得仅凭 `protocol_version` 假设对端支持所有可选能力。

本文使用以下关键词：

- **MUST / 必须**：兼容实现必须满足。
- **MUST NOT / 禁止**：兼容实现不得发生。
- **SHOULD / 应当**：除非有明确理由，否则应满足。
- **MAY / 可以**：可选能力，必须通过 Capability 声明。

# 2. 协议版本

## 2.1 版本维度

系统存在多个相互独立的版本：

| 字段 | 含义 |
| --- | --- |
| `protocol_version` | 本协议版本 |
| `manifest_version` | Manifest Schema 版本 |
| `agent_version` | 业务 Agent 发布版本 |
| `runtime_version` | RuntimeService 软件版本 |
| `sdk_version` | 接入 SDK 版本 |
| `event_schema_version` | 事件载荷 Schema 版本 |

这些版本不得混用。Agent 软件升级不必然改变 AgentVersion；业务输入输出或语义变化必须发布新的 AgentVersion。

## 2.2 兼容规则

- 主版本变化表示可能破坏兼容性。
- 次版本只允许增加可选字段、事件类型或能力。
- 补丁版本修正文档、约束或实现错误，不改变数据语义。
- 作者和发布端必须使用文档声明的精确 Schema 严格校验；未知字段是作者错误。
- 同一主版本的兼容消费者必须保留或忽略未知可选字段；但影响授权、签名、幂等、副作用或必需扩展的未知语义必须拒绝。
- 生产者不得改变既有字段类型、含义、必填性或枚举语义。
- 新的必填字段必须进入新的主版本。

注册时双方协商共同支持的最高兼容版本。没有交集时返回 `PROTOCOL_VERSION_UNSUPPORTED`。

# 3. 标识符

## 3.1 必需标识符

| ID | 生命周期 |
| --- | --- |
| `agent_id` | AgentDefinition 稳定业务标识 |
| `skill_id` | Agent 内 Skill 标识 |
| `service_id` | RuntimeService 标识 |
| `instance_id` | 运行实例稳定配置标识 |
| `session_id` | 某次进程启动标识 |
| `deployment_id` | Control Plane 分配的部署记录标识 |
| `lease_id` | RuntimeInstance 在线租约 |
| `run_id` | 一次业务调用 |
| `attempt_id` | 一次实际执行尝试 |
| `event_id` | 一个不可变事件 |
| `asset_id` | 文件或产物 |
| `conversation_ref` | 跨 Run 会话引用 |

## 3.2 ID 规则

- 所有 ID 必须区分大小写并按原样传输。
- ID 不得包含密钥、手机号、邮箱、用户姓名或业务敏感信息。
- 资源 ID 应由资源所有方生成或由 Control Plane 分配。
- `event_id` 在同一 `source` 下必须唯一。
- ID 不得被回收后代表另一个资源。
- `agent_id`、`skill_id` 等面向人的稳定标识使用小写 slug；发布后不得原地改名。
- 系统生成资源 ID 使用 `<resource-prefix>_<uuidv7>`，例如 `run_019...`、`att_019...`、`evt_019...`。
- `instance_id` 是可持久化稳定标识；`session_id` 每次进程启动都必须使用新的 UUIDv7。
- `effect_id` 必须由业务副作用的稳定身份生成，重试和新 Attempt 不得改变同一副作用的 `effect_id`。

# 4. 时间与时钟

- 所有外部时间使用 UTC RFC 3339，建议微秒精度。
- Lease、Deadline 和 Expiration 由服务端时间决定。
- 客户端时间仅用于观测，不得单独决定租约有效性。
- 事件同时记录 `occurred_at` 和 Control Plane `received_at`。
- 超时使用绝对 `deadline_at` 传递，避免多跳重复扣减不一致。

# 5. Agent Manifest

## 5.1 顶层结构

```yaml
protocol: arop/v1
kind: AgentManifest

identity:
  id: image.generate
  version: 1.0.0
  name: 商品图生成
  summary: 根据商品素材生成营销图片
  description: 支持参数和对话调用
  owner:
    team: ai-design
    contact: ai-design@example.com
  categories: [design, image]
  tags: [商品图, 生图]

skills:
  - id: default
    name: 生成商品图
    invoke_modes: [params, chat]
    input_schema:
      $ref: ./schemas/input.schema.json
    output_schema:
      $ref: ./schemas/output.schema.json

content:
  input_types: [text, json, asset_ref]
  output_types: [text, json, asset_ref]
  accepted_media_types: [image/png, image/jpeg]
  max_assets: 10
  max_input_bytes: 1048576

execution:
  default_timeout_seconds: 300
  max_timeout_seconds: 1800
  effects:
    level: none
    idempotency: supported
    human_confirmation: false
  capabilities:
    streaming: true
    progress: true
    cancellation: true
    status_query: true
    usage: true

session:
  mode: stateless

usage:
  meters: [calls, input_tokens, output_tokens, duration_seconds]

presentation:
  icon: image
  preferred_input_ui: form
  preferred_result_ui: gallery
  suggested_prompts: [生成一张白底商品图]
```

## 5.2 Manifest 约束

- Manifest 必须通过对应版本 JSON Schema。
- `agent_id + agent_version` 唯一。
- Published Manifest 内容不可原地修改。
- 发布时先完成 Schema 校验，再把语义对象转换为 JSON，并按 RFC 8785 JCS 规范化。
- Digest 输入排除 `manifest_digest` 和签名字段；算法固定为 SHA-256，Wire 格式为 `sha256:<lowercase-hex>`。
- RuntimeInstance 注册的 Digest 必须与已发布版本一致。
- Manifest 不得包含 Endpoint、飞书群、人员授权、密码、Token 或客户数据。
- `presentation` 只能提供通用 UI 提示，渠道最终呈现由 ChannelBinding 决定。

## 5.3 Skill

- 每个 AgentVersion 至少包含一个 Skill。
- 只有一个 Skill 时可以使用 `default`。
- 每个 Skill 独立声明输入输出 Schema 和调用模式。
- Run 必须绑定一个确定 Skill；省略时仅允许解析到唯一默认 Skill。

## 5.4 副作用

`effects.level`：

| 值 | 含义 |
| --- | --- |
| `none` | 纯计算、可安全重试 |
| `read` | 读取外部系统，不修改 |
| `write` | 修改外部状态 |
| `irreversible` | 下单、退款、发送等难以撤销操作 |

`write` 和 `irreversible` Agent 必须声明业务幂等能力。Control Plane 可以要求人工确认和更严格的 Token Scope。

## 5.5 Schema Declaration 与发布包

Skill 的 Input/Output Schema 必须使用且只使用以下一种形式：

- 内联 JSON Schema。
- AgentVersion 发布包内的 POSIX 相对 `$ref`。

禁止绝对路径、`file:`、带 URI authority 的引用、HTTP(S) 远程引用和越界 `..`。Publisher 必须上传完整的不可变发布包；Control Plane 在不访问网络的情况下解析全部引用，限制文件数、总字节和引用深度，验证引用闭包后才允许发布。

v1 的可移植 Schema Profile 固定为 JSON Schema Draft 2020-12。Schema 出现 `$schema` 时，其值必须精确为 `https://json-schema.org/draft/2020-12/schema`；不接受其他 dialect，也不接受属于旧 dialect 的 `$recursiveRef` 和 `$recursiveAnchor`，递归动态引用使用 `$dynamicRef` 和 `$dynamicAnchor`。

Publisher 提交的 Skill Input/Output Schema 和 Extension Schema 使用同一可移植 profile：`format` 仅允许值为 `date-time`，并按大写 `T`/`Z`、有效公历日期与时区偏移的严格 RFC 3339 语义断言；`pattern` 必须为 `^...$` 首尾锚定、最多 512 字节的可见 ASCII 表达式，仅允许安全字面量、非取反 ASCII 字符类和 1–256 的固定 `{n}` 重复，禁止任意长度重复、分组、分支、回溯引用、环视和转义；`patternProperties` 和 `multipleOf` 不在 v1 可移植 profile 中。Manifest、Publisher Schema 和 Extension data 的任意对象层级都递归拒绝 `__proto__`、`prototype`、`constructor` 键。协议自有的内建 Schema 使用仓库固定实现和契约测试的校验器。

包内 `$ref`、`$id` 和 Extension `schema_ref` 的路径使用可移植 ASCII 子集：每个路径段必须匹配 `[A-Za-z0-9._~-]+`，用 `/` 分隔，只允许可选的前导 `./`，并必须与磁盘上的实际大小写精确一致。禁止 `%` 编码、Unicode、空白/控制字符、反斜杠和 `{}`、`[]`、`|`、`^`、反引号等非 portable 标点。URI fragment 不按文件路径处理，但只允许 portable anchor 或 RFC 6901 JSON Pointer。

## 5.6 生成模型映射 Profile

Go、Python 和 TypeScript 模型必须由同一份 Schema 资源闭包和同一个固定版本的 mapping profile 生成。生成器只能读取命令中显式列举且已绑定 Digest 的本地资源，不得在解析 `$ref` 或生成期间访问网络。未知 Schema 关键字、不在闭包内的引用、生成标识符冲突、歧义 union 和不受支持的循环必须终止生成，不得降级成宽松类型。

跨语言 wire `integer` 必须声明有限 `minimum`/`maximum`，且整个区间落在 `[-9007199254740991, 9007199254740991]` 内。该规则递归适用于所有通用 `JsonValue`，包括 Extension `data`、`attributes` 以及任意嵌套数组或对象中的整数；任意一层无界或越界都必须拒绝生成，v1 不引入 BigInt 或 string integer profile。`date-time` 映射为先按本规范严格验证的 wire string，不默认转成 `time.Time`、`datetime` 或 `Date`，因为这些转换可能改写原始时区偏移和小数秒精度。

`oneOf` 只有在每个分支都含一个必需、唯一且互不相同的 `const` discriminator 时才映射为 tagged union。字段未出现与字段显式为 `null` 是不同的 wire 状态；生成类型和 codec 必须保留缺失、null 和具体值三态，不得用单一零值或空指针合并。

P07 只生成代表性 spike 并冻结 pipeline、mapping profile、固定工具版本、输入/输出 Digest 与 exact file inventory。后续 Schema/OpenAPI/AsyncAPI 生成物由首次引入它们的 Contract 阶段调用同一 pipeline 交付，禁止在 P07 预生成未来尚不存在的合同。

# 6. 内容模型

## 6.1 ContentPart

统一内容容器：

```json
{
  "type": "text",
  "text": "生成一张白底商品图"
}
```

首期类型：

- `text`
- `json`
- `asset_ref`
- `data_ref`

未知类型必须通过 Capability/Extension 协商，不得把任意二进制直接塞入 JSON。

Core ContentPart 保持闭合。非核心内容不得直接创造新的 `type`，而必须使用统一扩展包装：

```json
{
  "type": "extension",
  "extension_id": "com.example.arop.chart.v1",
  "schema_digest": "sha256:...",
  "data": {}
}
```

扩展 ContentPart 必须先完成 Capability/Extension 协商和 Schema 校验。

## 6.2 Message

```json
{
  "message_id": "msg_01...",
  "role": "user",
  "parts": [
    {"type": "text", "text": "分析这张商品图"},
    {"type": "asset_ref", "asset": {"asset_id": "asset_019...", "name": "product.png", "media_type": "image/png", "size_bytes": 238291, "access": {"mode": "brokered"}}}
  ]
}
```

`role` 首期支持 `user`、`assistant`、`system`、`tool`。是否允许调用方传入 `system` 必须由 Agent 和安全策略共同决定。

`type=asset_ref` 的 `asset` 必须是完整 AssetRef，不存在只传 `asset_id` 的第二种 v1 AssetPart 形状。

## 6.3 AssetRef

```json
{
  "asset_id": "asset_01...",
  "name": "product.png",
  "media_type": "image/png",
  "size_bytes": 238291,
  "digest": "sha256:...",
  "access": {
    "mode": "brokered",
    "expires_at": "2026-09-21T08:10:00Z"
  }
}
```

- 长期 URL、Cookie 和对象存储密钥不得进入 AssetRef。
- 实际下载地址应按 Run、Agent、操作和期限动态签发。
- `digest` 对需要完整性验证的资产必须提供。

## 6.4 DataRef 与 SecretRef

DataRef 表示受控业务资源范围；SecretRef 表示通过凭据代理解析的逻辑凭据。Agent 只能在当前 Run Scope 内兑换所需短期授权。

# 7. RunRequest

```json
{
  "protocol_version": "1.0",
  "run_id": "run_01...",
  "attempt_id": "att_019...",
  "agent": {
    "id": "image.generate",
    "version": "1.0.0",
    "skill_id": "default",
    "manifest_digest": "sha256:..."
  },
  "mode": "params",
  "input": {
    "prompt": "生成一张白底商品图"
  },
  "messages": [],
  "assets": [],
  "context": {
    "conversation_ref": "conv_01...",
    "locale": "zh-CN",
    "timezone": "Asia/Shanghai"
  },
  "delivery": {
    "event_batch_url": "https://control.example/v1/agent-runs/run_01/events:batch",
    "event_session_url": "https://control.example/v1/agent-runs/run_01/event-session",
    "response_mode": "stream"
  },
  "deadline_at": "2026-09-21T08:10:00Z",
  "trace": {
    "traceparent": "00-...",
    "tracestate": ""
  }
}
```

约束：

- Header 中必须携带短期 Run Token。
- `run_id`、`attempt_id`、Agent、Deployment 必须与 Token Claims 一致。
- `input` 必须通过 Skill Input Schema。
- Agent 必须先持久化接收状态或幂等记录，再返回 Accepted。
- 同一 `attempt_id` 的重复请求必须返回同一接收结果，不得重复产生副作用。
- Event Token 不由调用方长期持有。RuntimeInstance 使用自己的 Runtime Session 身份和当前 Attempt 信息向 `event_session_url` 换取只写当前 Run Event 的短期 Token。

# 8. Run 与 Attempt 状态

## 8.1 Run 状态

```text
queued -> dispatching -> running <-> waiting_input
queued | dispatching | running | waiting_input -> cancel_requested
任一非终态 -> succeeded | failed | cancelled | timed_out
```

终态不可逆。迟到事件保留审计，但不得覆盖终态。

`cancel_requested` 是非终态；收到 Cancel 不表示执行已停止。终态由第一个通过 State Version 和 Fencing 校验的合法终态事件决定。

未授权请求不创建可执行 Run。拒绝记录属于 Authorization Audit，不进入 Runtime 状态机。

## 8.2 Attempt 状态

```text
created
-> assigned
-> accepted
-> running
-> succeeded | failed | cancelled | expired | fenced
```

Run 可以有多个 Attempt，但同一时刻只允许一个 Attempt 拥有有效执行权，除非明确启用无副作用的 Hedged Execution。

# 9. 事件信封

```json
{
  "specversion": "1.0",
  "id": "evt_01...",
  "source": "https://runtime.example.invalid/instances/instance-01",
  "type": "com.example.agent.output.delta.v1",
  "subject": "runs/run_01",
  "time": "2026-09-21T08:00:01.125Z",
  "datacontenttype": "application/json",
  "dataschema": "https://schemas.example/jinyun/output-delta-v1.json",
  "runid": "run_01...",
  "attemptid": "att_019...",
  "producersequence": 18,
  "traceparent": "00-...",
  "data": {}
}
```

约束：

- `source + id` 唯一标识事件。
- 同一 Attempt 的 `producersequence` 从 1 严格递增。
- Control Plane 接收后分配独立 `run_sequence`。
- 重发必须保持相同 `id` 和相同业务内容。
- 同一事件 ID 内容不一致时返回 `EVENT_ID_CONFLICT`。
- CloudEvents wire 扩展属性固定使用 `runid`、`attemptid`、`producersequence`、`runsequence` 等无下划线小写名称。
- SDK 公开模型使用本语言习惯命名，例如 Go `RunID`、Python `run_id`、TypeScript `runId`；映射必须由同一 Schema Source 生成。
- `com.example` 仅为公共名称冻结前的文档占位命名空间，不得用于正式发布。

# 10. 事件类型

## 10.1 生命周期

- `run.accepted`
- `run.started`
- `run.waiting_input`
- `run.cancel_requested`
- `run.succeeded`
- `run.failed`
- `run.cancelled`
- `run.timed_out`

## 10.2 输出

- `output.started`
- `output.delta`
- `output.snapshot`
- `output.reset`
- `output.completed`

## 10.3 进度与步骤

- `progress.updated`
- `step.started`
- `step.progress`
- `step.completed`
- `step.failed`

## 10.4 资产与用量

- `asset.created`
- `asset.updated`
- `usage.updated`
- `checkpoint.created`

## 10.5 工具、错误与心跳

- `tool.started`
- `tool.completed`
- `tool.failed`
- `error.raised`
- `heartbeat`

`heartbeat` 是 Agent 产生、可持久化的运行事件，与仅用于保持连接的 SSE Comment Heartbeat 不是同一个概念。

终态事件必须包含最终输出快照或可解析的 ResultRef，不允许只依赖历史 Delta 重建最终结果。

# 11. Command

上行控制统一为 Command：

- `run.cancel`
- `input.append`
- `approval.approve`
- `approval.reject`
- `session.close`

Command 具有独立 `command_id` 和幂等语义。Agent 必须返回 Command Accepted/Rejected，并通过事件报告实际状态变化。

同一 Command 载荷和状态语义有两个明确的 HTTP 传输绑定：

- 外部 Caller 请求 Control Plane：`POST /v1/agent-runs/{run_id}/commands`。
- Control Plane 或已授权可信调用方请求 Direct Runtime：`POST /v1/runs/{run_id}/commands`。

`run.cancel` 也使用这两个绑定，v1 禁止额外的 `/cancel` 别名。

收到 Cancel 不等于已经取消；只有 `run.cancelled` 终态表示执行已停止。

# 12. Usage

```json
{
  "meters": [
    {
      "name": "input_tokens",
      "value": 1200,
      "unit": "token",
      "source": "provider_verified"
    }
  ]
}
```

来源：

- `provider_verified`
- `agent_reported`
- `estimated`

Usage 更新应使用累计值或明确的 Delta 类型，避免重放时重复计费。建议默认上报 Attempt 累计值，Control Plane 按 Meter 取最新合法序号。

Protocol v1 默认规则：

- `value` 必须是整数，默认表示当前 Attempt、当前 Meter 的累计值。
- 货币使用 ISO 4217 三字母币种与整数 `amount_micros`，禁止使用浮点金额。
- 来源固定为 `provider_verified`、`agent_reported` 或 `estimated`；兼容 Adapter 可以保留原始来源字段。
- Control Plane 按合法 Sequence 接受最新累计值，不得因事件重放重复累加。
- Run 终态必须携带最终 Usage Snapshot；暂时无法完成对账时标记 `usage_pending`。

# 13. Error

```json
{
  "code": "DEPENDENCY_UNAVAILABLE",
  "category": "dependency",
  "message": "模型服务暂时不可用",
  "retryable": true,
  "retry_after_seconds": 5,
  "details": {},
  "trace_id": "..."
}
```

错误分类：

- `validation`
- `authentication`
- `authorization`
- `not_found`
- `conflict`
- `capacity`
- `dependency`
- `timeout`
- `cancelled`
- `protocol`
- `internal`

`message` 不得包含密钥、完整请求体、内部堆栈和客户敏感数据。机器行为只根据 `code`、`category`、`retryable` 和 `retry_after_seconds` 决定。

# 14. 扩展机制

Manifest、Capability、ContentPart 和 Event 可以通过命名空间扩展。扩展名必须使用组织控制的反向域名或明确前缀。

扩展不得改变核心字段语义。依赖某扩展才能安全执行的 Agent 必须在注册时声明 `required_extensions`，不支持的实例不得进入发现视图。

`required_extensions` 必须是 `extensions` 键的子集。每个 Extension 载荷必须使用包含 `schema_ref`、`schema_digest` 和 `data` 的统一信封；`schema_ref` 只能指向 AgentVersion 发布包内容。扩展载荷必须先校验，其绑定和载荷一起进入 Manifest Digest。

v1 中，Extension Schema 必须是单文档自包含闭包：可以使用同一文档内的 fragment `$ref`，不得继续引用其他包内 Schema 文件。这个限制使一个 `schema_digest` 能完整绑定校验语义，避免只修改间接依赖却不改变 Digest。`schema_digest` 的算法为：用 Manifest 相同的 JSON-compatible YAML/JSON profile 解析 `schema_ref` 指向的完整文件，对语义值执行 RFC 8785 JCS 规范化，再计算 SHA-256，格式为 `sha256:<lowercase-hex>`。`schema_ref` 即使携带 fragment，Digest 也始终覆盖整份 Schema 文档，不只覆盖 fragment。

Core Manifest 可以省略 Governance。但当 Manifest 声明 Enterprise Governance Extension，或发布到受治理 Control Plane 时，必须提供完整 Governance 字段或由发布 API 绑定等价的签名治理元数据。

# 15. HTTP 通用规则

- Content-Type 使用 `application/json`；SSE 使用 `text/event-stream`。
- 所有请求传播 `traceparent` 和可选 `tracestate`。
- 创建类请求使用 `Idempotency-Key`。
- 限流返回 `429` 和 `Retry-After`。
- 临时不可用返回 `503` 和可选 `Retry-After`。
- 乐观并发使用 `ETag` / `If-Match` 或显式 `resource_version`。
- `traceparent` 必须通过 W3C 语义校验：禁止 `ff` 版本、全零 Trace ID、全零 Parent ID，v00 禁止多余字段。
- 未知字段按第 2.2 节的作者/发布与兼容消费双模式处理。
- 生产 Endpoint 必须使用 TLS。

# 16. A2A 映射

建议保持以下可映射关系：

```text
AgentManifest  <-> Agent Card
AgentSkill     <-> Skill
Run            <-> Task
Message        <-> Message
ContentPart    <-> Part
AssetRef       <-> Artifact
RunEvent       <-> StreamResponse / Push Notification
```

Run Token、企业权限、Registry Lease 和 Deployment Routing 属于 AROP 运行治理语义，不要求外部 A2A Agent 原生实现，可由 Adapter 转换。
