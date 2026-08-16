# Agent OS 项目现状(截至 v0.6)

日期:2026-08-16 · 分支:`agent/runtime-execution-recovery`

本文件是项目的整体进度说明:已完成什么、当前做到哪一步、验证到什么
程度、哪些是明确的边界、下一步做什么。每阶段的交付细节见对应的
`docs/v0.X-*.md`。

## 1. 项目是什么

Agent OS 是一个"Agent 操作系统"控制面:发布、调度、执行、恢复、治理与
审计异构 AI Agent 的系统软件层。核心设计:

- **PostgreSQL 18(pgvector)为唯一事实源**:`Task` / `Run` / `Attempt`
  持久化生命周期、不可变 `AgentVersion` 注册表、预算台账、租约 + fencing
  token、结果先于成功(Result-before-success)、ADR-002 事务语义
  (行锁 + CAS + 唯一约束,弃用 SERIALIZABLE)。
- **NATS JetStream 出站箱(outbox)**:生命周期事件与业务状态同事务写入,
  幂等发布,按聚合版本排序。
- **确定性运行时**:Go 参考 runtime + Rust/Wasmtime 无能力 provider +
  OCI/gVisor(runsc)provider(命令面待 Linux CI 复核)。
- **默认拒绝**:一切策略(ReGo 嵌入策略、签名包准入、审计、身份)失败即
  拒绝;边界文档化(SPIRE、Firecracker 等)。

## 2. 版本演进(40 次提交,6 个阶段)

| 阶段 | 主题 | 代表提交 | 交付内容 |
|---|---|---|---|
| v0.1 | 内核基线 | `3c0ed7c` → `88579a3`、`02a1cac` | 生命周期内核、调度与 IPC、执行与恢复循环、不可变版本注册表、任务预算台账 + 硬停、默认拒绝 Rego 策略、双 provider 一致性、SSE 任务流、OIDC、OTel、Memory 存储、JetStream 容量上限 |
| v0.3 | 安全运行时 | `ad737f5` → `dbe4a1e` | SPIFFE X.509-SVID mTLS 身份边界、签名 Agent Package 准入(ADR-010)、OpenBao 密钥代理、OpenSearch 记忆投影 + tombstone 删除传播、安全负向测试套件、Firecracker 边界文档化 |
| v0.4 | 生产加固 | `52f8946` → `7aefb39` | 事务哈希链审计台账 + 签名导出(ADR-014)、OpenSearch 审计投影、OpenBao 动态数据库凭据全租约生命周期(ADR-015)、控制器加固(逐任务隔离、路由预算、流上限、MCP 上限)、OCI 镜像 digest 钉 |
| v0.5 | 运行时加固与规模化就绪 | `85716f6` → `2e5ae30` | 容器类资源限额全链路强制、孤儿容器回收 + stdout 有界 spool、cosign 风格镜像签名 + CycloneDX SBOM、工件聚合配额 + 保留 GC、三控制器并行 Reconcile(P1) |
| v0.6 | 多租户规模与容量证据 | `d28ffcf` → `52c61fc` | 租户聚合消费配额、运行时感知池健康、O6 无放置立即释放 + 指数退避、双实例并发回归、1,000 任务容量基线(详见 §3) |
| — | 架构基线 | `6ead9a3` | 完整架构设计、技术选型与工程基线、四角度评审 |

## 3. v0.6 当前进度(已完成)

v0.6 Multi-tenant Scale & Capacity Evidence 六项全部交付、验证并提交,
工作树干净:

1. **租户聚合消费配额**——`tenant_quotas` + `tenant_consumption_windows`
   (固定窗口,epoch 对齐);结算与任务预算同事务、窗口行锁下精确累加
   (重放/delta 不重计);准入双层:控制器产出 `TENANT_QUOTA_EXCEEDED`
   拒绝决策,`DecideAdmission` 事务内原子复检;控制面 API
   `GET/PUT/DELETE /v1/quota`(仅认证主体租户)。
2. **运行时感知池健康**——`PoolInstanceHealth` 从租约心跳新鲜度派生实例
   活性;`LeaseAwarePoolSource` 叠加到静态池配置,失活 worker 的池被
   placement 以 `POOL_NOT_READY` 拒绝;恢复接管/正常完成自动自愈。
3. **O6 调度退避**——无放置任务立即释放 claim(不再等 TTL),任务行记录
   `next_schedule_attempt_at` + `schedule_retry_count`,5s×2ⁿ 指数退避
   (上限 5 分钟),多控制器实例共享同一进度;`TaskScheduleDeferred`
   outbox 事件 + 审计行。
4. **双实例并发回归**——两个 admission 控制器 + 两个 outbox dispatcher
   竞速真实 Postgres:每任务恰好准入一次、fencing 拒绝 0、零未发布事件;
   重复运行无抖动。
5. **容量基线**——1,000 任务全流水线(入队→准入→调度→完成)独立 schema
   可重复;断言全部是计数精确性,不断言墙钟阈值;开发机基线:
   端到端 ~40 tasks/s(入队 255/s、准入 215/s、调度 209/s、完成 85/s),
   端到端 p50 17.75s。这是 v0.6+ 中间件决策(是否需要 Kafka/ClickHouse)
   的对照证据。
