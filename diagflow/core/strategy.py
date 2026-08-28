"""
Strategy — YAML-driven diagnostic plan.

A Strategy defines WHAT to collect (which logs, metrics, configs) and in what
order. It does NOT decide how to interpret the evidence — that's the Agent's
job. This separation lets ops engineers add new diagnostic flows by editing
YAML, without touching code.

Strategy file resolution order:
  1. data/strategies/{component}_{problem_type}.yaml  (most specific)
  2. data/strategies/{component}_default.yaml         (component fallback)
  3. built-in default                                 (last resort)

Runtime semantics:
  - Steps with the same `priority` run in parallel (asyncio.gather)
  - `priority: 0` runs first, then 1, 2, ...
  - `fingerprint_match` at priority 0 short-circuits: if KB hits, skip the rest
  - `{{ context.xxx }}` template variables are resolved from user input at runtime
  - `tool_call` steps don't invoke the LLM — pure deterministic execution
"""

from __future__ import annotations

import re
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import yaml


# ---------------------------------------------------------------------------
# Step definition
# ---------------------------------------------------------------------------

@dataclass
class StrategyStep:
    """A single step in a diagnostic strategy.

    Supports conditional execution via ``if_decision`` + ``llm_decide`` steps:

    - ``action: llm_decide`` — calls LLM once with the evidence collected so far,
      returns a structured decision (e.g. "other_component" vs "flink_only").
    - ``if_decision: "value"`` — step only runs if a prior llm_decide step chose
      that value.

    This replaces brittle keyword matching with LLM judgment.
    """
    action: str                 # "fingerprint_match" | "tool_call" | "llm_decide"
    description: str = ""
    tool: str = ""              # only for action=tool_call
    params: dict[str, Any] = field(default_factory=dict)
    priority: int = 10
    # llm_decide fields
    decide_prompt: str = ""     # what to ask the LLM
    decide_choices: list[dict] = field(default_factory=list)  # [{value, description}]
    # conditional execution (applies to tool_call steps)
    if_decision: str = ""       # only run if llm_decide returned this value

    def should_run(self, current_decision: str = "") -> bool:
        """Check whether this step should execute given the LLM's decision."""
        if not self.if_decision:
            return True  # no condition → always run
        return current_decision == self.if_decision

    def render_params(self, context: dict[str, Any]) -> dict[str, Any]:
        """Resolve {{ context.xxx }} template variables from context."""
        rendered: dict[str, Any] = {}
        for k, v in self.params.items():
            rendered[k] = self._render_value(v, context)
        return rendered

    @staticmethod
    def _render_value(value: Any, context: dict[str, Any]) -> Any:
        if isinstance(value, str):
            # {{ context.xxx }} → context["xxx"]
            def replace(match: re.Match) -> str:
                path = match.group(1).strip()
                if path.startswith("context."):
                    key = path[len("context."):]
                    return str(context.get(key, ""))
                return match.group(0)

            return re.sub(r"\{\{\s*(context\.\w+)\s*\}\}", replace, value)
        if isinstance(value, dict):
            return {k: StrategyStep._render_value(v, context) for k, v in value.items()}
        if isinstance(value, list):
            return [StrategyStep._render_value(v, context) for v in value]
        return value


# ---------------------------------------------------------------------------
# Strategy
# ---------------------------------------------------------------------------

