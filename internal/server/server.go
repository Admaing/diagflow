// Package server provides the HTTP API entrypoint for DiagFlow.
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Admaing/diagflow/internal/cluster"
	"github.com/Admaing/diagflow/internal/config"
	"github.com/Admaing/diagflow/internal/diagagent"
	"github.com/Admaing/diagflow/internal/infra"
	"github.com/Admaing/diagflow/internal/mcpclient"
	"github.com/Admaing/diagflow/internal/metrics"
	"github.com/Admaing/diagflow/internal/observability/report"
	"github.com/Admaing/diagflow/internal/observability/store"
	"github.com/Admaing/diagflow/internal/rag/knowledgebase"
	"github.com/Admaing/diagflow/internal/rag/vectorstore"
	"github.com/Admaing/diagflow/internal/simulated"
	"github.com/Admaing/diagflow/internal/tools/v3tools"
)

// Server is the HTTP API server.
type Server struct {
	cfg       config.ServerConfig
	authToken string
	mode      string
	region    string
	zone      string
	dbStore   *store.Store
	sessions  *sessionCache
	log       *slog.Logger
	requestID atomic.Int64

	accessOnce sync.Once
	access     *infra.ClusterAccess
	accessErr  error

	kbOnce sync.Once
	kb     *knowledgebase.KnowledgeBase

	// diagnose runs one diagnosis. It is a field rather than a direct call so
	// the streaming transport can be exercised without a live model; every
	// production path uses runDiagnosis.
	diagnose diagnoseFunc
}

// diagnoseFunc matches runDiagnosis' signature.
type diagnoseFunc func(ctx context.Context, req *DiagnoseRequest, onEvent func(string)) (*diagagent.Report, string, error)

// New builds a Server. dbStore may be nil (persistence disabled).
func New(log *slog.Logger, dbStore *store.Store) *Server {
	cfg := config.Get()
	s := &Server{
		cfg:       cfg.Server,
		authToken: cfg.AuthToken,
		mode:      cfg.Mode,
		region:    cfg.Region,
		zone:      cfg.Zone,
		dbStore:   dbStore,
		sessions:  newSessionCache(2*time.Hour, 50),
		log:       log,
	}
	s.diagnose = s.runDiagnosis
	return s
}

// DiagnoseRequest is the POST /api/v1/diagnose payload.
type DiagnoseRequest struct {
	Component   string         `json:"component"`
	Problem     string         `json:"problem"`
	Detail      string         `json:"detail,omitempty"`
	JobID       string         `json:"job_id,omitempty"`
	Scenario    string         `json:"scenario,omitempty"`
	ClusterID   string         `json:"cluster_id,omitempty"`
	SessionID   string         `json:"session_id,omitempty"`
	Investigate bool           `json:"investigate,omitempty"`
	Context     map[string]any `json:"context,omitempty"`
}

// Handler returns the routed handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(metrics.Render()))
	})
	mux.Handle("POST /api/v1/diagnose", s.authMiddleware(http.HandlerFunc(s.handleDiagnose)))
	mux.Handle("POST /api/v1/investigate", s.authMiddleware(http.HandlerFunc(s.handleInvestigate)))
	// Same run, narrated as it happens. The plain endpoint stays for callers
	// that only want the final report (scripts, the MCP-facing path).
	mux.Handle("POST /api/v1/diagnose/stream", s.authMiddleware(http.HandlerFunc(s.handleDiagnoseStream)))
	// Embedded single-page console. A static asset, so it sits outside the API
	// token — the page carries no data, and each call it makes still needs one.
	mux.HandleFunc("GET /", s.handleWebUI)
	return s.recoverMiddleware(s.requestIDMiddleware(mux))
}

