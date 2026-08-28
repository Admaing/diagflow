"""Tests for DiagAgent core flows — using fake LLM + KB, no network."""

from types import SimpleNamespace

import pytest

from diagflow.core.diag_agent import DiagAgent, ToolDef
from diagflow.core.memory import EvidencePool, Evidence
from diagflow.core.strategy import Strategy, StrategyStep


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------


def _text_block(text):
    return SimpleNamespace(type="text", text=text)


def _tool_use_block(name, input_dict, _id="toolu_1"):
    return SimpleNamespace(type="tool_use", name=name, input=input_dict, id=_id)


def _synthesis_response(root_cause, confidence="high", suggestions=None):
    return SimpleNamespace(content=[
        _tool_use_block(
            "report_diagnosis",
            {
                "root_cause": root_cause,
                "confidence": confidence,
                "suggestions": suggestions or ["Increase heap", "Reduce slots"],
            },
        )
    ])


def _no_tool_response(text="ROOT_CAUSE: default\nCONFIDENCE: medium\n- fix 1"):
    return SimpleNamespace(content=[_text_block(text)])


class _FakeValidator:
    def __init__(self, passed=True):
        self.passed = passed
        self.validate_calls = 0

    async def validate(self, root_cause, suggestions, evidence_count):
        self.validate_calls += 1
        return (True, None) if self.passed else (False, "needs more detail")


class _FakeKb:
    class Embedder:
        api_key = None

    def __init__(self):
        self.embedder = self.Embedder()
        self.search_calls = 0
        self.index_calls = 0

    def search(self, query, n=3):
        self.search_calls += 1
        return []

    def fingerprint_match(self, component, error_pattern, version=""):
        return None

    def add_case(self, *args, **kwargs):
        self.index_calls += 1


class _FakeClient:
    def __init__(self):
        self.responses = []
        self.calls = 0

    def create(self, *args, **kwargs):
        self.calls += 1
        if self.responses:
            return self.responses.pop(0)
        return _no_tool_response()


def _make_agent(tools=(), strategy=None, client=None, kb=None):
    agent = DiagAgent(api_key="sk-test")
    agent.model = "deepseek-v4-flash"
    agent.client = SimpleNamespace(messages=(client or _FakeClient()))
    agent.kb = kb if kb is not None else _FakeKb()
    agent.validator = _FakeValidator(passed=True)
    for t in tools:
        agent.register_tool(t)
    agent._strategy = strategy
    return agent


# ---------------------------------------------------------------------------
# Phase 1 — KB fast path
# ---------------------------------------------------------------------------


class TestPhase1KbFastPath:
    @pytest.mark.asyncio
    async def test_kb_hit_short_circuits(self, monkeypatch):
        kb = _FakeKb()
        kb.embedder.api_key = "present"  # enable Phase 1 semantic search
        kb.search = lambda query, n=3: [{
            "id": "a",
            "document": "doc",
            "metadata": {"root_cause": "flink oom", "suggestions": "Increase heap"},
            "fusion_score": 0.5,
        }]

        agent = _make_agent(kb=kb)

        # Patch the module-level load_strategy so diagnose() can proceed.
        import diagflow.core.diag_agent as mod
        monkeypatch.setattr(mod, "load_strategy", lambda *a, **k: Strategy(component="flink", problem_type="job_failure", steps=[]))

        report = await agent.diagnose("flink", "job_failure", {"component": "flink"})
        assert report.matched_knowledge is True
        assert report.confidence == "high"
        # No LLM calls: the fast path returns before Phase 3
        assert agent.client.messages.calls == 0


# ---------------------------------------------------------------------------
# Phase 2 — strategy execution
# ---------------------------------------------------------------------------


class TestPhase2StrategyExecution:
    @pytest.mark.asyncio
    async def test_strategy_step_invokes_tool(self):
        calls = []

        async def handler(**kw):
            calls.append(("query_yarn", kw))
            return "1 app: flink-job FAILED"

        agent = _make_agent(
            tools=[ToolDef(name="query_yarn", description="d", input_schema={}, handler=handler)],
        )
        evidence = EvidencePool()
        strategy = Strategy(
            component="flink",
            problem_type="job_failure",
            steps=[StrategyStep(action="tool_call", tool="query_yarn", params={"action": "list_apps"}, priority=1)],
        )
        await agent._run_strategy(strategy, {"component": "flink"}, evidence)
        assert calls == [("query_yarn", {"action": "list_apps"})]
        assert len(evidence.all()) == 1

    @pytest.mark.asyncio
    async def test_unknown_tool_yields_error_evidence(self):
        agent = _make_agent(tools=[])  # no tools registered
        evidence = EvidencePool()
        strategy = Strategy(
            component="flink",
            problem_type="job_failure",
            steps=[StrategyStep(action="tool_call", tool="no_such_tool", params={}, priority=1)],
        )
        await agent._run_strategy(strategy, {"component": "flink"}, evidence)
        items = evidence.all()
        assert any("unknown tool" in e.summary for e in items)
        assert items[0].confidence == 0.0


# ---------------------------------------------------------------------------
# Phase 4 — synthesis + validation
# ---------------------------------------------------------------------------


class TestPhase4Synthesis:
    @pytest.mark.asyncio
    async def test_synthesis_structured_output(self):
        client = _FakeClient()
        client.responses.append(_synthesis_response("TaskManager OOM from 2G heap", "high", ["Increase heap"]))
        agent = _make_agent(client=client)

        evidence = EvidencePool()
        evidence.add(Evidence(source_agent="strategy", category="tool:x", summary="OOM", detail="OutOfMemoryError"))

        root_cause, suggestions, confidence = await agent._synthesize(
            evidence, {"component": "flink"}, "flink", "job_failure"
        )
        assert root_cause == "TaskManager OOM from 2G heap"
        assert confidence == "high"
        assert suggestions == ["Increase heap"]

    @pytest.mark.asyncio
    async def test_synthesis_falls_back_to_text(self):
        client = _FakeClient()  # returns default text-only response
        agent = _make_agent(client=client)
        evidence = EvidencePool()
        evidence.add(Evidence(source_agent="s", category="c", summary="s", detail="d"))
        root_cause, suggestions, confidence = await agent._synthesize(
            evidence, {"component": "flink"}, "flink", "job_failure"
        )
        assert root_cause  # non-empty fallback
        assert suggestions
