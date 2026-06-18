package workflow

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"

	"github.com/haakotsm/triage-worker/internal/types"
)

// retriageWorkflowTimeout mirrors the webhook's workflowExecutionTimeout — a
// re-triage runs the same TriageWorkflow and must be bounded identically.
const retriageWorkflowTimeout = 15 * time.Minute

// maxAttemptProbe bounds how many successive attempt IDs allocateAttemptID will
// try before giving up, so a corrupt history can't spin forever.
const maxAttemptProbe = 16

// workflowSignalStarter is the subset of client.Client that RetriageStarter
// needs. Declaring it as an interface lets tests inject a fake.
type workflowSignalStarter interface {
	SignalWithStartWorkflow(ctx context.Context, workflowID, signalName string, signalArg interface{},
		options client.StartWorkflowOptions, workflow interface{}, workflowArgs ...interface{}) (client.WorkflowRun, error)
}

// RetriageStarter starts a fresh TriageWorkflow for an existing incident on
// operator request. It mints the next attempt ID for the incident's identity,
// writes a preliminary 'processing' row so the dashboard reflects the re-triage
// immediately (mirroring the webhook's resolveWorkflowID), and signals the
// workflow with a synthetic alert (TriageWorkflow requires at least one alert).
//
// It satisfies web.RetrieveStarter.
type RetriageStarter struct {
	client    workflowSignalStarter
	db        *sql.DB
	taskQueue string
	logger    *slog.Logger
}

// NewRetriageStarter wires a RetriageStarter. db is required — without it there
// is nowhere to allocate the preliminary attempt row.
func NewRetriageStarter(c client.Client, db *sql.DB, taskQueue string, logger *slog.Logger) *RetriageStarter {
	return &RetriageStarter{client: c, db: db, taskQueue: taskQueue, logger: logger}
}

// StartRetriage allocates a new attempt for the incident's identity and starts a
// TriageWorkflow for it, returning the new workflow ID. workflowID is the
// existing incident's workflow_id; the new attempt is minted from it.
func (r *RetriageStarter) StartRetriage(ctx context.Context, workflowID, namespace, workload, kind, alertName string) (string, error) {
	if r.db == nil {
		return "", fmt.Errorf("re-triage requires a database")
	}
	identity := types.IncidentIdentity{Namespace: namespace, Kind: kind, Name: workload, AlertName: alertName}

	newWfID, err := r.allocateAttemptID(ctx, identity, workflowID)
	if err != nil {
		return "", err
	}

	alert := types.Alert{
		Status:      "firing",
		StartsAt:    time.Now(),
		Fingerprint: "retriage-" + newWfID,
		Labels: map[string]string{
			"namespace": namespace,
			"workload":  workload,
			"alertname": alertName,
		},
		Annotations: map[string]string{
			"summary": "Operator-initiated re-triage of " + workflowID,
		},
	}

	opts := client.StartWorkflowOptions{
		ID:                       newWfID,
		TaskQueue:                r.taskQueue,
		WorkflowExecutionTimeout: retriageWorkflowTimeout,
		WorkflowIDConflictPolicy: enums.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING,
	}

	if _, err := r.client.SignalWithStartWorkflow(ctx, newWfID, AlertSignalName, alert, opts,
		TriageWorkflow, types.TriageParams{Identity: identity}); err != nil {
		// Leave the preliminary row — the stuck-processing TTL in the webhook's
		// open-incident lookup reaps orphaned 'processing' rows.
		return "", fmt.Errorf("signal-with-start re-triage workflow %q: %w", newWfID, err)
	}

	r.logger.Info("re-triage started", "from_workflow_id", workflowID, "new_workflow_id", newWfID,
		"namespace", namespace, "workload", workload)
	return newWfID, nil
}

// allocateAttemptID finds a free attempt ID for the identity and reserves it by
// inserting a preliminary 'processing' row. It probes upward from the attempt
// after fromWfID's, using INSERT ... ON CONFLICT DO NOTHING so a collision with
// a concurrently-minted attempt (e.g. the webhook) simply advances to the next.
func (r *RetriageStarter) allocateAttemptID(ctx context.Context, identity types.IncidentIdentity, fromWfID string) (string, error) {
	_, attempt := types.ParseAttempt(fromWfID)
	for n := attempt + 1; n <= attempt+maxAttemptProbe; n++ {
		candidate := identity.WorkflowIDForAttempt(n)
		res, err := r.db.ExecContext(ctx,
			`INSERT INTO triage.reports (workflow_id, namespace, workload, kind, alert_name, state)
			 VALUES ($1, $2, $3, $4, $5, 'processing')
			 ON CONFLICT (workflow_id) DO NOTHING`,
			candidate, identity.Namespace, identity.Name, identity.Kind, identity.AlertName)
		if err != nil {
			return "", fmt.Errorf("reserve re-triage attempt row: %w", err)
		}
		if affected, err := res.RowsAffected(); err != nil {
			return "", fmt.Errorf("rows affected: %w", err)
		} else if affected == 1 {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("no free re-triage attempt id within %d of %q", maxAttemptProbe, fromWfID)
}
