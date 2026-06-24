"""Tool: query node logs with keyword filtering."""

from diagflow.core.tool import Tool, ToolResult


def make_node_log_tool(cluster):
    """Create a tool that retrieves and filters node logs."""
    async def execute(log_path: str, keywords: str = "", max_lines: int = 50) -> ToolResult:
        try:
            content = cluster.get_node_log(log_path, keywords=keywords, max_lines=max_lines)
            return ToolResult.ok(
                data=content,
                duration_ms=0,
            )
        except Exception as exc:
            return ToolResult.failed(str(exc))

    return Tool(
        name="query_node_log",
        description="Query log files from cluster nodes. Supports keyword filtering. Use this to find error patterns in component logs.",
        parameters={
            "log_path": {
                "type": "string",
                "description": "Path to the log file, e.g. 'taskmanager-1.log' or 'jobmanager.log'",
                "required": True,
            },
            "keywords": {
                "type": "string",
                "description": "Comma-separated keywords to filter (e.g. 'ERROR,Exception,OOM'). Empty string returns all lines.",
                "default": "",
            },
            "max_lines": {
                "type": "integer",
                "description": "Maximum number of matching lines to return",
                "default": 50,
            },
        },
        fn=execute,
    )
