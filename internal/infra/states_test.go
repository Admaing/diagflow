package infra

import (
	"strings"
	"testing"
)

func TestStateNames(t *testing.T) {
	if got := ClusterStateName(2); got != "CS_RUNNING" {
		t.Errorf("ClusterStateName(2) = %q", got)
	}
	if got := NodeStateName(1); got != "NS_RUNNING" {
		t.Errorf("NodeStateName(1) = %q", got)
	}
	if got := AppStateName(1); got != "AS_RUNNING" {
		t.Errorf("AppStateName(1) = %q", got)
	}
	if got := ClusterStateName(999); !strings.Contains(got, "UNKNOWN") {
		t.Errorf("unknown state = %q", got)
	}
}

func TestClusterReadyAndHint(t *testing.T) {
	if !ClusterReady(2) || !ClusterReady(7) {
		t.Fatal("CS_RUNNING and CS_SERVICE_EXCEPTION should be ready")
	}
	for _, st := range []int{1, 3, 8, 13, 4, 6, 12, 5} {
		if ClusterReady(st) {
			t.Errorf("state %d should not be ready", st)
		}
	}
	if hint := ClusterStateHint(2); !strings.Contains(hint, "available") {
		t.Errorf("running hint = %q", hint)
	}
	if hint := ClusterStateHint(13); !strings.Contains(hint, "created") {
		t.Errorf("creating hint = %q", hint)
	}
	if hint := ClusterStateHint(6); !strings.Contains(hint, "unreachable") {
		t.Errorf("deleting hint = %q", hint)
	}
}
