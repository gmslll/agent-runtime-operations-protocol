---
title: Agent 注册与发现设计
status: draft-normative
updated: 2026-09-22
---

# 1. 目标

注册与发现用于管理 Agent 的持久能力和临时运行实例，使 Dispatcher 能安全选择当前可用的 Deployment。

设计借鉴 etcd/Nacos 的 Lease、Revision、Watch、CAS、健康视图和客户端恢复思想，但不实现通用 KV，也不依赖 etcd/Nacos。

本文的 Registry 是单个管理域内的 Runtime Registry，用于路由当前可执行实例。跨企业、跨域名的 Agent/MCP/Skill 公共发现使用 ARD/AI Catalog 和 A2A Agent Card；公共发现结果必须经过审核后才能进入本地目录和授权体系。

# 2. 持久资源与临时资源

## 2.1 持久资源

- AgentDefinition
- AgentVersion
- AgentSkill
- RuntimeService
- AgentBinding
- ChannelBinding
- 发布、审核和运维策略

这些资源由管理 API 创建，不能因为某个 RuntimeInstance 离线而消失。

## 2.2 临时资源

- RuntimeInstance Session
- Lease
- Readiness
- Health
- Capacity
- 当前负载

临时资源由运行时注册和 Keepalive 维护。租约到期后，从 Runtime Discovery View 移除，但保留历史和审计。

# 3. 注册身份

参与注册的身份分为：

| 身份 | 权限 |
| --- | --- |
| Publisher | 创建 AgentDefinition、提交 AgentVersion |
| Reviewer/Admin | 审核、发布、停用、转移所有权 |
| RuntimeService | 注册被授权的 AgentBinding |
| RuntimeInstance | 续租、上报运行态、Drain、注销 |

Deployment Credential 必须限制允许注册的 `service_id`、`agent_id`、版本范围和环境。

# 4. RuntimeInstance 注册

## 4.1 请求

建议采用幂等 PUT：

```http
PUT /v1/registry/instances/{instance_id}
Authorization: Bearer <deployment-credential>
Idempotency-Key: <registration-key>
```

```json
{
  "session_id": "ses_019...",
  "service_id": "image-agent-runtime",
  "environment": "production",
  "runtime_version": "2026.09.21",
  "protocol_versions": ["1.0"],
  "transport_profiles": ["direct"],
  "bindings": [
    {
      "agent_id": "image.generate",
      "agent_version": "1.0.0",
      "skill_ids": ["default"],
      "manifest_digest": "sha256:..."
    }
  ],
  "endpoint": {
    "base_url": "https://agent.internal.example",
    "health_path": "/v1/health/ready"
  },
  "capabilities": {
    "streaming": true,
    "stream_resume": true,
    "cancellation": true,
    "status_query": true,
    "event_outbox": "durable"
  },
  "capacity": {
    "max_concurrency": 8,
    "max_queue_depth": 20
  },
  "runtime_metadata": {
    "region": "cn-east",
    "zone": "office-a"
  }
}
```

## 4.2 服务端校验

Control Plane 必须验证：

1. Credential 是否允许注册当前 Service 和 Agent。
2. AgentVersion 是否已发布且可在目标环境运行。
3. Manifest Digest 是否完全一致。
4. 是否存在共同支持的 Protocol Version。
5. Transport Profile 与 Endpoint 是否匹配。
6. Endpoint 是否通过 URL、TLS、网络和 SSRF 策略校验。
7. Runtime Capability 是否满足 AgentVersion 的 required capabilities。
8. `instance_id` 是否属于当前 RuntimeService。

## 4.3 响应

```json
{
  "deployment_id": "dep_01...",
  "lease_id": "lease_01...",
  "generation": 7,
  "registry_revision": 10248,
  "selected_protocol_version": "1.0",
  "lease_ttl_seconds": 30,
  "keepalive_interval_seconds": 10,
  "security": {
    "issuer": "https://control.example",
    "jwks_uri": "https://control.example/.well-known/jwks.json",
    "expected_audiences": ["image.generate"]
  }
}
```

# 5. Session、Generation 与 Fencing

- `instance_id` 是稳定实例标识，可以跨进程重启复用。
- `session_id` 每次进程启动必须重新生成。
- 相同 Instance 新 Session 注册时，服务端递增 `generation`。
- 所有 Keepalive 和状态更新必须携带当前 Session、Lease 和 Generation。
- 旧 Generation 的请求返回 `INSTANCE_GENERATION_FENCED`。

