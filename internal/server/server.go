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
	mux.Handle("POST /api/v1/investigate", s.authMiddleware(http.HandlerFunc(s.handleDiagnose)))
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

// readyz reports whether the process can serve diagnoses (dependency probe:
// config is loadable and, in production, cluster discovery is configured).
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	cfg := config.Get()
	info := map[string]any{
		"status": "ready",
		"mode":   modeLabel(cfg),
		"region": cfg.Region,
		"zone":   cfg.Zone,
	}
	if cfg.IsProduction() {
		if len(cfg.Infra.ZKHosts) == 0 {
			info["status"] = "not ready"
			info["reason"] = "DIAGFLOW_ZK_HOSTS is required in production"
			writeJSON(w, http.StatusServiceUnavailable, info)
			return
		}
		info["zk_hosts"] = cfg.Infra.ZKHosts
		info["zk_zone"] = cfg.ZKZone()
	}
	writeJSON(w, http.StatusOK, info)
}

func modeLabel(cfg *config.Config) string {
	if cfg.IsProduction() {
		return "production"
	}
	return "demo"
}

func (s *Server) handleDiagnose(w http.ResponseWriter, r *http.Request) {
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

	ctx, cancel := context.WithTimeout(r.Context(),
		time.Duration(s.cfg.RequestTimeoutS)*time.Second)
	defer cancel()

	start := time.Now()
	rpt, markdown, err := s.diagnose(ctx, &req, nil)
	durationMS := float64(time.Since(start).Milliseconds())
	diagTotal.Inc(req.Component, statusFor(err))
	if err != nil {
		s.log.Error("diagnosis failed", "component", req.Component, "problem", req.Problem, "error", err)
		if strings.Contains(err.Error(), "unknown scenario") {
			writeJSON(w, http.StatusBadRequest, errJSON("unknown_scenario", err.Error()))
			return
		}
		writeJSON(w, http.StatusInternalServerError, errJSON("diagnosis_failed", truncateErr(err)))
		return
	}
	diagDuration.Observe(durationMS)

	s.recordDiagnosis(&req, rpt)

	writeJSON(w, http.StatusOK, map[string]any{
		"report":   rpt,
		"markdown": markdown,
	})
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
		"confidence", rpt.Confidence, "duration_ms", rpt.DurationMS,
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
	vstore := vectorstore.NewMemory()
	kb := knowledgebase.New(vstore)
	defs := v3tools.BuildV3Tools(c, kb, s.mcpClient(), s.deepwikiClient())

	if onEvent == nil {
		onEvent = func(msg string) { s.log.Debug("event", "msg", msg) }
	}
	agent := diagagent.New(
		diagagent.WithKB(kb),
		diagagent.WithEventFunc(onEvent),
	)
	agent.RegisterTools(defs)

	contextData := req.Context
	if contextData == nil {
		contextData = map[string]any{}
	}
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