// authMiddleware enforces the static bearer token when DIAGFLOW_AUTH_TOKEN is
// configured (the user identity itself is owned by the frontend/gateway).
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	if s.authToken == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || got != s.authToken {
			writeJSON(w, http.StatusUnauthorized, errJSON("unauthorized", "missing or invalid bearer token"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ListenAndServe runs the server until ctx is cancelled, then drains.
func (s *Server) ListenAndServe(ctx context.Context) error {
	httpSrv := &http.Server{
		Addr:              fmt.Sprintf(":%d", s.cfg.Port),
		Handler:           s.Handler(),
		ReadHeaderTimeout: time.Duration(s.cfg.ReadHeaderTimeoutS) * time.Second,
		ReadTimeout:       time.Duration(s.cfg.ReadTimeoutS) * time.Second,
		WriteTimeout:      time.Duration(s.cfg.WriteTimeoutS) * time.Second,
		IdleTimeout:       time.Duration(s.cfg.IdleTimeoutS) * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.ListenAndServe() }()

	s.log.Info("server listening", "port", s.cfg.Port)
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}
	drainCtx, cancel := context.WithTimeout(context.Background(),
		time.Duration(s.cfg.DrainTimeoutS)*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(drainCtx); err != nil {
		return errors.Join(ctx.Err(), err)
	}
	return ctx.Err()
}

// ---- handlers ----

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// readyz reports whether the process can serve diagnoses. Kubernetes only
// understands 200/503, so the body carries a dependency matrix: "degraded"
// still serves traffic but names the optional dependency that is unavailable.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	cfg := config.Get()
	checks := map[string]any{}
	criticalFailures := []string{}
	warnings := []string{}
	degraded := false

	add := func(name, status, detail string, critical bool) {
		checks[name] = map[string]any{"status": status, "detail": detail}
		switch status {
		case "unavailable":
			if critical {
				criticalFailures = append(criticalFailures, name+": "+detail)
			} else {
				degraded = true
				warnings = append(warnings, name+": "+detail)
			}
		case "degraded":
			degraded = true
			warnings = append(warnings, name+": "+detail)
		}
	}

	if strings.TrimSpace(cfg.LLM.APIKey) != "" {
		add("llm", "ok", "API key configured", true)
	} else if cfg.IsProduction() {
		add("llm", "unavailable", "DEEPSEEK_API_KEY/LLM_API_KEY is not configured", true)
	} else {
		add("llm", "degraded", "no LLM API key; diagnoses cannot complete", false)
	}

	if !cfg.IsProduction() || len(cfg.Infra.ZKHosts) > 0 {
		add("cluster_discovery", "ok", "ZooKeeper discovery configured", cfg.IsProduction())
	} else {
		add("cluster_discovery", "unavailable", "DIAGFLOW_ZK_HOSTS is required in production", true)
	}

	if cfg.MCP.BaseURL == "" {
		add("mcp", "disabled", "direct infrastructure mode", false)
	} else {
		probeCtx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		err := s.mcpClient().Health(probeCtx)
		cancel()
		if err != nil {
			add("mcp", "unavailable", err.Error(), true)
		} else {
			add("mcp", "ok", "health endpoint reachable", true)
		}
	}

	if s.dbStore.Enabled() {
		add("persistence", "ok", "MySQL connected", false)
	} else {
		add("persistence", "degraded", "history persistence disabled or MySQL unreachable", false)
	}

	if strings.TrimSpace(cfg.AuthToken) != "" {
		add("api_auth", "ok", "bearer authentication enabled", false)
	} else if cfg.IsProduction() {
		add("api_auth", "degraded", "DIAGFLOW_AUTH_TOKEN is not configured", false)
	} else {
		add("api_auth", "disabled", "demo mode without bearer authentication", false)
	}

	status := "ready"
	code := http.StatusOK
	if len(criticalFailures) > 0 {
		status = "not ready"
		code = http.StatusServiceUnavailable
	} else if degraded {
		status = "degraded"
	}
	info := map[string]any{
		"status": status,
		"mode":   modeLabel(cfg),
		"region": cfg.Region,
		"zone":   cfg.Zone,
		"checks": checks,
	}
	if len(criticalFailures) > 0 {
		info["reason"] = strings.Join(criticalFailures, "; ")
	}
	if len(warnings) > 0 {
		info["warnings"] = warnings
	}
	if cfg.IsProduction() {
		info["zk_hosts"] = cfg.Infra.ZKHosts
		info["zk_zone"] = cfg.ZKZone()
	}
	writeJSON(w, code, info)
}

func modeLabel(cfg *config.Config) string {
	if cfg.IsProduction() {
		return "production"
	}
	return "demo"
}

func (s *Server) handleDiagnose(w http.ResponseWriter, r *http.Request) {
	s.handleDiagnosis(w, r, false)
}

func (s *Server) handleInvestigate(w http.ResponseWriter, r *http.Request) {
	s.handleDiagnosis(w, r, true)
}

func (s *Server) handleDiagnosis(w http.ResponseWriter, r *http.Request, forceInvestigate bool) {
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
	if forceInvestigate {
		req.Investigate = true
	}

	ctx, cancel := context.WithTimeout(r.Context(),
		time.Duration(s.cfg.RequestTimeoutS)*time.Second)
	defer cancel()

	start := time.Now()
	rpt, markdown, err := s.diagnose(ctx, &req, nil)
	durationMS := float64(time.Since(start).Milliseconds())
	diagTotal.Inc(req.Component, diagnosisStatus(err, rpt))
	if err != nil {
		s.log.Error("diagnosis failed", "component", req.Component, "problem", req.Problem, "error", err)
		writeDiagnosisError(w, err)
		return
	}
	diagDuration.Observe(durationMS)

	s.recordDiagnosis(&req, rpt)

	writeJSON(w, http.StatusOK, map[string]any{
		"report":   rpt,
		"markdown": markdown,
	})
}

func writeDiagnosisError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		writeJSON(w, http.StatusGatewayTimeout, errJSON("diagnosis_timeout", "诊断超时，未生成结论"))
	case errors.Is(err, diagagent.ErrLLMUnavailable):
		writeJSON(w, http.StatusBadGateway, errJSON("llm_unavailable",
			"模型调用失败，未生成诊断结论: "+truncateErr(err)))
	case errors.Is(err, diagagent.ErrNoConclusion):
		writeJSON(w, http.StatusUnprocessableEntity, errJSON("no_conclusion",
			"证据不足或模型输出无效，未生成可验证结论: "+truncateErr(err)))
	case strings.Contains(err.Error(), "unknown scenario"):
		writeJSON(w, http.StatusBadRequest, errJSON("unknown_scenario", err.Error()))
	case strings.Contains(err.Error(), "cluster_id is required"):
		writeJSON(w, http.StatusBadRequest, errJSON("cluster_id_required", err.Error()))
	default:
		writeJSON(w, http.StatusInternalServerError, errJSON("diagnosis_failed", truncateErr(err)))
	}
}

