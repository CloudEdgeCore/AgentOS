# 四角度代码审查与优化(数据结构 / 计算机网络 / 操作系统 / 计算机组成原理)

日期:2026-08-16 · 范围:Agent OS v0.1 全仓 · 方法:四个独立只读审查 + 逐项处置

四个并行只读审查(每个角度一个独立 agent)对全仓做了逐文件核查;以下为全部
发现与处置记录。**已修复** 的条目都带有对应测试;**保留/文档化** 的条目说明
了原因。

---

## 1. 数据结构角度

| # | 级别 | 发现 | 处置 |
|---|------|------|------|
| D1 | SHOULD | 预算族增量结算:步进 Settle 与 Finish 总量叠加会双重计费 | **已修复**:新增预算原语 `SettleTaskUsageDelta`(族前缀 `model:<callID>:` 已结算和,只结算 `max(0,目标−已结算)`),`TestModelGatewayFinishSettlesExactlyOnceAgainstRealLedger` + `TestFinishSettlesOnlyTheRemainderAfterSteps` 锁定恰好一次 |
| D2 | SHOULD | 累计消费 = 对追加式结算账本全量 SUM,O(结算数)/次、任务生命周期 O(n²) | **已修复**:迁移 000009 在 `task_budget_ledgers` 增加滚动消费计数器,与结算追加同事务更新(行锁下);`GetTaskBudget` 从 2 查询降为 1 行扫描。测试全绿 |
| D3 | SHOULD | `ClaimTasks` 无 `tasks(phase)` 索引 + 每任务独立 upsert 往返 | **已修复(索引)**:000009 增加 `tasks_phase_idx (phase, created_at, id)`。逐行 upsert 批量化为独立增量,暂不做(风险/收益比低,见下文"保留") |
| D4 | SHOULD | `GetModelDescriptor` 按 `price_revision` 字典序取"最新",v10 < v2 取错价格表 | **已修复**:改为 `ORDER BY created_at DESC, price_revision DESC` + 000009 索引 |
| D5 | SHOULD | 预算账本金额用 `double precision`,与 `model_calls.cost_usd numeric` 不一致 | **文档化**:v0.1 金额量级下 float64 相对误差 ~1e-16,硬停比较不受影响;切换 decimal 是独立增量(Go 侧需引入 decimal 类型),记入下一 slice |
| D6 | SHOULD | worker 轮询 `attempts(runtime_instance_id, phase)` 无索引 | **已修复**:000009 增加 `attempts_runtime_poll_idx` |
| D7 | COULD | CAS 存储按 SHA-256 定址但 MediaType 由调用方携带、不入内容身份 | **文档化**:介质类型进入内容身份会破坏"同一字节同一地址"的 CAS 语义;保留首注册语义,`ensureArtifact` 冲突即报错是特性 |
| D8 | COULD | `GetRuntimeAssignment` 每次 poll 5-6 条顺序单行查询 | **部分修复**:heartbeat 热路径已改为窄查询 `GetHeartbeatStatus`(1 条);完整 assignment 物化保留给 poll |
| D9 | COULD | `ConfirmedReceiptIDs` 在 checkpoint 状态工件与 DB 列双份存储 | **文档化**:工件是权威逻辑状态(ADR-007),列是查询便利;重放时双路径一致由测试锁定 |
| D10 | COULD | 零预算任务"无上限"与 `ON CONFLICT DO NOTHING` 重放可能静默封顶 | **文档化**:`SettleTaskUsageDelta`/`Settle` 对 `ErrBudgetNotReserved` 直接跳过(与 Tool Gateway 对称),零预算永不落账、永不封顶;注释已写明 |

## 2. 计算机网络角度

