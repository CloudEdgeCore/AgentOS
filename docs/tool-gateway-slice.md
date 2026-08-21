# Tool Gateway Slice: Implementation Baseline

> 文档状态:Implemented(v0.1 kernel slice)
> 日期:2026-08-16
> 关联文档:[ADR-008](adr/008-policy-engine.md)、[架构设计 §9.2/§12.2](Agent_OS_完整架构设计.md)、
> [技术选型 §12.2](Agent_OS_技术选型与工程基线.md)

## 1. 定位

Tool Gateway 是预算硬停止与工具副作用证据链的执行点,完成 v0.1 验收基线 #5、#6:
Tool Call 具备完整的 **Policy Decision、Usage 与 Side-effect Receipt**。决策链、
独立 `agentos-gateway` 进程、人工审批恢复闭环与 **MCP 边缘适配器**均已落地并通过
端到端测试。

## 2. 已落地代码

```text
db/migrations/000007_tool_gateway.up.sql   # tool_descriptors / tool_calls / tool_approvals
internal/kernel/store/tool.go              # store 契约 + 决策账本状态机 + approval 绑定
internal/kernel/store/postgres/tool.go     # PostgreSQL 实现(幂等/CAS/唯一绑定)
internal/kernel/tool/args.go               # JSON Schema 校验 + 规范化参数 + 规范哈希
internal/kernel/tool/gateway.go            # 决策链编排器(策略/审批/预算/receipt)
internal/kernel/tool/gateway_test.go       # 10 个单元测试(fake store/executor/secrets)
internal/kernel/store/postgres/tool_integration_test.go  # 5 个集成测试(真实 PostgreSQL)
internal/kernel/policy/agentos.rego        # tool.allow / tool.deny_reasons / tool.requires_approval
internal/control/api/handler.go            # GET /v1/tools、POST /v1/approvals/{id}:decide
api/openapi/control-v1alpha1.yaml          # ToolDescriptor/ToolApproval/ToolList 契约
proto/agentos/gateway/v1alpha1/gateway.proto  # fenced gRPC 边界(ListTools/InvokeTool)
internal/gateway/service.go                # gRPC 服务:tenant 校验 + 错误码映射 + 有界消息
internal/gateway/service_test.go           # 5 个单元测试(bufconn + fake invoker)
internal/gateway/dev.go                    # DevExecutor + DevSecretBroker(仅开发)
cmd/agentos-gateway/main.go                # 独立进程(loopback-only + 固定 tenant + seed 工具)
internal/runtime/reference/worker.go       # workload spec 工具脚本执行 + 审批停泊/恢复
internal/kernel/workload/spec.go           # Spec.ToolCalls 确定性工具脚本字段
proto/agentos/runtime/v1alpha1/runtime.proto  # Assignment.phase + Assignment.approval_id
internal/runtime/control/service.go        # assignmentProto 携带 phase/approval_id
internal/kernel/store/runtime.go           # RuntimeAssignment.PendingApprovalID
internal/runtime/control/conformance_integration_test.go  # 端到端测试(执行链 + 审批恢复)
internal/mcp/message.go                    # JSON-RPC 2.0 严格消息模型(重复键/批处理/未知字段拒绝)
internal/mcp/server.go                     # Streamable HTTP 传输(JSON/SSE、406/415、1 MiB 上限)
internal/mcp/gateway.go                    # ToolAdapter:IdentityResolver + 映射规则 + 确定性幂等键
internal/mcp/server_test.go                # 11 个协议/传输单元测试
internal/mcp/gateway_test.go               # 4 个适配单元测试
internal/mcp/integration_test.go           # 端到端(真实 PostgreSQL,独立 schema)
internal/runtime/reference/identity.go     # IdentitySlot:执行窗口内 fenced 身份发布/清除
internal/runtime/reference/invoker.go      # GrpcToolInvoker:MCP 调用经 gRPC 转发到 Tool Gateway
internal/runtime/reference/worker.go       # WithIdentitySlot + RunOnce 窗口内 set/clear
cmd/agentos-runtime-reference/main.go      # -mcp-listen:沙箱 Agent MCP 端点(需 -gateway-address)
internal/runtime/reference/identity_test.go / mcp_endpoint_test.go  # 身份槽 + 端点转发单元测试
internal/runtime/reference/integration_test.go  # 沙箱 MCP 入口端到端(独立 schema)
deploy/dev/tenant-policies.json            # dev 租户 allowed_tools + approval_required_risk
```

## 3. 决策链(与架构 §9.2 时序一致)

