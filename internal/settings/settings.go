// Package settings provides a tiny DB-backed store for runtime-toggleable
// operational flags, with an in-memory atomic cache for hot-path reads.
package settings

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sync/atomic"
)

const keyWorkflowsEnabled = "workflows_enabled"

// Store holds runtime settings. The single-replica triage-worker keeps the
// authoritative value in Postgres (so a dashboard toggle survives pod restarts
// and image redeploys) and caches it in an atomic.Bool for the webhook hot
// path, which checks WorkflowsEnabled on every firing alert.
type Store struct {
	db               *sql.DB
	logger           *slog.Logger
	workflowsEnabled atomic.Bool
}

// New returns a Store defaulting to workflows ENABLED (fail-open). Call Load to
// prime the cache from the database.
func New(db *sql.DB, logger *slog.Logger) *Store {
	s := &Store{db: db, logger: logger}
	s.workflowsEnabled.Store(true)
	return s
}

// Load primes the in-memory cache from the database. A missing row leaves the
// default (enabled) in place. A nil db is a no-op (in-memory only).
func (s *Store) Load(ctx context.Context) error {
	if s.db == nil {
		return nil
	}
	var v string
	err := s.db.QueryRowContext(ctx,
		`SELECT value FROM triage.settings WHERE key = $1`, keyWorkflowsEnabled).Scan(&v)
	switch {
	case err == sql.ErrNoRows:
		return nil
	case err != nil:
		return fmt.Errorf("load %s: %w", keyWorkflowsEnabled, err)
	}
	s.workflowsEnabled.Store(v == "true")
	s.logger.Info("loaded settings", keyWorkflowsEnabled, s.workflowsEnabled.Load())
	return nil
}

// WorkflowsEnabled reports whether the webhook may start new triage workflows.
// It reads the in-memory cache, so it is safe on the per-alert hot path.
func (s *Store) WorkflowsEnabled() bool {
	return s.workflowsEnabled.Load()
}

// SetWorkflowsEnabled persists the flag, then updates the in-memory cache. The
// cache is only updated after a successful persist so the two cannot diverge
// across a restart (a failed write leaves both at their previous value).
func (s *Store) SetWorkflowsEnabled(ctx context.Context, enabled bool) error {
	if s.db != nil {
		v := "false"
		if enabled {
			v = "true"
		}
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO triage.settings (key, value, updated_at)
			 VALUES ($1, $2, NOW())
			 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = NOW()`,
			keyWorkflowsEnabled, v); err != nil {
			return fmt.Errorf("persist %s: %w", keyWorkflowsEnabled, err)
		}
	}
	s.workflowsEnabled.Store(enabled)
	return nil
}
