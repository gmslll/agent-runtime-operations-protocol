---
title: Agent Runtime Operations Protocol 开发与发布计划
status: planning-candidate
updated: 2026-09-22
---

# 1. 执行约定

本计划是可调度的单 Agent 阶段 DAG。阶段类型只使用 paseo-epic 支持的 `implement`、`refactor`、`verify · review`、`verify · spec`、`gate` 和 `deliver`。每个 `implement`/`refactor` 阶段只包含一个有界的组件或纵向路径，单个 Agent 能在一个上下文内完成、验证和回退。

每个阶段的 Make 验收必须生成 `build/reports/<phase>/report.json` 与 `junit.xml`；聚合目标的子报告数量、exact commit、输入/Checker Digest 必须一致，不允许只根据 exit code 宣告成功。P03/P04/P40/P44 只校验真实外部证据，不自动伪造证据。
原始外部证据必须保存在仓库外，Git 仅接受脱敏签名摘要和机器报告。

# 2. 状态和里程碑

| 区间 | 里程碑 | 当前含义 |
| --- | --- | --- |
| P01–P04 | 规划、独立审计、用户 Gate | P01/P02 候选完成；等待 P03 重新审计和 P04 用户确认 |
| P05–P38 | 可运行闭环 | 目标实现、发布工具链和内部验证 |
| P39 | code-complete | 只表示仓库内可自证要求通过 |
| P40–P43 | public v1 RC | 真实公开配置、全量再生成、最终验证、`v1.0.0-rc.N` |
| P44–P46 | 外部证据与 v1 | 证据绑定 v1 RC，严格只读 freeze，同源 commit 正式交付 |

# 3. 可调度阶段

## P01 — Contract authority and metadata integrity

- **Type:** implement
- **Status:** complete
- **Capability owner:** spec-governance
- **Goal:** 建立 Decision 权威链、14 条不可变 statement、冲突台账、制品 DAG 和规划 Meta-Schema 候选。
- **Scope:** `docs/DECISIONS.md`、`spec/*`、`scripts/spec-index-check.mjs`、公共 report writer；不改业务实现。
- **Dependencies:** none
- **First-path invariants:** Authoring strict/Consumer forward compatible；离线 `$ref`；先校验再 Digest；制品路径唯一、DAG 无环、语言派生正确。
- **Machine acceptance:** `make spec-index-check` → `build/reports/P01/report.json` 和 `junit.xml`。
- **Rollback point:** 恢复上一份规划元数据；冲突或 DAG 错误时不进入 P02。
- **Definition of done:** Meta-Schema、路径、DAG、Decision/链接/引用闭包和反例探针全绿，报告带 commit/dirty/command/runtime/input/checker digest。

## P02 — Schedulable implementation blueprint

- **Type:** implement
- **Status:** complete
- **Capability owner:** implementation-planning
- **Goal:** 形成双 Go Module、组件边界、单 Agent 阶段 DAG、API 归属、机器验收和 v1 发布闭环候选。
- **Scope:** `DIRECTORY_STRUCTURE.md`、`IMPLEMENTATION_BLUEPRINT.md`、本计划、README/AGENTS/ARCHITECTURE/SDK 交叉引用和 blueprint checker。
- **Dependencies:** P01
- **First-path invariants:** 根公共 module + 唯一 Reference CP 嵌套 module；Conformance 语言中立；Console/厂商 Adapter 隔离。
- **Machine acceptance:** `make blueprint-check` → `build/reports/P02/report.json` 和 `junit.xml`。
- **Rollback point:** 仅回退规划文档/检查器，不物理搬迁目录。
- **Definition of done:** P01..Pn 连续、无环、type 合法，每个 implement/refactor 范围单一，需求到阶段/真实 Make 目标可追溯。

## P03 — Independent planning audit

- **Type:** verify · review
- **Status:** pending-review
- **Capability owner:** independent-reviewer
- **Goal:** 由未参与 P01/P02 撰写的审核者验证需求覆盖、可调度性和必修项归零。
- **Scope:** 只读核心文档/规划元数据；输入为仓库外审计证据，仓库只生成脱敏签名摘要报告。
- **Dependencies:** P02
- **First-path invariants:** `requirements_phase_coverage=YES`；`must_fix_count=0`；subject commit 和 requirements/plan/blueprint digest 精确匹配；审核者独立。
- **Machine acceptance:** `make planning-audit EVIDENCE=<outside-repo-review>` → `build/reports/P03/report.json` 和 `junit.xml`。
- **Rollback point:** must-fix 非零时回 P01/P02 修复并重新独立审计，不编辑证据伪造通过。
- **Definition of done:** 外部证据 Schema 通过、对象 digest 匹配、must-fix 为零，报告不包敏感原始证据。

## P04 — User implementation gate

- **Type:** gate
- **Status:** blocked
- **Capability owner:** project-owner
- **Goal:** 在任何物理重构和新协议行为前获得用户对已审计计划的明确确认。
- **Scope:** P03 成功报告、计划 subject commit/digest、仓库外用户批准证据。
- **Dependencies:** P03
- **First-path invariants:** 批准必须绑定已审计版本；工具只验证而不自动批准。
- **Machine acceptance:** `make gate-check GATE=P04 EVIDENCE=<outside-repo-approval>` → `build/reports/P04/report.json` 和 `junit.xml`。
- **Rollback point:** 用户要求修改时回 P02/P03；新 digest 使旧批准失效。
- **Definition of done:** 用户批准证据绑定当前计划并通过检查；此前 P05 保持未开始。

## P05 — Physical two-module repository refactor

