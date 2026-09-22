---
title: Agent Runtime Operations Protocol 开发与发布计划
status: execution-blueprint
updated: 2026-09-22
---

# 1. 执行规则

本计划将实施分为连续的 P01–P24。开发者必须先阅读 [DECISIONS](DECISIONS.md)、[IMPLEMENTATION_BLUEPRINT](IMPLEMENTATION_BLUEPRINT.md) 和相关领域规范，不能跳过 Gate，也不能把“代码写完”与“已达到公开 v1”混为一个状态。

状态只使用 `complete`、`in-progress`、`pending`、`blocked-user-evidence` 和 `blocked-external-evidence`。只有对应 JSON/JUnit 报告可重现且所有完成定义均满足时才可标记 `complete`。每个阶段的验收输出为 `build/reports/<phase>/report.json` 和 `junit.xml`；Gate 也必须把外部证据摘要转换为这两种机器报告，但不得用内部模拟伪造外部证据。

# 2. 里程碑与状态

| 里程碑 | 阶段 | 完成的真实含义 |
| --- | --- | --- |
| 规划冻结 | P01–P04 | 权威、冲突、布局、依赖和验收被审核，用户明确同意开始实现 |
| 可运行纵向闭环 | P05–P17 | 完成契约、Go 后端、SDK、参考实现、Conformance、故障/HA 和发布 dry-run |
| Code-complete | P18 | 内部可自证的要求全部通过，但尚未声称公开 RC/v1 |
| Public RC | P19–P21 | 真实公开配置完成、全量再生成并发布可验证 v0.1 RC |
| v1 evidence/freeze | P22–P23 | 独立实现与外部伙伴证据绑定 RC，再做 v1 冻结验证 |
| v1 delivery | P24 | 签名、可重现、带 SBOM/provenance 的正式交付 |

# 3. 阶段详情

## P01 — Contract authority and conflict closure

- **Type:** planning
- **Status:** complete
- **Goal:** 冻结权威链、14 条不可静默改写的要求、机器制品目录和全部已知冲突决议。
- **Scope:** `docs/DECISIONS.md`、`spec/artifact-manifest.yaml`、`spec/requirements.yaml`、`spec/conflicts.yaml` 和被冲突影响的规范文本。
- **Dependencies:** none
- **Built-in invariants:** Authoring strict/Consumer forward compatible；`$ref` 离线闭包；发布必须先校验再 Digest；不含 Console/飞书/厂商字段。
- **Machine acceptance:** `make spec-index-check` → `build/reports/spec-index-check/report.json` 和 `junit.xml`。
- **Rollback point:** 回到 P01 开始前的文档基线；不允许带未解冲突进入 P02。
- **Definition of done:** 14 条 statement 逐字保留，冲突零未解，Decision/链接/Schema 本地引用闭包报告全绿。

## P02 — Implementation blueprint freeze

- **Type:** planning
- **Status:** complete
- **Goal:** 冻结最终目录、双 Go Module、依赖 DAG、存储/迁移语义、测试与发布闭环。
- **Scope:** `DIRECTORY_STRUCTURE.md`、`IMPLEMENTATION_BLUEPRINT.md`、本计划、README/AGENTS/架构/SDK 导航与 Phase-2 检查器。
- **Dependencies:** P01
- **Built-in invariants:** 根公共 module + 唯一 `reference/control-plane` 嵌套 module；portable runner 在根 module；Conformance 语言中立；无厂商 Adapter；Console 隔离。
- **Machine acceptance:** `make blueprint-check` → `build/reports/blueprint-check/report.json` 和 `junit.xml`。
- **Rollback point:** 仅回退 P02 文档/检查器，不物理搬迁当前基线。
- **Definition of done:** 24 阶段连续无环、双 module/发布/Gate/报告约束可机器验证，需求映射只指向有效阶段。

## P03 — Independent plan audit

