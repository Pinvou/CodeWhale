package account

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	billingdomain "github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// newEntitlementCrossDomainBillingFixture provisions a production-shaped
// Billing database for the cross-domain entitlement correction gate: the
// database and public schema belong to billing_schema_owner, migrations run as
// billing_migrator with MIGRATION_ROLE=billing_service, and the runtime pool
// runs as the non-superuser billing_service identity. Activation stays exactly
// as deployed: policy v2 is appended INACTIVE next to the immutable v1 history.
func newEntitlementCrossDomainBillingFixture(t *testing.T, ctx context.Context, databaseURL string) (*billingdomain.PostgresRepository, *pgxpool.Pool, *pgx.Conn) {
	t.Helper()
	base, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := pgx.ConnectConfig(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	database := "billing_entitlement_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	// Mirror deploy/compose/init-databases.sh: the schema owner holds public and
	// the runtime keeps schema CREATE only until migration 53 revokes it.
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
	seedEntitlementCrossDomainBillingEvidence(t, ctx, pool)
	return repository, pool, connection
}

func seedEntitlementCrossDomainBillingEvidence(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	entitlementCrossDomainWalletID, entitlementCrossDomainPackageID, entitlementCrossDomainOrderID = uuid.New(), uuid.New(), uuid.New()
	entitlementCrossDomainComponentID = uuid.New()
	transactionID, walletAccountID, clearingAccountID, journalID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	now := time.Now().UTC()
	reference := "payment-transaction:" + transactionID.String()
	product, err := json.Marshal(billingdomain.Product{ProductID: "ENTITLEMENT_CROSS_DOMAIN", AmountFen: 100, CreditMicrocredits: 100, BasePoints: 80, BonusPoints: 20, Currency: "CNY"})
	if err != nil {
		t.Fatal(err)
	}
	statements := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO wallets(id,owner_type,owner_id,status,posted_microcredits,version,created_at,updated_at) VALUES($1,'PERSONAL',$2,'ACTIVE',100,1,$3,$3)`, []any{entitlementCrossDomainWalletID, uuid.New(), now}},
		{`INSERT INTO ledger_accounts(id,wallet_id,code,account_type,created_at) VALUES($1,$2,$3,'WALLET_LIABILITY',$4)`, []any{walletAccountID, entitlementCrossDomainWalletID, "entitlement-cross-wallet:" + entitlementCrossDomainWalletID.String(), now}},
		{`INSERT INTO ledger_accounts(id,code,account_type,created_at) VALUES($1,$2,'PAYMENT_CLEARING',$3)`, []any{clearingAccountID, "entitlement-cross-clearing:" + entitlementCrossDomainWalletID.String(), now}},
		{`INSERT INTO payment_orders(id,merchant_order_no,requester_id,payer_user_id,wallet_id,product_id,product_snapshot,channel,amount_fen,currency,credit_microcredits,status,fulfillment_status,expires_at,provider_payable_until,late_review_boundary_at,final_archive_eligible_at,created_at,updated_at,provider_id,provider_version) VALUES($1,$2,$3,$3,$4,'ENTITLEMENT_CROSS_DOMAIN',$5,'ALIPAY',100,'CNY',100,'PAID','CREDITED',$6,$6,$7,$8,$9,$9,'test-provider','v1')`, []any{entitlementCrossDomainOrderID, "entitlement-cross-" + entitlementCrossDomainOrderID.String(), uuid.New(), entitlementCrossDomainWalletID, product, now.Add(time.Hour), now.Add(72 * time.Hour), now.Add(8 * 24 * time.Hour), now}},
		{`INSERT INTO payment_transactions(id,payment_order_id,channel,provider_transaction_id,status,amount_fen,currency,source,provider_paid_at,verified_at,created_at,updated_at) VALUES($1,$2,'ALIPAY',$3,'PAID',100,'CNY','QUERY',$4,$4,$4,$4)`, []any{transactionID, entitlementCrossDomainOrderID, "entitlement-cross-txn-" + transactionID.String(), now}},
		{`INSERT INTO credit_lots(id,wallet_id,source_type,source_reference,original_microcredits,remaining_microcredits,refundable,expires_at,created_at) VALUES($1,$2,'RECHARGE',$3,100,100,TRUE,$4,$5)`, []any{entitlementCrossDomainComponentID, entitlementCrossDomainWalletID, reference, now.Add(24 * time.Hour), now}},
		{`INSERT INTO entitlement_packages(id,user_id,wallet_id,payment_order_id,source_payment_transaction_id,credit_lot_id,product_id,plan_name,sku_code,amount_fen,points_total,points_remaining,credited_at,valid_from,valid_until,status,created_at,updated_at,source_type,source_reference) VALUES($1,$2,$3,$4,$5,$6,'ENTITLEMENT_CROSS_DOMAIN','Test','',100,100,100,$7,$7,$8,'ACTIVE',$7,$7,'PAYMENT',$9)`, []any{entitlementCrossDomainPackageID, uuid.New(), entitlementCrossDomainWalletID, entitlementCrossDomainOrderID, transactionID, entitlementCrossDomainComponentID, now, now.Add(24 * time.Hour), reference}},
		{`INSERT INTO entitlement_components(id,package_id,component_type,credit_lot_id,original_microcredits,available_microcredits,credited_at,expires_at,status,created_at,updated_at) VALUES($1,$2,'BASE',$1,100,100,$3,$4,'ACTIVE',$3,$3)`, []any{entitlementCrossDomainComponentID, entitlementCrossDomainPackageID, now, now.Add(24 * time.Hour)}},
		{`INSERT INTO journal_entries(id,entry_type,business_reference,description,created_at) VALUES($1,'RECHARGE',$2,'fixture',$3)`, []any{journalID, reference, now}},
		{`INSERT INTO ledger_postings(id,journal_entry_id,ledger_account_id,delta_microcredits,credit_lot_id,created_at) VALUES($1,$2,$3,100,$4,$5)`, []any{uuid.New(), journalID, walletAccountID, entitlementCrossDomainComponentID, now}},
		{`INSERT INTO ledger_postings(id,journal_entry_id,ledger_account_id,delta_microcredits,created_at) VALUES($1,$2,$3,-100,$4)`, []any{uuid.New(), journalID, clearingAccountID, now}},
	}
	// The ledger balance trigger is deferred, so the whole paid evidence set
	// must be written in one transaction exactly like the real fulfillment.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	for index, statement := range statements {
		if _, err := tx.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatalf("Billing evidence statement %d: %v", index, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit Billing evidence: %v", err)
	}
}

var (
	entitlementCrossDomainWalletID    uuid.UUID
	entitlementCrossDomainPackageID   uuid.UUID
	entitlementCrossDomainOrderID     uuid.UUID
	entitlementCrossDomainComponentID uuid.UUID
)
