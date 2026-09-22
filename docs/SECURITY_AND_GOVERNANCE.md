---
title: 安全、权限与企业治理
status: draft
updated: 2026-09-22
---

# 1. 安全目标

- Agent 不能绕过 Control Plane 接受未授权的新业务调用。
- 调用方只能使用被授权的 Agent、Skill、Deployment 和资源。
- RuntimeInstance 不能冒充未授权 AgentVersion。
- Token 泄露后的影响范围被限制到单个 Run、Attempt、Audience 和短时间窗口。
- 事件、结果和 Usage 可追踪且不可被旧实例覆盖。
- 密钥、用户会话、文件授权和 Agent 运行凭据彼此隔离。
- 高风险 Agent 具备额外确认、幂等和审计机制。

# 2. 身份模型

必须区分以下身份：

| 身份 | 示例 |
| --- | --- |
| Human Subject | Web 用户、飞书用户 |
| Calling Client | Web BFF、Bot Gateway、后台服务 |
| Publisher | 创建 AgentVersion 的开发者或团队 |
| RuntimeService | Python 服务、通用 HTTP 或 Worker Pull Bridge |
| RuntimeInstance | 某台机器上的具体进程 Session |
| Approver | 对高风险操作进行批准的人 |

一个 Run 可以同时记录 Original Subject、Calling Client、Approver 和 RuntimeInstance，不能只保存一个 `user_id`。

# 3. Credential 与 Token 类型

| 类型 | 用途 | 生命周期 |
| --- | --- | --- |
| User Session | 访问 Web/Control Plane | 用户会话 |
| Service Credential | Gateway/BFF 调用 Control Plane | 服务级，可轮换 |
| Publisher Credential | 发布 AgentVersion | 人员/CI 级 |
| Deployment Credential | 注册 RuntimeService/Instance | 服务级，限定 Agent 范围 |
| Worker Session Token | Worker Claim 与续租 | 单次进程 Session |
| Run Token | 执行单个 Attempt | 短期、Audience 绑定 |
| Event Token | 上报指定 Run 事件 | 短期、只写事件 |
| Asset Token | 读取/写入指定 Asset | 极短期、操作绑定 |
| Secret Exchange Token | 兑换受控业务凭据 | 极短期、资源绑定 |

这些 Token 禁止混用。

RuntimeInstance 接收 Run 后，使用自己的 Runtime Session Token、Deployment、Attempt 和 Fencing 信息向 Control Plane 换取 Event Token。调用方只负责转交 RunRequest 和 Run Token，不负责保管或刷新 Event Token。

长任务中 Run Token 可以在 Agent 成功接收后过期；它用于授权“开始这次 Attempt”。后续事件上传、命令和资产访问分别使用独立短期 Token。

# 4. Run Token

Run Token 默认使用 ES256 非对称签名。Control Plane 必须通过 HTTPS JWKS 发布验证公钥，并以 `kid` 支持密钥轮换；新旧验证密钥的可用窗口至少重叠两个最大 Token 生命周期。Run Token 生命周期默认 1～5 分钟，具体值由服务端策略返回。

禁止接受 `alg=none`，禁止根据 Token 自带字段动态放宽算法白名单，也禁止让多个互不信任 Runtime 共享同一个对称 HMAC 密钥。高安全 Profile 可以额外要求 mTLS 或 sender-constrained token，但它们不是本地开发和普通 v1 接入的前置条件。

建议 Claims：

```json
{
  "iss": "https://control.example",
  "aud": "https://control.example.invalid/deployments/dep_01",
  "sub": "user_123",
  "azp": "bot-gateway",
  "jti": "token_01...",
  "run_id": "run_01...",
  "attempt_id": "att_019...",
  "agent_id": "image.generate",
  "agent_version": "1.0.0",
  "skill_id": "default",
  "deployment_id": "dep_01...",
  "fencing_token": 43,
  "channel": "feishu_bot",
  "scope": ["agent:invoke", "run:stream"],
  "iat": 1790000000,
  "nbf": 1790000000,
  "exp": 1790000300
}
```

Agent 必须校验：

- 签名和允许算法。
- Issuer、Audience、有效期和 Not Before。
- Run、Attempt、Agent、Version、Skill、Deployment 是否与请求一致。
- Scope 是否覆盖当前操作。
- Fencing Token 是否仍有效。
- Token ID 是否已撤销或已完成消费（适用于一次性 Token）。

授权可以作用于 AgentDefinition 或具体 Skill。未配置 Skill 规则时继承 Agent 级结果；显式 Skill Deny、高风险确认和更严格 Scope 优先于继承的 Agent Allow。Control Plane 创建 Run 和 Agent 接收 Run 时都必须校验最终 `skill_id`。

# 5. Direct 模式安全

Direct 模式扩大了 Caller 与 Agent 的信任边界，必须增加：

- Endpoint 只能来自已审核注册表。
- Registry 对 URL Scheme、Host、Port、DNS 和证书进行验证。
- 防止注册 `localhost`、云元数据地址或未授权内网地址造成 SSRF。
- Run Token 绑定具体 Deployment Audience。
- Caller 不得把 Token 写入 URL、日志、埋点或错误报告。
- Protocol v1 不允许浏览器直接持有 Run Token 请求 Runtime；浏览器必须通过自身 BFF 或 Control Plane Proxy。
- Bot Gateway、服务器后端和编排器属于可信服务调用方，可以在获得 Dispatch Ticket 后使用 Direct。
- 后续 Browser Direct Extension 必须使用一次性短期 Token、严格 CORS Origin 白名单，并禁止暴露内网 Endpoint。
- 高安全环境可以要求 mTLS 或公钥绑定 Token。

