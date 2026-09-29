# AgentOS 下一阶段执行文档

> 仓库：`CloudEdgeCore/AgentOS`
>
> 项目目标：构建真正的 **Agent Operating System**
>
> 当前阶段：**Agent Kernel → Agent Process System**
>
> 当前重点：从已经较成熟的 Task / Workflow / Scheduler / Runtime / IPC 基础，继续补齐 Agent OS 的进程管理与系统调用能力。

---

# 1. 本阶段核心目标

下一阶段按以下顺序推进：

```text
1. IPC Authorization 收尾
2. 外部副作用 Effect 语义
3. Agent Service / Supervisor
4. Agent Syscall ABI
5. Resource / Namespace 完整化
6. 72h / 7d Soak Test
```

原则：

> 不再继续给 Workflow 堆功能，开始完善 Agent OS 的“进程系统”。

---

# 2. P0：完成 IPC Authorization

当前 IPC 已经完成：

```text
Durable Mailbox
Send
Receive
Ack
Request / Reply
Fencing
Tenant Isolation 基础
```

下一步重点完成 peer authorization。

## 2.1 目标

Agent A 是否可以给 Agent B 发消息，必须同时经过：

```text
Identity
Capability
Peer Policy
Tenant
Namespace
Fencing
Audit
```

## 2.2 建议权限模型

Capability：

```text
ipc.send
ipc.receive
ipc.signal
```

Agent Manifest 增加：

```yaml
capabilities:
  peers:
    - namespace: research
      agent: writer-agent
```

支持：

```text
allowed_peers
```

默认：

```text
Deny
```

禁止：

```text
默认允许任意 Agent 互发消息
```

## 2.3 必须补齐

- sender allowlist
- receiver policy
- namespace policy
- tenant isolation
- peer authorization audit
- authorization decision trace
- deny reason

## 2.4 验收标准

```text
Cross-Tenant Delivery = 0
Unauthorized Peer Delivery = 0
Unauthorized Signal = 0
Stale Instance Send = 0
```

测试：

```text
Agent A → allowed Agent B      PASS
Agent A → denied Agent C       DENY
Tenant A → Tenant B            DENY
Old Instance → Agent B         ErrFenced
```

---

# 3. P0：解决外部副作用语义

## 3.1 当前问题

AgentOS 内部可以依靠：

```text
Lease
Fencing
CAS
Transaction
Idempotency
```

控制重复执行。

但外部副作用例如：

```text
Webhook
Payment
Email
HTTP API
Database Write
Third-party Tool
```

一旦请求已经发送出去：

```text
Old Attempt
→ External Side Effect
→ Lease expires
→ New Attempt takeover
→ Side Effect again
```

就可能出现重复执行。

因此不能简单承诺：

```text
All external effects = exactly-once
```

---

# 4. P0：新增 Effect API

建议引入 Kernel 一级抽象：

```text
Effect
EffectRequest
EffectReceipt
EffectStore
```

调用流程：

```text
Agent
 ↓
Effect Syscall
 ↓
Kernel
 ↓
Capability / Policy
 ↓
Fencing
 ↓
Idempotency
 ↓
External Provider
 ↓
Receipt
```

## 4.1 EffectRequest 建议字段

```text
effect_id
tenant_id
agent_id
run_id
attempt_id
fencing_token
provider
operation
idempotency_key
payload_hash
deadline
trace_id
```

## 4.2 EffectReceipt

建议记录：

```text
effect_id
status
provider_receipt
started_at
completed_at
attempt_id
fencing_token
result_hash
```

## 4.3 状态

```text
PENDING
EXECUTING
COMMITTED
FAILED
UNKNOWN
```

`UNKNOWN` 很重要。

表示：

> 请求可能已经到达外部系统，但 AgentOS 无法确定最终结果。

禁止自动把 UNKNOWN 当成 FAILED 再执行。

## 4.4 验收标准

必须证明：

```text
Same idempotency_key → logical effect = 1
Stale Attempt → cannot create new effect
Committed Effect → takeover 后不能重复执行
Unknown Effect → 不自动重放
```

---

# 5. P0：Agent Service / Supervisor

这是下一阶段最核心的新功能。

## 5.1 目标

当前：

```text
Task → 一次执行
Workflow → 多任务编排
```

新增：

```text
AgentService → 长期运行 Agent
Supervisor → Agent 进程管理器
```

形成：

```text
Task          = Job
Workflow      = Job orchestration
IPC           = Inter-process communication
AgentService  = Daemon
Supervisor    = Process manager
```

