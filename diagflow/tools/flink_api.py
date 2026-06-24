"""Tool: query Flink job status via REST API (simulated)."""

from diagflow.core.tool import Tool, ToolResult


def make_flink_status_tool(cluster):
    """Query Flink job status and checkpoint statistics."""
    async def execute(job_id: str) -> ToolResult:
        try:
            metrics = cluster.get_metrics()
            c = cluster.context

            # Simulate a Flink REST API response
            import json
            data = {
                "jid": job_id,
                "name": f"Flink Job on {c.get('cluster_id', 'unknown')}",
                "state": c.get("problem", "UNKNOWN"),
                "duration": 3600000,
                "checkpoint_stats": {
                    "total": 50,
                    "failed": cluster.metrics.get("checkpoint_failure_rate", 0),
                    "in_progress": 0,
                    "last_checkpoint_status": "FAILED",
                },
                "backpressure": cluster.metrics.get("backpressure_level", "OK"),
                "nodes": [
                    {
                        "id": "node_1",
                        "status": "RUNNING",
                        "metrics": {
                            "heap_usage": cluster.metrics.get("heap_usage_percent", 50),
                        },
                    }
                ],
            }
            return ToolResult.ok(json.dumps(data, indent=2))
        except Exception as exc:
            return ToolResult.failed(str(exc))

    return Tool(
        name="query_flink_status",
        description="Query Flink job status via REST API. Returns job state, checkpoint stats, and backpressure info.",
        parameters={
            "job_id": {
                "type": "string",
                "description": "The Flink job ID to query",
                "required": True,
            },
        },
        fn=execute,
    )
