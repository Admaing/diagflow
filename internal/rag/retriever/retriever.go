// Package retriever mirrors diagflow/rag/retriever.py — hybrid semantic +
// BM25 search fused with Reciprocal Rank Fusion (RRF).
package retriever

import (
	"math"
	"sort"
	"strings"

	"github.com/Admaing/diagflow/internal/rag/embedder"
	"github.com/Admaing/diagflow/internal/rag/vectorstore"
)

// HybridRetriever combines semantic and BM25 search.
type HybridRetriever struct {
	store    vectorstore.VectorStore
	embedder *embedder.Embedder
	rrfK     float64

	bm25          *BM25
	bm25IDs       []string
	bm25Metadatas []map[string]any
	bm25Documents []string
}

// New builds a retriever.
func New(store vectorstore.VectorStore, emb *embedder.Embedder, rrfK int) *HybridRetriever {
	if rrfK <= 0 {
		rrfK = 60
	}
	return &HybridRetriever{store: store, embedder: emb, rrfK: float64(rrfK)}
}

// Search runs hybrid search with RRF fusion.
func (r *HybridRetriever) Search(query string, nResults int, semanticWeight, bm25Weight float64) []vectorstore.SearchResult {
	q := r.embedder.Embed(query)
	semantic := r.store.Search(q, nResults*2)
	bm25 := r.bm25Search(query, nResults*2)

	scores := make(map[string]float64)
	for rank, res := range semantic {
		scores[res.ID] += semanticWeight / (r.rrfK + float64(rank))
	}
	for rank, res := range bm25 {
		scores[res.ID] += bm25Weight / (r.rrfK + float64(rank))
	}

	// Sort ids by descending fusion score.
	type idScore struct {
		id    string
		score float64
	}
	var ranked []idScore
	for id, s := range scores {
		ranked = append(ranked, idScore{id: id, score: s})
	}
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].score > ranked[j].score })

	// Build id → result index for merging.
	byID := make(map[string]vectorstore.SearchResult)
	for _, res := range semantic {
		if _, ok := byID[res.ID]; !ok {
			byID[res.ID] = res
		}
	}
	for _, res := range bm25 {
		if _, ok := byID[res.ID]; !ok {
			byID[res.ID] = res
		}
	}

	limit := nResults
	if limit > len(ranked) {
		limit = len(ranked)
	}
	out := make([]vectorstore.SearchResult, 0, limit)
	for i := 0; i < limit; i++ {
		res := byID[ranked[i].id]
		f := ranked[i].score
		res.FusionScore = &f
		out = append(out, res)
	}
	return out
}

func (r *HybridRetriever) bm25Search(query string, nResults int) []vectorstore.SearchResult {
	if r.bm25 == nil {
		r.RebuildFromStore()
	}
	if r.bm25 == nil {
		return nil
	}
	tokens := tokenize(query)
	scores := r.bm25.Scores(tokens)

	type idxScore struct {
		idx   int
		score float64
	}
	var top []idxScore
	for i, s := range scores {
		if s > 0 {
			top = append(top, idxScore{idx: i, score: s})
		}
	}
	sort.SliceStable(top, func(i, j int) bool { return top[i].score > top[j].score })
	if nResults < len(top) {
		top = top[:nResults]
	}

	out := make([]vectorstore.SearchResult, 0, len(top))
	for _, t := range top {
		out = append(out, vectorstore.SearchResult{
			ID:       r.bm25IDs[t.idx],
			Document: r.bm25Documents[t.idx],
			Metadata: r.bm25Metadatas[t.idx],
			Distance: math.Log(1 + t.score), // BM25 score → monotonic "distance-ish" field
		})
	}
	return out
}

// RebuildFromStore rebuilds the BM25 index from the store's full corpus.
func (r *HybridRetriever) RebuildFromStore() {
	all := r.store.Collection().Get([]string{"documents", "metadatas"})
	if len(all.Documents) == 0 {
		r.bm25 = nil
		r.bm25IDs = nil
		r.bm25Metadatas = nil
		r.bm25Documents = nil
		return
	}
	r.buildIndex(all.Documents, all.IDs, all.Metadatas)
}

// BuildIndex pre-seeds the BM25 index from explicit slices.
func (r *HybridRetriever) BuildIndex(documents, ids []string, metadatas []map[string]any) {
	r.buildIndex(documents, ids, metadatas)
}

func (r *HybridRetriever) buildIndex(documents, ids []string, metadatas []map[string]any) {
	corpus := make([][]string, len(documents))
	for i, d := range documents {
		corpus[i] = tokenize(d)
	}
	r.bm25 = NewBM25(corpus)
	r.bm25IDs = ids
	r.bm25Metadatas = metadatas
	r.bm25Documents = documents
}

func tokenize(text string) []string {
	return strings.Fields(strings.ToLower(text))
}

// BM25 is a minimal Okapi BM25 implementation.
type BM25 struct {
	corpus    [][]string
	docFreq   map[string]int
	avgDocLen float64
	docCount  int
	k1        float64
	b         float64
}

// NewBM25 builds an index over tokenized corpus.
func NewBM25(corpus [][]string) *BM25 {
	b := &BM25{
		corpus:   corpus,
		docFreq:  make(map[string]int),
		docCount: len(corpus),
		k1:       1.5,
		b:        0.75,
	}
	total := 0
	for _, doc := range corpus {
		total += len(doc)
		seen := make(map[string]bool)
		for _, tok := range doc {
			if !seen[tok] {
				b.docFreq[tok]++
				seen[tok] = true
			}
		}
	}
	if b.docCount > 0 {
		b.avgDocLen = float64(total) / float64(b.docCount)
	}
	return b
}

// Scores returns the BM25 score of each document for the query tokens.
func (b *BM25) Scores(query []string) []float64 {
	out := make([]float64, b.docCount)
	if b.docCount == 0 {
		return out
	}
	idf := make(map[string]float64)
	for _, q := range query {
		df := b.docFreq[q]
		idf[q] = math.Log(1 + (float64(b.docCount)-float64(df)+0.5)/(float64(df)+0.5))
	}

	for i, doc := range b.corpus {
		tf := make(map[string]int)
		for _, tok := range doc {
			tf[tok]++
		}
		docLen := float64(len(doc))
		var score float64
		for _, q := range query {
			f := float64(tf[q])
			if f == 0 {
				continue
			}
			denom := f + b.k1*(1-b.b+b.b*(docLen/b.avgDocLen))
			score += idf[q] * (f * (b.k1 + 1)) / denom
		}
		out[i] = score
	}
	return out
}
