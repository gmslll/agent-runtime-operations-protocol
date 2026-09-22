---
title: Run、Attempt、事件与流式通信
status: draft-normative
updated: 2026-09-22
---

# 1. 目标

本设计确保 Direct、Proxy 和 Worker Pull 使用同一套 Run、Attempt、Event、Command 和 Result 语义，并支持：

- 短任务和长任务。
- 参数调用和多轮对话。
- 文本、结构化数据和资产输出。
- 实时流式展示。
- 断线重连和事件重放。
- 重试、取消、超时和人工审批。
- 多节点 Control Plane 与 Agent 故障恢复。

# 2. Run 创建边界

Run 由 Control Plane 创建。协议仓定义 Agent Runtime 请求结构；面向最终用户的 Run 创建 API 可以由不同 Control Plane 自行扩展，但必须生成相同的核心 Run/Attempt 语义。

Control Plane 必须原子完成：

1. 解析可信调用身份。
2. 归一为内部 Subject。
3. 检查 Agent、Skill、Channel、Group、Resource Scope。
4. 检查 Agent 状态、风险确认、预算和限额。
5. 检查外部 `Idempotency-Key`。
6. 绑定确定的 AgentVersion 和 Manifest Digest。
7. 创建 `run_id` 和初始 Run Event。

未授权或身份不可信时，不创建可执行 Run。

# 3. Dispatch Ticket

Dispatcher 选择实例后创建 Attempt，并返回调用计划：

```json
{
  "run_id": "run_01...",
  "attempt_id": "attempt_01...",
  "agent": {
    "id": "image.generate",
    "version": "1.0.0",
    "skill_id": "default"
  },
  "delivery": {
    "mode": "direct",
    "endpoint": "https://agent.internal.example/v1/runs",
    "stream_endpoint": "https://agent.internal.example/v1/runs/run_01/events",
    "expires_at": "2026-09-21T08:05:00Z"
  },
  "run_token": "<short-lived-jwt>",
  "traceparent": "00-..."
}
```

Ticket 是一次 Attempt 的能力授权，不是长期服务发现结果。

# 4. Direct Push

## 4.1 创建执行

```http
POST /v1/runs
Authorization: Bearer <run-token>
Idempotency-Key: <attempt-id>
Content-Type: application/json
```

Agent 校验并持久化接收记录后返回：

```http
202 Accepted
```

```json
{
  "run_id": "run_01...",
  "attempt_id": "attempt_01...",
  "status": "accepted",
  "status_url": "/v1/runs/run_01",
  "events_url": "/v1/runs/run_01/events",
  "cancel_url": "/v1/runs/run_01/cancel"
}
```

如果 Agent 已处理过同一 Attempt，必须返回相同结果或当前状态。

## 4.2 调用结果不确定

调用方超时时，无法判断 Agent 是否已接收。调用方应使用相同 Attempt 和 Idempotency Key 重试。不得直接创建新 Attempt，除非 Control Plane 明确关闭旧 Attempt 并签发新的 Fencing Token。

# 5. Proxy 模式

Proxy 模式仍使用相同 RunRequest。区别是 Control Plane 或独立 Execution Gateway 代表调用方请求 Agent。

典型用途：

- 浏览器不能安全持有 Agent Endpoint 或 Run Token。
- Agent 位于 Control Plane 可达但用户不可达的网络。
- 需要强制内容过滤或统一出口。

Proxy 是 Transport Profile，不改变 Agent 的业务协议。

# 6. Worker Pull

## 6.1 Claim

```http
POST /v1/workers/{worker_id}/claim
Authorization: Bearer <worker-session-token>
```

```json
{
  "session_id": "boot_01...",
  "generation": 7,
  "available_slots": 2,
  "supported_bindings": [
    {"agent_id": "image.generate", "version": "1.0.0"}
  ],
  "wait_seconds": 30
}
```

有任务时返回：

```json
{
  "assignment": {
    "run_request": {},
    "attempt_lease_id": "attempt_lease_01...",
    "fencing_token": 43,
    "lease_expires_at": "2026-09-21T08:01:00Z"
  }
}
```

## 6.2 Attempt Lease

- Worker 执行期间必须续租 Attempt Lease。
- 租约到期后 Control Plane 可以创建新 Attempt。
- 旧 Worker 的事件和结果因 Fencing Token 失效而被标记为迟到。
- 迟到事件进入审计，不改变当前 Run 状态。

# 7. 事件上报

## 7.1 Event Session

Agent 接收 Run 后，使用 Runtime Session Token 建立 Event Session：

