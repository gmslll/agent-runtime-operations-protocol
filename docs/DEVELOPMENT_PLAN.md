---
title: Agent Runtime Operations Protocol 开发与发布计划
status: planning-candidate
updated: 2026-09-22
---

# 1. 执行约定

本计划是连续 P01..Pn 的单 Agent 阶段 DAG。阶段类型只使用 paseo-epic 支持的 `implement`、`refactor`、`verify · review`、`verify · spec`、`gate` 和 `deliver`。`Components` 必须来自受控集合；`Artifacts owned` 必须引用 [`spec/artifact-manifest.yaml`](../spec/artifact-manifest.yaml) 中由该阶段唯一拥有的关键制品。机器检查依赖真实边界，不再把 Scope 字数当作“单 Agent 可完成”的证明。

每个阶段的 Make 验收必须生成 `build/reports/<phase>/report.json` 与 `junit.xml`；聚合目标的子报告数量、exact commit、输入/Checker Digest 必须一致。P03/P04/P42/P46 只校验真实外部证据，不自动伪造证据。原始外部证据保存在仓库外；Git 仅接受脱敏 canonical 内容摘要、可选 Ed25519 attestation 和机器报告，未验签的 SHA-256 只能称为内容摘要。

所有新增持久化阶段必须同时提交 SQLite/PostgreSQL migration，并在本阶段运行 `empty`、`N-1→N`、`idempotent`、`dirty` 子矩阵；P38 只聚合既有结果，不能首次补 migration。P41 前所有产物都是 private/dev snapshot；取消独立公共 v0.1 里程碑，首个公开候选仅为 P45 的 `v1.0.0-rc.N`。

# 2. 状态和里程碑

| 区间 | 里程碑 | 当前含义 |
| --- | --- | --- |
| P01–P04 | 规划、独立审计、用户 Gate | P01/P02 候选完成；等待 P03 重新审计和 P04 用户确认 |
| P05–P37 | 可运行实现 | 两 Module、Control Plane、SDK、互操作、Conformance 和部署 |
| P38–P41 | 私有验证 | resilience、release dry-run、code-complete；仍不是公共发行 |
| P42–P45 | public v1 RC | 外部配置、全量再生成、只读验证、首个公开 `v1.0.0-rc.N` |
| P46–P48 | 外部证据与 v1 | 证据绑定 RC commit A，审核 final overlay，再从 metadata-only commit B 发布 v1 |

# 3. 可调度阶段

## P01 — Contract authority and metadata integrity

- **Type:** implement
- **Status:** complete
- **Capability owner:** spec-governance
- **Components:** governance
- **Artifacts owned:** spec-index-validation, check-report-meta-schema, machine-report-verifier
- **Goal:** 建立 Decision 权威链、14 条不可变 `statement_original_zh`、冲突台账、制品 DAG 和规划 Meta-Schema 候选。
- **Scope:** `docs/DECISIONS.md`、`spec/*`、`scripts/spec-index-check.mjs`、公共 report writer/schema/verifier；不改业务实现。
- **Dependencies:** none
- **First-path invariants:** Authoring strict/Consumer forward compatible；离线 `$ref`；先校验再 Digest；制品路径唯一、DAG 无环、语言派生正确。
- **Machine acceptance:** `make spec-index-check` → `build/reports/P01/report.json` 和 `junit.xml`。
- **Rollback point:** 恢复上一份规划元数据；冲突或 DAG 错误时不进入 P02。
- **Definition of done:** Meta-Schema、路径、DAG、Decision/链接/引用闭包和反例探针全绿，报告带 commit/dirty/command/runtime/input/checker digest。

## P02 — Schedulable implementation blueprint

- **Type:** implement
- **Status:** complete
- **Capability owner:** implementation-planning
- **Components:** planning
- **Artifacts owned:** blueprint-validation
- **Goal:** 形成双 Go Module、组件边界、单 Agent 阶段 DAG、API 归属、机器验收和 v1 发布闭环候选。
- **Scope:** `DIRECTORY_STRUCTURE.md`、`IMPLEMENTATION_BLUEPRINT.md`、本计划、README/AGENTS/ARCHITECTURE/SDK 交叉引用和 blueprint checker。
- **Dependencies:** P01
- **First-path invariants:** 根公共 module + 唯一 Reference CP 嵌套 module；Conformance 语言中立；Console/厂商 Adapter 隔离。
- **Machine acceptance:** `make blueprint-check` → `build/reports/P02/report.json` 和 `junit.xml`。
- **Rollback point:** 仅回退规划文档/检查器，不物理搬迁目录。
- **Definition of done:** P01..Pn 连续无环，类型、组件、关键制品唯一 owner 和真实 Make 验收可追溯；P01–P04 目标当前存在，P05 后目标在实现前为 planned。

## P03 — Independent planning audit

- **Type:** verify · review
- **Status:** pending-review
- **Capability owner:** independent-reviewer
- **Components:** governance
- **Artifacts owned:** planning-audit-validation
- **Goal:** 由未参与 P01/P02 撰写的审核者验证需求覆盖、可调度性和必修项归零。
- **Scope:** 只读核心文档/规划元数据；输入为仓库外审计证据，仓库只生成脱敏 canonical 内容摘要报告和可选验签结果。
- **Dependencies:** P02
- **First-path invariants:** IR-01–IR-14 与 P01–最后阶段逐条 `PASS`；`must_fix_count=0` 且明细为空；subject commit 和 requirements/plan/blueprint digest 精确匹配；`summary_sha256` 重算一致。
- **Machine acceptance:** `make planning-audit EVIDENCE=<outside-repo-review> TRUSTED_KEYS=<outside-repo-key-registry>`，或人工 Gate 提供 `TRUSTED_CHANNEL_CONFIRMATION=<outside-repo-record>` → `build/reports/P03/report.json` 和 `junit.xml`。
- **Rollback point:** must-fix 非零时回 P01/P02 修复并重新独立审计，不编辑证据伪造通过。
- **Definition of done:** 14 条需求和全部阶段结果完整、digest 匹配、must-fix 为零；审核者独立性由受信 key 角色与 Ed25519 签名或人工可信渠道确认。

## P04 — User implementation gate