- **Type:** audit
- **Status:** pending
- **Goal:** 由未参与 P01/P02 撰写的审核者进行反向审查，找出过度范围、循环依赖、不可测验收和安全缺口。
- **Scope:** 只读全部核心文档与 `spec/*`，输出审核台账和 must-fix 闭环。
- **Dependencies:** P02
- **Built-in invariants:** 审核者不以内部模拟替代外部证据；不跳过必修项。
- **Machine acceptance:** `make planning-audit` → `build/reports/P03/report.json` 和 `junit.xml`。
- **Rollback point:** must-fix 返回 P01 或 P02 修改并重跑审核。
- **Definition of done:** 审核报告具有独立审核身份、exact commit、问题严重度，must-fix 为零。

## P04 — User implementation gate

- **Type:** user-gate
- **Status:** blocked-user-evidence
- **Goal:** 在任何物理重构或新业务实现前，获得用户对核心文档和计划的明确确认。
- **Scope:** P01–P03 报告、用户确认记录、确认时的 exact commit。
- **Dependencies:** P03
- **Built-in invariants:** “继续”必须能明确对应已展示的计划版本；无证据不得自动通过。
- **Machine acceptance:** `make gate-check GATE=P04 EVIDENCE=<user-approval-record>` → `build/reports/P04/report.json` 和 `junit.xml`。
- **Rollback point:** 用户要求改计划时回 P01/P02，审核后重新请求确认。
- **Definition of done:** 用户确认记录绑定计划 commit，Gate 报告成功；此前 P05 必须保持未开始。

## P05 — Physical repository refactor

- **Type:** implementation
- **Status:** pending
- **Goal:** 按冻结树迁移现有基线，建立双 module 和分层边界，不改变协议行为。
- **Scope:** 根 module、嵌套 Reference CP module、portable runner 骨架、`go.work.example`、路径/导入检查。
- **Dependencies:** P04
- **Built-in invariants:** 只有两个 `go.mod`；无永久 `replace`；公共 module 不导入 Reference `internal`。
- **Machine acceptance:** `make test-go-workspace` → `build/reports/P05/report.json` 和 `junit.xml`。
- **Rollback point:** 保留迁移前基线；路径映射不完整时整体回退，不长期并行两套实现。
- **Definition of done:** 两 module 在 `GOWORK=off` 分别测试，当前 CLI/基线行为不变，仓库分层检查通过。

## P06 — Foundation corrections and codegen spike

- **Type:** implementation
- **Status:** pending
- **Goal:** 修复先校验再 Digest、重复 JSON Key、错误码、Fixture 路径等基线债务，用代表 Schema 验证三语言 codegen。
- **Scope:** 公共验证内核、Digest Corpus、Go/Python/TypeScript 生成 spike、生成器锁版、漂移检查。
- **Dependencies:** P05
- **Built-in invariants:** 严格作者/兼容消费模式分离；所有 `$ref` 离线；生成代码禁止手改。
- **Machine acceptance:** `make test-foundation` → `build/reports/P06/report.json` 和 `junit.xml`。
- **Rollback point:** spike 不通过则更换生成器/映射策略，不扩大 Schema 生成面。
- **Definition of done:** 三语言编译/类型检查、JSON round-trip 和重新生成零 diff，Go/Node 对非法输入结论一致。

## P07 — Publication contracts and vertical slice

- **Type:** implementation
- **Status:** pending
- **Goal:** 完成 AgentVersion Bundle 的 Schema/API/Go 发布纵向路径。
- **Scope:** Manifest/Resource 契约、包内 Schema 闭包、Digest/Extension/Governance、发布仓储与 Audit。
- **Dependencies:** P06
- **Built-in invariants:** 发布过程禁网络 `$ref`；校验、Digest、不可变版本和 Audit 同事务。
- **Machine acceptance:** `make test-publication` → `build/reports/P07/report.json` 和 `junit.xml`。
- **Rollback point:** 停留在未发布 draft；不修改已发布 AgentVersion。
- **Definition of done:** SQLite/PostgreSQL 契约同过，并发/重放幂等，非法引用、Digest 冲突和 Governance 缺失按规则拒绝。

## P08 — Registry and discovery vertical slice