```http
POST /v1/agent-runs/{run_id}/event-session
Authorization: Bearer <runtime-session-token>
```

```json
{
  "attempt_id": "attempt_01...",
  "deployment_id": "dep_01...",
  "generation": 7,
  "fencing_token": 43
}
```

Control Plane 验证当前 Attempt 确实分配给该 Deployment 后返回：

```json
{
  "event_sink": "https://control.example/v1/agent-runs/run_01/events",
  "event_token": "<short-lived-event-token>",
  "expires_at": "2026-09-21T08:10:00Z",
  "max_batch_events": 100,
  "max_batch_bytes": 262144
}
```

Event Token 只能写当前 Run/Attempt 的事件，不能执行 Agent、读取结果或注册实例。长任务由 Runtime Session 身份续换 Event Token；调用方不参与续换。

## 7.2 Event Batch

Agent/Worker 将事件批量上报 Control Plane：

```http
POST /v1/agent-runs/{run_id}/events
Authorization: Bearer <event-token>
Idempotency-Key: <batch-id>
```

```json
{
  "batch_id": "batch_01...",
  "attempt_id": "attempt_01...",
  "fencing_token": 43,
  "events": [
    {
      "id": "evt_17",
      "producersequence": 17,
      "type": "com.example.agent.output.delta.v1",
      "data": {"output_id": "answer", "offset": 120, "delta": "商品"}
    },
    {
      "id": "evt_18",
      "producersequence": 18,
      "type": "com.example.agent.output.delta.v1",
      "data": {"output_id": "answer", "offset": 126, "delta": "分析"}
    }
  ]
}
```

响应：

```json
{
  "accepted_through_producer_sequence": 18,
  "assigned_run_sequence": 1844,
  "duplicate_event_ids": [],
  "run_state": "running"
}
```

## 7.3 Outbox

生产级 Agent SDK 应提供 Event Outbox：

```text
业务产生事件
→ 本地持久化 Outbox
→ 可选地推给 Direct Stream
→ 批量上报 Control Plane
→ 收到 ACK
→ 标记已确认并按保留策略清理
```

内存 Outbox 可以用于实验环境，但必须在注册 Capability 中声明 `event_outbox=memory`，不得认证为高可用生产实例。

# 8. Control Plane Event Ledger

接收事件顺序：

```text
验证 Token、Attempt、Generation、Fencing
→ 校验 Event Schema
→ 按 source + event_id 去重
→ 校验 Producer Sequence
→ 追加 Run Event
→ 分配 Run Sequence
→ 更新 Run/Attempt 状态投影
→ 更新 Usage/Result/Asset 投影
→ 提交事务
→ 广播给订阅者
→ 返回 ACK
```

Ledger 事件不可原地修改。修正通过新事件完成。

# 9. SSE 消费

Control Plane 向 Web、Gateway 或后端服务提供：

```http
GET /v1/agent-runs/{run_id}/events
Accept: text/event-stream
Last-Event-ID: 1842
```

```text
id: 1843
event: output.delta
data: {"run_id":"run_01...","output_id":"answer","offset":120,"delta":"商品"}

id: 1844
event: progress.updated
data: {"run_id":"run_01...","percent":40}
```

规则：

- SSE `id` 使用 Control Plane `run_sequence`。
- 重连携带 `Last-Event-ID`。
- 服务端先重放缺失事件，再切换实时事件。
- 定期发送 SSE Comment Heartbeat，避免中间代理误判空闲。
- SSE 连接断开不取消 Run。
- 认证和授权必须在每次连接与重连时重新校验。

# 10. Direct Streaming

可信调用方可以直接订阅 Agent 的 SSE Endpoint，降低延迟：

```http
GET /v1/runs/{run_id}/events
Authorization: Bearer <run-token>
Last-Event-ID: <producer-sequence>
```

Direct Stream 使用 Producer Sequence；Control Plane Stream 使用 Run Sequence。Agent 仍必须通过 Outbox 向 Control Plane 上报必要事件和最终快照。

Dispatch Ticket 必须说明：

```text
stream_mode: direct | relay
resumable: true | false
replay_window_seconds
```

浏览器默认使用 Relay/BFF。Bot Gateway 和后端服务可使用 Direct。

# 11. 输出模型

## 11.1 文本 Delta

```json
{
  "output_id": "answer",
  "part_index": 0,
  "offset": 128,
  "delta": "新的内容"
}
```

