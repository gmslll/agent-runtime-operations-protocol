# agent-runtime-operations-protocol 开发规则

本仓库是供应商中立 Agent Runtime Operations 协议和 SDK 的权威来源；金运 AI 中台是首个使用方，不是唯一允许的实现。

## 开始工作前

1. 实现前按顺序完整阅读 `README.md` → `docs/DECISIONS.md` → `docs/IMPLEMENTATION_BLUEPRINT.md` → `docs/DEVELOPMENT_PLAN.md` →任务所属领域规范；目录与制品状态另核对 `spec/artifact-manifest.yaml`。
2. 仅当仓库位于金运内部工作区且上层文件存在时，再阅读上层 `AGENTS.md`、`CLAUDE.md`、`BOT_DEPLOYMENT.md` 和 `AI中台/` 文档；公共独立克隆不得依赖这些内部文件。
3. 涉及历史结论或“为什么这样设计”时，按工作区 `jinyun-memory-brain` 规则检索本工作区会话。
4. 开始实现前检查 [docs/DECISIONS.md](docs/DECISIONS.md) 中的已确认决策和发布配置待填写项。
5. P04 用户 Gate 通过前不得进行 P05 物理重构或新协议行为实现；实施时不得跳过 `docs/DEVELOPMENT_PLAN.md` 的阶段依赖。
6. P03/P04/P45/P50/P52 原始外部证据不入 Git；只允许提交脱敏的 canonical 内容摘要、可选验签 attestation 和机器报告。未验签的 SHA-256 不得称为“签名摘要”，工具不得自动生成“已批准”或“独立”证据。

## 固定边界

1. 本仓库不得依赖 `kinglucky-agent-console`、飞书、cc-connect、Teable 或具体 Agent 框架。
2. Console 可以生成或引用本仓库类型，不得在 Console 仓复制维护第二份 Runtime Schema。
3. AgentDefinition、AgentVersion、RuntimeService、RuntimeInstance、Run 和 Attempt 必须保持独立概念。
4. 所有新调用必须经过 Control Plane 授权和创建 Run；获得短期 Dispatch Ticket 后，可信调用方可以按交付模式直接请求 Agent。
5. 支持 `direct`、`proxy` 和 `worker_pull` 三种交付模式，不得假设所有业务流量都经过 Console。
6. 大文件只传 `AssetRef`，不得把文件本体、Cookie、密钥或长期签名 URL 写入协议事件。
7. 交付语义为至少一次；所有写操作和事件消费必须具备幂等语义。
8. Protocol v1 必须支持结构化流式事件和断线恢复。
9. 注册发现采用自研实现；可以借鉴 etcd/Nacos 语义，但不得把它们变成本协议依赖。
10. 第一阶段跨进程使用 HTTP/JSON、SSE 和 HTTP Event Batch，不主动引入 gRPC。
11. 协议不得重定义 MCP、A2A、ARD/AI Catalog、CloudEvents 或 OpenTelemetry 已负责的语义。
12. Runtime Registry 负责管理域内实例路由；跨域公开发现通过 ARD/AI Catalog 和 A2A Agent Card 互操作。
13. 核心协议不得出现飞书、金运 Console、cc-connect 或具体框架的专属字段；专属能力只能作为带命名空间的扩展。
14. 第三方必须能够在不安装金运 Console 的情况下运行 Schema 校验、Quickstart 和 Conformance。

## 公共项目规则

- 公共项目名称固定为 Agent Runtime Operations Protocol，仓库名为 `agent-runtime-operations-protocol`，简称 AROP。
- 许可证、公共包名和治理方式以 `docs/DECISIONS.md` 为准；真实 GitHub URL、域名和包所有权填写前不得发布稳定包。
- 不得使用已经存在并可能混淆的 `Agent Runtime Protocol` / `ARP` 名称。
- 规范按 Core、Runtime Management、Delivery Profile、Streaming Extension 和 Governance Extension 分层。
- “兼容”必须附带 Conformance 等级和测试报告，不得只做宣传性声明。
- 金运专属扩展与第三方扩展遵守同一注册、Schema、版本和安全规则。

## Schema 规则

- JSON Schema 是数据结构权威来源。
- OpenAPI 描述同步 HTTP 接口；AsyncAPI 描述事件和流式消息。
- SDK 类型应从同一份 Schema 生成，不得手工维护语义重复的多语言结构。
- 发布的 Schema 不得原地修改语义；破坏性变化必须升级协议主版本。
- 新增可选字段属于兼容变化；消费者必须忽略未知字段。
- 所有 ID、枚举、时间、错误码和状态迁移必须有黄金样例与反向测试。

## 安全规则

- 不提交真实密钥、Token、Cookie、验证码或客户数据。
- 示例 Token 必须明显不可用。
- Run Token 必须绑定 Run、Attempt、Agent、Deployment、Audience、Scope 和过期时间。
- 注册凭据、Run Token、Asset Token 和用户会话 Token 不得混用。
- Agent Endpoint、Callback URL 和 Asset URL 必须考虑 SSRF、DNS 重绑定和重放风险。

## 变更要求

- 协议字段变化必须同步更新 Schema、OpenAPI/AsyncAPI、样例、SDK 和契约测试。
- 架构决定变化必须同步更新 `docs/DECISIONS.md` 和相关设计文档。
- 实现必须遵守 `docs/DECISIONS.md` 已冻结的 wire 语义；发布配置未填写时只能使用明确占位符，不得擅自声明永久公共命名空间。
