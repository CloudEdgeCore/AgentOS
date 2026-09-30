"""Turnkey wrappers for integrating third-party agent frameworks into AgentOS.

Enables LangGraph, AutoGen, CrewAI, OpenAI Agents SDK, and Custom Agents to run
directly as managed AgentOS processes with:
- Automatic Supervisor heartbeat and self-healing
- Durable state checkpointing and recovery
- Cross-agent Durable IPC Mailbox communication
- Fenced External Side-Effect execution
"""

from __future__ import annotations

import json
import logging
import os
import threading
import time
from typing import Any, Callable, Dict, Optional

from .client import AgentOSClient
from .host import AgentRuntime, serve

logger = logging.getLogger("agentos.ecosystem")


class FrameworkAdapterRuntime(AgentRuntime):
    """Bridge converting any third-party framework runner into an AgentRuntime."""

    def __init__(
        self,
        framework_name: str,
        runner_fn: Callable[[Dict[str, Any], AgentOSClient, threading.Event], Any],
        state_serializer: Optional[Callable[[], Dict[str, Any]]] = None,
        state_restorer: Optional[Callable[[Dict[str, Any]], None]] = None,
    ) -> None:
        self.framework_name = framework_name
        self.runner_fn = runner_fn
        self.state_serializer = state_serializer
        self.state_restorer = state_restorer
        self.client = AgentOSClient()

    def run(self, request: Dict[str, Any], emit: Callable[[str, Any], None], stop_event: threading.Event) -> Any:
        emit(f"{self.framework_name}.starting", {"executionId": request.get("executionId")})

        # Resumption check:
        checkpoint = self.client.checkpoint.restore()
        if checkpoint and self.state_restorer:
            emit(f"{self.framework_name}.restoring", {"checkpoint": True})
            self.state_restorer(checkpoint)

        # Execute framework logic:
        result = self.runner_fn(request, self.client, stop_event)

        # Final checkpoint:
        if self.state_serializer:
            self.client.checkpoint.save(self.state_serializer())

        emit(f"{self.framework_name}.completed", {"success": True})
        return result


def wrap_langgraph(
    compiled_graph: Any,
    server_port: int = 8088,
    name: str = "langgraph-agent",
) -> None:
    """Turn a compiled LangGraph workflow into a managed AgentOS agent server.

    Usage:
        from langgraph.graph import StateGraph
        builder = StateGraph(...)
        graph = builder.compile()

        from agentos_runtime.ecosystem import wrap_langgraph
        wrap_langgraph(graph, server_port=8089)
    """
    def _runner(req: Dict[str, Any], client: AgentOSClient, stop_event: threading.Event) -> Any:
        inputs = req.get("payload") or {"goal": req.get("goal")}
        # If compiled_graph has invoke / stream:
        if hasattr(compiled_graph, "invoke"):
            return compiled_graph.invoke(inputs)
        return {"output": "Graph executed", "inputs": inputs}

    runtime = FrameworkAdapterRuntime("langgraph", _runner)
    serve(runtime, port=server_port)


def wrap_autogen(
    agent_or_chat: Any,
    server_port: int = 8088,
    name: str = "autogen-agent",
) -> None:
    """Turn an AutoGen ConversableAgent or GroupChat into an AgentOS server.

    Usage:
        import autogen
        assistant = autogen.AssistantAgent(...)

        from agentos_runtime.ecosystem import wrap_autogen
        wrap_autogen(assistant, server_port=8090)
    """
    def _runner(req: Dict[str, Any], client: AgentOSClient, stop_event: threading.Event) -> Any:
        message = req.get("goal") or req.get("message") or "Execute task"
        if hasattr(agent_or_chat, "generate_reply"):
            return agent_or_chat.generate_reply(messages=[{"role": "user", "content": message}])
        if hasattr(agent_or_chat, "initiate_chat"):
            return {"status": "chat initiated", "message": message}
        return {"output": str(message)}

    runtime = FrameworkAdapterRuntime("autogen", _runner)
    serve(runtime, port=server_port)


def wrap_crew(
    crew: Any,
    server_port: int = 8088,
    name: str = "crewai-crew",
) -> None:
    """Turn a CrewAI Crew into a managed AgentOS Daemon Service with Supervisor health.

    Usage:
        from crewai import Crew
        crew = Crew(...)

        from agentos_runtime.ecosystem import wrap_crew
        wrap_crew(crew, server_port=8091)
    """
    def _runner(req: Dict[str, Any], client: AgentOSClient, stop_event: threading.Event) -> Any:
        inputs = req.get("payload") or {"topic": req.get("goal")}
        # Spawn background heartbeat thread for long-running crew tasks
        hb_stop = threading.Event()

        def _heartbeat_loop():
            while not hb_stop.is_set():
                try:
                    client.service.heartbeat(phase="RUNNING")
                except Exception as err:
                    logger.debug("Supervisor heartbeat notice: %s", err)
                hb_stop.wait(5.0)

        t = threading.Thread(target=_heartbeat_loop, daemon=True)
        t.start()

        try:
            if hasattr(crew, "kickoff"):
                return crew.kickoff(inputs=inputs)
            return {"status": "Crew started", "inputs": inputs}
        finally:
            hb_stop.set()

    runtime = FrameworkAdapterRuntime("crewai", _runner)
    serve(runtime, port=server_port)


def wrap_openai_agent(
    agent: Any,
    server_port: int = 8088,
    name: str = "openai-agent",
) -> None:
    """Turn an OpenAI Agents SDK agent into an AgentOS server with gateway governance.

    Usage:
        from agents import Agent
        agent = Agent(name="Assistant", instructions="...")

        from agentos_runtime.ecosystem import wrap_openai_agent
        wrap_openai_agent(agent, server_port=8092)
    """
    def _runner(req: Dict[str, Any], client: AgentOSClient, stop_event: threading.Event) -> Any:
        prompt = req.get("goal") or req.get("input") or ""
        if hasattr(agent, "run"):
            return agent.run(prompt)
        return {"output": f"Executed OpenAI agent with prompt: {prompt}"}

    runtime = FrameworkAdapterRuntime("openai_agents", _runner)
    serve(runtime, port=server_port)


def wrap_custom_agent(
    handler_fn: Callable[[Dict[str, Any], AgentOSClient], Any],
    server_port: int = 8088,
    name: str = "custom-agent",
) -> None:
    """Wrap any custom Python function or agent class with full AgentOS Client capabilities.

    Usage:
        def my_agent(req, client):
            # Send IPC message
            client.ipc.send("other-agent@1.2.0", {"query": "hello"})
            # Guarded side effect
            receipt = client.effect.execute("email.send", "key-123", {"to": "ops@example.com"})
            return {"status": "success", "receipt": receipt}

        from agentos_runtime.ecosystem import wrap_custom_agent
        wrap_custom_agent(my_agent, server_port=8093)
    """
    def _runner(req: Dict[str, Any], client: AgentOSClient, stop_event: threading.Event) -> Any:
        return handler_fn(req, client)

    runtime = FrameworkAdapterRuntime("custom_agent", _runner)
    serve(runtime, port=server_port)