- **Type:** refactor
- **Status:** pending
- **Capability owner:** repository-layout
- **Goal:** 按目标树搬迁基线，建立根公共 module 和唯一 `reference/control-plane` 嵌套 module。
- **Scope:** Go 包/命令搬迁、`go.work.example`、导入层级和 CI 骨架；不改变 wire 行为。
- **Dependencies:** P04
- **First-path invariants:** 只有两个 `go.mod`；开发期 `go.work` 本地可用但默认不提交；无永久 `replace`；根 module 不导入 Reference `internal`。
- **Machine acceptance:** `make test-go-workspace` → `build/reports/P05/report.json` 和 `junit.xml`。
- **Rollback point:** 使用迁移前 tag/commit 整体回退，不长期维护两套目录。
- **Definition of done:** 两 module 在 `GOWORK=off` 分别编译/测试，基线 CLI/健康行为不变，层级检查通过。

## P06 — Strict input, digest and error foundation

- **Type:** implement
- **Status:** pending
- **Capability owner:** protocol-foundation
- **Goal:** 完成先验证再 Digest、重复 JSON Key、错误码、UTF-8 offset 和状态机基础。
- **Scope:** `sdk/go/protocol`、共享 Fixture/Digest corpus 和 CLI 验证路径；不含生成模型。
- **Dependencies:** P05
- **First-path invariants:** strict authoring/forward consumer 分开；离线 `$ref`；重复 key 早拒绝；终态迁移 Fixture 是行为权威。
- **Machine acceptance:** `make test-protocol-foundation` → `build/reports/P06/report.json` 和 `junit.xml`。
- **Rollback point:** 保留旧 CLI 可运行 tag；任一跨语言 digest 分歧时不进入 codegen。
- **Definition of done:** Go/Node 对黄金/反例、JCS、中英文 offset、错误码和状态迁移结论一致。

## P07 — Three-language codegen spike

- **Type:** implement
- **Status:** pending
- **Capability owner:** code-generation
- **Goal:** 用代表 Schema 冻结 Go/Python/TypeScript 生成器、映射和可重现 pipeline，再决定批量生成。
- **Scope:** pinned generators、union/nullable/optional/format/extension/离线 `$ref` spike、生成目录和 drift check。
- **Dependencies:** P06
- **First-path invariants:** 生成代码不手改；Schema 仍是结构权威；生成器版本/命令/输入输出 digest 进 provenance。
- **Machine acceptance:** `make codegen-spike` → `build/reports/P07/report.json` 和 `junit.xml`。
- **Rollback point:** spike 失败则更换映射/生成器，不扩展到全量 Schema。
- **Definition of done:** 三语言编译/类型检查、JSON round-trip、strict/forward 模式和清洁重生成零 diff 通过。

## P08 — Control Plane application and HTTP foundation

- **Type:** implement
- **Status:** pending
- **Capability owner:** control-plane-platform
- **Goal:** 建立 Go Reference CP 配置、HTTP 装配、UoW/Repository ports、统一错误和安全中间件。
- **Scope:** `reference/control-plane/cmd/aropd`、`internal/app|ports|adapters/http|config`；不含业务 aggregate。
- **Dependencies:** P07
- **First-path invariants:** Clock/ID/Fault/Signer/Telemetry 全经 port 注入；请求大小/超时/类型在解析前限制；Trace/Audit context 从入口创建。
- **Machine acceptance:** `make test-control-plane-platform` → `build/reports/P08/report.json` 和 `junit.xml`。
- **Rollback point:** 保留 P05 可启动骨架；新装配不 ready 时不替换基线。
- **Definition of done:** 配置分层、graceful shutdown、错误/Trace/Audit 上下文、端口契约与故障 seam 单测通过。

## P09 — Dual storage migrations and truthful readiness

- **Type:** implement
- **Status:** pending
- **Capability owner:** control-plane-storage
- **Goal:** 建立 SQLite/PostgreSQL adapter、migration runner、checksum/dirty 检测和真实 readiness。
- **Scope:** `internal/adapters/storage`、`migrations/sqlite|postgres`、UoW 契约和 readiness probe。
- **Dependencies:** P08
- **First-path invariants:** 语义一致而非 SQL 一致；空库/N-1/幂等/并发/dirty/backup-restore 从首次 migration 内建；未迁移不 ready。
- **Machine acceptance:** `make test-storage-migrations` → `build/reports/P09/report.json` 和 `junit.xml`。
- **Rollback point:** dirty/partial 立即 fail closed；破坏性变更仅用验证过的 backup restore。
- **Definition of done:** 双库同一 port suite 与 migration 矩阵通过，readiness 反映连接、checksum、版本和必需约束。

## P10 — Development identity, credentials and base audit

- **Type:** implement
- **Status:** pending
- **Capability owner:** control-plane-security-foundation
- **Goal:** 建立开发身份、Credential 生命周期、基础 Audit/Trace 存储和 Endpoint 安全检查器。
- **Scope:** dev identity provider、credential issue/rotate/revoke、audit repository、Endpoint/Callback/Asset URL policy。
- **Dependencies:** P09
- **First-path invariants:** Credential 类型/audience/scope 不混用；静态 scheme/host/IP 检查 + 连接时 DNS/IP 重解析 + 每次 redirect 重检；日志脱敏。
- **Machine acceptance:** `make test-security-foundation` → `build/reports/P10/report.json` 和 `junit.xml`。
- **Rollback point:** 旋转失败保留上一有效重叠窗口；无法重检的 Endpoint fail closed。
- **Definition of done:** issue/rotate/revoke/replay、SSRF/DNS rebinding/redirect、Audit/Trace 持久化和脱敏反例全过。

## P11 — Publication and Control Plane public contracts

