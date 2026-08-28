"""Tests for HybridRetriever — BM25 caching and RRF fusion."""

from diagflow.rag.retriever import HybridRetriever
from diagflow.rag.embedder import Embedder


class _FakeVectorStore:
    """Vector store double whose semantic results and collection.get are steered."""

    def __init__(self, semantic_results=None, docs=None):
        self._semantic = semantic_results or []
        self._docs = docs or {}
        self.get_calls = 0

    def search(self, query_embedding, n_results=5):
        return self._semantic[:n_results]

    @property
    def collection(self):
        return self

    def get(self, include=None):
        self.get_calls += 1
        return {
            "ids": self._docs.get("ids", []),
            "documents": self._docs.get("documents", []),
            "metadatas": self._docs.get("metadatas", []),
        }


class _FakeEmbedder:
    api_key = "present"  # non-None so semantic path is exercised
    model = "fake"

    def embed(self, text):
        return [0.1] * 1536


def _make_retriever(semantic=None, docs=None):
    store = _FakeVectorStore(semantic_results=semantic, docs=docs)
    return HybridRetriever(store, _FakeEmbedder()), store


class TestBm25Cache:
    def test_first_search_builds_index_from_store(self):
        docs = {
            "ids": ["a", "b"],
            "documents": ["flink oom taskmanager", "hdfs disk full"],
            "metadatas": [{"c": "x"}, {"c": "y"}],
        }
        retriever, store = _make_retriever(docs=docs)
        retriever.search("flink oom")
        assert store.get_calls == 1

    def test_second_search_reuses_cached_index(self):
        docs = {
            "ids": ["a", "b"],
            "documents": ["flink oom taskmanager", "hdfs disk full"],
            "metadatas": [{"c": "x"}, {"c": "y"}],
        }
        retriever, store = _make_retriever(docs=docs)
        retriever.search("flink oom")
        retriever.search("flink oom")
        # Second call must not re-fetch the whole corpus
        assert store.get_calls == 1

    def test_build_index_then_search_uses_index(self):
        docs = {
            "ids": ["a", "b"],
            "documents": ["flink oom taskmanager", "hdfs disk full"],
            "metadatas": [{"c": "x"}, {"c": "y"}],
        }
        retriever, store = _make_retriever(docs=docs)
        retriever.build_index(docs["documents"], docs["ids"], docs["metadatas"])
        retriever.search("flink oom")
        # build_index pre-seeds the cache, so collection.get should never fire
        assert store.get_calls == 0


class TestRrfFusion:
    def test_fusion_sorts_by_combined_score(self):
        semantic = [
            {"id": "a", "document": "doc a", "metadata": {}, "distance": 0.1},
            {"id": "b", "document": "doc b", "metadata": {}, "distance": 0.2},
        ]
        # BM25 will come from the store's docs via the corpus cache path.
        docs = {
            "ids": ["a", "b"],
            "documents": ["doc a flink", "doc b"],
            "metadatas": [{}, {}],
        }
        retriever, store = _make_retriever(semantic=semantic, docs=docs)
        retriever.build_index(docs["documents"], docs["ids"], docs["metadatas"])
        results = retriever.search("flink")
        # Result "a" appears in both semantic and BM25 ranks → highest fusion score
        assert results[0]["id"] == "a"
        assert "fusion_score" in results[0]