- **Type:** implementation
- **Status:** pending
- **Goal:** 实现自研 Lease/Revision/Watch/CAS/Fencing 注册发现。
- **Scope:** Runtime Instance/Session/Generation、Keepalive、Drain、Snapshot/Watch/Compaction/Resync、Discovery Filter、双库存储。
- **Dependencies:** P07
- **Built-in invariants:** 服务端 Clock、Generation Fencing、原始 Registry Event 不可变、Audit/Trace 从首条路径内建。
- **Machine acceptance:** `make test-registry-all` → `build/reports/P08/report.json` 和 `junit.xml`。
- **Rollback point:** 可回退通知/Cache，Ledger 修订号不回退；失效实例 fail closed。
- **Definition of done:** 旧 Session 被 fencing，Lease 过期即退出路由，Watch 丢通知能 Replay，压缩后能 Resync。

## P09 — Run, authorization, attempt and ticket

- **Type:** implementation
- **Status:** pending
- **Goal:** 原子完成授权、Run/Attempt 建立、路由与短期 Ticket 签发。
- **Scope:** Run/Attempt 状态机、Authz 快照、Dispatcher、JWKS/Token、Cancel/Deadline、Usage 骨架、Audit/Trace。
- **Dependencies:** P08
- **Built-in invariants:** 所有新调用先过 Control Plane；`run_id` 跨重试稳定；Attempt/Ticket/Fencing 逐次更新；浏览器不得 Direct。
- **Machine acceptance:** `make test-run-ticket` → `build/reports/P09/report.json` 和 `junit.xml`。
- **Rollback point:** 调度失败创建可审计失败/待重试状态，不删 Run 历史。
- **Definition of done:** 鉴权与建 Run 不存在 TOCTOU，Token claims/audience/scope/TTL/旋转契约全过，Cancel/Deadline 竞态可重现。

## P10 — Direct, proxy, event ledger and streaming

- **Type:** implementation
- **Status:** pending
- **Goal:** 用同一 Run/Event 语义交付 Direct 与 Proxy，实现断线恢复和最终结果。
- **Scope:** Agent Runtime/Event Ingest API、Event Session Token、SSE/Event Batch、Run Sequence、Snapshot/ResultRef、Usage、Inbox/Outbox、`effect_id`。
- **Dependencies:** P09
- **Built-in invariants:** 原始事件 append-only；Producer/Run Sequence 分离；终态不可逆；迟到事件仅审计；Cancel/Deadline/Usage/Effect 从首条路径完整处理。
- **Machine acceptance:** `make test-direct-proxy-streaming` → `build/reports/P10/report.json` 和 `junit.xml`。
- **Rollback point:** 停止新 Attempt/Stream，保留 Ledger 和 Final Snapshot；不回写历史事件。
- **Definition of done:** 中英文 UTF-8 offset、重复/乱序/冲突 Event、SSE Resume、Batch Partial Failure、Outbox 重试全过。

## P11 — Worker Pull vertical slice

- **Type:** implementation
- **Status:** pending
- **Goal:** 为无入站端点的通用 Worker 实现 Claim/Lease/Fencing/Complete。
- **Scope:** Worker API、长轮询、Capacity/Drain、Session Affinity、Attempt 重分配、通用 Pull Worker 参考。
- **Dependencies:** P10
- **Built-in invariants:** 无厂商专属 Adapter；旧 Worker 不得覆盖新 Attempt；Pull/Direct/Proxy 共用事件和终态。
- **Machine acceptance:** `make test-worker-pull` → `build/reports/P11/report.json` 和 `junit.xml`。
- **Rollback point:** 停止 Claim，等待 Lease 过期后安全重分配，不强行改写 Attempt。
- **Definition of done:** crash-before/after-accept、renew 超时、双完成竞态、fencing、drain 和 sticky 路由契约通过。

## P12 — Operations, security and retention

