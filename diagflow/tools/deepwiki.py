"""
DeepWiki MCP tool — query open-source component repositories for known issues.

Uses the official MCP Python SDK to talk to DeepWiki's remote MCP server
(https://mcp.deepwiki.com/mcp) over Streamable HTTP. Public repos need no auth.

This is the killer differentiator of DiagFlow: when the Agent identifies a
specific error pattern in logs (e.g. "OutOfMemoryError" in Flink 1.14.3),
it dynamically constructs a DeepWiki query to check whether this is a known
bug in the component's version — something a pure log/metric analysis can't do.

Why MCP instead of REST:
  DeepWiki only exposes MCP protocol (JSON-RPC over Streamable HTTP), not a
  REST API. We use the Anthropic-maintained `mcp` Python SDK to handle the
  protocol — session handshake, tool listing, tool invocation.
"""

from __future__ import annotations

import os
from typing import Any

from diagflow.core.tool import Tool, ToolResult


# Component name → DeepWiki repo name mapping
# These are all public repos — no authentication needed
COMPONENT_TO_REPO: dict[str, str] = {
    "flink": "apache/flink",
    "hdfs": "apache/hadoop",
    "yarn": "apache/hadoop",
    "hadoop": "apache/hadoop",
    "kafka": "apache/kafka",
    "spark": "apache/spark",
    "hbase": "apache/hbase",
    "hive": "apache/hive",
    "airflow": "apache/airflow",
    "trino": "trinodb/trino",
    "dolphinscheduler": "apache/dolphinscheduler",
    "zookeeper": "apache/zookeeper",
}

DEEPWIKI_MCP_URL = os.environ.get("DEEPWIKI_MCP_URL", "https://mcp.deepwiki.com/mcp")


async def _call_deepwiki(repo: str, question: str) -> str:
    """Call DeepWiki MCP ask_question tool.

    Uses a fresh session per call — DeepWiki is a remote stateless MCP server,
    no need to persist sessions across calls.
    """
    # Import lazily so the SDK is only required when this tool is actually used
    from mcp import ClientSession
    from mcp.client.streamable_http import streamablehttp_client

    async with streamablehttp_client(DEEPWIKI_MCP_URL) as (read, write, _):
        async with ClientSession(read, write) as session:
            await session.initialize()
            result = await session.call_tool(
                "ask_question",
                {"repoName": repo, "question": question},
            )
            # result.content is a list of content blocks; concatenate text ones
            parts: list[str] = []
            for block in result.content:
                text = getattr(block, "text", None)
                if text:
                    parts.append(text)
            return "\n".join(parts) if parts else "(empty response from DeepWiki)"


def make_deepwiki_tool() -> Tool:
    """Create the DeepWiki query tool.

    The LLM constructs the question based on the specific error pattern it
    found in logs — this is why DeepWiki must be called from the ReAct loop,
    not from a static Strategy step.
    """

    async def execute(
        component: str,
        question: str,
        version: str = "",
    ) -> ToolResult:
        """Query DeepWiki for known issues in an open-source component.

        Args:
            component: Component name — flink, hdfs, yarn, kafka, spark, etc.
            question: Natural language question about known issues, bugs,
                     release notes. Be specific about the error pattern.
            version: Optional component version to scope the question.
        """
        repo = COMPONENT_TO_REPO.get(component.lower())
        if not repo:
            return ToolResult.failed(
                f"Unknown component '{component}'. "
                f"Supported: {list(COMPONENT_TO_REPO.keys())}"
            )

        # Scope the question by version if provided
        full_question = f"{question} (version: {version})" if version else question

        try:
            answer = await _call_deepwiki(repo, full_question)
            return ToolResult.ok(
                data=f"[DeepWiki · {repo}]\n{answer}",
                duration_ms=0,
            )
        except Exception as exc:
            return ToolResult.failed(
                f"DeepWiki query failed: {type(exc).__name__}: {exc}"
            )

    return Tool(
        name="deepwiki_query",
        description=(
            "Query DeepWiki for known issues, bugs, and release notes in "
            "open-source component repositories. Use this to verify if a "
            "diagnosed error matches a known bug in the component's version. "
            "Construct the question based on the SPECIFIC error pattern found "
            "in logs (e.g. 'TaskManager OutOfMemoryError Java heap space "
            "known issues'). Component → repo mapping: flink→apache/flink, "
            "hdfs/yarn/hadoop→apache/hadoop, kafka→apache/kafka, "
            "spark→apache/spark, hbase→apache/hbase, airflow→apache/airflow."
        ),
        parameters={
            "component": {
                "type": "string",
                "description": "Component name: flink, hdfs, yarn, kafka, spark, hbase, hive, airflow, trino, etc.",
                "required": True,
            },
            "question": {
                "type": "string",
                "description": "Natural language question about known issues. Be specific about the error pattern.",
                "required": True,
            },
            "version": {
                "type": "string",
                "description": "Component version to scope the query (e.g. '1.14.3')",
                "default": "",
            },
        },
        fn=execute,
        timeout_s=60,  # DeepWiki can be slow — give it room
    )
