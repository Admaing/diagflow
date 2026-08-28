package infra

import (
	"testing"
)

func TestSignByteStable(t *testing.T) {
	params := map[string]string{
		"Action":   "GetLogs",
		"Date":     "1720000000000",
		"Path":     "/data/flink/log/taskmanager.log",
		"MaxLines": "100",
	}
	key := "umr-agent-secret"
	want := Sign(params, key)
	// Determinism across calls.
	if Sign(params, key) != want {
		t.Fatal("sign must be deterministic")
	}
}

func TestSignOrderIndependent(t *testing.T) {
	a := map[string]string{"Action": "GetBaseInfo", "Date": "1"}
	b := map[string]string{"Date": "1", "Action": "GetBaseInfo"}
	if Sign(a, "k") != Sign(b, "k") {
		t.Fatal("sign must be order-independent on sorted keys")
	}
}

func TestAgentKeyOfCompat(t *testing.T) {
	// agent_key field preferred.
	n1 := map[string]any{"agent_key": "ak", "umr_agent_key": "umk"}
	if got := agentKeyOf(n1); got != "ak" {
		t.Fatalf("expected 'ak', got '%s'", got)
	}
	// Only umr_agent_key present.
	n2 := map[string]any{"umr_agent_key": "umk"}
	if got := agentKeyOf(n2); got != "umk" {
		t.Fatalf("expected 'umk', got '%s'", got)
	}
}
