package account

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Use a real runtime LOGIN on the explicitly isolated cluster. Preserve the
// original password verifier in memory and restore it before fixture cleanup;
// neither the verifier nor the generated password is logged or persisted.
func newApprovalGenericPartition(t *testing.T, ctx context.Context, db *approvalAuthorizationIntegrationDatabase) *events.PostgresStore {
	t.Helper()
	configuration, err := url.Parse(os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL"))
	if err != nil {
		t.Fatal("invalid isolated database configuration")
	}
	configuration.Path = "/" + db.databaseName
	var originalVerifier *string
	var login bool
	if err := db.adminPool.QueryRow(ctx, `SELECT rolpassword,rolcanlogin FROM pg_catalog.pg_authid WHERE rolname='account_service'`).Scan(&originalVerifier, &login); err != nil || !login {
		t.Fatal("isolated runtime LOGIN precondition failed")
	}
	password := uuid.NewString()
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		literal := "NULL"
		if originalVerifier != nil {
			literal = "'" + strings.ReplaceAll(*originalVerifier, "'", "''") + "'"
		}
		if _, err := db.adminPool.Exec(cleanup, "ALTER ROLE account_service PASSWORD "+literal); err != nil {
			t.Error("restore isolated runtime password verifier")
		}
	})
	if _, err := db.adminPool.Exec(ctx, "ALTER ROLE account_service PASSWORD '"+password+"'"); err != nil {
		t.Fatal("configure isolated runtime credential")
	}
	configuration.User = url.UserPassword("account_service", password)
	probe, err := pgx.Connect(ctx, configuration.String())
	if err != nil {
		t.Fatal("isolated generic identity probe connection failed")
	}
	var runtimeIdentity bool
	err = probe.QueryRow(ctx, `SELECT session_user='account_service' AND current_user='account_service'`).Scan(&runtimeIdentity)
	_ = probe.Close(ctx)
	if err != nil || !runtimeIdentity {
		t.Fatal("isolated generic connection did not assume runtime identity")
	}
	store, err := events.NewPartitionedPostgresStore(ctx, configuration.String(), []string{ApprovalAuthorizationEventType})
	if err != nil {
		t.Fatal("isolated generic partition connection failed")
	}
	t.Cleanup(store.Close)
	return store
}

