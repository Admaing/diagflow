"""
Memory and evidence pooling for multi-agent diagnosis.

Two complementary constructs:
  1. SessionMemory — keeps conversation context within a single diagnosis,
     managing the sliding window of messages sent to the LLM.
  2. EvidencePool — a shared, structured store where specialist agents
     deposit findings. The orchestrator reads from this pool when
     synthesising the final diagnosis. This decouples agents — they never
     talk to each other directly, only through evidence.
"""

from __future__ import annotations

import json
import logging
import threading
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Any

logger = logging.getLogger(__name__)


# ---------------------------------------------------------------------------
# Evidence
# ---------------------------------------------------------------------------

@dataclass
class Evidence:
    """A single finding from one diagnostic step."""
    source_agent: str            # e.g. "log_analyst", "metrics_analyst"
    category: str                # e.g. "log_error", "config_anomaly", "metric_spike"
    summary: str                 # human-readable finding
    detail: str                  # full context (e.g. matched log lines)
    confidence: float = 0.5      # 0.0 – 1.0
    timestamp: str = field(default_factory=lambda: datetime.now(timezone.utc).isoformat())
    raw_data: dict[str, Any] | None = None

    def to_dict(self) -> dict[str, Any]:
        return {
            "source_agent": self.source_agent,
            "category": self.category,
            "summary": self.summary,
            "detail": self.detail,
            "confidence": self.confidence,
            "timestamp": self.timestamp,
        }

    def short_str(self) -> str:
        return f"[{self.source_agent}] {self.summary} (conf={self.confidence:.1f})"


# ---------------------------------------------------------------------------
# Evidence Pool
# ---------------------------------------------------------------------------

class EvidencePool:
    """Thread-safe collection of evidence gathered during diagnosis.

    Agents write into the pool; the orchestrator reads from it.
    This is the backbone of our multi-agent architecture.
    """

    def __init__(self) -> None:
        self._items: list[Evidence] = []
        self._lock = threading.Lock()

    def add(self, evidence: Evidence) -> None:
        with self._lock:
            self._items.append(evidence)

    def add_many(self, evidences: list[Evidence]) -> None:
        with self._lock:
            self._items.extend(evidences)

    def all(self) -> list[Evidence]:
        with self._lock:
            return list(self._items)

    def by_agent(self, agent_name: str) -> list[Evidence]:
        with self._lock:
            return [e for e in self._items if e.source_agent == agent_name]

    def by_category(self, category: str) -> list[Evidence]:
        with self._lock:
            return [e for e in self._items if e.category == category]

    def high_confidence(self, threshold: float = 0.7) -> list[Evidence]:
        with self._lock:
            return [e for e in self._items if e.confidence >= threshold]

    def summary(self) -> str:
        """Compact summary for inclusion in the LLM prompt."""
        with self._lock:
            items = list(self._items)
        lines = ["=== Evidence Pool ==="]
        for e in items:
            lines.append(f"  {e.short_str()}")
        if not items:
            lines.append("  (no evidence collected yet)")
        return "\n".join(lines)

    def clear(self) -> None:
        with self._lock:
            self._items.clear()


# ---------------------------------------------------------------------------
# Session Memory
# ---------------------------------------------------------------------------

class SessionMemory:
    """Manages the conversation window for a single diagnosis session.

    Implements a simple sliding-window: once the estimated token count
    exceeds ``max_tokens``, earlier tool responses are summarised away
    to keep the context within bounds.
    """

    def __init__(self, max_tokens: int = 24_000) -> None:
        self.max_tokens = max_tokens
        self.messages: list[dict[str, Any]] = []

    def add_user(self, content: str) -> None:
        self.messages.append({"role": "user", "content": content})

    def add_assistant(self, content: str | None, tool_calls: list | None = None) -> None:
        msg: dict[str, Any] = {"role": "assistant", "content": content or ""}
        if tool_calls:
            msg["tool_calls"] = [
                {"id": tc.id, "type": "function",
                 "function": {"name": tc.name, "arguments": json.dumps(tc.arguments)}}
                for tc in tool_calls
            ]
        self.messages.append(msg)

    def add_tool_result(self, tool_call_id: str, content: str) -> None:
        self.messages.append({
            "role": "tool",
            "tool_call_id": tool_call_id,
            "content": content,
        })

    def get_messages(self) -> list[dict[str, Any]]:
        return self.messages
