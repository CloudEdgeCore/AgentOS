# Firecracker / microVM Runtime Provider: Explicit v0.3 Boundary

> 文档状态:Boundary (explicitly not implemented in v0.3)
> 日期:2026-08-16
> 关联文档:[ADR-006](adr/006-runtime-protocol.md)、[ADR-007](adr/007-logical-checkpoint-recovery.md)、
> [OCI/gVisor Provider](runtime-provider-oci-gvisor.md)、[v0.3 Secure Runtime](v0.3-secure-runtime.md)

## 1. 定位与边界

Firecracker microVM 是架构中面向最高隔离需求的 Runtime Provider 投影:
每个 Attempt 一个专用 microVM,由 **Kernel 直接管理**(非容器运行时),
内存开销 ~5 MiB/VM,启动 ~125 ms,通过 jailer + seccomp + cgroup v2 获得
硬件虚拟化级隔离。

**v0.3 明确不实现 Firecracker。** 它保持为文档化边界,原因:

1. v0.3 的安全切片(SPIFFE mTLS 身份边界、签名 Agent Package 准入、
   OpenBao Secret Broker、OpenSearch 投影、负向测试套件)已经把
   Runtime Protocol 之外的边界全部封口;Runtime 侧的工程基线仍是
   OCI/gVisor(runsc),且其安全加固清单(见
   `runtime-provider-oci-gvisor.md` §4)尚未在 Linux CI 上逐条验证。
2. Firecracker 的 KVM/API 依赖使其无法在当前 Windows 开发机与
   macOS CI 上验证;把它作为“已交付”会违反本仓库“未验证不承诺”的纪律。
3. microVM 与现有逻辑 Checkpoint 模型(ADR-007)的关系需要专门设计:
   Firecracker snapshot/restore 是**进程级快照**,而 Agent OS 的恢复契约
   是逻辑 envelope——两者可以结合(快照加速 + 逻辑校验),但不能混为一谈。

## 2. 若实现,它将是什么样

以下内容已与现有代码约定对齐,是后续 slice 的实施蓝图,不是承诺:

- **RuntimeClass**:`microvm`(与 `oci`、`reference-go`、`wasmtime` 并列),
  在 `deploy/dev/runtime-pools.json` 新增独立池;Admission 为
  `RuntimeClass=microvm` 的 workload 强制 CPU/内存/工作区限额(零值不放行,
  与 OCI 基线 §4.1 同规则)。
- **Provider 进程**:`cmd/agentos-runtime-microvm`(独立 fenced 进程,走
  ADR-006 Runtime Protocol,与 reference/oci/wasmtime 相同的 worker 循环:
  Poll → STARTING/RUNNING → heartbeat → checkpoint → CompleteAttempt)。
- **执行器**:`internal/runtime/microvm/` 包,驱动 Firecracker REST API
  (`/boot-source`、`/drives`、`/network-interfaces`、`/actions`(InstanceStart/
  SendCtrlAltDel)),经 jailer 启动;每 Attempt 一个 VM,结束即销毁。
- **环境注入**:仅白名单 env + 只读 virtio-blk 输入卷 + 有界 tmpfs 工作区
  (与 OCI 基线的 mount/env 白名单语义一致)。
- **快照边界**:v1 只交付“每 Attempt 冷启动 microVM + 逻辑 Checkpoint”;
  snapshot/restore 加速恢复是独立 slice,恢复正确性始终以逻辑 envelope
  校验为准(ADR-007)。
- **身份**:复用 v0.3 已交付的 SPIFFE 边界——microVM Provider 进程持
  `spiffe://<domain>/ns/<tenant>/worker/<instance>` SVID 连接
  runtime-control,与 reference/oci worker 完全一致。

## 3. 为什么现在这个边界是安全的

v0.3 的威胁模型不依赖 microVM:

- **身份**:Runtime Protocol 已强制 X.509-SVID mTLS + 请求 claim 绑定
  (ADR-011),任何 Provider(含未来的 microVM)都不能冒充租户/实例。
- **可信执行**:Agent 可执行内容来自签名 Agent Package,发布准入 fail-closed
  (ADR-010);runtime 侧的运行隔离是纵深防御的第二层,不是第一层。
- **凭据**:Secret Broker(ADR-012)把 scoped 凭据注入执行适配器,结果与
  receipt 一律脱敏;负向套件证明 handle 无法外泄。
- **数据**:Memory 删除经墓碑传播到 OpenSearch 投影(ADR-013),跨引擎一致。

## 4. 验收前置条件(实现 Firecracker 前必须满足)

- [ ] `runtime-provider-oci-gvisor.md` §4 加固清单在 Linux CI 逐条验证;
- [ ] Linux runner 可用 KVM,`/dev/kvm` 存在且 CI 模板允许嵌套虚拟化;
- [ ] 固定 Firecracker + jailer 版本矩阵并钉住 digest;
- [ ] microVM conformance 腿并入
      `TestSameAgentVersionRunsOnBothProviders`(第四腿),与
      wasmtime/oci 腿同构、同一断言;
- [ ] 快照(snapshot/restore)与逻辑 Checkpoint 的合并语义在 ADR-007
      修订中定稿后再动工。
