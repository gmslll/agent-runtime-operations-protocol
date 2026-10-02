# Agent Runtime Operations Protocol

`agent-runtime-operations-protocol` 是供应商中立的开放 Agent 运行与运维互操作协议仓库，简称 AROP。

它解决的不是某一种 Agent 框架如何运行，而是不同企业平台中的 Agent 如何被标准化发布、注册、授权调用、可靠调度、流式通信、追踪、审计、升级和下线。

本项目是 Agent 运行与运维互操作层，不替代 MCP、A2A、ARD/AI Catalog、CloudEvents 或 OpenTelemetry。

## 设计目标

- 任意语言、框架和部署位置的 Agent 使用同一套接入契约。
- Web、飞书 Bot、后台服务和后续渠道调用同一个 AgentDefinition。
- 每次新调用必须先经过 Control Plane 授权并创建 Run。
- 获得短期 Dispatch Ticket 后，可信调用方可以直接请求 Agent，业务流量不必全部经过 Console。
- 支持 Direct HTTP、Control Plane Proxy 和内网 Worker Pull 三种交付方式。
- Protocol v1 原生支持结构化流式事件、断线恢复、幂等、租约、重试和全链路追踪。
- 注册发现借鉴 etcd/Nacos 的 Lease、Revision、Watch、CAS 和健康视图思想，但由金运自行实现，不依赖 etcd 或 Nacos。
- 协议仓不依赖 Console、飞书、cc-connect、Teable 或具体 Agent 框架。
- 任意第三方可以独立实现兼容 Control Plane 或 Runtime，不需要安装金运 Console。
- A2A 用于 Agent 间互操作，MCP 用于工具与资源接入，ARD/AI Catalog 用于跨域公开发现。
- 协议按 Core、Runtime Management、Delivery、Streaming 和 Governance 分层，接入方不必实现全部能力。

## 仓库交付物

计划交付：

```text
JSON Schema
OpenAPI
AsyncAPI
协议黄金样例
兼容性规则
Go / Python / TypeScript SDK
CLI 与脚手架
参考 Agent 与 Worker Adapter
轻量 Reference Control Plane
A2A / MCP / ARD 互操作 Adapter
提供方和消费方契约测试
```

## 文档导航

建议按以下顺序阅读：

1. [总体架构](docs/ARCHITECTURE.md)
2. [协议规范](docs/PROTOCOL_SPECIFICATION.md)
3. [注册与发现](docs/REGISTRY_AND_DISCOVERY.md)
4. [Run、事件与流式通信](docs/RUN_AND_STREAMING.md)
5. [安全与治理](docs/SECURITY_AND_GOVERNANCE.md)
6. [可靠性与运维](docs/RELIABILITY_AND_OPERATIONS.md)
7. [SDK 与开发者体验](docs/SDK_AND_DX.md)
8. [外部标准互操作](docs/INTEROPERABILITY.md)
9. [公共项目与采用策略](docs/PUBLIC_PROJECT_AND_ADOPTION.md)
10. [最终仓库布局](docs/DIRECTORY_STRUCTURE.md)
11. [实施蓝图](docs/IMPLEMENTATION_BLUEPRINT.md)
12. [P01–P53 可调度开发与发布计划](docs/DEVELOPMENT_PLAN.md)
13. [架构决策与发布配置](docs/DECISIONS.md)
14. [机器可读制品目录](spec/artifact-manifest.yaml)
15. [不可变需求映射](spec/requirements.yaml)
16. [冲突决议台账](spec/conflicts.yaml)

## 规范权威链

AROP 的权威顺序为：`DECISIONS` 约束 → 领域规范与状态机 Fixture 定义行为 → JSON Schema 定义结构 → OpenAPI/AsyncAPI 定义传输绑定。SDK 生成模型、参考实现和生成文档都是派生物，不得反向定义规范。

[`spec/artifact-manifest.yaml`](spec/artifact-manifest.yaml) 是当前和规划协议制品的唯一机器可读目录。目录规划文档只是人类可读视图，不维护第二份制品清单。

## 与 Console 的边界

本协议项目负责稳定、实现无关的契约、SDK、Conformance 和 Reference Implementation。

