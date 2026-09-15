package account

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func claimLegacyAuthorizationRow(ctx context.Context, tx pgx.Tx, eventID any, worker string) error {
	_, err := tx.Exec(ctx, `UPDATE public.outbox_events
SET locked_by=$2,locked_until=clock_timestamp()+interval '30 seconds'
WHERE event_id=$1 AND published_at IS NULL AND dead_lettered_at IS NULL
  AND (locked_until IS NULL OR locked_until<clock_timestamp())`, eventID, worker)
	return err
}

func waitApprovalRelayBlockedBy(t *testing.T, ctx context.Context, pool *pgxpool.Pool, blocker uint32) uint32 {
	t.Helper()
	waitContext, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var pid int32
		err := pool.QueryRow(waitContext, `SELECT a.pid FROM pg_catalog.pg_stat_activity a
			WHERE a.usename='account_approval_relay' AND $1::integer=ANY(pg_catalog.pg_blocking_pids(a.pid))
			ORDER BY a.pid LIMIT 1`, int32(blocker)).Scan(&pid)
		if err == nil {
			return uint32(pid)
		}
		if err != pgx.ErrNoRows {
			t.Fatal(err)
		}
		select {
		case <-waitContext.Done():
			t.Fatal("expected authorization relay database lock wait was not observed")
		case <-ticker.C:
		}
	}
}

func waitBackendHasBlocker(t *testing.T, ctx context.Context, pool *pgxpool.Pool, waiter uint32, description string) {
	t.Helper()
	waitContext, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked bool
		if err := pool.QueryRow(waitContext, `SELECT cardinality(pg_catalog.pg_blocking_pids($1::integer))>0`, int32(waiter)).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			return
		}
		select {
		case <-waitContext.Done():
			t.Fatal(description)
		case <-ticker.C:
		}
	}
}

func TestApprovalRelayDedicatedClaimAndFenceLockOrderIntegration(t *testing.T) {
	for _, order := range []string{"claim-first", "fence-first"} {
		t.Run(order, func(t *testing.T) {
			db, store := newApprovalRelayIntegrationDatabase(t)
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			eventID := seedBackfillAuthorization(t, ctx, db, true)
			owner := newControlActor(t, ctx, db.adminPool, "account_approval_control_owner")
			fenceSQL := `UPDATE account_approval_control.admin_approval_control
				SET authorization_relay_state='FENCED',authorization_relay_epoch=authorization_relay_epoch+1,version=version+1
				WHERE control_id='ADMIN_APPROVAL'`

			if order == "fence-first" {
				ownerTx, err := owner.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
				if err != nil {
					t.Fatal(err)
				}
				defer ownerTx.Rollback(context.Background())
				if _, err := ownerTx.Exec(ctx, fenceSQL); err != nil {
					t.Fatal(err)
				}
				claimResult := make(chan error, 1)
				go func() {
					_, err := store.ClaimFrozen(ctx, "late-dedicated-worker", 1, 30*time.Second)
					claimResult <- err
				}()
				waitApprovalRelayBlockedBy(t, ctx, db.adminPool, owner.PgConn().PID())
				if err := ownerTx.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				if err := <-claimResult; err != ErrApprovalRelayUnavailable {
					t.Fatalf("late claim did not serialize behind fence: %v", err)
				}
				claims, err := store.ClaimFrozen(ctx, "late-dedicated-worker", 1, 30*time.Second)
				if err != nil || len(claims) != 0 {
					t.Fatalf("fenced retry claimed work: %+v %v", claims, err)
				}
				var lockedBy *string
				if err := db.adminPool.QueryRow(ctx, `SELECT locked_by FROM public.outbox_events WHERE event_id=$1`, eventID).Scan(&lockedBy); err != nil || lockedBy != nil {
					t.Fatal("fence-first claim changed the row")
				}
				return
			}

			claimPrepared := make(chan struct{})
			releaseClaim := make(chan struct{})
			releasedClaim := false
			defer func() {
				if !releasedClaim {
					close(releaseClaim)
				}
			}()
			store.commit = func(commitContext context.Context, tx pgx.Tx) error {
				close(claimPrepared)
				select {
				case <-releaseClaim:
					return tx.Commit(commitContext)
				case <-commitContext.Done():
					return commitContext.Err()
				}
			}
			claimResult := make(chan struct {
				claims []events.FrozenClaim
				err    error
			}, 1)
			go func() {
				claims, err := store.ClaimFrozen(ctx, "dedicated-first-worker", 1, 30*time.Second)
				claimResult <- struct {
					claims []events.FrozenClaim
					err    error
				}{claims, err}
			}()
			select {
			case <-claimPrepared:
			case early := <-claimResult:
				t.Fatalf("dedicated claim failed before commit boundary: %+v %v", early.claims, early.err)
			case <-ctx.Done():
				t.Fatal("dedicated claim did not reach its commit boundary")
			}
			fenceResult := make(chan error, 1)
			go func() {
				tx, err := owner.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
				if err == nil {
					_, err = tx.Exec(ctx, fenceSQL)
				}
				if err == nil {
					err = tx.Commit(ctx)
				} else if tx != nil {
					_ = tx.Rollback(context.Background())
				}
				fenceResult <- err
			}()
			waitBackendHasBlocker(t, ctx, db.adminPool, owner.PgConn().PID(), "fence did not wait for the dedicated claim transaction")
			close(releaseClaim)
			releasedClaim = true
			claimed := <-claimResult
			if claimed.err != nil || len(claimed.claims) != 1 || claimed.claims[0].EventID != eventID {
				t.Fatalf("dedicated claim failed: %+v %v", claimed.claims, claimed.err)
			}
			requireControlSQLState(t, <-fenceResult, "40001")
			retryOwner := newControlActor(t, ctx, db.adminPool, "account_approval_control_owner")
			retryTx, err := retryOwner.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := retryTx.Exec(ctx, fenceSQL); err != nil {
				t.Fatal(err)
			}
			requireControlSQLState(t, retryTx.Commit(ctx), "23514")
		})
	}
}

