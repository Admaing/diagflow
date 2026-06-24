"""Tool: read component configuration files."""

import asyncio
from diagflow.core.tool import Tool, ToolResult


def make_config_reader_tool(cluster):
    """Read configuration files from the cluster."""
    async def execute(config_path: str) -> ToolResult:
        try:
            data = cluster.get_config(config_path)
            if asyncio.iscoroutine(data):
                data = await data
            return ToolResult.ok(f"=== {config_path} ===\n{data}")
        except Exception as exc:
            return ToolResult.failed(str(exc))

    return Tool(
        name="read_config",
        description="Read a component's configuration file. Use this to check resource settings, timeouts, and tunables.",
        parameters={
            "config_path": {
                "type": "string",
                "description": "Path to config file, e.g. 'flink-conf.yaml' or 'hdfs-site.xml'",
                "required": True,
            },
        },
        fn=execute,
    )
