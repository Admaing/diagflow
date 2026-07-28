"""
v3 Tool definitions — plain async functions wrapped as Anthropic SDK tools.

Each tool is a ToolDef(name, description, input_schema, handler).
No Tool class boilerplate — just async functions with JSON Schema params.
"""

from __future__ import annotations

import asyncio
import hashlib
import hmac
import logging
import os
import time
from typing import Any

import httpx
import paramiko
import yaml
from pathlib import Path

from diagflow.core.diag_agent import ToolDef
from diagflow.core.memory import EvidencePool

logger = logging.getLogger(__name__)


# ===========================================================================
# Tool 1: query_yarn — direct YARN RM query
# ===========================================================================

async def _query_yarn_handler(
    cluster,
    action: str = "list_apps",
    app_id: str = "",
    app_name: str = "",
) -> str:
    await cluster._ensure_node_data()
    master = cluster._find_node("master1")
    if not master:
        return "Error: No master node found"
    ipv6 = master.get("ipv6", "")
    rm_url = f"http://[{ipv6}]:23188/ws/v1/cluster"

    async with httpx.AsyncClient(timeout=10) as c:
        if action == "list_apps":
            states = app_name or "RUNNING,FAILED,KILLED,FINISHED"
            resp = await c.get(f"{rm_url}/apps?states={states}")
            data = resp.json()
            apps = data.get("apps", {}).get("app", [])
            if not apps:
                return "0 YARN apps found."
            result = []
            for a in apps[:20]:
                fs = a.get("finalStatus", a.get("state", ""))
                diag = (a.get("diagnostics", "") or "")[:200]
                result.append(f"  {a.get('name','?')}  id={a.get('id','?')}  state={a.get('state','?')}  final={fs}")
                if fs == "FAILED" and diag:
                    result.append(f"    diagnostics: {diag}")
            failed = sum(1 for a in apps if a.get("finalStatus") == "FAILED")
            return f"{len(apps)} YARN apps ({failed} FAILED):\n" + "\n".join(result)

        elif action == "app_nodes":
            resp = await c.get(f"{rm_url}/apps/{app_id}/appattempts")
            data = resp.json()
            attempts = data.get("appAttempts", {}).get("appAttempt", [])
            nodes = list({a.get("host", "") for a in attempts if a.get("host")})
            container_ids = [a.get("containerId", "") for a in attempts if a.get("containerId")]

            lines = [f"App {app_id} on {len(nodes)} nodes:"]
            for n in nodes:
                lines.append(f"  node: {n}")

            if container_ids:
                lines.append(f"  containers: {', '.join(container_ids[:10])}")

                # Build container log directory paths (production format):
                # /data/yarn/logs/{app_id}/{container_id}/taskmanager.err
                lines.append("")
                lines.append("  === Two-tier log retrieval ===")
                lines.append("")
                lines.append("  Tier 1 — yarn CLI (run on master node):")
                lines.append(f"    yarn logs -applicationId {app_id} -logFiles stderr -size -1 | tail -200")
                lines.append(f"    yarn logs -applicationId {app_id} -logFiles stdout -size -1 | tail -200")
                lines.append(f"    # Add -containerId {container_ids[0]} to target a specific container")
                lines.append("")
                lines.append("  Tier 2 — direct file access (if yarn logs fails, SSH to the node):")
                for cid in container_ids[:5]:
                    # containerId: container_e06_1783421408688_12571_02_000003
                    log_dir = f"/data/yarn/logs/{app_id}/{cid}"
                    # Find the node for this container
                    c_node = attempts[container_ids.index(cid)].get("host", "?") if cid in container_ids else "?"
                    lines.append(f"    # Container {cid} on {c_node}")
                    lines.append(f"    ssh_exec(node='{c_node}', cmd='ls {log_dir}/taskmanager.* 2>/dev/null')")
                    lines.append(f"    ssh_exec(node='{c_node}', cmd='tail -200 {log_dir}/taskmanager.err 2>/dev/null')")
            else:
                lines.append("  No container info available.")

            return "\n".join(lines)

        return f"Unknown action: {action}"


def make_query_yarn_tool(cluster):
    return ToolDef(
        name="query_yarn",
        description="Query YARN RM. Use 'list_apps' to see all apps (FAILED first). Use 'app_nodes' with app_id to find nodes.",
        input_schema={
            "type": "object",
            "properties": {
                "action": {"type": "string", "description": "'list_apps' or 'app_nodes'"},
                "app_id": {"type": "string", "description": "YARN app ID for 'app_nodes'"},
                "app_name": {"type": "string", "description": "Comma-separated states: RUNNING,FAILED,KILLED"},
            },
            "required": ["action"],
        },
        handler=lambda **kw: _query_yarn_handler(cluster, **kw),
    )


