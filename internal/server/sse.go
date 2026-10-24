package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Admaing/diagflow/internal/diagagent"
)

// SSE event names. Narration rides on a single "step" event whose payload
// carries a kind, so the browser can style each step without the server having
// to invent an event name per tool.
const (
	sseStep   = "step"
	sseReport = "report"
	sseFailed = "failed"
	sseDone   = "done"
)

// heartbeatInterval keeps proxies and browsers from dropping an idle stream.
// A diagnosis can sit in a thinking-mode LLM call for 20s with nothing to say.
const heartbeatInterval = 15 * time.Second

// streamBuffer is generous because events are a few hundred bytes and a run
// emits a few dozen; it is only here so a stalled reader cannot block the
// diagnosis goroutine.
const streamBuffer = 512

// stepEvent is one narration line from the agent.
type stepEvent struct {
	Kind      string `json:"kind"`
	Text      string `json:"text"`
	Tool      string `json:"tool,omitempty"`
	ElapsedMS int64  `json:"elapsed_ms"`
}

type diagResult struct {
	report   *diagagent.Report
	markdown string
	err      error
}

// handleDiagnoseStream runs a diagnosis and narrates it as it happens.
//
// The request is validated before the stream opens, so a malformed call still
// gets a plain JSON error with a usable status code; only once the run is
// certain to start does the response switch to text/event-stream.
func (s *Server) handleDiagnoseStream(w http.ResponseWriter, r *http.Request) {
	var req DiagnoseRequest
	r.Body = http.MaxBytesReader(w, r.Body, int64(s.cfg.MaxBodyBytes))
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errJSON("invalid_json", truncateErr(err)))
		return
	}
	req.Component = strings.TrimSpace(req.Component)
	req.Problem = strings.TrimSpace(req.Problem)
	if req.Component == "" || req.Problem == "" {
		writeJSON(w, http.StatusBadRequest, errJSON("missing_fields",
			"component and problem are required"))
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError,
			errJSON("stream_unsupported", "response writer does not support streaming"))
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// Buffering proxies defeat the whole point of streaming this.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// The client's context is the parent, so a browser that navigates away
	// cancels the LLM work instead of leaving it running to completion.
	ctx, cancel := context.WithTimeout(r.Context(),
		time.Duration(s.cfg.RequestTimeoutS)*time.Second)
	defer cancel()

	start := time.Now()
	events := make(chan stepEvent, streamBuffer)
	result := make(chan diagResult, 1)
	go func() {
		report, markdown, err := s.diagnose(ctx, &req, func(msg string) {
			select {
			case events <- classifyEvent(msg, time.Since(start)):
			default:
				// A full buffer means nobody is reading. Losing narration is
				// preferable to stalling the diagnosis behind it.
			}
		})
		result <- diagResult{report: report, markdown: markdown, err: err}
	}()

	heartbeat := time.NewTicker(heartbeatInterval)
	defer heartbeat.Stop()

	for {
		select {
		case ev := <-events:
			writeSSE(w, flusher, sseStep, ev)
		case res := <-result:
			// The agent may have queued events while the result was in flight.
			for drained := false; !drained; {
				select {
				case ev := <-events:
					writeSSE(w, flusher, sseStep, ev)
				default:
					drained = true
				}
			}
			s.finishStream(ctx, w, flusher, &req, res, start)
			return
		case <-heartbeat.C:
			// A comment line is valid SSE and keeps the connection warm.
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case <-ctx.Done():
			return
		}
	}
}

// finishStream reports the outcome and records the run. It mirrors the tail of
// handleDiagnose so both transports keep identical metrics and persistence.
func (s *Server) finishStream(ctx context.Context, w http.ResponseWriter, flusher http.Flusher,
	req *DiagnoseRequest, res diagResult, start time.Time) {
	durationMS := float64(time.Since(start).Milliseconds())
	diagTotal.Inc(req.Component, diagnosisStatus(res.err, res.report))

	if res.err != nil {
		msg := truncateErr(res.err)
		s.log.Error("diagnosis failed", "component", req.Component, "problem", req.Problem, "error", res.err)
		writeSSE(w, flusher, sseFailed, map[string]any{"message": msg, "code": diagnosisErrorCode(res.err)})
		writeSSE(w, flusher, sseDone, map[string]any{"duration_ms": durationMS})
		return
	}
	diagDuration.Observe(durationMS)
	s.recordDiagnosis(req, res.report)
	writeSSE(w, flusher, sseReport, map[string]any{
		"report":   res.report,
		"markdown": res.markdown,
	})
	writeSSE(w, flusher, sseDone, map[string]any{"duration_ms": durationMS})
}

// writeSSE emits one event. The payload is JSON on a single line, so a newline
// inside a log message can never break the framing.
func writeSSE(w http.ResponseWriter, flusher http.Flusher, event string, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		body = []byte(`{"kind":"log","text":"event encoding failed"}`)
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, body)
	flusher.Flush()
}

// classifyEvent turns one agent narration line into a structured step. The
// agent's messages are free-form ("[react] ssh_exec({...})"), so the tag and a
// few markers are the only reliable signal available.
func classifyEvent(msg string, elapsed time.Duration) stepEvent {
	ev := stepEvent{Kind: "log", Text: clipStep(msg), ElapsedMS: elapsed.Milliseconds()}
	tag, body := splitTag(msg)
	switch {
	case tag == "react" && strings.Contains(body, " error: "):
		ev.Kind, ev.Tool = "tool_error", toolName(body)
	case tag == "react" && strings.Contains(body, " result: "):
		ev.Kind, ev.Tool = "tool_result", toolName(body)
	case tag == "react":
		ev.Kind, ev.Tool = "tool_call", toolName(body)
	case tag == "strategy":
		ev.Kind = "strategy"
	case tag == "agent" || tag == "intent":
		ev.Kind = "warn"
	case strings.HasPrefix(tag, "diag-") && (strings.HasPrefix(body, "Phase") || strings.HasPrefix(body, "done in")):
		ev.Kind = "phase"
	case strings.HasPrefix(tag, "diag-"):
		ev.Kind = "notice"
	}
	ev.Text = clipStep(body)
	if ev.Tool == "" {
		ev.Tool = tag
	}
	return ev
}

// splitTag peels the "[react] " style prefix off a narration line. Messages
// without one keep the whole string as the body.
func splitTag(msg string) (tag, body string) {
	if !strings.HasPrefix(msg, "[") {
		return "", msg
	}
	end := strings.Index(msg, "] ")
	if end < 0 {
		return "", msg
	}
	return msg[1:end], msg[end+2:]
}

// toolName reads the tool name off "ssh_exec({...})" or "ssh_exec result: ...".
func toolName(body string) string {
	cut := len(body)
	for _, stop := range []string{"(", " ", ":"} {
		if i := strings.Index(body, stop); i >= 0 && i < cut {
			cut = i
		}
	}
	return body[:cut]
}

// clipStep bounds one narration line. Tool results are truncated by the agent
// already, but a stray config dump would otherwise blow up the event payload.
func clipStep(s string) string {
	const max = 600
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