- **Type:** implement
- **Status:** pending
- **Capability owner:** publication-contracts
- **Goal:** 完成 AgentVersion Publication 和 Control Plane 公共 OpenAPI，明确 JWKS/Event Session/Asset/Secret 边界。
- **Scope:** Manifest/Resource Schema、`openapi/control-plane-v1.yaml`、Publication/Run 骨架、JWKS、Event Session、Asset Token Exchange 合同；Secret 解析仅 Reference port。
- **Dependencies:** P10
- **First-path invariants:** 所有 `$ref` 离线；JWKS/Event Session/Asset Exchange 是 public contract；Secret value 不进 wire，Secret Exchange 不标准化为公共 HTTP API。
- **Machine acceptance:** `make test-publication-contracts` → `build/reports/P11/report.json` 和 `junit.xml`。
- **Rollback point:** 未冻结 API 保持 experimental；边界不清时不实现 Handler。
- **Definition of done:** Publication/JWKS/Event Session/Asset 公共路径与 Secret reference-only port 都有 owner/phase/test，Schema/OpenAPI 闭包通过。

## P12 — Publication Go vertical slice

- **Type:** implement
- **Status:** pending
- **Capability owner:** publication-service
- **Goal:** 实现验证、Digest、不可变 AgentVersion 和 Audit 的 Go 发布路径。
- **Scope:** publication domain/app/storage/http，双库契约和发布 Fixture。
- **Dependencies:** P11
- **First-path invariants:** Bundle 闭包、验证、Digest、版本、Audit 同事务；无网络 Schema 获取；已发版本不覆盖。
- **Machine acceptance:** `make test-publication-service` → `build/reports/P12/report.json` 和 `junit.xml`。
- **Rollback point:** 失败请求保留 draft/审计，不产生 published 记录。
- **Definition of done:** SQLite/PostgreSQL 并发/重放/冲突/非法引用契约全过。

## P13 — Registry domain and persistence core

- **Type:** implement
- **Status:** pending
- **Capability owner:** registry-core
- **Goal:** 实现 RuntimeInstance/Session/Generation/Lease/Revision/CAS 领域模型和双库 Ledger。
- **Scope:** registry Schema/Fixture、domain、repository ports、SQLite/PostgreSQL adapter；不含 Watch HTTP。
- **Dependencies:** P12
- **First-path invariants:** 服务端 Clock；Generation Fencing；Registry Event append-only；revision 与状态同事务；Audit/Trace 内建。
- **Machine acceptance:** `make test-registry-core` → `build/reports/P13/report.json` 和 `junit.xml`。
- **Rollback point:** 修订号/原始 Event 不回退；实例状态无法确认时退出路由。
- **Definition of done:** 双库对 register/keepalive/CAS/fence/expire 同一序列得到相同语义。

## P14 — Registry public API, lease and drain

- **Type:** implement
- **Status:** pending
- **Capability owner:** registry-api
- **Goal:** 实现 Registry/Discovery OpenAPI、Deployment Credential 验证、Keepalive、健康视图和 Drain。
- **Scope:** `openapi/registry-runtime-v1.yaml`、`discovery-runtime-v1.yaml`、HTTP handler、router snapshot。
- **Dependencies:** P13
- **First-path invariants:** Credential lifecycle 复用 P10；正交健康字段；Lease 到期立即不可发现；Drain 禁止新 Attempt。
- **Machine acceptance:** `make test-registry-api` → `build/reports/P14/report.json` 和 `junit.xml`。
- **Rollback point:** 停止新注册/路由，保留已有 Lease 审计，不延长过期 Lease。
- **Definition of done:** auth/register/keepalive/patch/drain/deregister/snapshot 的正反契约通过。

## P15 — Registry watch, HA and recovery

- **Type:** implement
- **Status:** pending
- **Capability owner:** registry-recovery
- **Goal:** 实现 Watch long-poll、Replay、Compaction/Resync、Router Cache 和多节点恢复。
- **Scope:** registry change feed、notifier seam、cache、compactor、PostgreSQL 多节点 Fixture。
- **Dependencies:** P14
- **First-path invariants:** 通知可丢，Ledger 不可丢；Cache 可重建；compaction watermark 可审计；旧 session 始终 fenced。
- **Machine acceptance:** `make test-registry-recovery` → `build/reports/P15/report.json` 和 `junit.xml`。
- **Rollback point:** 禁用通知/Cache 并回到 snapshot/replay；不删除未过 retention 的 Ledger。
- **Definition of done:** 丢通知、断线、压缩、主节点切换后可 replay/resync，无旧实例复活。

## P16 — Registry conformance verification

- **Type:** verify · spec
- **Status:** pending
- **Capability owner:** registry-verification
- **Goal:** 只读验证 Registry Core/API/Recovery 对 Schema、OpenAPI 和故障向量的完整性。
- **Scope:** P13–P15 制品/报告和独立黑盒 Registry suite；不修代码。
- **Dependencies:** P15
- **First-path invariants:** Lease/Generation/Revision/CAS/Watch/Resync/Drain 无缺项；双库报告 exact commit/digest 一致。
- **Machine acceptance:** `make verify-registry` → `build/reports/P16/report.json` 和 `junit.xml`。
- **Rollback point:** 失败回首个引入错误的 P13–P15 阶段，修复后全量重验。
- **Definition of done:** 子报告数量、commit、Schema/Fixture digest 一致，Registry Profile 全绿。

## P17 — Run authorization and durable lifecycle

- **Type:** implement
- **Status:** pending
- **Capability owner:** run-service
- **Goal:** 实现鉴权与创建 Run 原子事务，内建 Outbox、Cancel、Deadline、Usage、Audit、Trace 和 Effect 语义。
- **Scope:** Run Schema/状态机、authz snapshot、run domain/app/storage、durable outbox；不含 Dispatcher。
- **Dependencies:** P16
- **First-path invariants:** 鉴权/Run/Audit/Outbox 同 UoW；Cancel requested 不等于 cancelled；Deadline 用注入 Clock；Usage 累计；`effect_id` 从首个写路径进 Inbox。
- **Machine acceptance:** `make test-run-lifecycle` → `build/reports/P17/report.json` 和 `junit.xml`。
- **Rollback point:** 投递失败保留 Run/Audit/Outbox 可重试状态，不删历史。
- **Definition of done:** TOCTOU、重复创建、Cancel/Deadline 竞态、Usage/effect 幂等和 Outbox crash window 契约通过。

