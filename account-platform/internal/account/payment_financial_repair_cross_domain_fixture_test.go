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

// paymentRepairCrossDomainFacts names the seeded review-required payment so the
// test can assert the frozen projection, the presenter and the committed repair
// against the same identifiers.
type paymentRepairCrossDomainFacts struct {
	OrderID               uuid.UUID
	WalletID              uuid.UUID
	EvidenceID            uuid.UUID
	ProviderTransactionID string
	AmountFen             int64
	CreditMicrocredits    int64
}

// newPaymentRepairCrossDomainBillingFixture provisions a production-shaped
// Billing database for the cross-domain payment repair gate: the database and
// public schema belong to billing_schema_owner, migrations run as
// billing_migrator with MIGRATION_ROLE=billing_service, and the runtime pool
// runs as the non-superuser billing_service identity. Activation stays exactly
// as deployed: the command remains the original INACTIVE v1/s1 row.
func newPaymentRepairCrossDomainBillingFixture(t *testing.T, ctx context.Context, databaseURL string) (*billingdomain.PostgresRepository, *pgxpool.Pool, *pgx.Conn, paymentRepairCrossDomainFacts) {
	t.Helper()
	base, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := pgx.ConnectConfig(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	database := "billing_payment_repair_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	facts := seedPaymentRepairCrossDomainEvidence(t, ctx, pool)
	return repository, pool, connection, facts
}

// seedPaymentRepairCrossDomainEvidence writes one wallet, one MANUAL_REVIEW
// order with a matching successful provider evidence row, and the
// review-required transaction that both put the order into MANUAL_REVIEW and
// lets the reused writer take its REPAIR_CREDIT branch.
func seedPaymentRepairCrossDomainEvidence(t *testing.T, ctx context.Context, pool *pgxpool.Pool) paymentRepairCrossDomainFacts {
	t.Helper()
	facts := paymentRepairCrossDomainFacts{
		OrderID: uuid.New(), WalletID: uuid.New(), EvidenceID: uuid.New(),
		ProviderTransactionID: "trade-" + uuid.NewString(), AmountFen: 100, CreditMicrocredits: 100,
	}
	ownerID, walletAccountID := uuid.New(), uuid.New()
	now := time.Now().UTC()
	product, err := json.Marshal(billingdomain.Product{ProductID: "PAYMENT_REPAIR", AmountFen: facts.AmountFen,
		CreditMicrocredits: facts.CreditMicrocredits, BasePoints: 80, BonusPoints: 20, Currency: "CNY", PlanName: "Repair"})
	if err != nil {
		t.Fatal(err)
	}
	for index, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO wallets(id,owner_type,owner_id,status,posted_microcredits,version,created_at,updated_at)
VALUES($1,'PERSONAL',$2,'ACTIVE',0,1,$3,$3)`, []any{facts.WalletID, ownerID, now}},
		{`INSERT INTO ledger_accounts(id,wallet_id,code,account_type,created_at) VALUES($1,$2,$3,'WALLET_LIABILITY',$4)`,
			[]any{walletAccountID, facts.WalletID, "wallet:" + facts.WalletID.String(), now}},
		{`INSERT INTO payment_orders(id,merchant_order_no,requester_id,payer_user_id,wallet_id,product_id,product_snapshot,channel,
amount_fen,currency,credit_microcredits,status,fulfillment_status,expires_at,provider_payable_until,late_review_boundary_at,final_archive_eligible_at,
created_at,updated_at,provider_sku,provider_id,provider_version)
VALUES($1,$2,$3,$3,$4,'PAYMENT_REPAIR',$5,'ALIPAY',$6,'CNY',$7,'MANUAL_REVIEW','MANUAL_REVIEW',$8,$8,$9,$10,$11,$11,'sku-repair','test-provider','v1')`,
			[]any{facts.OrderID, "repair-" + facts.OrderID.String(), ownerID, facts.WalletID, product, facts.AmountFen,
				facts.CreditMicrocredits, now.Add(time.Hour), now.Add(72 * time.Hour), now.Add(8 * 24 * time.Hour), now}},
		{`INSERT INTO payment_evidence(id,payment_order_id,evidence_type,provider_id,provider_version,evidence_key,
merchant_order_no,provider_transaction_id,amount_fen,currency,provider_status,provider_event_at,received_at,payload_digest,payload_snapshot,merge_result)
VALUES($1,$2,'QUERY','test-provider','v1',$3,$4,$5,$6,'CNY','PAY_SUCCESS',$7,$7,$8,'{}','RECORDED')`,
			[]any{facts.EvidenceID, facts.OrderID, "query:" + facts.EvidenceID.String(), "repair-" + facts.OrderID.String(),
				facts.ProviderTransactionID, facts.AmountFen, now, strings.Repeat("ab", 32)}},
		{`INSERT INTO payment_transactions(id,payment_order_id,provider_id,provider_version,channel,provider_transaction_id,
status,fulfillment_status,amount_fen,currency,source,provider_status,provider_paid_at,verified_at,created_at,updated_at)
VALUES($1,$2,'test-provider','v1','ALIPAY',$3,'REVIEW_REQUIRED','REVIEW_REQUIRED',$4,'CNY','QUERY','PAY_SUCCESS',$5,$5,$5,$5)`,
			[]any{uuid.New(), facts.OrderID, facts.ProviderTransactionID, facts.AmountFen, now}},
	} {
		if _, err := pool.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatalf("Billing evidence statement %d: %v", index, err)
		}
	}
	return facts
}
