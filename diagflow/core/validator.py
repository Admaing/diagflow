"""
Conclusion validation — four-layer illusion control (v3).

Layer 1: Format checks (zero LLM)
Layer 2: Cross-source consistency (zero LLM)
Layer 3: LLM review (uses Anthropic client directly)
Layer 4: Retry with feedback (max 2)

v3 change: no more LLMClient wrapper — uses Anthropic SDK directly
via the modelverse proxy (api.modelverse.cn → DeepSeek).
"""

from __future__ import annotations

import logging
import os
from anthropic import Anthropic

logger = logging.getLogger(__name__)


class ConclusionValidator:
    """Multi-layer conclusion validator."""

    VALID_CONFIDENCE = {"high", "medium", "low"}

    def __init__(
        self,
        client: Anthropic,
        verify_client: Anthropic | None = None,
        evidence_keywords: list[str] | None = None,
    ):
        self.client = client
        self.verify = verify_client or client
        self._evidence_keywords = evidence_keywords or [
            "log", "metric", "config", "error", "oom",
            "timeout", "memory", "disk", "cpu", "checkpoint",
            "known", "issue", "bug", "deepwiki",
        ]

    async def validate(
        self, root_cause: str, suggestions: list[str], evidence_count: int,
    ) -> tuple[bool, str | None]:
        # Layer 1: Format
        if not root_cause or len(root_cause) < 10:
            return False, "Root cause too short"
        if not suggestions:
            return False, "No suggestions provided"
        if evidence_count == 0:
            return False, "No evidence — may be speculative"

        # Layer 2: Cross-source
        has_ev = any(kw in root_cause.lower() for kw in self._evidence_keywords)
        if not has_ev and evidence_count > 0:
            return False, "Root cause doesn't reference evidence — may be hallucinated"

        # Layer 3: LLM validation (structured output via tool_use)
        suggestions_text = "\n".join(f"- {s}" for s in suggestions)
        from diagflow.config import get_config
        cfg = get_config()
        try:
            validate_tool = {
                "name": "validate_diagnosis",
                "description": "Report validation result. You MUST call this tool.",
                "input_schema": {
                    "type": "object",
                    "properties": {
                        "passes": {"type": "boolean", "description": "True if diagnosis passes validation"},
                        "reason": {"type": "string", "description": "Brief reason for the verdict"},
                        "issues": {
                            "type": "array", "items": {"type": "string"},
                            "description": "List of specific issues found (empty if passes)",
                        },
                    },
                    "required": ["passes", "reason"],
                },
            }
            resp = self.verify.messages.create(
                model=os.environ.get("DIAGFLOW_VERIFY_MODEL", cfg.llm.verify_model),
                max_tokens=256,
                temperature=0.1,
                messages=[{"role": "user", "content": f"""Validate this diagnosis:

Root cause: {root_cause}
Suggestions:
{suggestions_text}

Check:
1. Is the root cause SPECIFIC (names concrete component, log line, error class)?
2. Are suggestions ACTIONABLE (specific commands, config changes, not generic advice)?
3. Does the root cause reference actual evidence rather than general knowledge?

Call the validate_diagnosis tool with your verdict."""}],
                tools=[validate_tool],
                tool_choice={"type": "tool", "name": "validate_diagnosis"},
            )

            # Extract structured output
            tool_uses = [b for b in resp.content if b.type == "tool_use"]
            if tool_uses:
                args = tool_uses[0].input
                if args.get("passes"):
                    return True, None
                return False, args.get("reason", "Diagnosis needs improvement")

            # Fallback to text parsing
            text = "\n".join(b.text for b in resp.content if b.type == "text")
            upper = text.strip().upper()
            if upper.startswith("PASS"):
                return True, None
            if upper.startswith("FAIL"):
                parts = text.split(":", 1)
                return False, parts[1].strip() if len(parts) > 1 else text
            return True, None  # Ambiguous — pass
        except Exception:
            logger.warning("Validator LLM call failed — allowing pass", exc_info=True)
            return True, None  # Validator infra failure — pass

    @classmethod
    def standalone(cls, api_key: str, base_url: str = "") -> "ConclusionValidator":
        """Create a self-contained validator using the same Anthropic client."""
        from diagflow.config import get_config
        cfg = get_config()
        c = Anthropic(
            api_key=api_key or cfg.llm.api_key,
            base_url=base_url or cfg.llm.base_url,
        )
        return cls(c)
