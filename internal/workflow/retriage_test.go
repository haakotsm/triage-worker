package workflow

import (
	"context"
	"log/slog"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/haakotsm/triage-worker/internal/types"
)

func TestRetriageStarter_AllocateAttemptID(t *testing.T) {
	id := types.IncidentIdentity{Namespace: "ns", Kind: "Deployment", Name: "web", AlertName: "OOMKilled"}

	t.Run("mints next attempt from stem", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatalf("sqlmock.New: %v", err)
		}
		defer db.Close()
		r := &RetriageStarter{db: db, logger: slog.Default()}

		want := id.WorkflowIDForAttempt(2) // from the unsuffixed (attempt-1) stem
		mock.ExpectExec("INSERT INTO triage.reports").
			WithArgs(want, "ns", "web", "Deployment", "OOMKilled").
			WillReturnResult(sqlmock.NewResult(0, 1))

		got, err := r.allocateAttemptID(context.Background(), id, id.WorkflowID())
		if err != nil {
			t.Fatalf("allocateAttemptID: %v", err)
		}
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet expectations: %v", err)
		}
	})

	t.Run("advances past a colliding attempt", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatalf("sqlmock.New: %v", err)
		}
		defer db.Close()
		r := &RetriageStarter{db: db, logger: slog.Default()}

		// Attempt 2 already exists (ON CONFLICT DO NOTHING → 0 rows), so it must
		// advance to attempt 3.
		mock.ExpectExec("INSERT INTO triage.reports").
			WithArgs(id.WorkflowIDForAttempt(2), "ns", "web", "Deployment", "OOMKilled").
			WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec("INSERT INTO triage.reports").
			WithArgs(id.WorkflowIDForAttempt(3), "ns", "web", "Deployment", "OOMKilled").
			WillReturnResult(sqlmock.NewResult(0, 1))

		got, err := r.allocateAttemptID(context.Background(), id, id.WorkflowID())
		if err != nil {
			t.Fatalf("allocateAttemptID: %v", err)
		}
		if want := id.WorkflowIDForAttempt(3); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet expectations: %v", err)
		}
	})

	t.Run("errors when every probed attempt collides", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatalf("sqlmock.New: %v", err)
		}
		defer db.Close()
		r := &RetriageStarter{db: db, logger: slog.Default()}

		// Every probe (attempt+1 .. attempt+maxAttemptProbe) collides → error,
		// no infinite loop.
		for i := 0; i < maxAttemptProbe; i++ {
			mock.ExpectExec("INSERT INTO triage.reports").
				WillReturnResult(sqlmock.NewResult(0, 0))
		}

		if _, err := r.allocateAttemptID(context.Background(), id, id.WorkflowID()); err == nil {
			t.Fatal("expected an error when all attempts collide, got nil")
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet expectations: %v", err)
		}
	})
}