---

# 6. AgentService 模型

建议新增：

```text
internal/kernel/service/
```

建议结构：

```text
service/
├── spec.go
├── instance.go
├── supervisor.go
├── health.go
├── reconcile.go
├── rollout.go
├── store.go
└── service_test.go
```

## 6.1 AgentService

建议字段：

```yaml
apiVersion: agentos.io/v1
kind: AgentService

metadata:
  name: research-agent
  namespace: research

spec:
  agentVersion: research-agent:v3

  replicas: 3

  runtimeClass: oci

  restartPolicy: Always

  resources:
    cpu: 2
    memory: 4Gi
    llmSlots: 2

  health:
    interval: 10s
    timeout: 3s

  rollout:
    maxUnavailable: 1
    maxSurge: 1
```

---

# 7. AgentInstance

Service 不能直接代表运行实体。

新增：

```text
AgentInstance
```

关系：

```text
AgentService
   ↓
AgentInstance
AgentInstance
AgentInstance
```

建议状态：

```text
CREATED
STARTING
RUNNING
DEGRADED
DRAINING
STOPPING
STOPPED
FAILED
RECOVERING
```

---

# 8. Supervisor

Supervisor 负责：

```text
start
stop
restart
health
recover
scale
drain
upgrade
rollback
```

但不直接执行 Runtime。

仍然通过现有：

```text
Scheduler
Runtime
Lease
Fencing
```

系统。

不要为 Service 创建第二套 Scheduler。

---

# 9. Service Crash Recovery

测试：

```text
AgentInstance RUNNING
↓
Runtime crash
↓
Lease expire
↓
Supervisor detect
↓
New Instance
↓
RUNNING
```

验收：

```text
Lost Service = 0
Stale Instance Active = 0
Replica Overflow = 0
```

---

# 10. Service Scaling

支持：

```text
replicas: 1 → 5
replicas: 5 → 2
```

Scale Down 必须：

```text
先 Drain
停止新 IPC
处理或转移 Mailbox
释放资源
停止 Instance
```

不能直接 kill。

## 验收标准

```text
Desired Replicas = Actual Healthy Replicas
```

最终收敛。

---

# 11. Rolling Upgrade

例如：

```text
Agent v1
↓
Agent v2
```

必须支持：

```text
Rolling Update
Drain
Health Check
Rollback
```

验收：

```text
Available replicas >= configured minimum
Old Instance eventually = 0
Failed rollout → rollback
```

---

# 12. Service + IPC 集成

这是关键。

Service Instance 必须可以拥有：

```text
AgentAddress
Mailbox
```

建议：

```text
service/research-agent
```

为逻辑地址。

具体：

```text
service/research-agent/instance-123
```

为物理实例。

Kernel 决定消息路由到哪个健康实例。

未来可以支持：

```text
Round Robin
Least Loaded
Sticky Session
Consistent Hash
```

第一阶段先做：

```text
Round Robin / Any Healthy Instance
```

即可。

---

# 13. P1：统一 Agent Syscall ABI

完成 Service 后，再开始统一 Syscall。

目标：

> Agent 不需要知道底层 Gateway、NATS、PostgreSQL、Provider。

第一版建议：

```text
model.invoke()
tool.call()

memory.read()
memory.write()
memory.search()

ipc.send()
ipc.receive()
ipc.reply()
ipc.signal()

effect.execute()

artifact.read()
artifact.write()

secret.get()

agent.spawn()
agent.stop()

service.discover()
```

---

# 14. Syscall 原则

每次调用必须经过：

```text
Identity
Capability
Policy
Namespace
Quota
Budget
Fencing
Audit
Trace
```

Syscall ABI 必须：

```text
Runtime-independent
Model-independent
Framework-independent
Provider-independent
Versioned
Backward-compatible
```

---

# 15. P1：Resource Model

统一资源：

```text
CPU
Memory
GPU
LLM
Browser
Filesystem
Database
Network
Tool
KnowledgeBase
Secret
```

三层关系保持清晰：

```text
Capability
= 能不能访问

Scheduler
= 在哪里运行

Quota / Budget
= 可以使用多少
```

---

# 16. P1：Namespace 完整化

目标结构：

```text
Tenant
└── Namespace
    ├── Agents
    ├── Services
    ├── Workflows
    ├── IPC
    ├── Resources
    ├── Secrets
    └── Policies
```

默认规则：