// recordDiagnosis persists a completed run to the session cache (hot) and to
// MySQL (write-through, async), then emits the completion log line. Both the
// buffered and the streaming transport share it so their behaviour cannot
// drift.
func (s *Server) recordDiagnosis(req *DiagnoseRequest, rpt *diagagent.Report) {
	entry := store.EntryFromReport(rpt, req.SessionID)
	if req.SessionID != "" {
		s.sessions.Append(req.SessionID, entry)
	}
	if s.dbStore.Enabled() {
		go func() {
			saveCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			s.dbStore.Save(saveCtx, entry)
		}()
	}

	s.log.Info("diagnosis done",
		"component", req.Component, "problem", req.Problem,
		"status", rpt.Status, "confidence", rpt.Confidence, "duration_ms", rpt.DurationMS,
		"warnings", len(rpt.Warnings),
		"phases", strings.Join(rpt.PhasesRun, ","))
}

// runDiagnosis executes the full pipeline. onEvent, when non-nil, receives the
// agent's narration as it happens; nil falls back to debug logging.
func (s *Server) runDiagnosis(ctx context.Context, req *DiagnoseRequest, onEvent func(string)) (*diagagent.Report, string, error) {
	c, err := s.resolveCluster(ctx, req.ClusterID, req.Scenario)
	if err != nil {
		return nil, "", err
	}

	// Per-request agent + toolset: the cluster-bound tools and the agent's
	// per-diagnose state (eventID, llmCalls) are request-local, so concurrent
	// requests never share mutable state.
	kb := s.knowledgeBase()
	defs := v3tools.BuildV3Tools(c, kb, s.mcpClient(), s.deepwikiClient())

	if onEvent == nil {
		onEvent = func(msg string) { s.log.Debug("event", "msg", msg) }
	}
	agent := diagagent.New(
		diagagent.WithKB(kb),
		diagagent.WithEventFunc(onEvent),
	)
	agent.RegisterTools(defs)

	contextData := diagnosisContext(req)
	for k, v := range c.Context() {
		if _, ok := contextData[k]; !ok {
			contextData[k] = v
		}
	}
	// Inject conversation history so the LLM knows what was discussed before.
	if hist := s.history(req.SessionID); len(hist) > 0 {
		contextData["session_history"] = hist
	}

	var rpt *diagagent.Report
	if req.Investigate {
		rpt, err = agent.Investigate(ctx, req.Component, req.Problem, contextData)
	} else {
		rpt, err = agent.Diagnose(ctx, req.Component, req.Problem, contextData)
	}
	if err != nil {
		return nil, "", err
	}
	return rpt, report.Render(rpt), nil
}

