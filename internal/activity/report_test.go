package activity

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// Anchored regex patterns that pin the full shape of the production SQL.
// These intentionally duplicate the query structure so any drift in the
// production statements — including operator changes like `<` → `<=` in the
// monotonic-state guard — fails the test rather than silently passing a
// substring match.
const (
	createIncidentSQLPattern = `^INSERT INTO triage\.reports \(workflow_id, namespace, workload, kind, alert_name, state\)\s+` +
		`VALUES \(\$1, \$2, \$3, \$4, \$5, 'processing'\)\s+` +
		`ON CONFLICT \(workflow_id\) DO NOTHING$`

	updateIncidentStateSQLPattern = `^UPDATE triage\.reports SET state = \$2, severity = CASE WHEN \$3 = '' THEN severity ELSE \$3 END\s+` +
		`WHERE workflow_id = \$1\s+` +
		`AND CASE state\s+` +
		`WHEN 'processing'\s+THEN 1\s+` +
		`WHEN 'reported'\s+THEN 2\s+` +
		`WHEN 'acknowledged' THEN 3\s+` +
		`WHEN 'resolved'\s+THEN 4\s+` +
		`ELSE 0\s+` +
		`END < CASE \$2::text\s+` +
		`WHEN 'processing'\s+THEN 1\s+` +
		`WHEN 'reported'\s+THEN 2\s+` +
		`WHEN 'acknowledged' THEN 3\s+` +
		`WHEN 'resolved'\s+THEN 4\s+` +
		`ELSE 0\s+` +
		`END$`
)

func TestCreateIncident_NilDB(t *testing.T) {
	r := &ReportActivity{DB: nil}
	if err := r.CreateIncident(context.Background(), "wf-1", "ns", "wl", "Pod", "alert"); err != nil {
		t.Fatalf("want nil, got %v", err)
	}
}

func TestCreateIncident_Success(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectExec(createIncidentSQLPattern).
		WithArgs("wf-1", "ns", "wl", "Pod", "alert").
		WillReturnResult(sqlmock.NewResult(1, 1))

	r := &ReportActivity{DB: db}
	if err := r.CreateIncident(context.Background(), "wf-1", "ns", "wl", "Pod", "alert"); err != nil {
		t.Fatalf("CreateIncident: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestCreateIncident_DBError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectExec(createIncidentSQLPattern).
		WithArgs("wf-1", "ns", "wl", "Pod", "alert").
		WillReturnError(errors.New("boom"))

	r := &ReportActivity{DB: db}
	err = r.CreateIncident(context.Background(), "wf-1", "ns", "wl", "Pod", "alert")
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestUpdateIncidentState_NilDB(t *testing.T) {
	r := &ReportActivity{DB: nil}
	if err := r.UpdateIncidentState(context.Background(), "wf-1", "reported", ""); err != nil {
		t.Fatalf("want nil, got %v", err)
	}
}

func TestUpdateIncidentState_NormalizesInternalPhases(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	r := &ReportActivity{DB: db}
	for _, phase := range []string{"correlating", "enriching", "triaging"} {
		mock.ExpectExec(updateIncidentStateSQLPattern).
			WithArgs("wf-1", "processing", "").
			WillReturnResult(sqlmock.NewResult(0, 1))

		if err := r.UpdateIncidentState(context.Background(), "wf-1", phase, ""); err != nil {
			t.Fatalf("phase=%s: %v", phase, err)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestUpdateIncidentState_PassesThroughOperatorStates(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectExec(updateIncidentStateSQLPattern).
		WithArgs("wf-1", "acknowledged", "warning").
		WillReturnResult(sqlmock.NewResult(0, 1))

	r := &ReportActivity{DB: db}
	if err := r.UpdateIncidentState(context.Background(), "wf-1", "acknowledged", "warning"); err != nil {
		t.Fatalf("UpdateIncidentState: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestUpdateIncidentState_DBError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectExec(updateIncidentStateSQLPattern).
		WithArgs("wf-1", "reported", "").
		WillReturnError(errors.New("boom"))

	r := &ReportActivity{DB: db}
	err = r.UpdateIncidentState(context.Background(), "wf-1", "reported", "")
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}
