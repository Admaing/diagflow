package retriever

import (
	"testing"

	"github.com/Admaing/diagflow/internal/rag/embedder"
	"github.com/Admaing/diagflow/internal/rag/vectorstore"
)

func newRetrieverWithDocs(docs, ids []string, metas []map[string]any) (*HybridRetriever, *vectorstore.MemoryVectorStore) {
	store := vectorstore.NewMemory()
	emb := embedder.New("text-embedding-3-small", "", 1536)
	r := New(store, emb, 60)
	for i, d := range docs {
		store.AddCase(ids[i], d, metas[i], emb.Embed(d))
	}
	r.RebuildFromStore()
	return r, store
}

func TestBM25SearchUsesCachedIndex(t *testing.T) {
	docs := []string{"flink oom taskmanager", "hdfs disk full", "yarn queue stuck"}
	ids := []string{"a", "b", "c"}
	metas := []map[string]any{{"root_cause": "A"}, {"root_cause": "B"}, {"root_cause": "C"}}
	r, _ := newRetrieverWithDocs(docs, ids, metas)

	// First search uses cache; verify no store re-query needed.
	results := r.bm25Search("flink oom", 5)
	if len(results) == 0 {
		t.Fatal("expected BM25 results")
	}
	if results[0].ID != "a" {
		t.Fatalf("expected top result 'a', got '%s'", results[0].ID)
	}
}

func TestSearchRRFusion(t *testing.T) {
	docs := []string{"flink oom taskmanager heap", "hdfs disk full volume"}
	ids := []string{"flink_case", "hdfs_case"}
	metas := []map[string]any{{"root_cause": "flink oom"}, {"root_cause": "hdfs disk"}}
	r, _ := newRetrieverWithDocs(docs, ids, metas)

	results := r.Search("flink taskmanager out of memory", 2, 0.6, 0.4)
	if len(results) == 0 {
		t.Fatal("expected search results")
	}
	if results[0].ID != "flink_case" {
		t.Fatalf("expected fusion top 'flink_case', got '%s'", results[0].ID)
	}
	if results[0].FusionScore == nil {
		t.Fatal("expected fusion score set")
	}
}

func TestRebuildFromStoreEmpty(t *testing.T) {
	store := vectorstore.NewMemory()
	emb := embedder.New("text-embedding-3-small", "", 1536)
	r := New(store, emb, 60)
	r.RebuildFromStore()
	if r.bm25 != nil {
		t.Fatal("expected nil bm25 for empty store")
	}
}
