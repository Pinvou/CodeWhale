package account

import (
	"context"
	"strings"
	"testing"
	"time"

	billingdomain "github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/Pinvou/pinvou3/account-platform/internal/billingschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// receivableWriteOffCrossDomainFacts names the seeded open overage receivable so
// the test can assert the frozen projection, the presenter and the committed
// write-off against the same identifiers.
type receivableWriteOffCrossDomainFacts struct {
	WalletID                uuid.UUID
	ReceivableID            uuid.UUID
	SourceReference         string
	OriginalMicrocredits    int64
	WriteOffMicrocredits    int64
	WalletReceivableCredits int64
}

// newReceivableWriteOffCrossDomainBillingFixture provisions a production-shaped
// Billing database for the cross-domain write-off gate: the database and public
// schema belong to billing_schema_owner, migrations run as billing_migrator with
// MIGRATION_ROLE=billing_service, and the runtime pool runs as the non-superuser
// billing_service identity. Activation stays exactly as deployed: the command
// remains the original INACTIVE v1/s1 row.
func newReceivableWriteOffCrossDomainBillingFixture(t *testing.T, ctx context.Context, databaseURL string) (*billingdomain.PostgresRepository, *pgxpool.Pool, *pgx.Conn, receivableWriteOffCrossDomainFacts) {
	t.Helper()
	base, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal("parse owned fixture configuration")
	}
	// This finite opt-in fixture uses the same exclusive local runtime as
	// TestPrivateF11PublicOwnerIntegration; it must never target a deployment.
	if (base.Host != "127.0.0.1" && base.Host != "localhost") || base.Port != 55504 || base.Database != "postgres" || len(base.Fallbacks) != 0 {
		t.Fatal("dedicated local fixture connection required")
	}
	admin, err := pgx.ConnectConfig(ctx, base)
	if err != nil {
		t.Fatal("open owned fixture coordinator")
	}
	var dataDirectory, coordinator string
	var port int
	if err := admin.QueryRow(ctx, `SELECT current_setting('data_directory'),session_user,current_setting('port')::int`).Scan(&dataDirectory, &coordinator, &port); err != nil || coordinator != "pinvou_wave_test" || port != 55504 || strings.ReplaceAll(dataDirectory, "\\", "/") != "D:/Projects/pinvou3-account-functional/.cache/parallel-runtimes/f04/data" {
		_ = admin.Close(context.Background())
		t.Fatal("owned runtime identity guard failed")
	}
	// Account's fixture has already opened its own database and sessions. Its
	// finite roles are allowed, but no unrelated deployment actor is accepted.
	allowedRoles := []string{"pinvou_wave_test", "account_service", "account_migrator", "account_approval_control_owner", "account_approval_authorization_owner", "account_approval_writer", "account_approval_backfill", "account_approval_relay", "account_approval_recovery", "billing_service", "billing_schema_owner", "billing_migrator"}
	var unrelatedActors int
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM pg_roles WHERE rolname !~ '^pg_' AND NOT(rolname=ANY($1))`, allowedRoles).Scan(&unrelatedActors); err != nil || unrelatedActors != 0 {
		_ = admin.Close(context.Background())
		t.Fatal("unrelated actors exist; fixture refused")
	}
	roles := []string{"billing_service", "billing_schema_owner", "billing_migrator"}
	var actorCount int
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM pg_roles WHERE rolname=ANY($1) AND NOT rolsuper AND NOT rolcreatedb AND NOT rolcreaterole AND NOT rolreplication AND NOT rolbypassrls`, roles).Scan(&actorCount); err != nil || actorCount != len(roles) {
		_ = admin.Close(context.Background())
		t.Fatal("owned runner must provision all finite non-superuser Billing actors")
	}
	database := "billing_receivable_write_off_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	identifier := pgx.Identifier{database}.Sanitize()
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+identifier+` OWNER billing_schema_owner TEMPLATE template0 ENCODING 'UTF8'`); err != nil {
		_ = admin.Close(context.Background())
		t.Fatal("create owned UUID database")
	}
	var connection *pgx.Conn
	var pool *pgxpool.Pool
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if pool != nil {
			pool.Close()
		}
		if connection != nil {
			_ = connection.Close(cleanup)
		}
		defer admin.Close(context.Background())
		var owner string
		var sessions int
		if err := admin.QueryRow(cleanup, `SELECT pg_get_userbyid(datdba),(SELECT count(*) FROM pg_stat_activity WHERE datname=$1) FROM pg_database WHERE datname=$1`, database).Scan(&owner, &sessions); err != nil || owner != "billing_schema_owner" || sessions != 0 {
			t.Error("owned database identity/session guard failed; database retained")
			return
		}
		if _, err := admin.Exec(cleanup, `DROP DATABASE `+identifier); err != nil {
			t.Error("normal owned database cleanup failed")
			return
		}
		var absent bool
		if err := admin.QueryRow(cleanup, `SELECT NOT EXISTS(SELECT 1 FROM pg_database WHERE datname=$1)`, database).Scan(&absent); err != nil || !absent {
			t.Error("owned database remains")
		}
	})
	migrationConfig := base.Copy()
	migrationConfig.Database, migrationConfig.User, migrationConfig.Password = database, "billing_migrator", ""
	connection, err = pgx.ConnectConfig(ctx, migrationConfig)
	if err != nil {
		t.Fatal("open genuine migration session")
	}
	if _, err := connection.Exec(ctx, `SET ROLE billing_schema_owner;
ALTER SCHEMA public OWNER TO billing_schema_owner;
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT USAGE,CREATE ON SCHEMA public TO billing_service;
SET ROLE billing_service; SET search_path TO public,pg_temp`); err != nil {
		t.Fatal(err)
	}
	var actor, session string
	var super bool
	if err := connection.QueryRow(ctx, `SELECT current_user,session_user,(SELECT rolsuper FROM pg_roles WHERE rolname=current_user)`).Scan(&actor, &session, &super); err != nil || actor != "billing_service" || session != "billing_migrator" || super {
		t.Fatal("migration identities invalid")
	}
	loaded, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	if err := billingschema.Up(ctx, connection, loaded); err != nil {
		t.Fatalf("public Billing fixture migrations: %v", err)
	}
	if _, err := migrations.VerifyApplied(ctx, connection, loaded); err != nil {
		t.Fatal("public Billing migration ledger mismatch")
	}
	migrations.Unlock(ctx, connection)
	configuration, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal("parse genuine runtime configuration")
	}
	configuration.MaxConns = 20
	configuration.ConnConfig.Database, configuration.ConnConfig.User, configuration.ConnConfig.Password = database, "billing_service", ""
	configuration.ConnConfig.RuntimeParams["search_path"] = "public,pg_temp"
	pool, err = pgxpool.NewWithConfig(ctx, configuration)
	if err != nil {
		t.Fatal("open genuine runtime pool")
	}
	if err := pool.QueryRow(ctx, `SELECT current_user,session_user,(SELECT rolsuper FROM pg_roles WHERE rolname=current_user)`).Scan(&actor, &session, &super); err != nil || actor != "billing_service" || session != actor || super {
		t.Fatal("runtime identities invalid")
	}
	repository := billingdomain.NewPostgresRepositoryWithPool(pool)
	facts := seedReceivableWriteOffCrossDomainEvidence(t, ctx, pool)
	return repository, pool, connection, facts
}

