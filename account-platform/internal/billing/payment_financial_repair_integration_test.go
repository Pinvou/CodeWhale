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
	setActivation := paymentFixtureActivation(t, ctx, pool)
	activationVersion := setActivation("ACTIVE")
	provider := &paymentRepairStubProvider{}
	coordinator, err := newBillingFixturePaymentCoordinator(repository, provider)
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
	orderID, walletID, evidenceID := seed(100, 100, 80, 20, "RECORDED", "PAY_SUCCESS")
	command := PaymentFinancialRepairCommandInput{PaymentOrderID: orderID, PaymentEvidenceID: evidenceID}
	plan, err := paymentFinancialRepairPlanFrom(ctx, pool, command, 1, 1, activationVersion)
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
	if _, err := paymentFinancialRepairPlanFrom(ctx, pool, PaymentFinancialRepairCommandInput{PaymentOrderID: orderID, PaymentEvidenceID: uuid.New()}, 1, 1, activationVersion); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown evidence err=%v", err)
	}
	reviewOrder, _, reviewEvidence := seed(100, 100, 80, 20, "REVIEW_REQUIRED", "PAY_SUCCESS")
	if _, err := paymentFinancialRepairPlanFrom(ctx, pool, PaymentFinancialRepairCommandInput{PaymentOrderID: reviewOrder, PaymentEvidenceID: reviewEvidence}, 1, 1, activationVersion); !errors.Is(err, ErrApprovalSnapshotMismatch) {
		t.Fatalf("review-required evidence err=%v", err)
	}
	failedOrder, _, failedEvidence := seed(100, 100, 80, 20, "RECORDED", "NOTPAY")
	if _, err := paymentFinancialRepairPlanFrom(ctx, pool, PaymentFinancialRepairCommandInput{PaymentOrderID: failedOrder, PaymentEvidenceID: failedEvidence}, 1, 1, activationVersion); !errors.Is(err, ErrApprovalSnapshotMismatch) {
		t.Fatalf("non-success evidence err=%v", err)
	}
	paidOrder, _, paidEvidence := seed(100, 100, 80, 20, "RECORDED", "PAY_SUCCESS")
	if _, err := pool.Exec(ctx, `UPDATE payment_orders SET status='PAID' WHERE id=$1`, paidOrder); err != nil {
		t.Fatal(err)
	}
	if _, err := paymentFinancialRepairPlanFrom(ctx, pool, PaymentFinancialRepairCommandInput{PaymentOrderID: paidOrder, PaymentEvidenceID: paidEvidence}, 1, 1, activationVersion); !errors.Is(err, ErrApprovalSnapshotMismatch) {
		t.Fatalf("already-paid order err=%v", err)
	}

	frozenFacts := func(plan PaymentFinancialRepairPlan) PaymentFinancialRepairFrozenFacts {
		return PaymentFinancialRepairFrozenFacts{
			OrderUpdatedAt: plan.OrderUpdatedAt, AmountFen: plan.AmountFen,
			CreditMicrocredits: plan.CreditMicrocredits, ProviderTransactionID: plan.ProviderTransactionID,
			WalletID: plan.WalletID, WalletVersion: plan.WalletVersion,
			AppliedToReceivables: plan.AppliedToReceivables, FactsDigest: plan.FactsDigest,
		}
	}
	// Freeze the digest actually published by the owner proof. Committed replay
	// reuses the original identity and frozen facts before fresh-effect gates.
	execute := func(requestID, eventID uuid.UUID, order, evidence uuid.UUID, frozen PaymentFinancialRepairFrozenFacts) (PaymentFinancialRepairResult, error) {
		cmd := PaymentFinancialRepairCommandInput{PaymentOrderID: order, PaymentEvidenceID: evidence}
		return coordinator.ApplyPaymentFinancialRepair(ctx, PaymentFinancialRepairExecutionInput{
			ApprovalRequestID: requestID, AuthorizationEventID: eventID, ApprovalEpoch: 2,
			ApproverUserID: approvingUserID, RequesterUserID: requestingUserID,
			ExecutionKey: "payment-repair:" + requestID.String(), PolicyVersion: 1, SchemaVersion: 1, ActivationVersion: activationVersion,
			Command: cmd, Frozen: frozen,
		})
	}
	// The reused audit table requires BOTH sides of the independent approval
	// (design §4 item 5, 000077), so a nil approver, a nil requester and a
	// requester that equals the approver must all be refused outright.
	if _, err := coordinator.ApplyPaymentFinancialRepair(ctx, PaymentFinancialRepairExecutionInput{
		ApprovalRequestID: uuid.New(), AuthorizationEventID: uuid.New(), ApprovalEpoch: 2,
		ExecutionKey: "payment-repair:no-approver", PolicyVersion: 1, SchemaVersion: 1, ActivationVersion: activationVersion,
		Command: PaymentFinancialRepairCommandInput{PaymentOrderID: orderID, PaymentEvidenceID: evidenceID},
		Frozen:  frozenFacts(plan),
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("execution without an approving identity err=%v", err)
	}
	if _, err := coordinator.ApplyPaymentFinancialRepair(ctx, PaymentFinancialRepairExecutionInput{
		ApprovalRequestID: uuid.New(), AuthorizationEventID: uuid.New(), ApprovalEpoch: 2,
		ApproverUserID: approvingUserID,
		ExecutionKey:   "payment-repair:no-requester", PolicyVersion: 1, SchemaVersion: 1, ActivationVersion: activationVersion,
		Command: PaymentFinancialRepairCommandInput{PaymentOrderID: orderID, PaymentEvidenceID: evidenceID},
		Frozen:  frozenFacts(plan),
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("execution without a requesting identity err=%v", err)
	}
	if _, err := coordinator.ApplyPaymentFinancialRepair(ctx, PaymentFinancialRepairExecutionInput{
		ApprovalRequestID: uuid.New(), AuthorizationEventID: uuid.New(), ApprovalEpoch: 2,
		ApproverUserID: approvingUserID, RequesterUserID: approvingUserID,
		ExecutionKey: "payment-repair:self-approved", PolicyVersion: 1, SchemaVersion: 1, ActivationVersion: activationVersion,
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
	// This fixture executes the governed path only; it must not also create
	// an operational row with NULL approval linkage.
	var operationalRows, unattributedRows int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM payment_order_repairs WHERE approval_request_id IS NULL`).Scan(&operationalRows); err != nil || operationalRows != 0 {
		t.Fatalf("governed fixture created operational rows=%d err=%v", operationalRows, err)
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
	planTwo, err := paymentFinancialRepairPlanFrom(ctx, pool, PaymentFinancialRepairCommandInput{PaymentOrderID: orderTwo, PaymentEvidenceID: evidenceTwo}, 1, 1, activationVersion)
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

	// Existing min(BASE, outstanding) behavior is correct. These are real
	// saturation coverage additions, not a claim of a new production cap bug.
	fullFinancialSnapshot := func() string {
		t.Helper()
		var value string
		if err := pool.QueryRow(ctx, `SELECT jsonb_build_array(
 (SELECT jsonb_agg(to_jsonb(x) ORDER BY to_jsonb(x)::text) FROM wallets x),
 (SELECT jsonb_agg(to_jsonb(x) ORDER BY to_jsonb(x)::text) FROM receivables x),
 (SELECT jsonb_agg(to_jsonb(x) ORDER BY to_jsonb(x)::text) FROM payment_orders x),
 (SELECT jsonb_agg(to_jsonb(x) ORDER BY to_jsonb(x)::text) FROM payment_transactions x),
 (SELECT jsonb_agg(to_jsonb(x) ORDER BY to_jsonb(x)::text) FROM payment_evidence x),
 (SELECT jsonb_agg(to_jsonb(x) ORDER BY to_jsonb(x)::text) FROM payment_order_repairs x),
 (SELECT jsonb_agg(to_jsonb(x) ORDER BY to_jsonb(x)::text) FROM entitlement_packages x),
 (SELECT jsonb_agg(to_jsonb(x) ORDER BY to_jsonb(x)::text) FROM entitlement_components x),
 (SELECT jsonb_agg(to_jsonb(x) ORDER BY to_jsonb(x)::text) FROM credit_lots x),
 (SELECT jsonb_agg(to_jsonb(x) ORDER BY to_jsonb(x)::text) FROM journal_entries x),
 (SELECT jsonb_agg(to_jsonb(x) ORDER BY to_jsonb(x)::text) FROM ledger_postings x),
 (SELECT jsonb_agg(to_jsonb(x) ORDER BY to_jsonb(x)::text) FROM receivable_allocations x),
 (SELECT jsonb_agg(to_jsonb(x) ORDER BY to_jsonb(x)::text) FROM outbox_events x))::text`).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	// An already-bound approval is not a fresh repair opportunity. Match every
	// persisted approval/evidence/actor identity before returning its receipt.
	baseReplay := PaymentFinancialRepairExecutionInput{
		ApprovalRequestID: requestID, AuthorizationEventID: eventID, ApprovalEpoch: 2,
		ApproverUserID: approvingUserID, RequesterUserID: requestingUserID,
		ExecutionKey: "payment-repair:" + requestID.String(), PolicyVersion: 1, SchemaVersion: 1, ActivationVersion: activationVersion,
		Command: command, Frozen: frozenFacts(plan),
	}
	for _, scenario := range []struct {
		name   string
		change func(*PaymentFinancialRepairExecutionInput)
	}{
		{"authorization-event", func(input *PaymentFinancialRepairExecutionInput) { input.AuthorizationEventID = uuid.New() }},
		{"approval-epoch", func(input *PaymentFinancialRepairExecutionInput) { input.ApprovalEpoch++ }},
		{"execution-key", func(input *PaymentFinancialRepairExecutionInput) { input.ExecutionKey += ":changed" }},
		{"order", func(input *PaymentFinancialRepairExecutionInput) { input.Command.PaymentOrderID = uuid.New() }},
		{"evidence", func(input *PaymentFinancialRepairExecutionInput) { input.Command.PaymentEvidenceID = uuid.New() }},
		{"requester", func(input *PaymentFinancialRepairExecutionInput) { input.RequesterUserID = uuid.New() }},
		{"approver", func(input *PaymentFinancialRepairExecutionInput) { input.ApproverUserID = uuid.New() }},
	} {
		t.Run("BoundApprovalIdentity/"+scenario.name, func(t *testing.T) {
			input := baseReplay
			scenario.change(&input)
			before := fullFinancialSnapshot()
			result, err := coordinator.ApplyPaymentFinancialRepair(ctx, input)
			if !errors.Is(err, ErrConflict) || result != (PaymentFinancialRepairResult{}) {
				t.Fatal("changed bound identity did not return definite conflict", result, err)
			}
			attempt := classifyAuthorizationDomainFailure(err)
			if !attempt.FailureIsDefinite || attempt.StableFailureCode != "DOMAIN_PRECONDITION_REJECTED" || fullFinancialSnapshot() != before {
				t.Fatal("bound approval conflict changed financial facts or became UNKNOWN", attempt)
			}
		})
	}
	t.Run("BoundApprovalIdentity/another-eligible-order", func(t *testing.T) {
		otherOrder, _, otherEvidence := seed(100, 100, 80, 20, "RECORDED", "PAY_SUCCESS")
		otherCommand := PaymentFinancialRepairCommandInput{PaymentOrderID: otherOrder, PaymentEvidenceID: otherEvidence}
		otherPlan, err := paymentFinancialRepairPlanFrom(ctx, pool, otherCommand, 1, 1, activationVersion)
		if err != nil {
			t.Fatal("second target must independently be a valid fresh repair", err)
		}
		input := baseReplay
		input.Command, input.Frozen = otherCommand, frozenFacts(otherPlan)
		before := fullFinancialSnapshot()
		result, err := coordinator.ApplyPaymentFinancialRepair(ctx, input)
		if !errors.Is(err, ErrConflict) || result != (PaymentFinancialRepairResult{}) {
			t.Fatal("existing approval reused for a valid second order", result, err)
		}
		attempt := classifyAuthorizationDomainFailure(err)
		if !attempt.FailureIsDefinite || attempt.StableFailureCode != "DOMAIN_PRECONDITION_REJECTED" || fullFinancialSnapshot() != before {
			t.Fatal("second-order conflict became UNKNOWN or changed original/new financial facts", attempt)
		}
	})
	t.Run("BoundApprovalIdentity/exact-receipt-after-deactivation", func(t *testing.T) {
		setActivation("INACTIVE")
		defer func() { activationVersion = setActivation("ACTIVE") }()
		before := fullFinancialSnapshot()
		result, err := coordinator.ApplyPaymentFinancialRepair(ctx, baseReplay)
		if err != nil || result != applied || fullFinancialSnapshot() != before {
			t.Fatal("exact immutable receipt became dependent on fresh-effect activation", result, err)
		}
	})
	for _, scenario := range []struct {
		name                   string
		credits, bonus, posted int64
	}{
		{"BASE40_credit40_noBonus", 40, 0, 0},
		{"BASE40_credit100_BONUS60", 100, 60, 60},
	} {
		t.Run("SaturatedReceivableBudget/"+scenario.name, func(t *testing.T) {
			orderID, walletID, evidenceID := seed(100, scenario.credits, 40, scenario.bonus, "RECORDED", "PAY_SUCCESS")
			receivableID := uuid.New()
			if _, err := pool.Exec(ctx, `INSERT INTO receivables(id,wallet_id,source_type,source_reference,original_microcredits,outstanding_microcredits,status,created_at,updated_at)
 VALUES($1,$2,'USAGE_OVERAGE',$3,100,100,'OPEN',$4,$4)`, receivableID, walletID, "saturation:"+receivableID.String(), now); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `UPDATE wallets SET receivable_microcredits=100,financial_status='RECEIVABLE_OPEN',financial_freeze_reason='USAGE_OVERAGE' WHERE id=$1`, walletID); err != nil {
				t.Fatal(err)
			}
			plan, err := paymentFinancialRepairPlanFrom(ctx, pool, PaymentFinancialRepairCommandInput{PaymentOrderID: orderID, PaymentEvidenceID: evidenceID}, 1, 1, activationVersion)
			if err != nil || plan.BaseAmount != 40 || plan.BonusAmount != scenario.bonus || plan.CreditMicrocredits != scenario.credits || plan.AppliedToReceivables != 40 {
				t.Fatalf("BASE saturation plan %+v err=%v", plan, err)
			}
			if scenario.bonus != 0 {
				// 50 is inside totalcredit100 but outside the owner's BASE40.
				// Keep the real digest/other facts, change only that claimed scalar.
				bad := frozenFacts(plan)
				bad.AppliedToReceivables = 50
				before := fullFinancialSnapshot()
				if _, err := execute(uuid.New(), uuid.New(), orderID, evidenceID, bad); !errors.Is(err, ErrApprovalSnapshotMismatch) {
					t.Fatal("above BASE within total credit accepted", err)
				}
				if fullFinancialSnapshot() != before {
					t.Fatal("above BASE rejection changed any financial or immutable fact")
				}
			}
			requestID, eventID := uuid.New(), uuid.New()
			frozen := frozenFacts(plan)
			result, err := execute(requestID, eventID, orderID, evidenceID, frozen)
			if err != nil || result.Status != "SUCCEEDED" || result.AppliedToReceivables != 40 || result.CreditMicrocredits != scenario.credits {
				t.Fatalf("BASE saturation result %+v err=%v", result, err)
			}
			var posted, reserved, aggregate, settled, outstanding int64
			var status, financialStatus string
			if err := pool.QueryRow(ctx, `SELECT w.posted_microcredits,w.reserved_microcredits,w.receivable_microcredits,w.financial_status,r.settled_microcredits,r.outstanding_microcredits,r.status FROM wallets w JOIN receivables r ON r.wallet_id=w.id WHERE w.id=$1 AND r.id=$2`, walletID, receivableID).Scan(&posted, &reserved, &aggregate, &financialStatus, &settled, &outstanding, &status); err != nil || posted != scenario.posted || reserved != 0 || aggregate != 60 || settled != 40 || outstanding != 60 || status != "PARTIALLY_SETTLED" || financialStatus != "RECEIVABLE_OPEN" {
				t.Fatal("wallet/receivable saturation projection differs", err)
			}
			var baseOriginal, bonusOriginal, baseAvailable, bonusAvailable, lotRemaining int64
			if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(c.original_microcredits) FILTER (WHERE c.component_type='BASE'),0),COALESCE(sum(c.original_microcredits) FILTER (WHERE c.component_type='BONUS'),0),COALESCE(sum(c.available_microcredits) FILTER (WHERE c.component_type='BASE'),0),COALESCE(sum(c.available_microcredits) FILTER (WHERE c.component_type='BONUS'),0),COALESCE(sum(l.remaining_microcredits),0) FROM entitlement_components c JOIN credit_lots l ON l.id=c.credit_lot_id WHERE c.package_id=$1`, result.EntitlementPackageID).Scan(&baseOriginal, &bonusOriginal, &baseAvailable, &bonusAvailable, &lotRemaining); err != nil || baseOriginal != 40 || bonusOriginal != scenario.bonus || baseAvailable != 0 || bonusAvailable != scenario.bonus || lotRemaining != scenario.posted {
				t.Fatal("BASE/BONUS/lot budget differs", err)
			}
			var allocation int64
			var allocationRows, postingRows int
			var delta int64
			if err := pool.QueryRow(ctx, `SELECT count(*),COALESCE(sum(amount_microcredits),0) FROM receivable_allocations WHERE receivable_id=$1 AND source_type='CASH_RECHARGE_BASE'`, receivableID).Scan(&allocationRows, &allocation); err != nil || allocationRows != 1 || allocation != 40 {
				t.Fatal("saturated receivable allocation differs", err)
			}
			if err := pool.QueryRow(ctx, `SELECT count(*),COALESCE(sum(delta_microcredits),0) FROM ledger_postings WHERE journal_entry_id=$1`, result.JournalEntryID).Scan(&postingRows, &delta); err != nil || postingRows < 2 || delta != 0 {
				t.Fatal("saturated payment journal is not balanced", err)
			}
			var awarded, beforeBalance, afterBalance, epoch int64
			var request, event, actor, requester uuid.UUID
			var digest string
			if err := pool.QueryRow(ctx, `SELECT points_awarded,wallet_balance_before,wallet_balance_after,approval_request_id,authorization_event_id,approval_epoch,actor_id,requester_user_id,facts_digest FROM payment_order_repairs WHERE approval_request_id=$1`, requestID).Scan(&awarded, &beforeBalance, &afterBalance, &request, &event, &epoch, &actor, &requester, &digest); err != nil || awarded != scenario.credits || beforeBalance != 0 || afterBalance != scenario.posted || awarded-(afterBalance-beforeBalance) != 40 || request != requestID || event != eventID || epoch != 2 || actor != approvingUserID || requester != requestingUserID || digest != plan.FactsDigest {
				t.Fatal("saturated immutable receipt or actors differ", err)
			}
			beforeReplay := fullFinancialSnapshot()
			same, err := execute(requestID, eventID, orderID, evidenceID, frozen)
			if err != nil || same != result || fullFinancialSnapshot() != beforeReplay {
				t.Fatal("saturated matching replay changed any financial or immutable fact", err)
			}
			if provider.calls != 0 {
				t.Fatal("persisted-evidence repair called controlled provider")
			}
		})
	}

}
