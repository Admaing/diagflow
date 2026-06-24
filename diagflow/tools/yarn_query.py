"""
Direct YARN RM query tool — call YARN REST API on master node via IPv6.

No SSH, no umrAgent needed. DiagFlow Pod can already reach cluster nodes
via IPv6 (umrAgent 65431 verified). YARN RM runs on port 23188.

Used by the Agent in Phase 3 to find which cluster nodes a Flink/Spark
application runs on, so it can target GetLogs to the right nodes.
"""

import httpx
from diagflow.core.tool import Tool, ToolResult


def make_yarn_query_tool(cluster):
    """Query YARN ResourceManager REST API on the master node."""

    async def _get_master_ipv6() -> str:
        """Get master1's IPv6 address from cluster node info."""
        await cluster._ensure_node_data()
        master = cluster._find_node("master1")
        if master:
            return master.get("ipv6", "")
        return ""

    async def execute(
        action: str = "list_apps",
        app_id: str = "",
        app_name: str = "",
    ) -> ToolResult:
        """Query YARN ResourceManager API directly via IPv6.

        Args:
            action: 'list_apps' (all apps, FAILED first) | 'app_nodes' (find nodes for an app)
            app_id: YARN application ID for 'app_nodes'
            app_name: For 'list_apps': filter by comma-separated states (RUNNING,FAILED,KILLED,FINISHED).
                      Default: all states. FAILED apps are listed first with diagnostics.
        """
        try:
            ipv6 = await _get_master_ipv6()
            if not ipv6:
                return ToolResult.failed("No master node IPv6 found")

            rm_url = f"http://[{ipv6}]:23188/ws/v1/cluster"

            if action == "list_apps":
                states = app_name or "RUNNING,FAILED,KILLED,FINISHED"
                resp = await httpx.AsyncClient(timeout=10).get(
                    f"{rm_url}/apps?states={states}"
                )
                resp.raise_for_status()
                data = resp.json()
                apps = data.get("apps", {}).get("app", [])

                result = []
                for a in apps[:30]:
                    final_status = a.get("finalStatus", a.get("state", ""))
                    result.append({
                        "id": a.get("id", ""),
                        "name": a.get("name", ""),
                        "state": a.get("state", ""),
                        "finalStatus": final_status,
                        "queue": a.get("queue", ""),
                        "user": a.get("user", ""),
                        "diagnostics": (a.get("diagnostics", "") or "")[:300],
                    })
                # Sort: failed first
                result.sort(key=lambda r: 0 if r["finalStatus"] == "FAILED" else 1)

                # Summary
                failed = [r for r in result if r["finalStatus"] == "FAILED"]
                killed = [r for r in result if r["finalStatus"] == "KILLED"]
                running = [r for r in result if r["state"] == "RUNNING"]
                summary = (
                    f"YARN apps: {len(running)} RUNNING, {len(failed)} FAILED, "
                    f"{len(killed)} KILLED, {len(result)} total\n\n"
                )
                if failed:
                    summary += "=== FAILED apps (diagnose these!) ===\n"
                    for r in failed:
                        diag = r["diagnostics"][:120] if r["diagnostics"] else "(no diagnostics)"
                        summary += f"  {r['name']}  id={r['id']}\n    diagnostics: {diag}\n"
                if running:
                    summary += "\n=== RUNNING apps ===\n"
                    for r in running[:10]:
                        summary += f"  {r['name']}  id={r['id']}\n"
                if killed:
                    summary += "\n=== KILLED apps ===\n"
                    for r in killed[:5]:
                        summary += f"  {r['name']}  id={r['id']}\n"
                return ToolResult.ok(summary)

            elif action == "app_nodes":
                if not app_id:
                    return ToolResult.failed("app_id is required for 'app_nodes' action")

                # Get app attempts to find container hostnames
                resp = await httpx.AsyncClient(timeout=10).get(
                    f"{rm_url}/apps/{app_id}/appattempts"
                )
                resp.raise_for_status()
                data = resp.json()
                attempts = data.get("appAttempts", {}).get("appAttempt", [])

                nodes = []
                seen = set()
                for attempt in attempts:
                    host = attempt.get("host", "")
                    if host and host not in seen:
                        seen.add(host)
                        nodes.append(host)

                return ToolResult.ok(
                    f"App {app_id} runs on {len(nodes)} nodes:\n"
                    + "\n".join(f"  {n}" for n in nodes)
                )

            else:
                return ToolResult.failed(f"Unknown action '{action}'. Use 'list_apps' or 'app_nodes'.")

        except Exception as exc:
            return ToolResult.failed(f"YARN query failed: {type(exc).__name__}: {exc}")

    return Tool(
        name="query_yarn",
        description=(
            "Query YARN ResourceManager REST API directly via IPv6. "
            "Use 'list_apps' to see ALL apps — FAILED apps are listed FIRST with diagnostics. "
            "This is the FIRST thing to call when a user reports task errors. "
            "FAILED/KILLED apps have diagnostics explaining why they failed. "
            "Use 'app_nodes' with a failed app's id to find which nodes it ran on, "
            "then call_umr_agent GetLogs on those nodes."
        ),
        parameters={
            "action": {
                "type": "string",
                "description": "'list_apps' (list all apps, FAILED first with diagnostics) or 'app_nodes' (find nodes for an app)",
                "default": "list_apps",
            },
            "app_id": {
                "type": "string",
                "description": "YARN application ID. Required for 'app_nodes'.",
                "default": "",
            },
            "app_name": {
                "type": "string",
                "description": "Comma-separated YARN states: RUNNING,FAILED,KILLED,FINISHED. Default=all.",
                "default": "",
            },
        },
        fn=execute,
        timeout_s=15,
    )
