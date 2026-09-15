package account

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestApprovalAuthorizationOutboxPrivilegesIntegration(t *testing.T) {
	db := newApprovalAuthorizationIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	runtime := authorizationTestConnection(t, ctx, db.runtimePool)
	var session, owner string
	if err := runtime.QueryRow(ctx, `SELECT session_user,(SELECT pg_get_userbyid(relowner) FROM pg_class WHERE oid='public.outbox_events'::regclass)`).Scan(&session, &owner); err != nil {
		t.Fatal(err)
	}
	if session != "account_service" || owner != "account_approval_authorization_owner" {
		t.Fatalf("incorrect actual runtime/table owner: %s/%s", session, owner)
	}
	for _, sql := range []string{
		`ALTER TABLE public.outbox_events DISABLE TRIGGER USER`,
		`ALTER TABLE public.outbox_events ADD COLUMN injected TEXT`,
		`DELETE FROM public.outbox_events`,
		`TRUNCATE public.outbox_events`,
		`UPDATE public.outbox_events SET approval_control_epoch=1`,
		`SET ROLE account_approval_authorization_owner`,
		`SET ROLE account_migrator`,
	} {
		_, err := runtime.Exec(ctx, sql)
		if err == nil {
			t.Fatalf("runtime unexpectedly permitted %s", sql)
		}
		requireControlSQLState(t, err, "42501")
	}
	// The actual shared legacy append API must continue working for other
	// subjects after ownership transfer and column-level grants.
	aggregateID := uuid.New()
	tx, err := runtime.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if err := events.AppendOutbox(ctx, tx, "account.fixture.compatibility.v1", aggregateID, 1, map[string]any{"value": "unchanged"}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var eventID uuid.UUID
	var epoch *int64
	if err := runtime.QueryRow(ctx, `SELECT event_id,approval_control_epoch FROM public.outbox_events WHERE aggregate_id=$1`, aggregateID).Scan(&eventID, &epoch); err != nil || epoch != nil {
		t.Fatalf("non-authorization event changed: %v", err)
	}
	if _, err := runtime.Exec(ctx, `UPDATE public.outbox_events SET attempts=attempts+1,next_attempt_at=now(),locked_by='fixture',locked_until=now()+interval '1 minute',last_error='fixture' WHERE event_id=$1`, eventID); err != nil {
		t.Fatalf("legacy relay claim/failure columns lost privileges: %v", err)
	}
	if _, err := runtime.Exec(ctx, `UPDATE public.outbox_events SET published_at=now(),attempts=attempts+1,last_error=NULL,locked_by=NULL,locked_until=NULL WHERE event_id=$1`, eventID); err != nil {
		t.Fatalf("legacy relay acknowledgement columns lost privileges: %v", err)
	}
}

// This is a deliberately retained deployment debt, not a denied capability:
// the legacy service still owns the database and therefore its public schema.
// Demonstrate the destructive boundary only inside this fixture's rollback-only
// transaction; never treat ownership transfer alone as protection from DROP.
func TestApprovalAuthorizationLegacyDatabaseOwnerDebtIntegration(t *testing.T) {
	db := newApprovalAuthorizationIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	tx, err := db.runtimePool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, `DROP TABLE public.outbox_events CASCADE`); err != nil {
		t.Fatalf("retained database-owner boundary changed; review deployment debt: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var restored bool
	if err := db.adminPool.QueryRow(ctx, `SELECT to_regclass('public.outbox_events') IS NOT NULL`).Scan(&restored); err != nil || !restored {
		t.Fatalf("rollback did not preserve isolated fixture: %v", err)
	}
}

func TestApprovalAuthorizationLegacyMaintenanceIntegration(t *testing.T) {
	db := newApprovalAuthorizationIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	for _, relay := range []string{"FENCED", "RUNNING"} {
		tx := authorizationControlOwnerTransaction(t, ctx, db.migratorPool)
		if _, err := tx.Exec(ctx, `UPDATE account_approval_control.admin_approval_control SET authorization_relay_state=$1,authorization_relay_epoch=authorization_relay_epoch+1,version=version+1,updated_at=now() WHERE control_id='ADMIN_APPROVAL'`, relay); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	control := readControlTuple(t, ctx, db.adminPool)
	if control.Version != 3 || control.RelayEpoch != 3 || control.ApprovalEpoch != 1 || control.Relay != "RUNNING" {
		t.Fatalf("maintenance did not produce the required epoch fixture: %+v", control)
	}
	request := seedSubmittedApprovalForAuthorization(t, db, "WALLET_ADJUSTMENT")
	runtime := authorizationTestConnection(t, ctx, db.runtimePool)
	tx, err := runtime.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	request = authorizationPrepareLegacyApproval(t, ctx, tx, request)
	if err := authorizationAppendOldWriter(ctx, tx, request); err != nil {
		t.Fatalf("legacy writer rejected maintenance version/relay epoch: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var epoch int64
	if err := db.adminPool.QueryRow(ctx, `SELECT approval_control_epoch FROM public.outbox_events WHERE event_type='account.approval-command.authorized.v1' AND aggregate_id=$1`, request.ID).Scan(&epoch); err != nil || epoch != 1 {
		t.Fatalf("legacy missing epoch was not filled exactly as 1: %d %v", epoch, err)
	}
}

func TestApprovalAuthorizationLegacyWinsControlRaceIntegration(t *testing.T) {
	db := newApprovalAuthorizationIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	request := seedSubmittedApprovalForAuthorization(t, db, "WALLET_ADJUSTMENT")
	runtime := authorizationTestConnection(t, ctx, db.runtimePool)
	owner := authorizationTestConnection(t, ctx, db.migratorPool)
	legacyTx, err := runtime.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		t.Fatal(err)
	}
	defer legacyTx.Rollback(context.Background())
	approved := authorizationPrepareLegacyApproval(t, ctx, legacyTx, request)
	if err := authorizationAppendOldWriter(ctx, legacyTx, approved); err != nil {
		t.Fatal(err)
	}
	ownerTx, err := owner.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		t.Fatal(err)
	}
	defer ownerTx.Rollback(context.Background())
	if _, err := ownerTx.Exec(ctx, `SET LOCAL ROLE account_approval_control_owner`); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, err := LockAdminApprovalControlExclusive(ctx, ownerTx)
		result <- err
	}()
	defer func() { cancel(); <-done }()
	waitControlBlocked(t, ctx, db.adminPool, owner.Conn().PgConn().PID(), runtime.Conn().PgConn().PID())
	if err := legacyTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	_, transitionErr := ownerTx.Exec(ctx, `UPDATE account_approval_control.admin_approval_control SET mode='QUIESCING',approval_epoch=approval_epoch+1,version=version+1,updated_at=now() WHERE control_id='ADMIN_APPROVAL'`)
	if transitionErr == nil {
		transitionErr = ownerTx.Commit(ctx)
	}
	var pgError *pgconn.PgError
	if !errors.As(transitionErr, &pgError) || (pgError.Code != "40001" && pgError.Code != "23514") {
		t.Fatalf("QUIESCING committed despite a preceding legacy row lacking sidecar: %v", transitionErr)
	}
	_ = ownerTx.Rollback(ctx)
	// A SERIALIZABLE snapshot may abort the first attempt before it can see
	// the winning legacy insert. A fresh transaction must deterministically
	// reject that committed row's missing sidecar, not a stale control version.
	retryTx := authorizationControlOwnerTransaction(t, ctx, db.migratorPool)
	if _, err := LockAdminApprovalControlExclusive(ctx, retryTx); err != nil {
		t.Fatal(err)
	}
	_, retryErr := retryTx.Exec(ctx, `UPDATE account_approval_control.admin_approval_control SET mode='QUIESCING',approval_epoch=approval_epoch+1,version=version+1,updated_at=now() WHERE control_id='ADMIN_APPROVAL'`)
	if retryErr == nil {
		retryErr = retryTx.Commit(ctx)
	}
	requireControlSQLState(t, retryErr, "23514")
	_ = retryTx.Rollback(ctx)
	control := readControlTuple(t, ctx, db.adminPool)
	if control.Mode != "LEGACY" || control.Version != 1 || control.ApprovalEpoch != 1 {
		t.Fatalf("failed transition changed control: %+v", control)
	}
	var count int
	if err := db.adminPool.QueryRow(ctx, `SELECT count(*) FROM public.outbox_events WHERE event_type='account.approval-command.authorized.v1' AND aggregate_id=$1 AND approval_control_epoch=1`, request.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("legacy winner was not preserved: %d %v", count, err)
	}
}

func TestApprovalAuthorizationQuiescingWinsLegacyRaceIntegration(t *testing.T) {
	db := newApprovalAuthorizationIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	request := seedSubmittedApprovalForAuthorization(t, db, "WALLET_ADJUSTMENT")
	runtime := authorizationTestConnection(t, ctx, db.runtimePool)
	owner := authorizationTestConnection(t, ctx, db.migratorPool)
	ownerTx, err := owner.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		t.Fatal(err)
	}
	defer ownerTx.Rollback(context.Background())
	if _, err := ownerTx.Exec(ctx, `SET LOCAL ROLE account_approval_control_owner`); err != nil {
		t.Fatal(err)
	}
	if _, err := LockAdminApprovalControlExclusive(ctx, ownerTx); err != nil {
		t.Fatal(err)
	}
	legacyTx, err := runtime.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		t.Fatal(err)
	}
	defer legacyTx.Rollback(context.Background())
	approved := authorizationPrepareLegacyApproval(t, ctx, legacyTx, request)
	result := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		result <- authorizationAppendOldWriter(ctx, legacyTx, approved)
	}()
	// Stop the query before deferred transaction rollback/connection release,
	// including if the lock observation or owner commit fails the test early.
	defer func() { cancel(); <-done }()
	waitControlBlocked(t, ctx, db.adminPool, runtime.Conn().PgConn().PID(), owner.Conn().PgConn().PID())
	if _, err := ownerTx.Exec(ctx, `UPDATE account_approval_control.admin_approval_control SET mode='QUIESCING',approval_epoch=approval_epoch+1,version=version+1,updated_at=now() WHERE control_id='ADMIN_APPROVAL'`); err != nil {
		t.Fatal(err)
	}
	if err := ownerTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		var pgError *pgconn.PgError
		if !errors.As(err, &pgError) || (pgError.Code != "40001" && pgError.Code != "55000") {
			t.Fatalf("late legacy append did not fail closed after QUIESCING: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("legacy append did not finish after releasing the control lock")
	}
	if err := legacyTx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var status string
	var version, eventsCount int64
	if err := db.adminPool.QueryRow(ctx, `SELECT status,version,(SELECT count(*) FROM public.outbox_events WHERE event_type='account.approval-command.authorized.v1' AND aggregate_id=$1) FROM approval_requests WHERE id=$1`, request.ID).Scan(&status, &version, &eventsCount); err != nil {
		t.Fatal(err)
	}
	if status != "SUBMITTED" || version != request.Version || eventsCount != 0 {
		t.Fatalf("failed legacy transaction leaked effects: status=%s version=%d events=%d", status, version, eventsCount)
	}
}