6. **交付文档**——`docs/v0.6-multi-tenant-scale.md` + README 更新。

## 4. 当前能力清单(已实现)

**内核(control plane)**:任务/运行/尝试生命周期与 fencing、准入 +
  调度 + 恢复三控制器(并行 Reconcile、逐任务隔离、可重试退避)、不可变
  AgentVersion 注册表 + 签名包准入 + 镜像 digest/SBOM 钉、任务预算台账
  (四维,硬停)、租户消费配额、O6 调度退避、池健康、出站箱(排序 + 锁
  fencing)、结果先于成功、取消与恢复、SSE 任务事件流、审计台账
  (哈希链 + 签名导出)、Memory(向量 + 全文检索 + tombstone)、工具网关
  (风险分级 + 人工审批)、模型网关。

**运行时**:Go 参考 runtime、Rust/Wasmtime 无能力 provider、OCI/gVisor
  provider(资源限额、孤儿回收、stdout spool、镜像验签)、租约心跳 +
  checkpoint 信封 + 恢复接管、运行时协议(gRPC,3 个 proto 契约:
  runtime / gateway / model)。

**平台**:迁移器(14 个 checksum 保护迁移)、OpenSearch 投影(记忆 + 审计)、
  OpenBao 密钥代理 + 动态数据库凭据、SPIFFE X.509-SVID、OpenTelemetry、
  工件存储(内容寻址 + 配额 + 保留 GC)、NATS 发布器、12 个可执行
  (`agentos-control` / `-controller` / `-gateway` / `-migrate` / `-outbox` /
  `-pkg` / `-projector` / `-runtime-control` / `-runtime-oci` /
  `-runtime-reference` / `-svid` / `mtlsutil`)。

**控制面 API**(`/v1`):tasks(创建/查询/取消/SSE 事件流)、agent-versions、
  tools、approvals、memories、audit(列表/校验/导出)、quota、healthz,
  全部带 OIDC/静态身份、路由预算、严格 JSON 校验与幂等键。

## 5. 工程规模与验证状态

| 指标 | 数值 |
|---|---|
| 提交数 | 40(v0.1 → v0.6) |
| Go 非测试代码 | ~20.7k 行 |
| 测试代码 | ~14k 行(69 个测试文件,其中 19 个集成测试文件,282 个测试函数) |
| 数据库迁移 | 14 个(checksum 校验,`agentos_schema_migrations`) |
| proto 契约 | 3(runtime / gateway / model,`buf.yaml` 配置) |
| 文档 | 12 篇 Markdown + 1 份架构设计 PDF(本文件提交后 13 篇 Markdown) |

**验证(全部在当前机器上实际跑过,全绿)**:

- `gofmt` / `go build ./...` / `go vet ./...`;
- 全部单元测试(29 个包)与 `go test -race ./...`;
- 全部集成套件:postgres(含配额/池健康/O6/双实例/容量基线)、migrate、
  outbox(NATS)、mcp、runtime-control(85s,真机腿可跑部分)、
  reference、security(独立 schema `agentos_security`);
- Linux 交叉编译(`GOOS=linux go build ./internal/runtime/oci/...` 等)通过。

**开发环境(本机,全部在跑)**:PostgreSQL 18(pgvector)`127.0.0.1:55432`、
  NATS 2.14 `54222`、OpenBao 2.4 `58200`、OpenSearch 2.17 `39200`、
  Tempo/Loki/Prometheus/Grafana/OTel collector、Redis。

## 6. 明确边界(未实现,文档化投影)

- **SPIRE / workload attestation**(ADR-011):当前是"CA 签发 + 信任束
  分发"模型;SPIRE agent / Workload API 接入仍是投影。
- **Linux CI 真机腿**:runsc/containerd 的 ctr 命令面、seccomp /
  capability drop / 只读 rootfs、Firecracker KVM 等必须接入 Linux CI
  (containerd + runsc 主机)逐条验证;本机(Windows)只验证了协议与
  纯逻辑面。
- **Firecracker / microVM**:验收前置未变(Linux CI 验证、KVM 可用、
  版本矩阵钉死)。
- **预留式配额**:当前配额语义对已结算消费精确,同瞬并发准入按 ceiling
  之和有界超窗;reservation/release 模型与结算期硬停未纳入。
- **引用感知 GC**:工件 GC 是 TTL 式;按 checkpoint/result 引用存活分析
  的精确 GC 需与审计留存权衡。
- **跨实例分片**:多实例控制面安全并存已锁定(fencing 回归测试),按
  pool/tenant 分片是下一个增量。

## 7. 下一步建议(按优先级)

1. **跨实例分片 + 多租户隔离压力**:把调度按 pool/tenant 分区,扩容量
   基线的租户维度(多租户同跑,验证配额隔离在负载下的行为)。
2. **真机腿落地**:把 v0.5 清单的 Linux CI 腿(containerd + runsc、
   Firecracker KVM)接起来,这是多条边界解锁的前置。
3. **中间件决策**:当前单 Postgres 控制面 1,000 任务 ~40 tasks/s;在
   1 万 ~ 10 万任务规模重跑容量基线,再决定是否需要 Kafka/ClickHouse。
4. **配额强化**:预留式配额(release-on-completion)与结算期硬停。
5. **SPIRE Workload API**:从静态信任束走向运行时动态身份。
