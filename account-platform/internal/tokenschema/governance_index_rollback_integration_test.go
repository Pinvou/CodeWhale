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

func testGovernanceIndexPublicRollback(t *testing.T, ctx context.Context, connection *pgx.Conn, config *pgx.ConnConfig, loaded []migrations.Migration) func(*testing.T) {
	t.Helper()
	ordered, err := Ordered(loaded)
	if err != nil {
		t.Fatal(err)
	}
	account, credential, actor, wallet := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	usage := nextIndexRollbackUsageID(t, ctx, connection)
	if _, err := connection.Exec(ctx, `INSERT INTO token_accounts(id,owner_type,owner_id,wallet_id,name,status,default_priority,created_by,created_at,updated_at,management_mode) VALUES($1,'USER',$2,$3,'controlled index rollback','ACTIVE',100,$2,now(),now(),'PLATFORM')`, account, actor, wallet); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(ctx, `INSERT INTO provider_credentials(id,token_account_id,provider,base_url,ciphertext,key_id,fingerprint,version,status,allowed_models,created_by,created_at) VALUES($1,$2,'controlled','https://provider.invalid','controlled-ciphertext','controlled',repeat('a',64),1,'ACTIVE',ARRAY['controlled'],$3,now())`, credential, account, actor); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(ctx, `INSERT INTO usage_requests(id,user_id,session_id,token_account_id,credential_id,wallet_id,model,status,maximum_microcredits,started_at,updated_at,request_snapshot,request_fingerprint,idempotency_key_digest) VALUES($1,$2,'controlled-session',$3,$4,$5,'controlled','CREATED',1,now(),now(),'{}',repeat('b',64),repeat('c',64))`, usage, actor, account, credential, wallet); err != nil {
		t.Fatal(err)
	}
	// Materialize the one controlled legacy row through the existing worker.
	// Later authority checks retain their original idle-backfill precondition.
	var processed int
	var complete bool
	if err := connection.QueryRow(ctx, `SELECT processed,complete FROM public.backfill_usage_history_projection(100)`).Scan(&processed, &complete); err != nil || processed != 1 || !complete {
		t.Fatal("controlled legacy history projection did not complete", processed, complete, err)
	}
	const usageSnapshot = `SELECT jsonb_agg(to_jsonb(request) ORDER BY id)::text FROM usage_requests request`
	var retainedUsage string
	if err := connection.QueryRow(ctx, usageSnapshot).Scan(&retainedUsage); err != nil {
		t.Fatal(err)
	}
	var retainedUsageIDs []uuid.UUID
	if err = connection.QueryRow(ctx, `SELECT array_agg(id ORDER BY id) FROM usage_requests`).Scan(&retainedUsageIDs); err != nil {
		t.Fatal(err)
	}
	assertRetainedUsage := func(t *testing.T, originalScopeOnly bool) {
		t.Helper()
		var current string
		query, args := usageSnapshot, []any{}
		if originalScopeOnly {
			query += " WHERE id=ANY($1::uuid[])"
			args = append(args, retainedUsageIDs)
		}
		if err := connection.QueryRow(ctx, query, args...).Scan(&current); err != nil || current != retainedUsage {
			t.Fatal("index lifecycle changed retained usage rows", err)
		}
	}
	const snapshot = `SELECT jsonb_build_object('ledger',(SELECT jsonb_agg(to_jsonb(m) ORDER BY version) FROM schema_migrations m),'usage',(SELECT coalesce(jsonb_agg(to_jsonb(e) ORDER BY id),'[]'::jsonb) FROM usage_requests e),'account_index',pg_get_indexdef('public.usage_governance_account_credential_idx'::regclass),'model_index',pg_get_indexdef('public.usage_governance_model_idx'::regclass))::text`
	var before, after string
	if err = connection.QueryRow(ctx, snapshot).Scan(&before); err != nil {
		t.Fatal(err)
	}
	// This is the exact DownOne branch called by cmd/migrate. The normal Up
	// identity must still fail closed, without changing the ledger or usage.
	err = migrations.DownOne(ctx, connection, ordered)
	migrations.Unlock(ctx, connection)
	var denied *pgconn.PgError
	if !errors.As(err, &denied) || denied.Code != "42501" {
		t.Fatal("runtime-selected rollback must require the index owner", err)
	}
	if err = connection.QueryRow(ctx, snapshot).Scan(&after); err != nil || before != after {
		t.Fatal("denied rollback changed persistent usage facts", err)
	}
	var retained string
	if err = connection.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m WHERE version<>900009`).Scan(&retained); err != nil {
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
	run("token_schema_owner", "down")
	assertRetainedUsage(t, false)
	var removed bool
	if err = connection.QueryRow(ctx, `SELECT to_regclass('public.usage_governance_account_credential_idx') IS NULL AND to_regclass('public.usage_governance_model_idx') IS NULL AND NOT EXISTS(SELECT 1 FROM schema_migrations WHERE version=900009)`).Scan(&removed); err != nil || !removed {
		t.Fatal("controlled CLI rollback did not remove exactly the new indexes and identity", err)
	}
	var current string
	if err = connection.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m`).Scan(&current); err != nil || current != retained {
		t.Fatal("controlled rollback changed predecessor ledger", err)
	}
	// Down 9 must retain 8; only the later combined Down 8/full Up may
	// legitimately reinstall that index ledger row. Keep both exact snapshots.
	var stablePredecessor string
	if err = connection.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m WHERE version NOT IN (900008,900009,900010)`).Scan(&stablePredecessor); err != nil {
		t.Fatal(err)
	}
	// The following dispatch-index phase performs the one full current CLI Up.
	return func(t *testing.T) {
		t.Helper()
		assertRetainedUsage(t, true)
		if err = connection.QueryRow(ctx, snapshot).Scan(&after); err != nil {
			t.Fatal(err)
		}
		// Ledger installation timestamps legitimately change on re-install. Check
		// original rows separately and verify every current published SQL identity.
		if err = connection.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m WHERE version NOT IN (900008,900009,900010)`).Scan(&current); err != nil || current != stablePredecessor {
			t.Fatal("reinstallation changed predecessor ledger", err)
		}
		if _, err = VerifyApplied(ctx, connection, loaded); err != nil {
			t.Fatal("reinstalled public package failed identity verification", err)
		}
		t.Log("token_migrator session: gateway Down denied atomically; owner CLI Down and gateway CLI Up passed")
	}
}