## P18 — Dispatcher, Attempt, Ticket and JWKS

- **Type:** implement
- **Status:** pending
- **Capability owner:** dispatch-security
- **Goal:** 实现路由选择、Attempt/Fencing、Dispatch Ticket、ES256/JWKS 旋转和重调度。
- **Scope:** dispatcher domain/app、Attempt storage、Token signer/verifier、JWKS public endpoint 和 run-token Fixture。
- **Dependencies:** P17
- **First-path invariants:** 调度只选已授权可用实例；Attempt/Ticket/Fencing 每次新建；claims 绑定 Run/Attempt/Agent/Deployment/Audience/Scope/TTL；旋转重叠。
- **Machine acceptance:** `make test-dispatch-ticket` → `build/reports/P18/report.json` 和 `junit.xml`。
- **Rollback point:** 禁止签发新 Ticket，已签发 Ticket 按 TTL/撤销策略处理，不复用 fencing token。
- **Definition of done:** 路由、claims 负向、JWKS cache/rotate/overlap、retry/fencing 契约全过。

## P19 — Immutable Event Ledger and ingest

- **Type:** implement
- **Status:** pending
- **Capability owner:** event-ledger
- **Goal:** 实现 Event Session 交换、durable Inbox、去重/冲突、Producer/Run Sequence、终态和 Snapshot/ResultRef。
- **Scope:** Event Schema、ledger domain/app/storage、Event Session Token、Event Batch ingest；不含 SSE。
- **Dependencies:** P18
- **First-path invariants:** 原始 Event append-only；Inbox 与投影同 UoW；终态不可逆且带最终 Snapshot/ResultRef；迟到事件仅审计；Final Usage 一起固化。
- **Machine acceptance:** `make test-event-ledger` → `build/reports/P19/report.json` 和 `junit.xml`。
- **Rollback point:** 禁止 ingest 时保留 Ledger/Inbox/Outbox；不重写事件和终态。
- **Definition of done:** duplicate/conflict/gap/reorder/late-terminal/crash-replay 与双库并发契约全过。

## P20 — Direct and Proxy delivery

- **Type:** implement
- **Status:** pending
- **Capability owner:** delivery-http
- **Goal:** 实现同一 RunRequest/Event 语义下的 Direct 和 Control Plane Proxy。
- **Scope:** Agent Runtime OpenAPI、Go provider/consumer HTTP 基础、Direct Ticket 验证、Proxy relay；不含 SSE resume。
- **Dependencies:** P19
- **First-path invariants:** 新 Run 已先过 Control Plane；浏览器 v1 仅 BFF/Proxy；Endpoint 复用静态+连接+跳转 DNS/IP 重检；Trace/Audit 跨交付传播。
- **Machine acceptance:** `make test-direct-proxy` → `build/reports/P20/report.json` 和 `junit.xml`。
- **Rollback point:** 可禁用 Direct 回到 Proxy，但不绕过 Run/Ticket。
- **Definition of done:** Direct/Proxy 同 Fixture/终态/错误，audience/scope 错误、SSRF 反例和中继中断通过。

## P21 — Structured streaming and resume

- **Type:** implement
- **Status:** pending
- **Capability owner:** streaming-delivery
- **Goal:** 实现 Relay/Direct SSE、Last-Event-ID、Event Batch partial ACK、Snapshot 纠正和断线恢复。
- **Scope:** AsyncAPI、SSE handler/client、batch ACK/error、retention/resume 窗口；复用 P19 Ledger。
- **Dependencies:** P20
- **First-path invariants:** 流式是 Ledger 投影而非真值；Producer/Run Sequence 不混；UTF-8 byte offset；超出 retention 显式要求 Snapshot/Resync。
- **Machine acceptance:** `make test-streaming-resume` → `build/reports/P21/report.json` 和 `junit.xml`。
- **Rollback point:** 停止 SSE 新连接但保留查询/Final Snapshot；不丢 Ledger。
- **Definition of done:** duplicate/reorder/disconnect/resume/retention/partial-failure/中英文 delta 和 Snapshot reset 契约通过。

## P22 — Run and delivery verification

- **Type:** verify · spec
- **Status:** pending
- **Capability owner:** run-delivery-verification
- **Goal:** 只读验证 P17–P21 对 auth/run/attempt/ticket/event/direct/proxy/streaming 的完整性。
- **Scope:** 黑盒组合场景、双库、Clock/ID/Fault 确定运行；不修实现。
- **Dependencies:** P21
- **First-path invariants:** Cancel/Deadline/Usage/effect/Inbox-Outbox/Audit/Trace 必须出现在首条路径报告，不允许延后补做。
- **Machine acceptance:** `make verify-run-delivery` → `build/reports/P22/report.json` 和 `junit.xml`。
- **Rollback point:** 失败回 P17–P21 首个引入阶段并重跑全套。
- **Definition of done:** 子报告数量/commit/digest 一致，Direct/Proxy/Streaming Profile 黑盒契约全绿。

## P23 — Worker Pull service

