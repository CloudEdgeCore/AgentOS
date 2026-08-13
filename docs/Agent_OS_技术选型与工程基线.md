# Agent OS 技术选型与工程基线

> 文档状态：Proposed（待 ADR 验证后转为 Accepted）  
> 版本：v0.1  
> 日期：2026-08-13  
> 适用范围：Architecture、v0.1 Kernel Slice、v0.3 Secure Runtime  
> 上游依据：[Agent OS 完整架构设计](./Agent_OS_完整架构设计.md)

---

## 1. 决策摘要

Agent OS 不应采用“单语言、单中间件解决全部问题”的方案。推荐基线是：

| 领域 | 选择 | 定位 |
|---|---|---|
| Control Plane / Kernel | **Go 1.26.x** | API、Controller、Admission、Scheduler、Budget、Gateway、CLI、Conformance Kit |
| 高风险 Runtime Host | **Rust stable，固定工具链版本** | Wasmtime Provider、未来的 microVM/本机隔离组件 |
| SDK | **Python + TypeScript** | Agent/应用开发，不进入 Kernel 可信计算基 |
| Dashboard | **TypeScript + React** | v0.9 平台能力，不阻塞 Kernel |
| 强一致真源 | **PostgreSQL 18.x** | Task/Run/Attempt、lease/fencing、权限、预算、审批、outbox/inbox、Memory metadata |
| 数据访问 | **pgx + sqlc + SQL-first migrations** | 显式事务、显式锁和可审计查询；不采用重 ORM |
| Durable IPC | **PostgreSQL outbox/inbox + NATS JetStream** | 数据库保证事务边界，JetStream 负责低延迟分发与重放 |
| 内部协议 | **Protobuf + gRPC + Buf** | Kernel ↔ Runtime、内部服务、兼容性检查 |
| 外部 API | **REST/OpenAPI 3.1 + SSE** | 稳定客户端接口；WebSocket 仅用于真正双向交互 |
| Policy | **OPA/Rego v1，嵌入 Go 服务** | ABAC、Admission、Tool/Memory/Artifact 决策；签名 Policy Bundle |
| 人/服务身份 | **OIDC/OAuth 2.0 安全最佳实践** | 外部主体认证，不自研账号密码体系 |
| Workload Identity | **SPIFFE/SPIRE + X.509-SVID mTLS** | Control Plane、Gateway、Runtime 之间短期身份 |
| Secret Broker | **OpenBao 参考实现 + Provider SPI** | 动态凭证、租约、撤销；兼容 Vault 与云 Secret Manager |
| v0.1 Runtime Provider A | **Wasmtime + WASI 0.2 Component Model** | 轻量、能力导向、低兼容面 Agent/Tool |
| v0.1 Runtime Provider B | **OCI/containerd + gVisor runsc** | 普通不可信 Linux workload |
| v0.3 高隔离 Runtime | **Firecracker microVM** | Coding、Browser、未知二进制和高敏感任务 |
| 基础设施调度 | **Kubernetes** | Pod、Node、CPU/RAM/GPU 等基础设施资源 |
| Agent 调度 | **自研两阶段 Scheduler** | Token、成本、Tool QPS、审批容量、地域和 RuntimeClass |
| Artifact / Checkpoint | **S3-compatible Object Store** | 内容寻址、版本化、加密、生命周期；产品接口不绑定厂商 |
| Package Registry | **OCI Distribution + ORAS** | Agent Package、SBOM、Provenance 与签名 |
| 供应链签名 | **Sigstore Cosign + in-toto/SLSA provenance** | Admission 前验证身份、digest、签名和来源 |
| v0.1 Memory 检索 | **PostgreSQL FTS + pgvector** | 真源与索引仍在同一事务系统，降低早期运维复杂度 |
| v0.3+ 检索投影 | **OpenSearch** | 多语言关键词、向量和 Hybrid Search；仍然只是派生索引 |
| Cache / 软限流 | **进程内缓存；必要时 Valkey** | 仅优化性能，绝不承载 lease、状态或硬预算真源 |
| 可观测性 | **OpenTelemetry + OTLP Collector** | Trace、Metric、Log 统一采集，后端可替换 |
| 指标/追踪/日志参考栈 | **Prometheus + Grafana + Tempo + Loki** | 本地和参考部署；允许替换为商业后端 |
| Audit 真源 | **PostgreSQL append-only + WORM Object Store** | 事务内审计、hash chain、签名归档；ClickHouse 仅作派生查询 |
| 部署 | **Kubernetes + Helm** | Cloud-neutral reference deployment |
| PostgreSQL 自托管参考 | **CloudNativePG** | 单集群 HA；生产优先采用成熟托管 PostgreSQL |

截至本文日期，官方稳定版本快照为 Go `1.26.5`、PostgreSQL `18.6`、Kubernetes `1.36.2`。仓库应固定精确构建版本，但支持策略应是“当前稳定版及可验证的 N-1”，而不是永久锁死上述数字。

## 2. 选型硬约束

任何候选技术必须先满足以下条件，再比较性能或流行度：

