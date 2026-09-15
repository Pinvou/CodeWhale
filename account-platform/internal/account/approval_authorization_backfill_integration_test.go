package account

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestApprovalAuthorizationBackfillLegacyAndFrozenIntegration(t *testing.T) {
	db := newApprovalAuthorizationIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	for _, frozen := range []bool{false, true} {
		for _, deliveryState := range []string{"unpublished", "published", "dlq"} {
			published := deliveryState == "published"
			name := "legacy"
			if frozen {
				name = "frozen"
			}
			if published {
				name += "_published"
			}
			if deliveryState == "dlq" {
				name += "_dlq"
			}
			t.Run(name, func(t *testing.T) {
				eventID := seedBackfillAuthorization(t, ctx, db, frozen)
				if published {
					if _, err := db.runtimePool.Exec(ctx, `UPDATE public.outbox_events SET published_at=now() WHERE event_id=$1`, eventID); err != nil {
						t.Fatal(err)
					}
				}
				if deliveryState == "dlq" {
					if _, err := db.runtimePool.Exec(ctx, `UPDATE public.outbox_events SET dead_lettered_at=now() WHERE event_id=$1`, eventID); err != nil {
						t.Fatal(err)
					}
				}
				var before []byte
				if err := db.adminPool.QueryRow(ctx, `SELECT to_jsonb(o)::text FROM public.outbox_events o WHERE event_id=$1`, eventID).Scan(&before); err != nil {
					t.Fatal(err)
				}
				for attempt := 0; attempt < 2; attempt++ {
					readiness, err := BackfillApprovalAuthorizationEvents(ctx, db.backfillPool, 1)
					if err != nil || readiness.MissingEpochCount != 0 || readiness.MissingSidecarCount != 0 || readiness.InvalidSidecarCount != 0 {
						t.Fatalf("backfill %d: %+v %v", attempt, readiness, err)
					}
				}
				var after []byte
				var provenance string
				var delivery, digest, semantic bool
				if err := db.adminPool.QueryRow(ctx, `SELECT to_jsonb(o)::text,s.provenance,s.delivery_bytes IS NOT NULL,s.event_digest IS NOT NULL,s.reconstructed_semantic_digest IS NOT NULL FROM public.outbox_events o JOIN account_approval_authorization.approval_authorization_event_bytes s USING(event_id) WHERE event_id=$1`, eventID).Scan(&after, &provenance, &delivery, &digest, &semantic); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(before, after) {
					t.Fatal("backfill changed original Outbox facts")
				}
				expected := "RECONSTRUCTED_FOR_DELIVERY"
				if published {
					expected = "RECONSTRUCTED_AFTER_PUBLISH"
				}
				if frozen {
					expected = "FROZEN_AT_APPROVAL"
				}
				semanticOnly := published && !frozen
				if provenance != expected || delivery == semanticOnly || digest == semanticOnly || semantic != semanticOnly {
					t.Fatalf("wrong immutable provenance: %s delivery=%v digest=%v semantic=%v", provenance, delivery, digest, semantic)
				}
			})
		}
	}
	validationTx, err := db.migratorPool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		t.Fatal(err)
	}
	defer validationTx.Rollback(context.Background())
	if _, err := ValidateApprovalAuthorizationForQuiescing(ctx, validationTx, 1, 2); err != nil {
		t.Fatal(err)
	}
	if err := validationTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestApprovalAuthorizationBackfillHighWaterIsBoundedIntegration(t *testing.T) {
	db := newApprovalAuthorizationIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	first := seedBackfillAuthorization(t, ctx, db, false)
	configuration := db.backfillPool.Config().Copy()
	configuration.MaxConns, configuration.MinConns = 1, 0
	pool, err := pgxpool.NewWithConfig(ctx, configuration)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var backfillPID uint32
	if err := pool.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&backfillPID); err != nil {
		t.Fatal(err)
	}
	locker := authorizationTestConnection(t, ctx, db.adminPool)
	lockTx, err := locker.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lockTx.Rollback(context.Background())
	if _, err := lockTx.Exec(ctx, `SELECT event_id FROM public.outbox_events WHERE event_id=$1 FOR UPDATE`, first); err != nil {
		t.Fatal(err)
	}
	type result struct {
		readiness ApprovalAuthorizationBackfillReadiness
		err       error
	}
	results := make(chan result, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		readiness, err := BackfillApprovalAuthorizationEvents(ctx, pool, 1)
		results <- result{readiness, err}
	}()
	defer func() { cancel(); <-done }()
	// The first row is reached only after the initial high-water was captured.
	waitControlBlocked(t, ctx, db.adminPool, backfillPID, locker.Conn().PgConn().PID())
	second := seedBackfillAuthorization(t, ctx, db, false)
	if err := lockTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	observed := <-results
	if observed.err != nil || observed.readiness.AuthorizationCount != 2 || observed.readiness.MissingSidecarCount != 1 {
		t.Fatalf("first run chased new rows or hid the gap: %+v %v", observed.readiness, observed.err)
	}
	var firstCount, secondCount int
	if err := db.adminPool.QueryRow(ctx, `SELECT (SELECT count(*) FROM account_approval_authorization.approval_authorization_event_bytes WHERE event_id=$1),(SELECT count(*) FROM account_approval_authorization.approval_authorization_event_bytes WHERE event_id=$2)`, first, second).Scan(&firstCount, &secondCount); err != nil {
		t.Fatal(err)
	}
	if firstCount != 1 || secondCount != 0 {
		t.Fatal("high-water boundary changed")
	}
	readiness, err := BackfillApprovalAuthorizationEvents(ctx, pool, 1)
	if err != nil || readiness.AuthorizationCount != 2 || readiness.MissingSidecarCount != 0 || readiness.InvalidSidecarCount != 0 {
		t.Fatalf("next bounded run did not converge: %+v %v", readiness, err)
	}
}