```text
InvokeTool(tenant, task, run, attempt, tool, action, resource, args, idempotencyKey[, approvalID])
  ├─ 解析版本化 ToolDescriptor(注册表,不可变 per name@version)
  ├─ 参数规范化:JSON Schema 校验 → 规范化文档 + SHA-256(审批/幂等绑定的基准)
  ├─ receipt 重放检查:同 key 同 hash → 返回已存储结果;同 key 异 hash → 冲突
  ├─ 策略决策(嵌入 Rego,默认拒绝)
  │    ├─ DENIED → 决策账本记录 DENIED + deny reasons + policy revision
  │    ├─ REQUIRES_APPROVAL → 创建绑定 approval(attempt+tool+version+action+resource+argsHash+expiry)
  │    │     返回 approvalID;re-invoke 携带 approvalID 时校验绑定(不变量 #9)
  │    └─ ALLOWED → 继续
  ├─ 预算硬停止:ledger 已耗尽 → FAILED(BUDGET_EXHAUSTED),不执行(不变量 #12)
  ├─ 结算:SettleTaskUsage(ToolCalls+1),由 ledger 本身强制执行(并发安全)
  ├─ Secret Broker:签发 scope-limited handle,结果侧脱敏(§12.4)
  ├─ 执行:注入的 ToolExecutor(adapter 可替换)
  └─ side-effect receipt 落库(runtime_operation_receipts, operation='TOOL:name@version')
       成功 → EXECUTED;失败 → FAILED + failure code,重放返回同一失败
```

## 4. 安全语义

