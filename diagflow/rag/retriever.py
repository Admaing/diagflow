"""
Hybrid retriever — combines semantic (vector) and keyword (BM25) search.

Uses Reciprocal Rank Fusion (RRF) to merge results from both methods.
This gives us the best of both worlds:
  - Semantic search captures intent ("memory pressure" ≈ "heap space")
  - BM25 captures exact keywords ("OutOfMemoryError", "exit code 137")
"""

from __future__ import annotations

from pathlib import Path
from typing import Any

import numpy as np
from rank_bm25 import BM25Okapi

from .embedder import Embedder
from .vector_store import VectorStore


class HybridRetriever:
    """Hybrid search: semantic + BM25 with RRF fusion."""

    def __init__(
        self,
        vector_store: VectorStore,
        embedder: Embedder,
        rrf_k: int = 60,
    ):
        self.vector_store = vector_store
        self.embedder = embedder
        self.rrf_k = rrf_k
        self._bm25: BM25Okapi | None = None
        self._bm25_corpus: list[str] = []

    def search(
        self,
        query: str,
        n_results: int = 5,
        semantic_weight: float = 0.6,
        bm25_weight: float = 0.4,
    ) -> list[dict[str, Any]]:
        """Hybrid search with configurable weighting."""
        # 1. Semantic search
        query_emb = self.embedder.embed(query)
        semantic_results = self.vector_store.search(query_emb, n_results=n_results * 2)

        # 2. BM25 search
        bm25_results = self._bm25_search(query, n_results=n_results * 2)

        # 3. RRF fusion
        scores: dict[str, float] = {}
        for rank, r in enumerate(semantic_results):
            scores[r["id"]] = scores.get(r["id"], 0) + semantic_weight / (self.rrf_k + rank)
        for rank, r in enumerate(bm25_results):
            scores[r["id"]] = scores.get(r["id"], 0) + bm25_weight / (self.rrf_k + rank)

        # Merge results
        merged = sorted(scores.items(), key=lambda x: -x[1])
        all_results = semantic_results + bm25_results
        seen = {}
        for r in all_results:
            if r["id"] not in seen:
                seen[r["id"]] = r

        final = []
        for doc_id, _score in merged[:n_results]:
            if doc_id in seen:
                seen[doc_id]["fusion_score"] = _score
                final.append(seen[doc_id])
        return final

    def _bm25_search(self, query: str, n_results: int = 5) -> list[dict[str, Any]]:
        """BM25 keyword search."""
        all_cases = self.vector_store.collection.get(include=["documents", "metadatas"])
        if not all_cases["documents"]:
            return []

        tokenized_corpus = [doc.lower().split() for doc in all_cases["documents"]]
        bm25 = BM25Okapi(tokenized_corpus)
        tokenized_query = query.lower().split()
        scores = bm25.get_scores(tokenized_query)
        top_indices = np.argsort(scores)[::-1][:n_results]

        results = []
        for idx in top_indices:
            if scores[idx] > 0:
                results.append({
                    "id": all_cases["ids"][idx],
                    "document": all_cases["documents"][idx],
                    "metadata": all_cases["metadatas"][idx],
                    "score": float(scores[idx]),
                })
        return results

    def build_index(self, documents: list[str], ids: list[str], metadatas: list[dict]) -> None:
        """Build or rebuild the BM25 index from stored cases."""
        tokenized = [doc.lower().split() for doc in documents]
        self._bm25 = BM25Okapi(tokenized)
        self._bm25_corpus = documents