func TestApprovalRelayPublishHoldsFenceLockAndFencePreventsBrokerCallIntegration(t *testing.T) {
	t.Run("publish-first", func(t *testing.T) {
		db, store := newApprovalRelayIntegrationDatabase(t)
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		eventID := seedBackfillAuthorization(t, ctx, db, true)
		claims, err := store.ClaimFrozen(ctx, "publishing-worker", 1, 30*time.Second)
		if err != nil || len(claims) != 1 {
			t.Fatal("claim", err)
		}
		entered := make(chan struct{})
		release := make(chan struct{})
		released := false
		defer func() {
			if !released {
				close(release)
			}
		}()
		processResult := make(chan error, 1)
		go func() {
			_, err := store.ProcessFrozen(ctx, "publishing-worker", claims[0], 12, time.Now().UTC(), func(publishContext context.Context, _ events.FrozenMessage) events.FrozenPublishResult {
				close(entered)
				select {
				case <-release:
					return events.FrozenPublishResult{Disposition: events.FrozenPublished}
				case <-publishContext.Done():
					return events.FrozenPublishResult{Disposition: events.FrozenUnknown}
				}
			})
			processResult <- err
		}()
		select {
		case <-entered:
		case err := <-processResult:
			t.Fatalf("process failed before broker boundary: %v", err)
		case <-ctx.Done():
			t.Fatal("process did not reach broker boundary")
		}
		owner := newControlActor(t, ctx, db.adminPool, "account_approval_control_owner")
		fenceResult := make(chan error, 1)
		go func() {
			tx, err := owner.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
			if err == nil {
				_, err = tx.Exec(ctx, `UPDATE account_approval_control.admin_approval_control
					SET authorization_relay_state='FENCED',authorization_relay_epoch=authorization_relay_epoch+1,version=version+1
					WHERE control_id='ADMIN_APPROVAL'`)
			}
			if err == nil {
				err = tx.Commit(ctx)
			} else if tx != nil {
				_ = tx.Rollback(context.Background())
			}
			fenceResult <- err
		}()
		waitBackendHasBlocker(t, ctx, db.adminPool, owner.PgConn().PID(), "fence did not wait for the publish transaction")
		close(release)
		released = true
		if err := <-processResult; err != nil {
			t.Fatal("publish close", err)
		}
		requireControlSQLState(t, <-fenceResult, "40001")
		if _, err := owner.Exec(ctx, `UPDATE account_approval_control.admin_approval_control
			SET authorization_relay_state='FENCED',authorization_relay_epoch=authorization_relay_epoch+1,version=version+1
			WHERE control_id='ADMIN_APPROVAL'`); err != nil {
			t.Fatal("fence retry after published close", err)
		}
		var attempts int
		var published bool
		if err := db.adminPool.QueryRow(ctx, `SELECT attempts,published_at IS NOT NULL FROM public.outbox_events WHERE event_id=$1`, eventID).Scan(&attempts, &published); err != nil || attempts != 1 || !published {
			t.Fatal("publish terminal facts were not unique")
		}
	})

	t.Run("fence-first", func(t *testing.T) {
		db, store := newApprovalRelayIntegrationDatabase(t)
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		seedBackfillAuthorization(t, ctx, db, true)
		owner := newControlActor(t, ctx, db.adminPool, "account_approval_control_owner")
		if _, err := owner.Exec(ctx, `UPDATE account_approval_control.admin_approval_control
			SET authorization_relay_state='FENCED',authorization_relay_epoch=authorization_relay_epoch+1,version=version+1
			WHERE control_id='ADMIN_APPROVAL'`); err != nil {
			t.Fatal(err)
		}
		fakeClaim := events.FrozenClaim{EventID: uuid.New(), EventType: ApprovalAuthorizationEventType, TraceID: "fenced", ClaimToken: uuid.New(), RelayControlEpoch: 1}
		calls := 0
		if _, err := store.ProcessFrozen(ctx, "fenced-publisher", fakeClaim, 12, time.Now().UTC(), func(context.Context, events.FrozenMessage) events.FrozenPublishResult {
			calls++
			return events.FrozenPublishResult{Disposition: events.FrozenPublished}
		}); err != ErrApprovalRelayUnavailable || calls != 0 {
			t.Fatalf("fence-first process reached broker: calls=%d err=%v", calls, err)
		}
	})
}

