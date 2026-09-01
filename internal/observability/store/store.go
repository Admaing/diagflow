// Package store persists diagnosis history to MySQL (write-through) and
// replays session history on cold start. All failures degrade gracefully:
// callers get nil Store and the app runs without persistence.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/Admaing/diagflow/internal/config"
	"github.com/Admaing/diagflow/internal/diagagent"
)

// Store is the MySQL-backed diagnosis history. nil = disabled.
type Store struct {
	db  *sql.DB
	log *slog.Logger
}

// Entry is one persisted diagnosis record.
type Entry struct {
	EventID     string
	SessionID   string
	Component   string
	ProblemType string
	RootCause   string
	Confidence  string
	Suggestions []string
	EvidenceCnt int
	KBMatched   bool
	DurationMS  int
	LLMCalls    int
	CreatedAt   time.Time
}

// New opens the MySQL pool and verifies connectivity. Returns nil (disabled)
// when the host is unset or unreachable.
func New(log *slog.Logger) *Store {
	cfg := config.Get()
	if cfg.MySQL.Host == "" {
		log.Info("mysql disabled: no host configured")
		return nil
	}
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?parseTime=true&timeout=3s",
		cfg.MySQL.User, cfg.MySQL.Password, cfg.MySQL.Host, cfg.MySQL.Port, cfg.MySQL.Database)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		log.Warn("mysql disabled: open failed", "error", err)
		return nil
	}
	db.SetMaxOpenConns(cfg.MySQL.PoolMax)
	db.SetMaxIdleConns(cfg.MySQL.PoolMin)
	db.SetConnMaxLifetime(30 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		log.Warn("mysql disabled: unreachable", "error", err)
		_ = db.Close()
		return nil
	}
	log.Info("mysql connected", "host", cfg.MySQL.Host, "database", cfg.MySQL.Database)
	return &Store{db: db, log: log}
}

// Enabled reports whether persistence is active.
func (s *Store) Enabled() bool { return s != nil }

// Save persists one diagnosis record (call in a goroutine — never block the
// request path).
func (s *Store) Save(ctx context.Context, e *Entry) {
	if s == nil {
		return
	}
	sugg, _ := json.Marshal(e.Suggestions)
	_, err := s.db.ExecContext(ctx, `INSERT INTO diagnosis_history
		(event_id, session_id, component, problem_type, root_cause, confidence,
		 suggestions, evidence_count, kb_matched, duration_ms, llm_calls)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		e.EventID, e.SessionID, e.Component, e.ProblemType, e.RootCause, e.Confidence,
		string(sugg), e.EvidenceCnt, e.KBMatched, e.DurationMS, e.LLMCalls)
	if err != nil {
		s.log.Warn("save diagnosis failed", "event_id", e.EventID, "error", err)
	}
}

// ListBySession returns the session's recent diagnoses oldest-first.
func (s *Store) ListBySession(ctx context.Context, sessionID string, limit int) ([]*Entry, error) {
	if s == nil || sessionID == "" {
		return nil, nil
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `SELECT event_id, session_id, component, problem_type,
			root_cause, confidence, suggestions, evidence_count, kb_matched,
			duration_ms, llm_calls, created_at
		FROM diagnosis_history WHERE session_id = ? ORDER BY id DESC LIMIT ?`,
		sessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*Entry
	for rows.Next() {
		var e Entry
		var sugg []byte
		var createdAt sql.NullTime
		if err := rows.Scan(&e.EventID, &e.SessionID, &e.Component, &e.ProblemType,
			&e.RootCause, &e.Confidence, &sugg, &e.EvidenceCnt, &e.KBMatched,
			&e.DurationMS, &e.LLMCalls, &createdAt); err != nil {
			return nil, err
		}
		if createdAt.Valid {
			e.CreatedAt = createdAt.Time
		}
		if len(sugg) > 0 {
			_ = json.Unmarshal(sugg, &e.Suggestions)
		}
		out = append(out, &e)
	}
	// oldest-first
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// EntryFromReport builds a persistence entry from a diagnosis report.
func EntryFromReport(report *diagagent.Report, sessionID string) *Entry {
	return &Entry{
		EventID:     report.EventID,
		SessionID:   sessionID,
		Component:   report.Component,
		ProblemType: report.ProblemType,
		RootCause:   report.RootCause,
		Confidence:  report.Confidence,
		Suggestions: report.Suggestions,
		EvidenceCnt: len(report.EvidenceSummary),
		KBMatched:   report.MatchedKnowledge,
		DurationMS:  int(report.DurationMS),
		LLMCalls:    report.LLMCalls,
	}
}
