// Package knowledgebase mirrors diagflow/rag/knowledge_base.py — closed-loop
// historical case reuse with MD5 fingerprint fast-path and hybrid search.
package knowledgebase

import (
	"crypto/md5"
	"encoding/hex"
	"strings"
	"sync"

	"github.com/Admaing/diagflow/internal/config"
	"github.com/Admaing/diagflow/internal/rag/embedder"
	"github.com/Admaing/diagflow/internal/rag/retriever"
	"github.com/Admaing/diagflow/internal/rag/vectorstore"
)

// KnownCase is a cached fingerprint entry.
type KnownCase struct {
	CaseID       string
	Component    string
	ErrorPattern string
	Version      string
	RootCause    string
	Suggestions  []string
	Hits         int
	Disputed     bool
}

// KnowledgeBase manages fingerprints + hybrid retrieval.
type KnowledgeBase struct {
	mu           sync.Mutex
	Store        vectorstore.VectorStore
	Embedder     *embedder.Embedder
	Retriever    *retriever.HybridRetriever
	fingerprints map[string]*KnownCase
}

// New builds a KnowledgeBase.
func New(store vectorstore.VectorStore) *KnowledgeBase {
	cfg := config.Get()
	emb := embedder.New("text-embedding-3-small", cfg.OpenAIAPIKey, cfg.VectorStore.EmbeddingDim)
	kb := &KnowledgeBase{
		Store:        store,
		Embedder:     emb,
		fingerprints: make(map[string]*KnownCase),
	}
	kb.Retriever = retriever.New(store, emb, 60)
	kb.loadFingerprints()
	return kb
}

// MakeFingerprint computes MD5(component:error:version)[:16].
func MakeFingerprint(component, errorPattern, version string) string {
	raw := component + ":" + errorPattern + ":" + version
	sum := md5.Sum([]byte(raw))
	return hex.EncodeToString(sum[:])[:16]
}

// HasRealEmbeddings reports whether the KB can produce semantically meaningful
// vectors (i.e. an embedding API key is present).
func (kb *KnowledgeBase) HasRealEmbeddings() bool {
	return kb.Embedder.HasRealEmbeddings()
}

func (kb *KnowledgeBase) loadFingerprints() {
	all := kb.Store.Collection().Get([]string{"metadatas"})
	if all.Metadatas == nil {
		return
	}
	for i, meta := range all.Metadatas {
		fp, _ := meta["fingerprint"].(string)
		if fp == "" {
			continue
		}
		if _, ok := kb.fingerprints[fp]; ok {
			continue
		}
		caseID := ""
		if i < len(all.IDs) {
			caseID = all.IDs[i]
		}
		kb.fingerprints[fp] = &KnownCase{
			CaseID:       caseID,
			Component:    strOr(meta["component"], "unknown"),
			ErrorPattern: strOr(meta["error_pattern"], "unknown"),
			Version:      strOr(meta["version"], ""),
			RootCause:    strOr(meta["root_cause"], ""),
			Suggestions:  toSuggestions(meta["suggestions"]),
		}
	}
}

// FingerprintMatch returns a non-disputed hit, or nil.
func (kb *KnowledgeBase) FingerprintMatch(component, errorPattern, version string) map[string]any {
	kb.mu.Lock()
	defer kb.mu.Unlock()
	fp := MakeFingerprint(component, errorPattern, version)
	if m, ok := kb.fingerprints[fp]; ok && !m.Disputed {
		m.Hits++
		return map[string]any{
			"root_cause":    m.RootCause,
			"suggestions":   m.Suggestions,
			"component":     m.Component,
			"error_pattern": m.ErrorPattern,
			"version":       m.Version,
		}
	}
	return nil
}

