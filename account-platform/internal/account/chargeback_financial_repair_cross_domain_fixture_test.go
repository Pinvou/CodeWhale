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

// chargebackRepairCrossDomainFacts names the seeded confirmed chargeback whose
// whole BASE credit was consumed, so the repair has to compensate the shortfall.
type chargebackRepairCrossDomainFacts struct {
	ChargebackCaseID     uuid.UUID
	EvidenceID           uuid.UUID
	AnomalyCaseID        uuid.UUID
	WalletID             uuid.UUID
	PackageID            uuid.UUID
	ComponentID          uuid.UUID
	OrderID              uuid.UUID
	TransactionID        uuid.UUID
	ProviderChargebackID string
	BaseMicrocredits     int64
}

// newChargebackRepairCrossDomainBillingFixture provisions a production-shaped
// Billing database for the cross-domain chargeback repair gate: the database and
// public schema belong to billing_schema_owner, migrations run as
// billing_migrator with MIGRATION_ROLE=billing_service, and the runtime pool
// runs as the non-superuser billing_service identity. Activation stays exactly
// as deployed: the command remains the original INACTIVE v1/s1 row.
func newChargebackRepairCrossDomainBillingFixture(t *testing.T, ctx context.Context, databaseURL string) (*billingdomain.PostgresRepository, *pgxpool.Pool, *pgx.Conn, chargebackRepairCrossDomainFacts) {
	t.Helper()
	base, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := pgx.ConnectConfig(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	database := "billing_chargeback_repair_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	facts := seedChargebackRepairCrossDomainCase(t, ctx, pool)
	return repository, pool, connection, facts
}

// seedChargebackRepairCrossDomainCase writes the production-shaped case the
// command's closed plan accepts: one FROZEN package whose BASE credit is fully
// consumed (so nothing is revocable), the confirmed provider chargeback evidence
// and the resolved anomaly that escalated the case to financial repair.
func seedChargebackRepairCrossDomainCase(t *testing.T, ctx context.Context, pool *pgxpool.Pool) chargebackRepairCrossDomainFacts {
	t.Helper()
	now := time.Now().UTC()
	facts := chargebackRepairCrossDomainFacts{
		ChargebackCaseID: uuid.New(), EvidenceID: uuid.New(), AnomalyCaseID: uuid.New(),
		WalletID: uuid.New(), PackageID: uuid.New(), ComponentID: uuid.New(),
		OrderID: uuid.New(), TransactionID: uuid.New(),
		ProviderChargebackID: "chargeback-" + uuid.NewString(), BaseMicrocredits: 100,
	}
	ownerID, lotID := uuid.New(), uuid.New()
	product, err := json.Marshal(billingdomain.Product{ProductID: "CHARGEBACK_REPAIR", AmountFen: 100,
		CreditMicrocredits: facts.BaseMicrocredits, BasePoints: 100, Currency: "CNY", PlanName: "ChargebackRepair"})
	if err != nil {
		t.Fatal(err)
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
		{`INSERT INTO wallets(id,owner_type,owner_id,status,posted_microcredits,version,created_at,updated_at)
VALUES($1,'PERSONAL',$2,'ACTIVE',0,1,$3,$3)`, []any{facts.WalletID, ownerID, now}},
		{`INSERT INTO ledger_accounts(id,wallet_id,code,account_type,created_at) VALUES($1,$2,$3,'WALLET_LIABILITY',$4)`,
			[]any{uuid.New(), facts.WalletID, "wallet:" + facts.WalletID.String(), now}},
		{`INSERT INTO payment_orders(id,merchant_order_no,requester_id,payer_user_id,wallet_id,product_id,product_snapshot,channel,
amount_fen,currency,credit_microcredits,status,fulfillment_status,expires_at,provider_payable_until,late_review_boundary_at,final_archive_eligible_at,
created_at,updated_at,provider_sku,provider_id,provider_version)
VALUES($1,$2,$3,$3,$4,'CHARGEBACK_REPAIR',$5,'ALIPAY',$6,'CNY',$7,'PAID','CREDITED',$8,$8,$9,$10,$11,$11,'sku-repair','test-provider','v1')`,
			[]any{facts.OrderID, "chargeback-repair-" + facts.OrderID.String(), ownerID, facts.WalletID, product,
				facts.BaseMicrocredits, facts.BaseMicrocredits, now.Add(time.Hour), now.Add(72 * time.Hour),
				now.Add(8 * 24 * time.Hour), now}},
		{`INSERT INTO payment_transactions(id,payment_order_id,provider_id,provider_version,channel,provider_transaction_id,
status,fulfillment_status,amount_fen,currency,source,provider_status,provider_paid_at,verified_at,fulfilled_at,created_at,updated_at)
VALUES($1,$2,'test-provider','v1','ALIPAY',$3,'PAID','CREDITED',$4,'CNY','QUERY','PAY_SUCCESS',$5,$5,$5,$5,$5)`,
			[]any{facts.TransactionID, facts.OrderID, "trade-" + facts.TransactionID.String(), facts.BaseMicrocredits, now}},
		{`INSERT INTO credit_lots(id,wallet_id,source_type,source_reference,original_microcredits,remaining_microcredits,refundable,expires_at,created_at)
VALUES($1,$2,'RECHARGE',$3,$4,0,FALSE,$5,$6)`,
			[]any{lotID, facts.WalletID, "recharge:" + lotID.String(), facts.BaseMicrocredits, now.Add(24 * time.Hour), now}},
		{`INSERT INTO entitlement_packages(id,user_id,wallet_id,payment_order_id,source_payment_transaction_id,credit_lot_id,product_id,plan_name,sku_code,
amount_fen,points_total,points_remaining,points_used,credited_at,valid_from,valid_until,status,created_at,updated_at,source_type,source_reference)
VALUES($1,$2,$3,$4,$5,$6,'CHARGEBACK_REPAIR','ChargebackRepair','sku-repair',$7,$8,0,$8,$9,$9,$10,'FROZEN',$9,$9,'PAYMENT',$11)`,
			[]any{facts.PackageID, ownerID, facts.WalletID, facts.OrderID, facts.TransactionID, lotID,
				facts.BaseMicrocredits, facts.BaseMicrocredits, now, now.Add(24 * time.Hour),
				"payment-transaction:" + facts.TransactionID.String()}},
		{`INSERT INTO entitlement_components(id,package_id,component_type,credit_lot_id,original_microcredits,available_microcredits,
used_microcredits,credited_at,expires_at,status,created_at,updated_at)
VALUES($1,$2,'BASE',$3,$4,0,$4,$5,$6,'FROZEN',$5,$5)`,
			[]any{facts.ComponentID, facts.PackageID, lotID, facts.BaseMicrocredits, now, now.Add(24 * time.Hour)}},
		{`INSERT INTO payment_evidence(id,payment_order_id,payment_transaction_id,evidence_type,provider_id,provider_version,evidence_key,
merchant_order_no,provider_transaction_id,amount_fen,currency,provider_status,received_at,payload_digest,payload_snapshot,merge_result)
VALUES($1,$2,$3,'CHARGEBACK','test-provider','v1',$4,$5,$6,$7,'CNY','CHARGEBACK_CONFIRMED',$8,$9,'{}','RECORDED')`,
			[]any{facts.EvidenceID, facts.OrderID, facts.TransactionID, facts.ProviderChargebackID,
				"chargeback-repair-" + facts.OrderID.String(), "trade-" + facts.TransactionID.String(),
				facts.BaseMicrocredits, now, strings.Repeat("ef", 32)}},
		{`INSERT INTO chargeback_cases(id,payment_evidence_id,payment_order_id,payment_transaction_id,entitlement_package_id,
wallet_id,provider_chargeback_id,status,created_at,version)
VALUES($1,$2,$3,$4,$5,$6,$7,'REVIEW_REQUIRED',$8,1)`,
			[]any{facts.ChargebackCaseID, facts.EvidenceID, facts.OrderID, facts.TransactionID,
				facts.PackageID, facts.WalletID, facts.ProviderChargebackID, now}},
		{`INSERT INTO manual_review_cases(id,domain,case_type,resource_type,resource_id,severity,status,created_at,claim_due_at,
first_response_due_at,mitigation_due_at,resolution_due_at,resolved_at,updated_at)
VALUES($1,'BILLING','PAYMENT_ANOMALY','CHARGEBACK_CASE',$2,'P1','RESOLVED',$3,$3,$3,$3,$3,$3,$3)`,
			[]any{facts.AnomalyCaseID, facts.ChargebackCaseID, now}},
		{`INSERT INTO payment_anomaly_cases(id,anomaly_type,dedupe_key,payment_order_id,payment_transaction_id,payment_evidence_id,
chargeback_case_id,status,resolution_code,financial_action_status,resource_snapshot,version,created_at,updated_at,resolved_at)
VALUES($1,'CHARGEBACK',$2,$3,$4,$5,$6,'RESOLVED','ESCALATE_FINANCIAL_REPAIR','PENDING','{}',1,$7,$7,$7)`,
			[]any{facts.AnomalyCaseID, "chargeback:" + facts.ChargebackCaseID.String(), facts.OrderID,
				facts.TransactionID, facts.EvidenceID, facts.ChargebackCaseID, now}},
	} {
		if _, err := tx.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatalf("chargeback repair fixture statement %d: %v", index, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return facts
}
