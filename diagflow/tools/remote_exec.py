"""
SSH remote execution tool — direct access to cluster nodes.

TEMPORARY: Uses SSH for fast iteration. Will be replaced by umrAgent calls
once the correct Actions (GetLogs, FindLogs) are deployed to all nodes.

Nodes are accessed via IPv6 + password from uhadoop-manage's describe_cluster.
Read-only commands only — no system changes.
"""

import asyncio
import base64
import paramiko

from diagflow.core.tool import Tool, ToolResult


def _decode_passwd(encoded: str) -> str:
    """go_framework stores passwords as base64."""
    try:
        return base64.b64decode(encoded).decode("utf-8")
    except Exception:
        return encoded  # might already be plaintext


async def _ssh_exec(ipv6: str, user: str, password: str, cmd: str,
                    timeout_s: int = 15) -> str:
    """Execute a command on a remote node via SSH + IPv6."""
    loop = asyncio.get_event_loop()
    client = paramiko.SSHClient()
    client.set_missing_host_key_policy(paramiko.AutoAddPolicy())

    def _run():
        client.connect(
            hostname=ipv6, port=22, username=user,
            password=password, timeout=timeout_s, look_for_keys=False,
            allow_agent=False,
        )
        stdin, stdout, stderr = client.exec_command(cmd, timeout=timeout_s)
        out = stdout.read().decode("utf-8", errors="replace")
        err = stderr.read().decode("utf-8", errors="replace")
        return (out, err)

    try:
        out, err = await loop.run_in_executor(None, _run)
        result = out.strip()
        if err.strip() and not result:
            result = err.strip()
        return result
    finally:
        try:
            client.close()
        except Exception:
            pass


def make_remote_exec_tool(cluster):
    """SSH-based command execution on cluster nodes."""

    async def _get_node_ssh_info(node_ref: str) -> tuple[str, str, str]:
        """Get IPv6, user, password for a node."""
        await cluster._ensure_node_data()
        node = cluster._find_node(node_ref)
        if not node:
            raise RuntimeError(f"Node '{node_ref}' not found")
        ipv6 = node.get("ipv6", "")
        user = node.get("access_user", "root")
        passwd = _decode_passwd(node.get("passwd", ""))
        if not ipv6:
            raise RuntimeError(f"No IPv6 for node '{node_ref}'")
        return ipv6, user, passwd

    async def execute(
        node_name: str,
        cmd: str,
        timeout_s: int = 15,
    ) -> ToolResult:
        """Execute a shell command on a cluster node via SSH.

        Args:
            node_name: Node identifier (e.g. "master1", "core1", or full name)
            cmd: Shell command to run (READ-ONLY, e.g. find, grep, cat, tail, curl)
            timeout_s: SSH timeout in seconds
        """
        try:
            ipv6, user, passwd = await _get_node_ssh_info(node_name)
        except Exception as exc:
            return ToolResult.failed(f"SSH node lookup failed: {exc}")

        try:
            output = await _ssh_exec(ipv6, user, passwd, cmd, timeout_s)
            return ToolResult.ok(data=output)
        except Exception as exc:
            return ToolResult.failed(f"SSH failed: {exc}")

    return Tool(
        name="ssh_exec",
        description=(
            "Execute a READ-ONLY shell command on a cluster node via SSH. "
            "Use this to find log files, check processes, query YARN, or read "
            "configs when umrAgent Actions don't exist yet. "
            "Common commands:\n"
            "  - Find logs: find /data /var/log /opt -name '*flink*.log' 2>/dev/null | head -20\n"
            "  - Tail errors: tail -100 <logfile> | grep -E 'ERROR|Exception|FATAL'\n"
            "  - Check process: ps aux | grep -i flink\n"
            "  - Disk usage: df -h\n"
            "  - YARN apps: curl -s http://localhost:23188/ws/v1/cluster/apps?states=FAILED\n"
            "  - Memory info: free -h"
        ),
        parameters={
            "node_name": {
                "type": "string",
                "description": "Node to SSH into: 'master1', 'core1', or full node_name",
                "required": True,
            },
            "cmd": {
                "type": "string",
                "description": "Shell command to run (read-only: find, grep, tail, ps, curl, df, free)",
                "required": True,
            },
            "timeout_s": {
                "type": "integer",
                "description": "SSH timeout in seconds (default 15)",
                "default": 15,
            },
        },
        fn=execute,
        timeout_s=30,
    )
