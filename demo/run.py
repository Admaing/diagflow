#!/usr/bin/env python3
"""DiagFlow v3 CLI demo / quick-test."""

import argparse, asyncio, os, sys
sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

from diagflow.core.diag_agent import DiagAgent
from diagflow.core.validator import ConclusionValidator
from diagflow.simulated.cluster import SimulatedCluster
from diagflow.simulated.scenarios import ALL_SCENARIOS
from diagflow.tools.v3tools import build_v3_tools
from diagflow.observability.report import render_report


async def diagnose_scenario(name, api_key):
    c = SimulatedCluster(name)
    print(f"\n{'='*50}\n🔍 {name}\n{'='*50}")
    print(c.summary())

    if not api_key:
        print(f"\n**预期根因**: {c.expected_root_cause}\n> 设置 DEEPSEEK_API_KEY 启用 LLM")
        return

    tools = build_v3_tools(c)
    agent = DiagAgent(
        api_key=api_key, model="deepseek-v4-flash",
        strategies_dir=os.path.join(
            os.path.dirname(os.path.dirname(__file__)),
            "data", "strategies",
        ),
        validator=ConclusionValidator.standalone(api_key),
        on_event=lambda m: print(f"  📋 {m}"),
    )
    agent.register_tools(tools)

    report = await agent.diagnose(
        component=c.context.get("component", "flink"),
        problem_type=c.context.get("problem", "unknown"),
        context=dict(c.context),
    )
    print(f"\n{render_report(report)}")

    match = c.expected_root_cause.lower() in report.root_cause.lower()
    print(f"\n{'✅' if match else '⚠️'} Expected: {c.expected_root_cause}")


async def main():
    p = argparse.ArgumentParser(description="DiagFlow v3")
    p.add_argument("--scenario", default="flink_oom")
    p.add_argument("--list", action="store_true")
    p.add_argument("--all", action="store_true")
    args = p.parse_args()

    if args.list:
        print("scenarios:", list(ALL_SCENARIOS)); return

    api_key = os.environ.get("DEEPSEEK_API_KEY") or ""
    if not api_key:
        print("⚡ DEEPSEEK_API_KEY not set — mock mode")

    if args.all:
        for k in ALL_SCENARIOS:
            await diagnose_scenario(k, api_key)
    else:
        await diagnose_scenario(args.scenario, api_key)


if __name__ == "__main__":
    asyncio.run(main())
