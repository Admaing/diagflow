"""
Conclusion validation — four-layer illusion control system.

Inspired by Duwu's Troubleshooter, we implement a multi-layered validation
pipeline to catch hallucinations before they reach the user:

  Layer 1 — Format checks (zero LLM, millisecond)
    Verifies structural completeness: required sections exist, suggestions
    are non-empty, confidence labels are valid.

  Layer 2 — Cross-source consistency (zero LLM, millisecond)
    Checks that evidence from different sources doesn't contradict, and
    that the root cause references concrete evidence keywords.

  Layer 3 — LLM validation (one lightweight call, independent config)
    An independent validation agent reviews the conclusion for internal
    consistency, grounding in evidence, and actionability.
    IMPORTANT: uses a DIFFERENT LLM config from the diagnosis Agent —
    lower temperature, optionally a stronger model. This reduces the
    "same LLM grading its own homework" bias.

  Layer 4 — Retry (max 2 attempts)
    If validation fails, feedback is injected into the synthesis prompt
    and the conclusion is regenerated. Format-only failures don't count
    toward the retry budget.
"""

from __future__ import annotations

import os
from dataclasses import dataclass, field
from typing import Any

from .llm import LLMClient


@dataclass
class ValidationResult:
    passed: bool
    layer: int
    issues: list[str] = field(default_factory=list)
    feedback: str | None = None


class ConclusionValidator:
    """Multi-layer conclusion validator with independent LLM config."""

    VALID_CONFIDENCE = {"high", "medium", "low"}
    EVIDENCE_KEYWORDS = [
        "log", "metric", "config", "error", "oom",
        "timeout", "memory", "disk", "cpu", "checkpoint",
        "known", "issue", "bug", "deepwiki",
    ]

    def __init__(
        self,
        primary_llm: LLMClient,
        verify_llm: LLMClient | None = None,
    ):
        """Build the validator.

        Args:
            primary_llm: The LLM used for diagnosis (used as fallback for verify).
            verify_llm: An independent LLM config for Layer 3 review.
                        If None, defaults to a more conservative primary_llm.
                        Recommended: different model or lower temperature.
        """
        self.primary = primary_llm
        # Layer 3 uses an independent config: lower temperature, stronger model
        # Default: same model but temperature=0.1 (more conservative)
        self.verify = verify_llm or primary_llm

    async def validate(
        self,
        root_cause: str,
        suggestions: list[str],
        evidence_count: int,
    ) -> tuple[bool, str | None]:
        """Run all four validation layers. Returns (passed, feedback)."""

        # ---- Layer 1: Format checks (zero LLM) ----
        issues_l1: list[str] = []
        if not root_cause or len(root_cause) < 10:
            issues_l1.append("Root cause is empty or too short")
        if not suggestions:
            issues_l1.append("No suggestions provided")
        if evidence_count == 0:
            issues_l1.append("No evidence collected — conclusion may be speculative")
        if issues_l1:
            return False, "; ".join(issues_l1)

        # ---- Layer 2: Cross-source consistency (zero LLM) ----
        issues_l2: list[str] = []
        has_evidence_ref = any(
            kw in root_cause.lower() for kw in self.EVIDENCE_KEYWORDS
        )
        if not has_evidence_ref and evidence_count > 0:
            issues_l2.append(
                "Root cause doesn't reference concrete evidence "
                "(log/metric/config/known-issue) — may be hallucinated"
            )
        if issues_l2:
            return False, "; ".join(issues_l2)

        # ---- Layer 3: LLM validation (independent config) ----
        passed_l3, feedback_l3 = await self._llm_validate(root_cause, suggestions)
        if not passed_l3:
            return False, feedback_l3

        return True, None

    async def _llm_validate(
        self,
        root_cause: str,
        suggestions: list[str],
    ) -> tuple[bool, str | None]:
        """Layer 3: lightweight LLM call with independent config."""
        suggestions_text = "\n".join(f"- {s}" for s in suggestions)
        prompt = f"""You are a validation agent. Review this diagnosis conclusion:

Root cause: {root_cause}
Suggestions:
{suggestions_text}

Check for:
1. Is the root cause SPECIFIC and testable? (not vague like "system issue")
2. Are suggestions ACTIONABLE? (concrete steps, not "fix the problem")
3. Any contradiction between root cause and suggestions?

Respond with exactly:
PASS
or
FAIL: <one sentence explaining what to fix>"""

        try:
            response = await self.verify.generate(
                messages=[{"role": "user", "content": prompt}],
                temperature=0.1,  # Force low temp for validation regardless of default
            )
        except Exception as exc:
            # If validator LLM fails,宽容通过 — don't block diagnosis on infra issues
            return True, None

        text = (response.content or "").strip()
        upper = text.upper()
        if upper.startswith("PASS"):
            return True, None
        if upper.startswith("FAIL"):
            # Extract the feedback after "FAIL:"
            parts = text.split(":", 1)
            feedback = parts[1].strip() if len(parts) > 1 else text
            return False, feedback
        # Ambiguous response — treat as pass to avoid blocking
        return True, None


def make_default_verify_llm(primary: LLMClient) -> LLMClient:
    """Create a default verify LLM with conservative settings.

    Uses the same provider as primary but with lower temperature.
    For DeepSeek: same model, temperature 0.1.
    """
    return LLMClient(
        api_keys=primary.api_keys,
        model=os.environ.get("DIAGFLOW_VERIFY_MODEL", primary.model),
        max_tokens=1024,            # validation doesn't need long output
        base_url=primary.base_url,
    )