- **Type:** gate
- **Status:** blocked
- **Capability owner:** project-owner
- **Components:** governance
- **Artifacts owned:** user-gate-validation
- **Goal:** 在任何物理重构和新协议行为前获得用户对已审计计划的明确确认。
- **Scope:** P03 成功报告、计划 subject commit/digest、仓库外用户批准证据。
- **Dependencies:** P03
- **First-path invariants:** Gate 绑定已审计版本，IR-01–IR-14 与 P01–最后阶段逐条 `PASS`，零 must-fix，`summary_sha256` 可重算；工具只验证而不自动批准。
- **Machine acceptance:** `make gate-check GATE=P04 EVIDENCE=<outside-repo-approval> TRUSTED_KEYS=<outside-repo-key-registry>`，或人工 Gate 提供 `TRUSTED_CHANNEL_CONFIRMATION=<outside-repo-record>` → `build/reports/P04/report.json` 和 `junit.xml`。
- **Rollback point:** 用户要求修改时回 P02/P03；新 digest 使旧批准失效。
- **Definition of done:** canonical hash 可重算，批准者由受信 `project_owner` key 或人工可信渠道确认；此前 P05 保持未开始。

## P05 — Physical two-module refactor and local Go proxy bootstrap

- **Type:** refactor
- **Status:** pending
- **Capability owner:** repository-layout
- **Components:** repository, release
- **Artifacts owned:** go-module-proxy-bootstrap
- **Goal:** 建立最终双 Module 目录，并让未发布 root module 时 nested module 仍可在 `GOWORK=off` 下验证。
- **Scope:** 物理搬迁、两个 `go.mod`、`go.work.example`、最小本地 Go module proxy/bootstrap harness 和 CI 骨架；不改变 wire 行为。
- **Dependencies:** P04
- **First-path invariants:** 仅两个 `go.mod`；真实 `go.work` 默认不提交；bootstrap 先把 root pseudo-version 写入临时 proxy，再让 nested 精确 require 并以 `GOWORK=off` 测试；无永久 `replace`。
- **Machine acceptance:** `make test-go-workspace` → `build/reports/P05/report.json` 和 `junit.xml`。
- **Rollback point:** 回到迁移前 commit 并销毁临时 proxy，不长期维护双目录。
- **Definition of done:** root/nested 分别离线测试，nested 在 root 尚未公开时从本地 proxy 解析，仓库不存在第三 Module。

## P06 — Protocol foundation and base state-machine fixtures

- **Type:** implement
- **Status:** pending
- **Capability owner:** protocol-foundation
- **Components:** protocol
- **Artifacts owned:** error-fixtures, state-machine-fixtures, conformance-fixtures, sdk-go
- **Goal:** 完成 strict/forward 解析、Digest、错误、UTF-8 offset 和基础状态机 Fixture。
- **Scope:** `sdk/go/protocol`、通用 Fixture/Digest corpus、重复 JSON Key、错误码和基础迁移；不含生成模型。
- **Dependencies:** P05
- **First-path invariants:** Authoring strict 与 Consumer forward compatible 分 API；所有 `$ref` 离线；先验证再 Digest；基础 Fixture 是行为权威。
- **Machine acceptance:** `make test-protocol-foundation` → `build/reports/P06/report.json` 和 `junit.xml`。
- **Rollback point:** 任一跨语言 digest 或状态迁移分歧时停在 P06。
- **Definition of done:** 黄金/反例、JCS、中英文 offset、错误码和基础状态机结论一致。

## P07 — Reusable three-language codegen pipeline

- **Type:** implement
- **Status:** pending
- **Capability owner:** code-generation
- **Components:** codegen
- **Artifacts owned:** codegen-pipeline, generated-models, generated-python-models, generated-typescript-models
- **Goal:** 交付可复用、固定版本、可重复执行的 Go/Python/TypeScript 完整 codegen pipeline，而非一次性 spike。
- **Scope:** 代表 Schema spike 后批量生成、编译、round-trip、drift 检查和 provenance。
- **Dependencies:** P06
- **First-path invariants:** 生成代码不手改；先验证 union/nullable/format/Extension/离线 `$ref`；生成器/命令/输入输出 digest 可追踪。
- **Machine acceptance:** `make test-codegen-pipeline` → `build/reports/P07/report.json` 和 `junit.xml`。
- **Rollback point:** 生成器失败回代表 Schema 配置，不保留部分全量生成物。
- **Definition of done:** 三语言完整生成、编译/类型检查、round-trip 和 clean regenerate 零 diff。

## P08 — Control Plane HTTP/application and observability foundation

- **Type:** implement
- **Status:** pending
- **Capability owner:** control-plane-platform
- **Components:** control-plane, operations
- **Artifacts owned:** reference-control-plane, control-plane-platform-foundation
- **Goal:** 建立 Go Control Plane 配置、HTTP 装配、UoW、Clock/ID/Fault seam 与 base Audit/Trace ports/storage。
- **Scope:** `reference/control-plane/internal/{app,ports,adapters/http,observability}`；不含业务领域和 Credential/URL policy。
- **Dependencies:** P07
- **First-path invariants:** handler→app→domain/ports→adapter；request ID/trace/audit 从第一条路径存在；Clock/ID/Fault 可确定注入；readiness 不伪造。
- **Machine acceptance:** `make test-control-plane-platform` → `build/reports/P08/report.json` 和 `junit.xml`。
- **Rollback point:** 保留 P05 可启动骨架；装配失败不替换基线。
- **Definition of done:** 配置失败闭合，Audit/Trace 可查询且脱敏，Clock/ID/Fault seam 有确定性测试。

## P09 — Dual-database migrations and truthful readiness

- **Type:** implement
- **Status:** pending
- **Capability owner:** control-plane-storage
- **Components:** storage
- **Artifacts owned:** sqlite-migrations, postgres-migrations
- **Goal:** 建立 SQLite/PostgreSQL 语义一致的 migration engine、UoW backend 和真实 readiness。
- **Scope:** 双库 runner、版本/dirty 状态、backup/restore hooks、并发互斥和启动检查；不加入领域表。
- **Dependencies:** P08
- **First-path invariants:** 语义一致而非 SQL 一致；本阶段运行双库 `empty/N-1→N/idempotent/dirty` 子矩阵；dirty 或不兼容必须 not-ready。
- **Machine acceptance:** `make test-storage-migrations` → `build/reports/P09/report.json` 和 `junit.xml`。
- **Rollback point:** migration 不可逆时先恢复备份；未证明恢复前不升级。
- **Definition of done:** 双库子矩阵、并发互斥、backup/restore 和 readiness 故障用例全绿。

## P10 — Development identity, credential lifecycle and SecretRef resolver

