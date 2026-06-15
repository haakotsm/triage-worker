package activity

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/haakotsm/triage-worker/internal/types"
)

// ReportActivity handles storing triage reports to PostgreSQL.
type ReportActivity struct {
	DB *sql.DB
}

// StoreTriageReport persists the triage result to the reports table.
// Uses UPSERT (ON CONFLICT) for idempotency on workflow retries.
// Returns nil (no-op) if database is not configured.
func (r *ReportActivity) StoreTriageReport(ctx context.Context, result types.TriageResult) error {
	if r.DB == nil {
		return nil
	}
	if result.Report == nil {
		return fmt.Errorf("store report: report is nil for workflow %s", result.WorkflowID)
	}

	alertsJSON, err := json.Marshal(result.Report.Evidence)
	if err != nil {
		return fmt.Errorf("marshal evidence: %w", err)
	}

	recommendationsJSON, err := json.Marshal(result.Report.Recommendations)
	if err != nil {
		return fmt.Errorf("marshal recommendations: %w", err)
	}

	causalChainJSON, err := json.Marshal(result.Report.CausalChain)
	if err != nil {
		return fmt.Errorf("marshal causal chain: %w", err)
	}

	// Extract blast_radius from report impact, validated to known values.
	blastRadius := types.ValidateBlastRadius(result.Report.Impact.BlastRadius)

	summary := result.Report.Summary

	query := `
		INSERT INTO triage.reports (
			workflow_id, namespace, workload, kind, alert_name,
			classification, severity, root_cause, causal_chain,
			evidence, recommendations, confidence,
			escalation_needed, alert_count, started_at, completed_at,
			summary, blast_radius, state
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, 'reported')
		ON CONFLICT (workflow_id) DO UPDATE SET
			classification = EXCLUDED.classification,
			severity = EXCLUDED.severity,
			root_cause = EXCLUDED.root_cause,
			causal_chain = EXCLUDED.causal_chain,
			evidence = EXCLUDED.evidence,
			recommendations = EXCLUDED.recommendations,
			confidence = EXCLUDED.confidence,
			escalation_needed = EXCLUDED.escalation_needed,
			completed_at = EXCLUDED.completed_at,
			summary = EXCLUDED.summary,
			blast_radius = EXCLUDED.blast_radius,
			state = 'reported'
		WHERE triage.reports.state != 'resolved'
	`

	_, err = r.DB.ExecContext(ctx, query,
		result.WorkflowID,
		result.Identity.Namespace,
		result.Identity.Name,
		result.Identity.Kind,
		result.Identity.AlertName,
		result.Classification,
		result.Severity,
		result.RootCause,
		causalChainJSON,
		alertsJSON,
		recommendationsJSON,
		result.Report.Confidence,
		result.Report.EscalationNeeded,
		result.AlertCount,
		result.StartedAt,
		result.CompletedAt,
		summary,
		blastRadius,
	)
	if err != nil {
		return fmt.Errorf("upsert report: %w", err)
	}

	return nil
}

// UpdateIncidentState transitions a report's lifecycle state.
// Operator-visible states: processing, reported, acknowledged, resolved.
// Internal phases (correlating, enriching, triaging) are normalized to 'processing'.
// State can only move forward (monotonic) to prevent backward transitions from retries.
func (r *ReportActivity) UpdateIncidentState(ctx context.Context, workflowID, state, severity string) error {
	if r.DB == nil {
		return nil
	}

	// Normalize internal automation phases to the single operator-visible state.
	switch state {
	case "correlating", "enriching", "triaging":
		state = "processing"
	}

	_, err := r.DB.ExecContext(ctx,
		`UPDATE triage.reports SET state = $2, severity = CASE WHEN $3 = '' THEN severity ELSE $3 END
		 WHERE workflow_id = $1
		   AND CASE state
		     WHEN 'processing'   THEN 1
		     WHEN 'reported'     THEN 2
		     WHEN 'acknowledged' THEN 3
		     WHEN 'resolved'     THEN 4
		     ELSE 0
		   END < CASE $2::text
		     WHEN 'processing'   THEN 1
		     WHEN 'reported'     THEN 2
		     WHEN 'acknowledged' THEN 3
		     WHEN 'resolved'     THEN 4
		     ELSE 0
		   END`,
		workflowID, state, severity,
	)
	if err != nil {
		return fmt.Errorf("update report state: %w", err)
	}
	return nil
}