# ===========================================================================
# Utilities
# ===========================================================================


def _validate_command(cmd: str) -> tuple[bool, str]:
    """Validate that a shell command is read-only and safe.

    Returns (allowed, reason).
    """
    from diagflow.config import get_config
    cfg = get_config()

    cmd_clean = cmd.strip()
    cmd_lower = cmd_clean.lower()

    # Check forbidden patterns
    for forbidden in cfg.security.ssh_forbidden:
        if forbidden in cmd_lower:
            return False, f"Forbidden pattern '{forbidden}' in command"

    # Check allowed first word
    first_word = cmd_lower.split()[0] if cmd_lower.split() else ""
    if first_word in cfg.security.ssh_allowed:
        return True, ""
    # Also allow piped commands starting with allowed words
    if "|" in cmd_clean:
        first_in_pipe = cmd_lower.split("|")[0].strip().split()[0] if cmd_lower.split("|")[0].strip().split() else ""
        if first_in_pipe in cfg.security.ssh_allowed:
            return True, ""

    return False, f"Command '{first_word}' not in read-only allowlist"


# ===========================================================================
# Tool 2: ssh_exec — SSH to cluster node and run command
# ===========================================================================

async def _ssh_exec_handler(cluster, node_name: str, cmd: str, timeout_s: int = 15) -> str:
    # ---- Command safety check ----
    allowed, reason = _validate_command(cmd)
    if not allowed:
        logger.warning("Blocked unsafe SSH command: %s (reason: %s)", cmd[:100], reason)
        return f"Command blocked by safety filter: {reason}"

    await cluster._ensure_node_data()
    node = cluster._find_node(node_name)
    if not node:
        return f"Error: Node '{node_name}' not found"
    ipv6 = node.get("ipv6", "")
    user = node.get("access_user", "root")
    passwd_raw = node.get("passwd", "")

    # Decode base64 password
    import base64
    try:
        passwd = base64.b64decode(passwd_raw).decode("utf-8")
    except Exception:
        passwd = passwd_raw

    if not ipv6:
        return f"Error: No IPv6 for '{node_name}'"

    loop = asyncio.get_event_loop()
    client = paramiko.SSHClient()
    client.set_missing_host_key_policy(paramiko.AutoAddPolicy())

    def _ssh():
        client.connect(hostname=ipv6, port=22, username=user, password=passwd,
                       timeout=timeout_s, look_for_keys=False, allow_agent=False)
        stdin, stdout, stderr = client.exec_command(cmd, timeout=timeout_s)
        out = stdout.read().decode("utf-8", errors="replace")
        err = stderr.read().decode("utf-8", errors="replace")
        return (out, err)

    try:
        out, err = await loop.run_in_executor(None, _ssh)
        result = out.strip() or err.strip()
        if not result:
            result = "(empty)"
        return result
    except Exception as e:
        return f"SSH failed: {e}"
    finally:
        try:
            client.close()
        except Exception:
            pass


def make_ssh_exec_tool(cluster):
    return ToolDef(
        name="ssh_exec",
        description="Execute read-only shell commands on cluster nodes via SSH. Find logs, grep errors, check processes.",
        input_schema={
            "type": "object",
            "properties": {
                "node_name": {"type": "string", "description": "Node: 'master1', 'core1', or full node_name"},
                "cmd": {"type": "string", "description": "Shell command: find, grep, tail, ps, curl, df, free"},
                "timeout_s": {"type": "integer", "description": "SSH timeout in seconds", "default": 15},
            },
            "required": ["node_name", "cmd"],
        },
        handler=lambda **kw: _ssh_exec_handler(cluster, **kw),
    )


# ===========================================================================
# Tool 3: deepwiki_query — MCP-based known-issue lookup
# ===========================================================================


async def _deepwiki_handler(component: str, question: str, version: str = "") -> str:
    from diagflow.config import get_config
    cfg = get_config()
    repo = cfg.components.repo_map.get(component.lower())
    if not repo:
        return f"Unknown component '{component}'. Supported: {list(cfg.components.repo_map.keys())}"

    from mcp import ClientSession
    from mcp.client.streamable_http import streamablehttp_client

    try:
        async with streamablehttp_client("https://mcp.deepwiki.com/mcp") as (read, write, _):
            async with ClientSession(read, write) as session:
                await session.initialize()
                result = await session.call_tool(
                    "ask_question",
                    {"repoName": repo, "question": f"{question} (version: {version})" if version else question},
                )
                parts = [getattr(b, "text", "") for b in result.content if getattr(b, "text", None)]
                return f"[{repo}]\n" + "\n".join(parts) if parts else "(empty)"
    except Exception as e:
        return f"DeepWiki error: {e}"


