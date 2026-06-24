"""Tool: query monitoring metrics."""

import asyncio
from diagflow.core.tool import Tool, ToolResult


def make_metrics_tool(cluster):
    """Query cluster monitoring metrics."""
    async def execute(
        component: str = "all",
        metric_types: str = "",
    ) -> ToolResult:
        try:
            metric_names = [m.strip() for m in metric_types.split(",") if m.strip()] if metric_types else None
            data = cluster.get_metrics(metric_names)
            if asyncio.iscoroutine(data):
                data = await data
            return ToolResult.ok(f"Metrics for {component}:\n{data}")
        except Exception as exc:
            return ToolResult.failed(str(exc))

    return Tool(
        name="query_metrics",
        description=(
            "Query monitoring metrics for a component. "
            "Available dimensions: heap_usage_percent, checkpoint_failure_rate, "
            "gc_pause_ms_avg, input_rate_mbps, backpressure_level."
        ),
        parameters={
            "component": {
                "type": "string",
                "description": "Component name (e.g. 'flink', 'hdfs', 'yarn')",
                "default": "all",
            },
            "metric_types": {
                "type": "string",
                "description": "Comma-separated metric names. Empty = all available",
                "default": "",
            },
        },
        fn=execute,
    )
