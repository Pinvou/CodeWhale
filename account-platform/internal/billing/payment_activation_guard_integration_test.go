package billing

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"os"
	"strings"
	"testing"
	"time"
)

func TestF12PaymentActivationFinancialTransactionPostgres(t *testing.T) {
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

	frozen := func(plan PaymentFinancialRepairPlan) PaymentFinancialRepairFrozenFacts {
		return PaymentFinancialRepairFrozenFacts{OrderUpdatedAt: plan.OrderUpdatedAt, AmountFen: plan.AmountFen, CreditMicrocredits: plan.CreditMicrocredits, ProviderTransactionID: plan.ProviderTransactionID, WalletID: plan.WalletID, WalletVersion: plan.WalletVersion, AppliedToReceivables: plan.AppliedToReceivables, FactsDigest: plan.FactsDigest}
	}
	input := PaymentFinancialRepairExecutionInput{ApprovalRequestID: uuid.New(), AuthorizationEventID: uuid.New(), ApprovalEpoch: 2, ApproverUserID: approvingUserID, RequesterUserID: requestingUserID, ExecutionKey: "payment-race:legacy-compat", PolicyVersion: 1, SchemaVersion: 1, ActivationVersion: activationVersion, Command: command, Frozen: frozen(plan)}
	result, err := paymentRepairWithDeactivationRace(t, ctx, coordinator, input)
	if err != nil || result.Status != "SUCCEEDED" || result.CreditMicrocredits != 100 || provider.calls != 0 {
		t.Fatal("qualified payment race failed", err)
	}
	var repairs, journals, postings, posted int64
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM payment_order_repairs WHERE payment_order_id=$1),(SELECT count(*) FROM journal_entries WHERE id=$2),(SELECT count(*) FROM ledger_postings WHERE journal_entry_id=$2),(SELECT posted_microcredits FROM wallets WHERE id=$3)`, orderID, result.JournalEntryID, walletID).Scan(&repairs, &journals, &postings, &posted); err != nil || repairs != 1 || journals != 1 || postings != 3 || posted != 100 {
		t.Fatalf("actual committed facts repairs=%d journals=%d postings=%d posted=%d", repairs, journals, postings, posted)
	}
	freshOrder, _, freshEvidence := seed(100, 100, 80, 20, "RECORDED", "PAY_SUCCESS")
	freshPlan, err := paymentFinancialRepairPlanFrom(ctx, pool, PaymentFinancialRepairCommandInput{PaymentOrderID: freshOrder, PaymentEvidenceID: freshEvidence}, 1, 1, activationVersion)
	if err != nil {
		t.Fatal(err)
	}
	fresh := input
	fresh.ApprovalRequestID = uuid.New()
	fresh.AuthorizationEventID = uuid.New()
	fresh.ExecutionKey = "payment-fresh:" + uuid.NewString()
	fresh.Command = PaymentFinancialRepairCommandInput{PaymentOrderID: freshOrder, PaymentEvidenceID: freshEvidence}
	fresh.Frozen = frozen(freshPlan)
	if _, err := coordinator.ApplyPaymentFinancialRepair(ctx, fresh); !errors.Is(err, ErrApprovalSnapshotMismatch) {
		t.Fatal("inactive fresh execution must reject", err)
	}
	setActivation("ACTIVE")
	if _, err := coordinator.ApplyPaymentFinancialRepair(ctx, fresh); !errors.Is(err, ErrApprovalSnapshotMismatch) {
		t.Fatal("old activation version must reject", err)
	}
	var count int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM payment_order_repairs WHERE payment_order_id=$1`, freshOrder).Scan(&count); err != nil || count != 0 {
		t.Fatal("rejected fresh effect wrote a receipt")
	}
	if replay, err := coordinator.ApplyPaymentFinancialRepair(ctx, input); err != nil || replay != result {
		t.Fatal("old receipt no longer replays after operator transitions")
	}
	t.Log("operator waited until real financial commit; inactive/version mismatch wrote zero receipts; old receipt remains read-only")
}