1. **状态权威唯一。** Task、Run、Attempt、lease、fencing token、budget 和 approval 必须存在明确的强一致真源。
2. **不宣称外部 exactly-once。** 系统以 at-least-once、幂等、intent、receipt 和 reconciliation 处理外部副作用。
3. **权限在 LLM 外执行。** Policy、Secret、Egress 和 Runtime 必须形成不可绕过的执行路径。
4. **旧持有者不能写入。** 每个 Attempt 状态写入都必须验证 lease、fencing token 和 `resource_version`。
5. **硬预算可停止。** 所有模型和高成本 Tool 流量必须经过可信 Gateway，支持预留、增量计量和硬停止。
6. **派生系统可重建。** NATS、OpenSearch、ClickHouse、Valkey 和观测后端故障不得破坏业务真源。
7. **多租户是数据模型属性。** `tenant_id` 不能只依靠 Kubernetes Namespace 或请求 Header 推断。
8. **协议适配不污染 Kernel。** MCP、A2A、模型供应商和 Agent Framework 变化由 Adapter 吸收。
9. **可恢复性必须可测试。** 组件必须支持故障注入、重启、重复消息、乱序和迟到写入测试。
10. **许可证与替换路径明确。** Kernel 不依赖无法自托管、无法审计或缺少合理退出路径的闭源核心能力。

## 3. 评估方法

对每项核心技术按以下权重评估：

| 维度 | 权重 | 说明 |
|---|---:|---|
| Correctness / 一致性 | 20% | 能否直接表达系统不变量，失败语义是否明确 |
| Security / 隔离 | 20% | 可信计算基、权限边界、供应链和多租户风险 |
| Ecosystem / 互操作 | 15% | Kubernetes、gRPC、OCI、OpenTelemetry、MCP/A2A 适配成本 |
| Operability | 15% | HA、备份、升级、观测、故障排查和团队值守成本 |
| Delivery velocity | 10% | 招聘、学习曲线、构建速度、测试工具和开发体验 |
| Performance / scalability | 10% | 延迟、吞吐、并发、资源效率和水平扩展能力 |
| Portability / lock-in | 5% | 云中立、数据可导出、接口标准化和替换成本 |
| License / governance | 5% | 许可证、社区治理、维护活跃度和安全响应 |

评分不能替代原型与基准。所有 P0 决策必须有 ADR、最小原型和退出条件。

## 4. 编程语言

### 4.1 Control Plane 选择 Go

Go 负责以下组件：

- External API、Controller、Admission、Scheduler、Quota/Budget。
- Model Gateway、Tool Gateway、Policy 集成和 Secret Broker client。
- OCI/gVisor Runtime Provider controller。
- CLI、Operator、Conformance Test Kit。

主要理由：

- Kubernetes、gRPC、Protobuf、OPA、OpenTelemetry、NATS、PostgreSQL 和 OCI 的 Go 生态完整。
- goroutine、context、标准网络库和静态二进制适合大量并发 I/O 与控制器。
- 构建、交叉编译、race detector、fuzzing、pprof 和部署链路简单。
- Go 兼容性承诺适合需要长期维护的系统 API。

限制与补偿：

- Go 的类型系统不能自动证明分布式不变量；核心状态机必须通过数据库约束、TLA+、property test 和故障注入弥补。
- GC 服务不得承担不可控的极低延迟数据面；用 profiling、内存限额、backpressure 和隔离进程控制尾延迟。
- 禁止在业务层随意启动无法追踪的 goroutine；所有后台任务必须受 `context`、errgroup、指标和 shutdown deadline 管理。

### 4.2 Runtime 安全边界选择 Rust

Rust 仅用于安全或性能收益足以抵消语言成本的边界：

- 嵌入 Wasmtime 的 `runtime-wasm`。
- 未来的 microVM host、文件系统代理或高风险本机隔离组件。
- 需要精确内存和生命周期控制的二进制协议/运行时模块。

不建议用 Rust 重写整个 Control Plane。这样会增加 Kubernetes 集成、控制器开发和招聘成本，也无法替代数据库层的一致性设计。

治理规则：

- `rust-toolchain.toml` 固定稳定版；显式定义 MSRV。
- 默认 `#![forbid(unsafe_code)]`；确需 `unsafe` 的 crate 单独隔离、评审和 fuzz。
- 通过 gRPC Runtime Protocol 与 Go 隔离，不在进程内创建复杂 Go/Rust FFI。

### 4.3 Python 与 TypeScript 的边界

- Python SDK：Agent 开发、Evals、数据处理和测试工具。
- TypeScript SDK：Web/Node 客户端；React Dashboard。
- Python/Node Agent 必须运行在 RuntimeClass 内，不得作为 Control Plane 的可信插件被动态加载。
- SDK 由 OpenAPI/Protobuf 生成底层类型，再提供手写的稳定高层接口。

### 4.4 不选择的语言方案