- **Type:** implement
- **Status:** pending
- **Capability owner:** identity-secret-foundation
- **Components:** identity
- **Artifacts owned:** reference-secret-exchange
- **Goal:** 只实现 dev identity、Credential 发行/轮换/撤销和 reference-only SecretRef resolver adapter。
- **Scope:** AuthN middleware、Credential store/cache invalidation、SecretRef port/deployment adapter；不实现 URL、Asset 或通用 Secret value HTTP API。
- **Dependencies:** P09
- **First-path invariants:** Credential 短期、最小权限、可撤销；Secret 值不进协议/日志；Audit/Trace 复用 P08。
- **Machine acceptance:** `make test-identity-secrets` → `build/reports/P10/report.json` 和 `junit.xml`。
- **Rollback point:** 撤销测试 Credential 并停用 resolver，不留下明文。
- **Definition of done:** 发行/轮换/撤销/cache 失效和 SecretRef deny-by-default 通过；无 URL/Asset 职责混入。

## P11 — Publication and Control Plane public contracts

- **Type:** implement
- **Status:** pending
- **Capability owner:** publication-contracts
- **Components:** publication
- **Artifacts owned:** openapi-control-plane, openapi-bundle
- **Goal:** 冻结 Publication、Run、JWKS、Event Session、Asset token exchange 的公共 Control Plane OpenAPI 边界。
- **Scope:** `openapi/control-plane-v1.yaml`、相关 Schema/Fixture/错误/鉴权声明；不写 handler/store。
- **Dependencies:** P10
- **First-path invariants:** 公共和 reference-only API 分离；Secret value 不成为公共 API；所有 `$ref` 离线；未知安全语义失败闭合。
- **Machine acceptance:** `make test-publication-contracts` → `build/reports/P11/report.json` 和 `junit.xml`。
- **Rollback point:** 只回退合同源，不留下部分生成 SDK。
- **Definition of done:** OpenAPI lint、离线 bundle、正反例、授权矩阵和三语言 compile probe 通过。

## P12 — Publication service and Manifest endpoint security

- **Type:** implement
- **Status:** pending
- **Capability owner:** publication-service
- **Components:** publication, storage
- **Artifacts owned:** manifest-fixtures, publication-service
- **Goal:** 实现 Publication 纵向路径及 Manifest endpoint 静态 URL/离线 `$ref` 安全。
- **Scope:** domain/app/http/storage、Bundle/Digest、SQLite/PostgreSQL migration、scheme/host/IP 静态 allowlist；不做网络连接和 Asset broker。
- **Dependencies:** P11
- **First-path invariants:** strict→offline `$ref`→Digest→immutable version；静态拒绝 loopback/link-local/private/metadata/credential-in-URL；运行双库 `empty/N-1→N/idempotent/dirty` 子矩阵。
- **Machine acceptance:** `make test-publication-service` → `build/reports/P12/report.json` 和 `junit.xml`。
- **Rollback point:** 禁用新发布，不删除已审计 Bundle；migration 失败按 P09 恢复。
- **Definition of done:** 发布/读取/幂等/并发、静态 URL policy、双库子矩阵和 Digest 一致性通过。

## P13 — Asset broker and token exchange service

- **Type:** implement
- **Status:** pending
- **Capability owner:** asset-broker
- **Components:** assets, storage
- **Artifacts owned:** asset-fixtures, asset-broker-service
- **Goal:** 独立实现 Asset handler/storage/token exchange 和实际连接安全。
- **Scope:** AssetRef、上传/下载 token、storage adapter、SQLite/PostgreSQL migration、redirect hop 与连接时 DNS/IP 重检；不承载 SecretRef。
- **Dependencies:** P12
- **First-path invariants:** token 短期最小权限；每次连接及 redirect 重新校验 DNS/IP；大小/MIME/digest 限制；运行双库 `empty/N-1→N/idempotent/dirty` 子矩阵。
- **Machine acceptance:** `make test-asset-broker` → `build/reports/P13/report.json` 和 `junit.xml`。
- **Rollback point:** 撤销 token、隔离未确认对象，migration 按 P09 恢复。
- **Definition of done:** handler/storage/token 生命周期、SSRF/DNS rebinding/redirect、双库子矩阵和审计全绿。

## P14 — Registry domain, fixtures and persistence core

- **Type:** implement
- **Status:** pending
- **Capability owner:** registry-core
- **Components:** registry, storage
- **Artifacts owned:** registry-fixtures, registry-core-service
- **Goal:** 实现 Lease/Revision/CAS/Fencing 领域核心、权威 Fixture 和双库存储。
- **Scope:** domain/repository、Instance/Session/Generation、append-only Registry Event、SQLite/PostgreSQL migration；不含 HTTP/SDK。
- **Dependencies:** P13
- **First-path invariants:** 服务端 Clock；revision 与状态同事务；旧 generation 被 fencing；运行双库 `empty/N-1→N/idempotent/dirty` 子矩阵。
- **Machine acceptance:** `make test-registry-core` → `build/reports/P14/report.json` 和 `junit.xml`。
- **Rollback point:** Registry Event 不删不改；状态不确定时退出发现。
- **Definition of done:** 领域 Fixture、CAS/fencing、租约过期和双库子矩阵通过。

## P15 — Registry API, drain and Go Registry SDK

- **Type:** implement
- **Status:** pending
- **Capability owner:** registry-api-sdk
- **Components:** registry, sdk-go
- **Artifacts owned:** openapi-registry-runtime, openapi-discovery-runtime, go-registry-sdk
- **Goal:** 交付注册、续租、发现、Drain API 和 Go Registry SDK。
- **Scope:** Registry/Discovery OpenAPI、handler/app、Go client/keepalive/drain helper；不含 Watch/HA。
- **Dependencies:** P14
- **First-path invariants:** Credential 复用 P10；健康字段正交；Lease 失效立即不可发现；Drain 禁止新 Attempt；SDK 遵守 generation fencing。
- **Machine acceptance:** `make test-registry-api` → `build/reports/P15/report.json` 和 `junit.xml`。
- **Rollback point:** 停止新注册并 drain，保留租约审计。
- **Definition of done:** 黑盒 API、Go SDK、错误码、Drain 与过期可见性通过。

## P16 — Registry Watch, HA and recovery

- **Type:** implement
- **Status:** pending
- **Capability owner:** registry-recovery
- **Components:** registry, storage
- **Artifacts owned:** registry-recovery-service
- **Goal:** 实现 Watch/Revision/Compaction/Resync 和 PostgreSQL 多节点恢复。
- **Scope:** watch app/http、compaction watermark、leader/lock、backup/restore、SQLite/PostgreSQL migration 增量；不改 P14 事件语义。
- **Dependencies:** P15
- **First-path invariants:** snapshot+watch 无缝；compact 后 resync；旧主不能复活；新增持久化运行双库 `empty/N-1→N/idempotent/dirty` 子矩阵。
- **Machine acceptance:** `make test-registry-recovery` → `build/reports/P16/report.json` 和 `junit.xml`。
- **Rollback point:** 关闭 Watch 回全量 Snapshot；不回退 revision/event。
- **Definition of done:** duplicate/reorder/partition/compaction/restore 与双库子矩阵通过。