这样可防止旧进程在网络恢复后覆盖新进程状态。

# 6. Lease 与 Keepalive

## 6.1 Keepalive

```http
POST /v1/registry/leases/{lease_id}/keepalive
```

```json
{
  "instance_id": "macmini-004-image-agent",
  "session_id": "ses_019...",
  "generation": 7,
  "heartbeat_sequence": 128,
  "reported_at": "2026-09-21T08:00:00Z",
  "runtime": {
    "active_runs": 3,
    "available_slots": 5,
    "queue_depth": 0
  }
}
```

响应：

```json
{
  "lease_id": "lease_01...",
  "server_time": "2026-09-21T08:00:00.100Z",
  "lease_expires_at": "2026-09-21T08:00:30.100Z",
  "registry_revision": 10249
}
```

## 6.2 Keepalive 约束

- 默认 Runtime Lease TTL 为 30 秒，默认 Keepalive 间隔为 10 秒；服务端可以按部署策略返回其他值。
- Keepalive 只续租和更新高频运行态。
- Keepalive 不得修改 Endpoint、Binding、Protocol Version 和管理员配置。
- 客户端使用服务端返回的间隔和 TTL，不得假设固定值。
- SDK 应在 Keepalive 间隔上加入不超过正负 20% 的随机抖动，避免大量实例同时续租。
- 临时网络错误应在 TTL 内重试。
- TTL 失效后实例必须立即从可调度 Discovery View 移除，并重新注册，不得继续使用旧 Lease。
- Lease 到期不物理删除实例历史、Generation 和审计记录。
- `unhealthy` 由独立健康检查产生，不得用一次 Keepalive 延迟直接替代健康结论。

# 7. 健康状态维度

不要使用单一 `status` 表达所有含义。

| 字段 | 所有者 | 含义 |
| --- | --- | --- |
| `lease_alive` | Registry | Session 是否仍在线 |
| `healthy` | Health Controller | 健康检查是否成功 |
| `ready` | RuntimeInstance | 是否已完成初始化 |
| `enabled` | Operator | 是否允许接收新流量 |
| `draining` | Instance/Operator | 是否停止接收新 Attempt |
| `capacity_available` | RuntimeInstance | 是否有可用执行容量 |

可发现条件：

```text
lease_alive
AND healthy
AND ready
AND enabled
AND NOT draining
AND capacity_available
AND protocol_compatible
AND binding_matches
```

# 8. Runtime Metadata 与 Operator Metadata

## 8.1 Runtime Metadata

实例可以上报：

- Runtime 软件版本。
- Region、Zone、设备类型。
- 可选硬件能力。
- Session/并发能力。
- 传输和流式能力。

## 8.2 Operator Metadata

管理员可以配置：

- `enabled`
- `weight`
- `priority`
- `maintenance_reason`
- 路由标签覆盖
- 环境与数据区域限制

Operator Metadata 优先级高于 Runtime Metadata。实例重新注册和 Keepalive 不能覆盖运维配置。

# 9. Metadata 更新与 CAS

```http
PATCH /v1/registry/instances/{instance_id}
If-Match: "resource-version-17"
```

成功响应包含新的：

```text
resource_version
ETag
registry_revision
```

版本冲突返回：

```http
409 Conflict
```

```json
{
  "code": "RESOURCE_VERSION_CONFLICT",
  "retryable": true
}
```

# 10. Drain 与注销

## 10.1 Drain

```http
POST /v1/registry/instances/{instance_id}/drain
```

```json
{
  "generation": 7,
  "deadline_at": "2026-09-21T08:10:00Z",
  "reason": "rolling_upgrade"
}
```

进入 Drain 后：

- 新 Discovery View 不再返回该实例。
- 已分配 Attempt 可以继续执行到 Deadline。
- 到达 Deadline 后按策略取消或标记失联。
- 实例重启或重新注册只能轮换 `session_id` 并递增 `generation`，不得隐式清除 Draining；必须经过明确的运维恢复或注销后重新注册才可回到可路由状态。

## 10.2 Deregister

```http
DELETE /v1/registry/instances/{instance_id}?generation=7
```

进程应在优雅关闭时注销。异常退出由 Lease Expiration 处理。

# 11. Registry Revision

每个改变 Registry Discovery View 的事务分配一个全局递增 Revision。

Revision 用途：

- 标记 Snapshot 的一致位置。
- 对 Registry Event 排序。
- Watch 断线续传。
- 判断本地缓存是否落后。

