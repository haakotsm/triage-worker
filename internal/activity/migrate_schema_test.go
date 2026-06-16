package activity

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

const (
	advisoryLockSQL   = `SELECT pg_advisory_lock\(\$1\)`
	advisoryUnlockSQL = `SELECT pg_advisory_unlock\(\$1\)`
	ddlMarker         = `CREATE TABLE IF NOT EXISTS triage\.reports`
	dmlMarker         = `CREATE OR REPLACE FUNCTION triage\.notify_report_change`
)

func TestMigrateSchema_HappyPath(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(false))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectExec(advisoryLockSQL).
		WithArgs(migrationLockKey).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(ddlMarker).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(dmlMarker).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(advisoryUnlockSQL).
		WithArgs(migrationLockKey).
		WillReturnResult(sqlmock.NewResult(0, 0))

	if err := MigrateSchema(context.Background(), db); err != nil {
		t.Fatalf("MigrateSchema: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestMigrateSchema_LockAcquireFailure_NoUnlock(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(false))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectExec(advisoryLockSQL).
		WithArgs(migrationLockKey).
		WillReturnError(errors.New("lock acquire failed"))
	// Critical invariant: NO unlock expectation. If MigrateSchema attempted
	// pg_advisory_unlock without holding the lock, ExpectationsWereMet would
	// still pass (no expectations), but a stray unmatched Exec call would
	// surface as an sqlmock error. We rely on absence of a pre-registered
	// unlock expectation: any unlock attempt would fail with
	// "call to ExecQuery ... which was not expected".

	err = MigrateSchema(context.Background(), db)
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !strings.Contains(err.Error(), "acquire advisory lock") {
		t.Fatalf("want acquire-lock error, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestMigrateSchema_UnlocksOnDDLError(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(false))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectExec(advisoryLockSQL).
		WithArgs(migrationLockKey).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(ddlMarker).
		WillReturnError(errors.New("ddl boom"))
	mock.ExpectExec(advisoryUnlockSQL).
		WithArgs(migrationLockKey).
		WillReturnResult(sqlmock.NewResult(0, 0))

	err = MigrateSchema(context.Background(), db)
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !strings.Contains(err.Error(), "ddl") {
		t.Fatalf("want ddl error, got %v", err)
	}
	// ExpectationsWereMet enforces that the unlock did run after the DDL
	// failure — i.e. the defer path is wired correctly.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations (unlock defer leaked?): %v", err)
	}
}

func TestMigrateSchema_UnlocksOnDMLError(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(false))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectExec(advisoryLockSQL).
		WithArgs(migrationLockKey).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(ddlMarker).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(dmlMarker).
		WillReturnError(errors.New("dml boom"))
	mock.ExpectExec(advisoryUnlockSQL).
		WithArgs(migrationLockKey).
		WillReturnResult(sqlmock.NewResult(0, 0))

	err = MigrateSchema(context.Background(), db)
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !strings.Contains(err.Error(), "dml") {
		t.Fatalf("want dml error, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations (unlock defer leaked?): %v", err)
	}
}

func TestMigrateSchema_UnlockFailureLogged(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(false))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectExec(advisoryLockSQL).
		WithArgs(migrationLockKey).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(ddlMarker).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(dmlMarker).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(advisoryUnlockSQL).
		WithArgs(migrationLockKey).
		WillReturnError(errors.New("unlock boom"))

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	// Unlock failure must NOT propagate — migration succeeded.
	if err := MigrateSchema(context.Background(), db); err != nil {
		t.Fatalf("want nil (unlock-failure is logged, not returned), got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}

	logged := buf.String()
	if !strings.Contains(logged, "level=WARN") {
		t.Fatalf("want WARN log, got: %q", logged)
	}
	if !strings.Contains(logged, "pg_advisory_unlock failed") {
		t.Fatalf("want unlock-failure message, got: %q", logged)
	}
	if !strings.Contains(logged, "unlock boom") {
		t.Fatalf("want underlying error in log, got: %q", logged)
	}
}
