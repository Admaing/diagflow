"""Tests for RealCluster — agent_key compat lookup in get_config."""

import pytest


class _FakeAgent:
    def __init__(self):
        self.calls = []

    async def call(self, ipv6, agent_key, action, params):
        self.calls.append({"ipv6": ipv6, "agent_key": agent_key, "action": action, "params": params})
        return "ok"


@pytest.fixture
def real_cluster(monkeypatch):
    from diagflow.infra import RealCluster

    cluster = RealCluster(instance_id="c-test")
    cluster._nodes_list = [
        {"node_name": "master1", "node_role": "master1", "ipv6": "::1"},
    ]
    agent = _FakeAgent()
    cluster._agent = agent
    # Bypass _ensure_node_data: it's already populated
    async def noop():
        return None
    cluster._ensure_node_data = noop
    return cluster, agent


class TestGetConfig:
    @pytest.mark.asyncio
    async def test_get_config_uses_agent_key_field(self, real_cluster):
        cluster, agent = real_cluster
        cluster._nodes_list[0]["agent_key"] = "key-from-agent-key"
        result = await cluster.get_config("/etc/flink.conf")
        assert result == "ok"
        assert agent.calls[0]["agent_key"] == "key-from-agent-key"

    @pytest.mark.asyncio
    async def test_get_config_uses_umr_agent_key_field(self, real_cluster):
        cluster, agent = real_cluster
        cluster._nodes_list[0]["umr_agent_key"] = "key-from-umr-agent-key"
        result = await cluster.get_config("/etc/flink.conf")
        assert result == "ok"
        assert agent.calls[0]["agent_key"] == "key-from-umr-agent-key"

    @pytest.mark.asyncio
    async def test_get_config_prefers_agent_key(self, real_cluster):
        cluster, agent = real_cluster
        cluster._nodes_list[0]["agent_key"] = "preferred"
        cluster._nodes_list[0]["umr_agent_key"] = "fallback"
        result = await cluster.get_config("/etc/flink.conf")
        assert agent.calls[0]["agent_key"] == "preferred"