Publisher 提交的 Manifest、Schema 和 Extension `$ref` 必须由离线包解析器处理，校验器禁止因为这些引用发起 HTTP 或 DNS 请求。Runtime Endpoint、Callback URL 和 Asset URL 属于另一类受控网络资源，必须在每次连接及重定向后重新校验 Scheme、Host、Port 和解析 IP，阻止 Loopback、Link-local、云元数据地址、未授权内网段及 DNS Rebinding。

调用方不得根据 Discovery Cache 自行切换实例；切换必须由 Control Plane 创建新 Attempt。

# 6. 发布与所有权治理

AgentDefinition 必须记录：

```text
owner_user_id
owner_team_id
maintainers
visibility
maturity
risk_level
data_classification
support_contact
```

这是受治理发布的必填条件，不表示 Core Manifest 在本地实验模式下必须内联 Governance。Control Plane 可以接受 Manifest 中的 Governance Extension，也可以在发布 API 中绑定等价的签名元数据；两者都必须可审计并绑定到确定 AgentVersion。

建议值：

```text
visibility: private | team | company
maturity: experimental | verified | production
risk_level: low | medium | high | critical
```

生命周期：

```text
draft -> testing -> review -> published -> deprecated -> retired
```

- 个人创建默认 `private + experimental`。
- 公司级能力必须通过契约、安全和所有权审核。
- 离职、团队解散或长期无人维护时必须转移或下线。
- Published Version 内容不可变；修改必须发布新版本。

# 7. 副作用与人工确认

高风险操作至少满足：

- Manifest 声明 `write` 或 `irreversible`。
- Control Plane 在 Run 创建时检查操作权限。
- Agent 在执行副作用前使用稳定 `effect_id`。
- 需要人工确认时进入 `waiting_input`。
- Approval Token 绑定 Run、具体操作、批准人和有效期。
- 批准前后的参数摘要必须一致，参数变化使批准失效。

发送消息、修改客户资料、下单、退款等不能只依赖自然语言中的“用户已经同意”。

# 8. Asset、Data 与 Secret

## 8.1 Asset

- 上传、下载分别授权。
- Token 绑定 `run_id + asset_id + operation`。
- 限制大小、媒体类型、次数和有效期。
- 可执行文件和外部输入根据策略进行恶意内容扫描。
- 长期保存前明确归属和保留策略。

## 8.2 DataRef

DataRef 必须表达最小资源范围，例如客户集合、日期范围、字段范围和允许动作。Agent 不得通过一个 DataRef 获得整个业务数据库权限。

## 8.3 SecretRef

- Manifest 只声明需要哪类凭据，不包含凭据内容。
- Runtime 使用 Secret Exchange Token 获取短期凭据。
- 凭据访问写入审计。
- Secret 不进入 Run Event、错误、Trace 和长期结果。

# 9. 数据分类与保留

每个 AgentVersion 应声明可处理的数据等级和允许输出位置。

建议至少区分：

```text
public
internal
confidential
restricted
```

需要分别制定：

- 输入正文是否保存。
- 流式 Delta 是否完整保存。
- 最终结果保存期限。
- Trace 是否采样。
- 审计记录保留期限。
- 用户删除或法务删除请求的处理。

“可追踪”不等于永久保存所有 Prompt、文件和模型输出。

# 10. 事件与审计完整性

- Event 使用稳定 ID、Source 和 Digest。
- Control Plane Ledger 追加写，不原地改历史。
- Runtime 与管理员操作记录操作者和资源版本。
- 高风险环境可以对事件批次做签名或 Hash Chain。
- 审计查询必须区分业务状态、运行日志和敏感内容。

# 11. Channel 安全

Web、Bot、API 渠道分别授权：

- Agent 在 Web 可见不代表 Bot 可用。
- 群权限不自动扩展到私聊。
- Bot 结果可见范围与调用权限分开。
- 外部群、外部用户和跨组织场景默认拒绝。
- Agent 不接收飞书 App Secret、用户 Cookie 或完整通讯录。

# 12. 配额、预算和滥用防护

Control Plane 应支持：

- 用户、部门、Agent、Channel 速率限制。
- 每 Run Token/金额/时长上限。
- 每日和每月预算。
- 并发 Run 限制。
- 大文件和事件速率限制。
- 异常重试和循环调用检测。
- Agent-to-Agent 调用深度限制。

# 13. 错误与日志脱敏

禁止写入日志：

- Bearer Token。
- Cookie、验证码和密码。
- 文件签名 URL 查询串。
- 完整 SecretRef 解析结果。
- 用户敏感输入全文。
- 模型 Provider Key。

协议错误返回稳定错误码、Trace ID 和可安全展示的信息；内部堆栈只进入受控诊断系统。

# 14. 威胁场景清单

契约和实现至少覆盖：

- 伪造 Manifest 或 Digest。
- 未授权 Runtime 注册已有 Agent。
- 旧实例复活并提交结果。
- Run Token 跨 Agent、跨 Deployment 使用。
- Callback/Event 重放和篡改。
- 恶意 Endpoint 引发 SSRF。
- Asset URL 泄露和越权读取。
- Prompt Injection 诱导 Agent 泄露 Secret。
- 高风险操作重复执行。
- 浏览器 Token 被第三方脚本窃取。
- Discovery Cache 使用已过期实例。
- Agent 伪报 Usage 或健康状态。
