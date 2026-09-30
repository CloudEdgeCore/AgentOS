# AgentOS Remote HTTP Runtime Adapter

An official reference runtime adapter that proxies task execution, event streaming, and checkpoints to remote HTTP agent microservices over `agentos.runtime.interface/v1`.

## Features
- Connects existing microservices and webhooks to AgentOS
- Supports upstream forwarding with automatic JSON marshalling
- Checkpoint persistence and graceful failover
- Certified with AgentOS Conformance Suite

## Running & Conformance Testing

```bash
# 1. Start Remote HTTP Runtime on port 8086
go run examples/runtimes/remote-http-runtime/main.go --port 8086

# 2. Run AgentOS Conformance Certification
agentos conformance -endpoint http://127.0.0.1:8086
```

Certification result:
```text
AgentOS Compatible = PASS
```
