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

// refundExceptionCrossDomainFacts names the seeded "provider refunded / local
// reversal missing" exception so the test can assert the frozen projection, the
// presenter and the committed repair against the same identifiers.
type refundExceptionCrossDomainFacts struct {
	RefundID           uuid.UUID
	WalletID           uuid.UUID
	PackageID          uuid.UUID
	ComponentID        uuid.UUID
	AnomalyCaseID      uuid.UUID
	EvidenceID         uuid.UUID
	ProviderRefundID   string
	AmountFen          int64
	CreditMicrocredits int64
}

// newRefundExceptionRepairCrossDomainBillingFixture provisions a
// production-shaped Billing database for the cross-domain refund exception
// repair gate: the database and public schema belong to billing_schema_owner,
// migrations run as billing_migrator with MIGRATION_ROLE=billing_service, and
// the runtime pool runs as the non-superuser billing_service identity. Activation
// stays exactly as deployed: the command remains the original INACTIVE v1/s1 row.
func newRefundExceptionRepairCrossDomainBillingFixture(t *testing.T, ctx context.Context, databaseURL string) (*billingdomain.PostgresRepository, *pgxpool.Pool, *pgx.Conn, refundExceptionCrossDomainFacts) {
	t.Helper()
	base, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := pgx.ConnectConfig(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	database := "billing_refund_exception_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	facts := seedRefundExceptionCrossDomainException(t, ctx, pool)
	return repository, pool, connection, facts
}

// seedRefundExceptionCrossDomainException writes the production-shaped exception
// the command's closed plan accepts: one FROZEN package whose FROZEN BASE
// component still holds the whole credit in one credit lot, a MANUAL_REVIEW
// refund with a channel refund id, the matching successful provider evidence and
// the resolved anomaly that escalated the refund to financial repair. The
// reservation covers exactly the refunded credit, so the repair reclaims it in
// full and needs no shortfall receivable.
func seedRefundExceptionCrossDomainException(t *testing.T, ctx context.Context, pool *pgxpool.Pool) refundExceptionCrossDomainFacts {
	t.Helper()
	now := time.Now().UTC()
	facts := refundExceptionCrossDomainFacts{
		RefundID: uuid.New(), WalletID: uuid.New(), PackageID: uuid.New(), ComponentID: uuid.New(),
		AnomalyCaseID: uuid.New(), EvidenceID: uuid.New(),
		ProviderRefundID: "refund-" + uuid.NewString(), AmountFen: 100, CreditMicrocredits: 100,
	}
	ownerID, orderID, transactionID, lotID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	product, err := json.Marshal(billingdomain.Product{ProductID: "REFUND_EXCEPTION_REPAIR", AmountFen: facts.AmountFen,
		CreditMicrocredits: facts.CreditMicrocredits, BasePoints: 100, Currency: "CNY", PlanName: "RefundException"})
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
VALUES($1,'PERSONAL',$2,'ACTIVE',100,1,$3,$3)`, []any{facts.WalletID, ownerID, now}},
		{`INSERT INTO ledger_accounts(id,wallet_id,code,account_type,created_at) VALUES($1,$2,$3,'WALLET_LIABILITY',$4)`,
			[]any{uuid.New(), facts.WalletID, "wallet:" + facts.WalletID.String(), now}},
		{`INSERT INTO payment_orders(id,merchant_order_no,requester_id,payer_user_id,wallet_id,product_id,product_snapshot,channel,
amount_fen,currency,credit_microcredits,status,fulfillment_status,expires_at,provider_payable_until,late_review_boundary_at,final_archive_eligible_at,
created_at,updated_at,provider_sku,provider_id,provider_version)
VALUES($1,$2,$3,$3,$4,'REFUND_EXCEPTION_REPAIR',$5,'ALIPAY',$6,'CNY',$7,'PAID','CREDITED',$8,$8,$9,$10,$11,$11,'sku-repair','test-provider','v1')`,
			[]any{orderID, "refund-exception-" + orderID.String(), ownerID, facts.WalletID, product, facts.AmountFen,
				facts.CreditMicrocredits, now.Add(time.Hour), now.Add(72 * time.Hour), now.Add(8 * 24 * time.Hour), now}},
		{`INSERT INTO payment_transactions(id,payment_order_id,provider_id,provider_version,channel,provider_transaction_id,
status,fulfillment_status,amount_fen,currency,source,provider_status,provider_paid_at,verified_at,fulfilled_at,created_at,updated_at)
VALUES($1,$2,'test-provider','v1','ALIPAY',$3,'PAID','CREDITED',$4,'CNY','QUERY','PAY_SUCCESS',$5,$5,$5,$5,$5)`,
			[]any{transactionID, orderID, "trade-" + transactionID.String(), facts.AmountFen, now}},
		{`INSERT INTO credit_lots(id,wallet_id,source_type,source_reference,original_microcredits,remaining_microcredits,refundable,expires_at,created_at)
VALUES($1,$2,'RECHARGE',$3,$4,$4,TRUE,$5,$6)`,
			[]any{lotID, facts.WalletID, "recharge:" + lotID.String(), facts.CreditMicrocredits, now.Add(24 * time.Hour), now}},
		{`INSERT INTO entitlement_packages(id,user_id,wallet_id,payment_order_id,source_payment_transaction_id,credit_lot_id,product_id,plan_name,sku_code,
amount_fen,points_total,points_remaining,credited_at,valid_from,valid_until,status,created_at,updated_at,source_type,source_reference)
VALUES($1,$2,$3,$4,$5,$6,'REFUND_EXCEPTION_REPAIR','RefundException','sku-repair',$7,$8,$8,$9,$9,$10,'FROZEN',$9,$9,'PAYMENT',$11)`,
			[]any{facts.PackageID, ownerID, facts.WalletID, orderID, transactionID, lotID, facts.AmountFen,
				facts.CreditMicrocredits, now, now.Add(24 * time.Hour), "payment-transaction:" + transactionID.String()}},
		{`INSERT INTO entitlement_components(id,package_id,component_type,credit_lot_id,original_microcredits,available_microcredits,
credited_at,expires_at,status,created_at,updated_at)
VALUES($1,$2,'BASE',$3,$4,$4,$5,$6,'FROZEN',$5,$5)`,
			[]any{facts.ComponentID, facts.PackageID, lotID, facts.CreditMicrocredits, now, now.Add(24 * time.Hour)}},
		{`INSERT INTO refunds(id,payment_order_id,requester_id,merchant_refund_no,channel_refund_id,amount_fen,credit_microcredits,
status,idempotency_key,request_hash,reason,created_at,updated_at,payment_transaction_id,entitlement_package_id,
base_microcredits,bonus_microcredits,entitlement_snapshot,intent_version)
VALUES($1,$2,$3,$4,$5,$6,$7,'MANUAL_REVIEW',$8,'hash','provider refunded locally unreversed',$9,$9,$10,$11,$7,0,'{}',1)`,
			[]any{facts.RefundID, orderID, ownerID, "merchant-refund-" + facts.RefundID.String(), facts.ProviderRefundID,
				facts.AmountFen, facts.CreditMicrocredits, "refund-idempotency-" + facts.RefundID.String(), now, transactionID, facts.PackageID}},
		{`INSERT INTO refund_component_reservations(refund_id,component_id,package_id,component_type,credit_lot_id,
available_microcredits,active_grant_microcredits,suspended_grant_microcredits,prior_status,created_at)
VALUES($1,$2,$3,'BASE',$4,$5,0,0,'FROZEN',$6)`,
			[]any{facts.RefundID, facts.ComponentID, facts.PackageID, lotID, facts.CreditMicrocredits, now}},
		{`INSERT INTO payment_evidence(id,payment_order_id,payment_transaction_id,evidence_type,provider_id,provider_version,evidence_key,
merchant_order_no,provider_transaction_id,provider_refund_id,amount_fen,currency,provider_status,received_at,payload_digest,payload_snapshot,merge_result)
VALUES($1,$2,$3,'QUERY','test-provider','v1',$4,$5,$6,$7,$8,'CNY','SUCCESS',$9,$10,'{}','RECORDED')`,
			[]any{facts.EvidenceID, orderID, transactionID, "query:" + facts.EvidenceID.String(),
				"refund-exception-" + orderID.String(), "trade-" + transactionID.String(), facts.ProviderRefundID,
				facts.AmountFen, now, strings.Repeat("ab", 32)}},
		{`INSERT INTO manual_review_cases(id,domain,case_type,resource_type,resource_id,severity,status,created_at,claim_due_at,
first_response_due_at,mitigation_due_at,resolution_due_at,resolved_at,updated_at)
VALUES($1,'BILLING','PAYMENT_ANOMALY','REFUND',$2,'P0','RESOLVED',$3,$3,$3,$3,$3,$3,$3)`,
			[]any{facts.AnomalyCaseID, facts.RefundID, now}},
		{`INSERT INTO payment_anomaly_cases(id,anomaly_type,dedupe_key,payment_order_id,payment_transaction_id,payment_evidence_id,refund_id,
status,resolution_code,financial_action_status,resource_snapshot,version,created_at,updated_at,resolved_at)
VALUES($1,'REFUND_FAILURE',$2,$3,$4,$5,$6,'RESOLVED','ESCALATE_FINANCIAL_REPAIR','PENDING','{}',1,$7,$7,$7)`,
			[]any{facts.AnomalyCaseID, "refund:" + facts.RefundID.String() + ":local-conflict", orderID, transactionID,
				facts.EvidenceID, facts.RefundID, now}},
	} {
		if _, err := tx.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatalf("refund exception fixture statement %d: %v", index, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return facts
}
