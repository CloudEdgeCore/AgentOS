# ADR-016: Tenant-Consistent Scheduling Sharding

日期:2026-08-16 · 状态:已接受(v0.8)

## 背景

v0.6 已证明多控制器实例可安全并存:claim 表以 (tenant_id, task_id,
controller_kind) 唯一、owner + fencing token 抢占、`SKIP LOCKED` 领取,
任何实例可领取任何任务,重复准入/丢事件由双实例回归测试锁定。但"任何
实例领取任何任务"意味着:

- 队列领取代价随任务数增长:每个实例的 `ClaimTasks` 都要扫描全量
  QUEUED/ADMITTED 集合(尽管 SKIP LOCKED 保证并发安全);
- 热点租户的决策不局部化:同一租户的任务可能被不同实例交错处理,审计/
  治理视角下"谁在处理我的租户"不可回答;
- 水平扩展没有明确的治理语义:加实例只靠 claim 竞争分担,无法按租户
  承诺隔离或限流。

v1.0 目标是"第三方 Agent 可稳定接入",租户级确定性是治理前提。

## 决策

**调度按租户确定性分片**:每个控制器实例声明 `(shard-index, shard-count)`,
只领取 `md5(tenant_id) % shard-count == shard-index` 的任务。分片键在
claim 时于 SQL 内计算,不新增列、不产生陈旧数据;`(0, 0)` 表示不分片
(向后兼容,现有部署不变)。

- **领取过滤**:`ClaimTasks` 增加可选 `ShardIndex/ShardCount`;分片时
  WHERE 追加 `('x' || substr(md5(t.tenant_id), 1, 8))::bit(32)::bigint %
  $count = $index`。md5 前缀 32 位转 bigint 恒为非负、跨 PG 版本稳定
  (对比 `hashtext` 无稳定保证),确定性可被测试断言。
- **语义不变**:claim 独占、fencing token、TTL 让渡、O6 退避、outbox、
  lease 全部不动——分片只收窄"哪个实例可以尝试领取",不改变领取后的
  任何事务语义。现有 `TestDualControllerInstancesAdmitEachTaskExactlyOnce`
  等回归继续全绿是验收底线。
- **准入与调度同构分片**:同一进程的 admission 与 scheduler 用同一
  `(index, count)`;部署不变量是**所有实例的分片配置必须一致**。
  配置不一致时任务会停留在 QUEUED/ADMITTED 无人领取(失败可见,不会
  静默错处理)。
- **重平衡安全**:实例数变化时,旧分片实例停止领取新租户任务;已持有
  claim 照常完成或按 TTL 让渡,新分片实例在 TTL 后接管——无双重处理
  窗口(claim 独占保证)。
- **hot tenant 边界**:单一热点租户仍收敛到一个分片。pool 维度分片
  (按 region/runtimeClass 派生键)留作增量,当前不引入 placement 派生
  列。
- **健康信号**:分片配置来自 `cmd/agentos-controller` 的
  `-shard-index/-shard-count` 参数,启动时校验 `0 <= index < count`;
  进程启动日志记录生效分片。

## 影响

- `store.ClaimTasksInput` 增加可选分片字段(零值 = 不分片);
- postgres `ClaimTasks` 在调度/准入两种 kind 上同样生效(两控制器共用
  该入口);
- `cmd/agentos-controller` 增加两个 flag 并透传给 admission/scheduler
  控制器;
- 新测试:分片过滤单测(每个租户恰好映射一个分片)、集成测试(两实例
  分片 0/1 各自只处理自己租户的任务,且与双实例回归同构的 exact-once
  断言)、多租户压力模型(见 v0.8 交付文档)。

## 备选

1. **Lease-based 动态分片**(shard 租约 + 接管):更强的故障转移,但引入
   第二套租约/接管机制,与 claim fencing 重叠,v0.8 不必要。
2. **任务 ID 哈希分片**:负载更均衡,但丢失租户局部性(治理目标不满足)。
3. **placement 派生列分片**:支持 pool 维度,但需要 admission 期写列、
   变更迁移,复杂度高于当前收益,留待池维度需求出现。
