---
title: 可靠性、高可用与运维规范
status: draft
updated: 2026-09-21
---

# 1. 可靠性目标

协议必须让实现能够回答：

- 请求是否被接收？
- 是否可能重复执行？
- 当前哪个 Attempt 拥有执行权？
- 断线后从哪里继续？
- 哪些结果是迟到或被 Fenced 的？
- 哪个实例、版本和发布产生了结果？
- 故障恢复是否会产生重复副作用？

# 2. 交付保证

## 2.1 至少一次

以下操作采用至少一次：

- Runtime 注册和 Keepalive。
- Run Dispatch。
- Worker Claim 后的执行交付。
- Event Batch。
- Command。

发送方在未收到明确 ACK 时可以重试；接收方必须幂等。

## 2.2 不承诺恰好一次

网络超时无法判断对端是否已提交。协议通过以下机制降低重复影响：

```text
Idempotency-Key
run_id / attempt_id
event_id
command_id
effect_id
generation
fencing_token
transactional inbox/outbox
```

# 3. 幂等作用域

| 键 | 作用域 |
| --- | --- |
| External Idempotency Key | 调用主体 + Agent + Channel + 有效窗口 |
| Attempt ID | 单个 Run 的一次执行尝试 |
| Event ID | Event Source 内唯一 |
| Batch ID | Event Ingest Endpoint 内唯一 |
| Command ID | Run 内唯一 |
| Effect ID | Agent 对外业务副作用范围 |

相同幂等键但请求摘要不同必须返回 Conflict，不得默默采用其中一个。

# 4. Inbox 与 Outbox

## 4.1 Agent Inbox

Agent 接收 RunRequest 时先记录：

```text
attempt_id
request_digest
received_at
current_status
```

重复请求根据 Inbox 返回同一结果。

生产等级 Provider 必须把 Inbox 持久化到进程崩溃后仍可恢复的存储中，并保证“记录接收”和“开始可见业务执行”之间不存在未受保护窗口。内存 Inbox 只能用于实验等级实现。

## 4.2 Agent Event Outbox

事件先进入 Outbox，再发送 Control Plane。收到 `accepted_through_sequence` 后标记已确认。

生产等级 Provider 必须使用持久化 Outbox；事件创建与相关本地状态迁移应尽可能在同一事务提交。内存 Outbox 必须声明 `event_outbox=memory`，不得通过 Production Conformance。

## 4.3 Control Plane Inbox

Control Plane 对 Event、Command Result、Callback 维护去重记录，并与状态投影在同一事务提交。

# 5. Fencing

以下场景必须使用 Fencing：

- 同一 RuntimeInstance 新旧 Session。
- 同一 Run 多个 Attempt。
- Worker Attempt Lease 过期后重新分配。
- Session Ownership 迁移。

Fencing Token 必须单调递增。接收方只接受当前 Token；旧 Token 结果进入 Late Event Audit。

# 6. 重试策略

错误必须明确：

```text
retryable
retry_after_seconds
category
safe_to_retry_effect
```

建议退避：指数退避 + 随机抖动 + 上限。SDK 不得对 Validation、Authentication、Authorization 和 Protocol Incompatible 自动重试。

副作用等级为 `write/irreversible` 时，除非 Agent 声明业务幂等并提供 Effect ID，否则 Control Plane 不自动创建新 Attempt。

# 7. 超时层级

需要区分：

| 超时 | 含义 |
| --- | --- |
| Connect Timeout | 无法连接 Endpoint |
| Accept Timeout | Agent 未确认接收 Attempt |
| Start Timeout | 已接收但长期未开始 |
| Idle Event Timeout | 长期无进度或心跳事件 |
| Run Deadline | 整个业务调用的最终期限 |
| Drain Deadline | 实例优雅下线期限 |
| Lease TTL | 实例或 Attempt 在线期限 |

这些超时不得复用一个配置项。

# 8. 故障矩阵

| 故障 | 期望行为 |
| --- | --- |
| Caller 调用 Agent 超时 | 使用相同 Attempt 重试；不自行换实例 |
| Agent 已执行但 ACK 丢失 | Inbox 返回相同接收/结果 |
| Agent 无法上报事件 | Outbox 暂存并重试 |
| Control Plane 短暂不可用 | 已签发 Direct Run 可继续；新 Run 默认拒绝 |
| SSE 节点宕机 | 客户端连接其他节点并用 Last-Event-ID 重放 |
| Registry 实时通知丢失 | Watch 按 Revision Replay 补齐 |
| Registry Revision 已压缩 | 重新读取 Snapshot |
| Worker 失联 | Attempt Lease 到期后重新调度 |
| 旧 Worker 恢复 | Fencing 拒绝改变当前状态 |
| Runtime Lease 到期 | 从 Discovery View 移除 |
| Session 原实例离线 | 按 stateless/sticky/portable 策略处理 |
| Usage 上报失败 | Run 可结束，但标记 `usage_pending` |
| 最终事件迟到 | 写审计，不覆盖已有终态 |