| 方案 | 不作为主方案的原因 | 可接受位置 |
|---|---|---|
| 全 Rust | Control Plane 生态和交付速度成本过高 | Runtime、安全边界 |
| Java/Kotlin | JVM 运维与启动/内存成本较高，且团队会同时承担 Go/Rust 生态适配 | 已有企业 Adapter |
| Python Kernel | 类型、并发、部署和资源可预测性不适合核心控制面 | SDK、Evals、Agent workload |
| C/C++ Runtime Host | 内存安全风险显著扩大可信计算基 | 必须使用的上游库，且隔离在进程外 |

## 5. 代码库与服务边界

采用 monorepo，但不采用“一个进程包含所有能力”或“从第一天拆成几十个微服务”。推荐初始可部署单元：

```text
agentos-control
  ├─ API / Admission
  ├─ Reconciliation Controllers
  ├─ Scheduler
  └─ Resource & Budget Manager

agentos-gateway
  ├─ Model Gateway
  ├─ Tool / MCP Gateway
  └─ Egress / Usage / Receipt

agentos-secret-broker

agentos-runtime-oci
agentos-runtime-wasm

agentos-projector
  ├─ Outbox Dispatcher
  ├─ Search Indexer
  └─ Audit Archiver
```

原则：

- 同一 Go 仓库内使用清晰 package 边界和依赖方向；禁止服务间共享可变内存。
- v0.1 可将 control 内部模块合并部署，但数据库角色、API 和领域边界从第一天分离。
- Gateway 和 Secret Broker 必须独立进程，因为它们属于凭证与外部副作用安全边界。
- Runtime Provider 必须独立进程/节点池；Kernel 不加载第三方 Runtime 动态库。

## 6. API、协议与 Schema

### 6.1 外部 API

- REST + OpenAPI 3.1 为公开控制面契约。
- Task 事件使用 SSE；只有双向终端、浏览器控制等场景才使用 WebSocket。
- 所有创建型请求要求 `Idempotency-Key`。
- 所有响应返回 `resourceVersion`、`traceId` 和结构化 reason code。
- 分页采用稳定 cursor，不使用大 offset。
- 时间统一 RFC 3339 UTC；ID 使用 UUIDv7，外部不暴露数据库序列。

### 6.2 内部协议

- Kernel ↔ Runtime 和内部服务使用 Protobuf/gRPC。
- Buf 执行 format、lint、generate 和 breaking-change check。
- Proto package 从 `agentos.<domain>.v1alpha1` 开始；字段只废弃不复用编号。
- gRPC deadline、cancellation、retry policy 和 idempotency 必须显式定义；客户端不能对非幂等方法自动重试。
- 大 payload 只传 `ArtifactRef`，不进入 gRPC/NATS 消息体。

公开 REST Contract 与内部 gRPC Contract 分开建模。不能把内部 Runtime RPC 自动暴露成公共 API，避免内部演进被外部兼容性锁死。

### 6.3 外部 Agent 协议

- MCP Adapter 固定支持的协议版本；截至本文采用 `2026-07-28`，利用其无会话核心和显式版本信息。
- A2A Adapter 以 `1.0.0` 为首个目标版本。
- 外部 A2A Task/MCP Task 与内部 Task 建立映射，不共享主键，也不允许外部主体写内部 status。
- 协议输入一律按不可信数据处理，执行 JSON Schema 限制、大小限制、超时、鉴权和内容清洗。

## 7. PostgreSQL：强一致真源

### 7.1 选择理由

PostgreSQL 能在一个事务中原子完成：

- Task/Run/Attempt 创建与状态转移。
- Runtime lease、fencing token 和 resource version 更新。
- Budget reservation/settlement ledger。
- Approval 与精确调用摘要绑定。
- 业务对象写入与 outbox event 创建。
- 消费结果与 inbox 去重记录写入。

`FOR UPDATE SKIP LOCKED` 可用于多 Controller 竞争 work item；RLS 可提供多租户纵深防御；PostgreSQL 生态也允许早期复用 FTS 与 pgvector。

### 7.2 数据访问规则

- 使用 `pgx` 和 `sqlc`；禁止在核心状态路径使用会隐藏 SQL/锁语义的重 ORM。
- 默认 `READ COMMITTED`，通过 `resource_version` CAS 和行锁表达大多数状态机。
- Budget 预留、全局唯一副作用或跨行不变量按场景使用 `SERIALIZABLE`，并实现有界重试。
- 状态转移由存储过程或单一 repository command 集中校验；API handler 不直接更新 status。
- 每张租户表必须有不可空 `tenant_id`，主键/唯一键应包含或验证 tenant boundary。
- RLS 是 defense-in-depth，不替代应用层 tenant 条件；数据库 owner/BYPASSRLS 角色不得用于业务流量。
- 所有 migrations 使用可审计 SQL；expand → migrate/backfill → contract，禁止一次性破坏性变更。

### 7.3 HA 与扩展

