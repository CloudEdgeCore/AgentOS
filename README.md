# AgentOS

AgentOS 是用于安全发布、调度、执行、恢复、治理和审计 AI Agent 的控制与运行平台。

> **当前版本：AgentOS 1.0.0.0（GA）**
>
> SemVer / Git 标签：[`v1.0.0`](https://github.com/CloudEdgeCore/AgentOS/releases/tag/v1.0.0)
>
> 稳定契约：Control API v1、Agent Manifest v1、Runtime / Gateway / Model Protocol v1、Runtime Interface v1

AgentOS 不是聊天 UI、工作流画布或托管式 SaaS。它解决的是 Agent 在生产环境中的后端系统问题：不可变版本、持久任务、准入策略、预算硬限制、运行时隔离、多租户身份、工具与模型网关、检查点、故障恢复和审计。

## 执行闭环

```mermaid
flowchart LR
    A["Agent Manifest / Signed Package"] --> B["Control API"]
    B --> C["PostgreSQL durable state"]
    C --> D["Admission / Policy / Budget"]
    D --> E["Scheduler / Recovery"]
    E --> F["Runtime Protocol v1"]
    F --> G["Wasmtime"]
    F --> H["OCI / gVisor"]
    F --> I["Reference / Adapter"]
    G --> J["Tool / Model / Memory Gateway"]
    H --> J
    I --> J
    J --> C
    C --> K["Checkpoint / Result / Audit"]
```

一次任务的典型生命周期是：

1. 开发者提交稳定版 Agent Manifest，可选附带签名 Package；
2. Control API 创建不可变 `AgentVersion` 和持久化 `Task`；
3. Admission 校验版本、能力、策略、租户配额和任务预算；
4. Scheduler 根据运行时类型、区域、容量和健康租约选择 Provider；
5. Worker 通过 Runtime Protocol 获取 fenced assignment；
6. Agent 在 Wasmtime、OCI/gVisor 或适配器运行时中执行；
7. 模型、工具、记忆和密钥请求通过 Gateway 进行授权、计量和审计；
8. Checkpoint、Result 和副作用回执持久化后，任务才进入终态；
9. Worker 失联、租约过期或进程重启时，Recovery Controller 负责收敛和重调度。

## v1.0 能力

| 领域 | 已实现能力 |
| --- | --- |
| Agent 生命周期 | 稳定 Manifest、不可变 AgentVersion、签名 Package、发布与版本查询 |
| 任务内核 | `Task → Run → Attempt` 状态机、取消、重试、超时、SSE 事件流和结果持久化 |
| 调度与恢复 | Admission、Rego 默认拒绝策略、容量感知调度、租约、fencing token、退避和孤儿恢复 |
| 运行时 | Wasmtime/Wasm Provider、OCI Provider、Linux gVisor 隔离、Reference Provider、HTTP Adapter Worker |
| Agent SDK | Go Runtime Interface SDK、Python 3.11+ Runtime Interface SDK、LangGraph 与 A2A 适配器 |
| Gateway | 工具、模型、记忆和能力网关；审批、幂等回执、预算结算和失败关闭 |
| 多租户 | tenant-scoped 存储、OIDC 主体、SPIFFE X.509-SVID、mTLS 身份与租户绑定 |
| 密钥与供应链 | OpenBao Secret Broker、动态数据库凭据、Package 签名、OCI digest pinning、SBOM |
| 数据与事件 | PostgreSQL/pgvector、NATS JetStream、transactional outbox/inbox、可选 OpenSearch 投影 |
| 可观测性 | OpenTelemetry、`/healthz`、`/readyz`、`/versionz`、哈希链审计和签名导出 |
| 质量门禁 | Race detector、真实 PostgreSQL/NATS 集成测试、双 Provider conformance、Go/Rust 漏洞审计 |

## 稳定契约与兼容策略

| 契约 | 稳定版本 | 源文件 |
| --- | --- | --- |
| Control REST API | `v1` | [`api/openapi/control-v1.yaml`](api/openapi/control-v1.yaml) |
| Agent Manifest | `agentos.dev/v1` | [`internal/kernel/agentversion/manifest.go`](internal/kernel/agentversion/manifest.go) |
| Runtime Protocol | `agentos.runtime.v1` | [`proto/agentos/runtime/v1/runtime.proto`](proto/agentos/runtime/v1/runtime.proto) |
| Runtime Interface | `agentos.runtime.interface/v1` | [`api/openapi/runtime-interface-v1.yaml`](api/openapi/runtime-interface-v1.yaml) |
| Gateway Protocol | `agentos.gateway.v1` | [`proto/agentos/gateway/v1/gateway.proto`](proto/agentos/gateway/v1/gateway.proto) |
| Model Protocol | `agentos.model.v1` | [`proto/agentos/model/v1/model.proto`](proto/agentos/model/v1/model.proto) |
| SLO contract | `agentos.slo/v1` | [`api/slo/v1.json`](api/slo/v1.json) |

`v1alpha1` 是 v1.0 的 N-1 兼容版本：旧 Manifest 可读取并确定性迁移，旧 gRPC service name 在 wire-compatible 范围内保留别名。兼容窗口不早于 **2027-02-17** 结束；稳定契约的破坏性变化必须发布新版本，未知字段继续失败关闭。机器可读策略位于 [`api/compatibility/v1alpha1-to-v1.json`](api/compatibility/v1alpha1-to-v1.json)。

迁移旧 Manifest：

```shell
go run ./cmd/agentos migrate \
  -manifest agent.v1alpha1.json \
  -out agent.v1.json
```

## 快速开始

### 环境要求

- Go `1.26.x`，CI 与正式发布使用 `1.26.6`；
- Python `3.11+`，仅在开发 Python/框架适配器时需要；
- Rust `1.97.1`，仅在构建 Wasmtime Provider 时需要；
- Docker 与 Docker Compose，用于本地 PostgreSQL、NATS 和可选基础设施；
- Linux、containerd 与 runsc，用于真实 OCI/gVisor Provider 验证。

检查源码版本：

```shell
go run ./cmd/agentos version -json
```

构建全部 Go 命令：

```shell
go build ./cmd/...
```

正式 Release 已提供 Linux AMD64/ARM64、macOS AMD64/ARM64、Windows AMD64 和 Python wheel，无需从源码构建。参见 [`v1.0.0` Release](https://github.com/CloudEdgeCore/AgentOS/releases/tag/v1.0.0)。

### 创建并认证一个 Python Agent Runtime

下面的流程只启动一个本地 Runtime Interface，并使用独立 conformance 工具验证公开协议，不需要启动控制面：

```shell
python -m pip install -e ./sdk/python
go run ./cmd/agentos init -dir ./tmp/hello-agent -name hello-agent -adapter python
python ./tmp/hello-agent/server.py
```

在另一个终端执行：

```shell
go run ./cmd/agentos-conformance -endpoint http://127.0.0.1:8088
```

成功时输出 `agentos.conformance/v1` JSON 报告，并且 `passed` 为 `true`。`agentos init` 也支持 `go`、`langgraph` 和 `a2a` 适配器。

### 启动本地控制面基础设施

```powershell
docker compose -f deploy/dev/compose.yaml up -d --wait postgres nats

$env:DATABASE_URL = "postgres://agentos:agentos-dev-only@127.0.0.1:55432/agentos?sslmode=disable"
go run ./cmd/agentos-migrate -database-url $env:DATABASE_URL
```

本地开发时，将以下进程分别运行在独立终端：

```powershell
# HTTP Control API
go run ./cmd/agentos-control `
  -database-url $env:DATABASE_URL `
  -dev-tenant dev

# Admission、Scheduler 与 Recovery
go run ./cmd/agentos-controller `
  -database-url $env:DATABASE_URL `
  -controller-id dev-controller `
  -runtime-pools deploy/dev/runtime-pools.json `
  -tenant-policies deploy/dev/tenant-policies.json `
  -dev-mode

# Transactional outbox → NATS JetStream
go run ./cmd/agentos-outbox `
  -database-url $env:DATABASE_URL `
  -nats-url nats://127.0.0.1:54222 `
  -dispatcher-id dev-outbox

# Worker Runtime Protocol
go run ./cmd/agentos-runtime-control `
  -database-url $env:DATABASE_URL `
  -listen 127.0.0.1:9090 `
  -dev-tenant dev `
  -dev-mode

# Tool / Model / Memory Gateway
go run ./cmd/agentos-gateway `
  -database-url $env:DATABASE_URL `
  -listen 127.0.0.1:9091 `
  -tenant-policies deploy/dev/tenant-policies.json `
  -tenant dev `
  -seed-dev-tools `
  -dev-mode

# 非沙箱 Reference Provider，仅用于开发和确定性测试
go run ./cmd/agentos-runtime-reference `
  -control-address 127.0.0.1:9090 `
  -gateway-address 127.0.0.1:9091 `
  -model-gateway-address 127.0.0.1:9091 `
  -mcp-listen 127.0.0.1:9092 `
  -tenant dev `
  -runtime-instance-id dev-worker-1 `
  -artifact-root tmp/artifacts `
  -dev-mode
```

这些命令使用固定开发租户、loopback 明文连接和开发执行器，只能用于本机开发。生产模式会拒绝这些降级配置。

### CLI 工作流

主 CLI 提供以下稳定工作流：

```text
agentos version   输出产品、提交和协议版本
agentos init      创建 Go/Python/LangGraph/A2A Agent 项目
agentos migrate   将旧 Manifest 提升到 v1
agentos validate  严格校验 Manifest
agentos package   生成带 provenance 的 Package Manifest
agentos sign      使用 Ed25519 密钥签名 Package
agentos publish   发布不可变 AgentVersion
agentos run       提交持久任务
agentos logs      读取任务 SSE 事件流
```

`publish`、`run` 和 `logs` 默认访问 `http://127.0.0.1:8080`。生产环境通过 `-endpoint` 指定 HTTPS Control API，并使用 `AGENTOS_TOKEN` 提供 Bearer Token。

## 生产安全基线

AgentOS 的生产模式是失败关闭的，缺少关键安全配置时进程会拒绝启动。

- **Control API**：必须使用 HTTPS、OIDC、生产 embedding endpoint、审计签名密钥和至少一个 Package trust key；
- **Runtime Protocol**：必须使用 SPIFFE X.509-SVID mTLS，Worker 身份与 tenant 绑定；
- **Gateway**：必须使用 mTLS，tenant 从对端 SVID 派生；工具必须映射到固定版本的 HTTPS endpoint；
- **Secret Broker**：通过 OpenBao 获取受控密钥或动态数据库凭据，不向 Agent 暴露原始平台凭据；
- **OCI Provider**：生产镜像必须 digest-pinned，Linux 隔离路径使用 containerd + gVisor/runsc；
- **Agent Package**：发布时校验签名、provenance、镜像签名和 CycloneDX SBOM；
- **审计**：使用事务内哈希链记录安全相关事件，并支持签名导出与完整性验证。

开发模式只允许 loopback 或显式 `-dev-mode`，不得暴露到非可信网络。

## 可观测性与可选服务

启用 OpenTelemetry 参考栈：

```powershell
docker compose -f deploy/dev/compose.yaml --profile observability up -d
$env:OTEL_EXPORTER_OTLP_ENDPOINT = "127.0.0.1:4317"
```

- Grafana：`http://127.0.0.1:3300`
- Prometheus：`http://127.0.0.1:9093`
- Tempo：`http://127.0.0.1:3320`
- Loki：`http://127.0.0.1:3310`

其他可选开发服务：

```powershell
docker compose -f deploy/dev/compose.yaml --profile secrets up -d  # OpenBao
docker compose -f deploy/dev/compose.yaml --profile search up -d   # OpenSearch
```

Control API 运维端点：

- `GET /healthz`：进程存活；
- `GET /readyz`：PostgreSQL 等必需依赖就绪；
- `GET /versionz`：产品、构建提交和全部稳定协议版本。

## 测试与质量门禁

单元测试、格式、静态检查和 Go 漏洞扫描：

```shell
gofmt -l .
go vet ./...
go test -race -count=1 ./...
go tool govulncheck ./...
```

真实 PostgreSQL/NATS 集成测试：

```powershell
docker compose -f deploy/dev/compose.yaml up -d --wait postgres nats
$env:AGENTOS_TEST_DATABASE_URL = "postgres://agentos:agentos-dev-only@127.0.0.1:55432/agentos?sslmode=disable"
$env:AGENTOS_TEST_NATS_URL = "nats://127.0.0.1:54222"
go test -race -tags=integration -count=1 ./...
```

> 集成测试会应用迁移并清理 AgentOS 测试表。不要将 `AGENTOS_TEST_DATABASE_URL` 指向包含持久业务数据的数据库。

Protobuf 兼容性和确定性生成：

```shell
buf lint
buf generate
git diff --exit-code -- gen/go
```

Wasmtime Provider：

```shell
cargo +1.97.1 fmt --all -- --check
cargo +1.97.1 clippy --workspace --all-targets --locked -- -D warnings
cargo +1.97.1 test --workspace --locked
```

Linux OCI/gVisor 真实隔离测试由 CI 的 `runtime-linux-leg` 执行；工具链和验收项位于 [`deploy/ci/runtime-matrix.md`](deploy/ci/runtime-matrix.md)。

SLO 样本评估：

```shell
go run ./cmd/agentos-slo -sample measured-slo.json
```

## Release 与供应链验证

正式发布工作流会执行 GA gates，并生成：

- Linux AMD64/ARM64、macOS AMD64/ARM64、Windows AMD64 完整命令归档；
- `agentos-runtime` Python wheel；
- CycloneDX 1.6 SBOM；
- `checksums.txt`；
- `checksums.txt.sigstore.json` 无密钥 Sigstore bundle。

下载并验证全部 Release 资产：

```shell
gh release download v1.0.0 --repo CloudEdgeCore/AgentOS
sha256sum -c checksums.txt
```

发布流程定义在 [`.github/workflows/release.yml`](.github/workflows/release.yml)，Action 依赖固定到精确提交 SHA。

## 仓库结构

| 路径 | 内容 |
| --- | --- |
| `cmd/` | CLI、控制面、Gateway、Runtime Provider 和运维工具入口 |
| `internal/kernel/` | Task/Run/Attempt、Admission、Scheduler、Policy、Budget、Recovery |
| `internal/runtime/` | Runtime Control、Reference、Adapter 和 OCI Provider |
| `internal/gateway/` | Tool、Model、Memory、Capability 和 Secret Broker |
| `sdk/agent/` | Go Runtime Interface SDK |
| `sdk/python/` | Python Runtime Interface SDK |
| `adapters/` | LangGraph 与 A2A 适配器 |
| `api/openapi/` | 稳定与兼容 REST/HTTP 契约 |
| `proto/agentos/` | Runtime、Gateway 和 Model Protobuf 契约 |
| `db/migrations/` | PostgreSQL 迁移 |
| `deploy/dev/` | 本地依赖与可观测性参考环境 |
| `deploy/ci/` | OCI/gVisor 隔离与环境指纹测试 |
| `modelcheck/tla/` | 内核状态机 TLA+ 模型 |

## 当前边界

- v1.0 是 Agent 控制与执行后端，不包含完整 Web 管理控制台或托管云服务；
- Reference Provider 是确定性开发实现，不是安全沙箱；生产执行应使用 Wasmtime 或 OCI/gVisor；
- Firecracker 当前仅有 CI KVM 环境探针，不应视为已交付的 MicroVM Provider；
- TypeScript 目录仍是后续客户端 SDK 的预留位置，不属于 v1.0 稳定 SDK；
- 生产部署需要自行提供 PostgreSQL、NATS、OIDC、SPIFFE/SPIRE、OpenBao，以及实际模型、工具和 embedding 服务。

以上边界是有意保留的：AgentOS v1.0 的交付目标是一个可验证、可恢复、默认拒绝且具有稳定公开契约的 Agent 运行内核。
