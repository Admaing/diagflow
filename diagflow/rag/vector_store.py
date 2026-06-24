"""
Vector store abstraction — wraps ChromaDB for local embedding storage.

Used by the RAG system to store and retrieve historical diagnosis cases.
ChromaDB is chosen for zero-config local operation (no external server needed).
"""

from __future__ import annotations

from pathlib import Path
from typing import Any

import chromadb
from chromadb.config import Settings


class VectorStore:
    """ChromaDB-backed vector store for diagnosis cases."""

    def __init__(self, persist_dir: str = "/tmp/diagflow/chroma") -> None:
        self.persist_dir = Path(persist_dir)
        self.persist_dir.mkdir(parents=True, exist_ok=True)
        self.client = chromadb.PersistentClient(
            path=str(self.persist_dir),
            settings=Settings(anonymized_telemetry=False),
        )
        self._collection: Any = None

    @property
    def collection(self):
        if self._collection is None:
            self._collection = self.client.get_or_create_collection(
                name="diagnosis_cases",
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
        """Store a case with its embedding."""
        self.collection.add(
            ids=[case_id],
            embeddings=[embedding],
            documents=[text],
            metadatas=[metadata],
        )

    def search(
        self,
        query_embedding: list[float],
        n_results: int = 5,
    ) -> list[dict[str, Any]]:
        """Search for similar cases by embedding."""
        results = self.collection.query(
            query_embeddings=[query_embedding],
            n_results=n_results,
        )
        items = []
        for i in range(len(results["ids"][0])):
            items.append({
                "id": results["ids"][0][i],
                "document": results["documents"][0][i],
                "metadata": results["metadatas"][0][i],
                "distance": results["distances"][0][i] if results.get("distances") else 0,
            })
        return items

    def count(self) -> int:
        return self.collection.count()
