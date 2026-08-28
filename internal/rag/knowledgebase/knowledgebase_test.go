package knowledgebase

import (
	"testing"

	"github.com/Admaing/diagflow/internal/rag/vectorstore"
)

func TestMakeFingerprintDeterministic(t *testing.T) {
	a := MakeFingerprint("flink", "OOM", "1.17")
	b := MakeFingerprint("flink", "OOM", "1.17")
	if a != b {
		t.Fatalf("fingerprint not deterministic: %s != %s", a, b)
	}
	if len(a) != 16 {
		t.Fatalf("fingerprint len %d", len(a))
	}
}

func TestMakeFingerprintDifferentInputs(t *testing.T) {
	a := MakeFingerprint("flink", "OOM", "1.17")
	b := MakeFingerprint("flink", "CheckpointExpired", "1.17")
	if a == b {
		t.Fatal("fingerprints should differ")
	}
}

func TestFingerprintMatchHit(t *testing.T) {
	kb := New(vectorstore.NewMemory())
	kb.AddCase("flink", "OOM", "1.17", "Root cause", []string{"Fix 1", "Fix 2"})
	hit := kb.FingerprintMatch("flink", "OOM", "1.17")
	if hit == nil {
		t.Fatal("expected hit")
	}
	if hit["root_cause"] != "Root cause" {
		t.Fatalf("root_cause %v", hit["root_cause"])
	}
}

func TestFingerprintMatchMiss(t *testing.T) {
	kb := New(vectorstore.NewMemory())
	if hit := kb.FingerprintMatch("flink", "unknown", "1.17"); hit != nil {
		t.Fatal("expected miss")
	}
}

func TestMarkIncorrectPreventsMatch(t *testing.T) {
	kb := New(vectorstore.NewMemory())
	fp := kb.AddCase("flink", "OOM", "1.17", "wrong", []string{"Fix"})
	kb.MarkIncorrect(fp)
	if hit := kb.FingerprintMatch("flink", "OOM", "1.17"); hit != nil {
		t.Fatal("disputed case should not match")
	}
}
