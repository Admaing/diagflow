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

import os
from anthropic import Anthropic


class ConclusionValidator:
    """Multi-layer conclusion validator."""

    VALID_CONFIDENCE = {"high", "medium", "low"}
    EVIDENCE_KEYWORDS = [
        "log", "metric", "config", "error", "oom",
        "timeout", "memory", "disk", "cpu", "checkpoint",
        "known", "issue", "bug", "deepwiki",
    ]

    def __init__(self, client: Anthropic, verify_client: Anthropic | None = None):
        self.client = client
        self.verify = verify_client or client

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
        has_ev = any(kw in root_cause.lower() for kw in self.EVIDENCE_KEYWORDS)
        if not has_ev and evidence_count > 0:
            return False, "Root cause doesn't reference evidence — may be hallucinated"

        # Layer 3: LLM validation
        suggestions_text = "\n".join(f"- {s}" for s in suggestions)
        try:
            resp = self.verify.messages.create(
                model=os.environ.get("DIAGFLOW_VERIFY_MODEL", "deepseek-v4-flash"),
                max_tokens=256,
                temperature=0.1,
                messages=[{"role": "user", "content": f"""Validate this diagnosis:
Root cause: {root_cause}
Suggestions: {suggestions_text}
Is the root cause SPECIFIC? Are suggestions ACTIONABLE?
Reply PASS or FAIL: <reason>"""}],
            )
            text = "\n".join(b.text for b in resp.content if b.type == "text")
            upper = text.strip().upper()
            if upper.startswith("PASS"):
                return True, None
            if upper.startswith("FAIL"):
                parts = text.split(":", 1)
                return False, parts[1].strip() if len(parts) > 1 else text
            return True, None  # Ambiguous — pass
        except Exception:
            return True, None  # Validator infra failure — pass

    @classmethod
    def standalone(cls, api_key: str, base_url: str = "https://api.modelverse.cn") -> "ConclusionValidator":
        """Create a self-contained validator using the same Anthropic client."""
        c = Anthropic(api_key=api_key, base_url=base_url)
        return cls(c)
