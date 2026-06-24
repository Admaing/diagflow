"""
DiagnosisEngine — the single entry point for a diagnosis.

Replaces the v1 trio (Orchestrator + Supervisor + Specialist Agents) with
one orchestrator that drives a clear 4-phase pipeline:

  Phase 1: KnowledgeBase.match()       — 0 LLM, fast-path for known issues
  Phase 2: Strategy execution          — 0 LLM, parallel deterministic tool calls
  Phase 3: Agent.run(evidence_so_far)  — LLM ReAct, DeepWiki verification
  Phase 4: Validator.validate()        — LLM (different config), quality gate

The engine itself makes NO LLM decisions. It just sequences the phases.
All intelligence lives in Agent (Phase 3) and Validator (Phase 4).
"""

from __future__ import annotations

import asyncio
import time
from dataclasses import dataclass, field
from typing import Any, Callable

from .agent import Agent
from .llm import LLMClient
from .memory import EvidencePool, Evidence
from .strategy import Strategy, StrategyStep, load_strategy
from .tool import ToolRegistry, ToolResult
from .validator import ConclusionValidator


@dataclass
class DiagnosisReport:
    """Final output of a diagnosis session."""
    event_id: str
    component: str
    problem_type: str
    root_cause: str
    confidence: str
    evidence_summary: list[dict[str, Any]]
    suggestions: list[str]
    matched_knowledge: bool
    duration_ms: float
    phases_run: list[str] = field(default_factory=list)
    raw_findings: list[Evidence] = field(default_factory=list)


