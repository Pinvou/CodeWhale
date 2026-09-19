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

type refundExceptionFixture struct {
	RefundID          uuid.UUID
	WalletID          uuid.UUID
	PackageID         uuid.UUID
	AnomalyCaseID     uuid.UUID
	EvidenceID        uuid.UUID
	ProviderRefundID  string
	CreditMicrocredits int64
}

// seedRefundException writes one "provider refunded / locally unreversed"
// exception. With twoComponents the reservation covers BASE 60 (reclaimable)
// plus BONUS 40 whose component was already partially used, so the repair has to
// split revoked/shortfall.
func seedRefundException(t *testing.T, ctx context.Context, repository *PostgresRepository, twoComponents bool) refundExceptionFixture {
	t.Helper()
	pool := repository.pool
	now := time.Now().UTC()
	fixture := refundExceptionFixture{
		RefundID: uuid.New(), WalletID: uuid.New(), PackageID: uuid.New(), AnomalyCaseID: uuid.New(), EvidenceID: uuid.New(),
		ProviderRefundID: "refund-" + uuid.NewString(), CreditMicrocredits: 100,
	}
	ownerID, orderID, transactionID, lotID, componentID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	// The single component carries the whole credit in the full-reclaim fixture;
	// the partial fixture splits it into BASE 60 (reclaimable) + BONUS 40.
	baseAmount := int64(100)
	if twoComponents {
		baseAmount = 60
	}
	if _, err := pool.Exec(ctx, `INSERT INTO wallets(id,owner_type,owner_id,status,posted_microcredits,version,created_at,updated_at)
VALUES($1,'PERSONAL',$2,'ACTIVE',100,1,$3,$3)`, fixture.WalletID, ownerID, now); err != nil {
		t.Fatal(err)
	}
	product, err := json.Marshal(Product{ProductID: "REFUND_REPAIR", AmountFen: 100, CreditMicrocredits: 100,
		BasePoints: 60, BonusPoints: 40, Currency: "CNY", PlanName: "Repair"})
	if err != nil {
		t.Fatal(err)
	}
	statements := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO ledger_accounts(id,wallet_id,code,account_type,created_at) VALUES($1,$2,$3,'WALLET_LIABILITY',$4)`,
			[]any{uuid.New(), fixture.WalletID, "wallet:" + fixture.WalletID.String(), now}},
		{`INSERT INTO payment_orders(id,merchant_order_no,requester_id,payer_user_id,wallet_id,product_id,product_snapshot,channel,
amount_fen,currency,credit_microcredits,status,fulfillment_status,expires_at,provider_payable_until,late_review_boundary_at,final_archive_eligible_at,
created_at,updated_at,provider_sku,provider_id,provider_version)
VALUES($1,$2,$3,$3,$4,'REFUND_REPAIR',$5,'ALIPAY',100,'CNY',100,'PAID','CREDITED',$6,$6,$7,$8,$9,$9,'sku-repair','test-provider','v1')`,
			[]any{orderID, "refund-repair-" + orderID.String(), ownerID, fixture.WalletID, product,
				now.Add(time.Hour), now.Add(72 * time.Hour), now.Add(8 * 24 * time.Hour), now}},
		{`INSERT INTO payment_transactions(id,payment_order_id,provider_id,provider_version,channel,provider_transaction_id,
status,fulfillment_status,amount_fen,currency,source,provider_status,provider_paid_at,verified_at,fulfilled_at,created_at,updated_at)
VALUES($1,$2,'test-provider','v1','ALIPAY',$3,'PAID','CREDITED',100,'CNY','QUERY','PAY_SUCCESS',$4,$4,$4,$4,$4)`,
			[]any{transactionID, orderID, "trade-" + transactionID.String(), now}},
		{`INSERT INTO credit_lots(id,wallet_id,source_type,source_reference,original_microcredits,remaining_microcredits,refundable,expires_at,created_at)
VALUES($1,$2,'RECHARGE',$3,$4,$4,TRUE,$5,$6)`, []any{lotID, fixture.WalletID, "recharge:" + lotID.String(), baseAmount, now.Add(24 * time.Hour), now}},
		{`INSERT INTO entitlement_packages(id,user_id,wallet_id,payment_order_id,source_payment_transaction_id,credit_lot_id,product_id,plan_name,sku_code,
amount_fen,points_total,points_remaining,credited_at,valid_from,valid_until,status,created_at,updated_at,source_type,source_reference)
VALUES($1,$2,$3,$4,$5,$6,'REFUND_REPAIR','Repair','sku-repair',100,100,100,$7,$7,$8,'FROZEN',$7,$7,'PAYMENT',$9)`,
			[]any{fixture.PackageID, ownerID, fixture.WalletID, orderID, transactionID, lotID, now, now.Add(24 * time.Hour), "payment-transaction:" + transactionID.String()}},
		{`INSERT INTO entitlement_components(id,package_id,component_type,credit_lot_id,original_microcredits,available_microcredits,
credited_at,expires_at,status,created_at,updated_at)
VALUES($1,$2,'BASE',$3,$4,$4,$5,$6,'FROZEN',$5,$5)`,
			[]any{componentID, fixture.PackageID, lotID, baseAmount, now, now.Add(24 * time.Hour)}},
		{`INSERT INTO refunds(id,payment_order_id,requester_id,merchant_refund_no,channel_refund_id,amount_fen,credit_microcredits,
status,idempotency_key,request_hash,reason,created_at,updated_at,payment_transaction_id,entitlement_package_id,
base_microcredits,bonus_microcredits,entitlement_snapshot,intent_version)
VALUES($1,$2,$3,$4,$5,100,100,'MANUAL_REVIEW',$6,'hash','provider refunded locally unreversed',$7,$7,$8,$9,100,0,'{}',1)`,
			[]any{fixture.RefundID, orderID, ownerID, "merchant-refund-" + fixture.RefundID.String(), fixture.ProviderRefundID,
				"refund-idempotency-" + fixture.RefundID.String(), now, transactionID, fixture.PackageID}},
		{`INSERT INTO refund_component_reservations(refund_id,component_id,package_id,component_type,credit_lot_id,
available_microcredits,active_grant_microcredits,suspended_grant_microcredits,prior_status,created_at)
VALUES($1,$2,$3,'BASE',$4,$5,0,0,'FROZEN',$6)`, []any{fixture.RefundID, componentID, fixture.PackageID, lotID, baseAmount, now}},
		{`INSERT INTO payment_evidence(id,payment_order_id,payment_transaction_id,evidence_type,provider_id,provider_version,evidence_key,
merchant_order_no,provider_transaction_id,provider_refund_id,amount_fen,currency,provider_status,received_at,payload_digest,payload_snapshot,merge_result)
VALUES($1,$2,$3,'QUERY','test-provider','v1',$4,$5,$6,$7,100,'CNY','SUCCESS',$8,$9,'{}','RECORDED')`,
			[]any{fixture.EvidenceID, orderID, transactionID, "query:" + fixture.EvidenceID.String(), "refund-repair-" + orderID.String(),
				"trade-" + transactionID.String(), fixture.ProviderRefundID, now, strings.Repeat("ab", 32)}},
		{`INSERT INTO manual_review_cases(id,domain,case_type,resource_type,resource_id,severity,status,created_at,claim_due_at,
first_response_due_at,mitigation_due_at,resolution_due_at,resolved_at,updated_at)
VALUES($1,'BILLING','PAYMENT_ANOMALY','REFUND',$2,'P0','RESOLVED',$3,$3,$3,$3,$3,$3,$3)`,
			[]any{fixture.AnomalyCaseID, fixture.RefundID, now}},
		{`INSERT INTO payment_anomaly_cases(id,anomaly_type,dedupe_key,payment_order_id,payment_transaction_id,payment_evidence_id,refund_id,
status,resolution_code,financial_action_status,resource_snapshot,version,created_at,updated_at,resolved_at)
VALUES($1,'REFUND_FAILURE',$2,$3,$4,$5,$6,'RESOLVED','ESCALATE_FINANCIAL_REPAIR','PENDING','{}',1,$7,$7,$7)`,
			[]any{fixture.AnomalyCaseID, "refund:" + fixture.RefundID.String() + ":local-conflict", orderID, transactionID,
				fixture.EvidenceID, fixture.RefundID, now}},
	}
	if twoComponents {
		bonusComponentID, bonusLotID := uuid.New(), uuid.New()
		statements = append(statements,
			struct {
				sql  string
				args []any
			}{`INSERT INTO credit_lots(id,wallet_id,source_type,source_reference,original_microcredits,remaining_microcredits,refundable,expires_at,created_at)
VALUES($1,$2,'RECHARGE',$3,40,40,FALSE,$4,$5)`, []any{bonusLotID, fixture.WalletID, "recharge:" + bonusLotID.String(), now.Add(24 * time.Hour), now}},
			struct {
				sql  string
				args []any
			}{`INSERT INTO entitlement_components(id,package_id,component_type,credit_lot_id,original_microcredits,available_microcredits,
used_microcredits,credited_at,expires_at,status,created_at,updated_at)
VALUES($1,$2,'BONUS',$3,40,30,10,$4,$5,'FROZEN',$4,$4)`,
				[]any{bonusComponentID, fixture.PackageID, bonusLotID, now, now.Add(24 * time.Hour)}},
			struct {
				sql  string
				args []any
			}{`INSERT INTO refund_component_reservations(refund_id,component_id,package_id,component_type,credit_lot_id,
available_microcredits,active_grant_microcredits,suspended_grant_microcredits,prior_status,created_at)
VALUES($1,$2,$3,'BONUS',$4,40,0,0,'FROZEN',$5)`, []any{fixture.RefundID, bonusComponentID, fixture.PackageID, bonusLotID, now}},
		)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	for index, statement := range statements {
		if _, err := tx.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatalf("refund exception statement %d: %v", index, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if twoComponents {
		_ = baseAmount
	}
	return fixture
}

// TestRefundExceptionRepairPostgresIntegration proves the GSA-30 domain
// contract on real PostgreSQL: the full reclaim (no receivable) and the partial
// reclaim (shortfall becomes a FORCED_REFUND receivable), the REFUND journal
// with the repair reference, the wallet/package/anomaly/refund transitions, the
// outbox fact, idempotent replay, and the zero-write negatives.
func TestRefundExceptionRepairPostgresIntegration(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("PINVOU_TEST_BILLING_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	repository, _ := newAdminPaginationIntegrationRepository(t, ctx, databaseURL)
	pool := repository.pool

	full := seedRefundException(t, ctx, repository, false)
	command := RefundExceptionRepairCommandInput{RefundID: full.RefundID, PaymentEvidenceID: full.EvidenceID, AnomalyCaseID: full.AnomalyCaseID}
	plan, err := refundExceptionRepairPlanFrom(ctx, pool, command, 1, 1, 1)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.RevokedMicrocredits != 100 || plan.ShortfallMicrocredits != 0 || plan.FactsDigest == "" ||
		plan.PackageStatus != "FROZEN" || plan.ProviderRefundID != full.ProviderRefundID {
		t.Fatalf("unexpected plan %+v", plan)
	}
	execute := func(requestID, eventID uuid.UUID, command RefundExceptionRepairCommandInput, frozen RefundExceptionRepairFrozenFacts) (RefundExceptionRepairResult, error) {
		return repository.ApplyRefundExceptionRepair(ctx, RefundExceptionRepairExecutionInput{
			ApprovalRequestID: requestID, AuthorizationEventID: eventID, ApprovalEpoch: 2,
			ExecutionKey: "refund-repair:" + requestID.String(), PolicyVersion: 1, SchemaVersion: 1, ActivationVersion: 1,
			Command: command, Frozen: frozen,
		})
	}
	frozen := RefundExceptionRepairFrozenFacts{RefundUpdatedAt: plan.RefundUpdatedAt, AnomalyVersion: plan.AnomalyVersion,
		AmountFen: plan.AmountFen, CreditMicrocredits: plan.CreditMicrocredits, ProviderRefundID: plan.ProviderRefundID,
		RevokedMicrocredits: plan.RevokedMicrocredits, ShortfallMicrocredits: plan.ShortfallMicrocredits,
		WalletID: plan.WalletID, WalletVersion: plan.WalletVersion, PackageID: plan.PackageID}
	requestID, eventID := uuid.New(), uuid.New()
	applied, err := execute(requestID, eventID, command, frozen)
	if err != nil {
		rows, queryErr := pool.Query(ctx, `SELECT conname||' :: '||pg_get_constraintdef(oid) FROM pg_constraint
WHERE conrelid=to_regclass('entitlement_packages') AND contype='c' ORDER BY conname`)
		if queryErr == nil {
			for rows.Next() {
				var definition string
				if rows.Scan(&definition) == nil {
					t.Logf("entitlement_packages constraint: %s", definition)
				}
			}
			rows.Close()
		}
		t.Fatalf("apply: %v", err)
	}
	if applied.Status != "SUCCEEDED" || applied.RevokedMicrocredits != 100 || applied.ShortfallMicrocredits != 0 ||
		applied.ReceivableID != nil || applied.JournalEntryID == uuid.Nil {
		t.Fatalf("unexpected result %+v", applied)
	}
	var refundStatus string
	var intentVersion int64
	if err := pool.QueryRow(ctx, `SELECT status,intent_version FROM refunds WHERE id=$1`, full.RefundID).Scan(&refundStatus, &intentVersion); err != nil ||
		refundStatus != "SUCCEEDED" || intentVersion != 2 {
		t.Fatalf("refund=%s/%d err=%v", refundStatus, intentVersion, err)
	}
	var posted, walletVersion int64
	if err := pool.QueryRow(ctx, `SELECT posted_microcredits,version FROM wallets WHERE id=$1`, full.WalletID).Scan(&posted, &walletVersion); err != nil ||
		posted != 0 || walletVersion != 2 {
		t.Fatalf("wallet=%d/%d err=%v", posted, walletVersion, err)
	}
	var packageStatus string
	var pointsRemaining, pointsRevoked int64
	if err := pool.QueryRow(ctx, `SELECT status,points_remaining,points_revoked FROM entitlement_packages WHERE id=$1`, full.PackageID).
		Scan(&packageStatus, &pointsRemaining, &pointsRevoked); err != nil ||
		packageStatus != "REVOKED" || pointsRemaining != 0 || pointsRevoked != 100 {
		t.Fatalf("package=%s/%d/%d err=%v", packageStatus, pointsRemaining, pointsRevoked, err)
	}
	var componentStatus string
	var revokedMicrocredits, lotRemaining int64
	if err := pool.QueryRow(ctx, `SELECT component.status,component.revoked_microcredits,lot.remaining_microcredits
FROM entitlement_components component JOIN credit_lots lot ON lot.id=component.credit_lot_id
WHERE component.package_id=$1`, full.PackageID).Scan(&componentStatus, &revokedMicrocredits, &lotRemaining); err != nil ||
		componentStatus != "REVOKED" || revokedMicrocredits != 100 || lotRemaining != 0 {
		t.Fatalf("component=%s/%d lot=%d err=%v", componentStatus, revokedMicrocredits, lotRemaining, err)
	}
	var actionStatus string
	var anomalyVersion int64
	if err := pool.QueryRow(ctx, `SELECT financial_action_status,version FROM payment_anomaly_cases WHERE id=$1`, full.AnomalyCaseID).
		Scan(&actionStatus, &anomalyVersion); err != nil || actionStatus != "APPLIED" || anomalyVersion != 2 {
		t.Fatalf("anomaly=%s/%d err=%v", actionStatus, anomalyVersion, err)
	}
	var entryType, reference string
	var postingCount int
	var sumDelta int64
	var receivableLegs int64
	if err := pool.QueryRow(ctx, `SELECT entry_type,business_reference FROM journal_entries WHERE id=$1`, applied.JournalEntryID).
		Scan(&entryType, &reference); err != nil || entryType != "REFUND" || reference != "refund-repair:"+full.RefundID.String() {
		t.Fatalf("journal=%s/%s err=%v", entryType, reference, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*),COALESCE(sum(delta_microcredits),0),
		COALESCE(sum(delta_microcredits) FILTER (WHERE ledger_account_id=$2 AND credit_lot_id IS NULL),0)
	FROM ledger_postings WHERE journal_entry_id=$1`, applied.JournalEntryID, receivableAccount).
		Scan(&postingCount, &sumDelta, &receivableLegs); err != nil || postingCount != 2 || sumDelta != 0 || receivableLegs != 0 {
		t.Fatalf("postings=%d sum=%d receivable=%d err=%v", postingCount, sumDelta, receivableLegs, err)
	}
	var outbox int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE aggregate_id=$1 AND event_type='billing.refund.succeeded.v1'`, full.RefundID).Scan(&outbox); err != nil || outbox != 1 {
		t.Fatalf("outbox=%d err=%v", outbox, err)
	}
	// The same approval replays to the same case with no further effect.
	replayed, err := execute(requestID, eventID, command, frozen)
	if err != nil || replayed != applied {
		t.Fatalf("idempotent replay %+v err=%v want %+v", replayed, err, applied)
	}
	var cases, journals int64
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM refund_exception_repair_cases WHERE refund_id=$1),
		(SELECT count(*) FROM journal_entries WHERE business_reference='refund-repair:'||$1)`, full.RefundID).Scan(&cases, &journals); err != nil ||
		cases != 1 || journals != 1 {
		t.Fatalf("idempotency cases=%d journals=%d err=%v", cases, journals, err)
	}

	// Partial reclaim: the consumed BONUS component becomes a FORCED_REFUND
	// shortfall receivable instead of being revoked.
	partial := seedRefundException(t, ctx, repository, true)
	partialCommand := RefundExceptionRepairCommandInput{RefundID: partial.RefundID, PaymentEvidenceID: partial.EvidenceID, AnomalyCaseID: partial.AnomalyCaseID}
	partialPlan, err := refundExceptionRepairPlanFrom(ctx, pool, partialCommand, 1, 1, 1)
	if err != nil {
		t.Fatalf("partial plan: %v", err)
	}
	if partialPlan.RevokedMicrocredits != 60 || partialPlan.ShortfallMicrocredits != 40 {
		t.Fatalf("partial plan split %+v", partialPlan)
	}
	partialFrozen := RefundExceptionRepairFrozenFacts{RefundUpdatedAt: partialPlan.RefundUpdatedAt, AnomalyVersion: partialPlan.AnomalyVersion,
		AmountFen: partialPlan.AmountFen, CreditMicrocredits: partialPlan.CreditMicrocredits, ProviderRefundID: partialPlan.ProviderRefundID,
		RevokedMicrocredits: partialPlan.RevokedMicrocredits, ShortfallMicrocredits: partialPlan.ShortfallMicrocredits,
		WalletID: partialPlan.WalletID, WalletVersion: partialPlan.WalletVersion, PackageID: partialPlan.PackageID}
	partialResult, err := execute(uuid.New(), uuid.New(), partialCommand, partialFrozen)
	if err != nil {
		t.Fatalf("partial apply: %v", err)
	}
	if partialResult.RevokedMicrocredits != 60 || partialResult.ShortfallMicrocredits != 40 || partialResult.ReceivableID == nil {
		t.Fatalf("partial result %+v", partialResult)
	}
	var receivableSource, receivableStatus string
	var receivableOriginal, outstanding int64
	if err := pool.QueryRow(ctx, `SELECT source_type,status,original_microcredits,outstanding_microcredits FROM receivables WHERE id=$1`, *partialResult.ReceivableID).
		Scan(&receivableSource, &receivableStatus, &receivableOriginal, &outstanding); err != nil ||
		receivableSource != "FORCED_REFUND" || receivableStatus != "OPEN" || receivableOriginal != 40 || outstanding != 40 {
		t.Fatalf("receivable=%s/%s/%d/%d err=%v", receivableSource, receivableStatus, receivableOriginal, outstanding, err)
	}
	var receivableCredits int64
	var financialStatus string
	if err := pool.QueryRow(ctx, `SELECT posted_microcredits,receivable_microcredits,financial_status FROM wallets WHERE id=$1`, partial.WalletID).
		Scan(&posted, &receivableCredits, &financialStatus); err != nil ||
		posted != 40 || receivableCredits != 40 || financialStatus != "RECEIVABLE_OPEN" {
		t.Fatalf("partial wallet=%d/%d/%s err=%v", posted, receivableCredits, financialStatus, err)
	}
	var partialPostings, partialReceivableLeg int64
	var partialSum int64
	if err := pool.QueryRow(ctx, `SELECT count(*),COALESCE(sum(delta_microcredits),0),
		COALESCE(sum(delta_microcredits) FILTER (WHERE ledger_account_id=$2 AND credit_lot_id IS NULL),0)
	FROM ledger_postings WHERE journal_entry_id=$1`, partialResult.JournalEntryID, receivableAccount).
		Scan(&partialPostings, &partialSum, &partialReceivableLeg); err != nil ||
		partialPostings != 3 || partialSum != 0 || partialReceivableLeg != -40 {
		t.Fatalf("partial postings=%d sum=%d receivable=%d err=%v", partialPostings, partialSum, partialReceivableLeg, err)
	}

	// Negatives keep the transaction closed with zero writes.
	notReview := seedRefundException(t, ctx, repository, false)
	if _, err := pool.Exec(ctx, `UPDATE refunds SET status='SUCCEEDED' WHERE id=$1`, notReview.RefundID); err != nil {
		t.Fatal(err)
	}
	if _, err := refundExceptionRepairPlanFrom(ctx, pool, RefundExceptionRepairCommandInput{RefundID: notReview.RefundID,
		PaymentEvidenceID: notReview.EvidenceID, AnomalyCaseID: notReview.AnomalyCaseID}, 1, 1, 1); !errors.Is(err, ErrApprovalSnapshotMismatch) {
		t.Fatalf("non review refund err=%v", err)
	}
	notEscalated := seedRefundException(t, ctx, repository, false)
	if _, err := pool.Exec(ctx, `UPDATE payment_anomaly_cases SET financial_action_status='NONE' WHERE id=$1`, notEscalated.AnomalyCaseID); err != nil {
		t.Fatal(err)
	}
	if _, err := refundExceptionRepairPlanFrom(ctx, pool, RefundExceptionRepairCommandInput{RefundID: notEscalated.RefundID,
		PaymentEvidenceID: notEscalated.EvidenceID, AnomalyCaseID: notEscalated.AnomalyCaseID}, 1, 1, 1); !errors.Is(err, ErrApprovalSnapshotMismatch) {
		t.Fatalf("non escalated anomaly err=%v", err)
	}
	mismatchedEvidence := seedRefundException(t, ctx, repository, false)
	otherEvidence := seedRefundException(t, ctx, repository, false)
	if _, err := refundExceptionRepairPlanFrom(ctx, pool, RefundExceptionRepairCommandInput{RefundID: mismatchedEvidence.RefundID,
		PaymentEvidenceID: otherEvidence.EvidenceID, AnomalyCaseID: mismatchedEvidence.AnomalyCaseID}, 1, 1, 1); !errors.Is(err, ErrApprovalSnapshotMismatch) {
		t.Fatalf("foreign evidence err=%v", err)
	}
	var strayCases int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM refund_exception_repair_cases`).Scan(&strayCases); err != nil || strayCases != 2 {
		t.Fatalf("negatives wrote cases=%d err=%v", strayCases, err)
	}
}
