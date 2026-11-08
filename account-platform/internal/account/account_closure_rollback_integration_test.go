package account

import (
	"context"
	"errors"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Complete public Up/Verify uses genuine owned deployment actors. The direct
// down invocation verifies 027 only, not the whole higher-version DownOne chain.
func TestAccountClosureRollbackPreservesEvidence(t *testing.T) {
	f := retainedWalletFenceActors(t)
	loaded, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	var down string
	for _, m := range loaded {
		if m.Version == 27 {
			down = m.DownSQL
		}
	}
	if down == "" {
		t.Fatal("missing 027 down")
	}
	reject := func(t *testing.T, ctx context.Context, conn *pgx.Conn) {
		t.Helper()
		_, err := conn.Exec(ctx, down)
		var failure *pgconn.PgError
		if !errors.As(err, &failure) || failure.Code != "55000" || failure.Message != "account closure saga is forward-only; preserve closure and approval evidence" {
			t.Fatalf("unexpected rollback result: %v", err)
		}
	}
	r := f.repositoryWithInitialStage(t, func(ctx context.Context, conn *pgx.Conn, _ []migrations.Migration) error {
		reject(t, ctx, conn)
		var absent bool
		if err := conn.QueryRow(ctx, `SELECT to_regclass('public.account_closure_requests') IS NULL AND to_regclass('public.approval_command_policies') IS NULL`).Scan(&absent); err != nil || !absent {
			t.Fatal("empty rollback changed schema")
		}
		return nil
	})
	snapshot := func(t *testing.T) string {
		t.Helper()
		var facts string
		err := r.pool.QueryRow(f.ctx, `SELECT jsonb_build_object(
   'ledger',(SELECT jsonb_agg(to_jsonb(x) ORDER BY version) FROM schema_migrations x),
   'policy',(SELECT jsonb_agg(to_jsonb(x) ORDER BY command_type,policy_version) FROM approval_command_policies x),
   'requests',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM account_closure_requests x),
   'commands',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM account_closure_commands x),
   'audit',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM account_closure_audit x),
   'users',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM users x),
   'triggers',(SELECT jsonb_agg(pg_get_triggerdef(oid) ORDER BY tgname) FROM pg_trigger WHERE NOT tgisinternal AND tgrelid IN ('approval_command_policies'::regclass,'account_closure_requests'::regclass,'account_closure_commands'::regclass,'account_closure_audit'::regclass)))::text`).Scan(&facts)
		if err != nil {
			t.Fatal(err)
		}
		return facts
	}
	check := func(t *testing.T) {
		t.Helper()
		before := snapshot(t)
		conn, err := r.pool.Acquire(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Release()
		reject(t, f.ctx, conn.Conn())
		conn.Release()
		if after := snapshot(t); after != before {
			t.Fatal("rollback changed ledger, policies, closure evidence or triggers")
		}
	}
	t.Run("installed-empty-closure", func(t *testing.T) { check(t) })
	user, request := uuid.New(), uuid.New()
	if _, err = r.pool.Exec(f.ctx, `INSERT INTO users(id,status,user_code,username,created_at,updated_at) VALUES($1,'CLOSING',generate_unique_user_code(),$2,now(),now())`, user, "rollback_"+user.String()); err != nil {
		t.Fatal(err)
	}
	if _, err = r.pool.Exec(f.ctx, `INSERT INTO account_closure_requests(id,user_id,idempotency_key_digest,request_fingerprint,status,stage,reauthenticated_at,sla_expires_at,created_at,updated_at) VALUES($1,$2,repeat('a',64),repeat('b',64),'IN_PROGRESS','CLOSURE_REQUESTED',now(),now()+interval '1 day',now(),now())`, request, user); err != nil {
		t.Fatal(err)
	}
	if _, err = r.pool.Exec(f.ctx, `INSERT INTO account_closure_commands(id,closure_request_id,actor_user_id,idempotency_key_digest,request_fingerprint,command_type,reason,created_at) VALUES($1,$2,$3,repeat('c',64),repeat('d',64),'RETRY_STAGE','retained rollback evidence',now())`, uuid.New(), request, user); err != nil {
		t.Fatal(err)
	}
	if _, err = r.pool.Exec(f.ctx, `INSERT INTO account_closure_audit(id,closure_request_id,event_type,actor_user_id,redacted_details,occurred_at) VALUES($1,$2,'ROLLBACK_FIXTURE',$3,'{"proof":"retained"}',now())`, uuid.New(), request, user); err != nil {
		t.Fatal(err)
	}
	t.Run("installed-with-evidence", func(t *testing.T) { check(t) })
}
