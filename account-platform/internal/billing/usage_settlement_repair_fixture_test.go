package billing

import (
	"context"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The runtime can only SELECT activation. Fixture setup uses its already-owned
// public-migration database and the existing migration actor, never broadening
// billing_service privileges or writing to a caller-provided database.
func usageRepairFixtureActivation(t *testing.T, ctx context.Context, pool *pgxpool.Pool) func(string) int64 {
	t.Helper()
	config := pool.Config().ConnConfig.Copy()
	if !strings.HasPrefix(config.Database, "retained_finance_") || len(strings.TrimPrefix(config.Database, "retained_finance_")) != 32 {
		t.Fatal("activation fixture requires the public helper's owned UUID database")
	}
	for _, c := range strings.TrimPrefix(config.Database, "retained_finance_") {
		if !strings.ContainsRune("0123456789abcdef", c) {
			t.Fatal("invalid owned UUID database")
		}
	}
	config.User, config.Password = "billing_migrator", ""
	connection, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal("cannot connect fixture activation actor")
	}
	t.Cleanup(func() { connection.Close(context.Background()) })
	var database, actor, owner string
	if err := connection.QueryRow(ctx, `SELECT current_database(),session_user,pg_get_userbyid(datdba) FROM pg_database WHERE datname=current_database()`).Scan(&database, &actor, &owner); err != nil || database != config.Database || actor != "billing_migrator" || owner != "billing_schema_owner" {
		t.Fatal("activation fixture database/actor/owner guard failed")
	}
	if _, err := connection.Exec(ctx, `SET ROLE billing_schema_owner`); err != nil {
		t.Fatal(err)
	}
	loaded, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrations.VerifyApplied(ctx, connection, loaded); err != nil {
		t.Fatal(err)
	}
	return func(state string) int64 {
		t.Helper()
		if state != "ACTIVE" && state != "INACTIVE" {
			t.Fatal("invalid fixture activation")
		}
		var version int64
		err := connection.QueryRow(ctx, `UPDATE billing_approval_command_activations SET state=$1,version=version+1 WHERE command_type='USAGE_SETTLEMENT_REPAIR' AND policy_version=1 AND state<>$1 RETURNING version`, state).Scan(&version)
		if errors.Is(err, pgx.ErrNoRows) {
			var current string
			err = connection.QueryRow(ctx, `SELECT state,version FROM billing_approval_command_activations WHERE command_type='USAGE_SETTLEMENT_REPAIR' AND policy_version=1`).Scan(&current, &version)
			if err != nil || current != state || version < 1 {
				t.Fatalf("unchanged fixture activation: %v", err)
			}
		} else if err != nil || version < 1 {
			t.Fatalf("set fixture activation: version=%d err=%v", version, err)
		}
		return version
	}
}
