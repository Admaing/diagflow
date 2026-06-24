"""Tool: fingerprint matching — fast-path for known issues.

Inspired by Duwu's Troubleshooter, we generate a fingerprint from the
(component, error_pattern, version) tuple and check the knowledge base
for an exact match before invoking the full AI pipeline.
"""

from diagflow.core.tool import Tool, ToolResult


def make_fingerprint_match_tool(cluster):
    """Check if this issue matches a known pattern in the knowledge base."""
    async def execute(
        component: str,
        error_pattern: str,
        version: str = "",
    ) -> ToolResult:
        import hashlib
        fp = hashlib.md5(
            f"{component}:{error_pattern}:{version}".encode()
        ).hexdigest()

        # Simulated knowledge base
        known_issues = {
            "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4": {
                "component": "flink",
                "pattern": "OutOfMemoryError",
                "root_cause": "TaskManager heap too small for input volume",
                "fix": "Increase taskmanager.memory.heap.size to 4096m or reduce parallelism",
                "cases": 3,
            },
        }

        match = known_issues.get(fp)
        if match:
            return ToolResult.ok(
                f"✅ Known issue found (confidence: high, {match['cases']} previous cases):\n"
                f"  Root cause: {match['root_cause']}\n"
                f"  Fix: {match['fix']}"
            )
        return ToolResult.ok(
            f"No exact fingerprint match for {component}/{error_pattern} v{version}.\n"
            "Proceeding with full AI diagnosis."
        )

    return Tool(
        name="fingerprint_match",
        description="Fast-path: check if this error matches a known issue pattern. Use this FIRST before deeper investigation.",
        parameters={
            "component": {
                "type": "string",
                "description": "Component name: flink, hdfs, yarn, airflow",
                "required": True,
            },
            "error_pattern": {
                "type": "string",
                "description": "Key error keyword, e.g. 'OutOfMemoryError', 'NoSpaceLeft', 'CheckpointExpired'",
                "required": True,
            },
            "version": {
                "type": "string",
                "description": "Component version for version-specific known issues",
                "default": "",
            },
        },
        fn=execute,
    )
