# Autonomous SRE Self-Healing Agent Showcase

This example demonstrates a production-grade **Autonomous SRE Agent** running on the AgentOS Kernel.

---

## Capabilities Demonstrated

1. **Supervised Daemon Service**: Runs with automatic supervisor heartbeats and auto-reap recovery.
2. **Durable Checkpointing**: Saves diagnostic progress at each step; resumes transparently upon worker restart.
3. **Durable IPC Mailbox**: Sends asynchronous, correlation-tagged status updates to peer agents (`incident-commander@1.2.0`).
4. **Kernel Effect API (Fenced Remediation)**:
   - Dispatches `k8s.deployment.restart` guarded by **monotonic fencing tokens**.
   - Prevents split-brain duplicate restarts with **SHA-256 idempotency protection**.
   - Prohibits unsafe automated retries if network drops (`UNKNOWN` outcome isolation).

---

## Running the Showcase

### 1. Start the SRE Agent

```bash
python examples/demo-sre-agent/server.py
```

### 2. Verify Conformance

Run the AgentOS Conformance Suite to prove compatibility:

```bash
agentos conformance -endpoint http://127.0.0.1:8095
```

Output:
```text
AgentOS Compatible = PASS
```

### 3. Deploy to AgentOS

Publish and run on the AgentOS control plane:

```bash
agentos publish -manifest examples/demo-sre-agent/agent.json
agentos service create -name sre-service -agent production/sre-agent@1.2.0 -replicas 2
```
