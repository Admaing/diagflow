#!/usr/bin/env python3
"""
DiagFlow — AI-Powered Diagnostic Agent for Big Data Platforms.

Quick-start demo:
  # Diagnose a Flink OOM scenario
  python -m demo.run

  # List all scenarios
  python -m demo.run --list

  # Run a specific scenario
  python -m demo.run --scenario flink_checkpoint_fail

  # Run all scenarios in sequence
  python -m demo.run --all
"""

from __future__ import annotations

import argparse
import asyncio
import os
import sys
import time

# Add parent to path so we can import diagflow
sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

from diagflow.core.llm import LLMClient
from diagflow.core.orchestrator import Orchestrator
from diagflow.core.validator import ConclusionValidator
from diagflow.core.memory import EvidencePool
from diagflow.tools.registry import build_tool_registry
from diagflow.simulated.cluster import SimulatedCluster
from diagflow.simulated.scenarios import ALL_SCENARIOS
from diagflow.agents.supervisor import create_supervisor_agent
from diagflow.observability.event_tracker import EventTracker
from diagflow.observability.report import render_report

# Import specialist agents
from diagflow.agents.log_analyst import create_log_analyst_agent
from diagflow.agents.metrics_analyst import create_metrics_analyst_agent
from diagflow.agents.config_analyst import create_config_analyst_agent

# Workflows
from diagflow.workflows.flink_diagnosis import FlinkDiagnosticWorkflow


def setup_environment() -> str | None:
    """Check for API key and return it."""
    key = os.environ.get("DEEPSEEK_API_KEY") or os.environ.get("LLM_API_KEY")
    if not key:
        print("⚠️  DEEPSEEK_API_KEY not set. Set it to use LLM-powered diagnosis.")
        print("   export DEEPSEEK_API_KEY=sk-...")
        print("   Falling back to simulated diagnostic output for demo.\n")
    return key


async def diagnose_scenario(
    scenario_name: str,
    api_key: str | None,
    event_tracker: EventTracker | None = None,
) -> None:
    """Run a full diagnosis on one scenario."""
    print(f"\n{'='*60}")
    print(f"🔍 DIAGNOSIS: {scenario_name}")
    print(f"{'='*60}\n")

    # 1. Setup cluster with scenario data
    cluster = SimulatedCluster(scenario_name)
    print(cluster.summary())
    print()

    if event_tracker:
        event_tracker.log(f"Starting diagnosis for {scenario_name}")

    # 2. Build tools + agents
    tool_registry = build_tool_registry(cluster)

    if api_key:
        llm = LLMClient(api_keys=[api_key], model="deepseek-v4-flash")
    else:
        # Demo mode without API — use mock
        from diagflow.core.llm import LLMResponse, ToolCall

        class MockLLM:
            """Simple mock that returns canned responses for the demo.
            Demonstrates the framework without requiring API keys."""
            async def generate(self, messages, tools=None, system=None, **kwargs):
                # In demo mode, show what tools would be available
                if tools:
                    tool_names = [t["name"] for t in tools]
                    print(f"  📋 Available tools: {', '.join(tool_names)}")
                return LLMResponse(
                    content=f"[Demo Mode] Diagnosis would use the ReAct loop "
                            f"with {len(tools or [])} tools to analyze this scenario.\n\n"
                            f"Expected finding: {cluster.expected_root_cause}",
                )

        llm = MockLLM()  # type: ignore

    # 3. Create agents
    supervisor = create_supervisor_agent(llm, tool_registry)
    evidence_pool = EvidencePool()

    # 4. Build workflow
    strategy_path = os.path.join(
        os.path.dirname(os.path.dirname(__file__)),
        "data", "strategies", f"{cluster.context.get('component', 'flink')}_default.yaml",
    )
    workflow = FlinkDiagnosticWorkflow(strategy_path=strategy_path)

    # 5. Run diagnosis
    async def _step_log(msg: str) -> None:
        print(f"  🔄 {msg}")
        if event_tracker:
            event_tracker.log(msg)

    orchestrator = Orchestrator(
        llm=llm,  # type: ignore
        evidence_pool=evidence_pool,
        validator=ConclusionValidator(llm),  # type: ignore
        on_event=lambda msg: _step_log(msg),
    )
    orchestrator.register_supervisor("flink", supervisor)

    task_prompt = workflow.build_task_prompt(cluster.context)
    print(f"📝 Task: {task_prompt[:100]}...\n")

    # Build specialist agents for parallel execution
    specialists = [
        create_log_analyst_agent(llm, tool_registry),  # type: ignore
        create_metrics_analyst_agent(llm, tool_registry),  # type: ignore
        create_config_analyst_agent(llm, tool_registry),  # type: ignore
    ]

    start = time.monotonic()
    report = await orchestrator.diagnose(
        component=cluster.context.get("component", "flink"),
        problem_type=cluster.context.get("problem", "unknown"),
        context=cluster.context,
        specialist_agents=specialists if not api_key else None,  # skip specialists in demo
    )
    elapsed = time.monotonic() - start

    # 6. Output report
    print(f"\n{'='*60}")
    print("📊 DIAGNOSIS REPORT")
    print(f"{'='*60}\n")
    print(render_report(report))
    print(f"\n⏱️  Total time: {elapsed:.1f}s")
    print(f"📁 Trace: /tmp/diagflow/{report.event_id}/")

    # Verify against expected
    expected = cluster.expected_root_cause
    if expected:
        match = expected.lower() in report.root_cause.lower()
        print(f"\n{'✅' if match else '⚠️'} Expected root cause: '{expected}'")
        print(f"   {'✓ Match!' if match else '✗ Mismatch — check diagnosis quality'}")

    return report


async def main():
    parser = argparse.ArgumentParser(description="DiagFlow — AI-powered diagnostic agent")
    parser.add_argument("--scenario", default="flink_oom", help="Scenario to run")
    parser.add_argument("--list", action="store_true", help="List all scenarios")
    parser.add_argument("--all", action="store_true", help="Run all scenarios")
    args = parser.parse_args()

    if args.list:
        print("Available scenarios:")
        for name in ALL_SCENARIOS:
            print(f"  - {name}")
        return

    api_key = setup_environment()
    event_tracker = EventTracker(f"demo-{int(time.time())}") if api_key else None

    if args.all:
        for name in ALL_SCENARIOS:
            await diagnose_scenario(name, api_key, event_tracker)
            print("\n" + "=" * 60 + "\n")
    else:
        await diagnose_scenario(args.scenario, api_key, event_tracker)


if __name__ == "__main__":
    asyncio.run(main())