- 生产优先使用成熟托管 PostgreSQL。
- Cloud-neutral 自托管参考使用 CloudNativePG，单区域跨可用区部署。
- PgBouncer 仅作为连接池；若采用 transaction pooling，租户上下文必须用事务局部设置并有测试覆盖。
- v0.1 不采用分布式 SQL。只有出现真实的跨区域写入、单主吞吐或 RTO/RPO 证据后，才通过 ADR 评估 YugabyteDB/CockroachDB 等方案。
- 分布式 SQL 迁移不能依赖“PostgreSQL wire compatible”假设；必须重跑隔离级别、锁、事务重试和 SQL 兼容测试。

## 8. Durable IPC：Outbox/Inbox + NATS JetStream

### 8.1 职责划分

```text
PostgreSQL transaction
  ├─ domain state
  └─ outbox event
          ↓ dispatcher
NATS JetStream
          ↓ at-least-once
consumer transaction
  ├─ inbox dedupe
  └─ consumer state / new outbox
```

- PostgreSQL 决定事件是否存在；NATS 不是真源。
- NATS 使用 JetStream durable pull consumer、显式 ack、`MaxDeliver` 和 DLQ。
- 生产采用 3 或 5 节点 JetStream，并为重要 stream 配置副本与容量告警。
- NATS deduplication window 和 double-ack 只能降低重复率，不能替代应用幂等。

### 8.2 顺序和幂等

- 每个 aggregate event 带 `aggregateId + aggregateVersion + eventId`。
- Run 内事件带单调 `sequence`；消费者检测 duplicate、gap 和 late event。
- 需要严格串行的执行事件按 `hash(runId)` 分到固定数量的单消费者 shard；不能假设全局有序。
- inbox 使用 `(consumer_name, event_id)` 唯一约束。
- 外部副作用 ledger 使用 `(task_id, run_id, logical_operation, idempotency_key)` 唯一约束，并保存 receipt。

### 8.3 为什么 v0.1 不选择 Kafka

Kafka 适合超大规模保留日志、数据平台和多下游重放，其默认仍是 at-least-once；Kafka 内部事务也不能自动保证外部系统 exactly-once。v0.1 的主要需求是低延迟 command/work queue、request/reply 和较低运维成本，JetStream 更合适。

当出现以下任一条件时重新评估 Kafka/Redpanda：

- 持续事件量超过 JetStream 经验证的容量模型。
- 需要月级全量日志重放和大量独立消费组。
- Audit/Usage 已成为企业级流数据平台输入。
- 跨团队已经具备成熟 Kafka 平台和 24×7 运维能力。

## 9. Scheduler 与 Kubernetes

采用两层调度：

```text
Agent OS Admission / Scheduler
  ├─ token / cost / model concurrency
  ├─ Tool QPS / egress / residency
  ├─ approval capacity / fairness / deadline
  └─ RuntimeClass / risk
              ↓ placement intent
Kubernetes Scheduler
  └─ Pod / Node / CPU / RAM / GPU / topology
```

v0.1 自研可解释的 Agent Scheduler，但不重写 Kubernetes Node Scheduler：

- Admission 先在 PostgreSQL 中完成 quota/budget reservation。
- Scheduler 对 Runtime Pool 打分，并在一个事务中创建 Run、Attempt 和 lease/fencing token。
- Runtime Worker 由 Deployment/DaemonSet 管理，优先复用热池；不为每个短 Task 创建 Control Plane Pod。
- Kubernetes Scheduler Framework 只在确实需要 Node 级自定义插件时使用。
- Core Task/Run/Attempt 不存成 Kubernetes CRD；CRD 无法自然满足所需跨对象事务、查询和多区域控制面语义。

Kubernetes 支持矩阵覆盖当前稳定 minor 和前两个受支持 minor；禁止使用已 EOL 版本。

## 10. Runtime 与隔离

### 10.1 v0.1 两个 Provider

**Wasmtime/WASI Provider**

- 适合编译为 Component 的 Agent、Tool 和轻量扩展。
- Host 只链接明确授权的 capability；默认无文件、网络、时钟、环境变量和 Secret。
- 使用 fuel/epoch interruption、内存上限、实例池和 host-call 计量。
- Component Model 尚在持续演进，必须固定 Wasmtime、WASI 和 WIT world 版本，并通过兼容矩阵升级。

**OCI + gVisor Provider**

- 适合需要 Linux 用户态兼容性的普通不可信 workload。
- 通过 containerd/OCI 接入，运行时为 `runsc`。
- 强制 user namespace、只读 rootfs、tmpfs workspace、capability drop、seccomp、cgroup、NetworkPolicy 和 egress proxy。
- gVisor 存在 syscall 兼容性与 I/O 性能代价，Admission 必须根据 workload capability 选择 RuntimeClass。

### 10.2 Firecracker 的位置

Firecracker 作为 v0.3 的高隔离 Provider，而不是 v0.1 前置依赖：

- 用于 Coding、Browser、未知二进制和高敏感跨租户 workload。
- 必须使用 jailer、seccomp、cgroup、最小 guest kernel/rootfs 和 host-level egress filtering。
- Snapshot 是 Provider 私有格式，且版本兼容受限；不能把它定义成 Agent OS 通用 Checkpoint ABI。

### 10.3 Checkpoint 决策

