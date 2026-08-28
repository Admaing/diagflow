package validator

import "testing"

func TestRootCauseTooShort(t *testing.T) {
	v := New()
	ok, fb := v.ValidateLayer("short", []string{"Fix"}, 3)
	if ok {
		t.Fatal("expected fail for short root cause")
	}
	if fb == "" {
		t.Fatal("expected feedback")
	}
}

func TestNoSuggestions(t *testing.T) {
	v := New()
	ok, _ := v.ValidateLayer("This is a valid root cause with enough detail", nil, 3)
	if ok {
		t.Fatal("expected fail for missing suggestions")
	}
}

func TestNoEvidence(t *testing.T) {
	v := New()
	ok, _ := v.ValidateLayer("This is a valid root cause", []string{"Fix"}, 0)
	if ok {
		t.Fatal("expected fail for zero evidence")
	}
}

func TestPassesWhenEvidenceKeywordPresent(t *testing.T) {
	v := New()
	ok, _ := v.ValidateLayer(
		"TaskManager OOM error in logs — heap exhausted at 2048m",
		[]string{"Increase heap"}, 2,
	)
	if !ok {
		t.Fatal("expected pass: root cause references OOM/log keywords")
	}
}

func TestFailsWhenNoEvidenceKeyword(t *testing.T) {
	v := New()
	ok, fb := v.ValidateLayer(
		"Something went wrong with the system.",
		[]string{"Fix everything"}, 3,
	)
	if ok {
		t.Fatal("expected fail: no evidence keyword in root cause")
	}
	if fb == "" {
		t.Fatal("expected feedback")
	}
}
