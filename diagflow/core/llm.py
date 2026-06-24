"""
LLM client abstraction — wraps the OpenAI-compatible API for ReAct-style tool use.

Default backend: DeepSeek (https://api.deepseek.com), which is OpenAI-API compatible.
Any OpenAI-compatible endpoint works (DeepSeek, OpenAI, Azure, local vLLM, etc.) —
just change base_url and model.

KUN (K8s) pods can access the public internet directly, so no LLM Proxy is needed.
The pod calls the LLM API over HTTPS just like a local dev machine.
"""

from __future__ import annotations

import hashlib
import os
import time
from dataclasses import dataclass
from typing import Any

from openai import AsyncOpenAI


# ---------------------------------------------------------------------------
# Response types
# ---------------------------------------------------------------------------

@dataclass
class ToolCall:
    """A tool invocation request from the LLM."""
    id: str
    name: str
    arguments: dict[str, Any]


@dataclass
class LLMResponse:
    """Parsed response from the LLM."""
    content: str | None = None
    tool_calls: list[ToolCall] = ()
    stop_reason: str | None = None
    usage: dict[str, int] | None = None


# ---------------------------------------------------------------------------
# LLM Client (OpenAI-compatible, default: DeepSeek)
# ---------------------------------------------------------------------------

class LLMClient:
    """Lightweight OpenAI-compatible client for agentic workflows.

    Works with any OpenAI-API-compatible endpoint:
      - DeepSeek (default):  base_url=https://api.deepseek.com
      - OpenAI:              base_url=https://api.openai.com/v1
      - Azure OpenAI:        base_url=https://<resource>.openai.azure.com
      - Local vLLM/Ollama:   base_url=http://localhost:11434/v1
    """

    def __init__(
        self,
        api_keys: list[str] | None = None,
        model: str = "deepseek-v4-flash",
        max_tokens: int = 4096,
        base_url: str = "https://api.modelverse.cn/v1",
    ):
        # Single key (DeepSeek model) — round-robin kept for future multi-key scenarios
        self.api_keys = api_keys or [
            os.environ.get("DEEPSEEK_API_KEY") or os.environ.get("LLM_API_KEY") or ""
        ]
        if not any(self.api_keys):
            raise ValueError(
                "No LLM API key provided. "
                "Set DEEPSEEK_API_KEY (or LLM_API_KEY) environment variable."
            )
        self.model = model
        self.max_tokens = max_tokens
        self.base_url = base_url
        # All clients share the same key (DeepSeek typically uses one key)
        self._clients = [
            AsyncOpenAI(api_key=k, base_url=base_url)
            for k in self.api_keys if k
        ]

    def _pick_client(self, session_id: str | None = None) -> AsyncOpenAI:
        """Pick a client by session hash for round-robin consistency."""
        if len(self._clients) == 1:
            return self._clients[0]
        idx = int(hashlib.md5((session_id or "").encode()).hexdigest(), 16) % len(self._clients)
        return self._clients[idx]

    async def generate(
        self,
        messages: list[dict[str, Any]],
        tools: list[dict[str, Any]] | None = None,
        system: str | None = None,
        session_id: str | None = None,
        temperature: float = 0.3,
        max_retries: int = 3,
    ) -> LLMResponse:
        """Send a message and return the parsed response.

        Handles tool-use responses transparently — the caller receives
        either text content or a list of tool calls.
        """
        client = self._pick_client(session_id)
        last_error: Exception | None = None

        # Convert to OpenAI message format
        openai_messages: list[dict[str, Any]] = []
        if system:
            openai_messages.append({"role": "system", "content": system})
        openai_messages.extend(messages)

        # Tools are already in OpenAI function-calling format from Tool.to_llm_definition()
        # {"type": "function", "function": {"name", "description", "parameters"}}
        openai_tools = tools if tools else None

        for attempt in range(max_retries):
            try:
                resp = await client.chat.completions.create(
                    model=self.model,
                    max_tokens=self.max_tokens,
                    temperature=temperature,
                    messages=openai_messages,  # type: ignore
                    tools=openai_tools,  # type: ignore
                )
            except Exception as exc:
                last_error = exc
                if attempt < max_retries - 1:
                    wait = 2 ** attempt
                    time.sleep(wait)
                continue

            # Parse response — OpenAI format
            choice = resp.choices[0]
            msg = choice.message

            tool_calls: list[ToolCall] = []
            if msg.tool_calls:
                import json
                for tc in msg.tool_calls:
                    try:
                        args = json.loads(tc.function.arguments) if tc.function.arguments else {}
                    except json.JSONDecodeError:
                        args = {}
                    tool_calls.append(ToolCall(
                        id=tc.id,
                        name=tc.function.name,
                        arguments=args,
                    ))

            return LLMResponse(
                content=msg.content,
                tool_calls=tool_calls,
                stop_reason=choice.finish_reason,
            )

        raise RuntimeError(
            f"LLM call failed after {max_retries} retries: {last_error}"
        )

    @classmethod
    def from_config(cls, config: dict) -> "LLMClient":
        """Build from a config dict (loaded from application.yaml).

        Example:
          llm:
            provider: deepseek
            model: deepseek-v4-flash
            baseUrl: "https://api.deepseek.com"
        """
        return cls(
            model=config.get("model", "deepseek-v4-flash"),
            max_tokens=config.get("maxTokens", 4096),
            base_url=config.get("baseUrl", "https://api.deepseek.com"),
        )

    def count_tokens(self, text: str) -> int:
        """Quick token estimate (~4 chars/token for Chinese-heavy text)."""
        return len(text) // 3