```text
Namespace A
不能发现 B Agent
不能 IPC 到 B
不能读取 B Memory
不能读取 B Secret
不能使用 B Resource
```

除非显式授权。

---

# 17. P1：72h / 7d Production Validation

Agent OS 核心继续扩展的同时，生产验证不能停。

## 72h

必须：

```text
Lost Task = 0
Lost IPC Message = 0
Duplicate Final Result = 0
Permanent Stuck = 0
Deadlock = 0
Panic = 0
```

随机：

```text
Scheduler restart
Controller restart
Runtime crash
Service Instance crash
PostgreSQL restart
NATS restart
Network delay
```

## 7d

重点观察：

```text
Memory
Goroutines
DB connections
Mailbox growth
IPC cleanup
Lease renewal
Service rollout
Artifact cleanup
Queue recovery
```

---

# 18. 推荐开发顺序

严格建议：

```text
01 Merge / 完成 IPC peer authorization

02 修复外部副作用语义
   → Effect API
   → Idempotency
   → Receipt

03 AgentService 数据模型

04 AgentInstance 生命周期

05 Supervisor

06 Health / Restart / Recovery

07 Scale / Drain

08 Rolling Upgrade / Rollback

09 Service + IPC Integration

10 Syscall ABI

11 Resource / Namespace

12 72h / 7d Validation
```

---

# 19. 推荐 PR 划分

```text
PR-1
feat(ipc): complete peer authorization and audit

PR-2
feat(effect): introduce effect request and receipt model

PR-3
feat(effect): enforce fencing and idempotent execution

PR-4
feat(service): introduce AgentService and AgentInstance

PR-5
feat(service): add supervisor reconciliation loop

PR-6
feat(service): add health and crash recovery

PR-7
feat(service): add scale and drain

PR-8
feat(service): add rolling upgrade and rollback

PR-9
feat(service): integrate service addressing with IPC

PR-10
feat(syscall): introduce versioned AgentOS syscall ABI
```

---

# 20. 本阶段 Release Gate

必须完成：

## IPC

- [ ] Peer Authorization
- [ ] Receiver Policy
- [ ] Namespace Policy
- [ ] Audit
- [ ] Trace
- [ ] Fencing

## Effect

- [ ] EffectRequest
- [ ] EffectReceipt
- [ ] Idempotency
- [ ] Fencing
- [ ] UNKNOWN 状态
- [ ] Recovery Test

## Service

- [ ] AgentService
- [ ] AgentInstance
- [ ] Supervisor
- [ ] Health
- [ ] Restart
- [ ] Recovery
- [ ] Scale
- [ ] Drain
- [ ] Rolling Update
- [ ] Rollback
- [ ] IPC Integration

## Production

- [ ] Race Test
- [ ] Fault Injection
- [ ] 72h Soak
- [ ] 7d Soak

---

# 21. 核心验收指标

最终要求：

```text
Lost Task = 0
Lost Durable IPC Message = 0
Duplicate Logical IPC Message = 0
Unauthorized IPC = 0
Stale Instance Write = 0

Lost Service = 0
Replica Overflow = 0
Stale Service Instance = 0

Duplicate Controlled Effect = 0
Unknown Effect Auto-Replay = 0

Deadlock = 0
Permanent Stuck = 0
Panic = 0
```

---

# 22. 完成后的 AgentOS 模型

完成这一阶段后：

```text
Agent Package
     ↓
Agent Process
     ↓
Task / Run / Attempt
     ↓
Scheduler + Runtime
     ↓
IPC
     ↓
AgentService + Supervisor
     ↓
Syscall ABI
     ↓
Resource / Capability / Namespace
```

对应传统操作系统：

```text
Agent Package     ≈ Program
Agent Instance    ≈ Process
Task              ≈ Job
Workflow          ≈ Job Graph
IPC               ≈ IPC
AgentService      ≈ Daemon
Supervisor        ≈ init/systemd
Syscall ABI       ≈ System Call Interface
Capability        ≈ Permission Model
Runtime           ≈ Execution Environment
Scheduler         ≈ Process Scheduler
```

---

# 23. 最终目标

下一阶段完成以后，AgentOS 不再只是：

> Production-grade Agent Infrastructure

而会进一步形成：

> **Agent Process + IPC + Service + Syscall 的完整 Agent Operating System Kernel。**

现在最优先的两个任务：

```text
1. IPC Authorization 收尾
2. Effect API / 外部副作用语义
```

这两个完成后，再正式进入：

```text
Agent Service / Supervisor
```
