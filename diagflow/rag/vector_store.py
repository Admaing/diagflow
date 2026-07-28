"""
Vector store abstraction — wraps ChromaDB for local embedding storage.

Used by the RAG system to store and retrieve historical diagnosis cases.
ChromaDB is chosen for zero-config local operation (no external server needed).
"""

from __future__ import annotations

import logging
from pathlib import Path
from typing import Any

import chromadb
from chromadb.config import Settings

logger = logging.getLogger(__name__)


class VectorStore:
    """ChromaDB-backed vector store for diagnosis cases."""

    def __init__(self, persist_dir: str = "") -> None:
        from diagflow.config import get_config
        cfg = get_config()
        self.persist_dir = Path(persist_dir or cfg.vector_store.persist_dir)
        self.persist_dir.mkdir(parents=True, exist_ok=True)
        self._collection_name = cfg.vector_store.collection_name
        self.client = chromadb.PersistentClient(
            path=str(self.persist_dir),
            settings=Settings(anonymized_telemetry=False),
        )
        self._collection: Any = None

    @property
    def collection(self):
        if self._collection is None:
            self._collection = self.client.get_or_create_collection(
                name=self._collection_name,
                metadata={"hnsw:space": "cosine"},
            )
        return self._collection

    def add_case(
        self,
        case_id: str,
        text: str,
        metadata: dict[str, Any],
        embedding: list[float],
    ) -> None:
        """Store or update a case with its embedding (idempotent upsert)."""
        try:
            self.collection.upsert(
                ids=[case_id],
                embeddings=[embedding],
                documents=[text],
                metadatas=[metadata],
            )
        except Exception:
            logger.warning("ChromaDB upsert failed for %s", case_id, exc_info=True)

    def search(
        self,
        query_embedding: list[float],
        n_results: int = 5,
    ) -> list[dict[str, Any]]:
        """Search for similar cases by embedding. Returns empty list on error."""
        try:
            results = self.collection.query(
                query_embeddings=[query_embedding],
                n_results=n_results,
            )
        except Exception:
            logger.warning("ChromaDB query failed", exc_info=True)
            return []

        items = []
        ids = results.get("ids", [[]])[0] if results.get("ids") else []
        if not ids:
            return []
        docs = results.get("documents", [[""]])[0] if results.get("documents") else [""] * len(ids)
        metas = results.get("metadatas", [[{}]])[0] if results.get("metadatas") else [{}] * len(ids)
        dists = results.get("distances", [[0.0]])[0] if results.get("distances") else [0.0] * len(ids)

        for i in range(len(ids)):
            items.append({
                "id": ids[i],
                "document": docs[i] if i < len(docs) else "",
                "metadata": metas[i] if i < len(metas) else {},
                "distance": dists[i] if i < len(dists) else 0.0,
            })
        return items

    def count(self) -> int:
        try:
            return self.collection.count()
        except Exception:
            logger.warning("ChromaDB count failed", exc_info=True)
            return 0

    def close(self) -> None:
        """Release ChromaDB resources."""
        try:
            # PersistentClient doesn't need explicit close, but clear refs
            self._collection = None
        except Exception:
            pass