func TestApprovalAuthorizationFinalValidationDoesNotBackfillIntegration(t *testing.T) {
	db := newApprovalAuthorizationIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	eventID := seedBackfillAuthorization(t, ctx, db, false)
	var before []byte
	if err := db.adminPool.QueryRow(ctx, `SELECT to_jsonb(o)::text FROM public.outbox_events o WHERE event_id=$1`, eventID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	validationTx, err := db.migratorPool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		t.Fatal(err)
	}
	defer validationTx.Rollback(context.Background())
	if _, err := ValidateApprovalAuthorizationForQuiescing(ctx, validationTx, 1, 10); err == nil {
		t.Fatal("missing sidecar unexpectedly validated")
	}
	if err := validationTx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var after []byte
	var count int
	if err := db.adminPool.QueryRow(ctx, `SELECT to_jsonb(o)::text,(SELECT count(*) FROM account_approval_authorization.approval_authorization_event_bytes WHERE event_id=$1) FROM public.outbox_events o WHERE event_id=$1`, eventID).Scan(&after, &count); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || count != 0 {
		t.Fatal("read-only validation secretly repaired history")
	}
}

func TestApprovalAuthorizationInvalidHistoryBlocksWithoutMutationIntegration(t *testing.T) {
	db := newApprovalAuthorizationIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	request := seedSubmittedApprovalForAuthorization(t, db, "WALLET_ADJUSTMENT")
	tx, err := db.runtimePool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	approved := authorizationPrepareLegacyApproval(t, ctx, tx, request)
	// The compatibility trigger does not rewrite a historic, unclosed payload.
	approved.NormalizedCommand = json.RawMessage(`{"unknown":"private-payload"}`)
	deadline := approved.ExecutionDeadline.UTC()
	approved.ExecutionDeadline = &deadline
	if err := authorizationAppendOldWriter(ctx, tx, approved); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var eventID uuid.UUID
	var before []byte
	if err := db.adminPool.QueryRow(ctx, `SELECT event_id,to_jsonb(o)::text FROM public.outbox_events o WHERE aggregate_id=$1`, request.ID).Scan(&eventID, &before); err != nil {
		t.Fatal(err)
	}
	_, err = BackfillApprovalAuthorizationEvents(ctx, db.backfillPool, 1)
	report, ok := ApprovalAuthorizationBlockerReportFrom(err)
	if !errors.Is(err, ErrApprovalBackfillUnavailable) || !ok || report.EventID != eventID {
		t.Fatalf("missing closed row diagnostic: %v", err)
	}
	if report.Reason != "LEGACY_CLOSED_DECODE_FAILED" || report.SchemaVersion != 1 || report.SerializationVersion != "LEGACY_TYPED_V1" || len(report.PayloadDigestPrefix) != 12 {
		t.Fatalf("unexpected closed history diagnostic: %+v", report)
	}
	var after []byte
	var count int
	if err := db.adminPool.QueryRow(ctx, `SELECT to_jsonb(o)::text,(SELECT count(*) FROM account_approval_authorization.approval_authorization_event_bytes WHERE event_id=$1) FROM public.outbox_events o WHERE event_id=$1`, eventID).Scan(&after, &count); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || count != 0 {
		t.Fatal("invalid history was modified")
	}
}