v0.1 优先实现**逻辑 Checkpoint**，不承诺任意进程的透明快照：

```text
Checkpoint Envelope
  ├─ agentVersionDigest
  ├─ runtimeClass / provider / ABI
  ├─ checkpointSchemaVersion
  ├─ logical state artifact
  ├─ confirmed side-effect receipts
  └─ sha256 checksum
```

Provider 可附加私有进程/VM snapshot，但 Kernel 只理解 envelope、兼容性和完整性。恢复前必须验证 AgentVersion、ABI、schema、fencing token 和已确认副作用集合。

## 11. Policy、Identity 与 Secret

### 11.1 OPA/Rego

OPA 适合当前 `Principal + Action + Resource + Context` 的 ABAC 模型：

- 在 Go 服务中嵌入 OPA，避免关键副作用检查依赖远程网络跳转。
- Policy 以签名 bundle 发布，包含 revision；每条 AuditEvent 记录 revision 和命中规则。
- Bundle 不可用、过期或 schema 不兼容时 fail closed。
- Policy input 使用版本化类型，禁止把任意业务 JSON 直接交给策略。
- 关键决策不得在 Rego 内执行 `http.send()`；属性在调用前由可信服务加载。
- 使用 `opa test`、schema validation、benchmark 和决策快照测试。

OpenFGA 在组织/项目/文件夹/共享对象形成大规模多跳 ReBAC 图后再引入。v0.1 同时部署 OPA 和 OpenFGA 会制造双重真源与决策组合风险。

### 11.2 身份链

- Human/Service：外部 OIDC Provider，短期 access token。
- Internal workload：SPIRE attestation，X.509-SVID mTLS；优先 X.509 而非可重放 JWT-SVID。
- AgentVersion、Task、Run、Attempt 是应用身份，由 Control Plane 创建并绑定到 workload identity。
- 数据面使用 mTLS identity + 短期、用途受限的 opaque capability handle；不发放通用长期 bearer credential。
- 子 Agent capability 必须是父 capability 的交集，并绑定 task/run/expiry。

### 11.3 Secret Broker

- OpenBao 作为 cloud-neutral reference provider，使用动态 Secret、lease、renew 和 revoke。
- 提供 Provider SPI 兼容 Vault、AWS/GCP/Azure Secret Manager 和企业 HSM/KMS。
- Agent 只持有 credential handle；实际 Secret 由 Tool Gateway 注入内存中的单次调用。
- Secret 不进入 Prompt、环境变量、普通文件系统、Artifact、Memory、Trace 或业务日志。
- 所有结果执行结构化脱敏和 canary secret 泄漏检测。

## 12. Model Gateway 与 Tool Gateway

两者属于 Agent OS 的可信数据面，建议自研薄 Gateway，而不是把第三方聚合代理作为不可替换的 Kernel 依赖。

### 12.1 Model Gateway

- Provider Adapter 统一 request、stream、usage、error、rate-limit 和 model capability。
- 模型路由策略与 AgentVersion 分离并版本化。
- 流式调用在开始前预留最大可接受成本，过程中按本地 tokenizer/供应商 usage 增量结算。
- 供应商 usage 延迟时以保守估算硬停止，完成后再 reconciliation。
- 记录 provider request ID、模型版本、token、价格表 revision、cost 和 finish reason。
- Prompt/Completion 默认不进入普通 telemetry；敏感采集使用独立策略、加密和保留期。

### 12.2 Tool Gateway

- MCP/HTTP/内部 Tool 统一成版本化 Tool Descriptor。
- 调用前完成 schema validation、参数规范化、args hash、Policy、Approval、Budget、Egress 和 Secret 检查。
- Approval 绑定 `tool version + canonical args hash + target + attempt + expiry`。
- 调用后保存 side-effect receipt；没有天然 idempotency 的外部系统必须进入 reconciliation/人工仲裁状态。
- Agent 只看到清洗后的结果，不看到 Secret、内部网络错误或其他租户信息。

## 13. Memory、Artifact 与搜索

### 13.1 Canonical Store

- Memory metadata、ACL、provenance、version、retention、tombstone：PostgreSQL。
- 大文本、文件、截图、数据集和 Checkpoint payload：S3-compatible Object Store。
- Artifact 以 `sha256` 内容寻址，metadata 与内容 digest 分开签名/校验。
- 生产对象存储必须支持版本、服务端加密、生命周期和可验证删除流程。

S3-compatible 是接口选择，不是单一厂商锁定。云上使用原生对象存储；自托管在容量与合规 ADR 中比较 Ceph RGW 等实现。开发测试使用实现相同接口的轻量后端或 fake。

### 13.2 v0.1 Search

- PostgreSQL FTS + pgvector，减少一个分布式系统，并允许 tenant/ACL metadata 在查询前过滤。
- 默认 exact vector search；数据量和延迟证明确有需要后使用 HNSW。
- ANN 必须测试 recall，不能把“返回了 K 条”当成正确性证明。
- 中文和多语言关键词质量必须单独评测；内置 FTS 不满足时提前启用 OpenSearch projector。

