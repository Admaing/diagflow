// Package vectorstore mirrors diagflow/rag/vector_store.py — a store abstraction
// with an in-memory implementation for offline demo/tests. The interface holds
// the same shape as the Python ChromaDB-backed store so upstream code sees a
// uniform API regardless of backend.
package vectorstore

import (
	"math"
	"sort"
	"sync"
)

// SearchResult mirrors the Python retriever's expected dict per hit.
type SearchResult struct {
	ID          string
	Document    string
	Metadata    map[string]any
	Distance    float64
	FusionScore *float64
}

// GetResult is the shape returned by collection.get().
type GetResult struct {
	IDs       []string
	Documents []string
	Metadatas []map[string]any
}

// VectorStore is the unified interface implemented by all backends.
type VectorStore interface {
	AddCase(caseID, text string, metadata map[string]any, embedding []float64)
	Search(queryEmbedding []float64, nResults int) []SearchResult
	Count() int
	Collection() StoreCollection
	Close()
}

// StoreCollection exposes collection.get()/collection.delete()-style access.
type StoreCollection interface {
	Get(include []string) GetResult
	Delete(ids []string)
}

// MemoryVectorStore is a simple in-memory backend for demo mode and tests.
type MemoryVectorStore struct {
	mu         sync.Mutex
	cases      []doc
	collection *memoryCollection
}

type doc struct {
	ID        string
	Document  string
	Metadata  map[string]any
	Embedding []float64
}

type memoryCollection struct {
	store *MemoryVectorStore
}

// NewMemory returns an in-memory store.
func NewMemory() *MemoryVectorStore {
	s := &MemoryVectorStore{}
	s.collection = &memoryCollection{store: s}
	return s
}

// AddCase inserts or updates a case.
func (s *MemoryVectorStore) AddCase(caseID, text string, metadata map[string]any, embedding []float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.cases {
		if s.cases[i].ID == caseID {
			s.cases[i] = doc{ID: caseID, Document: text, Metadata: metadata, Embedding: embedding}
			return
		}
	}
	s.cases = append(s.cases, doc{ID: caseID, Document: text, Metadata: metadata, Embedding: embedding})
}

// Search returns the nResults nearest cases by cosine similarity (1 - cosine
// distance approximated via dot product on normalized vectors; for the demo
// fallback hash vectors this is a rough ordering only).
func (s *MemoryVectorStore) Search(q []float64, nResults int) []SearchResult {
	s.mu.Lock()
	docs := make([]doc, len(s.cases))
	copy(docs, s.cases)
	s.mu.Unlock()

	type hit struct {
		d   doc
		cos float64
	}
	var hits []hit
	for _, d := range docs {
		hits = append(hits, hit{d: d, cos: cosine(q, d.Embedding)})
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].cos > hits[j].cos })

	limit := nResults
	if limit > len(hits) {
		limit = len(hits)
	}
	out := make([]SearchResult, 0, limit)
	for i := 0; i < limit; i++ {
		out = append(out, SearchResult{
			ID:       hits[i].d.ID,
			Document: hits[i].d.Document,
			Metadata: hits[i].d.Metadata,
			Distance: 1 - hits[i].cos,
		})
	}
	return out
}

// Count returns the number of stored cases.
func (s *MemoryVectorStore) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.cases)
}

// Collection returns the collection accessor.
func (s *MemoryVectorStore) Collection() StoreCollection {
	return s.collection
}

// Close is a no-op for the in-memory backend.
func (s *MemoryVectorStore) Close() {}

// Get returns all docs (optionally filtered by include keys).
func (c *memoryCollection) Get(include []string) GetResult {
	c.store.mu.Lock()
	defer c.store.mu.Unlock()

	var wantDocs, wantMeta bool
	if len(include) == 0 {
		wantDocs, wantMeta = true, true
	} else {
		for _, k := range include {
			if k == "documents" {
				wantDocs = true
			}
			if k == "metadatas" {
				wantMeta = true
			}
		}
	}

	out := GetResult{IDs: make([]string, 0, len(c.store.cases))}
	if wantDocs {
		out.Documents = make([]string, 0, len(c.store.cases))
	}
	if wantMeta {
		out.Metadatas = make([]map[string]any, 0, len(c.store.cases))
	}
	for _, d := range c.store.cases {
		out.IDs = append(out.IDs, d.ID)
		if wantDocs {
			out.Documents = append(out.Documents, d.Document)
		}
		if wantMeta {
			out.Metadatas = append(out.Metadatas, d.Metadata)
		}
	}
	return out
}

// Delete removes the given ids.
func (c *memoryCollection) Delete(ids []string) {
	c.store.mu.Lock()
	defer c.store.mu.Unlock()
	drop := make(map[string]bool, len(ids))
	for _, id := range ids {
		drop[id] = true
	}
	out := c.store.cases[:0]
	for _, d := range c.store.cases {
		if !drop[d.ID] {
			out = append(out, d)
		}
	}
	c.store.cases = out
}

func cosine(a, b []float64) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := 0; i < len(a) && i < len(b); i++ {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
