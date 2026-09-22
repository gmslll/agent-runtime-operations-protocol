---
title: Protocol v1 架构决策与发布配置
status: active
updated: 2026-09-23
---

# 1. 已确认方向

| 编号 | 决策 | 结论 |
| --- | --- | --- |
| D-001 | 仓库职责 | Protocol 保存稳定契约、Schema、样例、SDK 和契约测试 |
| D-002 | 框架关系 | 协议与编程语言、Agent 框架、桌面执行器和具体产品实现无关 |
| D-003 | 注册中心 | 借鉴 etcd/Nacos 语义，由金运自行实现，不依赖 etcd/Nacos |
| D-004 | 控制面 | 所有新调用必须经过 Control Plane 授权和创建 Run |
| D-005 | 数据面 | 获得 Dispatch Ticket 后，可信调用方可直接请求 Agent |
| D-006 | 交付模式 | 支持 Direct、Proxy 和 Worker Pull |
| D-007 | 流式 | Protocol v1 原生支持结构化流式事件 |
| D-008 | 第一阶段传输 | HTTP/JSON、SSE、HTTP Event Batch，不主动引入 gRPC |
| D-009 | 可靠性 | 至少一次交付；通过幂等、Attempt 和 Fencing 保证正确性 |
| D-010 | 大文件 | 文件本体走对象存储，协议只传 AssetRef |
| D-011 | 追踪 | 所有调用和事件传播 W3C Trace Context |
| D-012 | Web/Bot | Web 与飞书 Bot 是同一 Agent 的渠道，不重复注册 Agent |
| D-013 | SDK 位置 | 协议 SDK 位于 Protocol 仓，不放入 Console |
| D-014 | 公共定位 | 项目按供应商中立的开放 Agent Runtime Operations 协议设计，金运只是首个实现方 |
| D-015 | 项目名称 | 公共名称为 Agent Runtime Operations Protocol，简称 AROP；仓库名为 `agent-runtime-operations-protocol` |
| D-016 | 生态关系 | 补充而不替代 MCP、A2A、ARD/AI Catalog、CloudEvents 和 OpenTelemetry |
| D-017 | 协议分层 | 采用 Core、Runtime Management、Delivery Profile、Streaming 和 Governance Extension |
| D-018 | 独立实现 | 第三方不安装金运 Console 也必须能够运行 Quickstart、SDK 和 Conformance |
| D-019 | 发现边界 | Runtime Registry 管理域内实例；跨域资源发现通过 ARD/AI Catalog 和 A2A 互操作 |
| D-020 | ID 规范 | 人类可读的 `agent_id`/`skill_id` 使用稳定 slug；系统资源 ID 使用资源前缀加 UUIDv7；`instance_id` 稳定持久化，`session_id` 每次启动重建 |
| D-021 | Delta Offset | 文本 Delta 的 `offset` 固定表示当前完整文本的 UTF-8 字节偏移；修改历史输出必须使用 Snapshot 或 Reset |
| D-022 | 事件字段命名 | CloudEvents wire 扩展属性使用无下划线小写名；各语言 SDK 使用本语言惯用命名并由 Schema 生成固定映射 |
| D-023 | Manifest Digest | Manifest 先校验，再按 RFC 8785 JCS 规范化为 JSON，排除 Digest/签名字段后计算 SHA-256，格式为 `sha256:<lowercase-hex>` |
| D-024 | Capability 标识 | Core 保持最小；Direct、Proxy、Worker Pull 等使用标准 Profile；第三方 Extension 使用反向域名命名空间 |
| D-025 | Lease 默认值 | Runtime Lease 默认 TTL 30 秒、Keepalive 10 秒并带随机抖动；服务端返回实际参数；到期立即退出路由但保留历史审计 |
| D-026 | Token 与密钥 | Run Token 默认使用 ES256 非对称签名和 JWKS 发现，生命周期 1～5 分钟；密钥轮换至少重叠两个 Token 生命周期；mTLS 为高安全 Profile |
| D-027 | 浏览器 Direct | v1 浏览器只能通过 BFF/Proxy；可信 Bot Gateway、后端和编排器可 Direct；浏览器直连以后作为独立 Extension |
| D-028 | Inbox/Outbox | 生产级 Provider 必须使用 Durable Inbox/Outbox；内存实现仅限实验等级；`write/irreversible` 副作用必须有稳定 `effect_id` |
| D-029 | Skill 权限 | v1 正式支持 AgentDefinition 与 Skill 两级授权；简单 Agent 可只配置 Agent 级权限；Skill 级 Deny/高风险约束优先 |
| D-030 | Usage | Meter 使用整数累计值；货币使用 ISO 4217 币种和整数 `amount_micros`；终态携带最终 Usage Snapshot，并以 `provider_verified/agent_reported/estimated` 标记来源可信度 |
| D-031 | 数据保留 | 保留期由部署策略配置并由服务端公布；默认 Registry Event 24 小时、Run Delta 24 小时、Final Result 90 天、Audit 180 天 |
| D-032 | Reference Control Plane | 提供最小独立实现，覆盖开发身份、Manifest、Registry、Run/Dispatch、Event/SSE、查询/取消和 Conformance；不复制 Console 业务能力 |
| D-033 | Reference 存储 | Quickstart 默认单进程 SQLite；生产/多节点参考部署使用 PostgreSQL；两者必须通过相同协议 Fixture |
| D-034 | A2A 兼容 | 目标协议线为 A2A 1.0，首批 Fixture 固定到 v1.0.1；映射必须标记 `exact/extended/lossy/unsupported` 并保留原始对象引用 |
| D-035 | SDK 与 CLI 命名 | Go Module 使用 `github.com/gmslll/agent-runtime-operations-protocol`；Python Distribution 使用 `arop-sdk`、Import 使用 `arop`；npm 使用 `@arop/sdk`；CLI 使用 `arop` |
| D-036 | 开源许可证 | 协议文本、Schema、SDK、Conformance 和 Reference Implementation 统一采用 Apache-2.0 |
| D-037 | 公共治理 | 规范性变更走公开 RFC 并需两名 Maintainer 审核；初期使用 DCO，不使用 CLA；安全漏洞走私密报告渠道 |
| D-038 | 仓库归属 | 初期 GitHub 仓库为私有的 `gmslll/agent-runtime-operations-protocol`；取消独立公共 v0.1，C-002～C-004 完成后、首个公开 `v1.0.0-rc.N` 前转为公开；未来迁移组织应保留 GitHub Redirect 与 Go Module 兼容策略 |
| D-039 | 发布安全 | GitHub 强制 2FA 和分支保护；正式包使用 OIDC Trusted Publishing；Release 使用签名 Tag；发布与恢复权限至少由两人持有 |
| D-040 | 公共命名空间 | Schema `$id`、CloudEvents Type Prefix 和 Extension Namespace 使用项目控制域名；域名确定前保留占位符且不得发布稳定包 |
| D-041 | 后端语言 | Reference Control Plane、Registry、Dispatcher、Run/Event Ledger 和服务端 Conformance Harness 使用 Go 实现；Python 主要用于 Agent Provider SDK，TypeScript 主要用于 Web/BFF Consumer |
| D-042 | 权威来源链 | Decision 约束语义；领域规范和状态机 Fixture 是行为权威；JSON Schema 是结构权威；OpenAPI/AsyncAPI 只绑定传输；生成物均非权威 |
| D-043 | 未知字段兼容 | 发布和作者校验对其声明版本严格拒绝未知字段；同一主版本的兼容消费者必须保留或忽略未知可选字段，安全相关未知语义不得默认放行 |
| D-044 | Schema 封装与引用 | AgentVersion 发布包的 Schema 只能内联或使用包内相对引用；服务端不得为校验 Publisher 提交的 Schema 访问网络；必须校验引用闭包和 Digest |
| D-045 | Extension 绑定 | `required_extensions` 必须是 `extensions` 键的子集；每个扩展载荷必须绑定包内 Schema 及 Digest，Manifest Digest 覆盖该绑定与载荷 |
| D-046 | Governance 分层 | Core Manifest 允许省略 Governance；声明 Governance Extension 或发布到受治理 Control Plane 时必须提供完整治理元数据 |
| D-047 | ContentPart 扩展 | Core ContentPart 保持闭合；非核心内容统一使用 `type=extension` 包装并携带 `extension_id`、Schema Digest 和 `data`，不直接新增未协商的 `type` |
| D-048 | Trace Context 校验 | `traceparent` 除结构匹配外必须遵循 W3C 语义：禁止 `ff` 版本、全零 Trace/Parent ID，v00 禁止多余字段 |
| D-049 | v1 HTTP 规范路径 | Worker Claim 使用 `/v1/workers/{worker_id}/claims:next`；Event Batch 使用 `/v1/agent-runs/{run_id}/events:batch` 并以 `event_batch_url` 传递；同一 `run.cancel` Command 在 Caller→Control Plane 绑定 `/v1/agent-runs/{run_id}/commands`，在 Control Plane/已授权可信调用方→Direct Runtime 绑定 `/v1/runs/{run_id}/commands`，禁止 `/cancel` 别名 |
| D-050 | v1 运行命名与分类 | `session_id` 值前缀固定为 `ses_`，`attempt_id` 固定为 `att_`；Run 状态机包含非终态 `cancel_requested`；v1 标准事件补齐 Tool、Error 和 Agent Heartbeat |
| D-051 | Asset ContentPart | `type=asset_ref` 的 ContentPart 在 `asset` 中携带完整 AssetRef；仅有 `asset_id` 的简写不是合法 v1 ContentPart |
| D-052 | 外部证据 Gate | 域名、包所有权、维护者/安全入口和独立实现/外部伙伴均是发布 Gate；没有可核验外部证据时不得标记完成或用内部模拟替代 |
| D-053 | v1 两提交证据与发布闭环 | P42 实现并冻结唯一 public namespace/version generator，P43 用保留域名/临时 registry 验证；P45 后禁止 implement/refactor；P46 仅把已验公共配置交给该工具生成 RC tree，不创建 commit/tag；P47 创建 clean commit A；P48 只读验证；P49 从 A 发布 RC 并产生 detached RC subject manifest；P50 只验证外部 principals 已签 compatibility bundle，不改 A；P51 生成 unsigned deterministic final overlay，白名单精确覆盖版本/package/lock/nested go.mod/checksum；P52 只验证外部 `release_approver` attestation，绑定 A/tree/overlay/expected-B/version-policy/bound-evidence/bridged-reports；P53 只应用已批准 overlay 创建直接单父 `parent=A` 的 metadata-only B，分生态发布 final，在 B 重跑 Conformance/SBOM/provenance 并产生不进入 B tree 的 final detached evidence；Schema/API/Runner/生成模型/逻辑不得变化 |
| D-054 | Control Plane 公共 API 与 Secret 边界 | Publication、Run、JWKS、Event Session 和 Asset Token Exchange 进公共 `control-plane-v1` OpenAPI；SecretRef 的具体值解析是部署内 Reference port/adapter，不定义通用 Secret value HTTP API |
| D-055 | 规划证据真实性 | P03/P04 证据的 `summary_sha256` 只绑定 RFC 8785/JCS canonical 内容，不构成签名或身份证明；独立审核者/项目所有者真实性只能由外部受信 key registry 的 Ed25519 角色验签或人工可信渠道 Gate 确认。EVIDENCE、TRUSTED_KEYS、TRUSTED_CHANNEL_CONFIRMATION 的 lexical path 和 EvalSymlinks canonical target 都必须在仓库外。Registry 必须 strict Schema 校验且 key ID 唯一；证据算法、registry 算法、PKIX key 类型与 64-byte signature 都必须满足 Ed25519；count 必须是 json.Number 可表示的非负严格整数，禁止截断小数/溢出。candidate 保留 attestation 与 registry digest，或人工 confirmation digest 和不可密码学重放的限制；自检工具不得从布尔值推断真实性，也不取代独立审计的最终把关 |
| D-056 | 报告 commit 系谱 | 普通报告按 claimed commit 验证；`current-worktree` 现场重取并精确比较 Node/Go/OS platform/release/arch，历史验证只返回 `ancestor-archive-only`，不得把历史 runtime 当作当前环境；历史 success 只能隔离重跑或由受信 CI/OIDC/Sigstore provenance 绑定 repository、immutable workflow digest、commit/checker/完整 inputs/report/toolchain。P01/P02 额外绑定完整 Git tracked tree 的 path/type/mode/digest/bytes manifest，current/ancestor verifier 分别从 index+worktree/历史 Git tree 独立重发现且拒绝 symlink。P03/P04 历史规划签署与机器报告分类：P03 report 绑定 candidate/canonical/subject/result/raw/auth 并使用固定 checker/command/check/runtime 政策；P04 必须重新 strict/schema 读取 P03 原始 evidence 与 reviewer trust/confirmation、重跑 Authenticate，再将 raw/trust/report/promoted 逐项交叉核对，owner subject 同时绑定 P03 canonical content digest 与完整 promoted envelope 的 JCS/file digest；P03 JSON/JUnit/promoted summary 记为 runtime inputs，仓库外 evidence/trust 与 ignored build log 只以 kind+digest+bytes 记为 runtime evidence。规划签署固定 closure 为 requirements/plan/blueprint/artifact-manifest 四份 blob，证据字段、`git show subject:path`、current HEAD/worktree 三方 digest 必须一致，subject→HEAD ancestry 中任一修改后回退或删除都拒绝复用。混合 commit 聚合仍逐份验来源；全部 implement/refactor 报告覆盖本阶段 owned artifacts 与 baseline transition，P04、P17/P23/P26/P38、P43、P44、P45–P53 使用受控 exact runtime fan-in，P44 精确聚合 P01–P43 共 43 报告。A→B 只能由 P52 专用外部签名 equivalence attestation 的 `bridged_reports` 桥接，P53 在 B 重跑 final Conformance/SBOM/provenance |
| D-057 | 跨生态版本映射 | Release 命令唯一逻辑输入无 `v`：`1.0.0-rc.N`/`1.0.0`；Go root/nested tag 加 `v`，Python 映射为 PEP 440 `1.0.0rcN`/`1.0.0`，npm/OCI/CLI/Schema Bundle 保持逻辑值。拒绝 v-prefix、PEP-input、RC leading zero、build metadata；version-policy digest 绑定 journal/SBOM/provenance、P52 attestation 和 final evidence |
| D-058 | Baseline 未来归属 | 现存 baseline 的 future transition 与当前 availability 分开表达：Control Plane Lite 在 P05 migrate-retire；Go Manifest 与 Node digest 在 P06 repair-retain 并修复 validation-before-digest；`cmd/arop` 在 P36 extend-retain；Compatibility Matrix 在 P33 扩展为政策声明。计划与制品目录双向闭包 |
| D-059 | SDK/CLI/Conformance 首发闭包 | v1 包含 Go Consumer、Python RuntimeRegistration/Worker、TS Consumer；CLI 的 init/dev/test/publish/register/export/doctor 按纵向阶段实现；Scenario/Profile 是带唯一 ID、引用闭包、required-no-skip 和 digest 报告的机器制品，不以空目录或文档代替 |
| D-060 | Detached 外部证据 | P45/P50/P52 原始 signed bundle 的 producer 分别是外部 project owner/maintainer、独立实现/伙伴、release approver；仓库阶段只验证并输出 ignored detached summary，绝不代签。P49 RC subject、P50 compatibility、P51 candidate、P52 attestation、P53 final bundle 形成 release-asset/OCI-referrer/transparency 链，不写入冻结 A/B tree；A 内 Matrix 只承载政策，不预填伙伴 |
| D-061 | 工具运行时与严格输入边界 | 根 Go module 的 `internal/tooling/` 与私有命令拥有通用 report/evidence/planning/Gate/blueprint、Go proxy、Go/Container build 及 P39–P42 release/lineage/finalization；Python package primitive 使用原生 Python/PEP 517；Node 只允许 Schema/spec/manifest validation、Schema-driven codegen、TypeScript SDK 与 npm packaging。scope allowlist 不授予进程执行：当前 Node external-command allowlist 为空，child_process/exec/spawn/fork、dynamic import/require、eval/Function/native loading 全部拒绝；未来例外必须先成为 catalog 约束的最小机器 allowlist。通用 strict loader 递归拒 duplicate/trailing/multi-document，再离线执行 Draft 2020-12 + format assertion；writer 写前自验并传播所有序列化/Git/runtime/I/O 错误。P01 必须在净化 `GOFLAGS`/`GOENV`/缓存与工作区变量后用 `AROP_VERIFY_CURRENT=1 go test -count=1 -run=. -json` 实跑，实际 package/test 终态集合必须与受 Schema、artifact 和 phase owner 管理的 tracked inventory 完全一致且全部 pass，fail/skip/cache/no-tests 均为零，不得只硬编码总数。机器目录必须声明 `implementation_runtime + tool_scope`，并扫描 path/shebang/import/consumer/package-script/Make/workflow/image 以拒绝别名、伪 scope 与间接越界 |

