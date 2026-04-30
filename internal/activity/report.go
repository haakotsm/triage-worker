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

	query := `
		INSERT INTO triage.reports (
			workflow_id, namespace, workload, kind, alert_name,
			classification, severity, root_cause, causal_chain,
			evidence, recommendations, confidence,
			escalation_needed, alert_count, started_at, completed_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
		ON CONFLICT (workflow_id) DO UPDATE SET
			classification = EXCLUDED.classification,
			severity = EXCLUDED.severity,
			root_cause = EXCLUDED.root_cause,
			causal_chain = EXCLUDED.causal_chain,
			evidence = EXCLUDED.evidence,
			recommendations = EXCLUDED.recommendations,
			confidence = EXCLUDED.confidence,
			escalation_needed = EXCLUDED.escalation_needed,
			completed_at = EXCLUDED.completed_at
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
	)
	if err != nil {
		return fmt.Errorf("upsert report: %w", err)
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
			resolved_at     TIMESTAMPTZ
		);

		CREATE INDEX IF NOT EXISTS idx_reports_namespace ON triage.reports (namespace);
		CREATE INDEX IF NOT EXISTS idx_reports_classification ON triage.reports (classification);
		CREATE INDEX IF NOT EXISTS idx_reports_severity ON triage.reports (severity);
		CREATE INDEX IF NOT EXISTS idx_reports_created_at ON triage.reports (created_at DESC);
	`

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	_, err := db.ExecContext(ctx, schema)
	return err
}
