"""Tests for KnowledgeBase fingerprint matching and persistence."""

import pytest
from diagflow.rag.knowledge_base import KnowledgeBase


class TestFingerprint:
    def test_make_fingerprint_deterministic(self):
        fp1 = KnowledgeBase.make_fingerprint("flink", "OOM", "1.17")
        fp2 = KnowledgeBase.make_fingerprint("flink", "OOM", "1.17")
        assert fp1 == fp2
        assert len(fp1) == 16

    def test_make_fingerprint_different_inputs(self):
        fp1 = KnowledgeBase.make_fingerprint("flink", "OOM", "1.17")
        fp2 = KnowledgeBase.make_fingerprint("flink", "CheckpointExpired", "1.17")
        assert fp1 != fp2

    def test_fingerprint_match_hit(self):
        kb = KnowledgeBase()
        fp = kb.add_case("flink", "OOM", "1.17", "Root cause text", ["Fix 1", "Fix 2"])
        hit = kb.fingerprint_match("flink", "OOM", "1.17")
        assert hit is not None
        assert hit["root_cause"] == "Root cause text"
        assert len(hit["suggestions"]) == 2

    def test_fingerprint_match_miss(self):
        kb = KnowledgeBase()
        hit = kb.fingerprint_match("flink", "unknown_error", "1.17")
        assert hit is None

    def test_fingerprint_hit_count(self):
        kb = KnowledgeBase()
        fp = kb.add_case("flink", "OOM", "1.17", "Root cause", ["Fix 1"])
        kb.fingerprint_match("flink", "OOM", "1.17")
        kb.fingerprint_match("flink", "OOM", "1.17")
        # The match is by fp key, verify hit count
        assert kb._fingerprints[fp]["_hits"] >= 2

    def test_mark_incorrect_prevents_match(self):
        kb = KnowledgeBase()
        fp = kb.add_case("flink", "OOM", "1.17", "Wrong diagnosis", ["Fix"])
        # Mark as incorrect
        kb.mark_incorrect(fp)
        # Should no longer match
        hit = kb.fingerprint_match("flink", "OOM", "1.17")
        assert hit is None


class TestAddCase:
    def test_add_case_new(self):
        kb = KnowledgeBase()
        fp = kb.add_case("flink", "OOM", "1.17", "Root cause", ["Fix 1"])
        assert fp is not None
        assert len(fp) == 16
        assert len(kb._fingerprints) >= 1

    def test_add_case_merge_suggestions(self):
        kb = KnowledgeBase()
        kb.add_case("flink", "OOM", "1.17", "Root cause", ["Fix 1"])
        kb.add_case("flink", "OOM", "1.17", "Root cause v2", ["Fix 2", "Fix 3"])
        hit = kb.fingerprint_match("flink", "OOM", "1.17")
        # Suggestions should be merged
        assert len(hit["suggestions"]) >= 2

    def test_add_case_from_text(self):
        kb = KnowledgeBase()
        text = "OutOfMemoryError in Flink TaskManager\n- Increase heap\n- Reduce slots"
        fp = kb.add_case_from_text(text, {"component": "flink", "version": "1.17"})
        assert fp is not None
        hit = kb.fingerprint_match("flink", "OutOfMemoryError", "1.17")
        assert hit is not None


class TestExtractErrorPattern:
    def test_oom_detected(self):
        pattern = KnowledgeBase._extract_error_pattern(
            "java.lang.OutOfMemoryError: Java heap space"
        )
        assert pattern == "OutOfMemoryError"

    def test_checkpoint_detected(self):
        pattern = KnowledgeBase._extract_error_pattern(
            "Checkpoint expired before completing"
        )
        # "timeout" or "CheckpointExpired" should be detected
        assert pattern in ("CheckpointExpired", "timeout") or "checkpoint" in pattern.lower()

    def test_fallback_to_prefix(self):
        long_text = "x" * 200 + "SomeCustomError: bad thing happened"
        pattern = KnowledgeBase._extract_error_pattern(long_text)
        assert len(pattern) <= 50
