# AgentOS Conformance Certification Kit

The AgentOS Conformance Suite is the authoritative, black-box verification kit certifying that an agent runtime or framework adapter complies with the **AgentOS Runtime Interface v1** specification.

---

## 1. Overview

Any framework adapter (LangGraph, AutoGen, CrewAI, OpenAI Agents SDK, custom proprietary runtime) that passes this suite provides guaranteed compatibility with the AgentOS Kernel, including:
- **Durable Lifecycle**: Idempotent execution startup, non-preemptible execution, cancellation signaling.
- **Durable Checkpointing & Recovery**: State capture and resumption across worker failures.
- **Audit & Observability**: Real-time event streaming and cursor pagination.
- **Fail-Closed Security**: Default-deny capability enforcement and unauthorized resource rejection.

---

## 2. Running Certification

### 2.1 Single-Command Verification (Live Endpoint)

If your adapter server is already running (e.g. on port `8088`):

```bash
agentos-conformance -endpoint http://127.0.0.1:8088
```

Or via the unified CLI:

```bash
agentos conformance -endpoint http://127.0.0.1:8088
```

### 2.2 Auto-Spawning Candidate Servers (`-cmd`)

You can provide the command to start your adapter. `agentos-conformance` will launch the server, discover the listening port, run all checks, and cleanly shut down the server:

```bash
agentos-conformance -cmd "python examples/agents/langgraph/server.py --port 0"
```

### 2.3 Machine-Readable JSON Export (`-json`)

```bash
agentos-conformance -endpoint http://127.0.0.1:8088 -json
```

---

## 3. Certification Report Format

A successful conformance run outputs:

```text
=== AgentOS Runtime Interface Conformance Suite ===
Adapter:  langgraph
Protocol: agentos.runtime.interface/v1
Endpoint: http://127.0.0.1:8089

Checks executed:
  [PASS] health
  [PASS] protocol-negotiation
  [PASS] start
  [PASS] idempotency
  [PASS] conflict
  [PASS] event
  [PASS] event-cursor
  [PASS] result
  [PASS] checkpoint
  [PASS] restore
  [PASS] stop
  [PASS] default-deny-capabilities
--------------------------------------------------
AgentOS Compatible = PASS
--------------------------------------------------
```

---

## 4. Supported Framework Ecosystem

The repository provides verified adapters and reference servers for:

1. **LangGraph**: `adapters/langgraph/agentos_langgraph.py` & `examples/agents/langgraph/server.py`
2. **AutoGen**: `adapters/autogen/agentos_autogen.py` & `examples/agents/autogen/server.py`
3. **CrewAI**: `adapters/crewai/agentos_crewai.py` & `examples/agents/crewai/server.py`
4. **OpenAI Agents SDK**: `adapters/openai_agents/agentos_openai_agents.py` & `examples/agents/openai_agents/server.py`
5. **Custom Enterprise Agent**: `adapters/custom_agent/agentos_custom.py` & `examples/agents/custom/server.py`
