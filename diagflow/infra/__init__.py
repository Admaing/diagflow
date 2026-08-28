"""
Production infrastructure adapter — bridges DiagFlow to real UHadoop infrastructure.

Two critical architectural rules (per product requirements):

  1. **Node info via Go service API, NOT direct MySQL.**
     DiagFlow never touches t_uhadoop_node or t_uhadoop_umragent directly.
     It calls the uhadoop-manager API (via gRPC/HTTP, resolved through
     ZK/Consul service discovery) to get cluster node metadata.

  2. **Logs via umrAgent HTTP API, NOT SSH.**
     The LLM constructs the umrAgent Action + parameters. The Tool layer
     translates this into a signed HTTP call to http://[ipv6]:65431/.
     No SSH keys, no remote shell — zero-touch on the node filesystem.

The Tool layer sees the same interface in demo and production — only the
data source changes (SimulatedCluster vs RealCluster).
"""

from __future__ import annotations

import hashlib
import hmac
import json
import logging
import os
import time
from typing import Any

import httpx
from kazoo.client import KazooClient

logger = logging.getLogger(__name__)


# ===========================================================================
# 1. Service discovery — ZK-based, mirrors uhadoop-task/libs/name_container.js
#    Also supports Consul leader discovery (Go-idiomatic path)
# ===========================================================================

class UCloudServiceDiscovery:
    """Service discovery via ZooKeeper and/or Consul.

    Mirrors the pattern in uhadoop-task/libs/name_container.js for ZK,
    plus uhadoop-go's Consul leader discovery as a fallback.
    """

    def __init__(self, zk_hosts: str, consul_addr: str = "", region: str = ""):
        self.zk = KazooClient(hosts=zk_hosts)
        self.region = region
        self.consul_addr = consul_addr
        self._cache: dict[str, list[dict]] = {}
        self._started = False

    def start(self) -> None:
        if not self._started:
            self.zk.start(timeout=10)
            self._started = True

    # -- ZK discovery (mirrors NameContainer.getNameValue) --

    def get_service(self, zk_path: str) -> dict | None:
        """Return a random service instance from a ZK path."""
        if zk_path not in self._cache:
            self._refresh_zk(zk_path)
        instances = self._cache.get(zk_path, [])
        if not instances:
            return None
        import random
        return random.choice(instances)

    def _refresh_zk(self, zk_path: str) -> None:
        if not self.zk.exists(zk_path):
            self._cache[zk_path] = []
            return
        children = self.zk.get_children(zk_path)
        instances = []
        for child in children:
            data, _ = self.zk.get(f"{zk_path}/{child}")
            if data:
                try:
                    instances.append(json.loads(data))
                except Exception:
                    logger.debug("Failed to parse ZK node data", exc_info=True)
        self._cache[zk_path] = instances

    # -- Consul leader discovery (mirrors uhadoop-go's LeaderDiscovery) --

    async def get_leader(self, consul_leader_key: str) -> dict | None:
        """Find the active leader for a service via Consul."""
        if not self.consul_addr:
            return None
        url = f"http://{self.consul_addr}/v1/kv/{consul_leader_key}?raw"
        async with httpx.AsyncClient(timeout=5) as client:
            resp = await client.get(url)
            if resp.status_code == 200:
                raw = resp.text.strip()
                try:
                    return json.loads(raw)
                except json.JSONDecodeError:
                    return {"address": raw, "port": 0}
        return None

    @classmethod
    def from_env(cls) -> "UCloudServiceDiscovery":
        return cls(
            zk_hosts=os.environ.get("ZK_HOSTS", ""),
            consul_addr=os.environ.get("CONSUL_ADDR", ""),
            region=os.environ.get("REGION", "test03"),
        )


# ===========================================================================
# 2. umrAgent client — HTTP to [ipv6]:65431, HMAC-SHA1 signed, NO SSH
#    Port of uhadoop-task/util/agent.js signing, replicated in Go at
#    pkg/client/uagent/agent.go setGetSignature().
# ===========================================================================