`kinglucky-agent-console` 负责：

- 身份、组织和权限。
- Agent Catalog 与发布审核。
- Run、Dispatcher、注册表和发现服务的实现。
- 用量、审计和渠道接入。
- Web 用户工作台和管理后台。

协议仓定义任意 Control Plane 与 Agent、Worker、可信调用方之间如何通信，但不实现 Console 的业务规则和数据库。金运 Console 是一个实现，不是协议正确性的唯一来源。

公共 Reference Control Plane 和服务端组件使用 Go 实现。仓库中的 Node.js 依赖只用于 JSON Schema、Fixture 和文档校验，不属于后端运行时；Python 用于 Agent Provider SDK 和参考 Agent。

## 当前状态

当前是 **Protocol v1 规划候选**：P01 权威/元数据基线与 P02 可调度蓝图候选已完成，正在等待 P03 重新独立审计和 P04 用户明确确认；此前不执行目录重构或新协议行为实现。现有首批 Schema、Fixture 和 Go 基线仅是起点，不表示规划已冻结或 v1 已可发布。

项目域名、包注册表所有权和初始维护者名单仍属 P45 真实外部 Gate。P44 及之前均为 private/dev snapshot；取消独立公共 v0.1，首个公开候选是 P49 从 clean source commit A 发布的 `v1.0.0-rc.N`。

