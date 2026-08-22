"""Agent OS Runtime Interface SDK for remote Python Agents."""

from .host import AgentRuntime, LEGACY_PROTOCOL_VERSION, PROTOCOL_VERSION, RuntimeHost, serve
from .mcp_client import MCPClient, MCPError, MCPToolError
from .realagent import RealAgent

__all__ = [
    "AgentRuntime", "RuntimeHost", "serve", "PROTOCOL_VERSION", "LEGACY_PROTOCOL_VERSION",
    "MCPClient", "MCPError", "MCPToolError", "RealAgent",
]
__version__ = "1.1.0"