func TestApprovalRelayLegacyClaimAndFenceLockOrderIntegration(t *testing.T) {
	for _, order := range []string{"claim-first", "fence-first"} {
		t.Run(order, func(t *testing.T) {
			db, store := newApprovalRelayIntegrationDatabase(t)
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			eventID := seedBackfillAuthorization(t, ctx, db, true)
			runtime := newControlActor(t, ctx, db.adminPool, "account_service")
			owner := newControlActor(t, ctx, db.adminPool, "account_approval_control_owner")
			runtimeTx, err := runtime.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
			if err != nil {
				t.Fatal(err)
			}
			defer runtimeTx.Rollback(context.Background())
			ownerTx, err := owner.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
			if err != nil {
				t.Fatal(err)
			}
			defer ownerTx.Rollback(context.Background())

			fenceSQL := `UPDATE account_approval_control.admin_approval_control
SET authorization_relay_state='FENCED',authorization_relay_epoch=authorization_relay_epoch+1,version=version+1
WHERE control_id='ADMIN_APPROVAL'`
			if order == "claim-first" {
				if err := claimLegacyAuthorizationRow(ctx, runtimeTx, eventID, "legacy-worker"); err != nil {
					t.Fatal(err)
				}
				fenceResult := make(chan error, 1)
				go func() { _, err := ownerTx.Exec(ctx, fenceSQL); fenceResult <- err }()
				waitControlBlocked(t, ctx, db.adminPool, owner.PgConn().PID(), runtime.PgConn().PID())
				if err := runtimeTx.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				if err := <-fenceResult; err != nil {
					t.Fatal(err)
				}
				requireControlSQLState(t, ownerTx.Commit(ctx), "40001")
				retryOwner := newControlActor(t, ctx, db.adminPool, "account_approval_control_owner")
				retryTx, err := retryOwner.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := retryTx.Exec(ctx, fenceSQL); err != nil {
					t.Fatal(err)
				}
				requireControlSQLState(t, retryTx.Commit(ctx), "23514")
				var state string
				if err := db.adminPool.QueryRow(ctx, `SELECT authorization_relay_state FROM account_approval_control.admin_approval_control`).Scan(&state); err != nil || state != "RUNNING" {
					t.Fatal("fence committed over an active claim")
				}
				return
			}

			if _, err := ownerTx.Exec(ctx, fenceSQL); err != nil {
				t.Fatal(err)
			}
			claimResult := make(chan error, 1)
			go func() { claimResult <- claimLegacyAuthorizationRow(ctx, runtimeTx, eventID, "late-legacy-worker") }()
			waitControlBlocked(t, ctx, db.adminPool, runtime.PgConn().PID(), owner.PgConn().PID())
			if err := ownerTx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			requireControlSQLState(t, <-claimResult, "40001")
			_ = runtimeTx.Rollback(ctx)
			retryRuntime := newControlActor(t, ctx, db.adminPool, "account_service")
			retryTx, err := retryRuntime.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
			if err != nil {
				t.Fatal(err)
			}
			requireControlSQLState(t, claimLegacyAuthorizationRow(ctx, retryTx, eventID, "late-legacy-worker"), "55000")
			_ = retryTx.Rollback(ctx)
			var lockedBy *string
			if err := db.adminPool.QueryRow(ctx, `SELECT locked_by FROM public.outbox_events WHERE event_id=$1`, eventID).Scan(&lockedBy); err != nil || lockedBy != nil {
				t.Fatal("late legacy claimant changed the row")
			}
			if err := store.Ready(ctx); err != nil {
				t.Fatal("FENCED pending authorization row was not ready", err)
			}
			if stats, err := store.Stats(ctx); err != nil || stats.Pending != 1 || stats.ActiveClaims != 0 || stats.ReclaimableClaims != 0 {
				t.Fatalf("FENCED pending stats changed: %+v %v", stats, err)
			}
			if claims, err := store.ClaimFrozen(ctx, "fenced-observer", 1, 30*time.Second); err != nil || len(claims) != 0 {
				t.Fatalf("FENCED relay claimed pending work: %+v %v", claims, err)
			}
		})
	}
}