# 2. 实现默认值

以下是不会改变核心 wire 语义、可以通过兼容配置调整的实现默认值：

| 编号 | 默认值 |
| --- | --- |
| P-001 | Registry 使用全局递增 `revision` |
| P-002 | 运行实例使用 `lease_id + session_id + generation` |
| P-003 | 事件使用 CloudEvents 兼容信封 |
| P-004 | Agent 到 Control Plane 使用小批量事件上报和 ACK |
| P-005 | Control Plane 到 Web/Gateway 使用 SSE |
| P-006 | Go 与 Python 在首个公开 v1 RC 提供完整 SDK；TypeScript 同时提供生成模型和流式 Consumer，完整 Provider 随后实现 |
| P-007 | Reference Control Plane 的本地 Quickstart 使用 SQLite，生产/多节点模式使用 PostgreSQL |
| P-008 | 简单 Agent 默认授权到 AgentDefinition，高风险 Skill 使用 Skill 级覆盖规则 |
| P-009 | 浏览器默认使用 Proxy/BFF；可信服务优先 Direct |
| P-010 | Agent Manifest 支持一个 AgentVersion 包含多个 Skill |
| P-011 | 首个公开 `v1.0.0-rc.N` 同时提供 Python Provider、Go SDK 和 TypeScript Streaming Consumer；此前均为 private/dev snapshot |
| P-012 | 提供最小 Reference Control Plane 和 Docker Compose Quickstart |
| P-013 | Registry 和 Run/Event Ledger 的实时通知只用于降低延迟，持久化 Ledger 才是真值 |

