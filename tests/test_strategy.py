"""Tests for strategy loading and template rendering."""

import pytest
from diagflow.core.strategy import (
    StrategyStep,
    Strategy,
    load_strategy,
    _default_strategy,
)


class TestStrategyStep:
    def test_render_params_simple(self):
        step = StrategyStep(
            action="tool_call",
            tool="ssh_exec",
            params={"node_name": "{{ context.cluster_id }}-master1"},
        )
        rendered = step.render_params({"cluster_id": "uhadoop-test"})
        assert rendered == {"node_name": "uhadoop-test-master1"}

    def test_render_params_missing_context_key(self):
        step = StrategyStep(
            action="tool_call",
            tool="test",
            params={"key": "{{ context.missing }}"},
        )
        rendered = step.render_params({})
        assert rendered == {"key": ""}

    def test_render_params_nested(self):
        step = StrategyStep(
            action="tool_call",
            tool="test",
            params={
                "outer": {"inner": "{{ context.version }}"},
                "list": ["{{ context.component }}", "static"],
            },
        )
        rendered = step.render_params({"version": "1.17", "component": "flink"})
        assert rendered["outer"]["inner"] == "1.17"
        assert rendered["list"] == ["flink", "static"]

    def test_render_params_no_context(self):
        step = StrategyStep(
            action="tool_call",
            tool="test",
            params={"cmd": "grep ERROR taskmanager.log"},
        )
        rendered = step.render_params({})
        assert rendered == {"cmd": "grep ERROR taskmanager.log"}


class TestStrategy:
    def test_group_by_priority(self):
        strategy = Strategy(
            component="flink",
            problem_type="test",
            steps=[
                StrategyStep(action="tool_call", tool="a", priority=0),
                StrategyStep(action="tool_call", tool="b", priority=0),
                StrategyStep(action="tool_call", tool="c", priority=1),
                StrategyStep(action="tool_call", tool="d", priority=2),
            ],
        )
        batches = strategy.group_by_priority()
        assert len(batches) == 3
        # Priority 0 batch has 2 steps (parallel)
        assert len(batches[0]) == 2
        assert {s.tool for s in batches[0]} == {"a", "b"}
        # Priority 1 batch has 1 step
        assert len(batches[1]) == 1
        assert batches[1][0].tool == "c"
        # Priority 2 batch has 1 step
        assert len(batches[2]) == 1

    def test_group_by_priority_empty(self):
        strategy = Strategy(component="flink", problem_type="test")
        assert strategy.group_by_priority() == []

    def test_group_by_priority_single_batch(self):
        strategy = Strategy(
            component="flink",
            problem_type="test",
            steps=[
                StrategyStep(action="tool_call", tool="a", priority=5),
                StrategyStep(action="tool_call", tool="b", priority=5),
                StrategyStep(action="tool_call", tool="c", priority=5),
            ],
        )
        batches = strategy.group_by_priority()
        assert len(batches) == 1
        assert len(batches[0]) == 3

    def test_build_task_prompt(self):
        strategy = Strategy(component="flink", problem_type="job_failure")
        prompt = strategy.build_task_prompt({
            "problem": "TaskManager OOM",
            "cluster_id": "c-test",
            "region": "北京",
            "version": "1.17",
            "detail": "exit code 137",
        })
        assert "flink" in prompt
        assert "TaskManager OOM" in prompt
        assert "c-test" in prompt
        assert "北京" in prompt


class TestLoadStrategy:
    def test_load_from_yaml(self, sample_strategy_dir):
        strategy = load_strategy("flink", "job_failure", sample_strategy_dir)
        assert strategy.component == "flink"
        assert strategy.problem_type == "job_failure"
        assert len(strategy.steps) == 3
        assert strategy.steps[0].action == "fingerprint_match"

    def test_priority_order(self, sample_strategy_dir):
        strategy = load_strategy("flink", "job_failure", sample_strategy_dir)
        batches = strategy.group_by_priority()
        assert batches[0][0].priority == 0

    def test_fallback_to_default_strategy(self, tmp_path):
        empty_dir = str(tmp_path / "empty")
        import os
        os.makedirs(empty_dir, exist_ok=True)
        strategy = load_strategy("unknown", "unknown", empty_dir)
        assert strategy.component == "unknown"
        assert len(strategy.steps) > 0

    def test_default_strategy_has_fingerprint_first(self):
        strategy = _default_strategy("flink", "test")
        assert strategy.steps[0].action == "fingerprint_match"
        assert strategy.steps[0].priority == 0