- **Type:** implement
- **Status:** pending
- **Capability owner:** worker-service
- **Goal:** 实现 Claim/Accept/Renew/Complete、Attempt Lease/Fencing、Capacity/Drain 和 Session Affinity 服务端。
- **Scope:** Worker OpenAPI、worker domain/app/storage/http；不含 Go Worker SDK。
- **Dependencies:** P22
- **First-path invariants:** crash-before/after-accept 可恢复；旧 Worker 不覆盖新 Attempt；Pull 复用 P19 Event/Inbox/Outbox/effect_id 语义。
- **Machine acceptance:** `make test-worker-service` → `build/reports/P23/report.json` 和 `junit.xml`。
- **Rollback point:** 停止新 Claim，等 Lease 过期后安全重分配，不强改 Attempt。
- **Definition of done:** long-poll、renew、double-complete、crash、fence、drain、sticky 双库契约通过。

## P24 — Go Worker SDK and generic reference worker

- **Type:** implement
- **Status:** pending
- **Capability owner:** go-worker-client
- **Goal:** 交付 Go Worker SDK 和通用 Pull Worker 参考，证明无入站 Agent 可接入。
- **Scope:** `sdk/go/worker`、`reference/agents/pull-worker`、并发槽/renew/outbox/drain；无 cc-connect/Codex 专属 Adapter。
- **Dependencies:** P23
- **First-path invariants:** SDK 不导入 Reference `internal`；通用 wire 无厂商字段；本地 effect/inbox/outbox 幂等。
- **Machine acceptance:** `make test-go-worker-sdk` → `build/reports/P24/report.json` 和 `junit.xml`。
- **Rollback point:** 回退 SDK/参考 Worker 版本，不改 Worker 服务端 wire 迁就客户端。
- **Definition of done:** 参考 Worker 在全新环境完成 claim→stream→terminal，crash/drain 契约通过。

## P25 — Operations, recovery and security regression

- **Type:** verify · review
- **Status:** pending
- **Capability owner:** operations-security-review
- **Goal:** 只读验证前述首路径已内建的运维/安全语义，并做 retention、backup/restore、密钥旋转回归。
- **Scope:** P09/P10/P13–P24 报告、备份恢复、脱敏、限额、SSRF/DNS/redirect 和 Credential lifecycle 黑盒套件。
- **Dependencies:** P24
- **First-path invariants:** 本阶段不用来首次补 Audit/Trace/Cancel/Deadline/Usage/effect/inbox/outbox/seam；发现缺失必须回所属首路径阶段。
- **Machine acceptance:** `make verify-operations-security` → `build/reports/P25/report.json` 和 `junit.xml`。
- **Rollback point:** 安全回归失败时 fail closed，禁用受影响 Profile/发布流程。
- **Definition of done:** 子报告和黑盒反例全绿，备份恢复后同一 Contract Suite 通过。

## P26 — Python Provider SDK and ASGI runtime

- **Type:** implement
- **Status:** pending
- **Capability owner:** python-provider
- **Goal:** 交付 Python 生成模型、Provider API、ASGI 薄层、Token/Schema 验证和 durable Inbox/Outbox。
- **Scope:** `sdk/python`、Python HTTP reference agent、pytest fixtures；不实现 Control Plane。
- **Dependencies:** P25
- **First-path invariants:** 默认不绕 Token/TLS/Schema；Cancel/Deadline/Trace/Usage/effect_id 映射完整；SDK 不包含 Console 假设。
- **Machine acceptance:** `make test-python-provider` → `build/reports/P26/report.json` 和 `junit.xml`。
- **Rollback point:** 回退 Python package 而不改 wire 或参考 CP；不兼容需求走 RFC。
- **Definition of done:** 最小/流式/副作用/断线/取消参考 Agent 通过公共 Fixture，安装不需 Console。

## P27 — TypeScript Consumer, reducer and BFF/Web sample

- **Type:** implement
- **Status:** pending
- **Capability owner:** typescript-consumer
- **Goal:** 交付 TypeScript 生成模型、SSE Consumer、Event Reducer、BFF 和最小 Web 流式示例。
- **Scope:** `sdk/typescript`、BFF/Web sample；不提供浏览器 Direct 或 Node Provider。
- **Dependencies:** P26
- **First-path invariants:** 浏览器仅 BFF/Proxy；event_id/run_sequence 去重；Final Snapshot 纠正 delta；未知可选字段前向兼容。
- **Machine acceptance:** `make test-typescript-consumer` → `build/reports/P27/report.json` 和 `junit.xml`。
- **Rollback point:** 回退 npm 候选包/示例，不降级 Token 或 Proxy 边界。
- **Definition of done:** reconnect/resume/reorder/reset/final/error 与 BFF token isolation 测试全绿。

## P28 — A2A interoperability adapter

- **Type:** implement
- **Status:** pending
- **Capability owner:** interop-a2a
- **Goal:** 实现 A2A Agent Card/Task/Message/Artifact/Streaming 映射与损失分类。
- **Scope:** `adapters/a2a`、锁定上游 Fixture、exact/extended/lossy/unsupported 报告；不实现 MCP/ARD。
- **Dependencies:** P27
- **First-path invariants:** 不重定义 A2A；保留原始对象引用；取消/错误/产物/Trace 映射可检测。
- **Machine acceptance:** `make test-interop-a2a` → `build/reports/P28/report.json` 和 `junit.xml`。
- **Rollback point:** 独立禁用 A2A Adapter，不影响 Core/Delivery。
- **Definition of done:** 锁定版本的往返 Fixture、损失报告和 lifecycle/streaming 契约通过。

## P29 — MCP interoperability example and trace mapping

- **Type:** implement
- **Status:** pending
- **Capability owner:** interop-mcp
- **Goal:** 交付 MCP dependency declaration、Tool/Resource 边界与 Tool Event/Trace 映射示例。
- **Scope:** `adapters/mcp`、Fixture 与安全指南；不复制 MCP Server/Client 协议实现。
- **Dependencies:** P28
- **First-path invariants:** MCP credential 不进 AROP Event；Tool call 保留 Trace/effect 联系；未支持能力显式。
- **Machine acceptance:** `make test-interop-mcp` → `build/reports/P29/report.json` 和 `junit.xml`。
- **Rollback point:** 移除/禁用 MCP 示例不改 AROP Core。
- **Definition of done:** tool/resource/credential/error/trace 正反向 Fixture 通过，无密钥泄漏。