- **默认拒绝**:租户策略数据缺失、评估错误、未列入 `allowed_tools` 一律 DENIED。
- **Approval 不可复用**(不变量 #9):approval 绑定 call_id + attempt + 工具名/版本 +
  规范化参数哈希 + 目标资源 + 过期时间;其他调用复用 → `ErrApprovalNotUsable`。
- **幂等**(不变量 #6):receipt 主键 = (tenant, attempt, operation, idempotencyKey);
  重放不重新执行;同 key 异语义 → 冲突。外部副作用只承诺 at-least-once + 幂等消费(ADR-003)。
- **Secret 不外泄**:输出有界 + 签发 handle 全文脱敏;失败调用也写 receipt,重放返回同一失败。
- **状态机**:PENDING → REQUIRES_APPROVAL → APPROVED → EXECUTED;DENIED/FAILED 终态;
  store 层 `CanTransitionTo` + 行锁 + CAS 强制。

## 5. 验收证据

- 单元测试:tool 决策链 10 个 + gateway gRPC 边界 5 个 + worker 审批停泊/恢复 2 个。
- 集成测试:tool store 5 个(真实 PostgreSQL)+ **端到端执行链**
  `TestTaskToolCallFlowThroughGateway` + **端到端审批恢复**
  `TestTaskToolApprovalResumeThroughGateway`:高风险工具调用 → Attempt 停泊
  WAITING_APPROVAL(无预算结算、无执行)→ 人工决策 APPROVED → worker 恢复 →
  执行 → SUCCEEDED(调用 EXECUTED、receipt 1 条、结算 toolCalls=1)。
- OpenAPI 契约通过 kin-openapi 校验;buf lint 通过;Windows/Linux 双平台构建通过。

## 6. 审批恢复语义

- 首次遇到 REQUIRES_APPROVAL:worker 将 Attempt 转移至 WAITING_APPROVAL 并停泊;
  停泊期间每次轮询续租(lease 保持存活,不被 recovery 回收)。
- Kernel 通过 `tool_calls(status=REQUIRES_APPROVAL)` 解析 pending approval,
  随 Assignment(`approval_id` + `phase`)下发。
- 人工决策后:worker 重新呈现 approval;PENDING → 继续停泊;
  APPROVED → WAITING_APPROVAL→RUNNING → 重放已执行调用(receipt)→ 执行待批调用
  → checkpoint(confirmed receipt)→ 完成;REJECTED/EXPIRED → Attempt 失败收敛。
- 审批与 attempt 绑定(不变量 #9):worker 崩溃导致租约过期 → 新 attempt 重新请求
  审批(旧审批作废,需重新决策)——已文档化的 v0.1 语义。

## 7. MCP 边缘适配(架构 §15.1:Agent ↔ Tool = MCP)

`internal/mcp` 实现固定协议版本 `2026-07-28` 的 MCP 核心子集:
`initialize`、`notifications/initialized`、`ping`、`tools/list`、`tools/call`,
Streamable HTTP 传输(JSON 与 SSE 响应),无会话状态。所有输入按不可信处理:
严格 JSON(未知字段/重复键/批处理/尾部数据拒绝)、1 MiB 请求上限、bounded 处理。

映射规则(v0.1,文档化):

- **身份**:MCP 请求不携带身份。`IdentityResolver` 注入 fenced `AttemptContext`
  (tenant/task/run/attempt/fencing token/agent version);生产形态由持有
  Attempt 的 Runtime 注入,`StaticIdentity` 仅用于测试与显式 dev 布线。
- **工具名** = descriptor 名称(描述中含版本与风险),调用解析最新版本。
- **action** 默认 descriptor 首个声明动作(无则 `invoke`);
  **resource** 默认首个资源模式(无则 `tool:<name>`)。
- **幂等键** 由 attempt + 规范化参数确定性派生:相同调用重放(不重复执行/结算),
  不同参数是新调用——外部副作用维持 at-least-once + 幂等消费(ADR-003)。
- **结果**:EXECUTED/REPLAYED → text content,`isError=false`;DENIED /
  REQUIRES_APPROVAL / 预算硬停 → `isError=true` + 结构化文本。

验收:`TestMcpClientDrivesToolGatewayEndToEnd`(独立 schema,真实 PostgreSQL)——
JSON-RPC 客户端 initialize → tools/list → tools/call → fenced 决策链 →
断言 tool_calls EXECUTED、预算结算 1、receipt 1;相同调用重放不重复结算。

### 沙箱内 Agent 的 MCP 入口(Runtime 持有)

生产形态:Agent 运行在 RuntimeClass 内,通过 loopback MCP 调用工具。reference
Runtime 实现了该入口:

- `IdentitySlot`:worker 在每个 assignment 执行窗口内发布 fenced 身份
  (tenant/task/run/attempt/fencing token/agent version),窗口外 **默认拒绝**
  (`-32001` 或工具级 `isError`)。并发安全,worker 单线程循环安全。
- `GrpcToolInvoker`:MCP 调用经 fenced Tool Gateway gRPC 边界转发
  (fencing token 全程保留——为此 `tool.InvokeInput` 新增 `FencingToken`,
  gateway service 重建 input 时回填)。
- `cmd/agentos-runtime-reference -mcp-listen 127.0.0.1:9092`(需
  `-gateway-address`)启动端点。

验收:`TestSandboxAgentMcpEntryDrivesRealDecisionChain`(独立 schema,真实
PostgreSQL)——真实 attempt + worker 身份槽 + MCP 调用 → gRPC → 决策链 →
断言 tool_calls EXECUTED、预算结算 1、receipt 1;窗口外调用 fail closed。

## 8. Model Gateway(技术选型 §12.1)

流式模型调用的内核决策链,与 Tool Gateway 对称:

```text
Begin(modelRef, attempt 身份)
  ├─ 解析 ModelDescriptor(provider/model,价格表 + price revision,不可变)
  ├─ Rego 策略:tenant.allowed_models 默认拒绝(MODEL_NOT_ALLOWED)
  ├─ 预算预检:ledger 已 exhausted → 硬停,不开流
  └─ 创建 model_calls 账本行(STARTED,钉住 price revision,幂等键防重)
Settle(call, sequence, usage)
  ├─ 增量结算:幂等键 model:<callID>:<seq>(ledger 自动去重,重试不重复计费)
  └─ 超限 → ledger 标记 exhausted + ErrBudgetExhausted → 流式调用方有界时间内中断
Finish(call, finalUsage, reason, providerRequestID)
  ├─ 最终结算(幂等键 model:<callID>:finish)
  ├─ 成本由内核按钉住的价格表计算(不信任调用方上报)
  ├─ 超限 → 状态 STOPPED
  └─ 审计 receipt:仅元数据(model/status/tokens/cost/revision/reason/requestID),
     绝不存储 prompt/completion 内容
```

已落地:迁移 000008(model_descriptors/model_calls)、`internal/kernel/model` 决策链、
`internal/kernel/store/model.go` + postgres、Rego `model.allow/deny_reasons`、
单元测试 6 个 + 集成测试 3 个(真实 PostgreSQL:注册表不可变/价格修订、
调用账本 CAS/状态机、**token 预算硬停止端到端**——400 tokens 精确结算、重放不重复计费、
超限 Settle/Finish 被拒)。传输层(gRPC/worker 集成)为下一增量。

## 9. 1,000+ 并发负载验收(基线 #2)

`TestThousandConcurrentTasksStableUnderLoad`(独立 schema,真实 PostgreSQL):
预定义负载 1,000 个任务(30% 携带工具脚本,经真实 fenced Tool Gateway)在
admission + scheduling + 8 个 reference runtime worker 下收敛,并输出验收报告。

实测(2026-08-16,本机 PostgreSQL):

```text
tasks=1000 succeeded=1000 failed=0  submission=249ms
concurrent-running peak=1000 (target >= 1000)
task duration p50=54s  p95=1m25s  p99=1m27s
worker RunOnce p50=63ms p95=87ms p99=96ms
tool calls=300 ratio=30.0% settledToolCalls=300
```

负载测试驱动了三个工程修正(均为真实发现,非测试问题):

1. **并发领取竞争**:多个 worker 可能同时 poll 到同一 PLACED attempt;
   transition 的版本冲突(Aborted)现在被 worker 视为"已被他人领取"并跳过,
   而不是报错重试。
2. **瞬态事务冲突重试**:`store.IsRetryableTransaction` + gRPC 映射为
   `Unavailable`;控制器(`store.RetryRetryable`)与 worker(complete/checkpoint)
   都做有界退避重试(ADR-002: bounded retries)。
3. **隔离级别修正(ADR-002 已更新)**:实测 16 路并发 complete 在 SERIALIZABLE
   下冲突率 **94%**(SSI 页面级谓词锁使不相干行写入互相 abort),同一不变量
   (行锁 + CAS + 唯一约束)在 READ COMMITTED 下 **0 冲突、~50 倍吞吐**
   (`TestConcurrentCompletionConflictRate` 锁定该预算)。`beginSerializable`
   保留给未来行锁无法表达的不变量,采用前必须实测冲突率。

`AGENTOS_LOAD_TASKS` 可调任务数(CI 用默认 1000,本地调试可调小)。

## 10. Model Gateway 传输层与精确结算(2026-08-20 完成)

fenced gRPC 边界(`agentos.model.v1alpha1`,proto `model.proto`)已与 reference
worker 端到端集成:

- `internal/gateway/model_service.go`:Begin/Settle/Finish gRPC service,
  `BeginResponse.resource_version` 携带账本 CAS 版本(Finish 的 expected_version
  由 Begin 钉住)。
- `cmd/agentos-runtime-reference -model-gateway-address`:workload spec
  `modelCalls` 脚本在工具脚本之后执行:Begin → Finish(确定性脚本无中间步,
  不重复结算)。
- **精确结算修正(四角度审查发现)**:原 Finish 直接按"最终总量"再结算一次,
  与 Settle 增量步叠加会**双重计费**。现引入预算原语
  `SettleTaskUsageDelta`:在账本行锁下对调用族
  (`model:<callID>:*`,含各 seq 步与 finish 键)求已结算和,只结算
  `max(0, 最终总量 − 已结算)`,幂等键 `model:<callID>:finish`。
  流式(步进 Settle + 累计 Finish)、仅 Finish、崩溃重放(族已达目标 → delta 0)
  均**恰好计费一次**。无预算预留的任务与 Tool Gateway 一致:不设上限、不计量。
- 验收:
  - `TestModelGatewayFinishSettlesExactlyOnceAgainstRealLedger`(真实
    PostgreSQL):80+40 步进结算 + Finish(150)→ 累计恰好 150,族结算行 3 条,
    重放 delta 收敛 0。
  - `TestTaskModelCallFlowThroughGateway`(conformance e2e,真实 fenced
    gRPC):model_calls COMPLETED、成本按钉住价格表计算、预算结算恰好 200、
    receipt `MODEL:openai/gpt-4o` 1 条、结果文档携带 modelResults。
  - `TestTaskEventsStreamsLifecycle` 等:控制面 SSE 事件端点
    `GET /v1/tasks/{taskID}/events`(初始快照 + 版本变化流 + terminal 关闭 +
    心跳,OpenAPI `TaskEvent` 契约,kin-openapi 校验通过)。

## 11. 下一 slice(依赖本 slice)

1. **签名 Policy Bundle 分发**(ADR-008 deferred):Gateway 多服务分发时才需要。
2. **OCI Provider 接入 Linux CI**:验证 ctr/runsc 命令面并加入 conformance 第三腿。
3. **放行吞吐优化**:负载显示 admission/scheduler 每任务 ~10ms(1,000 任务放行
   ~20s);优化(批量、减少逐任务事务)将直接降低任务时长尾延迟。