class UmrAgentClient:
    """Call umrAgent on cluster nodes via HTTP (port 65431).

    The LLM decides WHAT to call (Action name, parameters). This client
    handles HOW to call it — HMAC-SHA1 signing, IPv6 URL construction,
    timeout isolation. No SSH keys, no remote execution — the caller
    never touches the node filesystem directly.

    Faithful replica of:
      - Node.js: uhadoop-task/util/agent.js  (auth function)
      - Go:      pkg/client/uagent/agent.go   (setGetSignature)

    Key constraint: get_node_access_info() is called via the Go API
    (NodeInfoService), NOT via direct MySQL. umr_agent_key and ipv6
    come from the API response, not from our own DB query.
    """

    def __init__(self, timeout_s: int = 10):
        self._client = httpx.AsyncClient(timeout=timeout_s)

    async def close(self) -> None:
        await self._client.aclose()

    async def call(
        self,
        ipv6: str,
        agent_key: str,
        action: str,
        params: dict | None = None,
        method: str = "GET",
    ) -> str:
        """Invoke an umrAgent Action on a node."""
        from diagflow.config import get_config
        cfg = get_config()

        all_params: dict[str, str] = {
            "Action": action,
            "Date": str(int(time.time() * 1000)),
        }
        if params:
            all_params.update({k: str(v) for k, v in params.items()})

        # HMAC-SHA1 signing — identical to util/agent.js + pkg/client/uagent
        signature = self._sign(all_params, agent_key)
        all_params["Signature"] = signature

        query = "&".join(f"{k}={v}" for k, v in all_params.items())
        url = f"http://[{ipv6}]:{cfg.umr_agent.port}/?{query}"

        if method == "GET":
            resp = await self._client.get(url)
        else:
            resp = await self._client.post(url)
        resp.raise_for_status()
        return resp.text

    @staticmethod
    def _sign(params: dict[str, str], key: str) -> str:
        """HMAC-SHA1 over sorted param values.

        Identical to:
          util/agent.js     auth(): sort keys → concat values → HMAC-SHA1
          uagent/agent.go   setGetSignature(): sort.Strings → hmac.New(SHA1)
        """
        string_to_sign = "".join(params[k] for k in sorted(params))
        return hmac.new(
            key.encode("utf-8"),
            string_to_sign.encode("utf-8"),
            hashlib.sha1,
        ).hexdigest()


# ===========================================================================
# 3. Node info API client — calls uhadoop-manage HTTP API (Action-based)
#    NO direct MySQL. uhadoop-manage is the single source of truth for
#    cluster/node metadata; it internally calls uhadoop-access via RpcTcp.
# ===========================================================================

class NodeInfoClient:
    """Fetch cluster/node metadata from uhadoop-manage's HTTP API.

    uhadoop-manage exposes an Action-based HTTP API on port 20141:
      POST /  body: {Action: "describe_cluster_nodes", instance_id, region}
      → {RetCode: 0, cluster_info: {...}, nodes: [{node_name, ipv6, agent_key, ...}]}

    The `describe_cluster_nodes` Action (added in controllor/describe_cluster_nodes.js)
    wraps the existing describeUHADOOP() function, which internally calls
    uhadoop-access via UCloud binary TCP protocol. DiagFlow only talks HTTP.
    """

    def __init__(self, base_url: str, timeout_s: int = 15, region: str = ""):
        self._client = httpx.AsyncClient(
            base_url=base_url,
            timeout=timeout_s,
            headers={"Content-Type": "application/json"},
        )
        self.region = region

    async def close(self) -> None:
        await self._client.aclose()

    async def describe_cluster(self, instance_id: str) -> dict:
        """Fetch cluster info + all nodes (with ipv6 + agent_key) in one call.

        Returns:
          {
            "cluster_info": {instance_id, framework, framework_version, cluster_state, ...},
            "nodes": [{node_name, node_ip, ipv6, node_role, agent_key, node_state, ...}]
          }
        """
        payload = {
            "Action": "describe_cluster_nodes",
            "instance_id": instance_id,
            "region": self.region,
        }
        resp = await self._client.post("/", json=payload)
        resp.raise_for_status()
        data = resp.json()

        if data.get("RetCode", 0) != 0:
            raise RuntimeError(
                f"uhadoop-manage error: {data.get('Error', 'unknown')}"
            )
        return data

    async def get_cluster_info(self, instance_id: str) -> dict:
        """Get cluster metadata only."""
        data = await self.describe_cluster(instance_id)
        return data.get("cluster_info", {})

    async def get_cluster_nodes(self, instance_id: str) -> list[dict]:
        """List all nodes for a cluster, each with ipv6 + agent_key."""
        data = await self.describe_cluster(instance_id)
        return data.get("nodes", [])

    @classmethod
    def from_discovery(cls, discovery: UCloudServiceDiscovery, region: str) -> "NodeInfoClient":
        """Resolve uhadoop-manage's HTTP address via ZK and build a client.

        uhadoop-manage registers its HTTP JSON endpoint at:
          /NS/uhadoop/set{regionId}/UHADOOPManageHttpJson/{part}

        Discovery strategy (in order):
          1. Exact match: /NS/uhadoop/set{region}/UHADOOPManageHttpJson
          2. Scan: iterate all /NS/uhadoop/set*/UHADOOPManageHttpJson paths,
             pick the first one with registered instances. This handles
             the case where the ZK set ID (e.g. 666003001) differs from
             the deployment region code (e.g. 3001).
          3. Env var: UHADOOP_MANAGE_HTTP_BASE=http://<ip>:20141
        """
        # Strategy 1: exact region match
        zk_path = f"/NS/uhadoop/set{region}/UHADOOPManageHttpJson"
        instance = discovery.get_service(zk_path)
        if instance and instance.get("ip"):
            base = f"http://{instance['ip']}:{instance.get('port', 20141)}"
            return cls(base_url=base, region=region)

        # Strategy 2: scan all sets (region code ≠ ZK set ID)
        try:
            top = "/NS/uhadoop"
            if discovery.zk.exists(top):
                for child in discovery.zk.get_children(top):
                    if not child.startswith("set"):
                        continue
                    set_path = f"{top}/{child}/UHADOOPManageHttpJson"
                    svc = discovery.get_service(set_path)
                    if svc and svc.get("ip"):
                        base = f"http://{svc['ip']}:{svc.get('port', 20141)}"
                        return cls(base_url=base, region=region)
        except Exception:
            logger.warning("ZK set scan failed, trying env var fallback", exc_info=True)

        # Strategy 3: env var fallback
        base = os.environ.get("UHADOOP_MANAGE_HTTP_BASE", "")
        if not base:
            raise RuntimeError(
                "Could not discover uhadoop-manage HTTP via ZK. "
                "Set UHADOOP_MANAGE_HTTP_BASE=http://<ip>:20141"
            )
        return cls(base_url=base, region=region)