def make_deepwiki_tool():
    return ToolDef(
        name="deepwiki_query",
        description="Query known issues in open-source repos. Component→repo: flink→apache/flink, hdfs→apache/hadoop, kafka→apache/kafka.",
        input_schema={
            "type": "object",
            "properties": {
                "component": {"type": "string", "description": "Component: flink, hdfs, yarn, kafka, spark, hbase"},
                "question": {"type": "string", "description": "Natural language question about known bugs"},
                "version": {"type": "string", "description": "Component version"},
            },
            "required": ["component", "question"],
        },
        handler=_deepwiki_handler,
    )


# ===========================================================================
# Tool 4: fingerprint_match
# ===========================================================================

async def _fingerprint_handler(component: str, error_pattern: str, version: str = "") -> str:
    fp = hashlib.md5(f"{component}:{error_pattern}:{version}".encode()).hexdigest()[:16]
    # KB is injected per-diagnosis; fallback if not set
    return f"No fingerprint match for {component}/{error_pattern} (fp={fp}). Proceed with diagnosis."


def make_fingerprint_tool():
    return ToolDef(
        name="fingerprint_match",
        description="Check if this error matches a known case. Fast-path before full diagnosis.",
        input_schema={
            "type": "object",
            "properties": {
                "component": {"type": "string", "description": "Component: flink, hdfs, yarn"},
                "error_pattern": {"type": "string", "description": "Key error: OutOfMemoryError, CheckpointExpired..."},
                "version": {"type": "string", "description": "Component version"},
            },
            "required": ["component", "error_pattern"],
        },
        handler=_fingerprint_handler,
    )


# ===========================================================================
# Tool 5: umrAgent proxy (for CheckProcess / GetBaseInfo / GetAppList)
# ===========================================================================

async def _umr_agent_handler(
    cluster, node_name: str, action: str, params: dict | None = None,
) -> str:
    await cluster._ensure_node_data()
    node = cluster._find_node(node_name)
    if not node:
        return f"Error: Node '{node_name}' not found"
    ipv6 = node.get("ipv6", "")
    agent_key = node.get("agent_key", "") or node.get("umr_agent_key", "")
    if not ipv6 or not agent_key:
        return f"Error: No ipv6/agent_key for '{node_name}'"

    all_params = {"Action": action, "Date": str(int(time.time() * 1000))}
    if params:
        all_params.update({k: str(v) for k, v in params.items()})

    # HMAC-SHA1 signing (replicates util/agent.js)
    string_to_sign = "".join(all_params[k] for k in sorted(all_params))
    sig = hmac.new(agent_key.encode(), string_to_sign.encode(), hashlib.sha1).hexdigest()
    all_params["Signature"] = sig

    query = "&".join(f"{k}={v}" for k, v in all_params.items())
    url = f"http://[{ipv6}]:65431/?{query}"

    async with httpx.AsyncClient(timeout=15) as c:
        resp = await c.get(url)
        return resp.text


def make_umr_agent_tool(cluster):
    return ToolDef(
        name="call_umr_agent",
        description="Call umrAgent on cluster nodes. Actions: GetLogs(Path,Keywords,MaxLines,Since), CheckProcess(ProcessName), GetBaseInfo, GetAppList.",
        input_schema={
            "type": "object",
            "properties": {
                "node_name": {"type": "string", "description": "Node: 'master1', 'core1', or full node_name"},
                "action": {"type": "string", "description": "umrAgent Action: GetLogs, CheckProcess, GetBaseInfo, GetAppList"},
                "params": {"type": "object", "description": "Action-specific params as JSON"},
            },
            "required": ["node_name", "action"],
        },
        handler=lambda **kw: _umr_agent_handler(cluster, **kw),
    )


# ===========================================================================
# Factory: build all tools for a cluster
# ===========================================================================

def build_v3_tools(cluster, kb=None) -> list[ToolDef]:
    tools = [
        make_query_yarn_tool(cluster),
        make_ssh_exec_tool(cluster),
        make_umr_agent_tool(cluster),
        make_deepwiki_tool(),
        make_fingerprint_tool(),
    ]
    # Inject KB into fingerprint tool
    if kb:
        fp_tool = tools[-1]
        async def _kb_handler(**kw):
            hit = kb.fingerprint_match(
                kw.get("component", ""), kw.get("error_pattern", ""), kw.get("version", "")
            )
            if hit:
                return f"✅ Known issue (confidence: high):\n  Root cause: {hit.get('root_cause','?')}\n  Suggestions: {hit.get('suggestions',[])}"
            return await _fingerprint_handler(**kw)
        tools[-1] = ToolDef(
            name="fingerprint_match",
            description=fp_tool.description,
            input_schema=fp_tool.input_schema,
            handler=_kb_handler,
        )
    return tools