// seedReceivableWriteOffCrossDomainEvidence writes one wallet with an open
// USAGE_OVERAGE receivable of 30 microcredits and the wallet-side receivable
// projection the real settlement path maintains.
func seedReceivableWriteOffCrossDomainEvidence(t *testing.T, ctx context.Context, pool *pgxpool.Pool) receivableWriteOffCrossDomainFacts {
	t.Helper()
	facts := receivableWriteOffCrossDomainFacts{
		WalletID: uuid.New(), ReceivableID: uuid.New(), OriginalMicrocredits: 30, WriteOffMicrocredits: 30,
	}
	facts.SourceReference = "usage:" + uuid.NewString()
	facts.WalletReceivableCredits = 30
	now := time.Now().UTC()
	if _, err := pool.Exec(ctx, `INSERT INTO wallets(id,owner_type,owner_id,status,posted_microcredits,receivable_microcredits,financial_status,financial_freeze_reason,version,created_at,updated_at)
VALUES($1,'PERSONAL',$2,'ACTIVE',100,30,'RECEIVABLE_OPEN','USAGE_OVERAGE',2,$3,$3)`, facts.WalletID, uuid.New(), now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO receivables(id,wallet_id,source_type,source_reference,original_microcredits,outstanding_microcredits,status,created_at,updated_at)
VALUES($1,$2,'USAGE_OVERAGE',$3,$4,$4,'OPEN',$5,$5)`, facts.ReceivableID, facts.WalletID, facts.SourceReference, facts.OriginalMicrocredits, now); err != nil {
		t.Fatal(err)
	}
	return facts
}
