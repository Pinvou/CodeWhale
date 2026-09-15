package account

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestApprovalRelayPublishesFrozenBytesAndRetainsClaimAuditIntegration(t *testing.T) {
	db, store := newApprovalRelayIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	eventID := seedBackfillAuthorization(t, ctx, db, true)
	var expected []byte
	if err := db.adminPool.QueryRow(ctx, `SELECT delivery_bytes FROM account_approval_authorization.approval_authorization_event_bytes WHERE event_id=$1`, eventID).Scan(&expected); err != nil {
		t.Fatal(err)
	}
	claims, err := store.ClaimFrozen(ctx, "relay-worker", 1, 30*time.Second)
	if err != nil || len(claims) != 1 || claims[0].EventID != eventID || claims[0].ClaimToken == uuid.Nil {
		t.Fatalf("claims=%+v err=%v", claims, err)
	}
	called := 0
	disposition, err := store.ProcessFrozen(ctx, "relay-worker", claims[0], 12, time.Now().UTC(), func(_ context.Context, message events.FrozenMessage) events.FrozenPublishResult {
		called++
		if message.EventID != eventID || message.Subject != ApprovalAuthorizationEventType || message.MsgID != eventID.String() || !bytes.Equal(message.Body, expected) {
			t.Fatal("frozen publication changed")
		}
		return events.FrozenPublishResult{Disposition: events.FrozenPublished}
	})
	if err != nil || disposition != events.FrozenPublished || called != 1 {
		t.Fatalf("disposition=%s called=%d err=%v", disposition, called, err)
	}
	var attempts int
	var published time.Time
	var token uuid.UUID
	var epoch int64
	if err := db.adminPool.QueryRow(ctx, `SELECT attempts,published_at,relay_claim_token,relay_control_epoch FROM public.outbox_events WHERE event_id=$1`, eventID).Scan(&attempts, &published, &token, &epoch); err != nil || attempts != 1 || token != claims[0].ClaimToken || epoch != claims[0].RelayControlEpoch {
		t.Fatalf("audit attempts=%d token=%s epoch=%d err=%v", attempts, token, epoch, err)
	}
	if again, err := store.ClaimFrozen(ctx, "relay-worker", 1, 30*time.Second); err != nil || len(again) != 0 {
		t.Fatal("published event was reclaimed")
	}
}

func TestApprovalRelayFinishRejectsNullEarlyDLQAndUnboundedRetryIntegration(t *testing.T) {
	db, store := newApprovalRelayIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	eventID := seedBackfillAuthorization(t, ctx, db, true)
	claims, err := store.ClaimFrozen(ctx, "validation-worker", 1, 30*time.Second)
	if err != nil || len(claims) != 1 {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		outcome any
		retry   any
		code    any
	}{
		{"null-outcome", nil, nil, nil},
		{"early-dead-letter", "DEAD_LETTERED", nil, "EVENT_DELIVERY_FAILED"},
		{"raw-error", "RETRY", time.Now().UTC().Add(time.Minute), "sensitive raw broker failure"},
		{"unbounded-retry", "RETRY", time.Now().UTC().Add(time.Hour), "EVENT_DELIVERY_FAILED"},
	} {
		t.Run(test.name, func(t *testing.T) {
			tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.Background())
			var eventType, trace, digest string
			var body []byte
			var attempts int
			var lease time.Time
			if err := tx.QueryRow(ctx, `SELECT * FROM account_approval_authorization.open_authorization_delivery($1,$2,$3,$4)`, eventID, claims[0].ClaimToken, claims[0].RelayControlEpoch, "validation-worker").Scan(&eventType, &trace, &body, &digest, &attempts, &lease); err != nil {
				t.Fatal(err)
			}
			_, err = tx.Exec(ctx, `SELECT account_approval_authorization.finish_authorization_delivery($1,$2,$3,$4,$5,$6,$7)`, eventID, claims[0].ClaimToken, claims[0].RelayControlEpoch, "validation-worker", test.outcome, test.retry, test.code)
			if test.name == "early-dead-letter" {
				requireControlSQLState(t, err, "55000")
			} else {
				requireControlSQLState(t, err, "22023")
			}
		})
	}
	if _, err := store.ClaimFrozen(ctx, "bad\nworker", 1, 30*time.Second); err != ErrApprovalRelayUnavailable {
		t.Fatal("control-character worker accepted")
	}
}