## P30 — ARD catalog interoperability

- **Type:** implement
- **Status:** pending
- **Capability owner:** interop-ard
- **Goal:** 实现 ARD/AI Catalog 导出、导入审查 Fixture 和跨域发现边界。
- **Scope:** `adapters/ard`、catalog fixtures/loss report；不导出运行注册表。
- **Dependencies:** P29
- **First-path invariants:** RuntimeInstance/Lease/容量/内网 Endpoint 永不进公开 Catalog；import 不自动授权/发布。
- **Machine acceptance:** `make test-interop-ard` → `build/reports/P30/report.json` 和 `junit.xml`。
- **Rollback point:** 独立禁用导入/导出，不改 Registry 真值。
- **Definition of done:** 公开字段白名单、泄漏反例、有损映射和审核流 Fixture 通过。

## P31 — CloudEvents and OpenTelemetry ownership mapping

- **Type:** implement
- **Status:** pending
- **Capability owner:** interop-observability
- **Goal:** 交付 CloudEvents structured/binary/batch 绑定和 OTel Trace/Metric/Log 映射，不重定义上游语义。
- **Scope:** interoperability fixtures/docs、event encoding adapter、OTel instrumentation mapping；不改 Event 领域语义。
- **Dependencies:** P30
- **First-path invariants:** CloudEvents 属 transport envelope；OTel 属 observability mapping；Run/Event ID 不替代 Trace ID；脱敏一致。
- **Machine acceptance:** `make test-interop-observability` → `build/reports/P31/report.json` 和 `junit.xml`。
- **Rollback point:** 退回基础 JSON/SSE 绑定，不改 Ledger。
- **Definition of done:** 三种 CloudEvents HTTP 编码和跨 Direct/Proxy/Pull/Adapter Trace 传播测试通过。

## P32 — Portable root Go conformance runner

- **Type:** implement
- **Status:** pending
- **Capability owner:** portable-conformance
- **Goal:** 在根 module 交付不依赖 Reference internal 的 `arop-conformance` 黑盒 runner。
- **Scope:** `cmd/arop-conformance`、顶层语言中立 fixtures/scenarios/profiles/reports；不含服务端 fault driver。
- **Dependencies:** P31
- **First-path invariants:** 被测实现仅需 Endpoint/制品；Profile 按实测声明；Runner digest 进每份外部报告。
- **Machine acceptance:** `make test-portable-conformance` → `build/reports/P32/report.json` 和 `junit.xml`。
- **Rollback point:** 回退 runner 版本而不降低 Fixture 迁就失败实现。
- **Definition of done:** Provider/Consumer/Registry/Worker 黑盒 Profile 生成带 runner/schema/artifact digest 的 JSON/JUnit。

## P33 — Nested server conformance driver

- **Type:** implement
- **Status:** pending
- **Capability owner:** server-conformance
- **Goal:** 在嵌套 Go module 交付 Control Plane 服务端 driver，调用 portable scenarios 而不把 internal 泄漏给 runner。
- **Scope:** `reference/control-plane/tests|internal/conformance`、服务端启停/清理和黑盒端口。
- **Dependencies:** P32
- **First-path invariants:** root runner 不导入 nested module；driver 对两数据库执行同一 scenarios；测试密钥与生产分离。
- **Machine acceptance:** `make test-server-conformance` → `build/reports/P33/report.json` 和 `junit.xml`。
- **Rollback point:** 可回退 driver，不改 portable fixture 语义。
- **Definition of done:** Control Plane Profile 在 SQLite/PostgreSQL 上黑盒通过，无第三 Go module。

## P34 — Fault and HA drivers

- **Type:** implement
- **Status:** pending
- **Capability owner:** fault-ha-harness
- **Goal:** 交付可确定注入 duplicate/drop/reorder/delay/crash/partition 和 PostgreSQL 多节点切换的 driver。
- **Scope:** `conformance/fault-injection`、Reference CP FaultHook adapter、故障代理/多节点 harness；生产默认 no-op。
- **Dependencies:** P33
- **First-path invariants:** FaultHook 只在预定义 checkpoint；生产不暴露远程 fault API；每次 seed/时钟/ID 可重现。
- **Machine acceptance:** `make test-fault-ha-drivers` → `build/reports/P34/report.json` 和 `junit.xml`。
- **Rollback point:** 禁用 harness/fault build tag，不影响生产二进制。
- **Definition of done:** 故障场景可用同 seed 复现，并生成故障时间线/节点/digest 报告。

## P35 — Independent quickstart and deployments

- **Type:** implement
- **Status:** pending
- **Capability owner:** developer-experience
- **Goal:** 交付 SQLite 单进程 Quickstart 和 PostgreSQL 生产参考部署，不安装 Console。
- **Scope:** `deployments/quickstart`、`production-reference`、Go/Python/Worker/Web 示例编排、升级/恢复 runbook。
- **Dependencies:** P34
- **First-path invariants:** 无真实 Credential；默认安全不降级；Quickstart 与生产参考共用语义而非共用 SQL。
- **Machine acceptance:** `make quickstart-smoke` → `build/reports/P35/report.json` 和 `junit.xml`。
- **Rollback point:** 销毁临时容器/卷/测试凭据，不触及用户数据。
- **Definition of done:** 全新环境十分钟内完成流式 Run，两部署运行相同 conformance subset 并输出报告。

## P36 — Read-only migration, fault and HA verification