# ===========================================================================
# 4. RealCluster adapter — drop-in replacement for SimulatedCluster
#    Same interface, different data sources (Go API + umrAgent, NOT MySQL)
# ===========================================================================

class RealCluster:
    """Production cluster adapter.

    Data flow:
      Node info:  DiagFlow → NodeInfoClient → uhadoop-manager → (shared DB)
      Logs:       LLM constructs Action→DiagFlow Tool→UmrAgentClient→[ipv6]:65431
      Metrics:    DiagFlow → ZK/Consul→uhadoop-go monitor API

    Same interface as SimulatedCluster so the Tool layer needs no changes.
    """

    def __init__(
        self,
        instance_id: str,
        node_client: NodeInfoClient | None = None,
        agent: UmrAgentClient | None = None,
        discovery: UCloudServiceDiscovery | None = None,
    ):
        self.instance_id = instance_id
        self._node_client = node_client
        self._agent = agent or UmrAgentClient()
        self._discovery = discovery

        # Populated lazily from the Go node-info API
        self._cluster_info: dict | None = None
        self._nodes_list: list[dict] | None = None
        self.context: dict[str, Any] = {"cluster_id": instance_id}

    async def _ensure_node_data(self) -> None:
        """Lazy-load cluster + node metadata from Go API."""
        if self._nodes_list is not None:
            return
        if self._node_client is None:
            discovery = self._discovery or UCloudServiceDiscovery.from_env()
            discovery.start()
            self._node_client = NodeInfoClient.from_discovery(
                discovery, os.environ.get("REGION", "test03")
            )

        self._cluster_info = await self._node_client.get_cluster_info(self.instance_id)
        self._nodes_list = await self._node_client.get_cluster_nodes(self.instance_id)

        # Populate context for the diagnosis workflow
        ci = self._cluster_info or {}
        self.context.update({
            "cluster_id": self.instance_id,
            "region": ci.get("region", ""),
            "component": self.context.get("component", "flink"),
            "problem": self.context.get("problem", "service_failure"),
            "nodes_count": len(self._nodes_list),
        })
        # Auto-generate topology from installed components
        self._load_topology(ci)

    def _load_topology(self, cluster_info: dict) -> None:
        """Load topology for this cluster from describe_cluster.app[] list."""
        import yaml
        from pathlib import Path
        try:
            topo_path = Path(__file__).parent.parent.parent / "data" / "service_topology.yaml"
            if not topo_path.exists():
                return
            with open(topo_path) as f:
                full_topo = yaml.safe_load(f) or {}
        except Exception:
            logger.warning("Failed to load service topology", exc_info=True)
            return

        apps = cluster_info.get("app", [])
        if not apps:
            return

        installed = set()
        for a in apps:
            name = (a.get("app_name", "") or a.get("name", "")).lower()
            installed.add(name)
        # Also infer from framework
        framework = (cluster_info.get("framework", "") or "").lower()
        if "hadoop" in framework:
            installed.update({"hdfs", "yarn"})

        # Filter topology to only installed components
        cluster_topo = {}
        for comp_name, comp_data in full_topo.items():
            if comp_name in installed:
                cluster_topo[comp_name] = comp_data

        if cluster_topo:
            self.context["topology"] = cluster_topo
            self.context["installed_components"] = list(installed)

    def _find_node(self, node_ref: str) -> dict | None:
        """Resolve a node reference to a node entry.

        node_ref can be: node_name, node role suffix ('master1'), or index.
        """
        if not self._nodes_list:
            return None
        for n in self._nodes_list:
            if n.get("node_name") == node_ref:
                return n
            if node_ref in (n.get("node_name", ""), n.get("node_role", "")):
                return n
        return self._nodes_list[0]  # fallback: first node

    async def get_node_log(self, log_path: str, keywords: str = "",
                           max_lines: int = 50) -> str:
        """Fetch logs via umrAgent — LLM decides Action, Tool executes.

        log_path format: "node_ref:filename"
          - "c-xxx-master1:/var/log/flink/taskmanager.log"
          - "master1:jobmanager.log"

        The Tool layer calls this; the LLM constructed the parameters
        (which node? which log file? what keywords?). This method
        translates to an umrAgent GetLogs call — NO SSH.
        """
        await self._ensure_node_data()

        node_ref, _, filename = log_path.partition(":")
        if not filename:
            node_ref = "master1"
            filename = log_path

        node = self._find_node(node_ref)
        if not node:
            return f"[Error] Node '{node_ref}' not found in cluster {self.instance_id}"

        ipv6 = node.get("ipv6", "")
        agent_key = node.get("agent_key", "") or node.get("umr_agent_key", "")

        if not ipv6 or not agent_key:
            return (
                f"[Error] Node '{node.get('node_name', node_ref)}' missing "
                f"ipv6 or umr_agent_key — check Go API response"
            )

        assert self._agent is not None
        return await self._agent.call(
            ipv6=ipv6,
            agent_key=agent_key,
            action="GetLogs",
            params={
                "Path": filename,
                "Keywords": keywords,
                "MaxLines": str(max_lines),
            },
        )

    async def get_config(self, config_path: str) -> str:
        """Fetch a config file from master node via umrAgent."""
        await self._ensure_node_data()
        master = self._find_node("master1")
        if not master:
            return f"[Error] No master node in cluster {self.instance_id}"
        assert self._agent is not None
        # Same compat lookup as get_node_log / _umr_agent_handler: umrAgent key
        # arrives as either "agent_key" or "umr_agent_key" across API responses.
        agent_key = master.get("agent_key", "") or master.get("umr_agent_key", "")
        return await self._agent.call(
            ipv6=master["ipv6"],
            agent_key=agent_key,
            action="GetLogs",
            params={"Path": config_path},
        )

    async def get_metrics(self, metric_names: list[str] | None = None) -> str:
        """Fetch metrics from uhadoop-go monitor via service discovery."""
        await self._ensure_node_data()
        if not self._discovery:
            return "[Production] Metrics require service discovery configured"

        # Try ZK path: /NS/umonitor2/set1/access (from application.yaml)
        monitor = self._discovery.get_service("/NS/umonitor2/set1/access")
        if not monitor:
            return "[Production] Monitor service not found via ZK"

        ip = monitor.get("ip", "")
        port = monitor.get("port", 0)
        async with httpx.AsyncClient(timeout=10) as client:
            params = {"instance_id": self.instance_id}
            if metric_names:
                params["metrics"] = ",".join(metric_names)
            resp = await client.get(f"http://{ip}:{port}/api/v1/metrics", params=params)
            resp.raise_for_status()
            return resp.text

    async def close(self) -> None:
        if self._agent:
            await self._agent.close()
        if self._node_client:
            await self._node_client.close()

    def summary(self) -> str:
        ci = self._cluster_info or {}
        return (
            f"RealCluster(instance_id={self.instance_id}, "
            f"region={ci.get('region', 'unknown')}, "
            f"nodes={len(self._nodes_list or [])})"
        )
