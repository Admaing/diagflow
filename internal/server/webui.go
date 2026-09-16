package server

import (
	_ "embed"
	"net/http"
)

// webuiHTML is the single-page diagnosis console. It is embedded in the
// binary so deployment stays one container with no asset pipeline.
//
//go:embed webui.html
var webuiHTML []byte

// handleWebUI serves the embedded console. The page holds no secrets: it reads
// the bearer token from the operator's browser and sends it on each API call,
// so it works whether or not DIAGFLOW_AUTH_TOKEN is configured.
func (s *Server) handleWebUI(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(webuiHTML)
}
