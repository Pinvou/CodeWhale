package account

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// This controlled transaction exercises the production terminal-write fence.
// Real row locking, reclaim and PostgreSQL clock behavior need the PG fixture.
type auditLeaseTestTx struct {
	pgx.Tx
	row         auditLeaseTestRow
	databaseNow time.Time
	queryOrder  []string
	queryError  error
}
type auditLeaseTestRow struct {
	attempt int
	actor   uuid.UUID
	status  string
	until   *time.Time
	now     time.Time
	clock   bool
	err     error
}

func (row auditLeaseTestRow) Scan(dest ...any) error {
	if row.err != nil {
		return row.err
	}
	if row.clock {
		*dest[0].(*time.Time) = row.now
		return nil
	}
	*dest[0].(*int) = row.attempt
	*dest[1].(*uuid.UUID) = row.actor
	*dest[2].(*string) = row.status
	*dest[3].(**time.Time) = row.until
	return nil
}
func (tx *auditLeaseTestTx) QueryRow(_ context.Context, query string, _ ...any) pgx.Row {
	tx.queryOrder = append(tx.queryOrder, query)
	if query == "SELECT clock_timestamp()" {
		return auditLeaseTestRow{clock: true, now: tx.databaseNow, err: tx.queryError}
	}
	return tx.row
}
func TestAuditExportTerminalWriteRequiresCurrentLiveAttempt(t *testing.T) {
	now := time.Date(2026, 10, 7, 1, 0, 0, 0, time.UTC)
	future, expired := now.Add(time.Minute), now.Add(-time.Nanosecond)
	actor := uuid.New()
	job := claimedAuditExport{ID: uuid.New(), ActorUserID: actor, Attempt: 2}
	for _, test := range []struct {
		name    string
		row     auditLeaseTestRow
		allowed bool
	}{
		{"current-live-owner", auditLeaseTestRow{attempt: 2, actor: actor, status: "GENERATING", until: &future}, true},
		{"reclaimed-new-attempt", auditLeaseTestRow{attempt: 3, actor: actor, status: "GENERATING", until: &future}, false},
		{"ready-cannot-be-failed", auditLeaseTestRow{attempt: 2, actor: actor, status: "READY", until: &future}, false},
		{"lease-expired-after-lock", auditLeaseTestRow{attempt: 2, actor: actor, status: "GENERATING", until: &expired}, false},
		{"deadline-is-exclusive", auditLeaseTestRow{attempt: 2, actor: actor, status: "GENERATING", until: &now}, false},
		{"no-lease", auditLeaseTestRow{attempt: 2, actor: actor, status: "GENERATING"}, false},
		{"different-actor", auditLeaseTestRow{attempt: 2, actor: uuid.New(), status: "GENERATING", until: &future}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			tx := &auditLeaseTestTx{row: test.row, databaseNow: now}
			actual, err := requireAuditExportLease(context.Background(), tx, job)
			if test.allowed {
				if err != nil || !actual.Equal(now) {
					t.Fatalf("live lease rejected: %v", err)
				}
			} else if !errors.Is(err, ErrConflict) {
				t.Fatalf("invalid lease accepted: %v", err)
			}
			if len(tx.queryOrder) != 2 || tx.queryOrder[0] != "SELECT attempts,actor_user_id,status,lease_until FROM audit_export_jobs WHERE id=$1 FOR UPDATE" || tx.queryOrder[1] != "SELECT clock_timestamp()" {
				t.Fatal("database clock must be read after the row lock")
			}
		})
	}
}
func TestAuditExportTerminalWriteDoesNotHideAuthorityReadFailure(t *testing.T) {
	job := claimedAuditExport{ID: uuid.New(), ActorUserID: uuid.New(), Attempt: 1}
	cause := errors.New("controlled authority unavailable")
	tx := &auditLeaseTestTx{row: auditLeaseTestRow{err: cause}}
	if _, err := requireAuditExportLease(context.Background(), tx, job); !errors.Is(err, cause) || len(tx.queryOrder) != 1 {
		t.Fatal("row authority failure was hidden")
	}
	tx = &auditLeaseTestTx{row: auditLeaseTestRow{attempt: 1, actor: job.ActorUserID, status: "GENERATING"}, queryError: cause}
	if _, err := requireAuditExportLease(context.Background(), tx, job); !errors.Is(err, cause) {
		t.Fatal("database clock failure was hidden")
	}
	job.Attempt = 0
	tx = &auditLeaseTestTx{}
	if _, err := requireAuditExportLease(context.Background(), tx, job); !errors.Is(err, ErrConflict) || len(tx.queryOrder) != 0 {
		t.Fatal("unclaimed worker was allowed")
	}
}
