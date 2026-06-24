"""
UmrAgent tool — lets the LLM construct calls to umrAgent on cluster nodes.

This is the NEW design pattern (replacing node_log.py in production):
  1. LLM decides: which node, which umrAgent Action, which parameters
  2. Tool executes: translates LLM's request into a signed HTTP call
     to http://[ipv6]:65431/?Action=...&Signature=...

NO SSH. NO direct MySQL. The LLM never sees the signing key, the node
IPv6 address, or any DB credentials. The Tool layer abstracts all of that.

Supported umrAgent Actions (LLM can call any of these):
  - GetLogs:     fetch log files with keyword filtering
  - GetAppList:  list running applications
  - GetBaseInfo: node health and system info
  - GetIOWait:   disk I/O wait metrics
  - CheckProcess: check if a specific process is running
  - RunCmd:      (disabled for safety — read-only operations only)
"""

from __future__ import annotations

from typing import Any

from diagflow.core.tool import Tool, ToolResult

# Actions the LLM is allowed to call (read-only only, no RunCmd)
ALLOWED_ACTIONS = {
    "GetLogs": {
        "description": "Fetch log lines with keyword filter. If FileExist=false, call FindLogs first to discover actual paths. ALWAYS use Keywords='ERROR,FATAL,Exception'.",
        "params": {
            "Path": "string", "Keywords": "string", "MaxLines": "integer", "Since": "string",
        },
    },
    "FindLogs": {
        "description": "Search the filesystem for log files of a service. Use this when GetLogs returns FileExist=false — the actual log paths may differ from the topology.",
        "params": {
            "Service": "string",      # "flink", "hadoop", "hbase", "kafka"
            "MaxResults": "integer",  # max paths to return (default 20)
        },
    },
    "GetAppList": {
        "description": "List running YARN applications (call on master node)",
        "params": {},
    },
    "GetYarnAppNodes": {
        "description": "Find which cluster nodes a YARN application runs on. Use AppName to filter by name (e.g. 'Flink'). Returns app details + list of node hostnames. Call this FIRST to find target nodes before GetLogs.",
        "params": {
            "AppName": "string",   # filter by app name (substring match)
            "AppId": "string",     # or exact app ID
        },
    },
    "GetBaseInfo": {
        "description": "Get node health and system info (CPU, memory, disk)",
        "params": {},
    },
    "GetIOWait": {
        "description": "Get disk I/O wait percentage",
        "params": {},
    },
    "CheckProcess": {
        "description": "Check if a specific process is running",
        "params": {
            "ProcessName": "string",  # e.g. "TaskManagerRunner", "DataNode"
        },
    },
}


def make_umr_agent_tool(cluster) -> Tool:
    """Create the umrAgent tool — LLM constructs Action + params.

    The tool description tells the LLM exactly what Actions are available
    and what parameters each accepts. The LLM picks the right Action
    and fills in the params. The tool handles the rest (lookups node
    by name, signs the request, calls umrAgent).
    """

    # Build the description with all valid actions
    actions_desc = "\n".join(
        f"  • {name}: {info['description']}"
        + (f" — params: {list(info['params'].keys())}" if info['params'] else "")
        for name, info in ALLOWED_ACTIONS.items()
    )

    async def execute(
        node_name: str,
        action: str,
        params: dict[str, Any] | None = None,
    ) -> ToolResult:
        """Execute an umrAgent Action on a node.

        Args:
            node_name: Node identifier, e.g. "c-xxx-master1" or "master1"
            action: umrAgent Action name (must be in ALLOWED_ACTIONS)
            params: Action-specific parameters (see description)
        """
        # Validate action
        if action not in ALLOWED_ACTIONS:
            return ToolResult.failed(
                f"Action '{action}' not allowed. Valid: {list(ALLOWED_ACTIONS.keys())}"
            )

        try:
            # For production clusters (RealCluster), this delegates to the
            # umrAgent adapter. For demo (SimulatedCluster), falls through
            # to node_log.
            if hasattr(cluster, '_agent') and cluster._agent is not None:
                # Production: resolve node info from Go API, sign, call
                await cluster._ensure_node_data()
                node = cluster._find_node(node_name)
                if not node:
                    return ToolResult.failed(
                        f"Node '{node_name}' not found in cluster {cluster.instance_id}"
                    )
                result_text = await cluster._agent.call(
                    ipv6=node["ipv6"],
                    agent_key=node.get("umr_agent_key") or node.get("agent_key", ""),
                    action=action,
                    params=params or {},
                )
                return ToolResult.ok(data=result_text, duration_ms=0)

            # Demo mode: fall back to simulated log data
            elif hasattr(cluster, 'get_node_log'):
                log_path = f"{node_name}:{params.get('Path', '')}" if params else node_name
                content = cluster.get_node_log(
                    log_path=log_path,
                    keywords=params.get("Keywords", "") if params else "",
                    max_lines=int(params.get("MaxLines", 50)) if params else 50,
                )
                return ToolResult.ok(data=content, duration_ms=0)

            return ToolResult.failed("No cluster adapter available")

        except Exception as exc:
            return ToolResult.failed(f"umrAgent call failed: {type(exc).__name__}: {exc}")

    return Tool(
        name="call_umr_agent",
        description=(
            "Call umrAgent on a cluster node to fetch diagnostic data. "
            "Available Actions:\n"
            + actions_desc
            + "\n\nUse this for ALL log retrieval, process checks, and node diagnostics. "
            "NEVER attempt SSH or direct file access — always go through umrAgent."
        ),
        parameters={
            "node_name": {
                "type": "string",
                "description": "Node identifier, e.g. 'c-xxx-master1', 'master1', or the full node_name from the cluster node list",
                "required": True,
            },
            "action": {
                "type": "string",
                "description": f"umrAgent Action to execute. One of: {', '.join(ALLOWED_ACTIONS.keys())}",
                "required": True,
            },
            "params": {
                "type": "object",
                "description": "Action-specific parameters as a JSON object. See action descriptions above.",
                "default": {},
            },
        },
        fn=execute,
    )
