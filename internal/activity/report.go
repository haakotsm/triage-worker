package activity

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
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
			alert_count = EXCLUDED.alert_count,
			completed_at = EXCLUDED.completed_at,
			summary = EXCLUDED.summary,
			blast_radius = EXCLUDED.blast_radius,
			state = 'reported',
			resolved_at = NULL
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
// Valid states: correlating, enriching, triaging, reported, resolved, failed.
// State can only move forward (monotonic) to prevent backward transitions from retries.
// 'failed' is a terminal state that can be reached from any non-terminal state.
func (r *ReportActivity) UpdateIncidentState(ctx context.Context, workflowID, state, severity string) error {
	if r.DB == nil {
		return nil
	}
	_, err := r.DB.ExecContext(ctx,
		`UPDATE triage.reports SET state = $2, severity = CASE WHEN $3 = '' THEN severity ELSE $3 END
		 WHERE workflow_id = $1
		   AND CASE state
		     WHEN 'correlating' THEN 1
		     WHEN 'enriching'   THEN 2
		     WHEN 'triaging'    THEN 3
		     WHEN 'reported'    THEN 4
		     WHEN 'resolved'    THEN 5
		     WHEN 'failed'      THEN 6
		     ELSE 0
		   END < CASE $2::text
		     WHEN 'correlating' THEN 1
		     WHEN 'enriching'   THEN 2
		     WHEN 'triaging'    THEN 3
		     WHEN 'reported'    THEN 4
		     WHEN 'resolved'    THEN 5
		     WHEN 'failed'      THEN 6
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
// The row is created with state='correlating' and placeholder defaults. The full
// report data is filled in later by StoreTriageReport via UPSERT.
func (r *ReportActivity) CreateIncident(ctx context.Context, workflowID, namespace, workload, kind, alertName string) error {
	if r.DB == nil {
		return nil
	}
	_, err := r.DB.ExecContext(ctx,
		`INSERT INTO triage.reports (workflow_id, namespace, workload, kind, alert_name, state)
		 VALUES ($1, $2, $3, $4, $5, 'correlating')
		 ON CONFLICT (workflow_id) DO NOTHING`,
		workflowID, namespace, workload, kind, alertName,
	)
	if err != nil {
		return fmt.Errorf("create incident: %w", err)
	}
	return nil
}

// MigrateSchema creates the triage schema and reports table if they don't exist.
func MigrateSchema(ctx context.Context, db *sql.DB) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

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

		-- Make completed_at nullable for in-flight reports.
		ALTER TABLE triage.reports ALTER COLUMN completed_at DROP NOT NULL;

		-- Enforce valid lifecycle states.
		-- Clean up any invalid state values first.
		UPDATE triage.reports SET state = 'reported'
			WHERE state NOT IN ('correlating','enriching','triaging','reported','resolved','failed');
		DO $$ BEGIN
			ALTER TABLE triage.reports ADD CONSTRAINT chk_state
				CHECK (state IN ('correlating','enriching','triaging','reported','resolved','failed'));
		EXCEPTION WHEN duplicate_object THEN NULL;
		END $$;

		-- Drop legacy incidents table if it exists (merged into reports).
		DROP TABLE IF EXISTS triage.incidents;
		DROP FUNCTION IF EXISTS triage.notify_incident_change() CASCADE;
	`
	if _, err := db.ExecContext(ctx, ddl); err != nil {
		return fmt.Errorf("ddl: %w", err)
	}

	// Phase 2: DML + triggers — runs after columns are visible.
	dml := `
		CREATE INDEX IF NOT EXISTS idx_reports_state ON triage.reports (state);

		-- Composite index for dashboard queries (filter by state, sort by created_at).
		CREATE INDEX IF NOT EXISTS idx_reports_state_created ON triage.reports (state, created_at DESC);

		-- Backfill state for existing rows.
		UPDATE triage.reports SET state = 'resolved' WHERE resolved_at IS NOT NULL AND state = 'reported';

		-- NOTIFY trigger on report changes — only fires when tracked columns change.
		CREATE OR REPLACE FUNCTION triage.notify_report_change() RETURNS trigger AS $$
		BEGIN
			IF TG_OP = 'INSERT' OR
			   OLD.state IS DISTINCT FROM NEW.state OR
			   OLD.severity IS DISTINCT FROM NEW.severity OR
			   OLD.resolved_at IS DISTINCT FROM NEW.resolved_at THEN
				PERFORM pg_notify('report_changes', json_build_object(
					'id',          NEW.id,
					'workflow_id', NEW.workflow_id,
					'state',       NEW.state,
					'severity',    NEW.severity,
					'namespace',   NEW.namespace,
					'workload',    NEW.workload,
					'resolved',    (NEW.resolved_at IS NOT NULL)
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
	if _, err := db.ExecContext(ctx, dml); err != nil {
		return fmt.Errorf("dml: %w", err)
	}

	return nil
}