- **Type:** implementation
- **Status:** pending
- **Goal:** 完成生产级安全、运维、保留、备份和观测闭环。
- **Scope:** SSRF/DNS 重绑、限流/限额、密钥旋转、脱敏、Retention/Compaction、Backup/Restore、Readiness、OTel、SLO 报表。
- **Dependencies:** P11
- **Built-in invariants:** Token 类型不混用；Secret/Asset URL 不进日志；未完成 migration 不 ready；审计不依赖可丢通知。
- **Machine acceptance:** `make test-operations-security` → `build/reports/P12/report.json` 和 `junit.xml`。
- **Rollback point:** 配置或旋转失败时 fail closed，使用上一有效密钥重叠窗口/完整备份恢复。
- **Definition of done:** 安全反向契约、保留边界、备份恢复后 Contract Suite、审计/Trace 链路均通过。

## P13 — Standards interoperability

- **Type:** implementation
- **Status:** pending
- **Goal:** 在不重定义上游标准的前提下实现 A2A/MCP/ARD/CloudEvents/OTel 映射。
- **Scope:** Adapter、精确上游版本 Fixture、`exact/extended/lossy/unsupported` 报告、Trace 传播。
- **Dependencies:** P12
- **Built-in invariants:** RuntimeInstance/Lease/内网 Endpoint 不导出到公开发现；有损映射必须显式。
- **Machine acceptance:** `make interoperability` → `build/reports/P13/report.json` 和 `junit.xml`。
- **Rollback point:** 单个 Adapter 可独立禁用，不影响 Core/Profile 语义。
- **Definition of done:** 所有支持的上游版本有锁定记录、往返 Fixture、损失报告和 Trace 连续性。

## P14 — Public SDKs, reference agents and quickstart

- **Type:** implementation
- **Status:** pending
- **Goal:** 交付 Go SDK、Python Provider SDK、TypeScript Consumer，以及不依赖 Console 的参考 Agent/Quickstart。
- **Scope:** 三语言生成类型与手写 runtime、Go/Python HTTP Agent、Pull Worker、SQLite Quickstart、PostgreSQL production reference。
- **Dependencies:** P13
- **Built-in invariants:** SDK 不嵌入 Reference CP internal；Python 不是后端；TypeScript v1 为 Consumer；默认安全设置不降级。
- **Machine acceptance:** `make test-sdks && make quickstart-smoke` → `build/reports/P14/report.json` 和 `junit.xml`。
- **Rollback point:** 单语言包可回退到上一可生成版本，但不改写已冻结 wire 语义。
- **Definition of done:** 全新环境十分钟内流式 Run，三语言共用 Fixture，无 Console 前置，安装/升级文档完整。

## P15 — Portable and server conformance

- **Type:** verification
- **Status:** pending
- **Goal:** 交付语言中立场景和根 module portable runner，验证 Provider/Consumer/Registry/Worker/Control Plane Profile。
- **Scope:** `conformance/`、`cmd/arop-conformance`、Reference CP server driver、报告签名/摘要。
- **Dependencies:** P14
- **Built-in invariants:** runner 不依赖 Reference `internal`；外部实现只需黑盒 Endpoint/制品；不宣传未测 Profile。
- **Machine acceptance:** `make conformance` → `build/reports/P15/report.json` 和 `junit.xml`。
- **Rollback point:** 不符合的 Profile 从认证列表移除，保留失败证据，不降低 Fixture 适配实现。
- **Definition of done:** 级别、场景、预期错误和运行器版本入报告，自身参考实现通过声明的全部 Profile。

## P16 — Migration, fault injection and HA

- **Type:** verification
- **Status:** pending
- **Goal:** 用真实双库、多节点和故障注入证明恢复、幂等、Fencing 和迁移语义。
- **Scope:** 空库/N-1/幂等/并发/dirty/backup-restore；duplicate/replay/reorder/cancel/deadline/crash/partition；PostgreSQL 多节点。
- **Dependencies:** P15
- **Built-in invariants:** SQLite/PostgreSQL 语义一致非 SQL 一致；通知可丢而 Ledger 不可丢；终态不可覆盖。
- **Machine acceptance:** `make migration-test && make fault-test && make ha-test` → `build/reports/P16/report.json` 和 `junit.xml`。
- **Rollback point:** dirty migration 禁止 ready；破坏性变更使用验证过的 backup restore，不伪造 downgrade。
- **Definition of done:** 迁移矩阵全绿，故障后无重复副作用/终态翻转，多节点恢复指标有报告。