# 9. 多节点 Control Plane

实现要求：

- 所有节点共享持久化 Registry、Run、Attempt 和 Event Ledger。
- 状态迁移通过数据库事务和 State Version 保护。
- Registry Revision 全局唯一递增。
- SSE 节点可以从共享 Ledger 重放，不依赖粘性会话。
- 实时广播可以使用 LISTEN/NOTIFY、消息系统或进程间通道，但不得成为唯一真值。
- 后台 Lease Reaper 必须避免多个节点重复执行；可以使用数据库锁或任务抢占 Lease。

# 10. 状态对账

系统不能只依赖 Callback。建议后台 Reconciler 检查：

- 长期 `dispatching` 未 Accepted。
- 长期 `running` 无 Event/Heartbeat。
- Agent Status 与 Control Plane 状态不一致。
- Run 终态但 Usage Pending。
- Asset 已创建但未绑定结果。
- Worker Lease 已过期但 Attempt 未关闭。

支持 Status Query 的 Agent：

```http
GET /v1/runs/{run_id}?attempt_id=attempt_01
```

对账修正必须写新事件，不直接静默改数据库。

# 11. Dead Letter 与人工处理

以下情况进入可查询的 Dead Letter/Manual Review：

- 超过最大自动重试次数。
- 不可判断副作用是否已经发生。
- Event Sequence 永久缺口。
- Agent 返回互相矛盾的终态。
- Usage/Asset 长期无法对账。
- Session 无法恢复且包含未完成操作。

# 12. 容量与过载

- RuntimeInstance 上报 `max_concurrency`、`active_runs`、`available_slots`、`queue_depth`。
- Router 不向无可用容量实例创建新 Attempt。
- Agent 可以返回 `429/503 + Retry-After`。
- 队列深度、事件积压和 Outbox 大小必须有上限。
- 系统过载时优先保护已运行任务和高优先级任务。
- 不允许无限堆积等待人工输入的 Run 占用执行槽位。

# 13. 可观测性

所有组件传播：

```text
traceparent
tracestate
request_id
run_id
attempt_id
deployment_id
instance_id
event_id
```

建议指标：

```text
registry_instances{state}
registry_watch_lag
lease_expirations_total
runs_created_total
runs_terminal_total{status}
run_duration_seconds
dispatch_attempts_total
dispatch_failures_total{category}
event_ingest_lag_seconds
event_outbox_depth
sse_connections
sse_replay_events_total
worker_claim_wait_seconds
usage_pending_total
```

# 14. SLO 建议

以下是规划维度，不是当前已确认数值：

- Control Plane Run Create 可用性。
- Registry Register/Keepalive 可用性。
- Discovery Snapshot 新鲜度。
- Event Ingest ACK 延迟。
- SSE 首事件与重放延迟。
- Run 状态最终一致时间。
- 审计和 Usage 对账完成时间。

具体目标应结合部署规模确定，不能在协议 Schema 中硬编码。

# 15. 数据保留与压缩

不同日志分别管理：

- Registry Events：有限 Replay Window，可压缩。
- Run Delta Events：按产品回放需求保留。
- Final Result Snapshot：按业务策略保留。
- Audit Events：按合规策略长期保留。
- Trace/Debug Logs：采样和短期保留。

Registry Compaction 不得删除当前 Snapshot；Run Delta Compaction 不得删除最终 Result Snapshot。

Reference Control Plane 的默认保留策略为：

| 数据 | 默认期限 |
| --- | --- |
| Registry Event Replay | 24 小时 |
| Run Delta Replay | 24 小时 |
| Final Result Snapshot/ResultRef | 90 天 |
| Audit Event | 180 天 |

这些是可配置默认值，不是所有企业的强制合规期限。服务端必须向客户端公布实际 Replay Window；到期清理敏感正文后，可以按治理策略保留哈希、主体、时间和结果状态等最小审计元数据。

# 16. 灾难恢复

备份至少包含：

- Published Manifest 和 Digest。
- RuntimeService/Binding 配置和 Operator Override。
- Run、Attempt、Final Result、Usage、Audit。
- Registry 当前资源状态。

临时 Lease 和在线连接不从备份恢复。系统恢复后 RuntimeInstance 重新注册，Router 在 Lease 建立前不使用旧实例。
