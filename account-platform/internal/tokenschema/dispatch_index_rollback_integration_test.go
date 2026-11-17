package tokenschema

import (
	"context"
	"errors"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func testDispatchIndexPublicRollback(t *testing.T, ctx context.Context, connection *pgx.Conn, config *pgx.ConnConfig, loaded []migrations.Migration) {
	t.Helper()
	ordered, err := Ordered(loaded)
	if err != nil {
		t.Fatal(err)
	}
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Fatal("Go tool is required to exercise the shipped migration command", err)
	}
	binary := filepath.Join(t.TempDir(), "migrate.exe")
	build := exec.CommandContext(ctx, goTool, "build", "-tags=nomsgpack", "-mod=readonly", "-o", binary, "../../cmd/migrate")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build public migration command: %v\n%s", err, output)
	}
	// pgx ConnString retains the original parsed DSN, before this fixture sets
	// its owned database and migrator. Serialize those current fields explicitly.
	cliURL := url.URL{Scheme: "postgresql", User: url.User(config.User), Host: net.JoinHostPort(config.Host, strconv.Itoa(int(config.Port))), Path: "/" + config.Database, RawQuery: "sslmode=disable"}
	run := func(role, direction string) {
		t.Helper()
		command := exec.CommandContext(ctx, binary, "--dir", "../../migrations/token", "--direction", direction)
		command.Env = append(os.Environ(), "DATABASE_URL="+cliURL.String(), "MIGRATION_ROLE="+role, "PINVOU_DEPLOYMENT_ENVIRONMENT=test")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("public migration command role=%s direction=%s: %v\n%s", role, direction, err, output)
		}
	}

	var originalHead int64
	if err = connection.QueryRow(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&originalHead); err != nil || (originalHead != 900008 && originalHead != 900009) {
		t.Fatal("dispatch fixture requires its explicit public index heads", originalHead, err)
	}
	const laterIndexFacts = `SELECT count(*),coalesce(jsonb_agg(jsonb_build_object('definition',pg_get_indexdef(c.oid),'owner',pg_get_userbyid(c.relowner),'ready',i.indisready,'valid',i.indisvalid) ORDER BY c.relname),'[]'::jsonb)::text FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid WHERE i.indexrelid IN (to_regclass('public.usage_governance_account_credential_idx'),to_regclass('public.usage_governance_model_idx'))`
	const lowerSnapshot = `SELECT jsonb_build_object('ledger',(SELECT jsonb_agg(to_jsonb(m) ORDER BY version) FROM schema_migrations m WHERE version<>900009),'usage',(SELECT coalesce(jsonb_agg(to_jsonb(u) ORDER BY id),'[]'::jsonb) FROM usage_requests u),'evidence',(SELECT coalesce(jsonb_agg(to_jsonb(e) ORDER BY id),'[]'::jsonb) FROM usage_cancellation_evidence e),'dispatch_index',pg_get_indexdef('public.usage_cancellation_dispatch_fence_idx'::regclass))::text`
	var originalLaterFacts string
	if originalHead == 900009 {
		var count int
		if err = connection.QueryRow(ctx, laterIndexFacts).Scan(&count, &originalLaterFacts); err != nil || count != 2 {
			t.Fatal("current governance indexes must be present before their public rollback", count, err)
		}
		var before, after string
		if err = connection.QueryRow(ctx, lowerSnapshot).Scan(&before); err != nil {
			t.Fatal(err)
		}
		run("token_schema_owner", "down")
		var removedOnlyLater bool
		if err = connection.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM schema_migrations WHERE version=900009) AND EXISTS(SELECT 1 FROM schema_migrations WHERE version=900008) AND to_regclass('public.usage_governance_account_credential_idx') IS NULL AND to_regclass('public.usage_governance_model_idx') IS NULL AND to_regclass('public.usage_cancellation_dispatch_fence_idx') IS NOT NULL`).Scan(&removedOnlyLater); err != nil || !removedOnlyLater {
			t.Fatal("first owner CLI Down must retire only the newer governance indexes", err)
		}
		if err = connection.QueryRow(ctx, lowerSnapshot).Scan(&after); err != nil || before != after {
			t.Fatal("newer index rollback changed lower ledger, usage, evidence or dispatch index", err)
		}
	}

	// Retained facts include one indexed row and both kinds of nonmatching row.
	// Use the public schema and current runtime role; do not grant fixture DDL
	// privileges or disable the evidence table's immutable-record trigger.
	controlledUsage := nextIndexRollbackUsageID(t, ctx, connection)
	var usageID string
	var seeded, matching int
	err = connection.QueryRow(ctx, `WITH account AS (
INSERT INTO token_accounts(id,owner_type,owner_id,wallet_id,name,status,default_priority,created_by,created_at,updated_at,management_mode)
VALUES(gen_random_uuid(),'USER',gen_random_uuid(),gen_random_uuid(),'dispatch index retained fixture','ACTIVE',100,gen_random_uuid(),'2026-01-01','2026-01-01','PLATFORM')
RETURNING id,owner_id,wallet_id), credential AS (
INSERT INTO provider_credentials(id,token_account_id,provider,base_url,ciphertext,key_id,fingerprint,version,status,allowed_models,created_by,created_at)
SELECT gen_random_uuid(),id,'controlled','https://provider.invalid',decode('0102','hex'),'fixture-key',repeat('a',64),1,'ACTIVE',ARRAY['model'],owner_id,'2026-01-01' FROM account
RETURNING id,token_account_id), request AS (
INSERT INTO usage_requests(id,idempotency_key_digest,request_fingerprint,user_id,session_id,token_account_id,credential_id,wallet_id,model,status,maximum_microcredits,request_snapshot,started_at,updated_at,canonicalization_version,execution_status,cancel_status,cancel_requested_at)
SELECT $1::uuid,repeat('b',64),repeat('b',64),account.owner_id,'dispatch-index-fixture',account.id,credential.id,account.wallet_id,'model','CREATED',100,'{"context_type":"PERSONAL"}','2026-01-01','2026-01-01',1,'RECEIVED','CANCEL_REQUESTED','2026-01-01'
FROM account JOIN credential ON credential.token_account_id=account.id RETURNING id), evidence AS (
INSERT INTO usage_cancellation_evidence(id,usage_request_id,evidence_type,adapter_version,sanitized_evidence,occurred_at)
SELECT gen_random_uuid(),request.id,facts.evidence_type,facts.adapter_version,'{}'::jsonb,'2026-01-01'::timestamptz
FROM request CROSS JOIN (VALUES ('CANCEL_REQUESTED','hold-expiry-v1'),('CANCEL_REQUESTED','fixture-other-v1'),('PROVIDER_CONFIRMED','hold-expiry-v1')) AS facts(evidence_type,adapter_version)
RETURNING usage_request_id)
SELECT min(usage_request_id::text),count(*) FROM evidence`, controlledUsage).Scan(&usageID, &seeded)
	if err != nil || seeded != 3 {
		t.Fatal("seed retained matching and nonmatching cancellation evidence", seeded, err)
	}
	if err = connection.QueryRow(ctx, `SELECT count(*) FROM usage_cancellation_evidence WHERE usage_request_id=$1::uuid AND adapter_version='hold-expiry-v1' AND evidence_type='CANCEL_REQUESTED'`, usageID).Scan(&matching); err != nil || matching != 1 {
		t.Fatal("retained fixture must have exactly one dispatch-index match", matching, err)
	}
	// Advance the public history worker's scan cursor over the new request
	// before the parent fixture checks that its backfill has no remaining work.
	// Keep that original zero-work authority assertion unchanged.
	var scanned int
	var projectionComplete bool
	if err = connection.QueryRow(ctx, `SELECT processed,complete FROM public.backfill_usage_history_projection(100)`).Scan(&scanned, &projectionComplete); err != nil || scanned != 1 || !projectionComplete {
		t.Fatal("scan retained request through the public history worker", scanned, projectionComplete, err)
	}
	const evidenceSnapshot = `SELECT coalesce(jsonb_agg(to_jsonb(e) ORDER BY id),'[]'::jsonb)::text FROM usage_cancellation_evidence e`
	var retainedEvidence string
	if err = connection.QueryRow(ctx, evidenceSnapshot).Scan(&retainedEvidence); err != nil {
		t.Fatal(err)
	}
	const snapshot = `SELECT jsonb_build_object('ledger',(SELECT jsonb_agg(to_jsonb(m) ORDER BY version) FROM schema_migrations m),'evidence',(SELECT coalesce(jsonb_agg(to_jsonb(e) ORDER BY id),'[]'::jsonb) FROM usage_cancellation_evidence e),'index',pg_get_indexdef('public.usage_cancellation_dispatch_fence_idx'::regclass))::text`
	var before, after string
	if err = connection.QueryRow(ctx, snapshot).Scan(&before); err != nil {
		t.Fatal(err)
	}
	// This is the exact DownOne branch called by cmd/migrate. The normal Up
	// identity must still fail closed, without changing the ledger or evidence.
	err = migrations.DownOne(ctx, connection, ordered)
	migrations.Unlock(ctx, connection)
	var denied *pgconn.PgError
	if !errors.As(err, &denied) || denied.Code != "42501" {
		t.Fatal("runtime-selected rollback must require the index owner", err)
	}
	if err = connection.QueryRow(ctx, snapshot).Scan(&after); err != nil || before != after {
		t.Fatal("denied rollback changed persistent facts", err)
	}
	var retainedVersions []int64
	if err = connection.QueryRow(ctx, `SELECT array_agg(version ORDER BY version) FROM schema_migrations WHERE version<>900008`).Scan(&retainedVersions); err != nil {
		t.Fatal(err)
	}
	var retained string
	if err = connection.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m WHERE version<>900008`).Scan(&retained); err != nil {
		t.Fatal(err)
	}
	run("token_schema_owner", "down")
	var removed bool
	if err = connection.QueryRow(ctx, `SELECT to_regclass('public.usage_cancellation_dispatch_fence_idx') IS NULL AND NOT EXISTS(SELECT 1 FROM schema_migrations WHERE version=900008)`).Scan(&removed); err != nil || !removed {
		t.Fatal("controlled CLI rollback did not remove exactly the new index identity", err)
	}
	var current string
	if err = connection.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m`).Scan(&current); err != nil || current != retained {
		t.Fatal("controlled rollback changed predecessor ledger", err)
	}
	if err = connection.QueryRow(ctx, evidenceSnapshot).Scan(&current); err != nil || current != retainedEvidence {
		t.Fatal("controlled rollback changed retained cancellation evidence", err)
	}
	run("token_gateway", "up")
	if err = connection.QueryRow(ctx, snapshot).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if err = connection.QueryRow(ctx, evidenceSnapshot).Scan(&current); err != nil || current != retainedEvidence {
		t.Fatal("reinstallation changed retained cancellation evidence", err)
	}
	// Ledger installation timestamps legitimately change on re-install. Check
	// original rows separately and verify every current published SQL identity.
	if err = connection.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m WHERE version=ANY($1)`, retainedVersions).Scan(&current); err != nil || current != retained {
		t.Fatal("reinstallation changed predecessor ledger", err)
	}
	verified, err := VerifyApplied(ctx, connection, loaded)
	if err != nil || verified.AppliedCount != len(loaded) {
		t.Fatal("reinstalled full public package failed identity verification", verified, err)
	}
	if originalHead == 900009 {
		var count int
		var restored string
		if err = connection.QueryRow(ctx, laterIndexFacts).Scan(&count, &restored); err != nil || count != 2 || restored != originalLaterFacts {
			t.Fatal("full public Up did not restore exact governance index definitions and owners", count, err)
		}
	}
	t.Log("token_migrator session: gateway Down denied atomically; owner CLI Down and gateway CLI Up passed")
}

// Each helper appends one legacy scan row to this isolated database. PostgreSQL
// scans UUID order, so insertion time and independent random UUIDs cannot prove
// that the next row lies beyond the cursor left by the preceding helper.
func nextIndexRollbackUsageID(t *testing.T, ctx context.Context, connection *pgx.Conn) uuid.UUID {
	t.Helper()
	var text string
	if err := connection.QueryRow(ctx, `SELECT id::text FROM (
SELECT id FROM usage_requests
UNION ALL SELECT last_usage_request_id FROM usage_history_projection_state WHERE singleton
UNION ALL SELECT '00000000-0000-0000-0000-000000000000'::uuid
) candidates WHERE id IS NOT NULL ORDER BY id DESC LIMIT 1`).Scan(&text); err != nil {
		t.Fatal(err)
	}
	id, err := uuid.Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	for i := len(id) - 1; i >= 0; i-- {
		id[i]++
		if id[i] != 0 {
			return id
		}
	}
	t.Fatal("isolated index rollback fixture exhausted UUID order")
	return uuid.Nil
}