- 同一 Output 的 Offset 必须单调增加。
- Offset 固定表示当前 Output 前缀的 UTF-8 字节数；不得使用 Unicode Code Point、UTF-16 Code Unit 或语言本地字符串长度。
- SDK 必须验证 Offset 落在合法 UTF-8 边界，并使用中文、Emoji 和组合字符 Fixture 做跨语言测试。
- 重发相同 Offset 必须包含相同内容。
- Delta 默认只允许追加；需要修改已经输出的内容时必须发送 `output.snapshot` 或 `output.reset`。

## 11.2 Snapshot

```json
{
  "output_id": "answer",
  "revision": 4,
  "content": [{"type": "text", "text": "完整结果"}],
  "digest": "sha256:..."
}
```

终态必须包含最终 Snapshot 或 ResultRef。

## 11.3 Reset

新 Attempt 无法延续旧部分输出时：

```json
{
  "output_id": "answer",
  "reason": "attempt_restarted",
  "replacement_attempt_id": "attempt_02..."
}
```

消费者应清空旧 Output 并从新 Attempt 重新渲染。

# 12. Backpressure 与慢消费者

- Agent SDK 对 Token 做小批量合并，建议默认 20～100ms 刷新。
- Event Batch 限制最大事件数和字节数。
- SSE 节点为每个连接设置有界缓冲。
- 慢消费者超过缓冲时，服务端断开连接；客户端使用 Last-Event-ID 重连。
- 不允许慢消费者阻塞 Run Event Ledger 写入。
- 飞书等渠道应节流更新，最终结果必须发送完整快照。

# 13. Event Replay 与保留

如果请求的 Run Sequence 仍在保留窗口，服务端重放所有缺失事件。

游标过期：

```http
410 Gone
```

```json
{
  "code": "STREAM_CURSOR_EXPIRED",
  "snapshot_url": "/v1/agent-runs/run_01",
  "latest_sequence": 2200
}
```

客户端获取最新 Snapshot 后，从最新可用 Sequence 重新订阅。

# 14. Command 与人工交互

```http
POST /v1/agent-runs/{run_id}/commands
Idempotency-Key: <command-id>
```

```json
{
  "command_id": "cmd_01...",
  "type": "approval.approve",
  "expected_state_version": 12,
  "data": {
    "approval_id": "approval_01..."
  }
}
```

Command 必须记录真实批准主体和渠道。原始调用者、批准者和执行 Agent 可以是不同身份。

等待人工输入时，Run 可以进入 `waiting_input`。是否释放 Runtime Capacity、会话是否继续租用，由 Agent Capability 声明。

# 15. Cancel、Timeout 与竞态

## 15.1 Cancel

取消是请求，不是立即终态：

```text
running
→ cancel_requested
→ cancelled
```

Agent 不支持强制终止时必须明确 `cancellation=best_effort`。

## 15.2 Deadline

- Deadline 由 Control Plane 签入 Run Token 和 RunRequest。
- Agent 不得在 Deadline 后开始新的外部副作用。
- Control Plane 可以在 Deadline 后将 Run 标记 `timed_out`。
- 迟到成功事件只保留审计，不覆盖 `timed_out`。

## 15.3 终态竞态

第一个通过 State Version 和 Fencing 校验并成功提交的合法终态获胜。后续终态记为 Late Event。

# 16. 重试

## 16.1 自动重试条件

- `retryable=true`。
- Agent 副作用允许重试。
- 未超过最大 Attempts 和 Deadline。
- 当前 Attempt 已被关闭或 Fenced。

## 16.2 新 Attempt

重试保持同一 `run_id`，创建新的：

```text
attempt_id
deployment_id
fencing_token
run_token
trace span
```

## 16.3 业务副作用

`write`/`irreversible` Agent 必须使用稳定 `effect_id` 与外部系统幂等键。仅靠 Attempt ID 无法避免跨 Attempt 重复副作用。

# 17. Session 与粘性路由

Run Context 可以包含 `conversation_ref`，但具体 `codex_session_id`、本机路径和框架字段不进入通用协议。

Control Plane 维护：

```text
conversation_ref
→ agent_id/version
→ owner_instance_id
→ opaque_session_ref
→ session_lease
```

Sticky Session 的原实例不可用时，按 Agent Capability 执行：等待恢复、创建新 Session、Checkpoint 迁移或失败。

# 18. 渠道呈现

Agent 输出协议与渠道无关。

Gateway/Web 根据事件渲染：

- `output.delta`：增量文本。
- `progress.updated`：进度卡片。
- `asset.created`：图片或文件。
- `run.succeeded`：完整最终结果。
- `run.failed`：标准错误。

飞书群、话题、消息 ID 和更新频率属于 Channel Adapter，不进入 Agent Runtime Schema。