func TestApprovalRelayQuiescingDrainsOnlyEpochOneLegacyClaimIntegration(t *testing.T) {
	db, store := newApprovalRelayIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	eventID := seedBackfillAuthorization(t, ctx, db, true)
	runtime := newControlActor(t, ctx, db.adminPool, "account_service")
	runtimeTx, err := runtime.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		t.Fatal(err)
	}
	if err := claimLegacyAuthorizationRow(ctx, runtimeTx, eventID, "legacy-drain-worker"); err != nil || runtimeTx.Commit(ctx) != nil {
		t.Fatal("legacy claim", err)
	}
	owner := newControlActor(t, ctx, db.adminPool, "account_approval_control_owner")
	if _, err := owner.Exec(ctx, `UPDATE account_approval_control.admin_approval_control
SET mode='QUIESCING',approval_epoch=approval_epoch+1,version=version+1 WHERE control_id='ADMIN_APPROVAL'`); err != nil {
		t.Fatal(err)
	}
	stats, err := store.Stats(ctx)
	if err != nil || stats.Pending != 1 || stats.ActiveClaims != 1 || stats.ReclaimableClaims != 0 {
		t.Fatalf("QUIESCING old claim was not observable: %+v %v", stats, err)
	}
	if _, err := runtime.Exec(ctx, `UPDATE public.outbox_events SET attempts=attempts+1,published_at=clock_timestamp(),last_error=NULL,
locked_by=NULL,locked_until=NULL WHERE event_id=$1`, eventID); err != nil {
		t.Fatal("QUIESCING old claim could not close", err)
	}
	if stats, err = store.Stats(ctx); err != nil || stats.Pending != 0 || stats.ActiveClaims != 0 {
		t.Fatalf("drained stats: %+v %v", stats, err)
	}
}

func TestApprovalRelayLegacyShapeNeverRevivesAfterRelayEpochLeavesOneIntegration(t *testing.T) {
	db, store := newApprovalRelayIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	eventID := seedBackfillAuthorization(t, ctx, db, true)
	owner := newControlActor(t, ctx, db.adminPool, "account_approval_control_owner")
	for _, state := range []string{"FENCED", "RUNNING"} {
		if _, err := owner.Exec(ctx, `UPDATE account_approval_control.admin_approval_control
SET authorization_relay_state=$1,authorization_relay_epoch=authorization_relay_epoch+1,version=version+1
WHERE control_id='ADMIN_APPROVAL'`, state); err != nil {
			t.Fatal(err)
		}
	}
	runtime := newControlActor(t, ctx, db.adminPool, "account_service")
	tx, err := runtime.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		t.Fatal(err)
	}
	requireControlSQLState(t, claimLegacyAuthorizationRow(ctx, tx, eventID, "revived-legacy-worker"), "55000")
	_ = tx.Rollback(ctx)
	claims, err := store.ClaimFrozen(ctx, "fenced-worker", 1, 30*time.Second)
	if err != nil || len(claims) != 1 || claims[0].RelayControlEpoch != 3 {
		t.Fatalf("dedicated relay did not take over after epoch change: %+v %v", claims, err)
	}
}