- **Type:** verify · review
- **Status:** pending
- **Capability owner:** resilience-verification
- **Goal:** 严格只读聚合迁移矩阵、故障注入、双库、备份恢复和多节点 HA 证据。
- **Scope:** P09/P13–P35 已生成子报告和独立重跑；不改 Schema/实现。
- **Dependencies:** P35
- **First-path invariants:** 子报告预期数量固定；commit/input/checker/artifact digest 一致；无重复副作用/终态翻转/旧主复活。
- **Machine acceptance:** `make verify-resilience` → `build/reports/P36/report.json` 和 `junit.xml`。
- **Rollback point:** 失败回首个缺陷所属实现阶段，并使后续聚合报告失效。
- **Definition of done:** empty/N-1/idempotent/concurrent/dirty/backup-restore 双库矩阵与 replay/reorder/cancel/fencing/partition/HA 全绿。

## P37 — Release engineering toolchain

- **Type:** implement
- **Status:** pending
- **Capability owner:** release-engineering
- **Goal:** 实现后续只读发布演练和正式交付共用的可重现发布编排工具链。
- **Scope:** `scripts/release/` 内的双 Go Module 临时 proxy、Go/Python/npm 打包、Container 构建、SBOM/provenance、测试签名和 payload manifest 编排；不执行公开发布。
- **Dependencies:** P36
- **First-path invariants:** 开发期 `go.work` 不进入制品；临时 proxy 先放根伪版本/RC 候选再以 `GOWORK=off` 解析 nested；无永久 `replace`；签名键只接受临时测试源；所有输出绑定 source/schema/runner/artifact digest。
- **Machine acceptance:** `make build-release-toolchain` → `build/reports/P37/report.json` 和 `junit.xml`。
- **Rollback point:** 删除本阶段工具链与临时容器/registry/proxy，不改变已发布 tag/package。
- **Definition of done:** 工具链在隔离测试环境能生成双 Module、三语言包、Container、SBOM/provenance、签名和 payload manifest，失败时清理临时状态。

## P38 — Read-only reproducible release dry-run

- **Type:** verify · spec
- **Status:** pending
- **Capability owner:** release-dry-run-verification
- **Goal:** 只读使用 P37 工具链验证双 Go Module、多语言包、Container、SBOM/provenance 与恢复流程，不在验证阶段补代码。
- **Scope:** clean clone、`GOWORK=off`、临时本地 Go proxy/package registries、测试签名键和 artifact manifest；工具链源码只读。
- **Dependencies:** P37
- **First-path invariants:** dry-run 前后 tracked tree 零变更；临时 proxy 先放根伪版本/RC 候选供 nested `GOWORK=off` 解析；无 `replace`；验证失败必须回 P37 修复后重跑。
- **Machine acceptance:** `make release-dry-run READ_ONLY=1` → `build/reports/P38/report.json` 和 `junit.xml`。
- **Rollback point:** 销毁临时 proxy/registry/测试键，不创建公开 tag/package；实现缺陷回 P37。
- **Definition of done:** 两次 clean build payload digest 一致，nested 精确解析根版本，SBOM/provenance/签名/撤回演练通过，tracked tree 不变。

## P39 — Code-complete verification

- **Type:** verify · review
- **Status:** pending
- **Capability owner:** release-readiness-review
- **Goal:** 聚合所有仓库内可自证要求，只宣告 code-complete，不宣告 public/v1 complete。
- **Scope:** P01–P38 报告、IR-01–IR-14 追溯、干净环境全量验证和已知风险。
- **Dependencies:** P38
- **First-path invariants:** 不伪造 C-002–C-004 或独立实现证据；聚合子报告数量/commit/digest 一致。
- **Machine acceptance:** `make validate-all` → `build/reports/P39/report.json` 和 `junit.xml`。
- **Rollback point:** 任一回归回首个引入阶段，修复后重跑后续报告。
- **Definition of done:** 内部可自证需求全绿，状态仅为 code-complete，P40/P44 仍阻塞。

## P40 — External public configuration gate

- **Type:** gate
- **Status:** blocked-external-evidence
- **Capability owner:** public-governance
- **Goal:** 验证真实项目域名、PyPI/npm 所有权、两名 Maintainer/恢复权限和私密安全入口。
- **Scope:** C-002–C-004 仓库外证据和脱敏签名摘要；不存原始 Credential。
- **Dependencies:** P39
- **First-path invariants:** 占位域名、内部 Fixture、单人自签不能通过；证据过期立即重新阻塞。
- **Machine acceptance:** `make external-config-gate EVIDENCE=<outside-repo-config-evidence>` → `build/reports/P40/report.json` 和 `junit.xml`。
- **Rollback point:** 所有权/安全入口失效则撤销 Gate，不继续再生成/发布。
- **Definition of done:** 每个外部项有可核验来源、时间、审核人和非敏感摘要。

## P41 — Real public namespace regeneration

- **Type:** implement
- **Status:** pending
- **Capability owner:** public-artifact-generation
- **Goal:** 写入 P40 真实配置，全量再生成 Schema/API/SDK/Fixture/Docs/Digest 制品。
- **Scope:** Schema `$id`、Event/Extension namespace、OpenAPI/AsyncAPI、三 SDK、Compatibility Matrix 和 artifact manifest。
- **Dependencies:** P40
- **First-path invariants:** 不手改生成物；真实 namespace 变更使旧内部报告失效；全量重生成而非字符替换。
- **Machine acceptance:** `make regenerate-public` → `build/reports/P41/report.json` 和 `junit.xml`。
- **Rollback point:** 恢复 P40 后/再生成前 commit，不发布部分更新制品。
- **Definition of done:** 占位 namespace 清零，全部派生物 provenance 指向同一 Schema/config digest，清洁重生成零 diff。

## P42 — Public namespace final verification