func TestApprovalRelayReadinessRejectsPersistedPayloadDriftIntegration(t *testing.T) {
	db, store := newApprovalRelayIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	eventID := seedBackfillAuthorization(t, ctx, db, true)
	if _, err := db.adminPool.Exec(ctx, `ALTER TABLE public.outbox_events DISABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = db.adminPool.Exec(cleanup, `ALTER TABLE public.outbox_events ENABLE TRIGGER USER`)
	})
	if _, err := db.adminPool.Exec(ctx, `UPDATE public.outbox_events SET payload=payload||'{"proof_digest":"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"}'::jsonb WHERE event_id=$1`, eventID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.adminPool.Exec(ctx, `ALTER TABLE public.outbox_events ENABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	if err := store.Ready(ctx); err != ErrApprovalRelayUnavailable {
		t.Fatal("persisted authorization drift remained ready")
	}
}

func TestApprovalRelayUnknownAndStaleWorkerNeverAdvanceOutcomeIntegration(t *testing.T) {
	db, store := newApprovalRelayIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	eventID := seedBackfillAuthorization(t, ctx, db, true)
	claims, err := store.ClaimFrozen(ctx, "owner-worker", 1, 30*time.Second)
	if err != nil || len(claims) != 1 {
		t.Fatal(err)
	}
	called := 0
	if _, err := store.ProcessFrozen(ctx, "wrong-worker", claims[0], 12, time.Now().UTC(), func(context.Context, events.FrozenMessage) events.FrozenPublishResult {
		called++
		return events.FrozenPublishResult{Disposition: events.FrozenPublished}
	}); err != ErrApprovalRelayUnavailable || called != 0 {
		t.Fatal("stale worker reached broker")
	}
	if _, err := store.ProcessFrozen(ctx, "owner-worker", claims[0], 12, time.Now().UTC(), func(context.Context, events.FrozenMessage) events.FrozenPublishResult {
		called++
		return events.FrozenPublishResult{Disposition: events.FrozenUnknown}
	}); err != ErrApprovalRelayUnavailable || called != 1 {
		t.Fatal("unknown broker outcome was accepted")
	}
	var attempts int
	var published, dead any
	var lockedBy string
	if err := db.adminPool.QueryRow(ctx, `SELECT attempts,published_at,dead_lettered_at,locked_by FROM public.outbox_events WHERE event_id=$1`, eventID).Scan(&attempts, &published, &dead, &lockedBy); err != nil || attempts != 0 || published != nil || dead != nil || lockedBy != "owner-worker" {
		t.Fatalf("outcome changed attempts=%d published=%v dead=%v lock=%s err=%v", attempts, published, dead, lockedBy, err)
	}
}

func TestApprovalRelayInvalidPublisherDispositionRollsBackIntegration(t *testing.T) {
	for _, tc := range []struct {
		name     string
		attempts int
		publish  func(int) events.FrozenPublishResult
	}{
		{"main-empty", 0, func(int) events.FrozenPublishResult { return events.FrozenPublishResult{} }},
		{"main-dead-letter", 0, func(int) events.FrozenPublishResult {
			return events.FrozenPublishResult{Disposition: events.FrozenDeadLettered}
		}},
		{"dead-letter-empty", 11, func(call int) events.FrozenPublishResult {
			if call == 1 {
				return events.FrozenPublishResult{Disposition: events.FrozenRetry, ErrorCode: "EVENT_DELIVERY_FAILED"}
			}
			return events.FrozenPublishResult{}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, store := newApprovalRelayIntegrationDatabase(t)
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			eventID := seedBackfillAuthorization(t, ctx, db, true)
			if tc.attempts != 0 {
				if _, err := db.adminPool.Exec(ctx, `ALTER TABLE public.outbox_events DISABLE TRIGGER USER`); err != nil {
					t.Fatal(err)
				}
				if _, err := db.adminPool.Exec(ctx, `UPDATE public.outbox_events SET attempts=$2 WHERE event_id=$1`, eventID, tc.attempts); err != nil {
					t.Fatal(err)
				}
				if _, err := db.adminPool.Exec(ctx, `ALTER TABLE public.outbox_events ENABLE TRIGGER USER`); err != nil {
					t.Fatal(err)
				}
			}
			claims, err := store.ClaimFrozen(ctx, "invalid-outcome-worker", 1, 30*time.Second)
			if err != nil || len(claims) != 1 {
				t.Fatal("claim", err)
			}
			calls := 0
			if _, err := store.ProcessFrozen(ctx, "invalid-outcome-worker", claims[0], 12, time.Now().UTC(), func(context.Context, events.FrozenMessage) events.FrozenPublishResult {
				calls++
				return tc.publish(calls)
			}); err != ErrApprovalRelayUnavailable {
				t.Fatal("invalid disposition was accepted")
			}
			var attempts int
			var lockedBy string
			var published, dead any
			if err := db.adminPool.QueryRow(ctx, `SELECT attempts,locked_by,published_at,dead_lettered_at FROM public.outbox_events WHERE event_id=$1`, eventID).Scan(&attempts, &lockedBy, &published, &dead); err != nil || attempts != tc.attempts || lockedBy != "invalid-outcome-worker" || published != nil || dead != nil {
				t.Fatalf("invalid outcome changed row: attempts=%d lock=%q published=%v dead=%v err=%v", attempts, lockedBy, published, dead, err)
			}
		})
	}
}

func TestApprovalRelayCommitUnknownUsesImmutableOutcomeWithoutRepublishIntegration(t *testing.T) {
	for _, outcome := range []string{"committed", "not-committed", "lookup-failed"} {
		t.Run(outcome, func(t *testing.T) {
			db, store := newApprovalRelayIntegrationDatabase(t)
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			eventID := seedBackfillAuthorization(t, ctx, db, true)
			claims, err := store.ClaimFrozen(ctx, "unknown-commit-worker", 1, 30*time.Second)
			if err != nil || len(claims) != 1 {
				t.Fatal("claim", err)
			}
			store.commit = func(ctx context.Context, tx pgx.Tx) error {
				if outcome != "not-committed" {
					if err := tx.Commit(ctx); err != nil {
						return err
					}
				}
				return errors.New("simulated transport outcome unknown")
			}
			if outcome == "lookup-failed" {
				store.outcomeLookup = func(events.FrozenClaim, string) (string, int, error) {
					return "", 0, errors.New("simulated readback failure")
				}
			}
			calls := 0
			disposition, processErr := store.ProcessFrozen(ctx, "unknown-commit-worker", claims[0], 12, time.Now().UTC(), func(context.Context, events.FrozenMessage) events.FrozenPublishResult {
				calls++
				return events.FrozenPublishResult{Disposition: events.FrozenPublished}
			})
			if calls != 1 {
				t.Fatal("unknown commit retried broker publication")
			}
			if outcome == "committed" {
				if processErr != nil || disposition != events.FrozenPublished {
					t.Fatalf("committed outcome was not recovered: %s %v", disposition, processErr)
				}
			} else if processErr != ErrApprovalRelayUnavailable || disposition != "" {
				t.Fatalf("unknown outcome was guessed: %s %v", disposition, processErr)
			}
			var attempts int
			var published bool
			if err := db.adminPool.QueryRow(ctx, `SELECT attempts,published_at IS NOT NULL FROM public.outbox_events WHERE event_id=$1`, eventID).Scan(&attempts, &published); err != nil {
				t.Fatal(err)
			}
			if (outcome == "not-committed" && (attempts != 0 || published)) ||
				(outcome != "not-committed" && (attempts != 1 || !published)) {
				t.Fatalf("unexpected durable outcome: attempts=%d published=%v", attempts, published)
			}
		})
	}
}

func TestApprovalRelayExpiredLeaseReplacesTokenAndRejectsLateWorkerIntegration(t *testing.T) {
	db, store := newApprovalRelayIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	eventID := seedBackfillAuthorization(t, ctx, db, true)
	first, err := store.ClaimFrozen(ctx, "crashed-worker", 1, 30*time.Second)
	if err != nil || len(first) != 1 {
		t.Fatal("first claim", err)
	}
	if _, err := db.adminPool.Exec(ctx, `ALTER TABLE public.outbox_events DISABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.adminPool.Exec(ctx, `UPDATE public.outbox_events SET locked_until=clock_timestamp()-interval '1 second' WHERE event_id=$1`, eventID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.adminPool.Exec(ctx, `ALTER TABLE public.outbox_events ENABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	second, err := store.ClaimFrozen(ctx, "recovery-worker", 1, 30*time.Second)
	if err != nil || len(second) != 1 || second[0].ClaimToken == first[0].ClaimToken {
		t.Fatalf("lease was not reclaimed with a new token: %+v %v", second, err)
	}
	lateCalls := 0
	if _, err := store.ProcessFrozen(ctx, "crashed-worker", first[0], 12, time.Now().UTC(), func(context.Context, events.FrozenMessage) events.FrozenPublishResult {
		lateCalls++
		return events.FrozenPublishResult{Disposition: events.FrozenPublished}
	}); err != ErrApprovalRelayUnavailable || lateCalls != 0 {
		t.Fatal("stale worker reached the broker")
	}
	if disposition, err := store.ProcessFrozen(ctx, "recovery-worker", second[0], 12, time.Now().UTC(), func(context.Context, events.FrozenMessage) events.FrozenPublishResult {
		return events.FrozenPublishResult{Disposition: events.FrozenPublished}
	}); err != nil || disposition != events.FrozenPublished {
		t.Fatal("recovery worker failed", err)
	}
}

func TestApprovalRelayRetryReplacesAuditTokenAndRejectsPreviousTokenIntegration(t *testing.T) {
	db, store := newApprovalRelayIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	eventID := seedBackfillAuthorization(t, ctx, db, true)
	first, err := store.ClaimFrozen(ctx, "first-retry-worker", 1, 30*time.Second)
	if err != nil || len(first) != 1 {
		t.Fatal("first claim", err)
	}
	if disposition, err := store.ProcessFrozen(ctx, "first-retry-worker", first[0], 12, time.Now().UTC(), func(context.Context, events.FrozenMessage) events.FrozenPublishResult {
		return events.FrozenPublishResult{Disposition: events.FrozenRetry, ErrorCode: "EVENT_DELIVERY_FAILED"}
	}); err != nil || disposition != events.FrozenRetry {
		t.Fatal("retry close", err)
	}
	if _, err := db.adminPool.Exec(ctx, `ALTER TABLE public.outbox_events DISABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.adminPool.Exec(ctx, `UPDATE public.outbox_events SET next_attempt_at=clock_timestamp()-interval '1 second' WHERE event_id=$1`, eventID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.adminPool.Exec(ctx, `ALTER TABLE public.outbox_events ENABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	second, err := store.ClaimFrozen(ctx, "second-retry-worker", 1, 30*time.Second)
	if err != nil || len(second) != 1 || second[0].ClaimToken == first[0].ClaimToken {
		t.Fatalf("retry claim did not replace token: %+v %v", second, err)
	}
	called := 0
	if _, err := store.ProcessFrozen(ctx, "first-retry-worker", first[0], 12, time.Now().UTC(), func(context.Context, events.FrozenMessage) events.FrozenPublishResult {
		called++
		return events.FrozenPublishResult{Disposition: events.FrozenPublished}
	}); err != ErrApprovalRelayUnavailable || called != 0 {
		t.Fatal("previous retry token reached broker")
	}
}

func TestApprovalRelayDatabaseRejectsInvalidDeliveryShapesIntegration(t *testing.T) {
	for _, tc := range []struct {
		name string
		sql  string
	}{
		{"attempt-jump", `UPDATE public.outbox_events SET attempts=attempts+2 WHERE event_id=$1`},
		{"lock-pair", `UPDATE public.outbox_events SET locked_by='unpaired-worker' WHERE event_id=$1`},
		{"double-terminal", `UPDATE public.outbox_events SET published_at=clock_timestamp(),dead_lettered_at=clock_timestamp() WHERE event_id=$1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, _ := newApprovalRelayIntegrationDatabase(t)
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			eventID := seedBackfillAuthorization(t, ctx, db, true)
			_, err := db.runtimePool.Exec(ctx, tc.sql, eventID)
			requireControlSQLState(t, err, "55000")
			var attempts int
			var lockedBy *string
			var published, dead bool
			if err := db.adminPool.QueryRow(ctx, `SELECT attempts,locked_by,published_at IS NOT NULL,dead_lettered_at IS NOT NULL FROM public.outbox_events WHERE event_id=$1`, eventID).Scan(&attempts, &lockedBy, &published, &dead); err != nil || attempts != 0 || lockedBy != nil || published || dead {
				t.Fatal("invalid delivery shape changed row")
			}
		})
	}
	db, store := newApprovalRelayIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	eventID := seedBackfillAuthorization(t, ctx, db, true)
	claim, err := store.ClaimFrozen(ctx, "token-owner", 1, 30*time.Second)
	if err != nil || len(claim) != 1 {
		t.Fatal("claim", err)
	}
	_, err = db.adminPool.Exec(ctx, `UPDATE public.outbox_events SET relay_claim_token=$2 WHERE event_id=$1`, eventID, uuid.New())
	requireControlSQLState(t, err, "55000")
}

func TestApprovalRelayClaimTokensAreDatabaseUniqueIntegration(t *testing.T) {
	db, store := newApprovalRelayIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	firstEvent := seedBackfillAuthorization(t, ctx, db, true)
	secondEvent := seedBackfillAuthorization(t, ctx, db, true)
	claims, err := store.ClaimFrozen(ctx, "unique-token-worker", 2, 30*time.Second)
	if err != nil || len(claims) != 2 {
		t.Fatalf("claims=%+v err=%v", claims, err)
	}
	byEvent := map[uuid.UUID]events.FrozenClaim{}
	for _, claim := range claims {
		byEvent[claim.EventID] = claim
	}
	if byEvent[firstEvent].ClaimToken == uuid.Nil || byEvent[secondEvent].ClaimToken == uuid.Nil || byEvent[firstEvent].ClaimToken == byEvent[secondEvent].ClaimToken {
		t.Fatal("claim tokens were not distinct before constraint injection")
	}
	if _, err := db.adminPool.Exec(ctx, `ALTER TABLE public.outbox_events DISABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		_, _ = db.adminPool.Exec(cleanup, `ALTER TABLE public.outbox_events ENABLE TRIGGER USER`)
	})
	_, err = db.adminPool.Exec(ctx, `UPDATE public.outbox_events SET relay_claim_token=$2 WHERE event_id=$1`, secondEvent, byEvent[firstEvent].ClaimToken)
	requireControlSQLState(t, err, "23505")
	if _, err := db.adminPool.Exec(ctx, `ALTER TABLE public.outbox_events ENABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
}

func TestApprovalRelayReadinessCountsEachClaimInOneLeaseBucketIntegration(t *testing.T) {
	db, store := newApprovalRelayIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	firstEvent := seedBackfillAuthorization(t, ctx, db, true)
	seedBackfillAuthorization(t, ctx, db, true)
	claims, err := store.ClaimFrozen(ctx, "lease-bucket-worker", 2, 2*time.Minute)
	if err != nil || len(claims) != 2 {
		t.Fatalf("claims=%+v err=%v", claims, err)
	}
	if _, err := db.adminPool.Exec(ctx, `ALTER TABLE public.outbox_events DISABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		_, _ = db.adminPool.Exec(cleanup, `ALTER TABLE public.outbox_events ENABLE TRIGGER USER`)
	})
	if _, err := db.adminPool.Exec(ctx, `UPDATE public.outbox_events SET locked_until=clock_timestamp()-interval '1 microsecond' WHERE event_id=$1`, firstEvent); err != nil {
		t.Fatal(err)
	}
	if _, err := db.adminPool.Exec(ctx, `ALTER TABLE public.outbox_events ENABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	stats, err := store.Stats(ctx)
	if err != nil || stats.Pending != 2 || stats.ActiveClaims != 1 || stats.ReclaimableClaims != 1 || stats.ActiveClaims+stats.ReclaimableClaims != stats.Pending {
		t.Fatalf("claim lease buckets were not mutually exclusive: %+v %v", stats, err)
	}
}

func TestApprovalRelayDefinitiveFailureDeadLettersOnceAndCannotReviveIntegration(t *testing.T) {
	db, store := newApprovalRelayIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	eventID := seedBackfillAuthorization(t, ctx, db, true)
	if _, err := db.adminPool.Exec(ctx, `ALTER TABLE public.outbox_events DISABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = db.adminPool.Exec(cleanupCtx, `ALTER TABLE public.outbox_events ENABLE TRIGGER USER`)
	})
	if _, err := db.adminPool.Exec(ctx, `UPDATE public.outbox_events SET attempts=11 WHERE event_id=$1`, eventID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.adminPool.Exec(ctx, `ALTER TABLE public.outbox_events ENABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	claims, err := store.ClaimFrozen(ctx, "dead-worker", 1, 30*time.Second)
	if err != nil || len(claims) != 1 {
		t.Fatal(err)
	}
	calls := 0
	disposition, err := store.ProcessFrozen(ctx, "dead-worker", claims[0], 12, time.Now().UTC(), func(_ context.Context, message events.FrozenMessage) events.FrozenPublishResult {
		calls++
		if calls == 1 {
			return events.FrozenPublishResult{Disposition: events.FrozenRetry, ErrorCode: "EVENT_BROKER_UNAVAILABLE"}
		}
		if message.Subject != "account-platform.dead-letter."+ApprovalAuthorizationEventType || message.MsgID != eventID.String()+":dead-letter" {
			t.Fatal("unstable dead-letter identity")
		}
		return events.FrozenPublishResult{Disposition: events.FrozenPublished}
	})
	if err != nil || disposition != events.FrozenDeadLettered || calls != 2 {
		t.Fatalf("disposition=%s calls=%d err=%v", disposition, calls, err)
	}
	var attempts int
	var dead time.Time
	var token uuid.UUID
	if err := db.adminPool.QueryRow(ctx, `SELECT attempts,dead_lettered_at,relay_claim_token FROM public.outbox_events WHERE event_id=$1`, eventID).Scan(&attempts, &dead, &token); err != nil || attempts != 12 || token != claims[0].ClaimToken {
		t.Fatal("dead-letter audit mismatch")
	}
	if rows, err := store.ClaimFrozen(ctx, "other-worker", 1, 30*time.Second); err != nil || len(rows) != 0 {
		t.Fatal("dead-letter was revived")
	}
	if _, err := db.runtimePool.Exec(ctx, `UPDATE public.outbox_events SET dead_lettered_at=NULL,attempts=0 WHERE event_id=$1`, eventID); err == nil {
		t.Fatal("legacy runtime revived authorization dead-letter")
	}
}
