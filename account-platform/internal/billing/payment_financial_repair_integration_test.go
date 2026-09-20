package billing

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/payment"
	"github.com/google/uuid"
)

// paymentRepairStubProvider only supplies capabilities: the repair path must
// never call the provider, and this stub fails the test if it does.
type paymentRepairStubProvider struct {
	calls int
}

func (p *paymentRepairStubProvider) Capabilities() payment.Capabilities {
	return payment.Capabilities{ProviderID: "test-provider", Version: "v1", Channels: []string{"ALIPAY"},
		OrderQuerySupported: true, MinimumQueryIntervalSeconds: 1, MaximumPayableWindowSeconds: 900,
		ManualRepairSupported: true}
}

func (p *paymentRepairStubProvider) Create(context.Context, payment.Order, string) (payment.ProviderOrder, error) {
	p.calls++
	return payment.ProviderOrder{}, errors.New("repair must not call the provider")
}

func (p *paymentRepairStubProvider) Get(context.Context, string) (payment.ProviderOrder, error) {
	p.calls++
	return payment.ProviderOrder{}, errors.New("repair must not call the provider")
}

func (p *paymentRepairStubProvider) Close(context.Context, string) (payment.ProviderOrder, error) {
	p.calls++
	return payment.ProviderOrder{}, errors.New("repair must not call the provider")
}

func (p *paymentRepairStubProvider) Refund(context.Context, string, string, int64) (payment.Refund, error) {
	p.calls++
	return payment.Refund{}, errors.New("repair must not call the provider")
}

func (p *paymentRepairStubProvider) Bill(context.Context, string, time.Time) ([]payment.BillEntry, error) {
	p.calls++
	return nil, errors.New("repair must not call the provider")
}