func TestApprovalRelayManualReplayCannotBypassOldIdempotencyIntegration(t *testing.T) {
	db, _ := newApprovalRelayIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	eventID := seedBackfillAuthorization(t, ctx, db, true)
	// Reproduce a historical dead-letter row without granting the runtime any
	// mutation privilege. The fixture restores all guards before API calls.
	if _, err := db.adminPool.Exec(ctx, `ALTER TABLE public.outbox_events DISABLE TRIGGER USER;
UPDATE public.outbox_events SET dead_lettered_at=now(),attempts=12,last_error='fixture private broker detail'
WHERE event_type='account.approval-command.authorized.v1';
ALTER TABLE public.outbox_events ENABLE TRIGGER USER`); err != nil {
		t.Fatal("seed isolated historical dead letter")
	}
	generic := newApprovalGenericPartition(t, ctx, db)
	_, err := db.adminPool.Exec(ctx, `SELECT * FROM public.inspect_authorization_dead_letter($1)`, eventID)
	requireControlSQLState(t, err, "42501")
	_, err = db.runtimePool.Exec(ctx, `SELECT delivery_bytes FROM account_approval_authorization.approval_authorization_event_bytes WHERE event_id=$1`, eventID)
	requireControlSQLState(t, err, "42501")
	inspected, err := generic.InspectDeadLetter(ctx, eventID)
	if err != nil || inspected.Replayable || inspected.LastError != "" || len(inspected.EventDigest) != 64 {
		var postgresError *pgconn.PgError
		if errors.As(err, &postgresError) {
			t.Logf("inspect SQLSTATE=%s", postgresError.Code)
		}
		t.Logf("inspect error=%t replayable=%t error_redacted=%t digest_length=%d", err != nil, inspected.Replayable, inspected.LastError == "", len(inspected.EventDigest))
		t.Fatal("authorization inspect was not a non-replayable redacted summary")
	}
	job, err := serviceauth.ContextForJob(ctx, "event-replay", "EVENT_REPLAY", "fixture-run")
	if err != nil {
		t.Fatal(err)
	}
	request := events.ReplayRequest{EventID: eventID, ExpectedDigest: inspected.EventDigest, IdempotencyKey: "fixture-old-replay-key", Reason: "historical approved recovery fixture"}
	// Match the historical fingerprint exactly; an arbitrary hash would only
	// demonstrate a conflict, not prevent the old successful-idempotency path.
	body, err := json.Marshal(struct {
		EventID                          uuid.UUID `json:"event_id"`
		Digest, Reason, Service, JobType string
	}{eventID, request.ExpectedDigest, request.Reason, "event-replay", "EVENT_REPLAY"})
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(body)
	insert := `INSERT INTO public.event_replay_requests(replay_id,event_id,event_digest,idempotency_key,request_hash,actor_type,service_name,job_type,job_run_id,reason,replayed_at)
VALUES($1,$2,$3,$4,$5,'SERVICE','event-replay','EVENT_REPLAY','fixture-run',$6,now())`
	args := []any{uuid.New(), eventID, request.ExpectedDigest, request.IdempotencyKey, hex.EncodeToString(hash[:]), request.Reason}
	// The current SQL boundary independently forbids new replay audit inserts.
	_, err = db.runtimePool.Exec(job, insert, args...)
	requireControlSQLState(t, err, "55000")
	var forbidden *pgconn.PgError
	if !errors.As(err, &forbidden) || forbidden.Message != "AUTHORIZATION_EVENT_REPLAY_FORBIDDEN" {
		t.Fatal("runtime replay insert did not reach authorization prohibition")
	}
	if _, err := db.adminPool.Exec(ctx, `ALTER TABLE public.event_replay_requests DISABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.adminPool.Exec(ctx, insert, args...); err != nil {
		t.Fatal("seed isolated historical idempotency record")
	}
	if _, err := db.adminPool.Exec(ctx, `ALTER TABLE public.event_replay_requests ENABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	var before string
	if err := db.adminPool.QueryRow(ctx, `SELECT to_jsonb(o)::text FROM public.outbox_events o WHERE event_id=$1`, eventID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{request.IdempotencyKey, "fixture-new-replay-key"} {
		request.IdempotencyKey = key
		result, err := generic.ReplayDeadLetter(job, request)
		if !errors.Is(err, events.ErrAuthorizationEventReplayForbidden) || result.ReplayID != uuid.Nil || result.Replayed {
			t.Fatal("authorization replay bypassed persistent event-type prohibition")
		}
	}
	for _, mutation := range []string{
		`UPDATE public.outbox_events SET dead_lettered_at=NULL WHERE event_id=$1`,
		`UPDATE public.outbox_events SET attempts=0 WHERE event_id=$1`,
		`UPDATE public.outbox_events SET next_attempt_at=now() WHERE event_id=$1`,
	} {
		_, err := db.runtimePool.Exec(ctx, mutation, eventID)
		requireControlSQLState(t, err, "55000")
	}
	var after string
	var count int
	if err := db.adminPool.QueryRow(ctx, `SELECT to_jsonb(o)::text FROM public.outbox_events o WHERE event_id=$1`, eventID).Scan(&after); err != nil || after != before {
		t.Fatal("replay attempts changed immutable authorization row")
	}
	if err := db.adminPool.QueryRow(ctx, `SELECT count(*) FROM public.event_replay_requests WHERE event_id=$1`, eventID).Scan(&count); err != nil || count != 1 {
		t.Fatal("replay attempts created an audit request")
	}
}

func TestApprovalRelayPartitionDoesNotStarveOrdinaryEventsIntegration(t *testing.T) {
	db, dedicated := newApprovalRelayIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	authorizationID := seedBackfillAuthorization(t, ctx, db, true)
	// Inject an invalid earlier authorization row through fixture-only owner
	// access; a generic claim must exclude it before LIMIT and row mutation.
	if _, err := db.adminPool.Exec(ctx, `ALTER TABLE public.outbox_events DISABLE TRIGGER USER;
UPDATE public.outbox_events SET payload='{}'::jsonb WHERE event_type='account.approval-command.authorized.v1';
ALTER TABLE public.outbox_events ENABLE TRIGGER USER`); err != nil {
		t.Fatal("inject isolated invalid authorization row")
	}
	var before string
	if err := db.adminPool.QueryRow(ctx, `SELECT to_jsonb(o)::text FROM public.outbox_events o WHERE event_id=$1`, authorizationID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	aggregate := uuid.New()
	tx, err := db.runtimePool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if err := events.AppendOutbox(ctx, tx, "account.fixture.v1", aggregate, 1, map[string]any{"ordinary": true}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	generic := newApprovalGenericPartition(t, ctx, db)
	stats, err := generic.Stats(ctx)
	if err != nil || stats.Pending != 1 || stats.DeadLettered != 0 {
		t.Fatalf("generic partition stats=%+v err=%v", stats, err)
	}
	if dedicated.Ready(ctx) == nil {
		t.Fatal("invalid authorization partition remained ready")
	}
	claimed, err := generic.Claim(ctx, "ordinary-worker", 1, time.Minute)
	if err != nil || len(claimed) != 1 || claimed[0].AggregateID != aggregate || claimed[0].Type != "account.fixture.v1" {
		t.Fatal("authorization row starved ordinary LIMIT 1 claim")
	}
	if _, err := claimed[0].Marshal(); err != nil {
		t.Fatal("ordinary envelope became invalid")
	}
	if err := generic.MarkPublished(ctx, "ordinary-worker", claimed[0]); err != nil {
		t.Fatal(err)
	}
	stats, err = generic.Stats(ctx)
	if err != nil || stats.Pending != 0 || stats.DeadLettered != 0 {
		t.Fatal("ordinary publication did not empty generic partition")
	}
	var after string
	if err := db.adminPool.QueryRow(ctx, `SELECT to_jsonb(o)::text FROM public.outbox_events o WHERE event_id=$1`, authorizationID).Scan(&after); err != nil || before != after {
		t.Fatal("generic operations mutated excluded authorization row")
	}
}
