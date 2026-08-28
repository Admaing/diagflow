"""
Knowledge base — closed-loop historical case reuse.

Two retrieval modes:
  1. MD5 fingerprint match (exact, zero LLM, Phase 1 fast-path)
  2. Hybrid semantic/keyword search (ChromaDB + BM25 + RRF, Phase 1 fallback)

Fingerprint is computed from the (component, error_pattern, version) tuple.
After each successful diagnosis, the case is auto-indexed — same issue next
time returns instantly. No manual case writing needed.
"""

from __future__ import annotations

import hashlib
import json
import logging
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

from .embedder import Embedder
from .retriever import HybridRetriever
from .vector_store import VectorStore

logger = logging.getLogger(__name__)


class KnowledgeBase:
    """Closed-loop knowledge base with auto-indexing."""

    def __init__(
        self,
        vector_store: VectorStore | None = None,
        embedder: Embedder | None = None,
    ):
        self.vector_store = vector_store or VectorStore()
        if embedder is None:
            from diagflow.config import get_config
            cfg = get_config()
            embedder = Embedder(embedding_dim=cfg.vector_store.embedding_dim)
        self.embedder = embedder
        self.retriever = HybridRetriever(self.vector_store, self.embedder)
        # {md5_hash: KnownCase}
        self._fingerprints: dict[str, dict[str, Any]] = {}
        # Restore fingerprints from persisted ChromaDB metadata
        self._load_fingerprints()

    @property
    def _has_real_embeddings(self) -> bool:
        """True only when the embedder can produce semantically meaningful vectors.

        The hash fallback (no API key) is not semantically meaningful — but it
        is useless for recall and, if written, pollutes the vector store.
        """
        return self.embedder.api_key is not None

    # ------------------------------------------------------------------
    # Fingerprint persistence
    # ------------------------------------------------------------------

    def _load_fingerprints(self) -> None:
        """Rebuild in-memory fingerprint dict from ChromaDB metadata."""
        try:
            all_cases = self.vector_store.collection.get(
                include=["metadatas"]
            )
            if all_cases and all_cases.get("metadatas"):
                for i, meta in enumerate(all_cases["metadatas"]):
                    fp = meta.get("fingerprint", "")
                    if fp and fp not in self._fingerprints:
                        self._fingerprints[fp] = {
                            "case_id": all_cases["ids"][i] if all_cases.get("ids") else fp,
                            "component": meta.get("component", "unknown"),
                            "error_pattern": meta.get("error_pattern", "unknown"),
                            "version": meta.get("version", ""),
                            "root_cause": meta.get("root_cause", ""),
                            "suggestions": meta.get("suggestions", "").split(",") if isinstance(meta.get("suggestions"), str) else meta.get("suggestions", []),
                            "_hits": meta.get("_hits", 0),
                        }
                logger.info("Loaded %d fingerprints from ChromaDB", len(self._fingerprints))
        except Exception:
            logger.warning("Failed to load fingerprints from ChromaDB", exc_info=True)

    # ------------------------------------------------------------------
    # Fingerprint — fast-path (Phase 1, zero LLM)
    # ------------------------------------------------------------------

    @staticmethod
    def make_fingerprint(component: str, error_pattern: str, version: str = "") -> str:
        """MD5 of (component, error_pattern, version) — deterministic key."""
        raw = f"{component}:{error_pattern}:{version}"
        return hashlib.md5(raw.encode()).hexdigest()[:16]

    def fingerprint_match(
        self, component: str, error_pattern: str, version: str = ""
    ) -> dict | None:
        """Phase 1 fast-path: exact MD5 match. Returns cached case or None.

        Skips disputed cases (marked via mark_incorrect).
        """
        fp = self.make_fingerprint(component, error_pattern, version)
        match = self._fingerprints.get(fp)
        if match and not match.get("_disputed"):
            match["_hits"] = match.get("_hits", 0) + 1
            return match
        return None

    # ------------------------------------------------------------------
    # Indexing — auto-inserted after each successful diagnosis
    # ------------------------------------------------------------------

    def add_case(
        self,
        component: str,
        error_pattern: str,
        version: str,
        root_cause: str,
        suggestions: list[str],
        evidence_summary: str = "",
    ) -> str:
        """Index a case (called automatically by engine Phase 5)."""
        fp = self.make_fingerprint(component, error_pattern, version)
        case_id = f"{component}_{error_pattern}_{fp[:8]}"

        # Existing case? Update hit count and merge suggestions.
        existing = self._fingerprints.get(fp)
        if existing:
            existing["_hits"] = existing.get("_hits", 0) + 1
            existing["suggestions"] = list(
                set(existing.get("suggestions", []) + suggestions)
            )
            existing["last_seen"] = datetime.now(timezone.utc).isoformat()
            # Overwrite root_cause if we have a better one (not from empty restore)
            if root_cause and not existing.get("root_cause"):
                existing["root_cause"] = root_cause
            return fp

        # New case.
        case_text = (
            f"Component: {component} v{version}\n"
            f"Error pattern: {error_pattern}\n"
            f"Root cause: {root_cause}\n"
            f"Suggestions:\n"
            + "\n".join(f"- {s}" for s in suggestions)
        )

        embedding = self.embedder.embed(case_text)
        if self._has_real_embeddings:
            self.vector_store.add_case(
                case_id=case_id,
                text=case_text,
                metadata={
                    "component": component,
                    "error_pattern": error_pattern,
                    "version": version,
                    "fingerprint": fp,
                    "root_cause": root_cause,
                    "suggestions": ",".join(suggestions) if suggestions else "",
                    "created": datetime.now(timezone.utc).isoformat(),
                },
                embedding=embedding,
            )

        self._fingerprints[fp] = {
            "case_id": case_id,
            "component": component,
            "error_pattern": error_pattern,
            "version": version,
            "root_cause": root_cause,
            "suggestions": suggestions,
            "_hits": 1,
            "created": datetime.now(timezone.utc).isoformat(),
        }

        # Rebuild BM25 so new case is searchable.
        self._rebuild_bm25()
        return fp

    def add_case_from_text(self, text: str, metadata: dict[str, Any] | None = None) -> str | None:
        """Convenience: add from raw text, auto-extract error pattern.

        Called by engine Phase 5 after _kb_index().
        """
        meta = metadata or {}
        component = meta.get("component", "unknown")
        version = meta.get("version", "")
        # Extract error pattern from text — simple keyword extraction.
        error_pattern = self._extract_error_pattern(text)
        root_cause = text.split("\n")[0][:200] if text else "unknown"
        suggestions = (
            [l[2:] for l in text.split("\n") if l.startswith("- ")]
            if text else []
        )
        return self.add_case(component, error_pattern, version, root_cause, suggestions)

    # ------------------------------------------------------------------
    # Search — Phase 1 fallback (semantic + keyword)
    # ------------------------------------------------------------------

    def search(self, query: str, n: int = 5) -> list[dict[str, Any]]:
        """Hybrid search across all indexed cases."""
        return self.retriever.search(query, n_results=n)

    # ------------------------------------------------------------------
    # Bulk loading — seed cases from Markdown files
    # ------------------------------------------------------------------

    def add_case_from_md(self, md_path: str) -> None:
        """Index a Markdown case file (for seed data)."""
        path = Path(md_path)
        if not path.exists():
            raise FileNotFoundError(f"Case file not found: {md_path}")

        text = path.read_text(encoding="utf-8")
        meta = self._parse_frontmatter(text)
        body = self._strip_frontmatter(text)

        case_id = path.stem
        component = meta.get("component", "unknown")
        error_pattern = meta.get("error_pattern", "unknown")
        version = meta.get("version", "")
        # Extract root cause: prefer frontmatter, then first non-heading line
        root_cause = meta.get("root_cause", "")
        if not root_cause:
            for line in body.split("\n"):
                stripped = line.strip()
                if stripped and not stripped.startswith("#"):
                    root_cause = stripped[:200]
                    break
            if not root_cause:
                root_cause = "unknown"

        # Fingerprint COMPUTED, not read from file.
        fp = self.make_fingerprint(component, error_pattern, version)

        embedding = self.embedder.embed(body)
        if self._has_real_embeddings:
            self.vector_store.add_case(
                case_id=case_id,
                text=body,
                metadata={**meta, "fingerprint": fp},
                embedding=embedding,
            )
        self._fingerprints[fp] = {
            "case_id": case_id,
            "component": component,
            "error_pattern": error_pattern,
            "version": version,
            "root_cause": root_cause,
            "suggestions": meta.get("suggestions", "").split(","),
            "_hits": 0,
        }

    def load_cases_from_dir(self, dir_path: str) -> int:
        """Load all .md case files from a directory."""
        count = 0
        for f in Path(dir_path).glob("*.md"):
            try:
                self.add_case_from_md(str(f))
                count += 1
            except Exception as exc:
                logger.warning("Failed to load case %s: %s", f, exc)
        self._rebuild_bm25()
        return count

    # ------------------------------------------------------------------
    # Helpers
    # ------------------------------------------------------------------

    @staticmethod
    def _extract_error_pattern(text: str) -> str:
        """Extract key error keywords from text."""
        from diagflow.config import get_config
        cfg = get_config()
        keywords = cfg.rag.error_keywords
        found = [k for k in keywords if k in text]
        return found[0] if found else text[:50].strip()

    @staticmethod
    def _parse_frontmatter(text: str) -> dict[str, Any]:
        metadata: dict[str, Any] = {}
        if text.startswith("---"):
            parts = text.split("---", 2)
            if len(parts) >= 3:
                for line in parts[1].strip().split("\n"):
                    if ":" in line:
                        k, v = line.split(":", 1)
                        metadata[k.strip()] = v.strip()
        return metadata

    @staticmethod
    def _strip_frontmatter(text: str) -> str:
        if text.startswith("---"):
            parts = text.split("---", 2)
            if len(parts) >= 3:
                return parts[2].strip()
        return text

    def _rebuild_bm25(self) -> None:
        all_cases = self.vector_store.collection.get(
            include=["documents", "metadatas"]
        )
        if all_cases and all_cases.get("documents"):
            self.retriever.build_index(
                documents=all_cases["documents"],
                ids=all_cases["ids"],
                metadatas=all_cases["metadatas"],
            )

    def stats(self) -> dict[str, Any]:
        return {
            "fingerprint_cases": len(self._fingerprints),
            "chroma_docs": self.vector_store.count(),
        }

    def mark_incorrect(self, fp: str) -> None:
        """Mark a fingerprint as disputed — prevents future fast-path hits."""
        if fp in self._fingerprints:
            self._fingerprints[fp]["_disputed"] = True
            logger.info("Marked fingerprint as disputed: %s", fp)

    def on_feedback_incorrect(self, component: str, error_pattern: str, version: str = "",
                              comment: str = "") -> str | None:
        """User marked a diagnosis as incorrect → dispute the case in KB.

        Returns the disputed fingerprint, or None if no match found.
        """
        fp = self.make_fingerprint(component, error_pattern, version)
        self.mark_incorrect(fp)
        if comment:
            self._fingerprints[fp]["_dispute_comment"] = comment
        return fp
