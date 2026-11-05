package billing

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/google/uuid"
)

// Actual public-migration fixture, production proof/claim/dispatcher and owner.
// The provider is controlled and must never be called; no Account issuer/NATS is inferred.
func TestPaymentFinancialRepairFrozenExecutionPostgres(t *testing.T) {
	if os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL") == "" {
		t.Skip("PINVOU_TEST_BILLING_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	repository, _ := newAdminPaginationIntegrationRepository(t, ctx, os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL"))
	pool := repository.pool
	setActivation := paymentFixtureActivation(t, ctx, pool)
	activationVersion := setActivation("ACTIVE")
	provider := &paymentRepairStubProvider{}
	coordinator, err := NewPaymentCoordinatorWithProvider(repository, provider)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
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

	frozen := func(p PaymentFinancialRepairPlan) PaymentFinancialRepairFrozenFacts {
		return PaymentFinancialRepairFrozenFacts{OrderUpdatedAt: p.OrderUpdatedAt, AmountFen: p.AmountFen, CreditMicrocredits: p.CreditMicrocredits, ProviderTransactionID: p.ProviderTransactionID, WalletID: p.WalletID, WalletVersion: p.WalletVersion, AppliedToReceivables: p.AppliedToReceivables, FactsDigest: p.FactsDigest}
	}
	input := func(p PaymentFinancialRepairPlan) PaymentFinancialRepairExecutionInput {
		return PaymentFinancialRepairExecutionInput{ApprovalRequestID: uuid.New(), AuthorizationEventID: uuid.New(), ApprovalEpoch: 1, ApproverUserID: uuid.New(), RequesterUserID: uuid.New(), ExecutionKey: "payment-frozen:" + p.OrderID.String(), PolicyVersion: 1, SchemaVersion: 1, ActivationVersion: activationVersion, Command: PaymentFinancialRepairCommandInput{PaymentOrderID: p.OrderID, PaymentEvidenceID: p.EvidenceID}, Frozen: frozen(p)}
	}
	planFrom := func(o, e uuid.UUID) PaymentFinancialRepairPlan {
		p, err := paymentFinancialRepairPlanFrom(ctx, pool, PaymentFinancialRepairCommandInput{PaymentOrderID: o, PaymentEvidenceID: e}, 1, 1, activationVersion)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	snapshot := func() string {
		var raw string
		err := pool.QueryRow(ctx, `SELECT jsonb_build_array((SELECT jsonb_agg(to_jsonb(w) ORDER BY id) FROM wallets w),(SELECT jsonb_agg(to_jsonb(r) ORDER BY id) FROM receivables r),(SELECT jsonb_agg(to_jsonb(o) ORDER BY id) FROM payment_orders o),(SELECT count(*) FROM payment_evidence),(SELECT count(*) FROM payment_order_repairs),(SELECT count(*) FROM payment_transactions),(SELECT count(*) FROM entitlement_packages),(SELECT count(*) FROM entitlement_components),(SELECT count(*) FROM credit_lots),(SELECT count(*) FROM journal_entries),(SELECT count(*) FROM ledger_postings),(SELECT count(*) FROM receivable_allocations),(SELECT count(*) FROM outbox_events))::text`).Scan(&raw)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	reject := func(t *testing.T, i PaymentFinancialRepairExecutionInput) {
		before := snapshot()
		if _, err := coordinator.ApplyPaymentFinancialRepair(ctx, i); !errors.Is(err, ErrApprovalSnapshotMismatch) {
			t.Fatalf("fresh rejection=%v", err)
		}
		if snapshot() != before {
			t.Fatal("definite rejection wrote effects")
		}
	}
	t.Run("empty-frozen-digest-zero-effect", func(t *testing.T) {
		o, _, e := seed(100, 100, 100, 0, "RECORDED", "PAY_SUCCESS")
		i := input(planFrom(o, e))
		i.Frozen.FactsDigest = ""
		reject(t, i)
	})
	t.Run("full-product-same-amount-drift-zero-effect", func(t *testing.T) {
		o, _, e := seed(100, 100, 100, 0, "RECORDED", "PAY_SUCCESS")
		p := planFrom(o, e)
		if _, err := pool.Exec(ctx, `UPDATE payment_orders SET product_snapshot=product_snapshot||'{"plan_name":"changed-frozen-plan"}'::jsonb WHERE id=$1`, o); err != nil {
			t.Fatal(err)
		}
		next := planFrom(o, e)
		if next.AmountFen != p.AmountFen || next.CreditMicrocredits != p.CreditMicrocredits || next.OrderUpdatedAt != p.OrderUpdatedAt {
			t.Fatal("same-amount fixture changed existing frozen fields")
		}
		if next.FactsDigest == p.FactsDigest {
			t.Error("full product change absent from digest")
		}
		reject(t, input(p))
	})
	t.Run("primary-transaction-matching-and-foreign", func(t *testing.T) {
		o, _, e := seed(100, 100, 100, 0, "RECORDED", "PAY_SUCCESS")
		p := planFrom(o, e)
		if _, err := pool.Exec(ctx, `UPDATE payment_orders SET primary_provider_transaction_id=$2 WHERE id=$1`, o, p.ProviderTransactionID); err != nil {
			t.Fatal(err)
		}
		_ = planFrom(o, e)
		if _, err := pool.Exec(ctx, `UPDATE payment_orders SET primary_provider_transaction_id=$2 WHERE id=$1`, o, "foreign-"+uuid.NewString()); err != nil {
			t.Fatal(err)
		}
		before := snapshot()
		if _, err := paymentFinancialRepairPlanFrom(ctx, pool, PaymentFinancialRepairCommandInput{PaymentOrderID: o, PaymentEvidenceID: e}, 1, 1, activationVersion); !errors.Is(err, ErrApprovalSnapshotMismatch) {
			t.Fatalf("foreign primary transaction accepted: %v", err)
		}
		if snapshot() != before {
			t.Fatal("rejected plan wrote")
		}
	})
	t.Run("live-state-version-and-committed-replay", func(t *testing.T) {
		t.Cleanup(func() { activationVersion = setActivation("ACTIVE") })
		o, _, e := seed(100, 100, 100, 0, "RECORDED", "PAY_SUCCESS")
		p := planFrom(o, e)
		i := input(p)
		setActivation("INACTIVE")
		reject(t, i)
		activationVersion = setActivation("ACTIVE")
		reject(t, i)
		i.ActivationVersion = activationVersion
		p = planFrom(o, e)
		i.Frozen = frozen(p)
		good, err := coordinator.ApplyPaymentFinancialRepair(ctx, i)
		if err != nil {
			t.Fatal(err)
		}
		setActivation("INACTIVE")
		before := snapshot()
		replayOwner := &PaymentCoordinator{repository: repository, now: time.Now}
		again, err := replayOwner.ApplyPaymentFinancialRepair(ctx, i)
		if err != nil || again != good || snapshot() != before {
			t.Fatalf("receipt-first after inactive/nil provider: %+v %v", again, err)
		}
		activationVersion = setActivation("ACTIVE")
	})
	t.Run("proof-claim-same-amount-receivable-drift-zero-effect", func(t *testing.T) {
		o, w, e := seed(100, 100, 100, 0, "RECORDED", "PAY_SUCCESS")
		rid := uuid.New()
		if _, err := pool.Exec(ctx, `INSERT INTO receivables(id,wallet_id,source_type,source_reference,original_microcredits,outstanding_microcredits,status,created_at,updated_at) VALUES($1,$2,'USAGE_OVERAGE',$3,40,40,'OPEN',$4,$4)`, rid, w, "usage:"+rid.String(), now); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE wallets SET receivable_microcredits=40,financial_status='RECEIVABLE_OPEN',financial_freeze_reason='USAGE_OVERAGE' WHERE id=$1`, w); err != nil {
			t.Fatal(err)
		}
		source, err := repository.PaymentFinancialRepairProofInput(ctx, PaymentFinancialRepairCommandInput{PaymentOrderID: o, PaymentEvidenceID: e})
		if err != nil {
			t.Fatal(err)
		}
		artifacts, err := approvalencoding.ConvertLegacyProofInput(source)
		if err != nil {
			t.Fatal(err)
		}
		requestID, eventID := uuid.New(), uuid.New()
		service := NewService(repository).WithPayments(coordinator)
		proof, err := service.billingApprovalProof(ctx, approvalencoding.ProofRequest{ApprovalRequestID: requestID, Artifacts: artifacts})
		if err != nil || proof.Verdict != "ALLOW" {
			t.Fatalf("real proof: %+v %v", proof, err)
		}
		b := ApprovalAuthorizationBinding{ApprovalRequestID: requestID, EventID: eventID, ApprovalEpoch: 1, CommandType: paymentFinancialRepairCommand, PolicyVersion: 1, ExecutorIdentity: billingApprovalExecutorIdentity, SerializationVersion: approvalencoding.ApprovalSerializationVersion}
		event := approvalencoding.AuthorizationEvent{ApprovalRequestID: requestID, EventID: eventID, ApprovalControlEpoch: 1, CommandType: paymentFinancialRepairCommand, PolicyVersion: 1, ExecutorIdentity: billingApprovalExecutorIdentity, TargetResourceType: source.TargetResourceType, TargetResourceID: source.TargetResourceID, NormalizedCommand: source.NormalizedCommand, CriticalResourceVersions: source.CriticalResourceVersions, FinancialSnapshot: source.FinancialSnapshot, ProofSchemaVersion: approvalencoding.ProofSchemaVersion, CommandDigest: artifacts.CommandDigest, SnapshotDigest: artifacts.SnapshotDigest, DomainRevision: proof.DomainRevision}
		approved := ApprovedCommand{ApprovalRequestID: requestID, CommandType: paymentFinancialRepairCommand, PolicyVersion: 1, ExecutorIdentity: billingApprovalExecutorIdentity, TargetResourceType: source.TargetResourceType, TargetResourceID: source.TargetResourceID, NormalizedCommand: source.NormalizedCommand, CriticalResourceVersions: source.CriticalResourceVersions, FinancialSnapshot: source.FinancialSnapshot, RequesterUserID: uuid.New(), ApproverUserID: uuid.New(), DecisionReason: "independent frozen review", ExecutionIdempotencyKey: "payment-repair:" + requestID.String()}
		domain := newBillingAuthorizationDomain(service).(*billingAuthorizationDomain)
		claim := domain.claim
		afterDrift := ""
		domain.claim = func(ctx context.Context, b ApprovalAuthorizationBinding, e approvalencoding.AuthorizationEvent) error {
			if err := claim(ctx, b, e); err != nil {
				return err
			}
			if _, err := pool.Exec(ctx, `UPDATE receivables SET version=version+1 WHERE id=$1`, rid); err != nil {
				return err
			}
			afterDrift = snapshot()
			return nil
		}
		attempt := domain.ExecuteAuthorizationAttempt(ctx, b, event, approved)
		if afterDrift == "" {
			t.Fatalf("actual proof/claim not reached: %+v", attempt)
		}
		if !attempt.FailureIsDefinite || attempt.StableFailureCode != "DOMAIN_PRECONDITION_REJECTED" || len(attempt.Result.DomainResult) != 0 {
			t.Fatalf("stale same-amount receivable accepted: %+v", attempt)
		}
		if snapshot() != afterDrift {
			t.Fatal("stale approval wrote owner effects")
		}
		var claims int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM billing_approval_proof_claims WHERE approval_request_id=$1 AND authorization_event_id=$2`, requestID, eventID).Scan(&claims); err != nil || claims != 1 {
			t.Fatalf("actual immutable claim=%d %v", claims, err)
		}
	})
	if provider.calls != 0 {
		t.Fatalf("provider called %d times", provider.calls)
	}
}
