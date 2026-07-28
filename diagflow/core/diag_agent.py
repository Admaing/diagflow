"""
DiagAgent v3 — single Agent powered by Anthropic SDK's native tool use.

Architecture (Troubleshooter-inspired, SDK-powered):
  Phase 1: KB fingerprint fast-path     (0 LLM)
  Phase 2: Strategy deterministic exec  (0 LLM)
  Phase 3: SDK ReAct + DeepWiki verify (Anthropic SDK handles loop)
  Phase 4: Validation                   (independent LLM check)
  Phase 5: Auto-index to KB            (best-effort)

Why Anthropic SDK:
  The modelverse proxy (api.modelverse.cn) translates Anthropic-format
  requests to DeepSeek. So we get SDK's native tool_use without writing
  our own ReAct loop, while using DeepSeek as the backend.
"""

from __future__ import annotations

import asyncio
import json
import logging
import os
import time
from dataclasses import dataclass, field
from typing import Any, Callable

from anthropic import Anthropic

from .memory import EvidencePool, Evidence
from .strategy import Strategy, StrategyStep, load_strategy

logger = logging.getLogger(__name__)


# ---------------------------------------------------------------------------
# Report
# ---------------------------------------------------------------------------

@dataclass
class DiagnosisReport:
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


# ---------------------------------------------------------------------------
# Tool definition (Anthropic SDK format)
# ---------------------------------------------------------------------------

@dataclass
class ToolDef:
    """An Anthropic-compatible tool definition with an async handler."""
    name: str
    description: str
    input_schema: dict[str, Any]
    handler: Callable[..., Any]  # async callable returning ToolResult


# ---------------------------------------------------------------------------
# DiagAgent
# ---------------------------------------------------------------------------