# 3. 发布配置待填写项

架构和 wire 语义已经冻结。以下项目不是设计分歧，只是需要在创建公共仓库、域名和发布账户时填写真实值：

| 编号 | 待填写值 | 当前约束 |
| --- | --- | --- |
| C-002 | 项目控制的文档域名 | 必须由项目长期控制；决定 Schema `$id`、Event Type Prefix 和 Extension Namespace |
| C-003 | PyPI/npm 实际注册结果 | 首选 `arop-sdk` 与 `@arop/sdk`；正式发布前必须完成占用检查和所有权验证 |
| C-004 | 初始 Maintainer、Reviewer 和安全邮箱 | 正式公开贡献前至少两名有恢复权限的维护者，并提供私密安全报告入口 |

在 C-002～C-004 填写完成前可以开发和运行本地 Fixture，但不得发布稳定包或宣称公共命名空间已经永久冻结。

这些配置和 v1 所需的独立实现、外部设计伙伴、非金运评审记录都是 **Gate**，不是可由代码库内部 Fixture 替代的交付物。只有存在可核验的外部链接、报告或签字记录时才能标记为完成；否则必须保持 `blocked` 或 `pending-external-evidence`。

# 4. 规范权威与冲突处理

权威顺序固定如下：

1. 本文档的 Decision 约束其他所有产物，新冲突必须先修改 Decision。
2. 领域规范与状态机 Fixture 共同定义可观测行为；二者不一致时不得发布。
3. JSON Schema 定义数据结构、必填性、格式和枚举。
4. OpenAPI 和 AsyncAPI 定义 HTTP/SSE/Event Batch 等传输绑定，不可重新定义载荷语义。
5. SDK 生成模型、生成文档和参考实现均为派生物，不得被当作反向修改规范的权威。

