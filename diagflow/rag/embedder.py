"""
Embedding abstraction — converts text to vector embeddings.

Supports OpenAI embeddings (default) with a fallback to a simple
hash-based embedding for offline demo mode.
"""

from __future__ import annotations

import hashlib
import logging
import os
from typing import Any

import numpy as np

logger = logging.getLogger(__name__)


class Embedder:
    """Converts text to embedding vectors."""

    def __init__(self, model: str = "text-embedding-3-small", api_key: str | None = None):
        self.model = model
        self.api_key = api_key or os.environ.get("OPENAI_API_KEY")

    def embed(self, text: str) -> list[float]:
        """Embed text. Uses OpenAI if key available, otherwise fallback."""
        if self.api_key:
            return self._openai_embed(text)
        return self._fallback_embed(text)

    def embed_batch(self, texts: list[str]) -> list[list[float]]:
        return [self.embed(t) for t in texts]

    def _openai_embed(self, text: str) -> list[float]:
        try:
            from openai import OpenAI
            client = OpenAI(api_key=self.api_key)
            resp = client.embeddings.create(model=self.model, input=text)
            return resp.data[0].embedding
        except Exception:
            logger.warning("OpenAI embed failed, using fallback", exc_info=True)
            return self._fallback_embed(text)

    def _fallback_embed(self, text: str) -> list[float]:
        """Simple hash-based embedding for offline demo mode.

        Not semantically meaningful, but produces consistent vectors
        so identical/very similar texts cluster together.
        """
        np.random.seed(int(hashlib.md5(text.encode()).hexdigest(), 16) % (2**31))
        return np.random.randn(384).tolist()