### 13.3 v0.3+ OpenSearch Projection

- OpenSearch 用于 keyword + vector + hybrid retrieval，不是真源。
- 索引文档必须携带 tenant、namespace、residency、sensitivity、visibility token 和 source version。
- 先执行 tenant/ACL/sensitivity pre-filter，再评分；返回后仍由 Memory Service 做精确 ACL recheck。
- 高敏感租户使用独立 index 或集群边界，不能只依赖 Dashboard tenant。
- tombstone、correction 和 deletion 使用可追踪 projection job；索引可从 Canonical Store 全量重建。

### 13.4 不选择专用向量数据库作为真源

向量数据库可以成为未来的派生检索 Provider，但不得存放唯一版本、ACL、删除意图或 provenance。否则无法可靠实现修正、撤销、法律删除和跨索引一致性。

## 14. Cache 与 Rate Limit

- v0.1 首选有界进程内 cache，key 必须包含 tenant、policy revision 和 resource version。
- 只有 profiling 证明跨实例缓存必要时才引入 Valkey。
- Valkey 只承载可丢失 cache、短期 soft rate limit 和非权威 session acceleration。
- lease/fencing、Task 状态、hard budget、Approval 和 Permission 真源禁止放入 Valkey；其异步复制/故障转移语义不满足这些不变量。
- Policy revocation 必须主动失效 cache，并设置短 TTL 作为第二道防线。

## 15. Observability 与 Audit

### 15.1 OpenTelemetry

- 应用通过 OTel API 产生 trace、metric、log，统一送到 OTLP Collector。
- `task.id/run.id/attempt.id` 进入 trace/log，不作为 Prometheus metric label，避免基数爆炸。
- Metric label 只保留 bounded dimensions，例如 runtime class、provider、result code、tenant tier。
- 所有 RPC/事件传播 W3C Trace Context；消息带 causation/correlation ID。
- OTel backend 可替换；参考栈为 Prometheus、Grafana、Tempo、Loki。

### 15.2 Audit 不能等同于 Log

- 安全决策和副作用 receipt 与业务状态在同一事务或受 outbox 保证的事务链中写入 PostgreSQL append-only 表。
- 每租户/时间窗口建立 hash chain 或 Merkle manifest，并由 KMS/HSM 签名。
- 异步归档到支持 WORM/Object Lock 的对象存储。
- ClickHouse 可作为高容量 Audit/Usage 查询投影，但可删除重建，不能成为合规真源。
- 高敏感 Prompt/Completion 与普通 Audit 分离存储、单独授权和保留。

## 16. Package、Registry 与供应链

- Agent Package 使用自定义 OCI artifact media type，通过 ORAS push/pull。
- Package manifest 固定 AgentVersion spec、runtime lock、tool lock、permission、memory schema、migration、SBOM 和 provenance digest。
- CI 生成 SPDX 或 CycloneDX SBOM，以及 in-toto/SLSA provenance。
- Cosign 对 Package/Container 签名；生产 Admission 校验 digest、签名者身份、OIDC issuer、provenance 和允许的构建工作流。
- 生产部署只引用 digest，不引用可变 tag。
- 依赖扫描、Secret scanning、license policy 和基础镜像 EOL 检查进入 required CI。

## 17. 工程工具链

### 17.1 Monorepo 建议结构

```text
cmd/                 # Go deployable binaries
internal/            # Go domain/application/adapters
proto/               # Protobuf Runtime/Internal APIs
api/openapi/          # Public REST contract
runtime/wasm/         # Rust Wasmtime provider
sdk/python/
sdk/typescript/
web/
db/migrations/
policy/
deploy/helm/
deploy/dev/
conformance/
modelcheck/tla/
docs/adr/
```

### 17.2 构建与质量

- Go Modules + `go tool`；Rust Cargo workspace；Node 使用 pnpm lockfile。
- Buf 管理 Protobuf；OpenAPI generator 生成 SDK 底层类型。
- Taskfile 或少量 Make targets 统一入口；v0.1 不引入 Bazel，除非构建时间/跨语言缓存出现量化瓶颈。
- 容器使用 BuildKit multi-stage build、非 root、只读 rootfs 和最小运行镜像。
- Renovate/Dependabot 提交依赖升级，所有 lockfile 和 image digest 入库。

## 18. 测试与验证体系

“测试通过”至少包含以下层次：

1. **Unit / race / fuzz**：Go race detector 与 native fuzz；Rust test、clippy、cargo audit 和 fuzz。
2. **Schema**：Buf lint/breaking、OpenAPI diff、JSON Schema、migration lint。
3. **State-machine property tests**：随机生成合法/非法状态转移，验证终态、版本和幂等。
4. **TLA+ model checking**：Run 单活 Attempt、lease/fencing、outbox/inbox、budget reservation、cancel/retry。
5. **Integration**：真实 PostgreSQL、NATS、OPA、对象存储；不以 mock 替代事务与消息语义。
6. **Provider conformance**：同一 AgentVersion 在 Wasmtime 与 gVisor Provider 运行。
7. **Fault injection**：进程 kill、网络分区、ACK 丢失、消息重复、时钟偏差、磁盘满、DB failover。
8. **Security negative tests**：Prompt injection、tenant escape、Secret exfiltration、SSRF、TOCTOU、旧 fencing token。
9. **Load/soak**：1,000+ 并发 Task 的明确 workload mix，报告模型/工具等待占比和资源曲线。
10. **Upgrade/rollback**：N-1 协议、Checkpoint 兼容拒绝、DB expand/contract 和 Policy bundle rollback。

