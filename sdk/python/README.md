# AgentOS Runtime SDK for Python

`agentos-runtime` is the supported Python implementation of the AgentOS
Runtime Interface. SDK `1.x` implements `runtime.interface/v1`; the deprecated
`v1alpha1` route remains available only for one-version migration testing.

The package provides:

- a bounded, concurrent `RuntimeHost` HTTP server;
- execution start, stop, events, result, checkpoint, and restore operations;
- an execution-scoped MCP client for brokered model, tool, and memory access;
- `RealAgent`, a governed model/tool/memory reference loop;
- PEP 561 typing metadata (`py.typed`).

Python agents run outside the AgentOS kernel trust boundary. Provider secrets
are never passed to the agent process; model and tool calls use the injected
loopback MCP endpoint and the execution identity supplied by AgentOS.

## Install

Python 3.11 or later is required.

```bash
python -m pip install agentos-runtime
```

For a checkout of this repository:

```bash
python -m pip install -e sdk/python
```

## Minimal adapter

```python
import threading
from agentos_runtime import serve


class Runtime:
    def run(self, request, emit, stop_event: threading.Event):
        emit("agent.started", {"executionId": request["executionId"]})
        if stop_event.is_set():
            raise RuntimeError("execution cancelled")
        return {"answer": request["goal"]}

    def checkpoint(self, execution_id):
        return {"schemaVersion": "example/v1", "state": {}}

    def restore(self, execution_id, checkpoint):
        return None


serve(Runtime(), "python-remote", max_concurrent=16).serve_forever()
```

Bind the development server to loopback. In production, the Runtime Adapter
connects to it locally while AgentOS authenticates cross-process control links
with SPIFFE/mTLS.

## Brokered MCP access

The runtime start request contains the MCP endpoint. Bind every client to the
current execution so concurrent attempts cannot share authority:

```python
from agentos_runtime import MCPClient

mcp = MCPClient(request["mcpEndpoint"], execution_id=request["executionId"])
mcp.initialize()
result = mcp.call_tool("agentos.model.invoke", {
    "modelRef": "deepseek-v4-flash",
    "messages": [{"role": "user", "content": request["goal"]}],
})
```

## Operational limits

`RuntimeHost` enforces bounded worker and control-operation concurrency,
execution and checkpoint timeouts, a 256 KiB event limit, a 1 MiB event-page
limit, and a 2 MiB result limit. Exceptions returned over the protocol are
redacted to stable public error messages.

## Verify an adapter

After starting it locally:

```bash
agentos conformance -endpoint http://127.0.0.1:8088
```

Run the Python unit suite with:

```bash
python -m unittest discover -s sdk/python/tests -v
```

This SDK is an agent-side runtime component, not a trusted control-plane
plugin. Its semantic version is independent of the AgentOS product version;
the supported Runtime Interface major version is the compatibility contract.
