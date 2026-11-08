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

type reconciliationRepairFixture struct {
	CaseID               uuid.UUID
	RunID                uuid.UUID
	EvidenceID           uuid.UUID
	MerchantOrderNo      string
	ChannelTransactionID string
	AmountFen            int64
	Currency             string
	ProviderRefundID     string
	BusinessDate         string
}

// seedProviderPaidInternalMissing writes one provider bill row the internal side
// never matched, plus its provider-side refund evidence: the frozen case carries
// no internal order, and the evidence row is a provider bill refund row with no
// internal attribution either.
func seedProviderPaidInternalMissing(t *testing.T, ctx context.Context, repository *PostgresRepository, suffix string) reconciliationRepairFixture {
	t.Helper()
	pool := repository.pool
	now := time.Now().UTC().Truncate(time.Second)
	businessDate := now.Format("2006-01-02")
	fixture := reconciliationRepairFixture{
		CaseID: uuid.New(), RunID: uuid.New(), EvidenceID: uuid.New(),
		MerchantOrderNo:      "unmatched-" + suffix + "-" + uuid.NewString(),
		ChannelTransactionID: "channel-tx-" + uuid.NewString(),
		AmountFen:            1500, Currency: "CNY", ProviderRefundID: "refund-" + uuid.NewString(),
		BusinessDate: businessDate,
	}
	snapshot, err := json.Marshal(map[string]any{
		"entry_type": "PAYMENT", "merchant_order_no": fixture.MerchantOrderNo,
		"channel_transaction_id": fixture.ChannelTransactionID, "channel": "ALIPAY",
		"amount_fen": fixture.AmountFen, "currency": fixture.Currency, "status": "SUCCESS",
		"paid_at": now.Format(time.RFC3339Nano),
	})
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
		{`INSERT INTO reconciliation_runs(id,channel,business_date,status,provider_row_count,case_count,started_at,completed_at,
provider_id,provider_version,source_mode)
VALUES($1,'ALIPAY',$2::date,'COMPLETED',1,1,$3,$3,'test-provider','v1','MANUAL_API')`,
			[]any{fixture.RunID, fixture.BusinessDate, now}},
		{`INSERT INTO manual_review_cases(id,domain,case_type,resource_type,resource_id,severity,status,created_at,claim_due_at,first_response_due_at,mitigation_due_at,resolution_due_at,updated_at) VALUES($1,'BILLING','RECONCILIATION_DIFFERENCE','RECONCILIATION_CASE',$1,'P1','OPEN',$2,$2::timestamptz+interval '1 hour',$2::timestamptz+interval '2 hours',$2::timestamptz+interval '3 hours',$2::timestamptz+interval '4 hours',$2)`, []any{fixture.CaseID, now}},
		{`INSERT INTO reconciliation_cases(id,reconciliation_run_id,payment_order_id,merchant_order_no,case_type,status,
severity,provider_snapshot,internal_snapshot,refund_snapshot,ledger_snapshot,manual_review_case_id,created_at,updated_at)
VALUES($1,$2,NULL,$3,'PROVIDER_PAID_INTERNAL_MISSING','OPEN','P1',$4,NULL,NULL,NULL,$1,$5,$5)`,
			[]any{fixture.CaseID, fixture.RunID, fixture.MerchantOrderNo, snapshot, now}},
	} {
		if _, err := tx.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatalf("reconciliation repair statement %d: %v", index, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	refundBill := ProviderBillEntry{EntryType: "REFUND", ProviderReferenceID: fixture.ProviderRefundID, MerchantOrderNo: fixture.MerchantOrderNo, ChannelTransactionID: fixture.ChannelTransactionID, Channel: "ALIPAY", AmountFen: fixture.AmountFen, Currency: fixture.Currency, Status: "SUCCESS", PaidAt: now}
	raw, _ := json.Marshal(refundBill)
	key := "REFUND|" + fixture.ProviderRefundID + "|" + fixture.MerchantOrderNo + "|" + now.Format(time.RFC3339Nano)
	if _, err := pool.Exec(ctx, `INSERT INTO reconciliation_provider_rows(reconciliation_run_id,row_number,entry_type,provider_reference_id,channel_transaction_id,merchant_order_no,channel,amount_fen,currency,status,paid_at,raw_snapshot,created_at,pipeline_evidence_key,pipeline_payload_digest) VALUES($1,1,'REFUND',$2,$3,$4,'ALIPAY',$5,'CNY','SUCCESS',$6,$7,$6,$8,$9)`, fixture.RunID, fixture.ProviderRefundID, fixture.ChannelTransactionID, fixture.MerchantOrderNo, fixture.AmountFen, now, raw, key, digestHex(raw)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO reconciliation_row_progress(run_id,row_number) VALUES($1,1)`, fixture.RunID); err != nil {
		t.Fatal(err)
	}
	p := &PaymentCoordinator{repository: repository, now: time.Now}
	if err := p.persistReadOnlyBillEvidence(ctx, ReconciliationRun{ID: fixture.RunID, ProviderID: "test-provider", ProviderVersion: "v1", ProviderRowCount: 1}); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT id FROM payment_evidence WHERE provider_id='test-provider' AND provider_version='v1' AND evidence_type='BILL' AND evidence_key=$1`, key).Scan(&fixture.EvidenceID); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// TestReconciliationFinancialRepairPostgresIntegration proves the GSA-34
// domain contract on real PostgreSQL: the closed plan recomputes the frozen
// difference, the closure resolves the case with zero accounting effect, the
// recheck gate refuses to close over another OPEN difference, the replay is
// idempotent and every negative stays at zero writes.
func TestReconciliationFinancialRepairPostgresIntegration(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("PINVOU_TEST_BILLING_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	repository, ctx := retainedFinanceRepository(t)
	version := reconciliationFixtureActivation(t, ctx, repository.pool)("ACTIVE")
	pool := repository.pool

	// The frozen zero-accounting baseline: any change of these counters during the
	// closure is a contract violation.
	var journalsBefore, postingsBefore, receivablesBefore, packagesBefore, lotsBefore int64
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM journal_entries), (SELECT count(*) FROM ledger_postings),
		(SELECT count(*) FROM receivables), (SELECT count(*) FROM entitlement_packages), (SELECT count(*) FROM credit_lots)`).
		Scan(&journalsBefore, &postingsBefore, &receivablesBefore, &packagesBefore, &lotsBefore); err != nil {
		t.Fatal(err)
	}

	fixture := seedProviderPaidInternalMissing(t, ctx, repository, "main")
	command := ReconciliationFinancialRepairCommandInput{ReconciliationCaseID: fixture.CaseID,
		PaymentEvidenceID: fixture.EvidenceID, BusinessDate: fixture.BusinessDate}
	plan, err := reconciliationFinancialRepairPlanFrom(ctx, pool, command, 1, 1, version)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.AmountFen != fixture.AmountFen || plan.Currency != fixture.Currency ||
		plan.ProviderTransactionID != fixture.ChannelTransactionID || plan.RecheckCaseCount != 0 ||
		plan.ProviderSnapshotDigest == "" || plan.FactsDigest == "" || plan.RunID != fixture.RunID {
		t.Fatalf("unexpected plan %+v", plan)
	}
	execute := func(requestID, eventID uuid.UUID, command ReconciliationFinancialRepairCommandInput, frozen ReconciliationFinancialRepairFrozenFacts) (ReconciliationFinancialRepairResult, error) {
		return repository.ApplyReconciliationFinancialRepair(ctx, ReconciliationFinancialRepairExecutionInput{
			ApprovalRequestID: requestID, AuthorizationEventID: eventID, ApprovalEpoch: 4,
			ExecutionKey: "reconciliation-repair:" + requestID.String(), PolicyVersion: 1, SchemaVersion: 1, ActivationVersion: version,
			Command: command, Frozen: frozen,
		})
	}
	frozen := ReconciliationFinancialRepairFrozenFacts{RunID: plan.RunID, AmountFen: plan.AmountFen,
		Currency: plan.Currency, ProviderTransactionID: plan.ProviderTransactionID,
		ProviderSnapshotDigest: plan.ProviderSnapshotDigest, FactsDigest: plan.FactsDigest}
	requestID, eventID := uuid.New(), uuid.New()
	applied, err := execute(requestID, eventID, command, frozen)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if applied.Status != "SUCCEEDED" || applied.LedgerEffect != "NONE" || applied.RecheckCaseCount != 0 ||
		applied.CaseID != fixture.CaseID || applied.RunID != fixture.RunID ||
		applied.ResolutionCode != "PROVIDER_REFUND_CONFIRMED" || applied.AmountFen != fixture.AmountFen {
		t.Fatalf("unexpected result %+v", applied)
	}
	var caseStatus, resolution string
	var resolvedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT status,COALESCE(resolution,''),resolved_at FROM reconciliation_cases WHERE id=$1`, fixture.CaseID).
		Scan(&caseStatus, &resolution, &resolvedAt); err != nil || caseStatus != "RESOLVED" ||
		resolution != "PROVIDER_REFUND_CONFIRMED" || resolvedAt == nil {
		t.Fatalf("case=%s/%s/%v err=%v", caseStatus, resolution, resolvedAt, err)
	}
	var caseRows, recheck int64
	if err := pool.QueryRow(ctx, `SELECT count(*),COALESCE(max(recheck_case_count),-1) FROM reconciliation_repair_cases WHERE reconciliation_case_id=$1`, fixture.CaseID).
		Scan(&caseRows, &recheck); err != nil || caseRows != 1 || recheck != 0 {
		t.Fatalf("repair cases=%d recheck=%d err=%v", caseRows, recheck, err)
	}
	// Zero accounting effect: every forbidden table keeps its exact counters.
	var journalsAfter, postingsAfter, receivablesAfter, packagesAfter, lotsAfter int64
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM journal_entries), (SELECT count(*) FROM ledger_postings),
		(SELECT count(*) FROM receivables), (SELECT count(*) FROM entitlement_packages), (SELECT count(*) FROM credit_lots)`).
		Scan(&journalsAfter, &postingsAfter, &receivablesAfter, &packagesAfter, &lotsAfter); err != nil ||
		journalsAfter != journalsBefore || postingsAfter != postingsBefore || receivablesAfter != receivablesBefore ||
		packagesAfter != packagesBefore || lotsAfter != lotsBefore {
		t.Fatalf("closure wrote accounting facts: journals %d->%d postings %d->%d receivables %d->%d packages %d->%d lots %d->%d",
			journalsBefore, journalsAfter, postingsBefore, postingsAfter, receivablesBefore, receivablesAfter,
			packagesBefore, packagesAfter, lotsBefore, lotsAfter)
	}
	// The same approval replays to the same case with no further effect.
	replayed, err := execute(requestID, eventID, command, frozen)
	if err != nil || replayed != applied {
		t.Fatalf("idempotent replay %+v err=%v want %+v", replayed, err, applied)
	}
	var totalCases int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM reconciliation_repair_cases WHERE reconciliation_case_id=$1`, fixture.CaseID).Scan(&totalCases); err != nil ||
		totalCases != 1 {
		t.Fatalf("idempotency cases=%d err=%v", totalCases, err)
	}

	// Negative: an amount-mismatched disposal evidence closes nothing.
	mismatched := seedProviderPaidInternalMissing(t, ctx, repository, "amount")
	// Payment evidence is immutable, so the mismatching row is inserted directly.
	mismatchedEvidenceID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO payment_evidence(id,payment_order_id,payment_transaction_id,evidence_type,provider_id,provider_version,
evidence_key,merchant_order_no,provider_transaction_id,provider_refund_id,amount_fen,currency,provider_status,
provider_event_at,received_at,payload_digest,payload_snapshot,merge_result)
VALUES($1,NULL,NULL,'BILL','test-provider','v1',$2,$3,$4,$5,$6,'CNY','SUCCESS',now(),now(),$7,'{}','REVIEW_REQUIRED')`,
		mismatchedEvidenceID, "bill|REFUND|mismatch|"+uuid.NewString(), mismatched.MerchantOrderNo,
		mismatched.ChannelTransactionID, "refund-"+uuid.NewString(), mismatched.AmountFen+1, strings.Repeat("cd", 32)); err != nil {
		t.Fatalf("mismatched evidence seed: %v", err)
	}
	if _, err := reconciliationFinancialRepairPlanFrom(ctx, pool, ReconciliationFinancialRepairCommandInput{
		ReconciliationCaseID: mismatched.CaseID, PaymentEvidenceID: mismatchedEvidenceID,
		BusinessDate: mismatched.BusinessDate}, 1, 1, 1); !errors.Is(err, ErrApprovalSnapshotMismatch) {
		t.Fatalf("amount-mismatched evidence err=%v", err)
	}
	// Negative: a second OPEN difference for the same merchant order in the same
	// run blocks the closure (repair-then-recheck gate).
	recheckFixture := seedProviderPaidInternalMissing(t, ctx, repository, "recheck")
	if _, err := pool.Exec(ctx, `INSERT INTO reconciliation_cases(id,reconciliation_run_id,payment_order_id,merchant_order_no,case_type,status,
severity,provider_snapshot,created_at,updated_at)
VALUES($1,$2,NULL,$3,'PROVIDER_PAID_INTERNAL_MISSING','OPEN','P1',$4,now(),now())`,
		uuid.New(), recheckFixture.RunID, recheckFixture.MerchantOrderNo, json.RawMessage(`{"entry_type":"PAYMENT"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciliationFinancialRepairPlanFrom(ctx, pool, ReconciliationFinancialRepairCommandInput{
		ReconciliationCaseID: recheckFixture.CaseID, PaymentEvidenceID: recheckFixture.EvidenceID,
		BusinessDate: recheckFixture.BusinessDate}, 1, 1, 1); !errors.Is(err, ErrConflict) {
		t.Fatalf("recheck gate err=%v", err)
	}
	// Negative: an internal attribution appearing later invalidates the closure.
	attributed := seedProviderPaidInternalMissing(t, ctx, repository, "attributed")
	ownerID := uuid.New()
	walletID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO wallets(id,owner_type,owner_id,status,posted_microcredits,version,created_at,updated_at)
VALUES($1,'PERSONAL',$2,'ACTIVE',0,1,now(),now())`, walletID, ownerID); err != nil {
		t.Fatalf("internal attribution wallet seed: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO payment_orders(id,merchant_order_no,requester_id,payer_user_id,wallet_id,product_id,product_snapshot,channel,
amount_fen,currency,credit_microcredits,status,fulfillment_status,expires_at,provider_payable_until,late_review_boundary_at,final_archive_eligible_at,
created_at,updated_at,provider_sku,provider_id,provider_version)
VALUES($1,$2,$3,$3,$4,'RECONCILE',$5,'ALIPAY',1500,'CNY',1500,'CREATED','PENDING',$6,$6,$6,$6,now(),now(),'sku','test-provider','v1')`,
		uuid.New(), attributed.MerchantOrderNo, ownerID, walletID, json.RawMessage(`{}`),
		time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatalf("internal attribution seed: %v", err)
	}
	if _, err := reconciliationFinancialRepairPlanFrom(ctx, pool, ReconciliationFinancialRepairCommandInput{
		ReconciliationCaseID: attributed.CaseID, PaymentEvidenceID: attributed.EvidenceID,
		BusinessDate: attributed.BusinessDate}, 1, 1, 1); !errors.Is(err, ErrApprovalSnapshotMismatch) {
		t.Fatalf("internal attribution err=%v", err)
	}
	var unresolvedCases int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM reconciliation_repair_cases WHERE reconciliation_case_id=$1`, fixture.CaseID).Scan(&unresolvedCases); err != nil ||
		unresolvedCases != 1 {
		t.Fatalf("negatives wrote cases=%d err=%v", unresolvedCases, err)
	}
}