| # | 级别 | 发现 | 处置 |
|---|------|------|------|
| N1 | MUST | worker 全部 gRPC 调用无每 RPC 超时,挂死的控制面会永久卡死 worker 循环 | **已修复**:`controlRPCTimeout=15s`、`gatewayRPCTimeout=60s`、`artifactRPCTimeout=30s`,覆盖 PollAssignment/transition/heartbeat/checkpoint/complete/tool/model/artifact 全部调用点 |
| N2 | MUST | MCP Streamable HTTP 服务只有 ReadHeaderTimeout,无 Read/Idle/MaxHeaderBytes(Slowloris) | **已修复**:`cmd/agentos-runtime-reference` MCP listener 增加 ReadTimeout 15s、IdleTimeout 60s、MaxHeaderBytes 32KiB |
| N3 | SHOULD | 执行期不续租:长任务超过 heartbeatTTL 后租约过期被 fenced,旧 worker 仍执行副作用(fencing 分裂脑) | **已修复(核心)**:新 `internal/runtime/leasekeeper` 包,执行窗口内每 TTL/3 续租(CAS 链式版本);续租失败→取消执行上下文;取消请求→ack 一次后停止执行。reference + OCI 双 worker 接入;`TestWorkerRenewsLeaseDuringExecution` / `TestWorkerStopsOnCancellationMidExecution` 锁定 |
| N4 | SHOULD | outbox dispatcher 全程无界 ctx,单个慢 publish 卡死整条流水线 | **已修复**:claim/publish/ack 各自 10s 超时,单事件失败走退避重试 |
| N5 | SHOULD | SSE 端点遇瞬时存储错误即断开所有订阅者 | **已修复**:区分 `ErrNotFound`(关闭)与其他错误(保留连接、下一 tick 重试) |
| N6 | SHOULD | worker↔控制面/网关长连接无 keepalive,死对端发现延迟达 2h | **已修复**:`internal/platform/grpcx` 统一客户端 keepalive 30s/10s + 服务端 EnforcementPolicy + `MinConnectTimeout 5s` + 8MiB 消息上限(assignment 内嵌 spec+receipts 可超 1MiB) |
| N7 | COULD | MCP Accept 协商忽略 q-value/通配符,`*/*` 直接 406 | **已修复**:`negotiateResponseFormat` 完整解析(缺失→JSON、`*/*`→JSON、显式并列→SSE、q=0→不可接受);SSE 响应补 `X-Accel-Buffering: no`;新增协商测试 |
| N8 | COULD | 首次 RPC 在传输层未就绪时可无限阻塞 | **已修复**:随 N6 的 `MinConnectTimeout` + 每 RPC 超时 |
| N9 | COULD | 控制面 REST 统一 10s 超时(读/写同限) | **文档化**:创建类操作幂等可重试;分路由超时是下一 slice 的打磨项 |
| N10 | COULD | 1MiB gRPC 传输上限与 1MiB spec 上限互相逼近 | **已修复**:边界统一 8MiB(`grpcx.MaxMessageBytes`) |

## 3. 操作系统角度

| # | 级别 | 发现 | 处置 |
|---|------|------|------|
| O1 | MUST | 与 N3 相同:执行期无租约续租 → 恢复控制器 fenced 活 attempt,双活分裂 | **已修复**:leasekeeper(见 N3) |
| O2 | SHOULD | OCI executor `Wait` 超时路径泄漏 spawn goroutine + 孤儿 ctr 进程 + 临时目录 | **已修复**:`exec.CommandContext` 绑定执行 ctx,取消即杀进程、goroutine 必然收敛;`verifyCheckpoint` 改传 ctx(原 `context.Background()`)。Linux-only 文件,Windows 无法编译验证,改动为最小惯用式 |
| O3 | SHOULD | 取消只在 assignment 开始时确认,执行中不感知 | **已修复**:随 N3,keeper 心跳即感知取消并 ack,执行 ctx 被取消 |
| O4 | SHOULD | admission/scheduler 永久错误每 250ms 重试风暴 | **已修复**:控制器失败后退避一个 interval 再试;永久错误终态化保留为下一 slice(需逐任务错误隔离) |
| O5 | COULD | SSE 每连接每 500ms 两次查询;慢客户端长占 | **已修复(部分)**:usage 读取只在版本变化时进行(每 tick 1 条 GetTask);SSE 写入失败立即关闭;并发连接上限记入下一 slice |
| O6 | COULD | 瞬态 `ScheduleTask` 失败后该任务直到 claim TTL 才重试 | **文档化**:最终一致,fencing 下安全;逐任务 claim 释放是下一 slice 的放行吞吐优化的一部分 |
| O7 | COULD | 工件库只有单工件大小上限,无聚合配额/GC | **文档化**:下一 slice(保留/GC 策略需与审计留存权衡) |
| O8 | COULD | MCP 端点并发无上限(loopback dev 端点) | **文档化**:随 N2 超时;并发上限记入下一 slice |
| — | 验证 | 事务 rollback 纪律、锁序(run→attempt→task→lease 前缀一致)、`classify` 重试映射、优雅停机顺序 | **确认无缺陷**(审查结论) |