## P17 — Reproducible release dry-run

- **Type:** verification
- **Status:** pending
- **Goal:** 在不公开发布的情况下验证双 Go tag、多语言包、Container、SBOM、provenance 和恢复流程。
- **Scope:** clean clone、临时 Go proxy、`GOWORK=off`、本地 package registries、签名测试键、制品 digest manifest。
- **Dependencies:** P16
- **Built-in invariants:** 根 tag 先于嵌套 tag；两 tag 同 commit；嵌套 module 依赖精确根版本且无 `replace`。
- **Machine acceptance:** `make release-dry-run` → `build/reports/P17/report.json` 和 `junit.xml`。
- **Rollback point:** 销毁临时 registry/proxy 和测试签名材料，不创建公开 tag/package。
- **Definition of done:** 两次独立构建 digest 一致，SBOM/provenance 可验，发布/撤回 runbook 通过演练。

## P18 — Code-complete verification

- **Type:** verification-gate
- **Status:** pending
- **Goal:** 集中验证所有可由仓库内部自证的不变量，宣告 code-complete 而非 public/v1 complete。
- **Scope:** P01–P17 报告、IR-01–IR-14 追溯、干净环境全量测试、已知风险清单。
- **Dependencies:** P17
- **Built-in invariants:** 不将 C-002–C-004 或独立实现标记为完成；Console 不是验收依赖。
- **Machine acceptance:** `make validate-all` → `build/reports/P18/report.json` 和 `junit.xml`。
- **Rollback point:** 任一回归失败回到首个引入阶段，修复后重跑后续报告。
- **Definition of done:** 全量内部检查通过，制品与报告 digest 归档，状态仅标记 code-complete。

## P19 — External public-configuration gate

- **Type:** external-gate
- **Status:** blocked-external-evidence
- **Goal:** 以可核验外部证据确认项目域名、PyPI/npm 所有权、两名 Maintainer/恢复权限和私密安全入口。
- **Scope:** C-002–C-004、DNS/账户所有权证据、治理与安全联系记录。
- **Dependencies:** P18
- **Built-in invariants:** 内部 Fixture、占位域名或单人自我签字不能通过；敏感凭据不写入报告。
- **Machine acceptance:** `make external-config-gate EVIDENCE_DIR=<verified-records>` → `build/reports/P19/report.json` 和 `junit.xml`。
- **Rollback point:** 证据过期/所有权失效则 Gate 重新阻塞，不继续发布。
- **Definition of done:** 每个外部项有可核验来源、时间、审核人与非敏感摘要，P19 报告全绿。

## P20 — Public namespace regeneration

- **Type:** implementation-verification
- **Status:** pending
- **Goal:** 将 P19 确认的真实域名/包信息写入源，全量再生成并重跑所有验证。
- **Scope:** Schema `$id`、Event Type/Extension Namespace、OpenAPI/AsyncAPI、SDK、文档、Fixture、Digest/Compatibility Matrix。
- **Dependencies:** P19
- **Built-in invariants:** 不手工批量替换生成物；公共命名变更后旧内部报告不得复用。
- **Machine acceptance:** `make regenerate-public && make validate-all` → `build/reports/P20/report.json` 和 `junit.xml`。
- **Rollback point:** 恢复到 P19 后、再生成前 commit；未全绿不发 RC。
- **Definition of done:** 占位 namespace 清零，重生成零 diff，全量合同/跨语言/故障/HA/发布检查在真实命名空间下通过。

## P21 — Public v0.1 release candidate delivery

