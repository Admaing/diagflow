package diagagent

import (
	"testing"

	"github.com/Admaing/diagflow/internal/config"
)

// The LLM model must come from config, so a model change is a one-line config
// change rather than a code edit (the two used to drift apart).
func TestDefaultModelComesFromConfig(t *testing.T) {
	config.Reset()
	t.Cleanup(config.Reset)

	if got := New().model; got != config.Get().LLM.Model {
		t.Fatalf("agent model %q does not match config %q", got, config.Get().LLM.Model)
	}
}

func TestDefaultModelOverrideFromEnv(t *testing.T) {
	config.Reset()
	t.Setenv("DIAGFLOW_LLM__MODEL", "some-other-model")
	t.Cleanup(config.Reset)

	if got := New().model; got != "some-other-model" {
		t.Fatalf("agent model = %q, want the env override", got)
	}
}

func TestWithModelStillWins(t *testing.T) {
	config.Reset()
	t.Cleanup(config.Reset)

	if got := New(WithModel("explicit")).model; got != "explicit" {
		t.Fatalf("explicit option ignored: %q", got)
	}
}
