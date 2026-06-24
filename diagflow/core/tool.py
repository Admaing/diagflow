"""
Tool abstraction — the primitive that bridges LLM decisions to external actions.

Each Tool wraps a callable with a JSON Schema describing its parameters,
allowing the LLM to invoke it through function calling. Tools are registered
in a central Registry and dispatched by name during ReAct cycles.

Design decisions:
  - Timeout: every tool runs under a configurable timeout; a timeout yields
    a structured TimeoutResult rather than raising, so the LLM can adapt.
  - Schema: uses JSON Schema (not a custom DSL) so the same schema feeds
    both LLM function-calling definitions and optional client-side validation.
"""

from __future__ import annotations

import asyncio
import time
from dataclasses import dataclass, field
from typing import Any, Callable, Coroutine


# ---------------------------------------------------------------------------
# Result types
# ---------------------------------------------------------------------------

@dataclass
class ToolResult:
    """The structured output of a tool invocation."""
    success: bool
    data: Any = None
    error: str | None = None
    duration_ms: float = 0.0
    is_timeout: bool = False   # renamed from 'timed_out' to avoid shadowing classmethod

    @classmethod
    def ok(cls, data: Any, duration_ms: float = 0) -> "ToolResult":
        return cls(success=True, data=data, duration_ms=duration_ms)

    @classmethod
    def failed(cls, error: str, duration_ms: float = 0) -> "ToolResult":
        return cls(success=False, error=error, duration_ms=duration_ms)

    @classmethod
    def timeout(cls, name: str, timeout_s: int) -> "ToolResult":
        return cls(
            success=False,
            error=f"Tool '{name}' timed out after {timeout_s}s",
            is_timeout=True,
            duration_ms=timeout_s * 1000,
        )

    def to_llm_message(self) -> str:
        """Format the result so the LLM can reason about it."""
        if self.is_timeout:
            return f"[Tool timed out: {self.error}]"
        if not self.success:
            return f"[Tool error: {self.error}]"
        if isinstance(self.data, str):
            return self.data
        import json
        return json.dumps(self.data, ensure_ascii=False, default=str)

    @staticmethod
    def compute_confidence(tool_name: str, data: str, success: bool) -> float:
        """Score evidence quality based on what the tool actually found."""
        if not success:
            return 0.05

        if tool_name in ("query_node_log", "call_umr_agent"):
            lines = data.strip().split("\n")
            err = sum(1 for l in lines if "ERROR" in l.upper())
            warn = sum(1 for l in lines if "WARN" in l.upper())
            if err >= 5 or "OOM" in data or "FATAL" in data:
                return 0.92
            if err >= 1:
                return 0.78
            if warn >= 3:
                return 0.60
            return 0.30

        if tool_name == "query_metrics":
            score = 0.40
            for line in data.split("\n"):
                if "=" not in line:
                    continue
                k, v_str = line.split("=", 1)
                kv_lower = line.lower()
                # Check value: non-numeric alert signals
                if "error" in kv_lower or "high" in kv_lower or "fail" in kv_lower:
                    score += 0.15
                try:
                    v = float(v_str)
                    if k.endswith("_pct") and v > 90:
                        score += 0.12
                    if ("_rate" in k or "failure" in k) and v > 3:
                        score += 0.10
                    if ("gc" in k.lower() or "pause" in k.lower()) and v > 500:
                        score += 0.10
                except ValueError:
                    pass
            return min(round(score, 2), 0.95)

        if tool_name == "read_config":
            return 0.95 if success else 0.05

        if tool_name == "deepwiki_query":
            length_score = min(len(data) / 2000, 1.0) * 0.25
            return round(0.60 + length_score, 2)

        if tool_name == "fingerprint_match":
            return 0.95 if "Known issue found" in data else 0.0

        return 0.65 if success else 0.05


# ---------------------------------------------------------------------------
# Tool definition
# ---------------------------------------------------------------------------

@dataclass
class ToolSpec:
    """Serialisable description of a tool, used to build LLM function definitions."""
    name: str
    description: str
    parameters: dict[str, Any]  # JSON Schema


class Tool:
    """A single callable capability exposed to an Agent."""

    def __init__(
        self,
        name: str,
        description: str,
        parameters: dict[str, Any],
        fn: Callable[..., Coroutine[Any, Any, ToolResult] | ToolResult],
        timeout_s: int = 30,
    ) -> None:
        self.name = name
        self.description = description
        self.parameters = parameters
        self.fn = fn
        self.timeout_s = timeout_s

    @property
    def spec(self) -> ToolSpec:
        return ToolSpec(
            name=self.name,
            description=self.description,
            parameters={
                "type": "object",
                "properties": {
                    k: {kk: vv for kk, vv in v.items() if kk != "required"}
                    for k, v in self.parameters.items()
                },
                "required": [k for k, v in self.parameters.items()
                             if v.get("required", False)],
            },
        )

    async def execute(self, **kwargs: Any) -> ToolResult:
        """Invoke the tool with timeout isolation."""
        start = time.monotonic()

        try:
            if asyncio.iscoroutinefunction(self.fn):
                result = await asyncio.wait_for(
                    self.fn(**kwargs), timeout=self.timeout_s
                )
            else:
                result = await asyncio.wait_for(
                    asyncio.to_thread(self.fn, **kwargs), timeout=self.timeout_s
                )
        except asyncio.TimeoutError:
            return ToolResult.timeout(self.name, self.timeout_s)
        except Exception as exc:
            return ToolResult.failed(f"{type(exc).__name__}: {exc}")

        elapsed = (time.monotonic() - start) * 1000
        if isinstance(result, ToolResult):
            result.duration_ms = elapsed
            return result
        return ToolResult.ok(result, duration_ms=elapsed)

    def to_llm_definition(self) -> dict[str, Any]:
        """Return an OpenAI-compatible function-calling tool definition.

        Format: {"type": "function", "function": {"name", "description", "parameters"}}
        Works with DeepSeek, OpenAI, and any OpenAI-compatible endpoint.
        """
        spec = self.spec
        return {
            "type": "function",
            "function": {
                "name": spec.name,
                "description": spec.description,
                "parameters": spec.parameters,
            },
        }


# ---------------------------------------------------------------------------
# Registry
# ---------------------------------------------------------------------------

class ToolRegistry:
    """Holds all available tools and dispatches by name."""

    def __init__(self) -> None:
        self._tools: dict[str, Tool] = {}

    def register(self, tool: Tool) -> None:
        if tool.name in self._tools:
            raise KeyError(f"Tool '{tool.name}' is already registered")
        self._tools[tool.name] = tool

    def get(self, name: str) -> Tool:
        tool = self._tools.get(name)
        if tool is None:
            raise KeyError(f"Unknown tool '{name}'")
        return tool

    def list(self) -> list[Tool]:
        return list(self._tools.values())

    def llm_definitions(self) -> list[dict[str, Any]]:
        return [t.to_llm_definition() for t in self._tools.values()]