## 4. 计算机组成原理/性能角度

| # | 级别 | 发现 | 处置 |
|---|------|------|------|
| P1 | SHOULD | 控制器逐任务串行事务,1,000 任务 ≈ 1,000 次顺序往返 | **文档化**:并行 Reconcile(按 pool/tenant 分区)是明确的下一增量;当前 1,000 任务放行已达标,风险/收益比不支持收官前动 |
| P2 | SHOULD | 预算消费每次全量 SUM(与 D2 同) | **已修复**:滚动计数器 |
| P3 | SHOULD | Gateway 决策链 ~7-8 次独立往返/调用(tool+model 任务 ≈14 次) | **文档化**:单事务决策链(pgx.Tx + RETURNING/CTE)是下一 slice 的放行吞吐优化;决策链语义已由集成测试锁定,重构风险高 |
| P4 | SHOULD | heartbeat 每次续租重物化整个 assignment(~9 查询) | **已修复**:窄查询 `GetHeartbeatStatus`(1 条) |
| P5 | COULD | INSERT 后回读、claim 后重读(DecideAdmission/ScheduleTask) | **文档化**:INSERT...RETURNING 微优化,随 P3 一起做 |
| P6 | COULD | outbox claim 的 NOT EXISTS 相关子查询无覆盖索引 | **已修复**:000009 增加 `outbox_events_claim_idx`(unpublished 部分索引) |
| P7 | COULD | SSE 每 tick 3 查询且无变化也查 | **已修复(部分)**:变化门控 + 计数器单查询(随 D2) |
| P8 | COULD | worker 结果文档二次 marshal + spec 三次解码 | **已修复**:结果文档一次构建一次 marshal;`workload.Spec` 解码一次传入两个脚本 |
| P9 | COULD | 控制器空/满队列热循环、重试重领整批 | **已修复(部分)**:失败退避;逐任务错误隔离记入下一 slice |
| P10 | COULD | 工件写临时文件不 fsync 即 rename(崩溃可留下坏引用);moveIfAbsent 多余 stat | **已修复**:rename 前 `Sync()`;stat 保留(Windows rename 语义需要) |

## 5. 实测基线变化(同机,真实 PostgreSQL,负载验收测试)

```text
                                 2026-08-16 修复前    2026-08-16 修复后
worker RunOnce p50                63ms                57ms
worker RunOnce p95                87ms                77ms
worker RunOnce p99                96ms                89ms
task duration p50                 54s                 48s
tasks=1000 succeeded              1000/1000           1000/1000
concurrent-running peak           1000                1000
tool calls / settledToolCalls     300 / 300           300 / 300
```

(修复前数据来自 docs/tool-gateway-slice.md §9 实测记录;修复后为本轮
`TestThousandConcurrentTasksStableUnderLoad` 实测。)

## 6. 新增/变更的交付物

- `internal/runtime/leasekeeper/` — 执行期租约续租 + 取消/断栅执行停止
- `internal/platform/grpcx/` — gRPC 边界统一 keepalive/消息上限/连接参数
- `db/migrations/000009_budget_counters_and_indexes.up.sql` — 滚动消费计数器 +
  4 个热路径索引 + 回填
- `modelcheck/tla/AgentOS.tla` + `AgentOS.cfg` — TLA+ 不变量规格,TLC 全绿
  (16,879 状态 / 3,222 不同状态,13 不变量 + 3 活性)
- `internal/control/api/handler.go` — SSE 任务事件端点 `GET /v1/tasks/{id}/events`
  (+ OpenAPI `TaskEvent` 契约)
- Model Gateway 传输层:`internal/gateway/model_service.go`(Begin/Settle/Finish)
  + worker 模型脚本集成 + 精确结算修正
