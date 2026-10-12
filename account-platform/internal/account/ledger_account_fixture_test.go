package account

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The cross-domain formal tests retain their existing assertions. Their helper
// uses the public Account migration wrapper and genuine migrator/runtime/writer/
// relay sessions. Fixed actors must already be provisioned by the owned runner;
// this helper neither creates nor alters cluster actors.
func newTokenOwnerCandidateIntegrationRepository(t *testing.T, ctx context.Context, databaseURL string, maximumVersion int64) (*PostgresRepository, *pgx.Conn, *ApprovalRelayStore, []migrations.Migration) {
	t.Helper()
	base, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal("parse dedicated fixture configuration")
	}
	admin, err := pgx.ConnectConfig(ctx, base)
	if err != nil {
		t.Fatal("open dedicated fixture coordinator")
	}
	roles := []string{"account_service", "account_migrator", "account_approval_control_owner", "account_approval_authorization_owner", "account_approval_writer", "account_approval_backfill", "account_approval_relay", "account_approval_recovery"}
	var actorCount int
	if err = admin.QueryRow(ctx, `SELECT count(*) FROM pg_roles WHERE rolname=ANY($1) AND NOT rolsuper AND NOT rolcreatedb AND NOT rolcreaterole AND NOT rolreplication AND NOT rolbypassrls`, roles).Scan(&actorCount); err != nil || actorCount != len(roles) {
		_ = admin.Close(context.Background())
		t.Fatal("owned runner must provision all finite non-superuser Account actors")
	}
	name := "account_ledger_fixture_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err = admin.Exec(ctx, "CREATE DATABASE "+quoted+" OWNER account_service TEMPLATE template0 ENCODING 'UTF8'"); err != nil {
		_ = admin.Close(context.Background())
		t.Fatal("create owned UUID database")
	}
	var connection *pgx.Conn
	var runtimePool, writerPool, relayPool *pgxpool.Pool
	t.Cleanup(func() {
		clean, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		if connection != nil {
			_ = connection.Close(clean)
		}
		for _, pool := range []*pgxpool.Pool{runtimePool, writerPool, relayPool} {
			if pool != nil {
				pool.Close()
			}
		}
		defer admin.Close(context.Background())
		var owner string
		var sessions int
		if err := admin.QueryRow(clean, `SELECT pg_get_userbyid(datdba),(SELECT count(*) FROM pg_stat_activity WHERE datname=$1) FROM pg_database WHERE datname=$1`, name).Scan(&owner, &sessions); err != nil || owner != "account_service" || sessions != 0 {
			t.Error("owned database identity/session guard failed; database retained")
			return
		}
		if _, err := admin.Exec(clean, "DROP DATABASE "+quoted); err != nil {
			t.Error("normal owned database cleanup failed")
			return
		}
		var absent bool
		if err := admin.QueryRow(clean, `SELECT NOT EXISTS(SELECT 1 FROM pg_database WHERE datname=$1)`, name).Scan(&absent); err != nil || !absent {
			t.Error("owned database remains")
		}
	})
	if _, err = admin.Exec(ctx, "GRANT CREATE ON DATABASE "+quoted+" TO account_migrator"); err != nil {
		t.Fatal("owned database migrator grant")
	}
	migrationConfig := base.Copy()
	migrationConfig.Database, migrationConfig.User, migrationConfig.Password = name, "account_migrator", ""
	connection, err = pgx.ConnectConfig(ctx, migrationConfig)
	if err != nil {
		t.Fatal("open genuine migration session")
	}
	if _, err = connection.Exec(ctx, "SET ROLE account_service; SET search_path TO public,pg_temp"); err != nil {
		t.Fatal("select migration schema owner")
	}
	var actor, session string
	var super bool
	if err = connection.QueryRow(ctx, `SELECT current_user,session_user,(SELECT rolsuper FROM pg_roles WHERE rolname=current_user)`).Scan(&actor, &session, &super); err != nil || actor != "account_service" || session != "account_migrator" || super {
		t.Fatal("migration identities invalid")
	}
	loaded, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	if maximumVersion > 0 {
		bounded := make([]migrations.Migration, 0, len(loaded))
		for _, item := range loaded {
			if item.Version <= maximumVersion {
				bounded = append(bounded, item)
			}
		}
		loaded = bounded
	}
	if err = accountschema.Up(ctx, connection, loaded); err != nil {
		t.Fatal("public Account Up failed")
	}
	if _, err = migrations.VerifyApplied(ctx, connection, loaded); err != nil {
		t.Fatal("public Account migration ledger mismatch")
	}
	openPool := func(role string) *pgxpool.Pool {
		configuration, err := pgxpool.ParseConfig(databaseURL)
		if err != nil {
			t.Fatal("parse genuine pool configuration")
		}
		configuration.MaxConns = 10
		configuration.ConnConfig.Database, configuration.ConnConfig.User, configuration.ConnConfig.Password = name, role, ""
		configuration.ConnConfig.RuntimeParams["search_path"] = "public,pg_temp"
		pool, err := pgxpool.NewWithConfig(ctx, configuration)
		if err != nil {
			t.Fatal("open genuine pool")
		}
		if err = pool.QueryRow(ctx, `SELECT current_user,session_user,(SELECT rolsuper FROM pg_roles WHERE rolname=current_user)`).Scan(&actor, &session, &super); err != nil || actor != role || session != role || super {
			pool.Close()
			t.Fatal("genuine pool identity invalid")
		}
		return pool
	}
	runtimePool, writerPool, relayPool = openPool("account_service"), openPool("account_approval_writer"), openPool("account_approval_relay")
	repository := &PostgresRepository{pool: runtimePool, approvalWriterPool: writerPool, now: func() time.Time { return time.Now().UTC() }, walletPolicyVersion: walletProvisioningPolicyVersion, walletProvisioningSLA: walletProvisioningSLA}
	return repository, connection, &ApprovalRelayStore{pool: relayPool}, loaded
}
