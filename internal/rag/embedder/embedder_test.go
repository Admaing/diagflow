package embedder

import "testing"

// TestDegradedModeGate pins the degradation contract: even with an API key,
// embeddings are NOT semantically meaningful until a real provider is wired,
// so HasRealEmbeddings must stay false (this gates DiagAgent Phase 1).
func TestDegradedModeGate(t *testing.T) {
	e := New("text-embedding-3-small", "sk-test", 1536)
	if e.HasRealEmbeddings() {
		t.Fatal("degraded mode: HasRealEmbeddings must be false while openaiEmbed is a stub")
	}
	v1 := e.Embed("flink OutOfMemoryError")
	v2 := e.Embed("flink OutOfMemoryError")
	if len(v1) != 1536 {
		t.Fatalf("dim = %d, want 1536", len(v1))
	}
	for i := range v1 {
		if v1[i] != v2[i] {
			t.Fatal("fallback embedding must be deterministic")
		}
		break
	}
}