// AddCase indexes a case (called after a successful diagnosis).
func (kb *KnowledgeBase) AddCase(component, errorPattern, version, rootCause string, suggestions []string) string {
	fp := MakeFingerprint(component, errorPattern, version)
	caseID := component + "_" + errorPattern + "_" + fp[:8]

	kb.mu.Lock()
	if existing, ok := kb.fingerprints[fp]; ok {
		existing.Hits++
		existing.Suggestions = mergeStr(existing.Suggestions, suggestions)
		if rootCause != "" && existing.RootCause == "" {
			existing.RootCause = rootCause
		}
		kb.mu.Unlock()
		return fp
	}
	kb.mu.Unlock()

	var b strings.Builder
	b.WriteString("Component: " + component + " v" + version + "\n")
	b.WriteString("Error pattern: " + errorPattern + "\n")
	b.WriteString("Root cause: " + rootCause + "\n")
	b.WriteString("Suggestions:\n")
	for _, s := range suggestions {
		b.WriteString("- " + s + "\n")
	}
	caseText := b.String()

	if kb.Embedder.HasRealEmbeddings() {
		embedding := kb.Embedder.Embed(caseText)
		kb.Store.AddCase(caseID, caseText, map[string]any{
			"component":     component,
			"error_pattern": errorPattern,
			"version":       version,
			"fingerprint":   fp,
			"root_cause":    rootCause,
			"suggestions":   strings.Join(suggestions, ","),
		}, embedding)
	}

	kb.mu.Lock()
	kb.fingerprints[fp] = &KnownCase{
		CaseID:       caseID,
		Component:    component,
		ErrorPattern: errorPattern,
		Version:      version,
		RootCause:    rootCause,
		Suggestions:  suggestions,
		Hits:         1,
	}
	kb.mu.Unlock()

	kb.RebuildBM25()
	return fp
}

// Search runs hybrid search across all indexed cases.
func (kb *KnowledgeBase) Search(query string, n int) []vectorstore.SearchResult {
	return kb.Retriever.Search(query, n, 0.6, 0.4)
}

// RebuildBM25 rebuilds the retriever index from the store.
func (kb *KnowledgeBase) RebuildBM25() {
	kb.Retriever.RebuildFromStore()
}

// MarkIncorrect disputes a fingerprint, preventing future fast-path hits.
func (kb *KnowledgeBase) MarkIncorrect(fp string) {
	kb.mu.Lock()
	defer kb.mu.Unlock()
	if m, ok := kb.fingerprints[fp]; ok {
		m.Disputed = true
	}
}

// AddInvestigationIntent stores an investigation intent (排查思路) linked to its
// root cause, so a future similar intent can semantically match and short-circuit.
// Only called after the user confirms a diagnosis was correct.
func (kb *KnowledgeBase) AddInvestigationIntent(intentDesc, rootCause, component, version string) {
	if !kb.Embedder.HasRealEmbeddings() {
		return // semantic retrieval requires a real embedding backend
	}
	caseID := "intent_" + MakeFingerprint(component, intentDesc, version)[:12]
	text := "排查思路: " + intentDesc + "\n根因: " + rootCause + "\n组件: " + component
	kb.Store.AddCase(caseID, text, map[string]any{
		"component":  component,
		"root_cause": rootCause,
		"version":    version,
	}, kb.Embedder.Embed(text))
	kb.RebuildBM25()
}

// Stats returns a summary.
func (kb *KnowledgeBase) Stats() map[string]any {
	kb.mu.Lock()
	n := len(kb.fingerprints)
	kb.mu.Unlock()
	return map[string]any{
		"fingerprint_cases": n,
		"chroma_docs":       kb.Store.Count(),
	}
}

func strOr(v any, def string) string {
	if s, ok := v.(string); ok {
		return s
	}
	return def
}

func toSuggestions(v any) []string {
	switch t := v.(type) {
	case string:
		if t == "" {
			return nil
		}
		return strings.Split(t, ",")
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return t
	default:
		return nil
	}
}

func mergeStr(a, b []string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, list := range [][]string{a, b} {
		for _, s := range list {
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	return out
}