Revision 不等于数据库主键、时间戳或 AgentVersion。

# 12. Discovery Snapshot

Router 查询：

```http
GET /v1/discovery/agents/{agent_id}/instances?version=1.0.0&skill_id=default
```

```json
{
  "revision": 10248,
  "agent_id": "image.generate",
  "agent_version": "1.0.0",
  "instances": [
    {
      "deployment_id": "dep_01...",
      "instance_id": "macmini-004-image-agent",
      "generation": 7,
      "transport_profile": "direct",
      "endpoint": "https://agent.internal.example",
      "weight": 100,
      "priority": 10,
      "available_slots": 5,
      "labels": {
        "region": "cn-east",
        "zone": "office-a"
      }
    }
  ]
}
```

Discovery API 只对 Dispatcher 和受信任组件开放。Web/Bot 用户不能获取内部 Endpoint 列表。

# 13. Watch 与断线恢复

第一阶段使用 HTTP Long Poll：

```http
GET /v1/discovery/changes?after_revision=10248&wait_seconds=30
```

```json
{
  "from_revision": 10248,
  "to_revision": 10251,
  "events": [
    {
      "revision": 10249,
      "type": "instance.updated",
      "resource_id": "dep_01...",
      "current": {}
    }
  ]
}
```

客户端算法：

1. 获取完整 Snapshot 和 Revision。
2. 原子替换本地 Discovery Cache。
3. 从 Snapshot Revision 开始 Watch。
4. 按 Revision 顺序应用变更。
5. 断线后从最后成功 Revision 恢复。
6. Revision 已压缩时重新获取 Snapshot。

历史不可用时：

```http
410 Gone
```

```json
{
  "code": "REGISTRY_REVISION_COMPACTED",
  "action": "RESYNC",
  "minimum_available_revision": 12000
}
```

# 14. 本地缓存

- Router SDK 可以缓存 Discovery View。
- 缓存必须记录 Snapshot Revision 和每个实例的 Lease Expiration。
- Watch 中断时可以暂时使用仍在有效 Lease 内的实例。
- Lease 已过期的实例不得因“最后一次已知健康”继续接收新 Run。
- 本地缓存不能反向修复 Registry。

# 15. 路由输入

协议只定义 Router 选择所需事实，不固定具体算法。常见输入：

- AgentVersion 和 Skill 匹配。
- Transport Profile。
- Protocol/Extension 兼容。
- 健康和容量。
- Region、Zone、数据区域。
- Session Affinity。
- Weight、Priority。
- 版本灰度或 Canary 标签。
- 副作用等级和执行认证等级。

Router 选择实例后，Control Plane 创建 Attempt；调用方不得自行从缓存任意换实例。

# 16. Worker Pull 注册

Pull Worker 也是 RuntimeInstance，使用同一 Lease、Generation、Binding 和 Capacity 模型，只是：

- `transport_profile = worker_pull`
- 不要求可入站 Endpoint。
- 注册响应包含 Claim Endpoint 和建议 Long Poll 参数。

```json
{
  "worker": {
    "claim_url": "https://control.example/v1/workers/worker_01/claims:next",
    "claim_wait_seconds": 30,
    "attempt_lease_seconds": 60
  }
}
```

# 17. 注册表高可用实现建议

本地单进程 Quickstart 可以使用 SQLite；生产级、多节点 Reference Control Plane 使用 PostgreSQL。两种实现共享同一状态机和 Fixture。PostgreSQL 模式包含：

```text
registry_revision_seq
runtime_services
runtime_instances
agent_bindings
instance_leases
registry_events
operator_overrides
```

每次改变 Discovery View 的操作在一个事务中：

```text
校验 Resource Version
→ 分配 Revision
→ 更新当前状态
→ 写 Registry Event
→ 提交
→ 发布实时通知
```

实时通知可以使用 PostgreSQL LISTEN/NOTIFY 或进程内广播；通知只用于降低延迟，正确性依赖 Registry Event Replay。

# 18. 不采用的行为

- 不实现任意 Key/Value API。
- 不允许运行实例修改管理员 Enabled 状态。
- 不使用保护阈值把已确认 Unhealthy 的 Agent 重新投入有副作用的调用。
- 不让普通业务客户端查询全部 Agent 或实例列表。
- 不把心跳当作修改静态元数据的渠道。
- 不依赖客户端本机时间判断 Lease 是否有效。