// diagnosisContext copies the caller's context and fills the fields the YAML
// strategies rely on. Copying avoids mutating a map owned by the request while
// still making job_id available to {{ context.job_id }} templates.
func diagnosisContext(req *DiagnoseRequest) map[string]any {
	contextData := make(map[string]any, len(req.Context)+4)
	for k, v := range req.Context {
		contextData[k] = v
	}
	if _, ok := contextData["component"]; !ok {
		contextData["component"] = req.Component
	}
	if _, ok := contextData["problem"]; !ok {
		contextData["problem"] = req.Problem
	}
	if req.Detail != "" {
		if _, ok := contextData["detail"]; !ok {
			contextData["detail"] = req.Detail
		}
	}
	if req.JobID != "" {
		contextData["job_id"] = req.JobID
	}
	return contextData
}

// knowledgeBase is process-scoped so fingerprints learned from one diagnosis
// are available to the next request. The vector store is still in-memory; this
// removes the per-request reset without pretending the KB is durable.
func (s *Server) knowledgeBase() *knowledgebase.KnowledgeBase {
	s.kbOnce.Do(func() {
		s.kb = knowledgebase.New(vectorstore.NewMemory())
	})
	return s.kb
}

// resolveCluster builds the cluster adapter for a request. In demo mode this is
// a simulated scenario; in production it is a RealCluster bound to the
// availability zone this instance serves.
func (s *Server) resolveCluster(ctx context.Context, clusterID, scenario string) (cluster.Cluster, error) {
	if !s.isProduction() {
		if scenario == "" {
			scenario = "flink_oom"
		}
		return simulated.NewCluster(scenario)
	}
	if clusterID == "" {
		return nil, fmt.Errorf("cluster_id is required in production mode")
	}
	access, err := s.clusterAccess()
	if err != nil {
		return nil, err
	}
	c := infra.NewRealCluster(clusterID, s.region, s.zone, access, infra.NewSSHExec())
	if err := c.EnsureNodeData(ctx); err != nil {
		return nil, fmt.Errorf("resolve cluster %s in zone %s: %w", clusterID, s.zone, err)
	}
	return c, nil
}

// clusterAccess lazily builds the zone's UHadoop access client from ZooKeeper
// discovery. It is built once and shared across requests.
func (s *Server) clusterAccess() (*infra.ClusterAccess, error) {
	s.accessOnce.Do(func() {
		cfg := config.Get()
		zone := cfg.ZKZone()
		if zone == "" {
			zone = s.zone
		}
		if len(cfg.Infra.ZKHosts) == 0 {
			s.accessErr = fmt.Errorf("DIAGFLOW_ZK_HOSTS is required in production: " +
				"cluster endpoints are discovered, not hard-coded")
			return
		}
		s.access = infra.NewZoneClusterAccess(infra.ZKOptions{
			Hosts:   cfg.Infra.ZKHosts,
			Root:    cfg.ZKRootOr(infra.DefaultZKRoot),
			ZoneID:  zone,
			Timeout: time.Duration(cfg.Infra.ZKTimeoutS) * time.Second,
		})
		s.log.Info("cluster access configured", "zone", zone, "service", infra.ServiceUHadoopAccess)
	})
	return s.access, s.accessErr
}