- **Type:** verify · review
- **Status:** pending
- **Capability owner:** public-release-verification
- **Goal:** 在真实 namespace 下只读重跑全套 contracts/conformance/resilience/release 检查。
- **Scope:** P41 制品、P16/P22/P25/P36/P38 验证套件和空环境安装；不修生成物。
- **Dependencies:** P41
- **First-path invariants:** 所有子报告指向同 source/schema/config/artifact digest；不带 `arop.invalid` 或本地 replace/workspace。
- **Machine acceptance:** `make verify-public-namespace` → `build/reports/P42/report.json` 和 `junit.xml`。
- **Rollback point:** 失败回 P41 修生成源并重新全量验证。
- **Definition of done:** 预期子报告数量、commit/digest 一致，公共名称、安装、Conformance、SBOM/provenance 全绿。

## P43 — Deliver v1 release candidate

- **Type:** deliver
- **Status:** pending
- **Capability owner:** v1-rc-delivery
- **Goal:** 交付给外部取证的最终 `v1.0.0-rc.N`，不用 v0.1 RC 代替 v1 证据对象。
- **Scope:** 根/嵌套 Go tag、Python/npm/CLI/Container、Schema/API Bundle、Runner、SBOM/provenance、digest manifest。
- **Dependencies:** P42
- **First-path invariants:** 先推根 `v1.0.0-rc.N`，proxy 可解析后再推同 source commit 的 `reference/control-plane/v1.0.0-rc.N`；nested 精确依赖根 RC，无 `replace`。
- **Machine acceptance:** `make deliver-v1-rc VERSION=v1.0.0-rc.N` → `build/reports/P43/report.json` 和 `junit.xml`。
- **Rollback point:** 废弃 RC 并发新 RC 号，不重写已发 tag/package。
- **Definition of done:** 外部空环境可下载/验签/安装/跑 Conformance，全部制品绑定 exact source/schema/runner/artifact digest。

## P44 — Independent implementation and partner evidence gate

- **Type:** gate
- **Status:** blocked-external-evidence
- **Capability owner:** external-conformance-review
- **Goal:** 验证独立 Runtime/Control Plane/Validator 和外部伙伴对 P43 最终 v1 RC 的真实互操证据。
- **Scope:** 至少两个独立 Runtime、一个非金运 Control Plane/Validator、三个外部设计伙伴证据；原始材料仓库外保存。
- **Dependencies:** P43
- **First-path invariants:** 每条证据绑定 `v1.0.0-rc.N` exact source commit、Schema Bundle Digest、Runner Digest、被测 Artifact Digest；内部 fork/模拟不算。
- **Machine acceptance:** `make external-evidence-gate RC=v1.0.0-rc.N EVIDENCE=<outside-repo-evidence>` → `build/reports/P44/report.json` 和 `junit.xml`。
- **Rollback point:** 任何 tracked 规范/Schema/transport/runner/security/compatibility 变化使证据失效，必须回 P41、产生新 v1 RC、重取证。
- **Definition of done:** 数量/独立性/来源可核验，四类 digest 与 exact v1 RC 一致，只提交脱敏签名摘要。

## P45 — Strictly read-only v1 freeze verification

- **Type:** verify · review
- **Status:** pending
- **Capability owner:** v1-freeze-review
- **Goal:** 在 P44 后对同一 RC 执行严格只读 freeze 验证，不关闭 RFC、不修 Matrix、不更改 tracked 制品。
- **Scope:** 只读比对 P43/P44 source/schema/runner/artifact digest、未解 blocker 快照和发布批准。
- **Dependencies:** P44
- **First-path invariants:** freeze 阶段 tracked tree 零变更；不关闭 RFC，不改 Compatibility Matrix；任何规范/Schema/transport/runner/security/compatibility 变化都回 P41–P44；不事后补签证据。
- **Machine acceptance:** `make verify-v1-freeze READ_ONLY=1` → `build/reports/P45/report.json` 和 `junit.xml`。
- **Rollback point:** 发现任何 tracked diff/blocker/digest 不一致即取消 freeze，回 P41 新建 RC 链。
- **Definition of done:** freeze 前后 tracked tree/source/schema/runner/artifact digest 不变，证据与批准指向同一 v1 RC。

## P46 — Deliver v1 from the approved source commit

- **Type:** deliver
- **Status:** pending
- **Capability owner:** v1-delivery
- **Goal:** 从 P43/P45 批准的同一 source commit 交付 v1，只允许 channel/tag/version metadata 变化，并证明 payload 等价。
- **Scope:** 正式 tag/package/channel metadata、payload-equivalence manifest、最终本地 Conformance、SBOM/provenance/签名和恢复记录。
- **Dependencies:** P45
- **First-path invariants:** 允许差异仅是由发布工具在 tracked source 之外注入的 RC→final 版本字符、发布 channel 和 tag 签名元数据，不创建新源码 commit；协议载荷、生成模型、二进制逻辑、Schema/API/Runner 必须字节或规范化等价。
- **Machine acceptance:** `make deliver-v1 VERSION=v1.0.0 APPROVED_RC=v1.0.0-rc.N` → `build/reports/P46/report.json` 和 `junit.xml`。
- **Rollback point:** 不重写已发制品；发布失败使用新 patch/errata，任何 payload 差异都回 P41–P45。
- **Definition of done:** root/nested tag 按顺序指向批准 source commit，payload-equivalence 机器通过，最终本地制品重跑 Conformance/SBOM/provenance 全绿。

# 4. 明确延后

gRPC、WebSocket、Portable Session Checkpoint、Hedged Execution、多区域调度、公共 Agent 市场、多租户 SaaS 和通用工作流引擎不阻塞 v1。Console 集成是下游独立计划，本仓库 P01–P46 不修改 `kinglucky-agent-console`。