func TestApprovalRelaySafeDrainClaimsOnlyPreviousApprovalEpochIntegration(t *testing.T) {
	db, store := newApprovalRelayIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	owner := newControlActor(t, ctx, db.adminPool, "account_approval_control_owner")
	for _, statement := range []string{
		`UPDATE account_approval_control.admin_approval_control SET mode='QUIESCING',approval_epoch=approval_epoch+1,version=version+1 WHERE control_id='ADMIN_APPROVAL'`,
		`UPDATE account_approval_control.admin_approval_control SET authorization_relay_state='FENCED',authorization_relay_epoch=authorization_relay_epoch+1,version=version+1 WHERE control_id='ADMIN_APPROVAL'`,
		`UPDATE account_approval_control.admin_approval_control SET mode='PROOF_REQUIRED',approval_wire_version=2,proof_required=true,approval_epoch=approval_epoch+1,version=version+1 WHERE control_id='ADMIN_APPROVAL'`,
		`UPDATE account_approval_control.admin_approval_control SET authorization_relay_state='RUNNING',authorization_relay_epoch=authorization_relay_epoch+1,version=version+1 WHERE control_id='ADMIN_APPROVAL'`,
	} {
		if _, err := owner.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile("../platform/approvalencoding/testdata/golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Vectors []authorizationProofVector `json:"proof_vectors"`
	}
	if json.Unmarshal(data, &fixture) != nil || len(fixture.Vectors) == 0 {
		t.Fatal("proof fixture")
	}
	vector := fixture.Vectors[0]
	request := seedAuthorizationProofRequest(t, ctx, db, vector)
	artifacts, err := approvalencoding.ConvertLegacyProofInput(approvalencoding.LegacyProofInput{
		CommandType: vector.CommandType, PolicyVersion: vector.PolicyVersion, SnapshotSchemaVersion: vector.SnapshotSchemaVersion,
		TargetResourceType: vector.Legacy.TargetType, TargetResourceID: vector.Legacy.TargetID,
		NormalizedCommand: json.RawMessage(vector.Legacy.Normalized), CriticalResourceVersions: json.RawMessage(vector.Legacy.Critical), FinancialSnapshot: json.RawMessage(vector.Legacy.Financial),
	})
	if err != nil {
		t.Fatal(err)
	}
	issuedAt := time.Now().UTC()
	proof := ApprovalAuthorizationProof{Artifacts: artifacts, Response: approvalencoding.ProofResponse{
		ApprovalRequestID: request.ID, CommandType: vector.CommandType, PolicyVersion: vector.PolicyVersion,
		CommandDigest: artifacts.CommandDigest, SnapshotDigest: artifacts.SnapshotDigest,
		ProofSchemaVersion: approvalencoding.ProofSchemaVersion, DomainRevision: "safe-drain-revision",
		Verdict: "ALLOW", ReasonCode: "PROOF_VALID", IssuedAt: issuedAt, ExpiresAt: issuedAt.Add(time.Minute),
	}}
	eventID := uuid.New()
	tx, err := db.writerPool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		t.Fatal(err)
	}
	control, err := lockApprovalAuthorizationControl(ctx, tx)
	if err != nil {
		t.Fatal(err)
	}
	_ = authorizationPrepareLegacyApproval(t, ctx, tx, request)
	if _, err := appendProofApprovalAuthorization(ctx, tx, control, request.ID, eventID, proof); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(ctx, `UPDATE account_approval_control.admin_approval_control
		SET mode='SAFE_ROLLBACK',approval_wire_version=1,proof_required=true,
			approval_epoch=approval_epoch+1,version=version+1
		WHERE control_id='ADMIN_APPROVAL'`); err != nil {
		t.Fatal(err)
	}
	claims, err := store.ClaimFrozen(ctx, "safe-drain-worker", 10, 30*time.Second)
	if err != nil || len(claims) != 1 || claims[0].EventID != eventID || claims[0].RelayControlEpoch != 3 {
		t.Fatalf("SAFE_DRAIN did not claim the previous approval epoch: %+v %v", claims, err)
	}
	if disposition, err := store.ProcessFrozen(ctx, "safe-drain-worker", claims[0], 12, time.Now().UTC(), func(context.Context, events.FrozenMessage) events.FrozenPublishResult {
		return events.FrozenPublishResult{Disposition: events.FrozenPublished}
	}); err != nil || disposition != events.FrozenPublished {
		t.Fatal("SAFE_DRAIN frozen publish", err)
	}
}
