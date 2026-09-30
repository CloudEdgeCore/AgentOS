# AgentOS Docker Runtime Adapter

An official reference runtime adapter that runs Agent tasks inside isolated Docker containers while conforming to `agentos.runtime.interface/v1`.

## Features
- Container-level task isolation
- State checkpointing and volume snapshot restore
- Event streaming via Server-Sent Events (SSE) and HTTP polling
- Cooperative cancellation signaling

## Running & Conformance Testing

```bash
# 1. Start Docker Runtime on port 8089
go run examples/runtimes/docker-runtime/main.go --port 8089

# 2. Run AgentOS Conformance Certification
agentos conformance -endpoint http://127.0.0.1:8089
```

Certification result:
```text
AgentOS Compatible = PASS
```
