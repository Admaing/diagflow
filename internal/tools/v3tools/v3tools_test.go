package v3tools

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/hex"
	"testing"
)

func TestValidateCommandAllowed(t *testing.T) {
	ok, reason := ValidateCommand("grep ERROR /var/log/app.log")
	if !ok {
		t.Fatalf("expected allowed, got %s", reason)
	}
}

func TestValidateCommandForbiddenSubstring(t *testing.T) {
	ok, _ := ValidateCommand("cat app.log; rm -rf /tmp")
	if ok {
		t.Fatal("expected forbidden due to rm")
	}
}

func TestValidateCommandForbiddenRedirection(t *testing.T) {
	ok, _ := ValidateCommand("echo hi > /etc/passwd")
	if ok {
		t.Fatal("expected forbidden due to >")
	}
}

func TestValidateCommandForbiddenSubstitution(t *testing.T) {
	ok, _ := ValidateCommand("echo $(whoami)")
	if ok {
		t.Fatal("expected forbidden due to $()")
	}
}

func TestValidateCommandPipeAllowed(t *testing.T) {
	ok, _ := ValidateCommand("grep ERROR app.log | tail -50")
	if !ok {
		t.Fatal("expected allowed pipe")
	}
}

func TestSignMatchesNodeJS(t *testing.T) {
	params := map[string]string{"Action": "GetLogs", "Date": "1720000000000", "Path": "/a.log"}
	key := "secret-agent-key"

	expected := signRef(params, key)
	got := SignParams(params, key)
	if got != expected {
		t.Fatalf("signature mismatch: %s != %s", got, expected)
	}
}

func signRef(params map[string]string, key string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	// sort
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	var b string
	for _, k := range keys {
		b += params[k]
	}
	mac := hmac.New(sha1.New, []byte(key))
	mac.Write([]byte(b))
	return hex.EncodeToString(mac.Sum(nil))
}

func TestSignStable(t *testing.T) {
	params := map[string]string{"b": "2", "a": "1"}
	if SignParams(params, "k") != SignParams(params, "k") {
		t.Fatal("signature must be deterministic")
	}
}
