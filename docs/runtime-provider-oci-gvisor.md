# OCI + gVisor Runtime Provider: Engineering Baseline

> 文档状态:Implementation baseline
> 日期:2026-08-16
> 关联文档:[ADR-006](adr/006-runtime-protocol.md)、[ADR-007](adr/007-logical-checkpoint-recovery.md)、
> [技术选型 §10.1](Agent_OS_技术选型与工程基线.md)、[架构设计 §11](Agent_OS_完整架构设计.md)

## 1. 定位

`oci-gvisor` 是 v0.1 的第二个 Runtime Provider(与 Wasmtime Provider 并列),
面向需要 Linux 用户态兼容性的普通不可信 workload。文档中的 `container-gvisor`
RuntimeClass 在本仓库中沿用现有代码约定,名为 `oci`(见 `deploy/dev/runtime-pools.json`)。

Provider 通过 fenced gRPC Runtime Protocol 与 Kernel 交互,不直接访问数据库:
轮询 assignment → STARTING/RUNNING 转移 → heartbeat/取消 → 逻辑 Checkpoint →
结果 Artifact → CompleteAttempt(ADR-006)。它绝不回退到无沙箱运行时。

## 2. 已落地代码

```text
internal/runtime/oci/executor.go           # Executor 契约、ExecutionSpec、RunResult
internal/runtime/oci/worker.go             # fenced 协议循环(Provider 无关)
internal/runtime/oci/worker_test.go        # fake Executor + bufconn 全协议测试
internal/runtime/oci/runsc_linux.go        # containerd ctr + runsc 执行器(Linux)
internal/runtime/oci/runsc_unsupported.go  # 非 Linux 编译桩,拒绝启动
cmd/agentos-runtime-oci/main.go            # 独立 Provider 进程入口
```

Worker 语义:

- 每个 Attempt 的每次状态变更都携带 `AttemptIdentity`(tenant + attempt +
  fencing token)与 `expected attempt version`;失败时以机器可读
  `failure_code` 转移到 `ATTEMPT_FAILED`,由 recovery controller 决定重试或终止(ADR-007)。
- 逻辑 Checkpoint envelope 声明 `Provider=oci-gvisor`、
  `RuntimeABI=agentos.oci/v1`、`SchemaVersion=agentos.oci-logical/v1`;
  恢复前校验 AgentVersion、RuntimeClass、Provider、ABI、schema 与 goal digest。
- 执行结果与 Checkpoint state 都是内容寻址 Artifact 引用,不进入 gRPC 消息体;
  container stdout 当前被有界丢弃,后续 slice 必须 spool 到 Artifact Store。

## 3. containerd/runsc 命令面(需在 Linux CI 验证)

Linux 执行器驱动 containerd CLI(`ctr`),默认 namespace `agentos`、默认 runtime
`io.containerd.runsc.v1`。以下命令面必须在接入 CI 时对固定的 containerd 版本
逐条验证(当前实现集中且带明确错误信息,便于替换):

| 步骤 | 命令 | 备注 |
|---|---|---|
| 拉取 | `ctr -n agentos images pull <ref>` | 生产必须 digest 固定;开发可用 `-skip-image-pull` |
| 启动 | `ctr -n agentos run --runtime io.containerd.runsc.v1 --mount type=bind,src=<spec>,dst=/agentos/input/workload.json,options=rbind:ro [--mount type=tmpfs,dst=/agentos/workspace,options=size=N] --env AGENTOS_* <ref> <id>` | 前台运行,ctr 返回即任务退出;退出码取自 ctr 进程 |
| 取消 | `ctr -n agentos tasks kill <id>` | SIGTERM;`Wait` 在 ctx 取消时调用 |
| 清理 | `ctr -n agentos tasks delete -f <id>`;`ctr -n agentos containers delete <id>` | not-found 视为成功 |

环境注入(仅白名单):`AGENTOS_TENANT_ID`、`AGENTOS_ATTEMPT_ID`、
`AGENTOS_AGENT_VERSION_REF`、`AGENTOS_INPUT_PATH`、`AGENTOS_WORKSPACE_PATH`。
工作负载 spec 以只读 bind mount 注入,工作区是大小有界 tmpfs;除此之外不挂载
任何宿主路径。

## 4. 安全加固清单(生产前必须完成)

runsc 本身提供 syscall 隔离,但 Provider 层仍需完成:

1. Admission 强制:image 必须 digest 固定;`RuntimeClass=oci` 的 workload
   必须有显式 CPU/内存/工作区限制(本代码零值 = 无限制,Admission 不得放行)。
2. runsc/containerd 配置:user namespace、只读 rootfs、capability drop、
   seccomp、cgroup v2 限额、NetworkPolicy/egress proxy。
3. 孤儿容器回收:worker 崩溃后遗留的 `agentos-*` 容器由启动时预检清理,
   并配合 lease 过期恢复(ADR-007)。
4. 输出治理:stdout 有界丢弃 → spool 到 Artifact;结果文档只含结构化字段,
   不携带 Secret 或未脱敏输出。
5. 开发传输仅 loopback + 固定 tenant(与 reference Provider 相同约束);
   生产暴露依赖 SPIFFE mTLS(ADR-006)。

## 5. 验收标准(接入 Linux CI 后)

- [ ] 同一 `AgentVersion` 在 Wasmtime 与 `oci-gvisor` 两个 Provider 上通过
      conformance 套件(扩展 `TestSameAgentVersionRunsOnBothProviders` 的第三腿)。
- [ ] 故障注入:进程 kill、旧 fencing token 迟到写入、消息重复——行为与
      reference Provider 一致。
- [ ] runsc 安全基线测试:越权 syscall、网络、文件访问全部 fail closed。
- [ ] 命令面逐条验证并固定 containerd/runsc 版本矩阵。
- [ ] 部署拓扑:DaemonSet/Deployment 管理 Runtime Worker 池,新增
      `deploy/dev/runtime-pools.json` 条目(当前 `dev-worker-1` 仍由 reference
      Provider 占用 `oci` 类)。

## 6. 明确不做(本 slice)

- 通用透明进程快照;恢复只承诺逻辑 Checkpoint envelope(ADR-007)。
- Firecracker/microVM 高隔离 Provider(v0.3,独立 RuntimeClass)。
- 从 Kernel 进程内加载第三方 Runtime 动态库(ADR-001)。