## P17 — Read-only Registry verification

- **Type:** verify · spec
- **Status:** pending
- **Capability owner:** registry-verification
- **Components:** registry
- **Artifacts owned:** none
- **Goal:** 只读验证 P14–P16 的领域、API、SDK、Watch、HA 和恢复证据。
- **Scope:** 既有制品与报告、独立黑盒 Registry suite；不修代码/Schema。
- **Dependencies:** P16
- **First-path invariants:** commit/input/checker/artifact digest 一致；失败回首个实现阶段。
- **Machine acceptance:** `make verify-registry` → `build/reports/P17/report.json` 和 `junit.xml`。
- **Rollback point:** 失败回 P14–P16 修复并重跑后续报告。
- **Definition of done:** 双库 Registry suite 与 migration 子矩阵证据完整，tracked tree 不变。

## P18 — Run authorization and durable lifecycle

- **Type:** implement
- **Status:** pending
- **Capability owner:** run-service
- **Components:** run, storage
- **Artifacts owned:** run-fixtures, run-lifecycle-service
- **Goal:** 实现 Run/Auth、durable Outbox、Cancel、Deadline、Usage、Audit、Trace 和 effect 语义。
- **Scope:** domain/app/http/storage、authz snapshot、outbox/effect records、SQLite/PostgreSQL migration；不含 Dispatcher。
- **Dependencies:** P17
- **First-path invariants:** Run 与 Outbox/Audit 同事务；Cancel/Deadline/Usage/Trace 首次路径内建；稳定 `effect_id`；运行双库 `empty/N-1→N/idempotent/dirty` 子矩阵。
- **Machine acceptance:** `make test-run-lifecycle` → `build/reports/P18/report.json` 和 `junit.xml`。
- **Rollback point:** 投递失败保留 Run/Audit/Outbox 可重试状态，不删历史。
- **Definition of done:** 授权拒绝、幂等创建、cancel/deadline race、effect 去重和双库子矩阵通过。

## P19 — Dispatcher, Attempt, Dispatch Ticket and JWKS

- **Type:** implement
- **Status:** pending
- **Capability owner:** dispatch-security
- **Components:** dispatch, storage
- **Artifacts owned:** dispatch-fixtures, dispatcher-ticket-service
- **Goal:** 实现候选选择、Attempt、短期 Dispatch Ticket 和 JWKS 生命周期。
- **Scope:** dispatcher/app/storage、attempt lease、ticket issuer/verifier、JWKS rotation、SQLite/PostgreSQL migration；不含 Event ingest。
- **Dependencies:** P18
- **First-path invariants:** ticket 绑定 run/attempt/audience/endpoint/mode/expiry；key rotation overlap；Attempt 递增；运行双库 `empty/N-1→N/idempotent/dirty` 子矩阵。
- **Machine acceptance:** `make test-dispatch-ticket` → `build/reports/P19/report.json` 和 `junit.xml`。
- **Rollback point:** 停止签发并撤销 key，保留 Attempt/Audit。
- **Definition of done:** 选择/重试、过期/错 audience、轮换和双库子矩阵通过。

## P20 — Immutable Event Ledger, Event Session and capacity release

- **Type:** implement
- **Status:** pending
- **Capability owner:** event-ledger
- **Components:** events, storage
- **Artifacts owned:** event-fixtures, event-ledger-service
- **Goal:** 实现 Event Session、ingest、durable Inbox、序列/终态投影，并保证终态与 capacity 释放同事务。
- **Scope:** event domain/app/http/storage、session token、producer/run sequence、SQLite/PostgreSQL migration；不含 SSE。
- **Dependencies:** P19
- **First-path invariants:** Event append-only；Inbox 去重与投影同事务；终态不可逆且含 Final Snapshot/ResultRef/Usage；终态写入与 Runtime capacity 释放同一 UoW；运行双库 `empty/N-1→N/idempotent/dirty` 子矩阵。
- **Machine acceptance:** `make test-event-ledger` → `build/reports/P20/report.json` 和 `junit.xml`。
- **Rollback point:** 投影可重建，Event 不删不改；容量不确定时不接新 Attempt。
- **Definition of done:** duplicate/out-of-order/late terminal、session scope、Final Usage、capacity 同事务释放和双库子矩阵通过。

## P21 — Direct/Proxy delivery, Go Provider SDK and reference agent

- **Type:** implement
- **Status:** pending
- **Capability owner:** go-provider-delivery
- **Components:** delivery, sdk-go, storage
- **Artifacts owned:** openapi-agent-runtime, go-provider-sdk, reference-http-agent, reference-agents
- **Goal:** 交付 Direct/Proxy、Go Provider SDK 和通用参考 Agent 的完整 provider-side 可靠路径。
- **Scope:** runtime handler/client、reference agent、provider durable Inbox/Outbox/effect store、SQLite/PostgreSQL migration、连接/redirect DNS/IP 重检；不含 SSE。
- **Dependencies:** P20
- **First-path invariants:** 浏览器仅 BFF/Proxy；每次连接及 redirect DNS/IP 重检；provider durable Inbox/Outbox、稳定 `effect_id` 去重、Cancel/Deadline/Usage/Trace、crash-before/after-effect retry 首次内建；运行双库 `empty/N-1→N/idempotent/dirty` 子矩阵。
- **Machine acceptance:** `make test-direct-proxy-provider` → `build/reports/P21/report.json` 和 `junit.xml`。
- **Rollback point:** 禁用 Direct 回 Proxy；副作用不确定时不自动重试 write/irreversible。
- **Definition of done:** Direct/Proxy、SSRF/DNS/redirect、provider crash/retry/effect 去重、cancel/deadline/usage/trace 和双库子矩阵通过。

## P22 — Structured streaming and AsyncAPI

- **Type:** implement
- **Status:** pending
- **Capability owner:** streaming-delivery
- **Components:** streaming
- **Artifacts owned:** asyncapi-agent-events, asyncapi-bundle
- **Goal:** 交付结构化事件 AsyncAPI、SSE、batch ingest/ACK 和断线续传。
- **Scope:** AsyncAPI、SSE handler/client、cursor/retention/resume、partial failure；复用 P20 Ledger 和 P21 Provider SDK。
- **Dependencies:** P21
- **First-path invariants:** Producer Sequence 与 Run Sequence 分离；Offset 统一；resume 超窗口返回明确错误；最终状态不靠 Delta 重建。
- **Machine acceptance:** `make test-streaming-resume` → `build/reports/P22/report.json` 和 `junit.xml`。
- **Rollback point:** 关闭 streaming 回同步 Result/Snapshot，不丢 Ledger。
- **Definition of done:** reconnect/replay/gap/duplicate/partial batch、慢消费者和终态恢复通过。