任何 P0 中间件替换必须先通过相同 Conformance Suite，而不是仅比较 API 是否相似。

## 19. 分阶段落地

### Architecture（0-8 周）

- 接受 ADR-001 至 ADR-010。
- 建立 Go/Rust/Proto/OpenAPI/SQL monorepo skeleton。
- 完成 PostgreSQL 状态机 schema、TLA+ 模型和 failure matrix。
- 用最小 prototype 验证 Postgres fencing、outbox → NATS、OPA fail-closed、两 Runtime RPC。

### v0.1 Kernel Slice（3-5 月）

- Go Control Plane、PostgreSQL、NATS JetStream。
- Wasmtime 与 gVisor 两个 Provider。
- OPA、基本 OIDC、开发环境 Secret Provider。
- Model/Tool Gateway、预算 ledger、Approval、Artifact/Checkpoint。
- PostgreSQL FTS/pgvector；OTel reference stack。

### v0.3 Secure Runtime（6-9 月）

- SPIRE、OpenBao、Firecracker Provider。
- OpenSearch projector 与删除传播。
- 签名 OCI Agent Package、SBOM/provenance admission。
- Security red-team、Runtime escape 和多租户隔离测试。

### v0.6+

- HA Scheduler、多区域 Runtime Pool、数据驻留。
- 依据容量数据决定 Kafka、ClickHouse、OpenFGA、Valkey 或分布式 SQL，禁止提前引入。

## 20. 明确拒绝的早期方案

| 方案 | 决定 |
|---|---|
| Kubernetes CRD 作为 Task/Run/Attempt 真源 | 拒绝 |
| Redis/Valkey 作为 lease、fencing 或硬预算真源 | 拒绝 |
| NATS/Kafka 作为业务状态真源 | 拒绝 |
| 宣称外部 Tool exactly-once | 拒绝 |
| Temporal 作为 Kernel 对象模型和状态机真源 | 拒绝；可作为 Agent Framework/Runtime Adapter |
| LiteLLM 等第三方 Model Proxy 作为不可替换可信 Kernel | 拒绝；可作为可选 Adapter/测试后端 |
| 普通 runc 容器执行任意不可信 Agent | 拒绝 |
| Service Mesh 作为 v0.1 强制依赖 | 拒绝；直接 SPIFFE mTLS，出现明确流量治理需求后再评估 |
| OpenSearch/Vector DB 作为 Memory 真源 | 拒绝 |
| 从 v0.1 引入 Kafka、ClickHouse、OpenFGA、Valkey 全家桶 | 拒绝；按证据演进 |
| 通用透明进程 Checkpoint ABI | 拒绝；先定义逻辑 Checkpoint envelope |

## 21. 必须建立的 ADR

1. ADR-001：Go/Rust 语言边界与 FFI 禁止规则。
2. ADR-002：PostgreSQL schema、隔离级别、lease/fencing 事务。
3. ADR-003：Outbox/Inbox、NATS stream/consumer、ordering 和 DLQ。
4. ADR-004：Agent Scheduler 与 Kubernetes Scheduler 的职责边界。
5. ADR-005：OPA input schema、bundle、revision、fail-closed 和缓存。
6. ADR-006：OIDC、SPIFFE/SPIRE、Capability Handle 和 Secret Broker。
7. ADR-007：Wasmtime/gVisor Runtime Provider 与 Runtime Protocol。
8. ADR-008：Checkpoint envelope、逻辑状态和 Provider 私有 snapshot。
9. ADR-009：Memory Canonical Store、pgvector/OpenSearch 投影和删除证明。
10. ADR-010：OCI Agent Package、Cosign、SBOM、Provenance 和信任根。

## 22. 重新评估触发条件

选型不是永久信仰。发生以下条件时必须重开 ADR：

- P99、吞吐、成本或恢复指标连续两个版本不达 SLO，且 profiling 指向当前组件。
- 上游进入 EOL、治理失效、许可证改变或安全响应不可接受。
- 需要跨区域 RPO 0 写入，单主 PostgreSQL 已成为实证瓶颈。
- JetStream 保留/消费组/吞吐超出容量模型，或企业已有成熟 Kafka 平台。
- OPA 数据规模导致内存/延迟超标，且关系图查询成为主要授权模式。
- pgvector/OpenSearch 无法在 ACL pre-filter 后达到检索 recall/latency 目标。
- Runtime compatibility 或隔离测试无法同时达标，需要新增或替换 RuntimeClass。

