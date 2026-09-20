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

// ledgerRepairCrossDomainFacts names the seeded attribution error so the test
// can assert the frozen projection, the presenter and the committed repair
// against the same identifiers.
type ledgerRepairCrossDomainFacts struct {
	WalletID           uuid.UUID
	JournalEntryID     uuid.UUID
	TargetPostingID    uuid.UUID
	WrongAccountID     uuid.UUID
	CorrectAccountID   uuid.UUID
	WalletAccountID    uuid.UUID
	AmountMicrocredits int64
}

// newLedgerRepairCrossDomainBillingFixture provisions a production-shaped
// Billing database for the cross-domain ledger-repair gate: the database and
// public schema belong to billing_schema_owner, migrations run as
// billing_migrator with MIGRATION_ROLE=billing_service, and the runtime pool
// runs as the non-superuser billing_service identity. Activation stays exactly
// as deployed: LEDGER_REPAIR remains the original INACTIVE v1/s1 row.
func newLedgerRepairCrossDomainBillingFixture(t *testing.T, ctx context.Context, databaseURL string) (*billingdomain.PostgresRepository, *pgxpool.Pool, *pgx.Conn, ledgerRepairCrossDomainFacts) {
	t.Helper()
	base, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := pgx.ConnectConfig(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	database := "billing_ledger_repair_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	facts := seedLedgerRepairCrossDomainEvidence(t, ctx, pool)
	return repository, pool, connection, facts
}

// seedLedgerRepairCrossDomainEvidence writes one USAGE journal whose revenue
// leg was attributed to the platform adjustment account instead of the usage
// revenue account. That is exactly the attribution error the first release of
// LEDGER_REPAIR may correct, and the whole journal is written in one deferred
// balancing transaction like the real writer.
func seedLedgerRepairCrossDomainEvidence(t *testing.T, ctx context.Context, pool *pgxpool.Pool) ledgerRepairCrossDomainFacts {
	t.Helper()
	facts := ledgerRepairCrossDomainFacts{
		WalletID: uuid.New(), JournalEntryID: uuid.New(), TargetPostingID: uuid.New(), WalletAccountID: uuid.New(),
		AmountMicrocredits: 30,
	}
	now := time.Now().UTC()
	if err := pool.QueryRow(ctx, `SELECT id FROM ledger_accounts WHERE code='platform:adjustment' AND wallet_id IS NULL`).Scan(&facts.WrongAccountID); err != nil {
		t.Fatalf("platform adjustment account: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT id FROM ledger_accounts WHERE code='platform:usage-revenue' AND wallet_id IS NULL`).Scan(&facts.CorrectAccountID); err != nil {
		t.Fatalf("platform usage revenue account: %v", err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	for index, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO wallets(id,owner_type,owner_id,status,posted_microcredits,version,created_at,updated_at) VALUES($1,'PERSONAL',$2,'ACTIVE',100,1,$3,$3)`, []any{facts.WalletID, uuid.New(), now}},
		{`INSERT INTO ledger_accounts(id,wallet_id,code,account_type,created_at) VALUES($1,$2,$3,'WALLET_LIABILITY',$4)`, []any{facts.WalletAccountID, facts.WalletID, "ledger-repair-wallet:" + facts.WalletID.String(), now}},
		{`INSERT INTO journal_entries(id,entry_type,business_reference,description,created_at) VALUES($1,'USAGE',$2,'attribution error',$3)`, []any{facts.JournalEntryID, "usage:" + facts.JournalEntryID.String(), now}},
		{`INSERT INTO ledger_postings(id,journal_entry_id,ledger_account_id,delta_microcredits,created_at) VALUES($1,$2,$3,30,$4)`, []any{facts.TargetPostingID, facts.JournalEntryID, facts.WrongAccountID, now}},
		{`INSERT INTO ledger_postings(id,journal_entry_id,ledger_account_id,delta_microcredits,created_at) VALUES($1,$2,$3,-30,$4)`, []any{uuid.New(), facts.JournalEntryID, facts.WalletAccountID, now}},
	} {
		if _, err := tx.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatalf("Billing evidence statement %d: %v", index, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit Billing evidence: %v", err)
	}
	return facts
}