func TestApprovalAuthorizationWriterRollbackIntegration(t *testing.T) {
	db := newApprovalAuthorizationIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	request := seedSubmittedApprovalForAuthorization(t, db, "WALLET_ADJUSTMENT")
	writer := authorizationTestConnection(t, ctx, db.writerPool)
	tx, err := writer.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	control, err := lockApprovalAuthorizationControl(ctx, tx)
	if err != nil {
		t.Fatal(err)
	}
	_ = authorizationPrepareLegacyApproval(t, ctx, tx, request)
	eventID := uuid.New()
	if _, err := appendLegacyApprovalAuthorization(ctx, tx, control, request.ID, eventID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var status string
	var version, outboxCount, sidecarCount int64
	if err := db.adminPool.QueryRow(ctx, `SELECT status,version,
        (SELECT count(*) FROM public.outbox_events WHERE event_id=$2),
        (SELECT count(*) FROM account_approval_authorization.approval_authorization_event_bytes WHERE event_id=$2)
        FROM public.approval_requests WHERE id=$1`, request.ID, eventID).Scan(&status, &version, &outboxCount, &sidecarCount); err != nil {
		t.Fatal(err)
	}
	if status != "SUBMITTED" || version != request.Version || outboxCount != 0 || sidecarCount != 0 {
		t.Fatalf("writer rollback leaked request/outbox/sidecar: %s/%d/%d/%d", status, version, outboxCount, sidecarCount)
	}
}

func authorizationTestConnection(t *testing.T, ctx context.Context, pool *pgxpool.Pool) *pgxpool.Conn {
	t.Helper()
	connection, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(connection.Release)
	return connection
}

func authorizationControlOwnerTransaction(t *testing.T, ctx context.Context, pool *pgxpool.Pool) pgx.Tx {
	t.Helper()
	connection := authorizationTestConnection(t, ctx, pool)
	tx, err := connection.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	if _, err := tx.Exec(ctx, `SET LOCAL ROLE account_approval_control_owner`); err != nil {
		t.Fatal(err)
	}
	return tx
}

func authorizationPrepareLegacyApproval(t *testing.T, ctx context.Context, tx pgx.Tx, request ApprovalRequest) ApprovalRequest {
	t.Helper()
	var deadline time.Time
	if err := tx.QueryRow(ctx, `UPDATE approval_requests SET status='APPROVED',version=version+1,execution_deadline=now()+interval '5 minutes',decided_at=now(),updated_at=now() WHERE id=$1 AND status='SUBMITTED' AND version=$2 RETURNING version,execution_deadline`, request.ID, request.Version).Scan(&request.Version, &deadline); err != nil {
		t.Fatal(err)
	}
	request.Status = "APPROVED"
	request.ExecutionDeadline = &deadline
	return request
}

func authorizationAppendOldWriter(ctx context.Context, tx pgx.Tx, request ApprovalRequest) error {
	policy, err := approvalPolicyForItem(request)
	if err != nil {
		return err
	}
	return events.AppendOutbox(ctx, tx, "account.approval-command.authorized.v1", request.ID, request.Version, map[string]any{
		"approval_request_id": request.ID, "command_type": request.CommandType,
		"command_fingerprint": request.CommandFingerprint, "policy_version": request.PolicyVersion,
		"executor_identity": policy.ExecutorIdentity, "execution_deadline": request.ExecutionDeadline,
		"normalized_command": request.NormalizedCommand, "target_resource_type": request.TargetResourceType,
		"target_resource_id": request.TargetResourceID, "critical_resource_versions": request.CriticalResourceVersions,
		"financial_snapshot": request.FinancialSnapshot,
	})
}