## P23 — Read-only Run and Delivery verification

- **Type:** verify · spec
- **Status:** pending
- **Capability owner:** run-delivery-verification
- **Components:** run, delivery, streaming
- **Artifacts owned:** none
- **Goal:** 只读验证 P18–P22 的 Run/Attempt/Ticket/Event/Provider/Direct/Proxy/Streaming 闭环。
- **Scope:** 既有报告和独立黑盒 suite；不修实现、Schema 或 migrations。
- **Dependencies:** P22
- **First-path invariants:** digest 一致；终态/capacity、effect、provider crash 证据不可缺失。
- **Machine acceptance:** `make verify-run-delivery` → `build/reports/P23/report.json` 和 `junit.xml`。
- **Rollback point:** 失败回 P18–P22 首个实现阶段并使后续报告失效。
- **Definition of done:** Direct/Proxy、流式恢复、双库与 Provider 可靠性证据全绿，tracked tree 不变。

## P24 — Worker Pull service

- **Type:** implement
- **Status:** pending
- **Capability owner:** worker-service
- **Components:** worker, storage
- **Artifacts owned:** openapi-worker-runtime, worker-fixtures, worker-pull-service
- **Goal:** 实现 Worker claim/lease/renew/complete/requeue 服务及双库存储。
- **Scope:** Worker OpenAPI、service/http/storage、SQLite/PostgreSQL migration；不含客户端 SDK。
- **Dependencies:** P23
- **First-path invariants:** claim 与 Attempt lease 原子；复用 P20 Inbox/Outbox/effect；旧 Worker 不覆盖新 Attempt；运行双库 `empty/N-1→N/idempotent/dirty` 子矩阵。
- **Machine acceptance:** `make test-worker-service` → `build/reports/P24/report.json` 和 `junit.xml`。
- **Rollback point:** 停止新 claim，让租约过期回收；不手工改 Attempt。
- **Definition of done:** crash-before/after-accept、lease reclaim、重复 complete 和双库子矩阵通过。

## P25 — Go Worker SDK and generic reference worker

- **Type:** implement
- **Status:** pending
- **Capability owner:** go-worker-client
- **Components:** worker, sdk-go
- **Artifacts owned:** go-worker-sdk, reference-worker
- **Goal:** 交付 Go Worker SDK 和无厂商依赖的参考 Worker。
- **Scope:** claim loop、renew/backoff、cancel/deadline、event/effect helper、graceful drain；复用 P24 存储契约，不新增表。
- **Dependencies:** P24
- **First-path invariants:** SDK 不导入 Reference internal；at-least-once 明示；退出前 drain；effect_id 跨 Attempt 稳定。
- **Machine acceptance:** `make test-go-worker-sdk` → `build/reports/P25/report.json` 和 `junit.xml`。
- **Rollback point:** 停止参考 Worker，服务端租约自动回收。
- **Definition of done:** 并发、backpressure、crash/restart、cancel/deadline、drain 和重复完成通过。

## P26 — Read-only operations and security verification

- **Type:** verify · review
- **Status:** pending
- **Capability owner:** operations-security-review
- **Components:** operations
- **Artifacts owned:** none
- **Goal:** 只读验证前序首次内建的安全、审计、恢复和可靠性能力，不在本阶段补能力。
- **Scope:** P08–P25 报告、Credential/Secret、URL/Asset/Provider SSRF、Audit/Trace、备份恢复和限额套件。
- **Dependencies:** P25
- **First-path invariants:** 只读；缺失回对应首次实现阶段；敏感值不进报告。
- **Machine acceptance:** `make verify-operations-security` → `build/reports/P26/report.json` 和 `junit.xml`。
- **Rollback point:** 阻断发布并回缺陷 owner；不在 verify 中打补丁。
- **Definition of done:** Credential、DNS/IP/redirect、审计/追踪、恢复和权限回归全绿。

## P27 — Python Provider SDK, ASGI runtime and package primitive

- **Type:** implement
- **Status:** pending
- **Capability owner:** python-provider
- **Components:** sdk-python
- **Artifacts owned:** sdk-python, python-package-primitive
- **Goal:** 交付 Python Provider/ASGI、P21 可靠语义绑定和供 P39 调用的 Python 打包 primitive。
- **Scope:** generated models、provider middleware、ASGI SSE、durable store port、wheel/sdist primitive；不发布包。
- **Dependencies:** P26
- **First-path invariants:** strict/forward API 分离；复用 Inbox/Outbox/effect；primitive 输出 deterministic manifest，不含 registry 凭据。
- **Machine acceptance:** `make test-python-provider` → `build/reports/P27/report.json` 和 `junit.xml`。
- **Rollback point:** 删除候选 wheel/sdist，不影响 Go 路径。
- **Definition of done:** Python contracts/provider crash-resume、clean build/install 通过。

## P28 — TypeScript Consumer, BFF/Web and npm package primitive

- **Type:** implement
- **Status:** pending
- **Capability owner:** typescript-consumer
- **Components:** sdk-typescript
- **Artifacts owned:** sdk-typescript, npm-package-primitive
- **Goal:** 交付 TypeScript Consumer/reducer、BFF/Web 示例和供 P39 调用的 npm 打包 primitive。
- **Scope:** generated models、SSE reducer/resume、BFF ticket shielding、Web sample、npm pack primitive；不实现浏览器 Direct，不发布包。
- **Dependencies:** P27
- **First-path invariants:** 浏览器不持有 Dispatch/Event Token；未知可选事件前向兼容；primitive 可重现且不含 registry 凭据。
- **Machine acceptance:** `make test-typescript-consumer` → `build/reports/P28/report.json` 和 `junit.xml`。
- **Rollback point:** 删除候选 tarball/Web build，不影响服务端。
- **Definition of done:** reducer replay、BFF auth、Web recovery、npm clean pack/install 通过。

## P29 — A2A interoperability adapter

- **Type:** implement
- **Status:** pending
- **Capability owner:** interop-a2a
- **Components:** interop
- **Artifacts owned:** interoperability-adapters, a2a-adapter
- **Goal:** 实现 A2A task/message/artifact 与 AROP Run/Event 的显式映射。
- **Scope:** adapter、fixtures、version pin、loss table；不改变 Core。
- **Dependencies:** P28
- **First-path invariants:** 不伪造无损映射；identity/effect/cancel/terminal 差异显式记录。
- **Machine acceptance:** `make test-interop-a2a` → `build/reports/P29/report.json` 和 `junit.xml`。
- **Rollback point:** 移除可选 adapter，不改 Core wire。
- **Definition of done:** 固定 upstream 版本 Fixture 与 loss/negative tests 通过。