@dataclass
class Strategy:
    """A complete diagnostic strategy for one (component, problem_type)."""
    component: str
    problem_type: str
    version: str = ""
    steps: list[StrategyStep] = field(default_factory=list)
    knowledge_base: dict[str, Any] = field(default_factory=dict)
    validation: dict[str, Any] = field(default_factory=dict)
    output: dict[str, Any] = field(default_factory=dict)

    def group_by_priority(self) -> list[list[StrategyStep]]:
        """Group steps by priority for parallel execution.

        Returns a list of batches; each batch can run in parallel.
        """
        if not self.steps:
            return []
        sorted_steps = sorted(self.steps, key=lambda s: s.priority)
        batches: list[list[StrategyStep]] = []
        current_batch: list[StrategyStep] = []
        current_priority: int | None = None
        for step in sorted_steps:
            if current_priority is None or step.priority == current_priority:
                current_batch.append(step)
                current_priority = step.priority
            else:
                batches.append(current_batch)
                current_batch = [step]
                current_priority = step.priority
        if current_batch:
            batches.append(current_batch)
        return batches

    def build_task_prompt(self, context: dict[str, Any]) -> str:
        """Build the task prompt for the Agent's free-exploration phase.

        This is what the Agent sees in Phase 3 — it tells the LLM what's known
        and what to figure out.
        """
        return (
            f"Diagnose a {self.component} issue.\n"
            f"Problem: {context.get('problem', 'unknown')}\n"
            f"Cluster: {context.get('cluster_id', 'unknown')}\n"
            f"Region: {context.get('region', 'unknown')}\n"
            f"Version: {context.get('version', 'unknown')}\n"
            f"Detail: {context.get('detail', '')}\n\n"
            "Strategy-driven evidence collection has already run. Analyze the "
            "evidence in the pool, form a root cause hypothesis, and use "
            "deepwiki_query to verify if this is a known bug in the component's "
            "version. Then produce a structured conclusion."
        )


# ---------------------------------------------------------------------------
# Loader
# ---------------------------------------------------------------------------

def load_strategy(
    component: str,
    problem_type: str,
    strategies_dir: str = "",
) -> Strategy:
    """Load a strategy from YAML files.

    Resolution order:
      1. {component}_{problem_type}.yaml
      2. {component}_default.yaml
      3. built-in default
    """
    if strategies_dir:
        search_dir = Path(strategies_dir)
    else:
        # Default: data/strategies/ relative to project root
        project_root = Path(__file__).parent.parent.parent
        search_dir = project_root / "data" / "strategies"

    candidates = [
        search_dir / f"{component}_{problem_type}.yaml",
        search_dir / f"{component}_default.yaml",
    ]

    for candidate in candidates:
        if candidate.exists():
            return _parse_strategy_file(candidate, component, problem_type)

    return _default_strategy(component, problem_type)


def _parse_strategy_file(path: Path, component: str, problem_type: str) -> Strategy:
    """Parse a YAML strategy file into a Strategy object."""
    with open(path) as f:
        data = yaml.safe_load(f) or {}

    steps: list[StrategyStep] = []
    for step_data in data.get("steps", []):
        steps.append(StrategyStep(
            action=step_data.get("action", "tool_call"),
            description=step_data.get("description", ""),
            tool=step_data.get("tool", ""),
            params=step_data.get("params", {}),
            priority=step_data.get("priority", 10),
            decide_prompt=step_data.get("decide_prompt", ""),
            decide_choices=step_data.get("decide_choices", []),
            if_decision=step_data.get("if_decision", ""),
        ))

    return Strategy(
        component=data.get("component", component),
        problem_type=data.get("problem_type", problem_type),
        version=data.get("version", ""),
        steps=steps,
        knowledge_base=data.get("knowledge_base", {}),
        validation=data.get("validation", {}),
        output=data.get("output", {}),
    )


def _default_strategy(component: str, problem_type: str) -> Strategy:
    """Built-in fallback when no YAML file matches."""
    return Strategy(
        component=component,
        problem_type=problem_type,
        steps=[
            StrategyStep(
                action="fingerprint_match",
                description="Check known issues first",
                priority=0,
            ),
            StrategyStep(
                action="tool_call",
                tool="query_node_log",
                description="Scan for errors in main log",
                params={"log_path": "jobmanager.log", "keywords": "ERROR,FATAL"},
                priority=1,
            ),
            StrategyStep(
                action="tool_call",
                tool="query_metrics",
                description="Check resource metrics",
                params={},
                priority=2,
            ),
        ],
        knowledge_base={"fingerprint": True, "semantic_search": True, "bm25_search": True},
        validation={"min_evidence_count": 1},
        output={"suggestions_min": 2, "suggestions_max": 5},
    )