func (s *Server) isProduction() bool { return config.Get().IsProduction() }

// mcpClient returns the node-log MCP client when configured (DIAGFLOW_MCP_BASE_URL),
// else nil for direct infra calls.
func (s *Server) mcpClient() *mcpclient.Client {
	cfg := config.Get()
	if cfg.MCP.BaseURL == "" {
		return nil
	}
	return mcpclient.New(cfg.MCP.BaseURL, cfg.MCP.Token)
}

// deepwikiClient returns the DeepWiki MCP client (a separate connection from
// the node-log gateway), or nil when disabled.
func (s *Server) deepwikiClient() *mcpclient.Client {
	url := config.Get().DeepWiki.MCPURL
	if url == "" {
		return nil
	}
	return mcpclient.New(url, "")
}

// history returns the session's past diagnoses: memory hit first, MySQL
// replay on cold start (service restart / different replica).
func (s *Server) history(sessionID string) []*store.Entry {
	if sessionID == "" {
		return nil
	}
	if hist := s.sessions.History(sessionID); hist != nil {
		return hist
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	hist, err := s.dbStore.ListBySession(ctx, sessionID, 20)
	if err != nil {
		s.log.Warn("session replay failed", "session_id", sessionID, "error", err)
		return nil
	}
	for _, e := range hist {
		s.sessions.Append(sessionID, e)
	}
	return hist
}

// ---- middleware ----

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (s *Server) recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic in handler",
					"path", r.URL.Path, "panic", fmt.Sprint(rec), "stack", string(debug.Stack()))
				writeJSON(w, http.StatusInternalServerError,
					errJSON("internal_error", "internal server error"))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" {
			id = newRequestID(&s.requestID)
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
	})
}

type requestIDKey struct{}

// RequestID returns the request id from ctx ("" if absent).
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

func newRequestID(counter *atomic.Int64) string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err == nil {
		return hex.EncodeToString(b[:])
	}
	return fmt.Sprintf("req-%d", counter.Add(1))
}

// ---- metrics ----

var (
	diagTotal    = metrics.NewCounter("diagflow_diagnoses_total", "Diagnoses by component/status", "component", "status")
	diagDuration = metrics.NewHistogram("diagflow_diagnosis_duration_ms", "Diagnosis wall time in ms")
	toolTotal    = metrics.NewCounter("diagflow_tool_calls_total", "Tool calls by tool/status", "tool", "status")
)

func statusFor(err error) string {
	if err != nil {
		return "error"
	}
	return "ok"
}

func diagnosisStatus(err error, rpt *diagagent.Report) string {
	if err != nil {
		return "error"
	}
	if rpt != nil && rpt.Status == diagagent.ReportStatusDegraded {
		return "degraded"
	}
	return "ok"
}

func diagnosisErrorCode(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "diagnosis_timeout"
	case errors.Is(err, diagagent.ErrLLMUnavailable):
		return "llm_unavailable"
	case errors.Is(err, diagagent.ErrNoConclusion):
		return "no_conclusion"
	case strings.Contains(err.Error(), "unknown scenario"):
		return "unknown_scenario"
	case strings.Contains(err.Error(), "cluster_id is required"):
		return "cluster_id_required"
	default:
		return "diagnosis_failed"
	}
}

// ---- helpers ----

func errJSON(code, message string) map[string]any {
	return map[string]any{"error": map[string]string{"code": code, "message": message}}
}

func truncateErr(err error) string {
	msg := err.Error()
	if len(msg) > 500 {
		return msg[:500]
	}
	return msg
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