## P30 — MCP interoperability example

- **Type:** implement
- **Status:** pending
- **Capability owner:** interop-mcp
- **Components:** interop
- **Artifacts owned:** mcp-adapter
- **Goal:** 实现 MCP 工具/资源调用与 AROP tool/effect/trace 的边界示例。
- **Scope:** adapter、fixtures、authorization/trace mapping；MCP 不承担 Runtime 注册发现。
- **Dependencies:** P29
- **First-path invariants:** MCP session 不冒充 Run/Attempt；write/irreversible 传播稳定 effect_id。
- **Machine acceptance:** `make test-interop-mcp` → `build/reports/P30/report.json` 和 `junit.xml`。
- **Rollback point:** 禁用 adapter，不影响协议执行。
- **Definition of done:** read/write 工具、资源、错误/cancel/trace 与副作用去重用例通过。

## P31 — ARD catalog interoperability

- **Type:** implement
- **Status:** pending
- **Capability owner:** interop-ard
- **Components:** interop
- **Artifacts owned:** ard-adapter
- **Goal:** 实现 ARD/AI Catalog 描述到 AROP Definition/Version 的导入导出。
- **Scope:** adapter、fixtures、capability/security/version loss table；不引入公共市场。
- **Dependencies:** P30
- **First-path invariants:** Catalog discovery 不等于 Runtime readiness；不可表达字段进入 Extension/loss report。
- **Machine acceptance:** `make test-interop-ard` → `build/reports/P31/report.json` 和 `junit.xml`。
- **Rollback point:** 停止同步，不删除本地版本历史。
- **Definition of done:** round-trip、版本冲突、未知字段和 loss report 通过。

## P32 — CloudEvents and OpenTelemetry mapping

- **Type:** implement
- **Status:** pending
- **Capability owner:** interop-observability
- **Components:** interop, operations
- **Artifacts owned:** cloudevents-otel-mapping
- **Goal:** 固化 CloudEvents envelope 归属与 OpenTelemetry trace/metric 语义。
- **Scope:** mapping library、semantic conventions、fixtures/exporter tests；不建立第二套 Event 权威。
- **Dependencies:** P31
- **First-path invariants:** AROP Event ID/sequence/terminal 仍由 AROP 定义；Trace 与 Audit 相关但不互相替代。
- **Machine acceptance:** `make test-interop-observability` → `build/reports/P32/report.json` 和 `junit.xml`。
- **Rollback point:** 禁用 exporter，不丢 Event/Audit。
- **Definition of done:** envelope round-trip、context propagation、sampling/redaction 和 metric cardinality 通过。

## P33 — Portable root Go conformance runner

- **Type:** implement
- **Status:** pending
- **Capability owner:** portable-conformance
- **Components:** conformance
- **Artifacts owned:** portable-conformance-runner
- **Goal:** 在 root module 交付语言中立 Fixture 的 portable runner；Fixture 从 P06 起随领域增量形成，而非本阶段首次出现。
- **Scope:** `cmd/arop-conformance`、profile/scenario loader、JSON/JUnit reporter；不导入 Reference internal。
- **Dependencies:** P32
- **First-path invariants:** 离线运行；Fixture digest 固定；第三方可复用；Runner 不成为行为权威。
- **Machine acceptance:** `make test-portable-conformance` → `build/reports/P33/report.json` 和 `junit.xml`。
- **Rollback point:** 回退 runner，不删历史 Fixture。
- **Definition of done:** 多 profile 正反例、过滤、超时和报告 Schema 通过。

## P34 — Nested Control Plane conformance driver

- **Type:** implement
- **Status:** pending
- **Capability owner:** server-conformance
- **Components:** conformance, control-plane
- **Artifacts owned:** server-conformance-driver
- **Goal:** 在唯一 nested module 交付 Reference Control Plane 黑盒 driver。
- **Scope:** server lifecycle、SQLite/PostgreSQL fixtures、root runner invocation；不复制 portable logic。
- **Dependencies:** P33
- **First-path invariants:** root runner 不导入 nested；driver 对双库执行相同 scenario；测试密钥与生产分离。
- **Machine acceptance:** `make test-server-conformance` → `build/reports/P34/report.json` 和 `junit.xml`。
- **Rollback point:** 回退 driver，不改 Fixture 语义。
- **Definition of done:** 两数据库 Control Plane Profile 黑盒通过，无第三 Module。

## P35 — Fault and HA drivers

- **Type:** implement
- **Status:** pending
- **Capability owner:** fault-ha-harness
- **Components:** fault-ha, conformance
- **Artifacts owned:** fault-ha-harness
- **Goal:** 交付可确定注入 duplicate/drop/reorder/delay/crash/partition 和 PostgreSQL 多节点切换的 driver。
- **Scope:** fault scenarios、Reference FaultHook adapter、故障代理/多节点 harness；生产默认 no-op。
- **Dependencies:** P34
- **First-path invariants:** FaultHook 仅预定义 checkpoint；生产不暴露远程 fault API；seed/Clock/ID 可重现。
- **Machine acceptance:** `make test-fault-ha-drivers` → `build/reports/P35/report.json` 和 `junit.xml`。
- **Rollback point:** 禁用 harness/build tag，不影响生产二进制。
- **Definition of done:** 故障场景可同 seed 复现并生成时间线/节点/digest 报告。

## P36 — SQLite Quickstart composition

- **Type:** implement
- **Status:** pending
- **Capability owner:** sqlite-quickstart
- **Components:** quickstart
- **Artifacts owned:** quickstart
- **Goal:** 只编排前序已有 Go/Python/Worker/Web 示例形成 SQLite 单进程 Quickstart。
- **Scope:** compose/config/sample manifest/start-stop/smoke；禁止在本阶段新写四套示例或协议行为。
- **Dependencies:** P35
- **First-path invariants:** 无真实 Credential；组件引用 P21/P25/P27/P28 制品；默认安全不降级。
- **Machine acceptance:** `make quickstart-smoke` → `build/reports/P36/report.json` 和 `junit.xml`。
- **Rollback point:** 销毁临时进程/SQLite/测试凭据，不触及用户数据。
- **Definition of done:** 全新环境十分钟内完成 publish/register/run/stream，编排不含重复实现。

## P37 — PostgreSQL production reference deployment