class DiagnosisEngine:
    """The single orchestrator — sequences phases, owns no intelligence."""

    def __init__(
        self,
        llm: LLMClient,
        tool_registry: ToolRegistry,
        agent: Agent,
        validator: ConclusionValidator,
        knowledge_base=None,
        strategies_dir: str = "",
        on_event: Callable[[str], None] | None = None,
    ):
        self.llm = llm
        self.tool_registry = tool_registry
        self.agent = agent
        self.validator = validator
        self.kb = knowledge_base
        self.strategies_dir = strategies_dir
        self.on_event = on_event

    async def _emit(self, msg: str) -> None:
        if self.on_event:
            result = self.on_event(msg)
            if asyncio.iscoroutine(result):
                await result

    async def diagnose(
        self,
        component: str,
        problem_type: str,
        context: dict[str, Any],
    ) -> DiagnosisReport:
        """Run the full 4-phase diagnostic pipeline."""
        event_id = f"diag-{int(time.time())}-{abs(hash(str(context))) % 10000:04d}"
        start = time.monotonic()
        phases_run: list[str] = []
        evidence = EvidencePool()

        await self._emit(f"[{event_id}] starting {component}/{problem_type} diagnosis")

        # Load strategy YAML
        strategy = load_strategy(component, problem_type, self.strategies_dir)
        await self._emit(f"[{event_id}] loaded strategy: {len(strategy.steps)} steps")

        # ---- Phase 1: Knowledge base fast-path ----
        if self.kb:
            kb_hit = await self._kb_match(context, strategy)
            if kb_hit:
                await self._emit(f"[{event_id}] KB hit — skipping LLM")
                phases_run.append("kb_fast_path")
                return DiagnosisReport(
                    event_id=event_id,
                    component=component,
                    problem_type=problem_type,
                    root_cause=kb_hit.get("root_cause", "known issue"),
                    confidence="high",
                    evidence_summary=[],
                    suggestions=kb_hit.get("suggestions", []),
                    matched_knowledge=True,
                    duration_ms=(time.monotonic() - start) * 1000,
                    phases_run=phases_run,
                )

        # ---- Phase 2: Strategy execution (deterministic, parallel by priority) ----
        await self._emit(f"[{event_id}] Phase 2: strategy execution")
        phases_run.append("strategy")
        await self._run_strategy(strategy, context, evidence)
        await self._emit(f"[{event_id}] evidence collected: {len(evidence.all())} items")

        # ---- Phase 2.5: KB re-check with actual evidence (log error patterns) ----
        if self.kb and not phases_run.count("kb_fast_path"):
            kb_hit = await self._kb_match_from_evidence(context, evidence)
            if kb_hit:
                await self._emit(f"[{event_id}] KB hit from evidence — skipping LLM")
                phases_run.append("kb_evidence_hit")
                # Build report directly from cache
                return DiagnosisReport(
                    event_id=event_id,
                    component=component,
                    problem_type=problem_type,
                    root_cause=kb_hit.get("root_cause", "known issue"),
                    confidence="high",
                    evidence_summary=[e.to_dict() for e in evidence.all()],
                    suggestions=kb_hit.get("suggestions", []),
                    matched_knowledge=True,
                    duration_ms=(time.monotonic() - start) * 1000,
                    phases_run=phases_run,
                    raw_findings=evidence.all(),
                )

        # ---- Phase 3: Agent ReAct (DeepWiki verification + reasoning) ----
        await self._emit(f"[{event_id}] Phase 3: agent ReAct")
        phases_run.append("react")
        task = strategy.build_task_prompt(context)
        agent_output = await self.agent.run(
            task=task,
            evidence_so_far=evidence,
        )
        # The Agent's free-form output is kept as one evidence item
        evidence.add(Evidence(
            source_agent="react",
            category="agent_analysis",
            summary=agent_output[:300],
            detail=agent_output,
            confidence=0.7,
        ))

        # ---- Phase 4: Synthesize + Validate ----
        await self._emit(f"[{event_id}] Phase 4: synthesis + validation")
        phases_run.append("synthesize")
        root_cause, suggestions, confidence = await self.agent.synthesize(
            evidence=evidence,
            context=context,
            component=component,
            problem_type=problem_type,
        )

        # Validation (4-layer)
        passed, feedback = await self.validator.validate(
            root_cause=root_cause,
            suggestions=suggestions,
            evidence_count=len(evidence.all()),
        )
        if not passed and feedback:
            await self._emit(f"[{event_id}] validation failed, retrying synthesis")
            root_cause, suggestions, confidence = await self.agent.synthesize(
                evidence=evidence,
                context=context,
                component=component,
                problem_type=problem_type,
                feedback=feedback,
            )
        phases_run.append("validate")

        duration_ms = (time.monotonic() - start) * 1000
        await self._emit(f"[{event_id}] done in {duration_ms:.0f}ms")

        # ---- Phase 5: Index the case into KB (async, best-effort) ----
        if self.kb:
            try:
                await self._kb_index(root_cause, suggestions, evidence, context)
                phases_run.append("kb_index")
            except Exception as exc:
                await self._emit(f"[{event_id}] KB index failed: {exc}")

        return DiagnosisReport(
            event_id=event_id,
            component=component,
            problem_type=problem_type,
            root_cause=root_cause,
            confidence=confidence,
            evidence_summary=[e.to_dict() for e in evidence.all()],
            suggestions=suggestions,
            matched_knowledge=False,
            duration_ms=duration_ms,
            phases_run=phases_run,
            raw_findings=evidence.all(),
        )

    # ------------------------------------------------------------------
    # Phase 2 internals — pure deterministic tool execution
    # ------------------------------------------------------------------

    async def _run_strategy(
        self,
        strategy: Strategy,
        context: dict[str, Any],
        evidence: EvidencePool,
    ) -> None:
        """Execute strategy steps in priority batches, parallel within batch."""
        for batch in strategy.group_by_priority():
            tasks = [
                self._execute_step(step, context, evidence)
                for step in batch
            ]
            await asyncio.gather(*tasks)

    async def _execute_step(
        self,
        step: StrategyStep,
        context: dict[str, Any],
        evidence: EvidencePool,
    ) -> None:
        """Execute a single strategy step — no LLM, just tool dispatch."""
        if step.action == "fingerprint_match":
            # Fingerprint match is handled by KB in Phase 1; no-op here
            return

        if step.action != "tool_call":
            return

        tool = self.tool_registry._tools.get(step.tool)
        if tool is None:
            evidence.add(Evidence(
                source_agent="strategy",
                category="error",
                summary=f"unknown tool: {step.tool}",
                detail=f"Strategy referenced non-existent tool {step.tool}",
                confidence=0.0,
            ))
            return

        params = step.render_params(context)
        await self._emit(f"[strategy] {step.tool}({params})")
        result = await tool.execute(**params)
        confidence = ToolResult.compute_confidence(
            step.tool, str(result.data or ""), result.success
        )
        evidence.add(Evidence(
            source_agent="strategy",
            category=f"tool:{step.tool}",
            summary=result.to_llm_message()[:200],
            detail=result.to_llm_message(),
            confidence=confidence,
        ))

    # ------------------------------------------------------------------
    # KB helpers (best-effort — KB is optional)
    # ------------------------------------------------------------------------------------------------------------------------------------

    async def _kb_match(self, context: dict[str, Any], strategy: Strategy) -> dict | None:
        """Phase 1: KB check from context alone (weak — only problem description)."""
        if not self.kb:
            return None
        try:
            component = context.get("component", "")
            error = context.get("problem", "")
            version = context.get("version", "")
            return self.kb.fingerprint_match(component, error, version)
        except Exception:
            return None

    async def _kb_match_from_evidence(
        self, context: dict[str, Any], evidence: EvidencePool
    ) -> dict | None:
        """Phase 2.5: KB check from actual evidence (strong — real error patterns)."""
        if not self.kb:
            return None
        component = context.get("component", "")
        version = context.get("version", "")
        try:
            # Extract error patterns from log evidence
            for ev in evidence.by_category("tool:query_node_log"):
                log_text = ev.detail or ""
                for keyword in [
                    "OutOfMemoryError", "OOM", "FATAL", "CheckpointExpired",
                    "NoSpaceLeft", "disk full", "Connection refused",
                    "GC overhead limit exceeded", "exit code 137",
                ]:
                    if keyword in log_text:
                        hit = self.kb.fingerprint_match(component, keyword, version)
                        if hit:
                            return hit
            # Also check metrics for critical anomalies
            for ev in evidence.by_category("tool:query_metrics"):
                metric_text = ev.detail or ""
                if "backpressure_level=HIGH" in metric_text:
                    hit = self.kb.fingerprint_match(component, "backpressure", version)
                    if hit:
                        return hit
        except Exception:
            pass
        return None

    async def _kb_index(
        self,
        root_cause: str,
        suggestions: list[str],
        evidence: EvidencePool,
        context: dict[str, Any],
    ) -> None:
        """Phase 5: Auto-index the case for future fast-path reuse."""
        if not self.kb:
            return
        try:
            component = context.get("component", "unknown")
            version = context.get("version", "")
            # Extract error pattern from evidence
            error_pattern = root_cause[:80]
            for ev in evidence.all():
                detail = ev.detail or ""
                for kw in ["OutOfMemoryError", "OOM", "CheckpointExpired",
                           "NoSpaceLeft", "FATAL", "backpressure"]:
                    if kw in detail:
                        error_pattern = kw
                        break
            self.kb.add_case(
                component=component,
                error_pattern=error_pattern,
                version=version,
                root_cause=root_cause,
                suggestions=suggestions,
            )
        except Exception:
            pass  # KB indexing is best-effort