// CreateIncident inserts a preliminary report row for realtime lifecycle tracking.
// The row is created with state='processing' and placeholder defaults. The full
// report data is filled in later by StoreTriageReport via UPSERT.
func (r *ReportActivity) CreateIncident(ctx context.Context, workflowID, namespace, workload, kind, alertName string) error {
	if r.DB == nil {
		return nil
	}
	_, err := r.DB.ExecContext(ctx,
		`INSERT INTO triage.reports (workflow_id, namespace, workload, kind, alert_name, state)
		 VALUES ($1, $2, $3, $4, $5, 'processing')
		 ON CONFLICT (workflow_id) DO NOTHING`,
		workflowID, namespace, workload, kind, alertName,
	)
	if err != nil {
		return fmt.Errorf("create incident: %w", err)
	}
	return nil
}

// migrationLockKey is an arbitrary stable bigint used by pg_advisory_lock to
// serialize MigrateSchema across concurrent replicas. Any value works as long
// as every replica agrees on it.
const migrationLockKey int64 = 0x7472696167655F31 // ASCII "triage_1"

// MigrateSchema creates the triage schema and reports table if they don't exist.
//
// Concurrent replicas serialize via a Postgres advisory lock: "CREATE … IF
// NOT EXISTS" is not concurrency-safe at the catalog level — two replicas
// racing on CREATE SCHEMA can both check, both proceed to insert, and one
// gets `duplicate key value violates unique constraint
// "pg_namespace_nspname_index" (23505)`. Observed when two replicas
// restarted in lockstep after fixing the NetworkPolicy egress.
//
// The lock is held on a dedicated connection for both DDL phases and
// released explicitly so it doesn't sit around until the pooled session
// closes.
func MigrateSchema(ctx context.Context, db *sql.DB) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire conn: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", migrationLockKey); err != nil {
		return fmt.Errorf("acquire advisory lock: %w", err)
	}
	defer func() {
		// Fresh, short context so the unlock still runs if the parent
		// expired mid-migration. conn.Close() returns the connection to
		// the pool — it does NOT close the PG session — so a swallowed
		// unlock error can leave the advisory lock held on the pooled
		// session until the pool eventually recycles it (potentially
		// hours). Log loudly if the unlock fails so the operator can
		// intervene.
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := conn.ExecContext(unlockCtx, "SELECT pg_advisory_unlock($1)", migrationLockKey); err != nil {
			slog.Warn("pg_advisory_unlock failed; lock may persist until the pooled session closes",
				"error", err, "lock_key", migrationLockKey)
		}
	}()

	// Phase 1: DDL — create schema, table, indexes, add columns.
	ddl := `
		CREATE SCHEMA IF NOT EXISTS triage;

		CREATE TABLE IF NOT EXISTS triage.reports (
			id              BIGSERIAL PRIMARY KEY,
			workflow_id     TEXT UNIQUE NOT NULL,
			namespace       TEXT NOT NULL,
			workload        TEXT NOT NULL,
			kind            TEXT NOT NULL,
			alert_name      TEXT NOT NULL,
			classification  TEXT NOT NULL DEFAULT '',
			severity        TEXT NOT NULL DEFAULT '',
			root_cause      TEXT NOT NULL DEFAULT '',
			causal_chain    JSONB DEFAULT '[]',
			evidence        JSONB DEFAULT '[]',
			recommendations JSONB DEFAULT '[]',
			confidence      REAL NOT NULL DEFAULT 0,
			escalation_needed BOOLEAN NOT NULL DEFAULT false,
			alert_count     INTEGER NOT NULL DEFAULT 1,
			started_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			completed_at    TIMESTAMPTZ,
			created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			resolved_at     TIMESTAMPTZ,
			summary         TEXT NOT NULL DEFAULT '',
			blast_radius    TEXT NOT NULL DEFAULT 'pod',
			state           TEXT NOT NULL DEFAULT 'reported'
		);

		CREATE INDEX IF NOT EXISTS idx_reports_namespace ON triage.reports (namespace);
		CREATE INDEX IF NOT EXISTS idx_reports_classification ON triage.reports (classification);
		CREATE INDEX IF NOT EXISTS idx_reports_severity ON triage.reports (severity);
		CREATE INDEX IF NOT EXISTS idx_reports_created_at ON triage.reports (created_at DESC);

		-- Migration: add columns if table already exists.
		ALTER TABLE triage.reports ADD COLUMN IF NOT EXISTS summary TEXT NOT NULL DEFAULT '';
		ALTER TABLE triage.reports ADD COLUMN IF NOT EXISTS blast_radius TEXT NOT NULL DEFAULT 'pod';
		ALTER TABLE triage.reports ADD COLUMN IF NOT EXISTS state TEXT NOT NULL DEFAULT 'reported';

		-- Incident workflow columns (v2 lean model).
		ALTER TABLE triage.reports ADD COLUMN IF NOT EXISTS assigned_to TEXT;
		ALTER TABLE triage.reports ADD COLUMN IF NOT EXISTS acknowledged_at TIMESTAMPTZ;
		ALTER TABLE triage.reports ADD COLUMN IF NOT EXISTS escalated_at TIMESTAMPTZ;
		ALTER TABLE triage.reports ADD COLUMN IF NOT EXISTS escalated_to TEXT;
		ALTER TABLE triage.reports ADD COLUMN IF NOT EXISTS escalation_level TEXT;
		ALTER TABLE triage.reports ADD COLUMN IF NOT EXISTS resolution_note TEXT;
		ALTER TABLE triage.reports ADD COLUMN IF NOT EXISTS resolution_source TEXT;
		ALTER TABLE triage.reports ADD COLUMN IF NOT EXISTS mitigation_started_at TIMESTAMPTZ;

		-- Make completed_at nullable for in-flight reports.
		ALTER TABLE triage.reports ALTER COLUMN completed_at DROP NOT NULL;

		-- Normalize legacy internal-phase states BEFORE adding constraint.
		UPDATE triage.reports SET state = 'processing'
			WHERE state IN ('correlating', 'enriching', 'triaging');

		-- Update lifecycle state constraint to the lean 4-state model.
		ALTER TABLE triage.reports DROP CONSTRAINT IF EXISTS chk_state;
		ALTER TABLE triage.reports ADD CONSTRAINT chk_state
			CHECK (state IN ('processing','reported','acknowledged','resolved'));

		-- Incident notes table for append-only operator commentary.
		CREATE TABLE IF NOT EXISTS triage.incident_notes (
			id          BIGSERIAL PRIMARY KEY,
			incident_id BIGINT NOT NULL REFERENCES triage.reports(id),
			author      TEXT NOT NULL,
			body        TEXT NOT NULL,
			created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);

		CREATE INDEX IF NOT EXISTS idx_incident_notes_incident_created
			ON triage.incident_notes (incident_id, created_at DESC);

		-- Drop legacy incidents table if it exists (merged into reports).
		DROP TABLE IF EXISTS triage.incidents;
		DROP FUNCTION IF EXISTS triage.notify_incident_change() CASCADE;
	`
	if _, err := conn.ExecContext(ctx, ddl); err != nil {
		return fmt.Errorf("ddl: %w", err)
	}

	// Phase 2: DML + triggers — runs after columns are visible.
	dml := `
		CREATE INDEX IF NOT EXISTS idx_reports_state ON triage.reports (state);

		-- Composite index for dashboard queries (filter by state, sort by created_at).
		CREATE INDEX IF NOT EXISTS idx_reports_state_created ON triage.reports (state, created_at DESC);

		-- Index for acknowledged incidents lookup.
		CREATE INDEX IF NOT EXISTS idx_reports_assigned_to ON triage.reports (assigned_to) WHERE assigned_to IS NOT NULL;

		-- Backfill state for existing rows that were resolved before state tracking.
		UPDATE triage.reports SET state = 'resolved' WHERE resolved_at IS NOT NULL AND state = 'reported';

		-- NOTIFY trigger on report changes — fires when tracked columns change.
		CREATE OR REPLACE FUNCTION triage.notify_report_change() RETURNS trigger AS $$
		BEGIN
			IF TG_OP = 'INSERT' OR
			   OLD.state IS DISTINCT FROM NEW.state OR
			   OLD.severity IS DISTINCT FROM NEW.severity OR
			   OLD.resolved_at IS DISTINCT FROM NEW.resolved_at OR
			   OLD.assigned_to IS DISTINCT FROM NEW.assigned_to OR
			   OLD.escalation_level IS DISTINCT FROM NEW.escalation_level THEN
				PERFORM pg_notify('report_changes', json_build_object(
					'id',          NEW.id,
					'workflow_id', NEW.workflow_id,
					'state',       NEW.state,
					'severity',    NEW.severity,
					'namespace',   NEW.namespace,
					'workload',    NEW.workload,
					'resolved',    (NEW.resolved_at IS NOT NULL),
					'assigned_to', COALESCE(NEW.assigned_to, ''),
					'escalation_level', COALESCE(NEW.escalation_level, '')
				)::text);
			END IF;
			RETURN NEW;
		END;
		$$ LANGUAGE plpgsql;

		DROP TRIGGER IF EXISTS trg_report_change ON triage.reports;
		CREATE TRIGGER trg_report_change
			AFTER INSERT OR UPDATE ON triage.reports
			FOR EACH ROW EXECUTE FUNCTION triage.notify_report_change();
	`
	if _, err := conn.ExecContext(ctx, dml); err != nil {
		return fmt.Errorf("dml: %w", err)
	}

	return nil
}
