"""Standard-library host for agentos.runtime.interface/v1alpha1.

Framework adapters implement AgentRuntime; this module owns transport,
idempotency, bounded events, cancellation, checkpoint/restore and results.
No framework or model provider is imported by the protocol layer.
"""

from __future__ import annotations

import hashlib
import json
import threading
import time
from dataclasses import dataclass, field
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any, Callable, Protocol
from urllib.parse import parse_qs, urlparse

PROTOCOL_VERSION = "agentos.runtime.interface/v1alpha1"
MAX_BODY = 2 << 20


class AgentRuntime(Protocol):
    def run(
        self,
        request: dict[str, Any],
        emit: Callable[[str, Any], None],
        stop_event: threading.Event,
    ) -> Any: ...

    def checkpoint(self, execution_id: str) -> dict[str, Any]: ...

    def restore(self, execution_id: str, checkpoint: dict[str, Any]) -> None: ...


@dataclass
class _Execution:
    digest: bytes
    created_at: float = field(default_factory=time.monotonic)
    stop_event: threading.Event = field(default_factory=threading.Event)
    status: str = "ACCEPTED"
    events: list[dict[str, Any]] = field(default_factory=list)
    next_sequence: int = 1
    result: dict[str, Any] | None = None


class RuntimeHost:
    """Thread-safe black-box host shared by Python and framework adapters."""

    def __init__(
        self,
        runtime: AgentRuntime,
        adapter: str,
        *,
        max_concurrent: int = 16,
        event_limit: int = 1024,
        execution_limit: int = 4096,
    ) -> None:
        if not adapter.strip():
            raise ValueError("adapter identity is required")
        if max_concurrent <= 0 or event_limit <= 0 or execution_limit < max_concurrent:
            raise ValueError("host limits must be positive")
        self.runtime = runtime
        self.adapter = adapter
        self.max_concurrent = max_concurrent
        self.event_limit = event_limit
        self.execution_limit = execution_limit
        self.lock = threading.RLock()
        self.executions: dict[str, _Execution] = {}
        self.active = 0

    def handler(self) -> type[BaseHTTPRequestHandler]:
        host = self

        class Handler(BaseHTTPRequestHandler):
            server_version = "AgentOSRuntime/0.9"

            def do_GET(self) -> None:  # noqa: N802
                host._handle(self, "GET")

            def do_POST(self) -> None:  # noqa: N802
                host._handle(self, "POST")

            def log_message(self, _format: str, *_args: Any) -> None:
                return

        return Handler

    def _handle(self, handler: BaseHTTPRequestHandler, method: str) -> None:
        parsed = urlparse(handler.path)
        if method == "GET" and parsed.path == "/v1alpha1/health":
            with self.lock:
                active = self.active
            self._write(
                handler,
                200,
                {
                    "status": "SERVING",
                    "protocolVersions": [PROTOCOL_VERSION],
                    "adapter": self.adapter,
                    "maxConcurrent": self.max_concurrent,
                    "activeExecutions": active,
                },
            )
            return
        if method == "POST" and parsed.path == "/v1alpha1/executions:start":
            self._start(handler)
            return
        prefix = "/v1alpha1/executions/"
        if not parsed.path.startswith(prefix):
            self._problem(handler, 404, "ROUTE_NOT_FOUND", "runtime interface route not found")
            return
        remainder = parsed.path[len(prefix) :]
        actions = {
            ":stop": "stop",
            ":checkpoint": "checkpoint",
            ":restore": "restore",
            "/events": "events",
            "/result": "result",
        }
        for suffix, action in actions.items():
            if remainder.endswith(suffix):
                execution_id = remainder[: -len(suffix)]
                if not execution_id or "/" in execution_id:
                    self._problem(handler, 400, "INVALID_EXECUTION_ID", "execution id is invalid")
                    return
                getattr(self, f"_{action}")(handler, execution_id, method, parsed.query)
                return
        self._problem(handler, 404, "ROUTE_NOT_FOUND", "runtime execution route not found")

    def _start(self, handler: BaseHTTPRequestHandler) -> None:
        body = self._read_json(handler)
        if body is None:
            return
        try:
            self._validate_start(body)
        except ValueError as error:
            self._problem(handler, 422, "INVALID_START", str(error))
            return
        canonical = json.dumps(body, separators=(",", ":"), sort_keys=True).encode()
        digest = hashlib.sha256(canonical).digest()
        execution_id = body["executionId"]
        with self.lock:
            existing = self.executions.get(execution_id)
            if existing is not None:
                if existing.digest != digest:
                    self._problem(handler, 409, "EXECUTION_CONFLICT", "execution id conflicts with another request")
                    return
                self._write(
                    handler,
                    200,
                    {"executionId": execution_id, "status": existing.status, "replayed": True},
                )
                return
            if self.active >= self.max_concurrent:
                self._problem(handler, 429, "CAPACITY_EXHAUSTED", "agent runtime capacity exhausted")
                return
            if len(self.executions) >= self.execution_limit:
                terminal = [
                    (key, value)
                    for key, value in self.executions.items()
                    if value.status not in {"ACCEPTED", "RUNNING"}
                ]
                if not terminal:
                    self._problem(handler, 429, "RETENTION_EXHAUSTED", "execution retention capacity exhausted")
                    return
                oldest, _ = min(terminal, key=lambda item: item[1].created_at)
                del self.executions[oldest]
            execution = _Execution(digest=digest)
            self.executions[execution_id] = execution
            self.active += 1
        threading.Thread(target=self._run, args=(body, execution), daemon=True).start()
        self._write(handler, 202, {"executionId": execution_id, "status": "ACCEPTED", "replayed": False})

    def _run(self, request: dict[str, Any], execution: _Execution) -> None:
        execution_id = request["executionId"]
        with self.lock:
            execution.status = "RUNNING"

        def emit(event_type: str, payload: Any) -> None:
            if not isinstance(event_type, str) or not event_type.strip() or len(event_type) > 128:
                raise ValueError("event type is required and bounded")
            encoded = json.dumps(payload, separators=(",", ":"))
            if len(encoded.encode()) > MAX_BODY:
                raise ValueError("event payload exceeds 2 MiB")
            with self.lock:
                if execution.status != "RUNNING":
                    raise RuntimeError("events cannot be emitted after execution termination")
                if len(execution.events) >= self.event_limit:
                    raise RuntimeError("event history limit exceeded")
                execution.events.append(
                    {
                        "sequence": execution.next_sequence,
                        "type": event_type,
                        "payload": payload,
                        "occurredAt": _timestamp(),
                    }
                )
                execution.next_sequence += 1

        try:
            output = self.runtime.run(request, emit, execution.stop_event)
            if execution.stop_event.is_set():
                result = self._terminal(execution_id, "CANCELLED", error_code="EXECUTION_CANCELLED", error="execution cancelled")
            else:
                json.dumps(output)
                result = self._terminal(execution_id, "SUCCEEDED", output=output)
        except Exception as error:  # fail closed at the adapter boundary
            status = "CANCELLED" if execution.stop_event.is_set() else "FAILED"
            code = "EXECUTION_CANCELLED" if status == "CANCELLED" else "ADAPTER_FAILED"
            result = self._terminal(execution_id, status, error_code=code, error=str(error))
        with self.lock:
            execution.status = result["status"]
            execution.result = result
            self.active -= 1

    def _stop(self, handler: BaseHTTPRequestHandler, execution_id: str, method: str, _query: str) -> None:
        if method != "POST":
            self._problem(handler, 405, "METHOD_NOT_ALLOWED", "POST is required")
            return
        with self.lock:
            execution = self.executions.get(execution_id)
            if execution is None:
                self._problem(handler, 404, "EXECUTION_NOT_FOUND", "agent execution not found")
                return
            execution.stop_event.set()
            status = execution.status
        self._write(handler, 202, {"executionId": execution_id, "status": status})

    def _checkpoint(self, handler: BaseHTTPRequestHandler, execution_id: str, method: str, _query: str) -> None:
        if method != "POST":
            self._problem(handler, 405, "METHOD_NOT_ALLOWED", "POST is required")
            return
        with self.lock:
            exists = execution_id in self.executions
        if not exists:
            self._problem(handler, 404, "EXECUTION_NOT_FOUND", "agent execution not found")
            return
        try:
            checkpoint = self.runtime.checkpoint(execution_id)
            self._validate_checkpoint(checkpoint)
        except Exception as error:
            self._problem(handler, 422, "CHECKPOINT_FAILED", str(error))
            return
        self._write(handler, 200, {"executionId": execution_id, "checkpoint": checkpoint})

    def _restore(self, handler: BaseHTTPRequestHandler, execution_id: str, method: str, _query: str) -> None:
        if method != "POST":
            self._problem(handler, 405, "METHOD_NOT_ALLOWED", "POST is required")
            return
        body = self._read_json(handler)
        if body is None:
            return
        if set(body) != {"executionId", "checkpoint"} or body["executionId"] != execution_id:
            self._problem(handler, 422, "RESTORE_ID_MISMATCH", "path and body execution ids must match")
            return
        try:
            self._validate_checkpoint(body["checkpoint"])
            self.runtime.restore(execution_id, body["checkpoint"])
        except Exception as error:
            self._problem(handler, 422, "RESTORE_FAILED", str(error))
            return
        self._write(handler, 200, {"executionId": execution_id, "restored": True})

    def _events(self, handler: BaseHTTPRequestHandler, execution_id: str, method: str, query: str) -> None:
        if method != "GET":
            self._problem(handler, 405, "METHOD_NOT_ALLOWED", "GET is required")
            return
        try:
            after = int(parse_qs(query).get("after", ["0"])[0])
            if after < 0:
                raise ValueError
        except ValueError:
            self._problem(handler, 400, "INVALID_CURSOR", "after must be a non-negative integer")
            return
        with self.lock:
            execution = self.executions.get(execution_id)
            if execution is None:
                self._problem(handler, 404, "EXECUTION_NOT_FOUND", "agent execution not found")
                return
            events = [event for event in execution.events if event["sequence"] > after]
        next_after = events[-1]["sequence"] if events else after
        self._write(handler, 200, {"executionId": execution_id, "events": events, "nextAfter": next_after})

    def _result(self, handler: BaseHTTPRequestHandler, execution_id: str, method: str, _query: str) -> None:
        if method != "GET":
            self._problem(handler, 405, "METHOD_NOT_ALLOWED", "GET is required")
            return
        with self.lock:
            execution = self.executions.get(execution_id)
            if execution is None:
                self._problem(handler, 404, "EXECUTION_NOT_FOUND", "agent execution not found")
                return
            result = execution.result
            status = execution.status
        if result is None:
            self._write(handler, 202, {"executionId": execution_id, "status": status})
            return
        self._write(handler, 200, result)

    @staticmethod
    def _validate_start(body: dict[str, Any]) -> None:
        required = {"executionId", "agentVersionRef", "goal", "input", "capabilities"}
        if set(body) != required:
            raise ValueError("start request contains missing or unknown fields")
        for key in ("executionId", "agentVersionRef", "goal"):
            if not isinstance(body[key], str) or not body[key].strip():
                raise ValueError(f"{key} is required")
        capabilities = body["capabilities"]
        classes = {"tools", "models", "memory", "secrets"}
        if not isinstance(capabilities, dict) or set(capabilities) != classes:
            raise ValueError("all capability classes must be explicit")
        if any(not isinstance(capabilities[name], list) for name in classes):
            raise ValueError("capability classes must be arrays")

    @staticmethod
    def _validate_checkpoint(checkpoint: dict[str, Any]) -> None:
        if not isinstance(checkpoint, dict) or set(checkpoint) != {"schemaVersion", "state", "createdAt"}:
            raise ValueError("checkpoint shape is invalid")
        if not isinstance(checkpoint["schemaVersion"], str) or not checkpoint["schemaVersion"].strip():
            raise ValueError("checkpoint schemaVersion is required")
        if not isinstance(checkpoint["createdAt"], str) or not checkpoint["createdAt"].strip():
            raise ValueError("checkpoint createdAt is required")
        if len(json.dumps(checkpoint["state"]).encode()) > MAX_BODY:
            raise ValueError("checkpoint state exceeds 2 MiB")

    @staticmethod
    def _terminal(
        execution_id: str,
        status: str,
        *,
        output: Any | None = None,
        error_code: str = "",
        error: str = "",
    ) -> dict[str, Any]:
        result: dict[str, Any] = {"executionId": execution_id, "status": status, "completedAt": _timestamp()}
        if output is not None:
            result["output"] = output
        if error_code:
            result["errorCode"] = error_code
        if error:
            result["error"] = error
        return result

    @staticmethod
    def _read_json(handler: BaseHTTPRequestHandler) -> dict[str, Any] | None:
        try:
            length = int(handler.headers.get("Content-Length", "0"))
            if length <= 0 or length > MAX_BODY:
                raise ValueError("request body size is invalid")
            body = json.loads(handler.rfile.read(length), object_pairs_hook=_unique_object)
            if not isinstance(body, dict):
                raise ValueError("request body must be an object")
            return body
        except (ValueError, json.JSONDecodeError) as error:
            RuntimeHost._problem(handler, 400, "INVALID_JSON", str(error))
            return None

    @staticmethod
    def _write(handler: BaseHTTPRequestHandler, status: int, body: dict[str, Any]) -> None:
        encoded = json.dumps(body, separators=(",", ":")).encode()
        handler.send_response(status)
        handler.send_header("Content-Type", "application/json")
        handler.send_header("Content-Length", str(len(encoded)))
        handler.send_header("AgentOS-Runtime-Interface", PROTOCOL_VERSION)
        handler.end_headers()
        handler.wfile.write(encoded)

    @staticmethod
    def _problem(handler: BaseHTTPRequestHandler, status: int, code: str, detail: str) -> None:
        RuntimeHost._write(handler, status, {"code": code, "detail": detail, "status": status})


def serve(
    runtime: AgentRuntime,
    adapter: str,
    host: str = "127.0.0.1",
    port: int = 8088,
) -> ThreadingHTTPServer:
    runtime_host = RuntimeHost(runtime, adapter)
    return ThreadingHTTPServer((host, port), runtime_host.handler())


def _timestamp() -> str:
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())


def _unique_object(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise ValueError(f"duplicate JSON field {key!r}")
        result[key] = value
    return result