每次重新评估必须给出：生产指标、故障语义、迁移/回滚方案、双写窗口、数据验证和 Conformance 结果。

## 23. 主要风险与缓解

| 风险 | 缓解 |
|---|---|
| Go + Rust 增加团队复杂度 | Rust 仅限独立 Runtime 服务；Protocol 隔离；明确 owner 与培训 |
| PostgreSQL 成为热点 | 短事务、分区、只读投影、容量测试；先优化模型再讨论分布式 SQL |
| NATS 重复/乱序 | inbox unique、aggregate version、sequence gap detection、幂等 side-effect ledger |
| OPA bundle 漂移 | 签名 revision、状态报告、fail closed、Audit 记录 revision |
| gVisor 不兼容或性能下降 | RuntimeClass capability matrix、基准、Wasmtime/Firecracker 替代路径 |
| MCP/A2A 快速变化 | Edge Adapter、版本协商、fixture/conformance、内部对象不直接复用外部 schema |
| OpenSearch ACL 错配 | pre-filter + exact post-check、高敏感物理隔离、可重建索引和隔离测试 |
| Secret 经结果侧漏 | Broker handle、Gateway 注入、结构化脱敏、canary secret、最短 lease |
| Audit 被普通日志替代 | 事务内 append、hash chain、签名 WORM 归档、独立访问策略 |

## 24. 官方依据

- [Go 当前稳定版本与兼容策略](https://go.dev/VERSION?m=text)
- [Go Profile-Guided Optimization](https://go.dev/doc/pgo)
- [Rust：Fearless Concurrency](https://doc.rust-lang.org/book/ch16-00-concurrency.html)
- [PostgreSQL 18 Release Notes](https://www.postgresql.org/docs/current/release.html)
- [PostgreSQL Row Security](https://www.postgresql.org/docs/current/ddl-rowsecurity.html)
- [PostgreSQL `SKIP LOCKED`](https://www.postgresql.org/docs/18/sql-select.html)
- [NATS JetStream delivery semantics](https://docs.nats.io/nats-concepts/jetstream)
- [NATS JetStream clustering](https://docs.nats.io/running-a-nats-service/configuration/clustering/jetstream_clustering)
- [Apache Kafka delivery semantics](https://kafka.apache.org/41/design/design/)
- [Kubernetes Scheduling Framework](https://kubernetes.io/docs/concepts/scheduling-eviction/scheduling-framework/)
- [Kubernetes controller pattern](https://kubernetes.io/docs/concepts/extend-kubernetes/)
- [Kubernetes supported releases](https://kubernetes.io/releases/)
- [OPA integration choices](https://www.openpolicyagent.org/docs/integration)
- [OPA Policy Bundles](https://www.openpolicyagent.org/docs/management-bundles)
- [SPIFFE concepts](https://spiffe.io/docs/latest/spiffe/concepts/)
- [SPIRE concepts](https://spiffe.io/docs/latest/spire-about/spire-concepts/)
- [OpenBao lease/renew/revoke](https://openbao.org/docs/concepts/lease/)
- [Wasmtime security model](https://docs.wasmtime.dev/security.html)
- [Wasmtime proposal stability matrix](https://docs.wasmtime.dev/stability-wasm-proposals.html)
- [WASI Component Model status](https://component-model.bytecodealliance.org/)
- [gVisor security architecture](https://gvisor.dev/docs/architecture_guide/intro/)
- [Firecracker design and sandboxing](https://github.com/firecracker-microvm/firecracker/blob/main/docs/design.md)
- [pgvector indexing and filtered ANN](https://github.com/pgvector/pgvector)
- [OpenSearch Hybrid Search and pre-filtering](https://docs.opensearch.org/latest/vector-search/ai-search/hybrid-search/index/)
- [OpenTelemetry signals](https://opentelemetry.io/docs/concepts/signals/)
- [ORAS / OCI Artifacts](https://oras.land/docs/)
- [Sigstore Cosign verification](https://docs.sigstore.dev/cosign/verifying/verify/)
- [Buf breaking-change detection](https://buf.build/docs/breaking/)
- [MCP 2026-07-28 specification update](https://blog.modelcontextprotocol.io/posts/2026-07-28/)
- [A2A Protocol 1.0 specification](https://a2a-protocol.org/latest/specification/)
- [S3 Object Lock / WORM](https://docs.aws.amazon.com/AmazonS3/latest/userguide/object-lock.html)
- [CloudNativePG architecture](https://cloudnative-pg.io/documentation/current/architecture/)

---

## 25. 最终结论

推荐栈的核心不是组件数量，而是责任边界：

> **PostgreSQL 保证真源与事务，Go 保证控制面交付效率，Rust 缩小高风险 Runtime 的内存安全风险，NATS 负责可恢复分发，Kubernetes 负责基础设施，OPA/SPIRE/OpenBao 负责不可绕过的策略、身份和 Secret，所有外部协议与搜索系统都保持可替换。**

这套组合能够从小规模 v0.1 开始，又不会牺牲架构文档已经定义的状态、安全、预算、恢复和兼容性不变量。
