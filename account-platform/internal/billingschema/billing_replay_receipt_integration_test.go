package billingschema

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// This fixture owns its actors/databases, invokes the public Billing migrator
// and the real operator executable. It never starts a relay or publishes NATS.
func TestBillingOrdinaryReplayOriginalMigrationPostgres(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL")
	if raw == "" {
		t.Skip("actual exclusive PostgreSQL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()
	f := newStageFixture(t, ctx, raw)
	loaded, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	var original migrations.Migration
	var without, through32 []migrations.Migration
	for _, m := range loaded {
		if m.Version == 32 {
			original = m
		} else {
			without = append(without, m)
		}
		if m.Version <= 32 || m.Version == generationVersion || m.Version == intentVersion || m.Version == memberIdentityVersion || m.Version == 900012 {
			through32 = append(through32, m)
		}
	}
	if original.Name != "event_replay_audit" || original.Checksum != "733b1f380b9f4cc848c28fabefa4621e42db67466bcb2fba9b4fbbcf4d677a61" || sqlChecksum(original.DownSQL) != "c266dec0a7c9ec8750018db70108a3dff568c9f3f80d7df3fe3eb5a1825b8762" {
		t.Fatal("original032 identity changed")
	}
	goTool := filepath.Join(runtime.GOROOT(), "bin", "go")
	binary := filepath.Join(t.TempDir(), "event-replay")
	if runtime.GOOS == "windows" {
		goTool += ".exe"
		binary += ".exe"
	}
	build := exec.CommandContext(ctx, goTool, "build", "-p", "2", "-o", binary, "./cmd/event-replay")
	build.Dir = "../.."
	if output, e := build.CombinedOutput(); e != nil {
		t.Fatalf("build actual event-replay failed: %v %s", e, output)
	}
	runReplay := func(url string, args ...string) ([]byte, error) {
		command := exec.CommandContext(ctx, binary, args...)
		command.Env = append(os.Environ(), "DATABASE_URL="+url)
		return command.CombinedOutput()
	}
	for _, kind := range []string{"current-hardened-missing032", "original032-already-installed-LF", "original032-already-installed-CRLF"} {
		t.Run(kind, func(t *testing.T) {
			c := f.database(t, ctx, "billing", "billing_schema_owner")
			// Apply the shipped administrator bootstrap, not a permissive replacement.
			sqlPath, err := filepath.Abs("../../deploy/postgres/billing-roles.sql")
			if err != nil {
				t.Fatal(err)
			}
			bootstrap := exec.CommandContext(ctx, "psql", "-X", "-v", "ON_ERROR_STOP=1", "-v", "billing_database="+c.Config().Database, "-f", sqlPath)
			bootstrap.Env = append(os.Environ(), "PGHOST="+f.config.Host, "PGPORT="+strconv.Itoa(int(f.config.Port)), "PGUSER="+f.config.User, "PGDATABASE="+c.Config().Database, "PGPASSWORD="+f.config.Password)
			if output, e := bootstrap.CombinedOutput(); e != nil {
				t.Fatalf("public Billing roles failed: %v %s", e, output)
			}
			var oldReceipt uuid.UUID
			var oldFacts, oldLedger, precedingEvents, precedingLedger string
			if kind == "current-hardened-missing032" {
				if err := Up(ctx, c, without); err != nil {
					t.Fatal(err)
				}
				var absent bool
				if err := c.QueryRow(ctx, `SELECT to_regclass('public.event_replay_requests') IS NULL`).Scan(&absent); err != nil || !absent {
					t.Fatal("032 unexpectedly supplied", err)
				}

				preID := uuid.New()
				if _, err := c.Exec(ctx, `INSERT INTO outbox_events(event_id,event_type,aggregate_id,aggregate_version,payload,trace_id,occurred_at,dead_lettered_at) VALUES($1,'billing.recharge.succeeded.v1',$2,1,'{"fixture":"pre-restoration-only"}','owned-pre-restoration',now(),now())`, preID, uuid.New()); err != nil {
					t.Fatal(err)
				}
				precedingEvents = stageValue(t, ctx, c, `SELECT jsonb_agg(to_jsonb(e) ORDER BY event_id)::text FROM outbox_events e`)
				precedingLedger = stageValue(t, ctx, c, `SELECT jsonb_agg(to_jsonb(s) ORDER BY version)::text FROM schema_migrations s`)
				t.Run("pre-restoration-real-CLI-refusal-zero-effects", func(t *testing.T) {
					url := f.serviceURL(t, c, "billing_service")
					output, e := runReplay(url, "--event-id", preID.String(), "--inspect")
					var inspected events.DeadLetterCandidate
					if e != nil || json.Unmarshal(output, &inspected) != nil || !inspected.Replayable {
						t.Fatal("baseline inspect failed", e)
					}
					if _, e := runReplay(url, "--event-id", preID.String(), "--expected-digest", inspected.EventDigest, "--idempotency-key", "owned-pre-restoration-replay-key", "--reason", "owned reviewed incident before restoring original audit carrier", "--confirm"); e == nil {
						t.Fatal("missing audit carrier unexpectedly queued event")
					}
					if precedingEvents != stageValue(t, ctx, c, `SELECT jsonb_agg(to_jsonb(e) ORDER BY event_id)::text FROM outbox_events e`) || precedingLedger != stageValue(t, ctx, c, `SELECT jsonb_agg(to_jsonb(s) ORDER BY version)::text FROM schema_migrations s`) {
						t.Fatal("baseline failed CLI changed events or ledger")
					}
				})
			} else {
				if err := Up(ctx, c, through32); err != nil {
					t.Fatal(err)
				}
				oldEvent, oldAudit := uuid.New(), uuid.New()
				oldReceipt = oldAudit
				if _, err := c.Exec(ctx, `INSERT INTO outbox_events(event_id,event_type,aggregate_id,aggregate_version,payload,trace_id,occurred_at) VALUES($1,'billing.recharge.succeeded.v1',$2,1,'{"fixture":"old-no-financial-effects"}','owned-replay-fixture',now())`, oldEvent, uuid.New()); err != nil {
					t.Fatal(err)
				}
				if _, err := c.Exec(ctx, `INSERT INTO event_replay_requests(replay_id,event_id,event_digest,idempotency_key,request_hash,actor_type,service_name,job_type,job_run_id,reason,replayed_at) VALUES($1,$2,repeat('a',64),'original-installed-receipt-key',repeat('b',64),'SERVICE','event-replay','EVENT_REPLAY','original-job','original reviewed operator recovery reason',now())`, oldAudit, oldEvent); err != nil {
					t.Fatal(err)
				}
				if strings.HasSuffix(kind, "CRLF") {
					sum := sha256.Sum256([]byte(strings.ReplaceAll(original.UpSQL, "\n", "\r\n")))
					if _, err := c.Exec(ctx, `SET ROLE billing_schema_owner`); err != nil {
						t.Fatal(err)
					}
					if _, err := c.Exec(ctx, `UPDATE schema_migrations SET checksum=$1 WHERE version=32`, hex.EncodeToString(sum[:])); err != nil {
						t.Fatal(err)
					}
					if _, err := c.Exec(ctx, `SET ROLE billing_service`); err != nil {
						t.Fatal(err)
					}
				}
				oldFacts = stageValue(t, ctx, c, `SELECT to_jsonb(r)::text FROM event_replay_requests r WHERE replay_id='`+oldAudit.String()+`'`)
				oldLedger = stageValue(t, ctx, c, `SELECT to_jsonb(s)::text FROM schema_migrations s WHERE version=32`)
			}
			if err := Up(ctx, c, loaded); err != nil {
				t.Fatal(err)
			}
			if _, err := VerifyApplied(ctx, c, loaded); err != nil {
				t.Fatal("full exact migration history", err)
			}
			if precedingEvents != "" {
				if precedingEvents != stageValue(t, ctx, c, `SELECT jsonb_agg(to_jsonb(e) ORDER BY event_id)::text FROM outbox_events e`) {
					t.Fatal("late032 changed preceding outbox facts")
				}
				if precedingLedger != stageValue(t, ctx, c, `SELECT jsonb_agg(to_jsonb(s) ORDER BY version)::text FROM schema_migrations s WHERE version<>32`) {
					t.Fatal("late032 rewrote existing ledger entries")
				}
			}
			if oldReceipt != uuid.Nil {
				if oldFacts != stageValue(t, ctx, c, `SELECT to_jsonb(r)::text FROM event_replay_requests r WHERE replay_id='`+oldReceipt.String()+`'`) || oldLedger != stageValue(t, ctx, c, `SELECT to_jsonb(s)::text FROM schema_migrations s WHERE version=32`) {
					t.Fatal("existing032 audit or ledger mutated")
				}
			}
			runtimeURL := f.serviceURL(t, c, "billing_service")
			actor, err := pgx.Connect(ctx, runtimeURL)
			if err != nil {
				t.Fatal("actual Billing runtime login", err)
			}
			defer actor.Close(context.Background())
			var session, current, owner string
			var super, ownerMembership, ddl bool
			if err := actor.QueryRow(ctx, `SELECT session_user,current_user,(SELECT pg_get_userbyid(relowner) FROM pg_class WHERE oid='public.event_replay_requests'::regclass),(SELECT rolsuper FROM pg_roles WHERE rolname=current_user),pg_has_role(current_user,'billing_schema_owner','MEMBER'),has_schema_privilege(current_user,'public','CREATE')`).Scan(&session, &current, &owner, &super, &ownerMembership, &ddl); err != nil || session != "billing_service" || current != "billing_service" || owner != "billing_schema_owner" || super || ownerMembership || ddl {
				t.Fatal("runtime authority changed", err)
			}
			eventID := uuid.New()
			if _, err := actor.Exec(ctx, `INSERT INTO outbox_events(event_id,event_type,aggregate_id,aggregate_version,payload,trace_id,occurred_at,attempts,last_error,dead_lettered_at,locked_by,locked_until) VALUES($1,'billing.recharge.succeeded.v1',$2,1,'{"fixture":"controlled-no-financial-effects"}','owned-replay-fixture',now(),8,'controlled-fixture-deadletter',now(),'old-relay',now()-interval '1 minute')`, eventID, uuid.New()); err != nil {
				t.Fatal(err)
			}
			immutable := stageValue(t, ctx, actor, `SELECT jsonb_build_array(event_id,event_type,aggregate_id,aggregate_version,payload,trace_id,occurred_at)::text FROM outbox_events WHERE event_id='`+eventID.String()+`'`)
			snapshot := func() string {
				return stageValue(t, ctx, actor, `SELECT jsonb_build_object('events',(SELECT jsonb_agg(to_jsonb(e) ORDER BY event_id) FROM outbox_events e),'receipts',(SELECT COALESCE(jsonb_agg(to_jsonb(r) ORDER BY replay_id),'[]') FROM event_replay_requests r),'ledger',(SELECT jsonb_agg(to_jsonb(s) ORDER BY version) FROM schema_migrations s),'funds',jsonb_build_array((SELECT count(*) FROM wallets),(SELECT count(*) FROM journal_entries),(SELECT count(*) FROM ledger_postings)))::text`)
			}
			run := func(args ...string) ([]byte, error) { return runReplay(runtimeURL, args...) }
			before := snapshot()
			inspected, e := run("--event-id", eventID.String(), "--inspect")
			if e != nil {
				t.Fatalf("actual CLI inspect failed: %v %s", e, inspected)
			}
			var candidate events.DeadLetterCandidate
			if json.Unmarshal(inspected, &candidate) != nil || candidate.EventID != eventID || !candidate.Replayable || len(candidate.EventDigest) != 64 || strings.Contains(string(inspected), "controlled-no-financial-effects") || snapshot() != before {
				t.Fatal("inspect changed facts or exposed payload")
			}
			key := "owned-billing-replay-" + uuid.NewString()
			reason := "owned fixture incident reviewed by a second operator; no funds delivery"
			base := []string{"--event-id", eventID.String(), "--expected-digest", candidate.EventDigest, "--idempotency-key", key, "--reason", reason, "--confirm"}
			t.Run("wrong-digest-and-missing-confirm-zero-effects", func(t *testing.T) {
				bad := append([]string(nil), base...)
				bad[3] = strings.Repeat("0", 64)
				if _, err := run(bad...); err == nil || snapshot() != before {
					t.Fatal("wrong digest caused effects or success")
				}
				if _, err := run(base[:len(base)-1]...); err == nil || snapshot() != before {
					t.Fatal("missing confirm caused effects or success")
				}
			})
			store, err := events.NewPostgresStore(ctx, runtimeURL)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			if _, err := store.ReplayDeadLetter(ctx, events.ReplayRequest{EventID: eventID, ExpectedDigest: candidate.EventDigest, IdempotencyKey: key, Reason: reason}); err == nil || snapshot() != before {
				t.Fatal("missing JOB identity accepted")
			}
			// Human review remains an operational prerequisite. The existing CLI records
			// its JOB identity/reason, not a fabricated cryptographic second-person proof.
			output, e := run(base...)
			if e != nil || !strings.Contains(string(output), "committed=true") {
				t.Fatalf("actual CLI confirm failed: %v %s", e, output)
			}
			var generation int64
			var count, attempts int
			var cleared bool
			var auditID uuid.UUID
			var digest, recordReason, actorType, service, jobType, jobID string
			if err := actor.QueryRow(ctx, `SELECT o.publication_generation,o.attempts,o.dead_lettered_at IS NULL AND o.locked_by IS NULL AND o.locked_until IS NULL,(SELECT count(*) FROM event_replay_requests WHERE idempotency_key=$2),r.replay_id,r.event_digest,r.reason,r.actor_type,r.service_name,r.job_type,r.job_run_id FROM outbox_events o JOIN event_replay_requests r ON r.event_id=o.event_id AND r.idempotency_key=$2 WHERE o.event_id=$1`, eventID, key).Scan(&generation, &attempts, &cleared, &count, &auditID, &digest, &recordReason, &actorType, &service, &jobType, &jobID); err != nil || generation != 1 || attempts != 0 || !cleared || count != 1 || auditID == uuid.Nil || digest != candidate.EventDigest || recordReason != reason || actorType != "SERVICE" || service != "event-replay" || jobType != "EVENT_REPLAY" || jobID == "" {
				t.Fatal("original event/audit/generation mismatch", err)
			}
			if immutable != stageValue(t, ctx, actor, `SELECT jsonb_build_array(event_id,event_type,aggregate_id,aggregate_version,payload,trace_id,occurred_at)::text FROM outbox_events WHERE event_id='`+eventID.String()+`'`) {
				t.Fatal("immutable event identity changed")
			}
			committed := snapshot()
			t.Run("original-key-replay-and-changed-facts-zero-new-effects", func(t *testing.T) {
				output, e := run(base...)
				if e != nil || !strings.Contains(string(output), "committed=false") || !strings.Contains(string(output), auditID.String()) || snapshot() != committed {
					t.Fatal("original key replay changed receipt or generation", e)
				}
				bad := append([]string(nil), base...)
				bad[7] = "changed original reviewed operator intent"
				if _, err := run(bad...); err == nil || snapshot() != committed {
					t.Fatal("changed reason/key tuple accepted")
				}
			})
			t.Run("immutable-runtime-audit-and-owner-boundary", func(t *testing.T) {
				for _, q := range []string{`UPDATE event_replay_requests SET reason=reason||'tamper'`, `DELETE FROM event_replay_requests`, `TRUNCATE event_replay_requests`, `ALTER TABLE event_replay_requests ADD COLUMN unsafe text`, `SET ROLE billing_schema_owner`} {
					_, err := actor.Exec(ctx, q)
					var pg *pgconn.PgError
					if err == nil || !errors.As(err, &pg) || (pg.Code != "P0001" && pg.Code != "55000" && pg.Code != "42501") || snapshot() != committed {
						t.Fatal("runtime mutation/owner boundary not enforced", q, err)
					}
				}
			})
			t.Run("authorization-event-confirm-permanently-forbidden", func(t *testing.T) {
				authID := uuid.New()
				if _, err := actor.Exec(ctx, `INSERT INTO outbox_events(event_id,event_type,aggregate_id,aggregate_version,payload,trace_id,occurred_at,dead_lettered_at) VALUES($1,'account.approval-command.authorized.v1',$2,1,'{}','owned-negative',now(),now())`, authID, uuid.New()); err != nil {
					t.Fatal(err)
				}
				authBefore := snapshot()
				authArgs := append([]string(nil), base...)
				authArgs[1] = authID.String()
				authArgs[5] = "owned-forbidden-authorization-key"
				if output, err := run(authArgs...); err == nil || snapshot() != authBefore {
					t.Fatal("authorization replay escaped original prohibition", string(output))
				}
			})
		})
	}
}
