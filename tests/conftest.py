"""Shared fixtures for DiagFlow tests."""

from __future__ import annotations

import pytest
from unittest.mock import AsyncMock, MagicMock, patch


@pytest.fixture
def sample_context():
    """Sample diagnosis context dict."""
    return {
        "component": "flink",
        "problem": "job_failure",
        "cluster_id": "uhadoop-test01",
        "region": "北京",
        "version": "1.17.0",
        "job_id": "job_test_001",
        "detail": "TaskManager OOM killed by YARN",
    }


@pytest.fixture
def sample_evidence_pool():
    """Pre-populated EvidencePool for testing."""
    from diagflow.core.memory import EvidencePool, Evidence

    pool = EvidencePool()
    pool.add(Evidence(
        source_agent="strategy", category="tool:query_yarn",
        summary="3 apps, 1 FAILED: flink-job", detail="Flink job FAILED with exit code 137",
        confidence=0.8,
    ))
    pool.add(Evidence(
        source_agent="strategy", category="tool:ssh_exec",
        summary="OutOfMemoryError in taskmanager.log",
        detail="java.lang.OutOfMemoryError: Java heap space\nat org.apache.flink...",
        confidence=0.9,
    ))
    pool.add(Evidence(
        source_agent="react", category="agent_analysis",
        summary="OOM caused by 2048m heap with 4 slots", detail="TaskManager configured 2048m heap for 4 slots = 512m per slot. GC overhead 95.5%.",
        confidence=0.7,
    ))
    return pool


@pytest.fixture
def mock_anthropic_client():
    """Mock Anthropic client that returns controlled responses."""
    client = MagicMock()
    # Default: return an empty-completion response
    client.messages.create.return_value = MagicMock(
        content=[MagicMock(type="text", text="ROOT_CAUSE: Test root cause\nCONFIDENCE: high\n- Fix suggestion 1\n- Fix suggestion 2")]
    )
    return client


@pytest.fixture
def sample_strategy_dir(tmp_path):
    """Create a temporary strategy directory with a YAML file."""
    import yaml

    strategies_dir = tmp_path / "strategies"
    strategies_dir.mkdir()

    strategy_yaml = {
        "component": "flink",
        "problem_type": "job_failure",
        "version": "1.0",
        "steps": [
            {
                "action": "fingerprint_match",
                "description": "Check known issues",
                "priority": 0,
            },
            {
                "action": "tool_call",
                "tool": "query_yarn",
                "description": "List YARN apps",
                "params": {"action": "list_apps"},
                "priority": 1,
            },
            {
                "action": "tool_call",
                "tool": "ssh_exec",
                "description": "Grep for errors",
                "params": {
                    "node_name": "{{ context.cluster_id }}-master1",
                    "cmd": "grep ERROR /var/log/flink/taskmanager.log | tail -50",
                },
                "priority": 1,
            },
        ],
    }

    file_path = strategies_dir / "flink_job_failure.yaml"
    with open(file_path, "w") as f:
        yaml.dump(strategy_yaml, f)

    return str(strategies_dir)
