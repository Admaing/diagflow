"""
Event tracker — records diagnostic steps for observability.

Inspired by Duwu's Troubleshooter's file-system logging, each diagnosis
session produces a trace of all steps taken, tool calls made, and
decisions reached. This is critical for:
  - Debugging when a diagnosis is wrong
  - Building trust with operators who can review what the AI did
  - Retrospective analysis of diagnosis quality
"""

from __future__ import annotations

import json
import logging
import os
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

logger = logging.getLogger(__name__)


class EventTracker:
    """Records the full trace of a diagnostic session."""

    def __init__(self, event_id: str, log_dir: str = "/tmp/diagflow") -> None:
        self.event_id = event_id
        self.session_dir = Path(log_dir) / event_id
        self.session_dir.mkdir(parents=True, exist_ok=True)
        self._step_counter = 0
        self._events_file = self.session_dir / "events.jsonl"

    def log(self, message: str) -> None:
        """Log a plain message (shown to user and saved to file)."""
        timestamp = datetime.now(timezone.utc).isoformat()
        self._step_counter += 1
        filename = f"{self._step_counter:02d}_{message.split(':')[0][:40].replace(' ', '_')}.log"
        safe = filename.replace("/", "_").replace(" ", "_")
        (self.session_dir / safe).write_text(
            f"[{timestamp}]\n{message}\n", encoding="utf-8"
        )

    def log_tool_call(self, tool_name: str, args: dict[str, Any], result: str) -> None:
        """Record a tool invocation."""
        self._step_counter += 1
        filename = f"{self._step_counter:02d}_tool_{tool_name}.log"
        content = (
            f"Tool: {tool_name}\n"
            f"Arguments: {json.dumps(args, ensure_ascii=False)}\n"
            f"Result:\n{result[:2000]}\n"
        )
        (self.session_dir / filename).write_text(content, encoding="utf-8")

    def log_structured(self, event_type: str, payload: dict[str, Any]) -> None:
        """Emit a structured JSON Lines event for downstream analysis.

        Event types: phase_started, phase_completed, tool_call_started,
        tool_call_completed, tool_call_failed, evidence_added, kb_hit,
        kb_miss, validation_passed, validation_failed.
        """
        record = {
            "ts": datetime.now(timezone.utc).isoformat(),
            "event_id": self.event_id,
            "type": event_type,
            **payload,
        }
        try:
            with open(self._events_file, "a", encoding="utf-8") as f:
                f.write(json.dumps(record, ensure_ascii=False, default=str) + "\n")
        except Exception:
            logger.warning("Failed to write structured event", exc_info=True)

    def summary(self) -> str:
        """List all logged steps."""
        files = sorted(self.session_dir.glob("*.log"))
        lines = [f"Event: {self.event_id}", f"Steps: {len(files)}", ""]
        for f in files:
            first_line = f.read_text(encoding="utf-8").split("\n")[0]
            lines.append(f"  {f.name}: {first_line[:80]}")
        return "\n".join(lines)
