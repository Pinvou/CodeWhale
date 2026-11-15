package account

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func registrationOutboxCandidate(t *testing.T, ctx context.Context, r *PostgresRepository, mobile string) RegistrationOperation {
	t.Helper()
	original := r.now
	now := original()
	r.now = func() time.Time { return now.Add(-25 * time.Hour) }
	defer func() { r.now = original }()
	id := inspectionRegistrationReady(t, r, "idxcandidate"+uuid.NewString()[:8], mobile)
	if err := r.MarkRegistrationIdentityCreated(ctx, id, "controlled-index-identity-"+uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	r.now = original
	if err := r.ScheduleRegistrationRetry(ctx, id, "ACCOUNT_COMMIT_FAILED"); err != nil {
		t.Fatal(err)
	}
	operation, err := r.RegistrationOperation(ctx, id)
	if err != nil || operation.Status != "REPAIR_REQUIRED" {
		t.Fatal("actual compensable registration producer", operation.Status, err)
	}
	return operation
}

// A source-table lock fixes the exact interleaving without production hooks:
// compensationProof has acquired its real operation row lock and is waiting
// in the outbox predicate before a second transaction attempts that same row.
func registrationOutboxLockSample(t *testing.T, parent context.Context, r *PostgresRepository, operation RegistrationOperation, blocked bool, phase string, repeat int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	configuration := r.pool.Config().ConnConfig.Copy()
	configuration.User = "account_migrator"
	configuration.Password = ""
	owner, err := pgx.ConnectConfig(ctx, configuration)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close(context.Background())
	if _, err := owner.Exec(ctx, `SET ROLE account_approval_authorization_owner`); err != nil {
		t.Fatal(err)
	}
	source, err := owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Rollback(context.Background())
	if _, err := source.Exec(ctx, `LOCK TABLE public.outbox_events IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	holder, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		t.Fatal(err)
	}
	holderStarted := false
	defer func() {
		if !holderStarted {
			_ = holder.Rollback(context.Background())
		}
	}()
	waiter, err := r.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	waiterStarted := false
	defer func() {
		if !waiterStarted {
			_ = waiter.Rollback(context.Background())
		}
	}()
	var holderPID, waiterPID int
	if err := holder.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&holderPID); err != nil {
		t.Fatal(err)
	}
	if err := waiter.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&waiterPID); err != nil {
		t.Fatal(err)
	}
	type sample struct {
		err       error
		completed int64
		elapsed   time.Duration
	}
	holderDone := make(chan sample, 1)
	waiterDone := make(chan sample, 1)
	defer func() {
		cancel()
		_ = source.Rollback(context.Background())
		if holderStarted && holderDone != nil {
			<-holderDone
		}
		if waiterStarted && waiterDone != nil {
			<-waiterDone
		}
	}()
	holderStarted = true
	go func() {
		start := time.Now()
		_, err := compensationProof(ctx, holder, operation.ID, r.now())
		elapsed := time.Since(start)
		rollbackErr := holder.Rollback(ctx)
		if rollbackErr != nil && err == nil {
			err = rollbackErr
		}
		holderDone <- sample{err, time.Now().UnixNano(), elapsed}
	}()
	wallClockWait(t, ctx, r.pool, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND wait_event_type='Lock' AND query LIKE '%outbox_events%')`, holderPID)
	waiterStarted = true
	go func() {
		start := time.Now()
		var id uuid.UUID
		err := waiter.QueryRow(ctx, `SELECT id FROM registration_operations WHERE id=$1 FOR UPDATE`, operation.ID).Scan(&id)
		rollbackErr := waiter.Rollback(ctx)
		if rollbackErr != nil && err == nil {
			err = rollbackErr
		}
		waiterDone <- sample{err, time.Now().UnixNano(), time.Since(start)}
	}()
	wallClockWait(t, ctx, r.pool, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND wait_event_type='Lock' AND query LIKE '%registration_operations%')`, waiterPID)
	released := time.Now().UnixNano()
	if err := source.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	holdResult, waitResult := <-holderDone, <-waiterDone
	holderDone, waiterDone = nil, nil
	if (blocked && !errors.Is(holdResult.err, ErrRegistrationCompensationForbidden)) || (!blocked && holdResult.err != nil) || waitResult.err != nil {
		t.Fatal("actual compensation proof/waiter outcome", blocked, holdResult.err, waitResult.err)
	}
	t.Logf("OUTBOX_COMPENSATION_LOCK phase=%s repeat=%d historical_reference=%t holder_total_us=%d holder_after_source_release_us=%d waiter_total_us=%d waiter_after_source_release_us=%d source_block_observed=true row_wait_observed=true", phase, repeat, blocked, holdResult.elapsed.Microseconds(), (holdResult.completed-released)/1000, waitResult.elapsed.Microseconds(), (waitResult.completed-released)/1000)
}

func registrationOutboxMeasurePhase(t *testing.T, ctx context.Context, r *PostgresRepository, phase string, registration, approval uuid.UUID, absent, referenced RegistrationOperation) {
	t.Helper()
	for repeat := 1; repeat <= 3; repeat++ {
		for _, target := range []struct {
			name string
			id   uuid.UUID
		}{{"registration", registration}, {"approval", approval}, {"absent", absent.CandidateUserID}, {"late-historical-reference", referenced.CandidateUserID}} {
			t.Logf("OUTBOX_AGGREGATE_TARGET phase=%s repeat=%d target=%s", phase, repeat, target.name)
			registrationOutboxPlan(t, ctx, r, target.id, phase)
		}
		registrationOutboxLockSample(t, ctx, r, absent, false, phase, repeat)
		registrationOutboxLockSample(t, ctx, r, referenced, true, phase, repeat)
	}
}
