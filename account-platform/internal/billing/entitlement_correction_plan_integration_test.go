package billing

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestEntitlementCorrectionReadPlanUsesPaymentEvidenceAndRejectsUsePostgresIntegration(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("PINVOU_TEST_BILLING_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	repository, _ := entitlementRepositoryToVersion(t, ctx, databaseURL, 0)
	walletID, ownerID, orderID, transactionID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	lotID, packageID, walletAccountID, clearingAccountID, journalID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	now := time.Now().UTC()
	reference := "payment-transaction:" + transactionID.String()
	product, err := json.Marshal(Product{ProductID: "CORRECTION_TEST", AmountFen: 100, CreditMicrocredits: 100, BasePoints: 80, BonusPoints: 20, Currency: "CNY"})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := repository.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	statements := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO wallets(id,owner_type,owner_id,status,posted_microcredits,created_at,updated_at) VALUES($1,'PERSONAL',$2,'ACTIVE',100,$3,$3)`, []any{walletID, ownerID, now}},
		{`INSERT INTO ledger_accounts(id,wallet_id,code,account_type,created_at) VALUES($1,$2,$3,'WALLET_LIABILITY',$4)`, []any{walletAccountID, walletID, "correction-wallet:" + walletID.String(), now}},
		{`INSERT INTO ledger_accounts(id,code,account_type,created_at) VALUES($1,$2,'PAYMENT_CLEARING',$3)`, []any{clearingAccountID, "correction-clearing:" + walletID.String(), now}},
		{`INSERT INTO payment_orders(id,merchant_order_no,requester_id,payer_user_id,wallet_id,product_id,product_snapshot,channel,amount_fen,currency,credit_microcredits,status,fulfillment_status,expires_at,provider_payable_until,late_review_boundary_at,final_archive_eligible_at,created_at,updated_at,provider_id,provider_version) VALUES($1,$2,$3,$3,$4,'CORRECTION_TEST',$5,'ALIPAY',100,'CNY',100,'PAID','CREDITED',$6,$6,$7,$8,$9,$9,'test-provider','v1')`, []any{orderID, "correction-" + orderID.String(), ownerID, walletID, product, now.Add(time.Hour), now.Add(72 * time.Hour), now.Add(8 * 24 * time.Hour), now}},
		{`INSERT INTO payment_transactions(id,payment_order_id,channel,provider_transaction_id,status,amount_fen,currency,source,provider_paid_at,verified_at,created_at,updated_at) VALUES($1,$2,'ALIPAY',$3,'PAID',100,'CNY','QUERY',$4,$4,$4,$4)`, []any{transactionID, orderID, "correction-txn-" + transactionID.String(), now}},
		{`INSERT INTO credit_lots(id,wallet_id,source_type,source_reference,original_microcredits,remaining_microcredits,refundable,expires_at,created_at) VALUES($1,$2,'RECHARGE',$3,100,100,TRUE,$4,$5)`, []any{lotID, walletID, reference, now.Add(24 * time.Hour), now}},
		{`INSERT INTO entitlement_packages(id,user_id,wallet_id,payment_order_id,source_payment_transaction_id,credit_lot_id,product_id,plan_name,sku_code,amount_fen,points_total,points_remaining,credited_at,valid_from,valid_until,status,created_at,updated_at,source_type,source_reference) VALUES($1,$2,$3,$4,$5,$6,'CORRECTION_TEST','Test','',100,100,100,$7,$7,$8,'ACTIVE',$7,$7,'PAYMENT',$9)`, []any{packageID, ownerID, walletID, orderID, transactionID, lotID, now, now.Add(24 * time.Hour), reference}},
		{`INSERT INTO entitlement_components(id,package_id,component_type,credit_lot_id,original_microcredits,available_microcredits,credited_at,expires_at,status,created_at,updated_at) VALUES($1,$2,'BASE',$1,100,100,$3,$4,'ACTIVE',$3,$3)`, []any{lotID, packageID, now, now.Add(24 * time.Hour)}},
		{`INSERT INTO journal_entries(id,entry_type,business_reference,description,created_at) VALUES($1,'RECHARGE',$2,'fixture',$3)`, []any{journalID, reference, now}},
		{`INSERT INTO ledger_postings(id,journal_entry_id,ledger_account_id,delta_microcredits,credit_lot_id,created_at) VALUES($1,$2,$3,100,$4,$5)`, []any{uuid.New(), journalID, walletAccountID, lotID, now}},
		{`INSERT INTO ledger_postings(id,journal_entry_id,ledger_account_id,delta_microcredits,created_at) VALUES($1,$2,$3,-100,$4)`, []any{uuid.New(), journalID, clearingAccountID, now}},
	}
	for index, item := range statements {
		if _, err := tx.Exec(ctx, item.sql, item.args...); err != nil {
			t.Fatalf("fixture statement %d: %v", index, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	plan, err := repository.planEntitlementComponentCorrection(ctx, packageID, orderID)
	if err != nil || plan.PackageID != packageID || plan.SourceComponentID != lotID || plan.SourceLotID != lotID ||
		plan.Amounts.SourceType != "BASE" || plan.Amounts.TargetType != "BONUS" || plan.Amounts.Amount != 20 {
		t.Fatalf("original payment plan=%+v err=%v", plan, err)
	}
	packages, _, err := repository.ListPackages(ctx, ownerID, walletID, 10)
	if err != nil || len(packages) != 1 || len(packages[0].Components) != 1 ||
		packages[0].Components[0].OriginalMicrocredits != 100 ||
		packages[0].Components[0].ReclassifiedOutMicrocredits != 0 ||
		packages[0].Components[0].EffectiveOriginalMicrocredits != 100 {
		t.Fatalf("backward-compatible component projection=%+v err=%v", packages, err)
	}
	if _, err := repository.planEntitlementComponentCorrection(ctx, packageID, uuid.New()); !errors.Is(err, ErrConflict) {
		t.Fatalf("wrong source order accepted: %v", err)
	}
	if _, err := repository.pool.Exec(ctx, `UPDATE entitlement_components SET component_type='BONUS' WHERE id=$1`, lotID); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.pool.Exec(ctx, `UPDATE credit_lots SET refundable=FALSE,source_reference=$2 WHERE id=$1`, lotID, reference+":bonus"); err != nil {
		t.Fatal(err)
	}
	bonusPlan, err := repository.planEntitlementComponentCorrection(ctx, packageID, orderID)
	if err != nil || bonusPlan.Amounts.SourceType != "BONUS" || bonusPlan.Amounts.TargetType != "BASE" || bonusPlan.Amounts.Amount != 80 {
		t.Fatalf("bonus-to-base payment plan=%+v err=%v", bonusPlan, err)
	}
	if _, err := repository.pool.Exec(ctx, `UPDATE credit_lots SET refundable=TRUE WHERE id=$1`, lotID); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.planEntitlementComponentCorrection(ctx, packageID, orderID); !errors.Is(err, ErrConflict) {
		t.Fatalf("lot with wrong refundable attribution accepted: %v", err)
	}
	if _, err := repository.pool.Exec(ctx, `UPDATE entitlement_components SET component_type='BASE' WHERE id=$1`, lotID); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.pool.Exec(ctx, `UPDATE credit_lots SET source_reference=$2 WHERE id=$1`, lotID, reference); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.pool.Exec(ctx, `UPDATE payment_orders SET product_snapshot='{}'::jsonb WHERE id=$1`, orderID); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.planEntitlementComponentCorrection(ctx, packageID, orderID); !errors.Is(err, ErrConflict) {
		t.Fatalf("missing source snapshot accepted: %v", err)
	}
	if _, err := repository.pool.Exec(ctx, `UPDATE payment_orders SET product_snapshot=$2 WHERE id=$1`, orderID, product); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.pool.Exec(ctx, `UPDATE entitlement_components SET available_microcredits=99,used_microcredits=1 WHERE id=$1`, lotID); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.pool.Exec(ctx, `UPDATE credit_lots SET remaining_microcredits=99 WHERE id=$1`, lotID); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.planEntitlementComponentCorrection(ctx, packageID, orderID); !errors.Is(err, ErrConflict) {
		t.Fatalf("already-used component accepted: %v", err)
	}
	if _, err := repository.pool.Exec(ctx, `UPDATE entitlement_components SET available_microcredits=100,used_microcredits=0 WHERE id=$1`, lotID); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.pool.Exec(ctx, `UPDATE credit_lots SET remaining_microcredits=100 WHERE id=$1`, lotID); err != nil {
		t.Fatal(err)
	}
	noOpSnapshot, err := json.Marshal(Product{ProductID: "CORRECTION_TEST", AmountFen: 100, CreditMicrocredits: 100, BasePoints: 100, Currency: "CNY"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.pool.Exec(ctx, `UPDATE payment_orders SET product_snapshot=$2 WHERE id=$1`, orderID, noOpSnapshot); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.planEntitlementComponentCorrection(ctx, packageID, orderID); !errors.Is(err, ErrConflict) {
		t.Fatalf("already-correct composition accepted: %v", err)
	}
	if _, err := repository.pool.Exec(ctx, `UPDATE payment_orders SET product_snapshot=$2 WHERE id=$1`, orderID, product); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.pool.Exec(ctx, `INSERT INTO payment_order_repairs(id,payment_order_id,actor_id,reason,provider_snapshot,created_at,
entitlement_package_id,payment_transaction_id,points_awarded,wallet_balance_before,wallet_balance_after)
VALUES($1,$2,$3,'existing repair','{}',$4,$5,$6,100,0,100)`, uuid.New(), orderID, ownerID, now, packageID, transactionID); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.planEntitlementComponentCorrection(ctx, packageID, orderID); !errors.Is(err, ErrConflict) {
		t.Fatalf("order with repair evidence accepted: %v", err)
	}
}
