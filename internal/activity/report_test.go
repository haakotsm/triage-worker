package activity

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
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

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO triage.reports")).
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

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO triage.reports")).
		WillReturnError(errors.New("boom"))

	r := &ReportActivity{DB: db}
	err = r.CreateIncident(context.Background(), "wf-1", "ns", "wl", "Pod", "alert")
	if err == nil {
		t.Fatal("want error, got nil")
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

	for _, phase := range []string{"correlating", "enriching", "triaging"} {
		mock.ExpectExec(regexp.QuoteMeta("UPDATE triage.reports")).
			WithArgs("wf-1", "processing", "").
			WillReturnResult(sqlmock.NewResult(0, 1))

		r := &ReportActivity{DB: db}
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

	mock.ExpectExec(regexp.QuoteMeta("UPDATE triage.reports")).
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

	mock.ExpectExec(regexp.QuoteMeta("UPDATE triage.reports")).
		WillReturnError(errors.New("boom"))

	r := &ReportActivity{DB: db}
	err = r.UpdateIncidentState(context.Background(), "wf-1", "reported", "")
	if err == nil {
		t.Fatal("want error, got nil")
	}
}
