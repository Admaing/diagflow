"""
Agent — the single ReAct core of DiagFlow v2.

One Agent, one ReAct loop. No multi-agent orchestration.

Two entry points:
  - run(task, evidence_so_far): the free-exploration ReAct loop (Phase 3).
    Used when the Agent should reason about evidence, call DeepWiki, and
    synthesize a conclusion.
  - synthesize(evidence, context, feedback): single-shot LLM call to turn
    an EvidencePool into a structured conclusion. No ReAct loop.

Context window management is real here: when estimated tokens exceed the
limit, older tool results are summarised away to keep the loop running.
"""

from __future__ import annotations

import json
from typing import Any, Callable

from .llm import LLMClient, LLMResponse
from .memory import EvidencePool, Evidence
from .tool import Tool, ToolRegistry, ToolResult


class Agent:
    """Single ReAct Agent — the only LLM-driven decision maker in v2."""

    def __init__(
        self,
        llm: LLMClient,
        tool_registry: ToolRegistry,
        max_steps: int = 12,
        context_window: int = 64_000,
        on_step: Callable[[str], None] | None = None,
        topology: str = "",
    ):
        self.llm = llm
        self.tool_registry = tool_registry
        self.max_steps = max_steps
        self.context_window = context_window
        self.on_step = on_step
        self._topology_text = topology

    def _log(self, msg: str) -> None:
        if self.on_step:
            self.on_step(msg)

    # ------------------------------------------------------------------
    # ReAct loop (Phase 3: free exploration + DeepWiki verification)
    # ------------------------------------------------------------------

    async def run(
        self,
        task: str,
        evidence_so_far: EvidencePool | None = None,
        max_steps: int | None = None,
    ) -> str:
        """Execute the ReAct loop with the evidence pool as background context.

        The Agent is told what evidence has already been collected (Phase 2),
        so it doesn't repeat those queries. Its job: interpret evidence,
        call DeepWiki to verify known bugs, and produce a conclusion.
        """
        steps = max_steps or self.max_steps
        self._log(f"[agent] starting ReAct, max_steps={steps}")

        # System prompt: role + available tools + evidence summary
        system_prompt = self._build_system_prompt(evidence_so_far)

        # OpenAI-style messages
        messages: list[dict[str, Any]] = [{"role": "user", "content": task}]
        tool_defs = [t.to_llm_definition() for t in self.tool_registry.list()]

        for step in range(1, steps + 1):
            self._log(f"[agent] ReAct step {step}/{steps}")

            response = await self.llm.generate(
                messages=messages,
                tools=tool_defs or None,
                system=system_prompt,
            )

            if response.tool_calls:
                # --- Act: execute each tool call ---
                messages.append({
                    "role": "assistant",
                    "content": response.content or "",
                    "tool_calls": [
                        {
                            "id": tc.id,
                            "type": "function",
                            "function": {
                                "name": tc.name,
                                "arguments": json.dumps(tc.arguments),
                            },
                        }
                        for tc in response.tool_calls
                    ],
                })

                for tc in response.tool_calls:
                    tool = self.tool_registry._tools.get(tc.name)
                    if tool is None:
                        result_msg = f"[Unknown tool: {tc.name}]"
                    else:
                        self._log(f"[agent] calling {tc.name}({json.dumps(tc.arguments)[:200]})")
                        result = await tool.execute(**tc.arguments)
                        result_msg = result.to_llm_message()
                        self._log(f"[agent] tool result: {result_msg[:200]}")
                        if result.success:
                            evidence_so_far = evidence_so_far or EvidencePool()
                            conf = ToolResult.compute_confidence(
                                tc.name, str(result.data or ""), result.success
                            )
                            evidence_so_far.add(Evidence(
                                source_agent="react",
                                category=f"tool:{tc.name}",
                                summary=result_msg[:200],
                                detail=result_msg,
                                confidence=conf,
                            ))

                    messages.append({
                        "role": "tool",
                        "tool_call_id": tc.id,
                        "content": result_msg,
                    })

                # Context window management: trim if messages too large
                messages = self._trim_messages(messages)
            else:
                # --- Final answer ---
                self._log("[agent] ReAct complete")
                return response.content or "(no response)"

        return (
            "[agent] Maximum ReAct steps reached without a final answer. "
            "Partial findings are in the evidence pool."
        )

    # ------------------------------------------------------------------
    # Single-shot synthesis (Phase 4: turn evidence into structured conclusion)
    # ------------------------------------------------------------------

    async def synthesize(
        self,
        evidence: EvidencePool,
        context: dict[str, Any],
        component: str,
        problem_type: str,
        feedback: str | None = None,
    ) -> tuple[str, list[str], str]:
        """Turn the evidence pool into a structured conclusion.

        Returns (root_cause, suggestions, confidence). No ReAct loop —
        this is a single LLM call.
        """
        evidence_text = evidence.summary()
        feedback_section = (
            f"=== Previous validation feedback to address ===\n{feedback}"
            if feedback else ""
        )

        prompt = f"""You are a diagnostic synthesiser for a big data platform.

Component: {component}
Problem: {problem_type}

=== Evidence collected ===
{evidence_text}

=== Context ===
{json.dumps(context, ensure_ascii=False, default=str)}

{feedback_section}

Based on ALL of the above, produce a diagnosis:

ROOT_CAUSE: <one specific sentence — not "system error" but "TaskManager heap OOM">
CONFIDENCE: <high|medium|low>
SUGGESTIONS:
- <actionable suggestion 1>
- <actionable suggestion 2>
- <actionable suggestion 3>"""

        response = await self.llm.generate(
            messages=[{"role": "user", "content": prompt}],
        )

        return self._parse_conclusion(response.content or "")

    # ------------------------------------------------------------------
    # Helpers
    # ------------------------------------------------------------------

    def _build_system_prompt(self, evidence: EvidencePool | None) -> str:
        """Build the system prompt with evidence summary and tool guidance."""
        evidence_section = evidence.summary() if evidence else "(no prior evidence)"

        # Service topology (if provided for this component)
        topo = f"""

## Service Deployment Topology (known — use this to target nodes)
The cluster has master nodes and core nodes. Below is the EXACT mapping of
services to node roles, log paths, and process names. Use this to go directly
to the right node and right log file — do NOT blindly try all nodes.

{self._topology_text}
""" if self._topology_text else ""

        return f"""You are a senior SRE diagnosing a big data platform issue.

## Evidence Already Collected (Phase 2 — do NOT re-query these)
{evidence_section}
{topo}
## Your Job (Phase 3 — ReAct)
1. **For Flink/Spark**: First call `query_yarn` to check YARN. If 0 apps, the cluster
   uses STANDALONE mode — skip YARN and go directly to node-level checks.
2. **If GetLogs returns FileExist=false**: The log paths in the topology may be wrong
   for this cluster. Call `call_umr_agent` Action=FindLogs(Service="flink") to find
   actual log paths, then GetLogs with those paths.
3. **For services with fixed topology** (HDFS, YARN daemons, Kafka, HBase):
   Use the topology above to go DIRECTLY to the correct node and log path.
3. After getting logs, call `deepwiki_query` if error patterns indicate a
   known bug. Verify the component version matches the query.
4. Produce a structured conclusion when done.

## Tool Guidance
- `ssh_exec`: Execute find/grep/tail/curl on cluster nodes via SSH. Use this FIRST
  to find log paths and tail errors — faster than deploying new umrAgent Actions.
  - Find logs: ssh_exec(node, "find /data /var/log /opt -name '*flink*.log' 2>/dev/null | head -20")
  - Tail errors: ssh_exec(node, "tail -200 <logfile> | grep -E 'ERROR|Exception'")
- `query_yarn`: Direct YARN RM query. If 0 apps, cluster uses standalone mode.
- `call_umr_agent`: Action=GetLogs, CheckProcess, GetBaseInfo as fallback.
  Action=CheckProcess(ProcessName) for process checks.
- `deepwiki_query`: Component → repo: flink→apache/flink, hdfs/yarn→apache/hadoop.
- `query_metrics`: monitoring data if needed."""

    def _trim_messages(self, messages: list[dict[str, Any]]) -> list[dict[str, Any]]:
        """Keep messages within the context window.

        Strategy: if estimated tokens exceed 80% of the limit, drop the oldest
        tool results (they're summarised in the evidence pool anyway).
        """
        est_tokens = sum(
            len(str(m.get("content", ""))) // 3 for m in messages
        )
        if est_tokens < self.context_window * 0.8:
            return messages

        # Drop oldest tool result messages (keep the first user task + recent turns)
        trimmed: list[dict[str, Any]] = []
        skipped = 0
        for m in messages:
            if m.get("role") == "tool" and skipped < 2:
                skipped += 1
                continue
            trimmed.append(m)
        self._log(f"[agent] trimmed {skipped} old tool results (ctx window mgmt)")
        return trimmed if trimmed else messages

    @staticmethod
    def _parse_conclusion(text: str) -> tuple[str, list[str], str]:
        """Parse ROOT_CAUSE/CONFIDENCE/SUGGESTIONS from LLM output."""
        root_cause = ""
        confidence = "medium"
        suggestions: list[str] = []

        for line in text.split("\n"):
            line = line.strip()
            if line.upper().startswith("ROOT_CAUSE:"):
                root_cause = line.split(":", 1)[1].strip()
            elif line.upper().startswith("CONFIDENCE:"):
                confidence = line.split(":", 1)[1].strip().lower()
            elif line.startswith("- ") and root_cause:
                suggestions.append(line[2:])

        if not root_cause:
            root_cause = text[:300]
        return root_cause, suggestions, confidence
