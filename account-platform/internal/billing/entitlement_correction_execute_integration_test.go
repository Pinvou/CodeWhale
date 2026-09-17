package billing

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestEntitlementCorrectionExecuteAndConsumerReadPostgresIntegration(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("PINVOU_TEST_BILLING_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	repository, schema := newAdminPaginationIntegrationRepository(t, ctx, databaseURL)

	walletID, ownerID, orderID, transactionID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	lotID, packageID, walletAccountID, clearingAccountID, sourceJournalID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	now := time.Now().UTC()
	reference := "payment-transaction:" + transactionID.String()
	product, err := json.Marshal(Product{ProductID: "CORRECTION_EXECUTE_TEST", AmountFen: 100, CreditMicrocredits: 100, BasePoints: 80, BonusPoints: 20, Currency: "CNY"})
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
		{`INSERT INTO ledger_accounts(id,wallet_id,code,account_type,created_at) VALUES($1,$2,$3,'WALLET_LIABILITY',$4)`, []any{walletAccountID, walletID, "correction-execute-wallet:" + walletID.String(), now}},
		{`INSERT INTO ledger_accounts(id,code,account_type,created_at) VALUES($1,$2,'PAYMENT_CLEARING',$3)`, []any{clearingAccountID, "correction-execute-clearing:" + walletID.String(), now}},
		{`INSERT INTO payment_orders(id,merchant_order_no,requester_id,payer_user_id,wallet_id,product_id,product_snapshot,channel,amount_fen,currency,credit_microcredits,status,fulfillment_status,expires_at,provider_payable_until,late_review_boundary_at,final_archive_eligible_at,created_at,updated_at,provider_id,provider_version) VALUES($1,$2,$3,$3,$4,'CORRECTION_EXECUTE_TEST',$5,'ALIPAY',100,'CNY',100,'PAID','CREDITED',$6,$6,$7,$8,$9,$9,'test-provider','v1')`, []any{orderID, "correction-execute-" + orderID.String(), ownerID, walletID, product, now.Add(time.Hour), now.Add(72 * time.Hour), now.Add(8 * 24 * time.Hour), now}},
		{`INSERT INTO payment_transactions(id,payment_order_id,channel,provider_transaction_id,status,amount_fen,currency,source,provider_paid_at,verified_at,created_at,updated_at) VALUES($1,$2,'ALIPAY',$3,'PAID',100,'CNY','QUERY',$4,$4,$4,$4)`, []any{transactionID, orderID, "correction-execute-txn-" + transactionID.String(), now}},
		{`INSERT INTO credit_lots(id,wallet_id,source_type,source_reference,original_microcredits,remaining_microcredits,refundable,expires_at,created_at) VALUES($1,$2,'RECHARGE',$3,100,100,TRUE,$4,$5)`, []any{lotID, walletID, reference, now.Add(24 * time.Hour), now}},
		{`INSERT INTO entitlement_packages(id,user_id,wallet_id,payment_order_id,source_payment_transaction_id,credit_lot_id,product_id,plan_name,sku_code,amount_fen,points_total,points_remaining,credited_at,valid_from,valid_until,status,created_at,updated_at,source_type,source_reference) VALUES($1,$2,$3,$4,$5,$6,'CORRECTION_EXECUTE_TEST','Test','',100,100,100,$7,$7,$8,'ACTIVE',$7,$7,'PAYMENT',$9)`, []any{packageID, ownerID, walletID, orderID, transactionID, lotID, now, now.Add(24 * time.Hour), reference}},
		{`INSERT INTO entitlement_components(id,package_id,component_type,credit_lot_id,original_microcredits,available_microcredits,credited_at,expires_at,status,created_at,updated_at) VALUES($1,$2,'BASE',$1,100,100,$3,$4,'ACTIVE',$3,$3)`, []any{lotID, packageID, now, now.Add(24 * time.Hour)}},
		{`INSERT INTO journal_entries(id,entry_type,business_reference,description,created_at) VALUES($1,'RECHARGE',$2,'fixture',$3)`, []any{sourceJournalID, reference, now}},
		{`INSERT INTO ledger_postings(id,journal_entry_id,ledger_account_id,delta_microcredits,credit_lot_id,created_at) VALUES($1,$2,$3,100,$4,$5)`, []any{uuid.New(), sourceJournalID, walletAccountID, lotID, now}},
		{`INSERT INTO ledger_postings(id,journal_entry_id,ledger_account_id,delta_microcredits,created_at) VALUES($1,$2,$3,-100,$4)`, []any{uuid.New(), sourceJournalID, clearingAccountID, now}},
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
	if err != nil {
		t.Fatal(err)
	}
	approvalRequestID, authorizationEventID := uuid.New(), uuid.New()
	requesterID, approverID := uuid.New(), uuid.New()
	executionKey := "approval:" + approvalRequestID.String() + ":ENTITLEMENT_COMPONENT_ADJUSTMENT"
	reasonCode := "ORIGINAL_SPLIT_MISMATCH"
	frozen := entitlementCorrectionFrozenFacts{
		WalletID: plan.WalletID, WalletVersion: plan.WalletVersion, PaymentTransactionID: plan.PaymentTransactionID,
		SourceComponentID: plan.SourceComponentID, SourceLotID: plan.SourceLotID,
		TargetComponentID: plan.TargetComponentID, TargetLotID: plan.TargetLotID,
		SourceType: plan.Amounts.SourceType, TargetType: plan.Amounts.TargetType, AmountMicrocredits: plan.Amounts.Amount,
		SourceEvidenceDigest: plan.SourceEvidenceDigest, FactsDigest: plan.FactsDigest,
	}
	if !frozen.matches(plan) {
		t.Fatalf("frozen owner facts differ plan=%+v frozen=%+v", plan, frozen)
	}
	result, err := repository.ExecuteEntitlementComponentCorrection(ctx, approvalRequestID, authorizationEventID, 2,
		packageID, orderID, requesterID, approverID, executionKey, "independent approval completed", reasonCode, frozen)
	if err != nil || result.Status != "SUCCEEDED" || result.AmountMicrocredits != 20 || result.SourceComponentID != lotID ||
		result.SourceType != "BASE" || result.TargetType != "BONUS" || result.TargetComponentID == uuid.Nil || result.TargetLotID == uuid.Nil {
		t.Fatalf("correction result=%+v err=%v", result, err)
	}

	var sourceAvailable, sourceReclassified, sourceRemaining int64
	if err := repository.pool.QueryRow(ctx, `SELECT component.available_microcredits,component.reclassified_out_microcredits,lot.remaining_microcredits
FROM entitlement_components component JOIN credit_lots lot ON lot.id=component.credit_lot_id WHERE component.id=$1`, lotID).
		Scan(&sourceAvailable, &sourceReclassified, &sourceRemaining); err != nil || sourceAvailable != 80 || sourceReclassified != 20 || sourceRemaining != 80 {
		t.Fatalf("source after correction available=%d reclassified=%d lot=%d err=%v", sourceAvailable, sourceReclassified, sourceRemaining, err)
	}
	var targetAvailable, targetOriginal, targetRemaining int64
	var targetType, targetStatus string
	var targetRefundable bool
	var targetCreditedAt, targetExpiresAt time.Time
	if err := repository.pool.QueryRow(ctx, `SELECT component.component_type,component.available_microcredits,component.original_microcredits,component.status,
component.credited_at,component.expires_at,lot.remaining_microcredits,lot.refundable
FROM entitlement_components component JOIN credit_lots lot ON lot.id=component.credit_lot_id WHERE component.id=$1`, result.TargetComponentID).
		Scan(&targetType, &targetAvailable, &targetOriginal, &targetStatus, &targetCreditedAt, &targetExpiresAt, &targetRemaining, &targetRefundable); err != nil ||
		targetType != "BONUS" || targetAvailable != 20 || targetOriginal != 20 || targetStatus != "ACTIVE" ||
		targetRemaining != 20 || targetRefundable || !targetCreditedAt.Equal(plan.SourceCreditedAt) || !targetExpiresAt.Equal(plan.SourceExpiresAt) {
		t.Fatalf("target after correction type=%s available=%d original=%d status=%s remaining=%d refundable=%v err=%v", targetType, targetAvailable, targetOriginal, targetStatus, targetRemaining, targetRefundable, err)
	}

	var journalType, journalReference string
	var postingCount int
	var postingSum int64
	if err := repository.pool.QueryRow(ctx, `SELECT entry_type,business_reference FROM journal_entries WHERE id=$1`, result.JournalEntryID).Scan(&journalType, &journalReference); err != nil || journalType != "ADJUSTMENT" || journalReference != "approval-execution:"+approvalRequestID.String() {
		t.Fatalf("adjustment journal type=%s reference=%s err=%v", journalType, journalReference, err)
	}
	postingRows, err := repository.pool.Query(ctx, `SELECT credit_lot_id,delta_microcredits FROM ledger_postings WHERE journal_entry_id=$1 ORDER BY delta_microcredits`, result.JournalEntryID)
	if err != nil {
		t.Fatal(err)
	}
	deltas := map[uuid.UUID]int64{}
	for postingRows.Next() {
		var postingLot uuid.UUID
		var delta int64
		if err := postingRows.Scan(&postingLot, &delta); err != nil {
			postingRows.Close()
			t.Fatal(err)
		}
		deltas[postingLot] = delta
		postingCount++
		postingSum += delta
	}
	if closeErr := postingRows.Err(); closeErr != nil {
		t.Fatal(closeErr)
	}
	postingRows.Close()
	if postingCount != 2 || postingSum != 0 || deltas[lotID] != -20 || deltas[result.TargetLotID] != 20 {
		t.Fatalf("paired postings count=%d sum=%d deltas=%v", postingCount, postingSum, deltas)
	}

	var walletPosted int64
	var packageTotal int64
	var effectiveTotal int64
	if err := repository.pool.QueryRow(ctx, `SELECT posted_microcredits FROM wallets WHERE id=$1`, walletID).Scan(&walletPosted); err != nil || walletPosted != 100 {
		t.Fatalf("wallet liability changed: posted=%d err=%v", walletPosted, err)
	}
	if err := repository.pool.QueryRow(ctx, `SELECT points_total,COALESCE(sum(original_microcredits-reclassified_out_microcredits),0) FROM entitlement_packages p
JOIN entitlement_components c ON c.package_id=p.id WHERE p.id=$1 GROUP BY p.points_total`, packageID).Scan(&packageTotal, &effectiveTotal); err != nil || packageTotal != 100 || effectiveTotal != 100 {
		t.Fatalf("package effective total changed: total=%d effective=%d err=%v", packageTotal, effectiveTotal, err)
	}

	replayed, err := repository.ExecuteEntitlementComponentCorrection(ctx, approvalRequestID, authorizationEventID, 2,
		packageID, orderID, requesterID, approverID, executionKey, "independent approval completed", reasonCode, frozen)
	if err != nil || replayed != result {
		t.Fatalf("idempotent replay result=%+v original=%+v err=%v", replayed, result, err)
	}
	if _, err := repository.ExecuteEntitlementComponentCorrection(ctx, approvalRequestID, authorizationEventID, 2,
		packageID, orderID, requesterID, approverID, "different-key", "independent approval completed", reasonCode, frozen); !errors.Is(err, ErrConflict) {
		t.Fatalf("different execution key accepted: %v", err)
	}
	var correctionCount, adjustmentCount int
	if err := repository.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM entitlement_component_corrections WHERE approval_request_id=$1),
	(SELECT count(*) FROM journal_entries WHERE business_reference=$2)`, approvalRequestID, "approval-execution:"+approvalRequestID.String()).Scan(&correctionCount, &adjustmentCount); err != nil || correctionCount != 1 || adjustmentCount != 1 {
		t.Fatalf("replay created facts corrections=%d journals=%d err=%v", correctionCount, adjustmentCount, err)
	}

	readResult, err := repository.EntitlementCorrectionForRequest(ctx, approvalRequestID)
	if err != nil || readResult != result {
		t.Fatalf("consumer result-first read=%+v expected=%+v err=%v", readResult, result, err)
	}
	packages, _, err := repository.ListPackages(ctx, ownerID, walletID, 10)
	if err != nil || len(packages) != 1 || len(packages[0].Components) != 2 {
		t.Fatalf("package projection packages=%d components=%d err=%v", len(packages), len(packages[0].Components), err)
	}
	byType := map[string]EntitlementComponent{}
	for _, component := range packages[0].Components {
		byType[component.ComponentType] = component
	}
	if byType["BASE"].EffectiveOriginalMicrocredits != 80 || byType["BONUS"].EffectiveOriginalMicrocredits != 20 ||
		byType["BASE"].AvailableMicrocredits != 80 || byType["BONUS"].AvailableMicrocredits != 20 {
		t.Fatalf("effective projection base=%+v bonus=%+v", byType["BASE"], byType["BONUS"])
	}

	qualifiedCorrections := pgx.Identifier{schema, "entitlement_component_corrections"}.Sanitize()
	var canRead, canInsert, canUpdate, canDelete bool
	if err := repository.pool.QueryRow(ctx, `SELECT has_table_privilege('billing_service',$1,'SELECT'),has_table_privilege('billing_service',$1,'INSERT'),
has_table_privilege('billing_service',$1,'UPDATE'),has_table_privilege('billing_service',$1,'DELETE')`, qualifiedCorrections).Scan(&canRead, &canInsert, &canUpdate, &canDelete); err != nil || !canRead || !canInsert || canUpdate || canDelete {
		t.Fatalf("correction grants read=%v insert=%v update=%v delete=%v err=%v", canRead, canInsert, canUpdate, canDelete, err)
	}
	if _, err := repository.pool.Exec(ctx, `UPDATE `+qualifiedCorrections+` SET reason_code=reason_code`); err == nil {
		t.Fatal("immutable correction accepted UPDATE")
	}
	if _, err := repository.pool.Exec(ctx, `DELETE FROM `+qualifiedCorrections); err == nil {
		t.Fatal("immutable correction accepted DELETE")
	}

	loaded, err := loadBillingIntegrationMigrations("../../migrations/billing", schema)
	if err != nil {
		t.Fatal(err)
	}
	configuration, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	configuration.RuntimeParams["search_path"] = schema
	connection, err := pgx.ConnectConfig(ctx, configuration)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(context.Background())
	if err := migrations.DownOne(ctx, connection, loaded); err == nil || !strings.Contains(err.Error(), "rollback migration 58") {
		t.Fatalf("forward-only migration down returned %v", err)
	}
	var latestVersion int64
	if err := connection.QueryRow(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&latestVersion); err != nil || latestVersion != 58 {
		t.Fatalf("failed down changed migration version: %d err=%v", latestVersion, err)
	}
}
