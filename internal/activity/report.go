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
			summary, blast_radius
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)
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
			blast_radius = EXCLUDED.blast_radius
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

	// Transition the incident to "reported" now that the report is stored.
	_, _ = r.DB.ExecContext(ctx,
		`UPDATE triage.incidents SET state = 'reported', severity = $2, updated_at = NOW()
		 WHERE workflow_id = $1`,
		result.WorkflowID, result.Severity,
	)

	return nil
}

// UpdateIncidentState transitions an incident's lifecycle state.
// Valid states: correlating, enriching, triaging, reported.
func (r *ReportActivity) UpdateIncidentState(ctx context.Context, workflowID, state, severity string) error {
	if r.DB == nil {
		return nil
	}
	_, err := r.DB.ExecContext(ctx,
		`UPDATE triage.incidents SET state = $2, severity = CASE WHEN $3 = '' THEN severity ELSE $3 END, updated_at = NOW()
		 WHERE workflow_id = $1`,
		workflowID, state, severity,
	)
	if err != nil {
		return fmt.Errorf("update incident state: %w", err)
	}
	return nil
}

// CreateIncident inserts a new incident row for realtime lifecycle tracking.
func (r *ReportActivity) CreateIncident(ctx context.Context, workflowID, namespace, workload, kind, alertName string) error {
	if r.DB == nil {
		return nil
	}
	_, err := r.DB.ExecContext(ctx,
		`INSERT INTO triage.incidents (workflow_id, namespace, workload, kind, alert_name, state)
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
	schema := `
		CREATE SCHEMA IF NOT EXISTS triage;

		CREATE TABLE IF NOT EXISTS triage.reports (
			id              BIGSERIAL PRIMARY KEY,
			workflow_id     TEXT UNIQUE NOT NULL,
			namespace       TEXT NOT NULL,
			workload        TEXT NOT NULL,
			kind            TEXT NOT NULL,
			alert_name      TEXT NOT NULL,
			classification  TEXT NOT NULL,
			severity        TEXT NOT NULL,
			root_cause      TEXT NOT NULL,
			causal_chain    JSONB DEFAULT '[]',
			evidence        JSONB DEFAULT '[]',
			recommendations JSONB DEFAULT '[]',
			confidence      REAL NOT NULL DEFAULT 0,
			escalation_needed BOOLEAN NOT NULL DEFAULT false,
			alert_count     INTEGER NOT NULL DEFAULT 1,
			started_at      TIMESTAMPTZ NOT NULL,
			completed_at    TIMESTAMPTZ NOT NULL,
			created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			resolved_at     TIMESTAMPTZ,
			summary         TEXT NOT NULL DEFAULT '',
			blast_radius    TEXT NOT NULL DEFAULT 'pod'
		);

		CREATE INDEX IF NOT EXISTS idx_reports_namespace ON triage.reports (namespace);
		CREATE INDEX IF NOT EXISTS idx_reports_classification ON triage.reports (classification);
		CREATE INDEX IF NOT EXISTS idx_reports_severity ON triage.reports (severity);
		CREATE INDEX IF NOT EXISTS idx_reports_created_at ON triage.reports (created_at DESC);

		-- Migration: add columns if table already exists
		ALTER TABLE triage.reports ADD COLUMN IF NOT EXISTS summary TEXT NOT NULL DEFAULT '';
		ALTER TABLE triage.reports ADD COLUMN IF NOT EXISTS blast_radius TEXT NOT NULL DEFAULT 'pod';

		-- Incidents table: tracks workflow lifecycle for realtime dashboard.
		CREATE TABLE IF NOT EXISTS triage.incidents (
			id           BIGSERIAL PRIMARY KEY,
			workflow_id  TEXT UNIQUE NOT NULL,
			namespace    TEXT NOT NULL,
			workload     TEXT NOT NULL,
			kind         TEXT NOT NULL DEFAULT 'Pod',
			alert_name   TEXT NOT NULL DEFAULT '',
			state        TEXT NOT NULL DEFAULT 'correlating',
			severity     TEXT NOT NULL DEFAULT '',
			created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);

		CREATE INDEX IF NOT EXISTS idx_incidents_state ON triage.incidents (state);
		CREATE INDEX IF NOT EXISTS idx_incidents_updated ON triage.incidents (updated_at DESC);

		-- Notify function: sends JSON payload on incident changes.
		CREATE OR REPLACE FUNCTION triage.notify_incident_change() RETURNS trigger AS $$
		BEGIN
			PERFORM pg_notify('incident_changes', json_build_object(
				'id',          NEW.id,
				'workflow_id', NEW.workflow_id,
				'state',       NEW.state,
				'namespace',   NEW.namespace,
				'workload',    NEW.workload,
				'severity',    NEW.severity,
				'updated_at',  NEW.updated_at
			)::text);
			RETURN NEW;
		END;
		$$ LANGUAGE plpgsql;

		-- Drop and recreate trigger to ensure latest version.
		DROP TRIGGER IF EXISTS trg_incident_change ON triage.incidents;
		CREATE TRIGGER trg_incident_change
			AFTER INSERT OR UPDATE ON triage.incidents
			FOR EACH ROW EXECUTE FUNCTION triage.notify_incident_change();

		-- Notify on report changes too (new report stored, resolved).
		CREATE OR REPLACE FUNCTION triage.notify_report_change() RETURNS trigger AS $$
		BEGIN
			PERFORM pg_notify('report_changes', json_build_object(
				'id',          NEW.id,
				'workflow_id', NEW.workflow_id,
				'severity',    NEW.severity,
				'resolved',    (NEW.resolved_at IS NOT NULL)
			)::text);
			RETURN NEW;
		END;
		$$ LANGUAGE plpgsql;

		DROP TRIGGER IF EXISTS trg_report_change ON triage.reports;
		CREATE TRIGGER trg_report_change
			AFTER INSERT OR UPDATE ON triage.reports
			FOR EACH ROW EXECUTE FUNCTION triage.notify_report_change();
	`

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	_, err := db.ExecContext(ctx, schema)
	return err
}
