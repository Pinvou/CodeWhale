package billing

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/billingschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
)

// This is constraint-valid historical seed data, not a production create API.
func seedF12Repair(t *testing.T, ctx context.Context, repository *PostgresRepository, now time.Time, amountFen, credits, basePoints, bonusPoints int64, mergeResult, providerStatus string) (uuid.UUID, uuid.UUID, uuid.UUID) {
	t.Helper()
	pool := repository.pool
	orderID, walletID, evidenceID := uuid.New(), uuid.New(), uuid.New()
	walletAccountID := uuid.New()
	// Package issuance requires the personal wallet owner to be the order
	// requester.
	ownerID := uuid.New()
	product, err := json.Marshal(Product{ProductID: "PAYMENT_REPAIR", AmountFen: amountFen, CreditMicrocredits: credits,
		BasePoints: basePoints, BonusPoints: bonusPoints, Currency: "CNY", PlanName: "Repair"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO wallets(id,owner_type,owner_id,status,posted_microcredits,version,created_at,updated_at)
VALUES($1,'PERSONAL',$2,'ACTIVE',0,1,$3,$3)`, walletID, ownerID, now); err != nil {
		t.Fatal(err)
	}
	// Package issuance posts the wallet leg against the wallet liability
	// account, exactly as the real fulfillment path requires.
	if _, err := pool.Exec(ctx, `INSERT INTO ledger_accounts(id,wallet_id,code,account_type,created_at)
VALUES($1,$2,$3,'WALLET_LIABILITY',$4)`, walletAccountID, walletID, "wallet:"+walletID.String(), now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO payment_orders(id,merchant_order_no,requester_id,payer_user_id,wallet_id,product_id,product_snapshot,channel,
amount_fen,currency,credit_microcredits,status,fulfillment_status,expires_at,provider_payable_until,late_review_boundary_at,final_archive_eligible_at,
created_at,updated_at,provider_sku,provider_id,provider_version)
VALUES($1,$2,$3,$3,$4,'PAYMENT_REPAIR',$5,'ALIPAY',$6,'CNY',$7,'MANUAL_REVIEW','MANUAL_REVIEW',$8,$8,$9,$10,$11,$11,'sku-repair','test-provider','v1')`,
		orderID, "repair-"+orderID.String(), ownerID, walletID, product, amountFen, credits,
		now.Add(time.Hour), now.Add(72*time.Hour), now.Add(8*24*time.Hour), now); err != nil {
		t.Fatal(err)
	}
	providerFacts, err := json.Marshal(map[string]any{"external_customer_key": ownerID.String(), "external_sku": "sku-repair"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO payment_evidence(id,payment_order_id,evidence_type,provider_id,provider_version,evidence_key,
merchant_order_no,provider_transaction_id,amount_fen,currency,provider_status,provider_event_at,received_at,payload_digest,payload_snapshot,merge_result)
VALUES($1,$2,'QUERY','test-provider','v1',$3,$4,$5,$6,'CNY',$7,$8,$8,$9,$11,$10)`,
		evidenceID, orderID, "query:"+evidenceID.String(), "repair-"+orderID.String(), "trade-"+evidenceID.String(),
		amountFen, providerStatus, now, strings.Repeat("ab", 32), mergeResult, providerFacts); err != nil {
		t.Fatal(err)
	}
	// The matching success evidence and the MANUAL_REVIEW order state are
	// produced by a review-required transaction (late or duplicate payment);
	// the approved repair reuses the existing repair branch on that row.
	if _, err := pool.Exec(ctx, `INSERT INTO payment_transactions(id,payment_order_id,provider_id,provider_version,channel,
provider_transaction_id,status,fulfillment_status,amount_fen,currency,source,provider_status,provider_paid_at,verified_at,created_at,updated_at)
VALUES($1,$2,'test-provider','v1','ALIPAY',$3,'REVIEW_REQUIRED','REVIEW_REQUIRED',$4,'CNY','QUERY','PAY_SUCCESS',$5,$5,$5,$5)`,
		uuid.New(), orderID, "trade-"+evidenceID.String(), amountFen, now); err != nil {
		t.Fatal(err)
	}
	return orderID, walletID, evidenceID
}

// Freeze every original column of the order's financial rows. Later additive
// columns are legitimate; original row values, memberships and ledger rows are not.
func captureF12PaymentFacts(t *testing.T, ctx context.Context, r *PostgresRepository, order, wallet uuid.UUID) func() string {
	t.Helper()
	type projection struct {
		table, predicate string
		columns          []string
	}
	projections := []projection{
		{table: "wallets", predicate: "record.id=$2"},
		{table: "payment_orders", predicate: "record.id=$1"},
		{table: "payment_evidence", predicate: "record.payment_order_id=$1"},
		{table: "payment_transactions", predicate: "record.payment_order_id=$1"},
		{table: "payment_order_repairs", predicate: "record.payment_order_id=$1"},
		{table: "entitlement_packages", predicate: "record.payment_order_id=$1"},
		{table: "entitlement_components", predicate: "record.package_id IN(SELECT id FROM entitlement_packages WHERE payment_order_id=$1)"},
		{table: "credit_lots", predicate: "record.id IN(SELECT credit_lot_id FROM entitlement_packages WHERE payment_order_id=$1 UNION SELECT credit_lot_id FROM entitlement_components WHERE package_id IN(SELECT id FROM entitlement_packages WHERE payment_order_id=$1))"},
		{table: "journal_entries", predicate: "record.business_reference IN(SELECT 'payment-transaction:'||id::text FROM payment_transactions WHERE payment_order_id=$1 UNION SELECT 'repair-payment-transaction:'||id::text FROM payment_transactions WHERE payment_order_id=$1)"},
		{table: "ledger_postings", predicate: "record.journal_entry_id IN(SELECT id FROM journal_entries WHERE business_reference IN(SELECT 'payment-transaction:'||id::text FROM payment_transactions WHERE payment_order_id=$1 UNION SELECT 'repair-payment-transaction:'||id::text FROM payment_transactions WHERE payment_order_id=$1))"},
	}
	for i := range projections {
		if err := r.pool.QueryRow(ctx, `SELECT array_agg(attname::text ORDER BY attnum) FROM pg_attribute WHERE attrelid=$1::regclass AND attnum>0 AND NOT attisdropped`, "public."+projections[i].table).Scan(&projections[i].columns); err != nil {
			t.Fatal("capture original financial columns", err)
		}
	}
	return func() string {
		var snapshot strings.Builder
		for _, p := range projections {
			query := fmt.Sprintf(`SELECT COALESCE(jsonb_agg(projected ORDER BY projected::text),'[]'::jsonb)::text FROM (SELECT (SELECT jsonb_object_agg(key,value) FROM jsonb_each(to_jsonb(record)) WHERE key=ANY($3::text[])) AS projected FROM public.%s record WHERE (%s) AND $2::uuid IS NOT NULL AND $1::uuid IS NOT NULL) frozen`, p.table, p.predicate)
			var facts string
			if err := r.pool.QueryRow(ctx, query, order, wallet, p.columns).Scan(&facts); err != nil {
				t.Fatal("read original financial facts", err)
			}
			snapshot.WriteString(p.table + ":" + facts + "\n")
		}
		return snapshot.String()
	}
}

// The deployed observation showed Billing max900038; this controlled public
// carrier is an upgrade model, not a reconstructed production snapshot. Account
// transport is explicitly controlled here; the three-domain test owns real Account.
func TestF12PaymentRepairDeployedBaselineUpgradePostgres(t *testing.T) {
	raw := strings.TrimSpace(os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL"))
	if raw == "" {
		t.Skip("explicit owned Billing database required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	r, _ := entitlementRepositoryWithStage(t, ctx, raw, func(loaded []migrations.Migration) ([]migrations.Migration, error) {
		var prior []migrations.Migration
		for _, m := range loaded {
			if m.Version <= 900038 {
				prior = append(prior, m)
			}
		}
		return prior, nil
	})
	activation := paymentFixtureActivation(t, ctx, r.pool)("ACTIVE")
	c, err := newBillingFixturePaymentCoordinator(r, &paymentRepairStubProvider{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	inputFor := func(order, evidence uuid.UUID) PaymentFinancialRepairExecutionInput {
		plan, err := paymentFinancialRepairPlanFrom(ctx, r.pool, PaymentFinancialRepairCommandInput{PaymentOrderID: order, PaymentEvidenceID: evidence}, 1, 1, activation)
		if err != nil {
			t.Fatal("real F12 plan", err)
		}
		return PaymentFinancialRepairExecutionInput{ApprovalRequestID: uuid.New(), AuthorizationEventID: uuid.New(), ApprovalEpoch: 2, ApproverUserID: uuid.New(), RequesterUserID: uuid.New(), ExecutionKey: "upgrade-repair:" + uuid.NewString(), PolicyVersion: 1, SchemaVersion: 1, ActivationVersion: activation, Command: PaymentFinancialRepairCommandInput{PaymentOrderID: order, PaymentEvidenceID: evidence}, Frozen: PaymentFinancialRepairFrozenFacts{OrderUpdatedAt: plan.OrderUpdatedAt, AmountFen: plan.AmountFen, CreditMicrocredits: plan.CreditMicrocredits, ProviderTransactionID: plan.ProviderTransactionID, WalletID: plan.WalletID, WalletVersion: plan.WalletVersion, AppliedToReceivables: plan.AppliedToReceivables, FactsDigest: plan.FactsDigest}}
	}
	oldOrder, oldWallet, oldEvidence := seedF12Repair(t, ctx, r, now, 100, 100, 80, 20, "RECORDED", "PAY_SUCCESS")
	oldInput := inputFor(oldOrder, oldEvidence)
	oldReceipt, err := c.applyHistoricalF12PaymentRepair(ctx, oldInput, 900038)
	if err != nil {
		t.Fatal("explicit historical38 writer seed", err)
	}
	pendingOrder, pendingWallet, pendingEvidence := seedF12Repair(t, ctx, r, now, 100, 100, 80, 20, "RECORDED", "PAY_SUCCESS")
	pendingInput := inputFor(pendingOrder, pendingEvidence)
	oldFacts := captureF12PaymentFacts(t, ctx, r, oldOrder, oldWallet)
	pendingFacts := captureF12PaymentFacts(t, ctx, r, pendingOrder, pendingWallet)
	beforeOld, beforePending := oldFacts(), pendingFacts()
	loaded, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	operator := ledgerActivationFixtureOperator(t, ctx, r.pool)
	// Match the public migrator: the login selects the provisioned migration
	// role before reading the existing ledger or applying the next migration.
	if _, err := operator.Exec(ctx, `SET ROLE billing_service`); err != nil {
		t.Fatal("select public Billing migration role", err)
	}
	if err := billingschema.Up(ctx, operator, loaded); err != nil {
		t.Fatal("deployed38 public complete upgrade", err)
	}
	migrations.Unlock(ctx, operator)
	if _, err := migrations.VerifyApplied(ctx, operator, loaded); err != nil {
		t.Fatal("complete carrier verification", err)
	}
	if oldFacts() != beforeOld || pendingFacts() != beforePending {
		t.Fatal("public upgrade changed original financial facts")
	}
	if _, err := c.applyHistoricalF12PaymentRepair(ctx, oldInput, 900038); err == nil {
		t.Fatal("historical writer accepted the full current schema")
	}
	if oldFacts() != beforeOld {
		t.Fatal("historical writer refusal changed original financial facts")
	}
	replay, err := c.ApplyPaymentFinancialRepair(ctx, oldInput)
	if err != nil || replay != oldReceipt {
		t.Fatal("current API lost original historical receipt", err)
	}
	result, err := c.ApplyPaymentFinancialRepair(ctx, pendingInput)
	if err != nil {
		t.Fatal("current API repair after complete public upgrade", err)
	}
	confirmed := captureF12PaymentFacts(t, ctx, r, pendingOrder, pendingWallet)
	after := confirmed()
	again, err := c.ApplyPaymentFinancialRepair(ctx, pendingInput)
	if err != nil || again != result || confirmed() != after || oldFacts() != beforeOld {
		t.Fatal("current repair replay duplicated money or changed original receipt facts", err)
	}
	var posted int64
	var applied int
	if err := r.pool.QueryRow(ctx, `SELECT posted_microcredits,(SELECT count(*) FROM payment_credit_attempts WHERE payment_order_id=$1 AND state='APPLIED') FROM wallets WHERE id=$2`, pendingOrder, pendingWallet).Scan(&posted, &applied); err != nil || posted != 100 || applied != 1 {
		t.Fatal("current F12 financial conservation/admission differs", err)
	}
}
