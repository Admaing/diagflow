"""Central tool registry — all tools available to agents.

In production mode, node operations go through the umrAgent tool
(no direct SSH or file access). In demo mode, simulated data is used.
"""

from __future__ import annotations

from diagflow.core.tool import Tool, ToolRegistry

from .node_log import make_node_log_tool       # demo: simulated log data
from .umr_agent import make_umr_agent_tool     # prod: LLM constructs umrAgent calls
from .flink_api import make_flink_status_tool
from .metrics_api import make_metrics_tool
from .fingerprint import make_fingerprint_match_tool
from .config_reader import make_config_reader_tool
from .deepwiki import make_deepwiki_tool       # DeepWiki MCP — known-issue verification
from .yarn_query import make_yarn_query_tool   # prod: direct YARN RM API query via IPv6
from .remote_exec import make_remote_exec_tool # prod: SSH exec (temporary, → umrAgent later)


def build_tool_registry(cluster) -> ToolRegistry:
    """Create and populate the tool registry with all available tools.

    In PRODUCTION mode (cluster is RealCluster from diagflow.infra),
    the umr_agent tool is included — LLM constructs umrAgent Action calls
    and the tool executes them via signed HTTP to [ipv6]:65431.

    In DEMO mode (cluster is SimulatedCluster), node_log is used instead
    for compatibility.

    DeepWiki is always available — it's a public MCP service with no auth.
    """
    registry = ToolRegistry()

    # Always available
    tools: list[Tool] = [
        make_flink_status_tool(cluster),
        make_metrics_tool(cluster),
        make_fingerprint_match_tool(cluster),
        make_config_reader_tool(cluster),
        make_deepwiki_tool(),                   # ← new: known-issue verification
    ]

    # Node/log access: umrAgent in prod, simulated in demo
    if hasattr(cluster, '_agent') and cluster._agent is not None:
        # Production: LLM constructs umrAgent Action calls
        tools.append(make_umr_agent_tool(cluster))
        # Direct YARN RM query via IPv6 (no SSH/umrAgent needed)
        tools.append(make_yarn_query_tool(cluster))
        # SSH exec (temporary fast-path, will be replaced by umrAgent later)
        tools.append(make_remote_exec_tool(cluster))
    else:
        # Demo: simulated log data
        tools.append(make_node_log_tool(cluster))

    for tool in tools:
        registry.register(tool)

    return registry
