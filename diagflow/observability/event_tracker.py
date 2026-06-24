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
import os
from datetime import datetime, timezone
from pathlib import Path
from typing import Any


class EventTracker:
    """Records the full trace of a diagnostic session."""

    def __init__(self, event_id: str, log_dir: str = "/tmp/diagflow") -> None:
        self.event_id = event_id
        self.session_dir = Path(log_dir) / event_id
        self.session_dir.mkdir(parents=True, exist_ok=True)
        self._step_counter = 0

    def log(self, message: str) -> None:
        """Log a plain message (shown to user and saved to file)."""
        timestamp = datetime.now(timezone.utc).isoformat()
        self._step_counter += 1
        filename = f"{self._step_counter:02d}_{message.split(':')[0][:40].replace(' ', '_')}.log"
        safe = filename.replace("/", "_").replace(" ", "_")
        (self.session_dir / safe).write_text(
            f"[{timestamp}]\n{message}\n", encoding="utf-8"
        )
        print(f"  🔍 {message}")

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

    def summary(self) -> str:
        """List all logged steps."""
        files = sorted(self.session_dir.glob("*.log"))
        lines = [f"Event: {self.event_id}", f"Steps: {len(files)}", ""]
        for f in files:
            first_line = f.read_text(encoding="utf-8").split("\n")[0]
            lines.append(f"  {f.name}: {first_line[:80]}")
        return "\n".join(lines)