- **Type:** implement
- **Status:** pending
- **Capability owner:** production-deployment
- **Components:** deployment
- **Artifacts owned:** production-reference-deployment, container-build-primitive
- **Goal:** 交付 PostgreSQL production reference、Container build primitive 和升级/恢复 runbook。
- **Scope:** deployment manifests、probes、container primitive、migration/backup/restore/rollback runbook；不实现语言包打包或 release orchestration。
- **Dependencies:** P36
- **First-path invariants:** 使用前序 migration；镜像非 root、最小能力、固定 base digest；升级前备份和恢复演练。
- **Machine acceptance:** `make production-reference-smoke` → `build/reports/P37/report.json` 和 `junit.xml`。
- **Rollback point:** 回滚 deployment revision/镜像并按 runbook 恢复备份。
- **Definition of done:** PostgreSQL 部署、container clean build、滚动升级、备份恢复和回滚演练通过。

## P38 — Read-only resilience verification

- **Type:** verify · review
- **Status:** pending
- **Capability owner:** resilience-verification
- **Components:** fault-ha, storage
- **Artifacts owned:** none
- **Goal:** 只读聚合所有 migration 子矩阵、故障注入、双库、备份恢复和 HA 证据。
- **Scope:** P09/P12/P13/P14/P16/P18/P19/P20/P21/P24/P34/P35/P37 报告；不改实现或 migration。
- **Dependencies:** P37
- **First-path invariants:** 报告 digest 一致；不能在本阶段首次补 `empty/N-1→N/idempotent/dirty`。
- **Machine acceptance:** `make verify-resilience` → `build/reports/P38/report.json` 和 `junit.xml`。
- **Rollback point:** 回首个缺陷阶段并使后续聚合失效。
- **Definition of done:** 全部持久化阶段双库子矩阵、replay/cancel/fencing/partition/HA/backup-restore 证据全绿，tracked tree 不变。

## P39 — Release orchestration and evidence toolchain

- **Type:** implement
- **Status:** pending
- **Capability owner:** release-engineering
- **Components:** release, governance
- **Artifacts owned:** release-engineering-toolchain, external-config-evidence-schema, external-config-validator, public-release-aggregator, external-conformance-evidence-schema, external-conformance-validator, freeze-overlay-checker, final-delivery-checker, release-report-integration
- **Goal:** 实现后期发布编排、供应链证明、外部证据验证和报告聚合工具，不重新实现各语言/Container build primitive。
- **Scope:** 调用 P05 Go proxy、P27 Python、P28 npm、P37 Container primitive；实现 P42/P44/P46/P47/P48 checker、通用 report schema/verifier 集成、SBOM/provenance/signing orchestration。
- **Dependencies:** P38
- **First-path invariants:** 原始 Gate 证据仓库外；只有受信 key registry 正确角色验签或人工可信渠道建立真实性；内容 hash 不是签名；输入/输出 digest 完整。
- **Machine acceptance:** `make build-release-toolchain` → `build/reports/P39/report.json` 和 `junit.xml`。
- **Rollback point:** 删除编排/临时 registry/test key，不改 primitives，不创建公开发行。
- **Definition of done:** 后期 schema/validator/aggregator/freeze/delivery 工具与反例齐全，SBOM/provenance/signing 和 report verifier 集成通过。

## P40 — Read-only reproducible release dry-run

- **Type:** verify · spec
- **Status:** pending
- **Capability owner:** release-dry-run-verification
- **Components:** release
- **Artifacts owned:** none
- **Goal:** 只读使用 P39 编排工具验证双 Module、多语言包、Container、SBOM/provenance/signing 和撤回流程。
- **Scope:** clean clone、`GOWORK=off`、临时 proxy/registries/test key、P05/P27/P28/P37 primitives；工具源码只读。
- **Dependencies:** P39
- **First-path invariants:** tracked tree 零变更；P41 前仅 private/dev snapshot；不得在 verify 阶段补代码；失败回对应 primitive owner 或 P39。
- **Machine acceptance:** `make release-dry-run READ_ONLY=1` → `build/reports/P40/report.json` 和 `junit.xml`。
- **Rollback point:** 销毁临时 proxy/registry/key，不创建公开 tag/package。
- **Definition of done:** clean build digest 一致、nested 解析、安装、SBOM/provenance/signing/撤回和报告复验通过。

## P41 — Code-complete verification

- **Type:** verify · review
- **Status:** pending
- **Capability owner:** release-readiness-review
- **Components:** governance, release
- **Artifacts owned:** none
- **Goal:** 聚合仓库内可自证要求，只宣告 private code-complete，不宣告 public/v1 complete。
- **Scope:** P01–P40 报告、IR-01–IR-14 追溯、clean environment 和已知风险；不修实现。
- **Dependencies:** P40
- **First-path invariants:** 不伪造外部配置/伙伴证据；取消独立公共 v0.1；首个公开候选必须是 P45 v1 RC。
- **Machine acceptance:** `make validate-all` → `build/reports/P41/report.json` 和 `junit.xml`。
- **Rollback point:** 回首个失败阶段并重跑后续报告。
- **Definition of done:** 内部要求全绿，状态仅 `private code-complete`，P42/P46 保持外部阻塞。

## P42 — External public configuration gate

- **Type:** gate
- **Status:** blocked-external-evidence
- **Capability owner:** public-governance
- **Components:** governance
- **Artifacts owned:** none
- **Goal:** 用 P39 validator 验证真实域名、PyPI/npm 所有权、两名 Maintainer/恢复权限和私密安全入口。
- **Scope:** 仓库外证据、可信 key registry/人工渠道和脱敏 canonical 摘要；不存原始 Credential。
- **Dependencies:** P41
- **First-path invariants:** 占位域名、内部 Fixture、单人自签不能通过；证据过期立即阻塞。
- **Machine acceptance:** `make external-config-gate EVIDENCE=<outside-repo-config-evidence> TRUSTED_KEYS=<outside-repo-key-registry>` → `build/reports/P42/report.json` 和 `junit.xml`。
- **Rollback point:** 所有权/安全入口失效即撤销 Gate。
- **Definition of done:** 外部项有来源、角色验签或人工渠道、时间和非敏感摘要。

## P43 — Real public namespace regeneration

- **Type:** implement
- **Status:** pending
- **Capability owner:** public-artifact-generation
- **Components:** codegen, release
- **Artifacts owned:** public-namespace-regeneration
- **Goal:** 写入 P42 真实配置并用 P07 pipeline 全量再生成公共制品。
- **Scope:** Schema `$id`、Event/Extension namespace、OpenAPI/AsyncAPI、三 SDK、Fixture/Docs/Digest/Compatibility Matrix；不手改生成物。
- **Dependencies:** P42
- **First-path invariants:** 真实 namespace 使旧报告失效；全量生成而非字符串替换；派生物指向同 config/schema digest。
- **Machine acceptance:** `make regenerate-public` → `build/reports/P43/report.json` 和 `junit.xml`。
- **Rollback point:** 回 P42 后、生成前 commit，不发布部分更新。
- **Definition of done:** 占位 namespace 清零，clean regenerate 零 diff，全部 provenance 一致。

