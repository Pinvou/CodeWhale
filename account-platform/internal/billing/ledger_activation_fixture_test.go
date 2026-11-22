package billing

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// This is an explicit operator action in the fixture's owned UUID database.
// It grants no additional runtime authority and never changes global roles.
func ledgerActivationFixtureOperator(t *testing.T, ctx context.Context, pool *pgxpool.Pool) *pgx.Conn {
	t.Helper()
	config := pool.Config().ConnConfig.Copy()
	config.User = "billing_migrator"
	config.Password = ""
	operator, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal("cannot connect fixture activation operator")
	}
	t.Cleanup(func() { operator.Close(context.Background()) })
	var actor string
	var super bool
	if err := operator.QueryRow(ctx, `SELECT session_user,rolsuper FROM pg_catalog.pg_roles WHERE rolname=session_user`).Scan(&actor, &super); err != nil || actor != "billing_migrator" || super {
		t.Fatal("fixture activation operator must be the dedicated non-super migration identity")
	}
	return operator
}

func setLedgerActivationFixtureState(t *testing.T, ctx context.Context, operator *pgx.Conn, state string) int64 {
	t.Helper()
	if state != "ACTIVE" && state != "INACTIVE" {
		t.Fatal("unsupported fixture activation state")
	}
	tx, err := operator.Begin(ctx)
	if err != nil {
		t.Fatal("cannot begin fixture activation change")
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SET LOCAL ROLE billing_schema_owner`); err != nil {
		t.Fatal("fixture operator cannot assume the provisioned schema owner")
	}
	var version int64
	if err := tx.QueryRow(ctx, `UPDATE public.billing_approval_command_activations SET state=$1,version=version+1 WHERE command_type='LEDGER_REPAIR' AND policy_version=1 AND snapshot_schema_version=1 RETURNING version`, state).Scan(&version); err != nil || version < 2 {
		t.Fatal("fixture activation update must affect the exact installed Ledger contract")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal("fixture activation change did not commit")
	}
	return version
}
