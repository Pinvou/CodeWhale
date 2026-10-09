package account

import (
	"context"
	"strings"
	"testing"
	"time"

	billingdomain "github.com/Pinvou/pinvou3/account-platform/internal/billing"
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
		t.Fatal(err)
	}
	admin, err := pgx.ConnectConfig(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	database := "billing_receivable_write_off_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	identifier := pgx.Identifier{database}.Sanitize()
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+identifier+` OWNER billing_schema_owner TEMPLATE template0 ENCODING 'UTF8'`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = admin.Exec(cleanup, `DROP DATABASE IF EXISTS `+identifier)
		_ = admin.Close(context.Background())
	})
	migrationConfig := base.Copy()
	migrationConfig.Database = database
	connection, err := pgx.ConnectConfig(ctx, migrationConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close(context.Background()) })
	if _, err := connection.Exec(ctx, `ALTER SCHEMA public OWNER TO billing_schema_owner;
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT USAGE,CREATE ON SCHEMA public TO billing_service`); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(ctx, `SET SESSION AUTHORIZATION billing_migrator; SET ROLE billing_service`); err != nil {
		t.Fatal(err)
	}
	loaded, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	if err := migrations.Up(ctx, connection, loaded); err != nil {
		t.Fatalf("Billing fixture migrations: %v", err)
	}
	migrations.Unlock(ctx, connection)

	configuration, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	configuration.MaxConns = 20
	configuration.ConnConfig.Database = database
	configuration.AfterConnect = func(connectContext context.Context, connection *pgx.Conn) error {
		_, connectErr := connection.Exec(connectContext, `SET SESSION AUTHORIZATION billing_service; SET search_path TO public,pg_temp`)
		return connectErr
	}
	pool, err := pgxpool.NewWithConfig(ctx, configuration)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
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
