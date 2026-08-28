"""Tests for v3 tool definitions — command safety, HMAC signing, factory."""

import hashlib
import hmac

import pytest

from diagflow.tools.v3tools import (
    _validate_command,
    build_v3_tools,
)
from diagflow.infra import UmrAgentClient


class TestValidateCommand:
    def test_allowed_simple_command(self):
        allowed, reason = _validate_command("grep ERROR /var/log/app.log")
        assert allowed is True, reason

    def test_allowed_pipe_with_allowed_first_word(self):
        allowed, reason = _validate_command("grep ERROR app.log | tail -50")
        assert allowed is True, reason

    def test_disallowed_command_not_in_allowlist(self):
        allowed, reason = _validate_command("rm -rf /tmp")
        assert allowed is False
        assert "rm" in reason or "allowlist" in reason

    def test_forbidden_substring_in_command(self):
        allowed, reason = _validate_command("cat app.log; rm -rf /tmp")
        assert allowed is False
        assert "Forbidden" in reason

    def test_forbidden_redirection(self):
        allowed, reason = _validate_command("echo hi > /etc/passwd")
        assert allowed is False
        assert "Forbidden" in reason

    def test_forbidden_command_substitution(self):
        allowed, reason = _validate_command("echo $(whoami)")
        assert allowed is False
        assert "Forbidden" in reason


class TestHmacSignature:
    def test_sign_matches_nodejs_reference(self):
        """HMAC-SHA1 over sorted-key values, byte-identical to util/agent.js."""
        params = {"Action": "GetLogs", "Date": "1720000000000", "Path": "/a.log"}
        key = "secret-agent-key"
        expected = hmac.new(
            key.encode("utf-8"),
            "".join(params[k] for k in sorted(params)).encode("utf-8"),
            hashlib.sha1,
        ).hexdigest()
        assert UmrAgentClient._sign(params, key) == expected

    def test_sign_is_stable(self):
        params = {"b": "2", "a": "1"}
        key = "k"
        assert UmrAgentClient._sign(params, key) == UmrAgentClient._sign(params, key)


class TestBuildTools:
    def test_build_v3_tools_returns_five(self):
        cluster = object()  # tools are closures; cluster is only passed through
        tools = build_v3_tools(cluster)
        names = {t.name for t in tools}
        assert names == {
            "query_yarn",
            "ssh_exec",
            "call_umr_agent",
            "deepwiki_query",
            "fingerprint_match",
        }