源码公开托管于 canonical 仓库 [`gmslll/agent-runtime-operations-protocol`](https://github.com/gmslll/agent-runtime-operations-protocol)（2026-10-03 起转为公开）。发布证据的信任身份固定绑定该仓库；凡携带历史身份的仓库外信任制品（role registry bundle、external config、已签发 envelope）会被验证器拒绝，必须以 canonical 身份重新签发。

## 当前验证

```bash
npm ci --ignore-scripts --omit=optional --no-audit --no-fund
make spec-index-check
make blueprint-check
make validate
```

P07 将以仓库内自有、确定性的 Schema-driven pipeline 生成 Go/Python/TypeScript 代表模型。TypeScript 编译器固定为 `5.9.3`，Python 严格类型检查器固定为 Pyright `1.1.414`，npm 版本声明为 `10.9.2`；版本与完整性信息由 `package-lock.json` 绑定，不调用全局 `tsc`、`npx` 或浮动版本。依赖安装完成后，生成、编译、类型检查与 drift 验证必须全程离线；若要求空机从零开始也能 air-gap 运行，还需另外提供与 lock Digest 绑定的受控 npm 缓存或工具链制品。

P07 的计划验收入口为：

```bash
make test-codegen-pipeline
```

只有该命令产生的 `build/reports/P07/report.json` 和 `junit.xml` 通过独立报告复验后，才能声明 P07 完成；本节的工具版本和命令不构成提前验收。

`make spec-index-check` 校验权威链、机器目录/DAG、14 条不可变需求、冲突、Decision ID 和本地引用闭包。`make blueprint-check` 动态校验双 Go Module、最终布局、连续阶段 DAG、baseline future ownership、关键制品 owner/phase/test/exposure、implement report closure、聚合报告精确 fan-in、跨生态版本、detached evidence 与 commit A→metadata-only B 发布链。`make test-evidence-lineage` 以真实临时 Git 历史验证 planning blob 的 ancestor/current/changed→reverted/deleted 负例。全部由 `make validate` 强制执行。

`make verify-report REPORT=build/reports/P01/report.json` 使用独立验证器重新核对报告 Schema、JSON/JUnit testcase 与 failure 数、HEAD、Checker Digest 及全部输入 Digest。P01/P02 报告还必须携带完整 Git tracked tree 的 path/type/mode/digest/bytes manifest；验证器不信任报告自报清单，会对 current index+worktree 或 ancestor Git tree 独立重发现，并拒绝任何 tracked/worktree symlink。默认模式要求报告 claimed HEAD 等于当前 HEAD，并如实核对当前 dirty 状态；`make verify-report REPORT=... ALLOW_ANCESTOR=1` 可显式核对祖先 commit 的 clean 报告存档完整性，此时 Checker 和每个输入都从 `git show <claimed>:<path>` 重算。该模式的通过不等于历史 success 可被聚合；P41 必须再在隔离 checkout 重跑受控 checker，或验证绑定完整来源的受信 CI/OIDC/Sigstore provenance。当前对应输入或 Checker 有任何变化时必须重跑，不得复用。

`make test-report-verifier` 在隔离的临时 Git clone 中机器测试当前报告、错误 Digest、显式 ancestor 验证、未授权 ancestor、不存在 commit 与非祖先 commit；`make validate` 会强制执行该组测试。

P03/P04 另使用仓库外证据：`make planning-audit EVIDENCE=... TRUSTED_KEYS=...` 和 `make gate-check GATE=P04 EVIDENCE=... TRUSTED_KEYS=... P03_EVIDENCE=... P03_TRUSTED_KEYS=...`；任一角色也可分别用 `TRUSTED_CHANNEL_CONFIRMATION` / `P03_TRUSTED_CHANNEL_CONFIRMATION` 引用仓库外可信渠道记录。P04 会重新 strict/schema 解析 P03 原始 evidence 与 reviewer trust/confirmation、重跑验签，再交叉核对 P03 raw/trust/report/promoted envelope；owner subject 必须同时绑定 P03 canonical content digest 和 promoted envelope 的 JCS/file digest。证据的 `summary_sha256` 是对明确字段按 RFC 8785/JCS 规范化后的内容绑定，不是签名。只有受信 key registry 中角色为 `independent_reviewer`/`project_owner` 的 Ed25519 验签，或人工可信渠道 Gate，才能建立审核者/批准者真实性。工具只验证内容和证据链，不会从布尔字段推断“独立”或“已批准”。
P03/P04 外部证据的 subject commit 可以是当前 HEAD 或其祖先；只有 requirements/plan/blueprint/制品目录等受审输入 Digest 与当前仓库仍完全相同时，才能在新 HEAD 重新生成 P03/P04 机器报告，无需让用户对未变的计划重复批准。P04 绑定 P03 稳定 canonical summary Digest，不绑定会因重生成而变化的报告文件 Digest。

不可变需求的权威原文是 [`spec/requirements.yaml`](spec/requirements.yaml) 中的 `statement_original_zh`；`translation_en` 只是非权威翻译。检查器内的固定原文、整组内容摘要和反例只用于防止仓库漂移，不能替代 P03 的独立计划审计；独立审计仍是最终把关。报告会记录精确 HEAD、dirty 状态、实际命令、Node/Go/OS、输入 Digest、Checker Digest 和 Testcase 数。

计算 Manifest Digest：

```bash
make manifest-digest FILE=examples/manifests/valid/minimal.yaml
```

Manifest 必须先通过包内 Schema 闭包和 Extension 载荷校验，才会输出 Digest。Publisher Schema 固定为 AROP 的 Draft 2020-12 可移植子集：`format` 仅支持严格 RFC 3339 `date-time`；`pattern` 仅支持锚定首尾、固定长度的安全 ASCII 子集；`patternProperties` 和 `multipleOf` 禁止使用；Manifest、Publisher Schema 和 Extension data 递归禁止 `__proto__`、`prototype`、`constructor` 对象键。Extension 信封的 `schema_digest` 对 `schema_ref` 指向的完整 Schema 文档按 JSON-compatible YAML/JSON profile 解析，再用 RFC 8785 JCS + SHA-256 生成；v1 Extension Schema 只允许文档内 fragment `$ref`，不允许跨文件依赖。完整示例见 `examples/manifests/valid/complete.yaml` 及其 `schemas/audit-extension.schema`。

当前 Schema `$id` 使用保留域名 `arop.invalid`，仅供预发布阶段稳定本地引用；项目域名确定后会在首个稳定公开版本前一次性迁移。

文档中的标记含义：

- **已确认**：本阶段可作为实现依据。
- **实现默认值**：可以通过兼容配置调整，调整时必须更新文档和契约测试。
- **发布配置待填写**：设计没有分歧，但必须填入真实账户、域名或维护者信息后才能公开发布。
