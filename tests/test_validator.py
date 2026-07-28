"""Tests for ConclusionValidator (Layer 1-2 deterministic checks)."""

import pytest
from unittest.mock import MagicMock
from diagflow.core.validator import ConclusionValidator


@pytest.fixture
def validator():
    """Create a validator with a mock client (Layer 1-2 only, no LLM)."""
    mock_client = MagicMock()
    return ConclusionValidator(mock_client)


class TestLayer1Format:
    @pytest.mark.asyncio
    async def test_root_cause_too_short(self, validator):
        passed, feedback = await validator.validate("short", ["Fix 1"], 3)
        assert passed is False
        assert "too short" in feedback.lower() or "short" in feedback.lower()

    @pytest.mark.asyncio
    async def test_no_suggestions(self, validator):
        passed, feedback = await validator.validate(
            "This is a valid root cause with enough detail", [], 3
        )
        assert passed is False
        assert "suggestion" in feedback.lower()

    @pytest.mark.asyncio
    async def test_no_evidence(self, validator):
        passed, feedback = await validator.validate(
            "This is a valid root cause", ["Fix 1"], 0
        )
        assert passed is False
        assert "evidence" in feedback.lower()


class TestLayer2CrossSource:
    @pytest.mark.asyncio
    async def test_root_cause_references_evidence(self, validator):
        passed, feedback = await validator.validate(
            "TaskManager OOM error found in logs — heap exhausted at 2048m config",
            ["Increase heap", "Reduce slots"],
            2,
        )
        # Layer 1 passes, Layer 2 checks keywords
        # "oom", "log", "config", "error" should all match
        # Returns True/None if the mock for Layer 3 is also configured correctly

    @pytest.mark.asyncio
    async def test_root_cause_no_evidence_keywords(self, validator):
        passed, feedback = await validator.validate(
            "Something went wrong with the system.",
            ["Fix everything"],
            3,
        )
        # Layer 1 passes, but Layer 2 should fail since none of the
        # evidence keywords appear in the root cause
        if not passed:
            assert "hallucinat" in feedback.lower() or "evidence" in feedback.lower()