## P44 — Read-only public namespace verification

- **Type:** verify · review
- **Status:** pending
- **Capability owner:** public-release-verification
- **Components:** release, conformance
- **Artifacts owned:** none
- **Goal:** 使用 P39 public aggregator 在真实 namespace 下只读重跑 contracts/conformance/resilience/release 检查。
- **Scope:** P43 制品、P17/P23/P26/P38/P40 套件和空环境安装；不修生成物。
- **Dependencies:** P43
- **First-path invariants:** 子报告 digest 一致；无 `arop.invalid`、本地 replace/workspace 或 private snapshot 依赖。
- **Machine acceptance:** `make verify-public-namespace READ_ONLY=1` → `build/reports/P44/report.json` 和 `junit.xml`。
- **Rollback point:** 失败回 P43 修生成源并全量重验。
- **Definition of done:** 公共命名、安装、Conformance、resilience、SBOM/provenance 全绿，tracked tree 不变。

## P45 — Deliver v1 release candidate from commit A

- **Type:** deliver
- **Status:** pending
- **Capability owner:** v1-rc-delivery
- **Components:** release
- **Artifacts owned:** none
- **Goal:** 从 source commit A 发布首个公开候选 `v1.0.0-rc.N`，作为外部取证唯一对象。
- **Scope:** root/nested Go RC tags、Python/npm/CLI/Container、Schema/API/Runner、SBOM/provenance 和 digest manifest。
- **Dependencies:** P44
- **First-path invariants:** commit A 的 nested `go.mod` 精确 require root `v1.0.0-rc.N`；先 root RC、proxy 可解析后再 nested RC；无 `replace`；不用 v0.1 或本地伪制品取证。
- **Machine acceptance:** `make deliver-v1-rc VERSION=v1.0.0-rc.N` → `build/reports/P45/report.json` 和 `junit.xml`。
- **Rollback point:** 废弃 RC 并使用新 RC 号，不重写 tag/package。
- **Definition of done:** 外部空环境可下载/验签/安装/跑 Conformance，制品绑定 commit A/source/schema/runner/artifact digest。

## P46 — Independent implementation and partner evidence gate

- **Type:** gate
- **Status:** blocked-external-evidence
- **Capability owner:** external-conformance-review
- **Components:** governance, conformance
- **Artifacts owned:** none
- **Goal:** 使用 P39 validator 验证独立 Runtime/Control Plane/Validator 和伙伴对 P45 commit A v1 RC 的真实证据。
- **Scope:** 至少两个独立 Runtime、一个非金运 Control Plane/Validator、三个外部伙伴；原始材料仓库外。
- **Dependencies:** P45
- **First-path invariants:** 每条证据绑定 RC、commit A、Schema Bundle、Runner、Artifact digest；内部 fork/模拟不算；可信 key/人工渠道规则一致。
- **Machine acceptance:** `make external-evidence-gate RC=v1.0.0-rc.N EVIDENCE=<outside-repo-evidence> TRUSTED_KEYS=<outside-repo-key-registry>` → `build/reports/P46/report.json` 和 `junit.xml`。
- **Rollback point:** tracked 规范/Schema/transport/runner/security/compatibility 变化使证据失效，回 P43 产生新 RC 并重取证。
- **Definition of done:** 数量/独立性/来源可核验，digest 指向 commit A v1 RC，只提交脱敏 canonical 摘要与有效 attestation。

## P47 — Read-only freeze and deterministic final overlay verification

- **Type:** verify · review
- **Status:** pending
- **Capability owner:** v1-freeze-overlay-review
- **Components:** release, governance
- **Artifacts owned:** none
- **Goal:** 冻结 commit A，并在临时树生成、审核确定性 final release overlay 和预期 commit B tree digest。
- **Scope:** 使用 P39 checker；overlay 仅含版本/channel/tag 元数据、nested `go.mod` root 依赖 `v1.0.0-rc.N→v1.0.0` 及必要 lock/checksum；tracked tree 保持不变。
- **Dependencies:** P46
- **First-path invariants:** 不关闭 RFC、不修 Matrix；Schema/API/Runner/生成模型/二进制逻辑不得变；计算 approved overlay digest、expected B tree digest、payload equivalence，以签名 A→B equivalence attestation 连接 P46 证据。
- **Machine acceptance:** `make verify-v1-freeze READ_ONLY=1 APPROVED_RC=v1.0.0-rc.N FINAL=v1.0.0` → `build/reports/P47/report.json` 和 `junit.xml`。
- **Rollback point:** 非白名单 diff 或 digest 不一致即取消 freeze，回 P43–P46。
- **Definition of done:** 临时树可重现同一 overlay/tree digest，白名单差异与 payload equivalence 通过，commit A 工作树未变。

## P48 — Deliver exact verified final release commit B

- **Type:** deliver
- **Status:** pending
- **Capability owner:** v1-delivery
- **Components:** release
- **Artifacts owned:** none
- **Goal:** 精确应用 P47 已审核 overlay 创建 release-metadata-only commit B，并从 B 发布 final v1。
- **Scope:** 应用 approved overlay、核对 B tree digest、创建 commit/tags、root final→proxy 可解析→nested final、Python/npm/Container channel；不允许临场修改 overlay。
- **Dependencies:** P47
- **First-path invariants:** B tree digest 等于 P47 approved value；A→B 仅白名单 metadata/nested dependency/lock-checksum 差异；先 root `v1.0.0`，proxy 可解析后再 nested final；最终制品重跑 Conformance/SBOM/provenance。
- **Machine acceptance:** `make deliver-v1 VERSION=v1.0.0 APPROVED_RC=v1.0.0-rc.N APPROVED_OVERLAY=<P47-attestation>` → `build/reports/P48/report.json` 和 `junit.xml`。
- **Rollback point:** tree digest 或 payload 不符立即停止且不重写制品；回 P47 审核新 overlay，协议/逻辑变更则回 P43 重走 RC/取证。
- **Definition of done:** commit B 匹配已审核 tree digest，root/nested final 顺序发布，A→B 签名 equivalence attestation 有效，最终 Conformance/SBOM/provenance 全绿。

# 4. 明确延后

gRPC、WebSocket、Portable Session Checkpoint、Hedged Execution、多区域调度、公共 Agent 市场、多租户 SaaS 和通用工作流引擎不阻塞 v1。Console 集成是下游独立计划，本仓库 P01–P48 不修改 `kinglucky-agent-console`。
