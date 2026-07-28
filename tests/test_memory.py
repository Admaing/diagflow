"""Tests for EvidencePool (thread safety and CRUD) and SessionMemory."""

import threading
import pytest
from diagflow.core.memory import Evidence, EvidencePool, SessionMemory


class TestEvidence:
    def test_evidence_creation(self):
        ev = Evidence(
            source_agent="strategy",
            category="log_error",
            summary="OOM in taskmanager",
            detail="java.lang.OutOfMemoryError: Java heap space",
            confidence=0.9,
        )
        assert ev.source_agent == "strategy"
        assert ev.confidence == 0.9

    def test_to_dict(self):
        ev = Evidence(
            source_agent="test", category="test", summary="s", detail="d",
        )
        d = ev.to_dict()
        assert d["source_agent"] == "test"
        assert d["category"] == "test"

    def test_short_str(self):
        ev = Evidence(
            source_agent="test", category="test", summary="OOM",
            detail="long detail", confidence=0.5,
        )
        s = ev.short_str()
        assert "test" in s
        assert "OOM" in s


class TestEvidencePool:
    def test_add_and_all(self):
        pool = EvidencePool()
        ev = Evidence(source_agent="a", category="c", summary="s", detail="d")
        pool.add(ev)
        assert len(pool.all()) == 1

    def test_add_many(self):
        pool = EvidencePool()
        evs = [
            Evidence(source_agent="a", category="c", summary="s", detail="d"),
            Evidence(source_agent="b", category="c", summary="s", detail="d"),
        ]
        pool.add_many(evs)
        assert len(pool.all()) == 2

    def test_by_agent(self):
        pool = EvidencePool()
        pool.add(Evidence(source_agent="a", category="c", summary="s", detail="d"))
        pool.add(Evidence(source_agent="b", category="c", summary="s", detail="d"))
        pool.add(Evidence(source_agent="a", category="c", summary="s", detail="d"))
        assert len(pool.by_agent("a")) == 2
        assert len(pool.by_agent("b")) == 1
        assert len(pool.by_agent("nonexistent")) == 0

    def test_by_category(self):
        pool = EvidencePool()
        pool.add(Evidence(source_agent="a", category="log", summary="s", detail="d"))
        pool.add(Evidence(source_agent="b", category="metric", summary="s", detail="d"))
        assert len(pool.by_category("log")) == 1
        assert len(pool.by_category("metric")) == 1
        assert len(pool.by_category("nonexistent")) == 0

    def test_high_confidence(self):
        pool = EvidencePool()
        pool.add(Evidence(source_agent="a", category="c", summary="s", detail="d", confidence=0.9))
        pool.add(Evidence(source_agent="b", category="c", summary="s", detail="d", confidence=0.4))
        assert len(pool.high_confidence(0.7)) == 1
        assert len(pool.high_confidence(0.3)) == 2

    def test_clear(self):
        pool = EvidencePool()
        pool.add(Evidence(source_agent="a", category="c", summary="s", detail="d"))
        pool.clear()
        assert len(pool.all()) == 0

    def test_summary_empty(self):
        pool = EvidencePool()
        s = pool.summary()
        assert "no evidence" in s.lower() or "(no evidence" in s

    def test_summary_with_items(self):
        pool = EvidencePool()
        pool.add(Evidence(source_agent="a", category="c", summary="OOM", detail="d"))
        s = pool.summary()
        assert "OOM" in s

    def test_thread_safety_concurrent_adds(self):
        """Verify EvidencePool handles concurrent writes without data loss."""
        pool = EvidencePool()
        n_threads = 10
        n_per_thread = 100

        def add_items(start_id):
            for i in range(n_per_thread):
                pool.add(Evidence(
                    source_agent=f"thread-{start_id}",
                    category="test",
                    summary=f"item-{start_id}-{i}",
                    detail="",
                ))

        threads = []
        for tid in range(n_threads):
            t = threading.Thread(target=add_items, args=(tid,))
            threads.append(t)
            t.start()

        for t in threads:
            t.join()

        assert len(pool.all()) == n_threads * n_per_thread

    def test_thread_safety_all_during_writes(self):
        """Verify all() returns consistent snapshot during concurrent writes."""
        pool = EvidencePool()
        for i in range(100):
            pool.add(Evidence(source_agent="pre", category="c", summary="s", detail="d"))

        results = []
        def reader():
            for _ in range(5):
                results.append(len(pool.all()))

        def writer():
            for i in range(50):
                pool.add(Evidence(source_agent=f"w{i}", category="c", summary="s", detail="d"))

        t1 = threading.Thread(target=reader)
        t2 = threading.Thread(target=writer)
        t1.start()
        t2.start()
        t1.join()
        t2.join()

        # All reads should have at least 100 items (no partial state visible)
        for r in results:
            assert r >= 100


class TestSessionMemory:
    def test_add_user_message(self):
        sm = SessionMemory()
        sm.add_user("Hello")
        msgs = sm.get_messages()
        assert len(msgs) == 1
        assert msgs[0]["role"] == "user"
        assert msgs[0]["content"] == "Hello"

    def test_add_assistant_message(self):
        sm = SessionMemory()
        sm.add_assistant("Response")
        assert sm.get_messages()[0]["content"] == "Response"

    def test_add_tool_result(self):
        sm = SessionMemory()
        sm.add_tool_result("call_1", "Tool output")
        msgs = sm.get_messages()
        assert msgs[0]["role"] == "tool"
        assert msgs[0]["tool_call_id"] == "call_1"

    def test_message_ordering(self):
        sm = SessionMemory()
        sm.add_user("Q1")
        sm.add_assistant("A1")
        sm.add_tool_result("t1", "R1")
        msgs = sm.get_messages()
        assert [m["role"] for m in msgs] == ["user", "assistant", "tool"]
