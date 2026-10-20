package account

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ConnString retains the original ParseConfig input; it does not serialize
// later fixture changes to User or Database. Preserve its transport options
// while overriding the actual connection fields, for both supported formats.
func ordinaryReplayFixtureDSN(config *pgx.ConnConfig) string {
	raw := config.ConnString()
	if strings.HasPrefix(raw, "postgres://") || strings.HasPrefix(raw, "postgresql://") {
		parsed, err := url.Parse(raw)
		if err != nil {
			panic("invalid parsed fixture connection URL")
		}
		query := parsed.Query()
		// pgx redacts URL userinfo in parse diagnostics, but not arbitrary query
		// parameters. Keep the actual password only in that userinfo position.
		query.Del("password")
		for key, value := range map[string]string{"host": config.Host, "port": strconv.Itoa(int(config.Port)), "user": config.User, "dbname": config.Database} {
			query.Set(key, value)
		}
		parsed.User = url.UserPassword(config.User, config.Password)
		parsed.Path, parsed.RawPath = "/"+config.Database, ""
		parsed.RawQuery = query.Encode()
		return parsed.String()
	}
	quote := func(value string) string {
		return "'" + strings.ReplaceAll(strings.ReplaceAll(value, `\`, `\\`), "'", `\'`) + "'"
	}
	return raw + " host=" + quote(config.Host) + " port=" + strconv.Itoa(int(config.Port)) +
		" user=" + quote(config.User) + " password=" + quote(config.Password) + " dbname=" + quote(config.Database)
}

func TestOrdinaryReplayFixtureDSNRoundTrip(t *testing.T) {
	for _, format := range []string{"url", "conninfo"} {
		t.Run(format, func(t *testing.T) {
			raw := "postgres://bootstrap:fixture@127.0.0.1:5432/postgres?sslmode=verify-full&application_name=replay-fixture&connect_timeout=7&password=old-controlled-query-password"
			if format == "conninfo" {
				raw = "host=127.0.0.1 port=5432 user=bootstrap password=fixture dbname=postgres sslmode=verify-full application_name=replay-fixture connect_timeout=7"
			}
			config, err := pgx.ParseConfig(raw)
			if err != nil {
				t.Fatal("parse controlled fixture configuration")
			}
			config.User, config.Database, config.Password = "account_service", "owned db-'\\", "controlled-'\\-password"
			dsn := ordinaryReplayFixtureDSN(config)
			if format == "url" {
				parsed, err := url.Parse(dsn)
				if err != nil || parsed.Query().Has("password") {
					t.Fatal("fixture password appeared outside URL userinfo")
				}
			}
			actual, err := pgx.ParseConfig(dsn)
			if err != nil {
				t.Fatal("roundtrip controlled fixture configuration")
			}
			if actual.User != config.User || actual.Database != config.Database || actual.Password != config.Password ||
				actual.Host != config.Host || actual.Port != config.Port || actual.ConnectTimeout != config.ConnectTimeout ||
				actual.RuntimeParams["application_name"] != config.RuntimeParams["application_name"] ||
				actual.TLSConfig == nil || actual.TLSConfig.ServerName != config.TLSConfig.ServerName ||
				actual.TLSConfig.InsecureSkipVerify != config.TLSConfig.InsecureSkipVerify {
				t.Fatal("fixture DSN changed identity, database or transport options")
			}
		})
	}
}

// Reuse the original 120s owned actor/UUID-database fixture and its natural
// cleanup budgets. The 90s below bounds this new scenario, not the old fixture.
func TestOrdinaryDeadLetterReplayRuntimePostgres(t *testing.T) {
	f := retainedWalletFenceActors(t)
	r := f.repository(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	runtimeDSN := ordinaryReplayFixtureDSN(r.pool.Config().ConnConfig)
	probe, err := pgx.Connect(ctx, runtimeDSN)
	if err != nil {
		t.Fatal("open actual runtime fixture connection")
	}
	var database, session, current string
	err = probe.QueryRow(ctx, `SELECT current_database(),session_user,current_user`).Scan(&database, &session, &current)
	_ = probe.Close(ctx)
	if err != nil || database != r.pool.Config().ConnConfig.Database || session != "account_service" || current != "account_service" {
		t.Fatal("fixture DSN did not preserve the nonce database and runtime identity")
	}
	store, err := events.NewAccountReplayPostgresStore(ctx, runtimeDSN)
	if err != nil {
		t.Fatal("open Account runtime replay store")
	}
	t.Cleanup(store.Close)
	operatorConfig := r.pool.Config().ConnConfig.Copy()
	operatorConfig.User = "account_migrator"
	operator, err := pgx.ConnectConfig(ctx, operatorConfig)
	if err != nil {
		t.Fatal("open original nonce database migrator")
	}
	t.Cleanup(func() { operator.Close(context.Background()) })
	if _, err := operator.Exec(ctx, "SET ROLE account_approval_authorization_owner"); err != nil {
		t.Fatal(err)
	}
	identity := serviceauth.Identity{ActorType: "SERVICE", Service: "event-replay", JobType: "EVENT_REPLAY", JobRunID: "initial-job"}
	job := func(run string) context.Context {
		c, err := serviceauth.ContextForJob(ctx, identity.Service, identity.JobType, run)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	snapshot := func() string {
		t.Helper()
		var body string
		if err := operator.QueryRow(ctx, `SELECT jsonb_build_object('outbox',(SELECT COALESCE(jsonb_agg(to_jsonb(o) ORDER BY event_id),'[]') FROM public.outbox_events o),'receipts',(SELECT COALESCE(jsonb_agg(to_jsonb(r) ORDER BY replay_id),'[]') FROM public.event_replay_requests r),'generations',(SELECT COALESCE(jsonb_agg(to_jsonb(g) ORDER BY replay_id),'[]') FROM account_approval_authorization.ordinary_replay_generations g))::text`).Scan(&body); err != nil {
			t.Fatal(err)
		}
		return body
	}
	seed := func() (events.Event, events.ReplayRequest, []byte) {
		t.Helper()
		tx, err := r.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		// This controlled fixture models a retained ordinary row; no application
		// producer is re-enabled by this test or by the replay capability.
		if err := events.AppendOutbox(ctx, tx, "account.user.updated", uuid.New(), 1, map[string]any{"retained_fixture": true}); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		claimed, err := store.Claim(ctx, "original-worker", 1, time.Minute, []string{"account.>"})
		if err != nil || len(claimed) != 1 {
			t.Fatalf("ordinary fixture claim: %v", err)
		}
		event := claimed[0]
		if err := store.MarkDeadLettered(ctx, "original-worker", event, "controlled retained failure"); err != nil {
			t.Fatal(err)
		}
		candidate, err := store.InspectDeadLetter(ctx, event.ID)
		if err != nil || !candidate.Replayable {
			t.Fatalf("inspect ordinary retained event: %v", err)
		}
		body, err := event.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		return event, events.ReplayRequest{EventID: event.ID, ExpectedDigest: candidate.EventDigest, IdempotencyKey: "retained-replay-" + uuid.NewString(), Reason: "independently approved recovery of retained delivery"}, body
	}
	args := func(e events.Event, q events.ReplayRequest, body []byte) []any {
		return []any{uuid.New(), e.ID, e.PublicationGeneration, q.ExpectedDigest, q.IdempotencyKey, strings.Repeat("a", 64), identity.Service, identity.JobType, identity.JobRunID, q.Reason, body}
	}
	const capability = `SELECT replay_id,event_id,replayed FROM public.enqueue_ordinary_dead_letter_replay($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`
	errorCode := func(err error, code string) bool {
		var pg *pgconn.PgError
		return errors.As(err, &pg) && pg.Code == code
	}

	t.Run("runtime-acl-and-empty-own-down-missing-capability", func(t *testing.T) {
		e, q, _ := seed()
		if _, err := r.pool.Exec(ctx, `UPDATE public.outbox_events SET publication_generation=publication_generation+1 WHERE event_id=$1`, e.ID); !errorCode(err, "42501") {
			t.Fatalf("original direct runtime generation UPDATE must be42501: %v", err)
		}
		generic, err := events.NewPostgresStore(ctx, ordinaryReplayFixtureDSN(r.pool.Config().ConnConfig))
		if err != nil {
			t.Fatal("open original generic runtime replay store")
		}
		t.Cleanup(generic.Close)
		beforeGeneric := snapshot()
		if _, err := generic.ReplayDeadLetter(job("original-generic-runtime"), q); !errorCode(err, "42501") || snapshot() != beforeGeneric {
			t.Fatalf("original generic receipt/enqueue must rollback on42501: %v", err)
		}
		var broad, column, member bool
		if err := r.pool.QueryRow(ctx, `SELECT has_table_privilege(current_user,'public.outbox_events','UPDATE'),has_column_privilege(current_user,'public.outbox_events','publication_generation','UPDATE'),pg_has_role(current_user,'account_approval_authorization_owner','MEMBER')`).Scan(&broad, &column, &member); err != nil || broad || column || member {
			t.Fatal("replay widened runtime authority")
		}
		ownDDL := func(direction string) {
			t.Helper()
			sql, err := os.ReadFile("../../migrations/account/000094_ordinary_dead_letter_replay_runtime." + direction + ".sql")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := operator.Exec(ctx, "SET ROLE account_service"); err != nil {
				t.Fatal(err)
			}
			tx, err := operator.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(ctx, string(sql)); err != nil {
				_ = tx.Rollback(ctx)
				t.Fatal(err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
		}
		ownDDL("down")
		var before string
		if err := r.pool.QueryRow(ctx, `SELECT to_jsonb(o)::text FROM public.outbox_events o WHERE event_id=$1`, e.ID).Scan(&before); err != nil {
			t.Fatal(err)
		}
		if _, err := store.ReplayDeadLetter(job("missing-capability"), q); !errorCode(err, "42883") {
			t.Fatalf("missing Account capability did not fail closed: %v", err)
		}
		var after string
		var receipts int
		if err := r.pool.QueryRow(ctx, `SELECT to_jsonb(o)::text,(SELECT count(*) FROM public.event_replay_requests) FROM public.outbox_events o WHERE event_id=$1`, e.ID).Scan(&after, &receipts); err != nil || before != after || receipts != 0 {
			t.Fatal("missing function fell back or left a receipt")
		}
		ownDDL("up")
		if _, err := operator.Exec(ctx, "SET ROLE account_approval_authorization_owner"); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("successful-replay-original-envelope-stale-generation-and-result-first", func(t *testing.T) {
		e, q, body := seed()
		result, err := store.ReplayDeadLetter(job("success"), q)
		if err != nil || !result.Replayed || result.EventID != e.ID {
			t.Fatalf("actual runtime ordinary replay: %+v %v", result, err)
		}
		before := snapshot()
		for _, mark := range []func() error{
			func() error { return store.MarkPublished(ctx, "original-worker", e) },
			func() error { return store.MarkFailed(ctx, "original-worker", e, time.Now(), "stale") },
			func() error { return store.MarkDeadLettered(ctx, "original-worker", e, "stale") },
		} {
			if mark() == nil || snapshot() != before {
				t.Fatal("old generation callback changed requeued event")
			}
		}
		fresh, err := store.Claim(ctx, "new-worker", 1, time.Minute, []string{"account.>"})
		if err != nil || len(fresh) != 1 || fresh[0].ID != e.ID || fresh[0].PublicationGeneration != e.PublicationGeneration+1 || fresh[0].Attempts != 0 {
			t.Fatal("replay did not acquire exactly one new generation")
		}
		newBody, _ := fresh[0].Marshal()
		if string(newBody) != string(body) {
			t.Fatal("replay changed immutable business envelope")
		}
		if err := store.MarkPublished(ctx, "new-worker", fresh[0]); err != nil {
			t.Fatal(err)
		}
		before = snapshot()
		retry, err := store.ReplayDeadLetter(job("different-job-run"), q)
		if err != nil || retry.Replayed || retry.ReplayID != result.ReplayID || snapshot() != before {
			t.Fatal("published old result was not returned without another enqueue")
		}
		var hash string
		if err := r.pool.QueryRow(ctx, `SELECT request_hash FROM public.event_replay_requests WHERE replay_id=$1`, result.ReplayID).Scan(&hash); err != nil {
			t.Fatal(err)
		}
		params := args(e, q, body)
		params[2], params[5] = int64(-1), hash
		var id, event uuid.UUID
		var replayed bool
		if err := r.pool.QueryRow(ctx, capability, params...).Scan(&id, &event, &replayed); err != nil || id != result.ReplayID || replayed || snapshot() != before {
			t.Fatal("DB result-first required old DLQ/generation or replayed receipt")
		}
		for _, change := range []string{"reason", "digest", "event"} {
			bad := q
			switch change {
			case "reason":
				bad.Reason += " changed"
			case "digest":
				bad.ExpectedDigest = strings.Repeat("0", 64)
			case "event":
				bad.EventID = uuid.New()
			}
			if _, err := store.ReplayDeadLetter(job("conflict"), bad); err == nil || snapshot() != before {
				t.Fatal("semantic key reuse changed durable facts")
			}
		}
	})

	t.Run("bad-digest-missing-envelope-and-generation-zero-effects", func(t *testing.T) {
		e, q, body := seed()
		before := snapshot()
		bad := q
		bad.ExpectedDigest = strings.Repeat("0", 64)
		if _, err := store.ReplayDeadLetter(job("bad-digest"), bad); err == nil || snapshot() != before {
			t.Fatal("bad digest wrote replay facts")
		}
		for _, mutate := range []func([]any){
			func(a []any) { a[2] = e.PublicationGeneration + 1 },
			func(a []any) { a[10] = nil },
			func(a []any) { a[3] = strings.Repeat("0", 64) },
			func(a []any) {
				var envelope map[string]any
				if err := json.Unmarshal(body, &envelope); err != nil {
					t.Fatal(err)
				}
				delete(envelope, "trace_id")
				wire, _ := json.Marshal(envelope)
				sum := sha256.Sum256(wire)
				a[3], a[10] = hex.EncodeToString(sum[:]), wire
			},
		} {
			p := args(e, q, body)
			mutate(p)
			if _, err := r.pool.Exec(ctx, capability, p...); err == nil || snapshot() != before {
				t.Fatal("malformed direct capability wrote facts")
			}
		}
	})

	t.Run("preexisting-runtime-receipt-is-not-enqueue-authority", func(t *testing.T) {
		e, q, body := seed()
		p := args(e, q, body)
		if _, err := r.pool.Exec(ctx, `INSERT INTO public.event_replay_requests(replay_id,event_id,event_digest,idempotency_key,request_hash,actor_type,service_name,job_type,job_run_id,reason,replayed_at) VALUES($1,$2,$3,$4,$5,'SERVICE',$6,$7,$8,$9,now())`, p[0], e.ID, q.ExpectedDigest, q.IdempotencyKey, p[5], identity.Service, identity.JobType, identity.JobRunID, q.Reason); err != nil {
			t.Fatal(err)
		}
		before := snapshot()
		var id, event uuid.UUID
		var replayed bool
		if err := r.pool.QueryRow(ctx, capability, p...).Scan(&id, &event, &replayed); err != nil || replayed || id != p[0] || snapshot() != before {
			t.Fatal("old/forged receipt acquired new generation authority")
		}
	})

	t.Run("wrong-runtime-and-private-map-acl", func(t *testing.T) {
		e, q, body := seed()
		before := snapshot()
		if _, err := r.pool.Exec(ctx, `SELECT * FROM account_approval_authorization.ordinary_replay_generations`); !errorCode(err, "42501") {
			t.Fatal("runtime can access private replay consumption evidence")
		}
		for _, role := range []string{"account_migrator", "account_approval_writer", "account_approval_backfill", "account_approval_relay"} {
			config := r.pool.Config().ConnConfig.Copy()
			config.User = role
			if incorrect, err := events.NewAccountReplayPostgresStore(ctx, ordinaryReplayFixtureDSN(config)); err == nil {
				incorrect.Close()
				t.Fatal("Account replay constructor accepted a different session identity")
			}
			connection, err := pgx.ConnectConfig(ctx, config)
			if err != nil {
				t.Fatal(err)
			}
			_, err = connection.Exec(ctx, capability, args(e, q, body)...)
			_ = connection.Close(ctx)
			if !errorCode(err, "42501") || snapshot() != before {
				t.Fatalf("wrong runtime %s enqueued event: %v", role, err)
			}
		}
		config := r.pool.Config().ConnConfig.Copy()
		config.User = "account_migrator"
		connection, err := pgx.ConnectConfig(ctx, config)
		if err != nil {
			t.Fatal(err)
		}
		defer connection.Close(context.Background())
		if _, err := connection.Exec(ctx, "SET ROLE account_service"); err != nil {
			t.Fatal(err)
		}
		if _, err := connection.Exec(ctx, capability, args(e, q, body)...); !errorCode(err, "42501") || snapshot() != before {
			t.Fatalf("SET ROLE bypassed actual session identity: %v", err)
		}
	})

	t.Run("authorization-event-is-never-replayable", func(t *testing.T) {
		// Seed the approved source facts, then use the actual legacy writer
		// prepare/finalize seam and its sidecar/commit guards. This is a controlled
		// authorization fixture, not a human approval or external proof claim.
		actor := seedEntitlementApprovalActor(t, ctx, r.pool, "PLATFORM_SUPER_ADMIN", "replay-deny")
		var binding uuid.UUID
		if err := r.pool.QueryRow(ctx, `SELECT id FROM role_assignments WHERE user_id=$1 AND role_code='PLATFORM_SUPER_ADMIN'`, actor.ID).Scan(&binding); err != nil {
			t.Fatal(err)
		}
		requestID, targetID, walletID, eventID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
		normalized := fmt.Sprintf(`{"adjustment_id":%q,"wallet_id":%q,"amount_microcredits":100,"requested_by":%q}`, targetID.String(), walletID.String(), actor.ID.String())
		financial := fmt.Sprintf(`{"wallet_id":%q,"delta_microcredits":100}`, walletID.String())
		if _, err := r.pool.Exec(ctx, `INSERT INTO approval_requests(id,command_type,policy_version,requester_user_id,requester_binding_id,requester_authorization_version,idempotency_key,normalized_command,command_fingerprint,target_resource_type,target_resource_id,reason,redacted_evidence,expected_impact,status,expires_at,execution_deadline,submitted_at,created_at,updated_at) VALUES($1,'WALLET_ADJUSTMENT',1,$2,$3,1,$4,$5,decode(repeat('00',32),'hex'),'MANUAL_ADJUSTMENT',$6,'controlled authorization replay refusal','[]','{}','APPROVED',now()+interval '1 hour',now()+interval '30 minutes',now(),now(),now())`, requestID, actor.ID, binding, "replay-auth-"+requestID.String(), normalized, targetID.String()); err != nil {
			t.Fatal(err)
		}
		if _, err := r.pool.Exec(ctx, `INSERT INTO approval_request_snapshots(approval_request_id,snapshot_schema_version,critical_resource_versions,financial_snapshot,policy_snapshot,requester_authorization_snapshot,created_at) VALUES($1,1,'{"adjustment_version":1}',$2,'{}','{}',now())`, requestID, financial); err != nil {
			t.Fatal(err)
		}
		config := r.pool.Config().ConnConfig.Copy()
		config.User = "account_approval_writer"
		writer, err := pgx.ConnectConfig(ctx, config)
		if err != nil {
			t.Fatal(err)
		}
		defer writer.Close(context.Background())
		tx, err := writer.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		control, err := LockAdminApprovalControlShared(ctx, tx)
		if err != nil {
			t.Fatal(err)
		}
		record, err := appendLegacyApprovalAuthorization(ctx, tx, control, requestID, eventID)
		if err != nil {
			t.Fatal("actual authorization writer fixture: ", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		before := snapshot()
		q := events.ReplayRequest{EventID: eventID, ExpectedDigest: record.EventDigest, IdempotencyKey: "authorization-deny-" + uuid.NewString(), Reason: "authorization events must never be replayed"}
		if _, err := store.ReplayDeadLetter(job("deny-authorization"), q); !errors.Is(err, events.ErrAuthorizationEventReplayForbidden) || snapshot() != before {
			t.Fatal("authorization event crossed the public replay type ban")
		}
		if _, err := r.pool.Exec(ctx, capability, args(events.Event{ID: eventID}, q, record.DeliveryBytes)...); !errorCode(err, "55000") || snapshot() != before {
			t.Fatal("direct capability bypassed persisted authorization event ban")
		}
	})

	t.Run("enqueue-and-receipt-rollback-together", func(t *testing.T) {
		e, q, body := seed()
		before := snapshot()
		tx, err := r.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, capability, args(e, q, body)...); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `SELECT 1/0`); err == nil {
			t.Fatal("controlled transaction failure was not triggered")
		}
		_ = tx.Rollback(ctx)
		if snapshot() != before {
			t.Fatal("rolled-back replay left generation or receipt")
		}
	})

	t.Run("concurrent-key-has-one-receipt-and-generation", func(t *testing.T) {
		e, q, _ := seed()
		start := make(chan struct{})
		var wg sync.WaitGroup
		results := make(chan error, 2)
		concurrentContext := job("concurrent")
		for range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, err := store.ReplayDeadLetter(concurrentContext, q)
				results <- err
			}()
		}
		close(start)
		wg.Wait()
		close(results)
		for err := range results {
			if err != nil && !errorCode(err, "40001") {
				t.Fatal(err)
			}
		}
		var generation int64
		var receipts, mapped int
		if err := operator.QueryRow(ctx, `SELECT publication_generation,(SELECT count(*) FROM public.event_replay_requests WHERE idempotency_key=$2),(SELECT count(*) FROM account_approval_authorization.ordinary_replay_generations WHERE event_id=$1) FROM public.outbox_events WHERE event_id=$1`, e.ID, q.IdempotencyKey).Scan(&generation, &receipts, &mapped); err != nil || generation != e.PublicationGeneration+1 || receipts != 1 || mapped != 1 {
			t.Fatal("concurrent stable key enqueued more than once")
		}
	})

	t.Run("nonempty-own-down-preserves-history", func(t *testing.T) {
		before := snapshot()
		sql, err := os.ReadFile("../../migrations/account/000094_ordinary_dead_letter_replay_runtime.down.sql")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := operator.Exec(ctx, "SET ROLE account_service"); err != nil {
			t.Fatal(err)
		}
		tx, err := operator.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, err = tx.Exec(ctx, string(sql))
		_ = tx.Rollback(ctx)
		if !errorCode(err, "55000") {
			t.Fatal("own Down erased consumed generation history")
		}
		if _, err := operator.Exec(ctx, "SET ROLE account_approval_authorization_owner"); err != nil || snapshot() != before {
			t.Fatal("rejected own Down changed durable history")
		}
	})
}
