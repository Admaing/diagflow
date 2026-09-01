// Package embedder mirrors diagflow/rag/embedder.py — text → embedding vectors.
package embedder

import (
	"crypto/md5"
	"math/rand"
)

// Embedder produces embeddings, falling back to a deterministic hash vector
// when no API key is present.
type Embedder struct {
	Model  string
	APIKey string
	dim    int
}

// New builds an Embedder. embeddingDim must match the vector store dimension.
func New(model, apiKey string, embeddingDim int) *Embedder {
	return &Embedder{Model: model, APIKey: apiKey, dim: embeddingDim}
}

// Embed produces a vector. With an API key it calls OpenAI; otherwise it uses
// the deterministic fallback.
func (e *Embedder) Embed(text string) []float64 {
	if e.APIKey != "" {
		return e.openaiEmbed(text)
	}
	return e.fallbackEmbed(text)
}

// EmbedBatch embeds multiple texts.
func (e *Embedder) EmbedBatch(texts []string) [][]float64 {
	out := make([][]float64, len(texts))
	for i, t := range texts {
		out[i] = e.Embed(t)
	}
	return out
}

// HasRealEmbeddings reports whether we can produce semantically meaningful
// vectors. DEGRADED MODE: the OpenAI embedding call is not yet wired (see
// openaiEmbed), so this is always false — the KB semantic fast-path in
// DiagAgent Phase 1 stays disabled and only the deterministic fallback is
// used. Flip this to true ONLY after wiring a real embedding provider.
func (e *Embedder) HasRealEmbeddings() bool {
	return false
}

// openaiEmbed is a stub returning the deterministic fallback. TODO: wire a
// real embedding provider (POST /v1/embeddings) before enabling
// HasRealEmbeddings.
func (e *Embedder) openaiEmbed(text string) []float64 {
	return e.fallbackEmbed(text)
}

// fallbackEmbed is a deterministic hash-based vector matching the store dim.
// Not semantically meaningful — the caller must not write these into a real
// vector store when no embedding key is present.
func (e *Embedder) fallbackEmbed(text string) []float64 {
	sum := md5.Sum([]byte(text))
	seedBytes := sum[:]
	r := rand.New(rand.NewSource(int64FromBytes(seedBytes)))
	out := make([]float64, e.dim)
	for i := range out {
		out[i] = r.NormFloat64()
	}
	return out
}

func int64FromBytes(b []byte) int64 {
	var v int64
	for i, c := range b[:4] {
		v |= int64(c) << (8 * i)
	}
	return v
}
