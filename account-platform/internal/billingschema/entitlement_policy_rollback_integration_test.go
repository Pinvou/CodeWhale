package billingschema

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/jackc/pgx/v5/pgconn"
)

// Full-chain rollback cannot select 069 past later forward-only migrations.
// Exercise its script directly without deleting any unrelated history.
func TestF09EntitlementPolicyRollbackIsExplicitlyForwardOnly(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL")
	if raw == "" {
		t.Skip("explicit isolated PostgreSQL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	f := newStageFixture(t, ctx, raw)
	c := f.database(t, ctx, "account", "account_service")
	loaded, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	if err = accountschema.Up(ctx, c, loaded); err != nil {
		t.Fatal(err)
	}
	migrations.Unlock(ctx, c)
	var down string
	for _, m := range loaded {
		if m.Version == 69 {
			down = m.DownSQL
		}
	}
	if down == "" {
		t.Fatal("policy rollback SQL absent")
	}
	const snapshotSQL = `SELECT jsonb_build_object('policies',(SELECT jsonb_agg(to_jsonb(p) ORDER BY command_type,policy_version) FROM approval_command_policies p),'ledger',(SELECT jsonb_agg(to_jsonb(m) ORDER BY version) FROM schema_migrations m),'triggers',(SELECT jsonb_agg(pg_get_triggerdef(t.oid) ORDER BY t.tgname) FROM pg_trigger t WHERE t.tgrelid='approval_command_policies'::regclass AND NOT t.tgisinternal))::text`
	var before, after string
	if err := c.QueryRow(ctx, snapshotSQL).Scan(&before); err != nil {
		t.Fatal(err)
	}
	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(ctx, down)
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "55000" {
		t.Fatalf("policy rollback SQLSTATE expected55000 got=%v", err)
	}
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = migrations.VerifyApplied(ctx, c, loaded); err != nil {
		t.Fatal(err)
	}
	if err := c.QueryRow(ctx, snapshotSQL).Scan(&after); err != nil || before != after {
		t.Fatal("forward-only refusal altered immutable facts, ledger or trigger")
	}
	empty := f.database(t, ctx, "account", "account_service")
	_, err = empty.Exec(ctx, down)
	if !errors.As(err, &pe) || pe.Code != "55000" {
		t.Fatal("policy absence must not open rollback authority")
	}
	var relations int
	if err := empty.QueryRow(ctx, `SELECT count(*) FROM pg_class WHERE relnamespace='public'::regnamespace`).Scan(&relations); err != nil || relations != 0 {
		t.Fatal("empty-policy refusal created schema facts")
	}
	t.Log("explicit forward-only refusal preserves installed policy/ledger/trigger and has no effects without policy rows")
}