唯一机器可读制品目录是 [`spec/artifact-manifest.yaml`](../spec/artifact-manifest.yaml)；需求映射和冲突决议分别记录在 [`spec/requirements.yaml`](../spec/requirements.yaml) 与 [`spec/conflicts.yaml`](../spec/conflicts.yaml)。`DIRECTORY_STRUCTURE.md` 只是面向人的布局说明，不是另一份机器清单。

14 条不可变需求以 `statement_original_zh` 为权威原文，`translation_en` 仅供阅读。仓内硬编码原文与整组 JCS 内容摘要只负责检测漂移；P03 独立审计仍负责最终把关，不得以自检通过代替。

# 5. 与现有上层架构文档的差异

工作区早期架构曾将以下内容列为第一阶段暂缓：

- SSE 全链路流式协议。
- 调用方直接访问 Agent。

本设计讨论已形成新的方向：

- Protocol v1 包含流式事件。
- “必须经过 Control Plane”指必须经过授权和 Run 创建；获得 Ticket 后可以 Direct。

在开始跨仓实现前，应同步更新工作区 `AI中台/ARCHITECTURE.md`，避免两个仓库持有冲突描述。

# 6. 公共化后的文档边界

- 公共定位、开放治理和采用路径以 `PUBLIC_PROJECT_AND_ADOPTION.md` 为准。
- MCP、A2A、ARD、CloudEvents 和 OpenTelemetry 映射以 `INTEROPERABILITY.md` 为准。
- 具体外部标准版本必须在发布时进入 Compatibility Matrix，不在核心字段中暗含。
