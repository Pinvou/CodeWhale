package token

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// Uses the installed public Token migrations and genuine token_gateway role.
// Usage/credential facts are controlled fixture inputs; this does not dispatch
// a supplier request or prove the downstream Billing release lifecycle.
func TestRequestCancellationPublicPostgresTimestamp(t *testing.T) {
	repository, ctx := retainedManagementTokenRepository(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	repository.now = func() time.Time { return now }
	for _, sample := range []struct {
		name, execution, expected string
		preDispatch               bool
	}{
		{"pre-dispatch", "RECEIVED", "CANCEL_REQUESTED", true},
		{"in-flight", "STREAMING", "UNSUPPORTED", false},
	} {
		t.Run(sample.name, func(t *testing.T) {
			user := uuid.New()
			id := retainedCancellationUsage(t, repository, ctx, user, uuid.New(), nil, sample.execution)
			if _, err := repository.pool.Exec(ctx, `UPDATE usage_requests SET cancel_status='NOT_REQUESTED',cancel_requested_at=NULL WHERE id=$1`, id); err != nil {
				t.Fatal("controlled initial cancellation state failed")
			}
			key := strings.Repeat("d", 64)
			fingerprint := cancellationFingerprint(id)
			result, preDispatch, err := repository.RequestCancellation(ctx, id, user, key, fingerprint, "controlled-adapter-v1")
			if err != nil {
				var postgresError *pgconn.PgError
				if errors.As(err, &postgresError) {
					t.Fatalf("production cancellation failed SQLSTATE=%s", postgresError.Code)
				}
				t.Fatal("production cancellation failed")
			}
			if result.CancelStatus != sample.expected || preDispatch != sample.preDispatch {
				t.Fatal("cancellation status or pre-dispatch decision changed")
			}
			var requested, stopped, deadline, updated time.Time
			var seconds, commands, evidence int
			var origin, delivery string
			if err := repository.pool.QueryRow(ctx, `SELECT cancel_requested_at,user_stopped_at,cancel_reconciliation_deadline,updated_at,cancel_reconciliation_seconds,cancellation_origin,delivery_status,(SELECT count(*) FROM usage_cancel_commands WHERE usage_request_id=$1),(SELECT count(*) FROM usage_cancellation_evidence WHERE usage_request_id=$1) FROM usage_requests WHERE id=$1`, id).Scan(&requested, &stopped, &deadline, &updated, &seconds, &origin, &delivery, &commands, &evidence); err != nil {
				t.Fatal("committed cancellation facts unavailable")
			}
			if !requested.Equal(now) || !stopped.Equal(now) || !updated.Equal(now) || !deadline.Equal(now.Add(time.Duration(seconds)*time.Second)) || origin != "USER" || delivery != "USER_STOPPED" || commands != 1 || evidence != 1 {
				t.Fatal("cancellation timestamp, deadline, origin or durable evidence changed")
			}
			replay, replayPreDispatch, err := repository.RequestCancellation(ctx, id, user, key, fingerprint, "controlled-adapter-v1")
			if err != nil || replay.CancelStatus != result.CancelStatus || replayPreDispatch != sample.preDispatch {
				t.Fatal("same-key cancellation did not replay original state")
			}
			if _, _, err := repository.RequestCancellation(ctx, id, user, key, strings.Repeat("e", 64), "controlled-adapter-v1"); !errors.Is(err, ErrIdempotencyConflict) {
				t.Fatal("same usage with foreign fingerprint accepted")
			}
			if err := repository.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM usage_cancel_commands WHERE usage_request_id=$1),(SELECT count(*) FROM usage_cancellation_evidence WHERE usage_request_id=$1)`, id).Scan(&commands, &evidence); err != nil || commands != 1 || evidence != 1 {
				t.Fatal("replay mutated durable cancellation command or evidence")
			}
		})
	}
	t.Run("foreign-owner-rejected-without-command", func(t *testing.T) {
		user := uuid.New()
		id := retainedCancellationUsage(t, repository, ctx, user, uuid.New(), nil, "RECEIVED")
		if _, _, err := repository.RequestCancellation(ctx, id, uuid.New(), strings.Repeat("d", 64), cancellationFingerprint(id), "controlled-adapter-v1"); !errors.Is(err, ErrNotFound) {
			t.Fatal("foreign owner cancellation accepted")
		}
		var commands int
		if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM usage_cancel_commands WHERE usage_request_id=$1`, id).Scan(&commands); err != nil || commands != 0 {
			t.Fatal("foreign owner cancellation wrote a command")
		}
	})
}