// TestPaymentFinancialRepairPostgresIntegration proves the GSA-28 domain
// contract on real PostgreSQL: one confirmed-payment repair reuses the audited
// merge writer from a persisted evidence row without any provider call, keeps
// the ledger/wallet/component conservation rules, applies open receivables in
// the existing order, and replays idempotently. Boundary, status and evidence
// negatives must fail closed with zero writes.
func TestPaymentFinancialRepairPostgresIntegration(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("PINVOU_TEST_BILLING_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	repository, _ := newAdminPaginationIntegrationRepository(t, ctx, databaseURL)
	pool := repository.pool
	provider := &paymentRepairStubProvider{}
	coordinator, err := NewPaymentCoordinatorWithProvider(repository, provider)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	// The approving identity the approval froze; every approval-driven audit row
	// must carry it as the actor.
	approvingUserID := uuid.New()
	requestingUserID := uuid.New()

	seed := func(amountFen, credits, basePoints, bonusPoints int64, mergeResult, providerStatus string) (uuid.UUID, uuid.UUID, uuid.UUID) {
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
		if _, err := pool.Exec(ctx, `INSERT INTO payment_evidence(id,payment_order_id,evidence_type,provider_id,provider_version,evidence_key,
merchant_order_no,provider_transaction_id,amount_fen,currency,provider_status,provider_event_at,received_at,payload_digest,payload_snapshot,merge_result)
VALUES($1,$2,'QUERY','test-provider','v1',$3,$4,$5,$6,'CNY',$7,$8,$8,$9,'{}',$10)`,
			evidenceID, orderID, "query:"+evidenceID.String(), "repair-"+orderID.String(), "trade-"+evidenceID.String(),
			amountFen, providerStatus, now, strings.Repeat("ab", 32), mergeResult); err != nil {
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
	orderID, walletID, evidenceID := seed(100, 100, 80, 20, "RECORDED", "PAY_SUCCESS")
	command := PaymentFinancialRepairCommandInput{PaymentOrderID: orderID, PaymentEvidenceID: evidenceID}
	plan, err := paymentFinancialRepairPlanFrom(ctx, pool, command, 1, 1, 1)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.AmountFen != 100 || plan.CreditMicrocredits != 100 || plan.BaseAmount != 80 || plan.BonusAmount != 20 ||
		plan.AppliedToReceivables != 0 || plan.FactsDigest == "" || plan.Provider.Status != "SUCCESS" ||
		plan.Provider.ChannelTransactionID == nil || plan.Provider.PaidAt == nil {
		t.Fatalf("unexpected plan %+v", plan)
	}
	// Evidence and status negatives must fail closed before any write. Persisted
	// payment evidence is immutable, so each negative uses its own seed.
	if _, err := paymentFinancialRepairPlanFrom(ctx, pool, PaymentFinancialRepairCommandInput{PaymentOrderID: orderID, PaymentEvidenceID: uuid.New()}, 1, 1, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown evidence err=%v", err)
	}
	reviewOrder, _, reviewEvidence := seed(100, 100, 80, 20, "REVIEW_REQUIRED", "PAY_SUCCESS")
	if _, err := paymentFinancialRepairPlanFrom(ctx, pool, PaymentFinancialRepairCommandInput{PaymentOrderID: reviewOrder, PaymentEvidenceID: reviewEvidence}, 1, 1, 1); !errors.Is(err, ErrApprovalSnapshotMismatch) {
		t.Fatalf("review-required evidence err=%v", err)
	}
	failedOrder, _, failedEvidence := seed(100, 100, 80, 20, "RECORDED", "NOTPAY")
	if _, err := paymentFinancialRepairPlanFrom(ctx, pool, PaymentFinancialRepairCommandInput{PaymentOrderID: failedOrder, PaymentEvidenceID: failedEvidence}, 1, 1, 1); !errors.Is(err, ErrApprovalSnapshotMismatch) {
		t.Fatalf("non-success evidence err=%v", err)
	}
	paidOrder, _, paidEvidence := seed(100, 100, 80, 20, "RECORDED", "PAY_SUCCESS")
	if _, err := pool.Exec(ctx, `UPDATE payment_orders SET status='PAID' WHERE id=$1`, paidOrder); err != nil {
		t.Fatal(err)
	}
	if _, err := paymentFinancialRepairPlanFrom(ctx, pool, PaymentFinancialRepairCommandInput{PaymentOrderID: paidOrder, PaymentEvidenceID: paidEvidence}, 1, 1, 1); !errors.Is(err, ErrApprovalSnapshotMismatch) {
		t.Fatalf("already-paid order err=%v", err)
	}

	frozenFacts := func(plan PaymentFinancialRepairPlan) PaymentFinancialRepairFrozenFacts {
		return PaymentFinancialRepairFrozenFacts{
			OrderUpdatedAt: plan.OrderUpdatedAt, AmountFen: plan.AmountFen,
			CreditMicrocredits: plan.CreditMicrocredits, ProviderTransactionID: plan.ProviderTransactionID,
			WalletID: plan.WalletID, WalletVersion: plan.WalletVersion,
			AppliedToReceivables: plan.AppliedToReceivables,
		}
	}
	// The frozen projection is what an approval would have captured; the
	// owner-only facts digest stays empty and is recomputed in the execution
	// transaction, and an idempotent replay reuses the same frozen facts.
	execute := func(requestID, eventID uuid.UUID, order, evidence uuid.UUID, frozen PaymentFinancialRepairFrozenFacts) (PaymentFinancialRepairResult, error) {
		cmd := PaymentFinancialRepairCommandInput{PaymentOrderID: order, PaymentEvidenceID: evidence}
		return coordinator.ApplyPaymentFinancialRepair(ctx, PaymentFinancialRepairExecutionInput{
			ApprovalRequestID: requestID, AuthorizationEventID: eventID, ApprovalEpoch: 2,
			ApproverUserID: approvingUserID, RequesterUserID: requestingUserID,
			ExecutionKey: "payment-repair:" + requestID.String(), PolicyVersion: 1, SchemaVersion: 1, ActivationVersion: 1,
			Command: cmd, Frozen: frozen,
		})
	}
	// The reused audit table requires BOTH sides of the independent approval
	// (design §4 item 5, 000077), so a nil approver, a nil requester and a
	// requester that equals the approver must all be refused outright.
	if _, err := coordinator.ApplyPaymentFinancialRepair(ctx, PaymentFinancialRepairExecutionInput{
		ApprovalRequestID: uuid.New(), AuthorizationEventID: uuid.New(), ApprovalEpoch: 2,
		ExecutionKey: "payment-repair:no-approver", PolicyVersion: 1, SchemaVersion: 1, ActivationVersion: 1,
		Command: PaymentFinancialRepairCommandInput{PaymentOrderID: orderID, PaymentEvidenceID: evidenceID},
		Frozen:  frozenFacts(plan),
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("execution without an approving identity err=%v", err)
	}
	if _, err := coordinator.ApplyPaymentFinancialRepair(ctx, PaymentFinancialRepairExecutionInput{
		ApprovalRequestID: uuid.New(), AuthorizationEventID: uuid.New(), ApprovalEpoch: 2,
		ApproverUserID: approvingUserID,
		ExecutionKey:   "payment-repair:no-requester", PolicyVersion: 1, SchemaVersion: 1, ActivationVersion: 1,
		Command: PaymentFinancialRepairCommandInput{PaymentOrderID: orderID, PaymentEvidenceID: evidenceID},
		Frozen:  frozenFacts(plan),
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("execution without a requesting identity err=%v", err)
	}
	if _, err := coordinator.ApplyPaymentFinancialRepair(ctx, PaymentFinancialRepairExecutionInput{
		ApprovalRequestID: uuid.New(), AuthorizationEventID: uuid.New(), ApprovalEpoch: 2,
		ApproverUserID: approvingUserID, RequesterUserID: approvingUserID,
		ExecutionKey: "payment-repair:self-approved", PolicyVersion: 1, SchemaVersion: 1, ActivationVersion: 1,
		Command: PaymentFinancialRepairCommandInput{PaymentOrderID: orderID, PaymentEvidenceID: evidenceID},
		Frozen:  frozenFacts(plan),
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("execution whose requester approved itself err=%v", err)
	}
	requestID, eventID := uuid.New(), uuid.New()
	applied, err := execute(requestID, eventID, orderID, evidenceID, frozenFacts(plan))
	if err != nil {
		var reviewEvidence, transactions, packages int64
		_ = pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM payment_evidence WHERE payment_order_id=$1 AND merge_result='REVIEW_REQUIRED'),
			(SELECT count(*) FROM payment_transactions WHERE payment_order_id=$1),
			(SELECT count(*) FROM entitlement_packages WHERE payment_order_id=$1) FROM payment_orders WHERE id=$1`, orderID).
			Scan(&reviewEvidence, &transactions, &packages)
		t.Fatalf("apply: %v (conflict=%v mismatch=%v notfound=%v) review=%d txns=%d packages=%d",
			err, errors.Is(err, ErrConflict), errors.Is(err, ErrApprovalSnapshotMismatch), errors.Is(err, ErrNotFound),
			reviewEvidence, transactions, packages)
	}
	if applied.Status != "SUCCEEDED" || applied.CreditMicrocredits != 100 || applied.AppliedToReceivables != 0 ||
		applied.PaymentTransactionID == uuid.Nil || applied.EntitlementPackageID == uuid.Nil || applied.JournalEntryID == uuid.Nil {
		t.Fatalf("unexpected result %+v", applied)
	}
	var orderStatus, fulfillmentStatus string
	var posted, walletVersion int64
	if err := pool.QueryRow(ctx, `SELECT status,fulfillment_status FROM payment_orders WHERE id=$1`, orderID).Scan(&orderStatus, &fulfillmentStatus); err != nil ||
		orderStatus != "PAID" || fulfillmentStatus != "CREDITED" {
		t.Fatalf("order=%s/%s err=%v", orderStatus, fulfillmentStatus, err)
	}
	if err := pool.QueryRow(ctx, `SELECT posted_microcredits,version FROM wallets WHERE id=$1`, walletID).Scan(&posted, &walletVersion); err != nil ||
		posted != 100 || walletVersion != 2 {
		t.Fatalf("wallet=%d/%d err=%v", posted, walletVersion, err)
	}
	var entryType, reference string
	var postingCount int
	var sumDelta int64
	if err := pool.QueryRow(ctx, `SELECT entry_type,business_reference FROM journal_entries WHERE id=$1`, applied.JournalEntryID).
		Scan(&entryType, &reference); err != nil || entryType != "REPAIR_CREDIT" ||
		!strings.HasPrefix(reference, "repair-payment-transaction:") {
		t.Fatalf("journal=%s/%s err=%v", entryType, reference, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*),COALESCE(sum(delta_microcredits),0) FROM ledger_postings WHERE journal_entry_id=$1`, applied.JournalEntryID).
		Scan(&postingCount, &sumDelta); err != nil || postingCount < 2 || sumDelta != 0 {
		t.Fatalf("postings=%d sum=%d err=%v", postingCount, sumDelta, err)
	}
	var baseTotal, bonusTotal int64
	if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(original_microcredits) FILTER (WHERE component_type='BASE'),0),
COALESCE(sum(original_microcredits) FILTER (WHERE component_type='BONUS'),0) FROM entitlement_components WHERE package_id=$1`, applied.EntitlementPackageID).
		Scan(&baseTotal, &bonusTotal); err != nil || baseTotal != 80 || bonusTotal != 20 {
		t.Fatalf("components base=%d bonus=%d err=%v", baseTotal, bonusTotal, err)
	}
	var pointsAwarded, balanceBefore, balanceAfter, approvalEpoch int64
	var linkRequestID, linkEventID, actorID, requesterID uuid.UUID
	var factsDigest, executionKeyDigest string
	if err := pool.QueryRow(ctx, `SELECT points_awarded,wallet_balance_before,wallet_balance_after,
COALESCE(approval_request_id,'00000000-0000-0000-0000-000000000000'),COALESCE(authorization_event_id,'00000000-0000-0000-0000-000000000000'),
COALESCE(approval_epoch,0),COALESCE(execution_key_digest,''),COALESCE(facts_digest,''),actor_id,
COALESCE(requester_user_id,'00000000-0000-0000-0000-000000000000')
FROM payment_order_repairs WHERE approval_request_id=$1`, requestID).
		Scan(&pointsAwarded, &balanceBefore, &balanceAfter, &linkRequestID, &linkEventID, &approvalEpoch, &executionKeyDigest, &factsDigest, &actorID, &requesterID); err != nil ||
		pointsAwarded != 100 || balanceBefore != 0 || balanceAfter != 100 || linkRequestID != requestID || linkEventID != eventID ||
		approvalEpoch != 2 || !validHexDigest(executionKeyDigest) || factsDigest != applied.FactsDigest ||
		actorID != approvingUserID || requesterID != requestingUserID {
		t.Fatalf("repair row points=%d before=%d after=%d links=%v/%v epoch=%d actor=%v requester=%v err=%v",
			pointsAwarded, balanceBefore, balanceAfter, linkRequestID, linkEventID, approvalEpoch, actorID, requesterID, err)
	}
	// The operational path keeps writing rows with NULL approval linkage.
	var operationalRows, unattributedRows int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM payment_order_repairs WHERE approval_request_id IS NULL`).Scan(&operationalRows); err != nil {
		t.Fatal(err)
	}
	// 000077 keeps the requester all-or-none with the approval linkage, so no
	// operational row may carry one.
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM payment_order_repairs WHERE requester_user_id IS NOT NULL`).Scan(&unattributedRows); err != nil {
		t.Fatal(err)
	}
	if unattributedRows != 1 {
		t.Fatalf("approval-driven rows with a recorded requester=%d", unattributedRows)
	}

	replayed, err := execute(requestID, eventID, orderID, evidenceID, frozenFacts(plan))
	if err != nil || replayed != applied {
		t.Fatalf("idempotent replay %+v err=%v want %+v", replayed, err, applied)
	}
	var repairRows, transactions, packages int64
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM payment_order_repairs WHERE payment_order_id=$1),
		(SELECT count(*) FROM payment_transactions WHERE payment_order_id=$1),
		(SELECT count(*) FROM entitlement_packages WHERE payment_order_id=$1)`, orderID).Scan(&repairRows, &transactions, &packages); err != nil ||
		repairRows != 1 || transactions != 1 || packages != 1 {
		t.Fatalf("idempotency repairs=%d transactions=%d packages=%d err=%v", repairRows, transactions, packages, err)
	}

	// A second order whose BASE component settles an open receivable must apply
	// the same order-dependent settlement rule and record it in the audit row.
	orderTwo, walletTwo, evidenceTwo := seed(100, 100, 100, 0, "RECORDED", "PAY_SUCCESS")
	if _, err := pool.Exec(ctx, `INSERT INTO receivables(id,wallet_id,source_type,source_reference,original_microcredits,outstanding_microcredits,status,created_at,updated_at)
VALUES($1,$2,'USAGE_OVERAGE',$3,40,40,'OPEN',$4,$4)`, uuid.New(), walletTwo, "usage:"+uuid.NewString(), now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE wallets SET receivable_microcredits=40,financial_status='RECEIVABLE_OPEN',financial_freeze_reason='USAGE_OVERAGE' WHERE id=$1`, walletTwo); err != nil {
		t.Fatal(err)
	}
	requestTwoID := uuid.New()
	planTwo, err := paymentFinancialRepairPlanFrom(ctx, pool, PaymentFinancialRepairCommandInput{PaymentOrderID: orderTwo, PaymentEvidenceID: evidenceTwo}, 1, 1, 1)
	if err != nil {
		t.Fatalf("plan two: %v", err)
	}
	appliedTwo, err := execute(requestTwoID, uuid.New(), orderTwo, evidenceTwo, frozenFacts(planTwo))
	if err != nil {
		t.Fatalf("apply with receivable: %v", err)
	}
	if appliedTwo.AppliedToReceivables != 40 || appliedTwo.CreditMicrocredits != 100 {
		t.Fatalf("receivable application %+v", appliedTwo)
	}
	var outstanding, settled int64
	var receivableStatus, financialStatus string
	if err := pool.QueryRow(ctx, `SELECT outstanding_microcredits,settled_microcredits,status FROM receivables WHERE wallet_id=$1`, walletTwo).
		Scan(&outstanding, &settled, &receivableStatus); err != nil || outstanding != 0 || settled != 40 || receivableStatus != "SETTLED" {
		t.Fatalf("receivable=%d/%d/%s err=%v", outstanding, settled, receivableStatus, err)
	}
	var receivableCredits int64
	if err := pool.QueryRow(ctx, `SELECT posted_microcredits,receivable_microcredits,financial_status FROM wallets WHERE id=$1`, walletTwo).
		Scan(&posted, &receivableCredits, &financialStatus); err != nil || posted != 60 || receivableCredits != 0 || financialStatus != "CLEAR" {
		t.Fatalf("wallet two=%d/%s err=%v", posted, financialStatus, err)
	}
	var allocationTotal int64
	if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(amount_microcredits),0) FROM receivable_allocations WHERE wallet_id=$1 AND source_type='CASH_RECHARGE_BASE'`, walletTwo).
		Scan(&allocationTotal); err != nil || allocationTotal != 40 {
		t.Fatalf("allocation=%d err=%v", allocationTotal, err)
	}
	read, err := repository.PaymentFinancialRepairForRequest(ctx, requestTwoID)
	if err != nil || read != appliedTwo {
		t.Fatalf("result-first read %+v err=%v want %+v", read, err, appliedTwo)
	}
	if _, err := repository.PaymentFinancialRepairForRequest(ctx, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("absent repair err=%v", err)
	}
}
