"""
Vector store abstraction — ChromaDB or Milvus Lite.

Set ``DIAGFLOW_VECTOR_STORE__BACKEND=milvus`` to use Milvus Lite (embedded,
no server needed). Defaults to ChromaDB.

The ``VectorStore`` class is a factory — upstream code doesn't know
which backend is active.
"""

from __future__ import annotations

import logging
from pathlib import Path
from typing import Any

logger = logging.getLogger(__name__)


# ============================================================================
# Factory
# ============================================================================


class VectorStore:
    """Unified vector store — picks ChromaDB or Milvus based on config."""

    def __new__(cls, persist_dir: str = "", **kwargs):
        from diagflow.config import get_config

        cfg = get_config()
        backend = cfg.vector_store.backend
        if backend == "milvus":
            try:
                return _MilvusVectorStore(**kwargs)
            except Exception:
                logger.warning(
                    "Milvus init failed — pymilvus not installed or unreachable. "
                    "Falling back to ChromaDB.",
                    exc_info=True,
                )
        if backend == "chromadb" or backend == "milvus":
            return _ChromaVectorStore(persist_dir=persist_dir, **kwargs)

        logger.warning("Unknown vector store backend '%s', falling back to ChromaDB", backend)
        return _ChromaVectorStore(persist_dir=persist_dir, **kwargs)


# ============================================================================
# ChromaDB backend
# ============================================================================


class _ChromaVectorStore:
    """ChromaDB-backed vector store."""

    def __init__(self, persist_dir: str = "", **kwargs) -> None:
        import chromadb
        from chromadb.config import Settings

        from diagflow.config import get_config
        cfg = get_config()
        self.persist_dir = Path(persist_dir or cfg.vector_store.chroma_persist_dir)
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

    def add_case(self, case_id: str, text: str, metadata: dict[str, Any],
                 embedding: list[float]) -> None:
        try:
            self.collection.upsert(
                ids=[case_id],
                embeddings=[embedding],
                documents=[text],
                metadatas=[metadata],
            )
        except Exception:
            logger.warning("ChromaDB upsert failed for %s", case_id, exc_info=True)

    def search(self, query_embedding: list[float], n_results: int = 5) -> list[dict[str, Any]]:
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
        """Release the ChromaDB client and cached collection.

        chromadb.PersistentClient has no public close() in current versions,
        so this is a best-effort dereference — the intent is to drop references
        so the embedded process can GC the client. Idempotent.
        """
        try:
            self._collection = None
            if getattr(self, "client", None) is not None:
                # Some chromadb builds expose a private _system / close hook.
                stop = getattr(self.client, "close", None)
                if callable(stop):
                    stop()
        except Exception as exc:
            logger.debug("ChromaDB close cleanup failed", exc_info=True)


# ============================================================================
# Milvus backend (lazy import — pymilvus only loaded when selected)
# ============================================================================


class _MilvusVectorStore:
    """Milvus Lite / Standalone vector store.

    Uses MilvusClient (pymilvus). Milvus Lite: file-based, zero deps beyond pip.
    """

    def __init__(self, **kwargs) -> None:
        from diagflow.config import get_config
        cfg = get_config()
        self._collection_name = cfg.vector_store.collection_name
        self._dim = cfg.vector_store.embedding_dim
        self._db_path = cfg.vector_store.milvus_db_path

        import pymilvus
        self._client = pymilvus.MilvusClient(self._db_path)
        self._adapter = _MilvusAdapter(self._client, self._collection_name)
        self._ensure_collection()

    def _ensure_collection(self) -> None:
        try:
            if self._client.has_collection(self._collection_name):
                self._client.load_collection(self._collection_name)
                return
            self._client.create_collection(
                collection_name=self._collection_name,
                dimension=self._dim,
                metric_type="COSINE",
                auto_id=False,
                enable_dynamic_field=True,
            )
            # Create index for fast search
            self._client.create_index(
                collection_name=self._collection_name,
                field_name="vector",
                index_type="IVF_FLAT",
                metric_type="COSINE",
                params={"nlist": 128},
            )
            logger.info("Milvus collection created: %s (dim=%d)", self._collection_name, self._dim)
        except Exception:
            logger.warning("Milvus collection init failed", exc_info=True)

    @property
    def collection(self):
        """Return adapter for collection.get() / .delete() compatibility."""
        return self._adapter

    def add_case(self, case_id: str, text: str, metadata: dict[str, Any],
                 embedding: list[float]) -> None:
        """Store or update (upsert) a case."""
        try:
            self._client.upsert(
                collection_name=self._collection_name,
                data=[{
                    "id": case_id,
                    "vector": embedding,
                    "text": text,
                    **{k: str(v)[:512] if v is not None else ""
                       for k, v in metadata.items()},
                }],
            )
        except Exception:
            logger.warning("Milvus upsert failed for %s", case_id, exc_info=True)

    def search(self, query_embedding: list[float], n_results: int = 5) -> list[dict[str, Any]]:
        """Search by embedding. Returns list of {id, document, metadata, distance}."""
        try:
            results = self._client.search(
                collection_name=self._collection_name,
                data=[query_embedding],
                limit=n_results,
                output_fields=["text", "id"],
            )
        except Exception:
            logger.warning("Milvus search failed", exc_info=True)
            return []

        items: list[dict[str, Any]] = []
        for hit in (results[0] if results else []):
            entity = hit.get("entity", {}) or hit
            items.append({
                "id": entity.get("id", ""),
                "document": entity.get("text", ""),
                "metadata": {
                    k: v for k, v in entity.items()
                    if k not in ("id", "vector", "text")
                },
                "distance": hit.get("distance", 1.0),
            })
        return items

    def count(self) -> int:
        try:
            stats = self._client.get_collection_stats(self._collection_name)
            return stats.get("row_count", 0)
        except Exception:
            logger.warning("Milvus count failed", exc_info=True)
            return 0

    def close(self) -> None:
        try:
            self._client.close()
        except Exception:
            pass


class _MilvusAdapter:
    """Provides collection.get() and collection.delete() — ChromaDB API compatible."""

    def __init__(self, client, collection_name: str):
        self._client = client
        self._name = collection_name

    def get(self, include: list[str] | None = None):
        """Fetch all entities, formatted like ChromaDB's collection.get()."""
        include = include or ["documents", "metadatas"]
        try:
            # Query all with a tautology expression
            results = self._client.query(
                collection_name=self._name,
                filter="id != ''",
                output_fields=["id", "text"],
                limit=10000,
            )
        except Exception:
            logger.warning("Milvus get_all failed", exc_info=True)
            return {"ids": [], "documents": [], "metadatas": []}

        ids = [r["id"] for r in results]
        docs = []
        metas = []
        for r in results:
            docs.append(r.get("text", ""))
            metas.append({
                k: v for k, v in r.items()
                if k not in ("id", "vector", "text")
            })
        out: dict[str, Any] = {"ids": ids}
        if "documents" in include:
            out["documents"] = docs
        if "metadatas" in include:
            out["metadatas"] = metas
        return out

    def delete(self, ids: list[str]) -> None:
        try:
            self._client.delete(
                collection_name=self._name,
                ids=ids,
            )
        except Exception:
            logger.warning("Milvus delete failed", exc_info=True)
