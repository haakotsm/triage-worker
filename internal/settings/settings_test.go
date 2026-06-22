package settings

import (
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestStore_DefaultEnabled(t *testing.T) {
	s := New(nil, testLogger())
	if !s.WorkflowsEnabled() {
		t.Error("a fresh store should default to workflows enabled")
	}
}

func TestStore_LoadNilDBIsNoop(t *testing.T) {
	s := New(nil, testLogger())
	if err := s.Load(t.Context()); err != nil {
		t.Fatalf("Load(nil db): %v", err)
	}
	if !s.WorkflowsEnabled() {
		t.Error("should remain enabled with no DB")
	}
}

func TestStore_LoadMissingRowKeepsDefault(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()
	s := New(db, testLogger())

	mock.ExpectQuery("SELECT value FROM triage.settings").
		WithArgs("workflows_enabled").
		WillReturnError(sql.ErrNoRows)

	if err := s.Load(t.Context()); err != nil {
		t.Fatalf("Load with no row should not error: %v", err)
	}
	if !s.WorkflowsEnabled() {
		t.Error("missing row should keep the default (enabled)")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestStore_LoadReadsFalse(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()
	s := New(db, testLogger())

	mock.ExpectQuery("SELECT value FROM triage.settings").
		WithArgs("workflows_enabled").
		WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow("false"))

	if err := s.Load(t.Context()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.WorkflowsEnabled() {
		t.Error("should be disabled after loading value 'false'")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestStore_SetPersistsAndCaches(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()
	s := New(db, testLogger())

	mock.ExpectExec("INSERT INTO triage.settings").
		WithArgs("workflows_enabled", "false").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := s.SetWorkflowsEnabled(t.Context(), false); err != nil {
		t.Fatalf("SetWorkflowsEnabled: %v", err)
	}
	if s.WorkflowsEnabled() {
		t.Error("cache should reflect the persisted disabled value")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// A failed persist must leave the in-memory cache at its previous value so the
// flag cannot diverge from the DB across a restart.
func TestStore_SetDBErrorLeavesCacheUnchanged(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()
	s := New(db, testLogger())

	mock.ExpectExec("INSERT INTO triage.settings").
		WithArgs("workflows_enabled", "false").
		WillReturnError(fmt.Errorf("db down"))

	if err := s.SetWorkflowsEnabled(t.Context(), false); err == nil {
		t.Fatal("expected an error when the persist fails")
	}
	if !s.WorkflowsEnabled() {
		t.Error("cache must stay at the previous value (enabled) when persist fails")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}