- **Type:** release
- **Status:** pending
- **Goal:** 发布可给外部团队验证的签名 v0.1 RC。
- **Scope:** 根/嵌套 Go tag、Python/npm/CLI/Container、Schema/API Bundle、SBOM、provenance、Conformance runner、升级说明。
- **Dependencies:** P20
- **Built-in invariants:** 先根 Go tag/proxy 可解析，再同 commit 嵌套 tag；所有制品绑定 source/schema digest。
- **Machine acceptance:** `make release-rc VERSION=<v0.1.0-rc.N>` → `build/reports/P21/report.json` 和 `junit.xml`。
- **Rollback point:** 撤回可变渠道/标记 RC 废弃；不重写已发 tag/package，使用新 RC。
- **Definition of done:** 清洁外部环境能下载、验签、安装、跑 Quickstart/Conformance，公开 digest manifest 可核验。

## P22 — Independent implementation and partner evidence gate

- **Type:** external-gate
- **Status:** blocked-external-evidence
- **Goal:** 证明 AROP 不是只有自己参考实现能通过的内部接口。
- **Scope:** 至少两个独立 Runtime 实现、一个非金运 Control Plane/Validator、三个外部设计伙伴的可验证报告。
- **Dependencies:** P21
- **Built-in invariants:** 证据绑定 exact commit SHA、Schema Bundle Digest、Runner Digest 和被测制品 Digest；内部 fork/模拟不算独立实现。
- **Machine acceptance:** `make external-evidence-gate RC=<exact-rc> EVIDENCE_DIR=<verified-records>` → `build/reports/P22/report.json` 和 `junit.xml`。
- **Rollback point:** 任何规范/Schema/传输/Runner/安全语义变化立即使相关证据失效，必须回 P20、产生新 RC，再重跑 P22。
- **Definition of done:** 满足数量和独立性，每条证据来源可核验且与 exact RC 的四类 digest 一致。

## P23 — Protocol v1 freeze verification

- **Type:** verification-gate
- **Status:** pending
- **Goal:** 在外部证据通过后冻结 v1 wire 语义、兼容性和治理承诺。
- **Scope:** RC 反馈/RFC 关闭、Compatibility Matrix、破坏性变更扫描、全套回归、证据有效性重检。
- **Dependencies:** P22
- **Built-in invariants:** 冻结后不原地修改已发 Schema；规范变更必须先使旧证据失效并重走 P20–P22。
- **Machine acceptance:** `make v1-freeze-check` → `build/reports/P23/report.json` 和 `junit.xml`。
- **Rollback point:** 存在未解 RFC/不兼容/证据失效时取消 freeze，回到首个受影响阶段。
- **Definition of done:** 无未解 blocker，全套报告指向同一 freeze commit/bundle，升级和弃用政策可执行。

## P24 — Protocol v1 signed delivery

- **Type:** release
- **Status:** pending
- **Goal:** 交付签名、可重现、可安装并有公开 Conformance 证据的 v1。
- **Scope:** 全部版本/tag/package/container、Schema/OpenAPI/AsyncAPI、SDK/CLI、SBOM/provenance、Changelog/迁移/安全/兼容报告。
- **Dependencies:** P23
- **Built-in invariants:** 双 Go tag 同 commit 且顺序正确；不重写已发制品；发布权限和恢复至少两人持有。
- **Machine acceptance:** `make release-v1 VERSION=<v1.x.y>` → `build/reports/P24/report.json` 和 `junit.xml`。
- **Rollback point:** 使用安全公告、废弃标记和新 patch；不删除/替换已发 tag、Schema 或包。
- **Definition of done:** 所有公开制品 digest/签名/SBOM/provenance 可验，Quickstart 与外部 Conformance 可重现，证据与 freeze commit 一致。

# 4. 延后且不影响 v1 的内容

- gRPC 和 WebSocket Transport Binding。
- Portable Session Checkpoint、Hedged Execution 和多区域调度。
- 公共 Agent 市场、多租户 SaaS 产品、通用工作流引擎。
- Console 集成：由下游单独计划，不进入本仓库 P01–P24 实施范围。

延后不等于没有扩展位置；提前实现必须经 RFC 修改 Decision、蓝图、要求映射和 Conformance。