class DiagAgent:
    """Single Agent — SDK-powered ReAct + Strategy-driven Phase 2."""

    def __init__(
        self,
        api_key: str = "",
        base_url: str = "",
        model: str = "",
        max_tokens: int = 4096,
        strategies_dir: str = "",
        knowledge_base=None,
        validator=None,
        on_event: Callable | None = None,
        db=None,  # optional MySQL persistence (diagflow.observability.db)
    ):
        # Resolve from config if not explicitly provided
        from diagflow.config import get_config
        cfg = get_config()

        self.client = Anthropic(
            api_key=api_key or cfg.llm.api_key or os.environ.get("DEEPSEEK_API_KEY", "") or os.environ.get("LLM_API_KEY", ""),
            base_url=base_url or cfg.llm.base_url,
        )
        self.model = model or cfg.llm.model
        self.max_tokens = max_tokens or cfg.llm.max_tokens
        self.strategies_dir = strategies_dir or cfg.strategies_dir
        self.kb = knowledge_base
        self.validator = validator
        self.on_event = on_event
        self._db = db  # optional MySQL for production metrics
        self._error_keywords = cfg.rag.error_keywords
        self._tools: dict[str, ToolDef] = {}
        self._event_id: str = ""  # set by diagnose()
        self._llm_calls: int = 0
        self._step_order: int = 0

    # ------------------------------------------------------------------
    # Tool registration
    # ------------------------------------------------------------------

    def register_tool(self, tool: ToolDef) -> None:
        self._tools[tool.name] = tool

    def register_tools(self, tools: list[ToolDef]) -> None:
        for t in tools:
            self.register_tool(t)

    def _tool_schemas(self) -> list[dict[str, Any]]:
        return [
            {
                "name": t.name,
                "description": t.description,
                "input_schema": t.input_schema,
            }
            for t in self._tools.values()
        ]

    # ------------------------------------------------------------------
    # Main entry point
    # ------------------------------------------------------------------

    async def diagnose(
        self,
        component: str,
        problem_type: str,
        context: dict[str, Any],
    ) -> DiagnosisReport:
        event_id = f"diag-{int(time.time())}-{abs(hash(str(context))) % 10000:04d}"
        self._event_id = event_id
        self._llm_calls = 0
        self._step_order = 0
        start = time.monotonic()
        phases: list[str] = []
        evidence = EvidencePool()
        matched_knowledge = False
        kb_match_phase = ""

        await self._emit(f"[{event_id}] starting {component}/{problem_type}")

        # Load strategy
        strategy = load_strategy(component, problem_type, self.strategies_dir)
        await self._emit(f"[{event_id}] strategy: {len(strategy.steps)} steps")

        # ---- Phase 1: KB fast-path ----
        if self.kb:
            hit = self._kb_match(context)
            if hit:
                await self._emit(f"[{event_id}] KB hit — skipping LLM")
                duration_ms = (time.monotonic() - start) * 1000
                report = DiagnosisReport(
                    event_id=event_id, component=component,
                    problem_type=problem_type,
                    root_cause=hit.get("root_cause", "known issue"),
                    confidence="high", evidence_summary=[],
                    suggestions=hit.get("suggestions", []),
                    matched_knowledge=True,
                    duration_ms=duration_ms,
                    phases_run=["kb_fast_path"],
                )
                await self._write_diagnosis(report, context, component, problem_type,
                                            kb_matched=True, kb_match_phase="phase1_semantic",
                                            error_msg="")
                return report

        # ---- Phase 2: Strategy execution (deterministic) ----
        await self._emit(f"[{event_id}] Phase 2: strategy execution")
        phases.append("strategy")
        await self._run_strategy(strategy, context, evidence)
        await self._emit(f"[{event_id}] evidence: {len(evidence.all())} items")

        # Phase 2.5: KB re-check with evidence
        if self.kb:
            hit = self._kb_evidence_match(context, evidence)
            if hit:
                await self._emit(f"[{event_id}] KB evidence hit")
                phases.append("kb_evidence_hit")
                report = DiagnosisReport(
                    event_id=event_id, component=component,
                    problem_type=problem_type,
                    root_cause=hit.get("root_cause", "known issue"),
                    confidence="high",
                    evidence_summary=[e.to_dict() for e in evidence.all()],
                    suggestions=hit.get("suggestions", []),
                    matched_knowledge=True,
                    duration_ms=(time.monotonic() - start) * 1000,
                    phases_run=phases,
                )
                await self._write_diagnosis(report, context, component, problem_type,
                                            kb_matched=True, kb_match_phase="phase2.5_fingerprint",
                                            error_msg="")
                return report

        # ---- Phase 3: SDK ReAct (Anthropic native tool_use) ----
        await self._emit(f"[{event_id}] Phase 3: SDK ReAct")
        phases.append("react")
        agent_output = await self._run_react(
            task=strategy.build_task_prompt(context),
            evidence=evidence,
            context=context,
            max_turns=12,
        )
        evidence.add(Evidence(
            source_agent="react", category="agent_analysis",
            summary=agent_output[:300], detail=agent_output, confidence=0.7,
        ))

        # ---- Phase 4: Synthesize + Validate ----
        await self._emit(f"[{event_id}] Phase 4: synthesis + validation")
        phases.append("validate")
        root_cause, suggestions, confidence = await self._synthesize(
            evidence, context, component, problem_type
        )
        if self.validator:
            passed, feedback = await self.validator.validate(
                root_cause, suggestions, len(evidence.all()),
            )
            if not passed and feedback:
                await self._emit(f"[{event_id}] validation failed, retrying")
                root_cause, suggestions, confidence = await self._synthesize(
                    evidence, context, component, problem_type, feedback=feedback,
                )

        duration_ms = (time.monotonic() - start) * 1000
        await self._emit(f"[{event_id}] done in {duration_ms:.0f}ms")

        # ---- Phase 5: Auto-index (only high-confidence, passed validation) ----
        if self.kb and confidence == "high":
            try:
                self._kb_index(root_cause, suggestions, evidence, context)
                phases.append("kb_index")
            except Exception:
                logger.warning("Phase 5 KB auto-index failed", exc_info=True)

        report = DiagnosisReport(
            event_id=event_id, component=component, problem_type=problem_type,
            root_cause=root_cause, confidence=confidence,
            evidence_summary=[e.to_dict() for e in evidence.all()],
            suggestions=suggestions, matched_knowledge=False,
            duration_ms=duration_ms, phases_run=phases,
        )
        await self._write_diagnosis(report, context, component, problem_type,
                                    kb_matched=False, error_msg="")
        return report

    # ------------------------------------------------------------------
    # Phase 2: Strategy execution
    # ------------------------------------------------------------------

    async def _run_strategy(
        self, strategy: Strategy, context: dict, evidence: EvidencePool,
    ) -> None:
        for batch in strategy.group_by_priority():
            tasks = [
                self._execute_step(step, context, evidence)
                for step in batch
            ]
            await asyncio.gather(*tasks)

    async def _execute_step(
        self, step: StrategyStep, context: dict, evidence: EvidencePool,
    ) -> None:
        if step.action in ("fingerprint_match",):
            return
        if step.action != "tool_call":
            return

        tool = self._tools.get(step.tool)
        if not tool:
            evidence.add(Evidence(
                source_agent="strategy", category="error",
                summary=f"unknown tool: {step.tool}",
                detail=f"Strategy referenced non-existent tool {step.tool}",
                confidence=0.0,
            ))
            await self._log_step("phase_2", tool_name=step.tool, status="error",
                                 error_detail=f"Unknown tool: {step.tool}")
            return

        params = step.render_params(context)
        action = params.get("action", "")
        t0 = time.monotonic()
        await self._emit(f"[strategy] {step.tool}({params})")
        try:
            result = await tool.handler(**params)
            duration_ms = int((time.monotonic() - t0) * 1000)
            result_str = str(getattr(result, "data", result))[:200]
            if isinstance(result, str):
                result_str = result[:200]
                evidence.add(Evidence(
                    source_agent="strategy",
                    category=f"tool:{step.tool}",
                    summary=result[:200], detail=result, confidence=0.8,
                ))
            elif hasattr(result, "success"):
                conf = 0.8 if result.success else 0.2
                evidence.add(Evidence(
                    source_agent="strategy",
                    category=f"tool:{step.tool}",
                    summary=str(result.data)[:200] if result.success else str(result.error)[:200],
                    detail=str(result.data) if result.success else str(result.error),
                    confidence=conf,
                ))
            await self._log_step("phase_2", tool_name=step.tool, action=action,
                                 status="success", duration_ms=duration_ms,
                                 summary=result_str)
        except Exception as exc:
            duration_ms = int((time.monotonic() - t0) * 1000)
            evidence.add(Evidence(
                source_agent="strategy", category="error",
                summary=f"{step.tool} failed: {exc}",
                detail=str(exc), confidence=0.0,
            ))
            await self._log_step("phase_2", tool_name=step.tool, action=action,
                                 status="error", duration_ms=duration_ms,
                                 error_detail=str(exc))

    # ------------------------------------------------------------------
    # Phase 3: SDK ReAct (Anthropic native tool_use loop)
    # ------------------------------------------------------------------

    async def _run_react(
        self, task: str, evidence: EvidencePool, context: dict, max_turns: int = 12,
    ) -> str:
        """Use Anthropic SDK's messages.create with tool_use.

        The SDK handles the ReAct loop natively — we just feed responses
        back into messages and dispatch tool calls.
        """
        # Inject topology if available
        topo = context.get("topology", {})
        topo_text = ""
        if topo:
            import json
            topo_lines = ["## Cluster Topology (installed components & log paths)"]
            for comp, cfg in topo.items():
                for svc_name, svc in cfg.items():
                    role = svc.get("node_role", "?")
                    paths = svc.get("log_paths", [])
                    proc = svc.get("process_name", "")
                    topo_lines.append(f"  {comp}/{svc_name}: role={role}, logs={paths}, process={proc}")
            topo_text = "\n".join(topo_lines) + "\n"

        system = f"""You are a senior SRE diagnosing a big data platform issue.

{topo_text}
## Evidence Already Collected (Phase 2 — do NOT re-query)
{evidence.summary()}

## Playbook
1. **YARN first**: If query_yarn shows apps, call app_nodes to find EXACT nodes.
   Then ssh_exec to /data/yarn/container-logs/{{app_id}}/ for container logs.
2. **Standalone**: If no YARN apps, ssh_exec 'find /data -name *flink*.log 2>/dev/null'.
3. **DeepWiki**: Verify specific error classes (OOM, CheckpointExpired) against component repo.
4. **3-turn rule**: If 3 tool calls find NO new evidence, STOP. Output conclusion with
   what you know. Mark unverified claims as low confidence.

Component → repo: flink→apache/flink, hdfs/yarn→apache/hadoop, kafka→apache/kafka.
ALWAYS use Keywords='ERROR,Exception,FATAL' on large logs."""

        messages: list[dict[str, Any]] = [{"role": "user", "content": task}]
        tools = self._tool_schemas()
        turns = 0
        dry_turns = 0           # consecutive turns with no new evidence
        last_evidence_count = 0
        collected_evidence: list[str] = []

        while turns < max_turns:
            self._llm_calls += 1
            resp = self.client.messages.create(
                model=self.model,
                max_tokens=self.max_tokens,
                temperature=0.3,
                system=system,
                messages=messages,
                tools=tools or None,
            )

            # Check for tool_use
            tool_uses = [b for b in resp.content if b.type == "tool_use"]
            text_parts = [b.text for b in resp.content if b.type == "text"]

            if not tool_uses:
                # No more tools — agent has concluded
                return "\n".join(text_parts) if text_parts else str(resp.content)

            # Build assistant message with tool_use blocks
            messages.append({
                "role": "assistant",
                "content": resp.content,
            })

            # Execute each tool and collect results
            tool_results: list[dict[str, Any]] = []
            for tu in tool_uses:
                tool = self._tools.get(tu.name)
                if tool:
                    try:
                        await self._emit(f"[react] {tu.name}({json.dumps(tu.input)[:200]})")
                        result = await tool.handler(**tu.input)
                        result_text = str(result) if isinstance(result, str) else str(getattr(result, 'data', result))
                        await self._emit(f"[react] result: {result_text[:200]}")
                        tool_results.append({
                            "type": "tool_result",
                            "tool_use_id": tu.id,
                            "content": result_text,
                        })
                    except Exception as exc:
                        await self._emit(f"[react] {tu.name} error: {exc}")
                        tool_results.append({
                            "type": "tool_result",
                            "tool_use_id": tu.id,
                            "content": f"Error: {exc}",
                            "is_error": True,
                        })
                else:
                    tool_results.append({
                        "type": "tool_result",
                        "tool_use_id": tu.id,
                        "content": f"Unknown tool: {tu.name}",
                    })

            if tool_results:
                messages.append({"role": "user", "content": tool_results})
                # Stall detection: track if we're finding new things
                new_evidence = sum(1 for tr in tool_results
                                   if not tr.get("is_error")
                                   and (tr.get("content", "") or "").strip())
                if new_evidence > 0:
                    dry_turns = 0
                else:
                    dry_turns += 1
                    collected_evidence.append(f"Turn {turns}: all tools returned empty/errors")
            else:
                dry_turns += 1

            turns += 1

            # 3-turn rule: force conclusion if stuck
            if dry_turns >= 3:
                messages.append({"role": "user", "content": (
                    "You've explored for 3 turns without finding new evidence. "
                    "The cluster may have unusual log paths, missing services, or "
                    "network issues. STOP now and produce a diagnosis with what you "
                    "HAVE. Mark uncertain claims as low confidence. List what you "
                    "could NOT verify."
                )})
                # One final turn for synthesis
                self._llm_calls += 1
                resp = self.client.messages.create(
                    model=self.model, max_tokens=self.max_tokens,
                    temperature=0.3, system=system, messages=messages,
                )
                text = "\n".join(b.text for b in resp.content if b.type == "text")
                return text or "Unable to diagnose — insufficient evidence after exhaustive search."

        return "[agent] Max turns reached. Evidence: " + str(len(collected_evidence)) + " pieces."

    # ------------------------------------------------------------------
    # Phase 4: Synthesis (structured output via SDK tool_use)
    # ------------------------------------------------------------------

    SYNTHESIS_TOOL_SCHEMA = {
        "type": "object",
        "properties": {
            "root_cause": {
                "type": "string",
                "description": "One specific sentence identifying the root cause, grounded in evidence.",
            },
            "confidence": {
                "type": "string",
                "enum": ["high", "medium", "low"],
                "description": "Confidence level: high (multiple corroborating sources), medium (strong indicator but not confirmed), low (speculative).",
            },
            "suggestions": {
                "type": "array",
                "items": {"type": "string"},
                "minItems": 1,
                "maxItems": 5,
                "description": "Actionable fix suggestions ordered by priority.",
            },
            "missing_evidence": {
                "type": "array",
                "items": {"type": "string"},
                "description": "What evidence we couldn't collect or verify — be honest about gaps.",
            },
            "evidence_citations": {
                "type": "array",
                "items": {"type": "string"},
                "description": "Which evidence items support the root cause (cite source_agent and summary).",
            },
        },
        "required": ["root_cause", "confidence", "suggestions"],
    }

    async def _synthesize(
        self, evidence: EvidencePool, context: dict, component: str,
        problem_type: str, feedback: str | None = None,
    ) -> tuple[str, list[str], str]:
        feedback_section = (
            f"\n\n=== Previous validation feedback ===\n{feedback}\nPlease address the issues above."
            if feedback else ""
        )

        # Use tool_use to force structured output
        tools = [{
            "name": "report_diagnosis",
            "description": "Submit the final diagnosis report with structured fields. You MUST call this tool to conclude.",
            "input_schema": self.SYNTHESIS_TOOL_SCHEMA,
        }]

        self._llm_calls += 1
        resp = self.client.messages.create(
            model=self.model,
            max_tokens=1024,
            temperature=0.2,
            messages=[{"role": "user", "content": f"""You are a diagnostic synthesiser. Review the evidence and produce a structured diagnosis.

Component: {component} — Problem: {problem_type}

=== Evidence Collected ===
{evidence.summary()}

=== Context ===
{json.dumps({k: v for k, v in context.items() if k != "topology"}, default=str)}{feedback_section}

IMPORTANT: Call the report_diagnosis tool with your structured conclusion. Every field is required.
- root_cause: MUST reference specific evidence (source_agent names, log lines, metrics).
- confidence: "high" only if multiple independent sources agree. "medium" if strong indicator. "low" if speculative.
- suggestions: actionable, specific. Include commands or config changes where possible.
- missing_evidence: be honest about what you couldn't verify."""}],
            tools=tools,
            tool_choice={"type": "tool", "name": "report_diagnosis"},
        )

        # Extract structured output from tool_use block
        tool_uses = [b for b in resp.content if b.type == "tool_use"]
        if tool_uses:
            try:
                args = tool_uses[0].input
                root_cause = args.get("root_cause", "")
                confidence = args.get("confidence", "medium")
                suggestions = args.get("suggestions", [])
                if root_cause and suggestions:
                    logger.info("Synthesis extracted structured output: confidence=%s, %d suggestions",
                               confidence, len(suggestions))
                    return root_cause, suggestions, confidence
            except Exception:
                logger.warning("Failed to parse structured synthesis output", exc_info=True)

        # Fallback: try to extract from text
        text_parts = [b.text for b in resp.content if b.type == "text"]
        text = "\n".join(text_parts) if text_parts else str(resp.content)
        logger.warning("Synthesis fell back to text parsing")

        root_cause, confidence, suggestions = "", "medium", []
        for line in text.split("\n"):
            s = line.strip()
            if s.upper().startswith("ROOT_CAUSE:"):
                root_cause = s.split(":", 1)[1].strip()
            elif s.upper().startswith("CONFIDENCE:"):
                confidence = s.split(":", 1)[1].strip().lower()
            elif s.startswith("- ") and root_cause:
                suggestions.append(s[2:])
        if not root_cause:
            root_cause = text[:300]
        if not suggestions:
            suggestions = ["Review the evidence and re-run diagnosis"]
        return root_cause, suggestions, confidence

    # ------------------------------------------------------------------
    # DB persistence (optional — gracefully skipped if MySQL unreachable)
    # ------------------------------------------------------------------

    async def _write_diagnosis(
        self, report: DiagnosisReport, context: dict, component: str,
        problem_type: str, kb_matched: bool = False, kb_match_phase: str = "",
        error_msg: str = "",
    ) -> None:
        if not self._db:
            return
        try:
            await self._db.insert_diagnosis(
                event_id=report.event_id,
                component=component,
                problem_type=problem_type,
                cluster_id=context.get("cluster_id", ""),
                region=context.get("region", ""),
                version=context.get("version", ""),
                root_cause=report.root_cause,
                confidence=report.confidence,
                suggestions=report.suggestions,
                evidence_count=len(report.evidence_summary),
                kb_matched=kb_matched,
                kb_match_phase=kb_match_phase,
                phases_run=report.phases_run,
                duration_ms=int(report.duration_ms),
                llm_calls=self._llm_calls,
                error_msg=error_msg,
            )
            # Upsert cluster profile
            if context.get("cluster_id"):
                await self._db.upsert_cluster(
                    cluster_id=context.get("cluster_id", ""),
                    component=component,
                    version=context.get("version", ""),
                    kb_hit=kb_matched,
                    duration_ms=int(report.duration_ms),
                )
        except Exception:
            logger.debug("DB write skipped", exc_info=True)

    async def _log_step(self, phase: str, tool_name: str = "", action: str = "",
                         status: str = "success", duration_ms: int = 0,
                         summary: str = "", error_detail: str = "") -> None:
        if not self._db:
            return
        self._step_order += 1
        try:
            await self._db.insert_strategy_log(
                event_id=self._event_id,
                phase=phase, tool_name=tool_name, action=action,
                status=status, duration_ms=duration_ms, summary=summary,
                error_detail=error_detail, step_order=self._step_order,
            )
        except Exception:
            logger.debug("DB step log skipped", exc_info=True)

    # ------------------------------------------------------------------
    # KB helpers
    # ------------------------------------------------------------------

    def _kb_match(self, context: dict) -> dict | None:
        """Phase 1: semantic search via ChromaDB + BM25 RRF fusion.

        Users describe problems in natural language (e.g. "Flink任务一直挂"),
        which doesn't match exact MD5 fingerprints. We use the hybrid retriever
        (semantic + BM25 + RRF) to find historically similar cases.

        Skip if embedder is using fallback (hash-based vectors are not
        semantically meaningful).
        """
        try:
            # Skip if embedder has no API key (fallback hash vectors are useless)
            if self.kb.embedder.api_key is None:
                return None

            query_parts = [
                context.get("component", ""),
                context.get("problem", ""),
                context.get("detail", ""),
                context.get("problem_desc", ""),
            ]
            query = " ".join(p for p in query_parts if p)
            if not query.strip():
                return None

            results = self.kb.search(query, n=3)
            for r in results:
                # Use RRF fusion_score when available (cross-method consensus),
                # fall back to 1-distance for pure semantic results
                score = r.get("fusion_score", None)
                if score is not None:
                    matched = score > 0.01  # RRF: non-trivial consensus
                else:
                    matched = r.get("distance", 1.0) < 0.3  # ChromaDB cosine

                if matched:
                    meta = r.get("metadata", {})
                    return {
                        "root_cause": meta.get("root_cause", r.get("document", "")[:200]),
                        "suggestions": (
                            meta.get("suggestions", "").split(",")
                            if isinstance(meta.get("suggestions"), str)
                            else meta.get("suggestions", [])
                        ),
                    }
        except Exception:
            logger.warning("KB semantic match failed", exc_info=True)
        return None

    def _kb_evidence_match(self, context: dict, evidence: EvidencePool) -> dict | None:
        component = context.get("component", "")
        version = context.get("version", "")
        for ev in evidence.all():
            for kw in self._error_keywords:
                if kw in (ev.detail or ""):
                    try:
                        hit = self.kb.fingerprint_match(component, kw, version)
                        if hit:
                            return hit
                    except Exception:
                        logger.warning("KB evidence match inner failed", exc_info=True)
                        continue
        return None

    def _kb_index(self, root_cause, suggestions, evidence, context):
        try:
            component = context.get("component", "unknown")
            version = context.get("version", "")
            error = root_cause[:80]
            for ev in evidence.all():
                for kw in self._error_keywords:
                    if kw in (ev.detail or ""):
                        error = kw
                        break
            self.kb.add_case(component, error, version, root_cause, suggestions)
        except Exception:
            logger.warning("KB indexing failed", exc_info=True)

    async def _emit(self, msg: str) -> None:
        if self.on_event:
            r = self.on_event(msg)
            if asyncio.iscoroutine(r):
                await r