func seedBackfillAuthorization(t *testing.T, ctx context.Context, db *approvalAuthorizationIntegrationDatabase, frozen bool) uuid.UUID {
	t.Helper()
	request := seedSubmittedApprovalForAuthorization(t, db, "WALLET_ADJUSTMENT")
	pool := db.runtimePool
	if frozen {
		pool = db.writerPool
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	control, err := lockApprovalAuthorizationControl(ctx, tx)
	if err != nil {
		t.Fatal(err)
	}
	approved := authorizationPrepareLegacyApproval(t, ctx, tx, request)
	if frozen {
		if _, err := appendLegacyApprovalAuthorization(ctx, tx, control, request.ID, uuid.New()); err != nil {
			t.Fatal(err)
		}
	} else {
		// Reproduce legacy UTC service serialization, independent of test-host TZ.
		deadline := approved.ExecutionDeadline.UTC()
		approved.ExecutionDeadline = &deadline
		if err := authorizationAppendOldWriter(ctx, tx, approved); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var eventID uuid.UUID
	if err := db.adminPool.QueryRow(ctx, `SELECT event_id FROM public.outbox_events WHERE aggregate_id=$1`, request.ID).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	return eventID
}

func TestApprovalAuthorizationFinalValidationRetainsCallerLockIntegration(t *testing.T) {
	db := newApprovalAuthorizationIntegrationDatabase(t)
	{
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		tx, err := db.migratorPool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		_, err = tx.Exec(ctx, `SELECT * FROM account_approval_authorization.validate_authorization_outbox_for_quiescing(1)`)
		_ = tx.Rollback(ctx)
		cancel()
		requireControlSQLState(t, err, "25001")
	}
	for _, pool := range []*pgxpool.Pool{db.runtimePool, db.writerPool, db.backfillPool} {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, err := pool.Exec(ctx, `SELECT * FROM account_approval_authorization.authorization_validation_candidates(NULL,NULL,1)`)
		cancel()
		requireControlSQLState(t, err, "42501")
	}
	for _, commit := range []bool{false, true} {
		t.Run(map[bool]string{false: "rollback", true: "commit"}[commit], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			owner := authorizationTestConnection(t, ctx, db.migratorPool)
			writer := authorizationTestConnection(t, ctx, db.writerPool)
			tx, err := owner.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.Background())
			if _, err := ValidateApprovalAuthorizationForQuiescing(ctx, tx, 1, 1); err != nil {
				t.Fatal(err)
			}
			writerTx, err := writer.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
			if err != nil {
				t.Fatal(err)
			}
			defer writerTx.Rollback(context.Background())
			result := make(chan error, 1)
			done := make(chan struct{})
			go func() { defer close(done); _, err := lockApprovalAuthorizationControl(ctx, writerTx); result <- err }()
			defer func() { cancel(); <-done }()
			waitControlBlocked(t, ctx, db.adminPool, writer.Conn().PgConn().PID(), owner.Conn().PgConn().PID())
			if commit {
				err = tx.Commit(ctx)
			} else {
				err = tx.Rollback(ctx)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := <-result; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestApprovalAuthorizationPreMigrationHistoryIntegration(t *testing.T) {
	db := newApprovalAuthorizationIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	connection := authorizationTestConnection(t, ctx, db.migratorPool)
	loaded, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(ctx, `SET ROLE account_service`); err != nil {
		t.Fatal(err)
	}
	defer migrations.Unlock(ctx, connection.Conn())
	if err := migrations.DownOne(ctx, connection.Conn(), loaded); err != nil {
		t.Fatal(err)
	}
	before := make(map[uuid.UUID][]byte)
	for _, published := range []bool{false, true} {
		eventID := seedBackfillAuthorization(t, ctx, db, false)
		if published {
			if _, err := db.runtimePool.Exec(ctx, `UPDATE public.outbox_events SET published_at=now() WHERE event_id=$1`, eventID); err != nil {
				t.Fatal(err)
			}
		}
		var raw []byte
		if err := db.adminPool.QueryRow(ctx, `SELECT to_jsonb(o)::text FROM public.outbox_events o WHERE event_id=$1`, eventID).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		before[eventID] = raw
	}
	if err := migrations.Up(ctx, connection.Conn(), loaded); err != nil {
		t.Fatal(err)
	}
	var missing int
	if err := db.adminPool.QueryRow(ctx, `SELECT count(*) FROM public.outbox_events WHERE approval_control_epoch IS NULL`).Scan(&missing); err != nil || missing != 2 {
		t.Fatalf("migration did not preserve historical null epoch: %d %v", missing, err)
	}
	if _, err := BackfillApprovalAuthorizationEvents(ctx, db.backfillPool, 1); err != nil {
		t.Fatal(err)
	}
	for eventID, original := range before {
		var raw []byte
		var epoch int64
		if err := db.adminPool.QueryRow(ctx, `SELECT (to_jsonb(o)-'approval_control_epoch')::text,approval_control_epoch FROM public.outbox_events o WHERE event_id=$1`, eventID).Scan(&raw, &epoch); err != nil {
			t.Fatal(err)
		}
		if epoch != 1 || !bytes.Equal(original, raw) {
			t.Fatal("historical backfill changed facts other than the nullable epoch")
		}
	}
}
