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

type chargebackRepairFixture struct {
	ChargebackCaseID     uuid.UUID
	EvidenceID           uuid.UUID
	AnomalyCaseID        uuid.UUID
	WalletID             uuid.UUID
	PackageID            uuid.UUID
	ComponentID          uuid.UUID
	LotID                uuid.UUID
	OrderID              uuid.UUID
	TransactionID        uuid.UUID
	ProviderChargebackID string
	BaseMicrocredits     int64
}

// seedChargebackShortfall writes one confirmed provider chargeback whose whole
// BASE credit was already consumed, so nothing is revocable and the only
// possible compensation is the BASE shortfall receivable. With bonusShortfall
// the package also carries a consumed BONUS component, which must never move.
func seedChargebackShortfall(t *testing.T, ctx context.Context, repository *PostgresRepository, bonusShortfall bool) chargebackRepairFixture {
	t.Helper()
	return seedChargebackShortfallWithEvidence(t, ctx, repository, bonusShortfall, "CHARGEBACK_CONFIRMED")
}

// seedChargebackShortfallWithEvidence lets a negative case seed a provider
// evidence row that is not a confirmation; payment evidence is immutable once
// written, so the status must be chosen at insert time.
func seedChargebackShortfallWithEvidence(t *testing.T, ctx context.Context, repository *PostgresRepository,
	bonusShortfall bool, evidenceStatus string) chargebackRepairFixture {
	t.Helper()
	pool := repository.pool
	now := time.Now().UTC()
	fixture := chargebackRepairFixture{
		ChargebackCaseID: uuid.New(), EvidenceID: uuid.New(), AnomalyCaseID: uuid.New(),
		WalletID: uuid.New(), PackageID: uuid.New(), ComponentID: uuid.New(), LotID: uuid.New(),
		OrderID: uuid.New(), TransactionID: uuid.New(),
		ProviderChargebackID: "chargeback-" + uuid.NewString(), BaseMicrocredits: 100,
	}
	ownerID := uuid.New()
	// The consumed BASE (and BONUS) credit keeps the effective-conservation
	// invariant: available+used = original for every component.
	product, err := json.Marshal(Product{ProductID: "CHARGEBACK_REPAIR", AmountFen: 100, CreditMicrocredits: 100,
		BasePoints: 100, Currency: "CNY", PlanName: "ChargebackRepair"})
	if err != nil {
		t.Fatal(err)
	}
	statements := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO wallets(id,owner_type,owner_id,status,posted_microcredits,version,created_at,updated_at)
VALUES($1,'PERSONAL',$2,'ACTIVE',0,1,$3,$3)`, []any{fixture.WalletID, ownerID, now}},
		{`INSERT INTO ledger_accounts(id,wallet_id,code,account_type,created_at) VALUES($1,$2,$3,'WALLET_LIABILITY',$4)`,
			[]any{uuid.New(), fixture.WalletID, "wallet:" + fixture.WalletID.String(), now}},
		{`INSERT INTO payment_orders(id,merchant_order_no,requester_id,payer_user_id,wallet_id,product_id,product_snapshot,channel,
amount_fen,currency,credit_microcredits,status,fulfillment_status,expires_at,provider_payable_until,late_review_boundary_at,final_archive_eligible_at,
created_at,updated_at,provider_sku,provider_id,provider_version)
VALUES($1,$2,$3,$3,$4,'CHARGEBACK_REPAIR',$5,'ALIPAY',100,'CNY',100,'PAID','CREDITED',$6,$6,$7,$8,$9,$9,'sku-repair','test-provider','v1')`,
			[]any{fixture.OrderID, "chargeback-repair-" + fixture.OrderID.String(), ownerID, fixture.WalletID, product,
				now.Add(time.Hour), now.Add(72 * time.Hour), now.Add(8 * 24 * time.Hour), now}},
		{`INSERT INTO payment_transactions(id,payment_order_id,provider_id,provider_version,channel,provider_transaction_id,
status,fulfillment_status,amount_fen,currency,source,provider_status,provider_paid_at,verified_at,fulfilled_at,created_at,updated_at)
VALUES($1,$2,'test-provider','v1','ALIPAY',$3,'PAID','CREDITED',100,'CNY','QUERY','PAY_SUCCESS',$4,$4,$4,$4,$4)`,
			[]any{fixture.TransactionID, fixture.OrderID, "trade-" + fixture.TransactionID.String(), now}},
		{`INSERT INTO credit_lots(id,wallet_id,source_type,source_reference,original_microcredits,remaining_microcredits,refundable,expires_at,created_at)
VALUES($1,$2,'RECHARGE',$3,100,0,FALSE,$4,$5)`,
			[]any{fixture.LotID, fixture.WalletID, "recharge:" + fixture.LotID.String(), now.Add(24 * time.Hour), now}},
		{`INSERT INTO entitlement_packages(id,user_id,wallet_id,payment_order_id,source_payment_transaction_id,credit_lot_id,product_id,plan_name,sku_code,
amount_fen,points_total,points_remaining,points_used,credited_at,valid_from,valid_until,status,created_at,updated_at,source_type,source_reference)
VALUES($1,$2,$3,$4,$5,$6,'CHARGEBACK_REPAIR','ChargebackRepair','sku-repair',100,100,0,100,$7,$7,$8,'FROZEN',$7,$7,'PAYMENT',$9)`,
			[]any{fixture.PackageID, ownerID, fixture.WalletID, fixture.OrderID, fixture.TransactionID, fixture.LotID,
				now, now.Add(24 * time.Hour), "payment-transaction:" + fixture.TransactionID.String()}},
		{`INSERT INTO entitlement_components(id,package_id,component_type,credit_lot_id,original_microcredits,available_microcredits,
used_microcredits,credited_at,expires_at,status,created_at,updated_at)
VALUES($1,$2,'BASE',$3,100,0,100,$4,$5,'FROZEN',$4,$4)`,
			[]any{fixture.ComponentID, fixture.PackageID, fixture.LotID, now, now.Add(24 * time.Hour)}},
		{`INSERT INTO payment_evidence(id,payment_order_id,payment_transaction_id,evidence_type,provider_id,provider_version,evidence_key,
merchant_order_no,provider_transaction_id,amount_fen,currency,provider_status,received_at,payload_digest,payload_snapshot,merge_result)
VALUES($1,$2,$3,'CHARGEBACK','test-provider','v1',$4,$5,$6,100,'CNY',$7,$8,$9,'{}','RECORDED')`,
			[]any{fixture.EvidenceID, fixture.OrderID, fixture.TransactionID, fixture.ProviderChargebackID,
				"chargeback-repair-" + fixture.OrderID.String(), "trade-" + fixture.TransactionID.String(),
				evidenceStatus, now, strings.Repeat("cd", 32)}},
		{`INSERT INTO chargeback_cases(id,payment_evidence_id,payment_order_id,payment_transaction_id,entitlement_package_id,
wallet_id,provider_chargeback_id,status,created_at,version)
VALUES($1,$2,$3,$4,$5,$6,$7,'REVIEW_REQUIRED',$8,1)`,
			[]any{fixture.ChargebackCaseID, fixture.EvidenceID, fixture.OrderID, fixture.TransactionID,
				fixture.PackageID, fixture.WalletID, fixture.ProviderChargebackID, now}},
		{`INSERT INTO manual_review_cases(id,domain,case_type,resource_type,resource_id,severity,status,created_at,claim_due_at,
first_response_due_at,mitigation_due_at,resolution_due_at,resolved_at,updated_at)
VALUES($1,'BILLING','PAYMENT_ANOMALY','CHARGEBACK_CASE',$2,'P1','RESOLVED',$3,$3,$3,$3,$3,$3,$3)`,
			[]any{fixture.AnomalyCaseID, fixture.ChargebackCaseID, now}},
		{`INSERT INTO payment_anomaly_cases(id,anomaly_type,dedupe_key,payment_order_id,payment_transaction_id,payment_evidence_id,
chargeback_case_id,status,resolution_code,financial_action_status,resource_snapshot,version,created_at,updated_at,resolved_at)
VALUES($1,'CHARGEBACK',$2,$3,$4,$5,$6,'RESOLVED','ESCALATE_FINANCIAL_REPAIR','PENDING','{}',1,$7,$7,$7)`,
			[]any{fixture.AnomalyCaseID, "chargeback:" + fixture.ChargebackCaseID.String(), fixture.OrderID,
				fixture.TransactionID, fixture.EvidenceID, fixture.ChargebackCaseID, now}},
	}
	if bonusShortfall {
		bonusLotID, bonusComponentID := uuid.New(), uuid.New()
		statements = append(statements,
			struct {
				sql  string
				args []any
			}{`INSERT INTO credit_lots(id,wallet_id,source_type,source_reference,original_microcredits,remaining_microcredits,refundable,expires_at,created_at)
VALUES($1,$2,'RECHARGE',$3,50,0,FALSE,$4,$5)`, []any{bonusLotID, fixture.WalletID, "recharge:" + bonusLotID.String(), now.Add(24 * time.Hour), now}},
			struct {
				sql  string
				args []any
			}{`INSERT INTO entitlement_components(id,package_id,component_type,credit_lot_id,original_microcredits,available_microcredits,
used_microcredits,credited_at,expires_at,status,created_at,updated_at)
VALUES($1,$2,'BONUS',$3,50,0,50,$4,$5,'FROZEN',$4,$4)`, []any{bonusComponentID, fixture.PackageID, bonusLotID, now, now.Add(24 * time.Hour)}},
		)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	for index, statement := range statements {
		if _, err := tx.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatalf("chargeback shortfall statement %d: %v", index, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// TestChargebackFinancialRepairPostgresIntegration proves the GSA-32 domain
// contract on real PostgreSQL: the BASE shortfall becomes a CHARGEBACK
// receivable with the two-leg compensation journal, the case and its anomaly
// advance, nothing else moves, the replay is idempotent and every negative stays
// at zero writes.
func TestChargebackFinancialRepairPostgresIntegration(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("PINVOU_TEST_BILLING_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	repository, _ := newAdminPaginationIntegrationRepository(t, ctx, databaseURL)
	pool := repository.pool
	activationVersion := chargebackFixtureActivation(t, ctx, pool)("ACTIVE")

	fixture := seedChargebackShortfall(t, ctx, repository, true)
	command := ChargebackFinancialRepairCommandInput{ChargebackCaseID: fixture.ChargebackCaseID, PaymentEvidenceID: fixture.EvidenceID}
	plan, err := chargebackFinancialRepairPlanFrom(ctx, pool, command, 1, 1, activationVersion)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.BaseShortfallMicrocredits != 100 || plan.ComponentDigest == "" || plan.FactsDigest == "" ||
		plan.CaseVersion != 1 || plan.AnomalyVersion != 1 || plan.ProviderChargebackID != fixture.ProviderChargebackID {
		t.Fatalf("unexpected plan %+v", plan)
	}
	execute := func(requestID, eventID uuid.UUID, command ChargebackFinancialRepairCommandInput, frozen ChargebackFinancialRepairFrozenFacts) (ChargebackFinancialRepairResult, error) {
		return repository.ApplyChargebackFinancialRepair(ctx, ChargebackFinancialRepairExecutionInput{
			ApprovalRequestID: requestID, AuthorizationEventID: eventID, ApprovalEpoch: 3,
			ExecutionKey: "chargeback-repair:" + requestID.String(), PolicyVersion: 1, SchemaVersion: 1, ActivationVersion: activationVersion,
			Command: command, Frozen: frozen,
		})
	}
	frozen := ChargebackFinancialRepairFrozenFacts{CaseVersion: plan.CaseVersion,
		BaseShortfallMicrocredits: plan.BaseShortfallMicrocredits, ComponentDigest: plan.ComponentDigest,
		WalletID: plan.WalletID, WalletVersion: plan.WalletVersion, PackageID: plan.PackageID, FactsDigest: plan.FactsDigest}
	requestID, eventID := uuid.New(), uuid.New()
	applied, err := execute(requestID, eventID, command, frozen)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if applied.Status != "SUCCEEDED" || applied.BaseShortfallMicrocredits != 100 ||
		applied.ReceivableID == uuid.Nil || applied.JournalEntryID == uuid.Nil ||
		applied.ProviderChargebackID != fixture.ProviderChargebackID {
		t.Fatalf("unexpected result %+v", applied)
	}
	var caseStatus, resolutionCode, resolutionReason string
	var caseVersion int64
	var revokedBase, revokedBonus int64
	if err := pool.QueryRow(ctx, `SELECT status,resolution_code,resolution_reason,version,revoked_base_microcredits,revoked_bonus_microcredits
FROM chargeback_cases WHERE id=$1`, fixture.ChargebackCaseID).
		Scan(&caseStatus, &resolutionCode, &resolutionReason, &caseVersion, &revokedBase, &revokedBonus); err != nil ||
		caseStatus != "CONFIRMED" || resolutionCode != "CONFIRM_CHARGEBACK_SHORTFALL" || resolutionReason == "" ||
		caseVersion != 2 || revokedBase != 0 || revokedBonus != 0 {
		t.Fatalf("case=%s/%s/%s/%d/%d/%d err=%v", caseStatus, resolutionCode, resolutionReason, caseVersion, revokedBase, revokedBonus, err)
	}
	var receivableSource, receivableStatus string
	var original, outstanding int64
	if err := pool.QueryRow(ctx, `SELECT source_type,status,original_microcredits,outstanding_microcredits FROM receivables WHERE id=$1`, applied.ReceivableID).
		Scan(&receivableSource, &receivableStatus, &original, &outstanding); err != nil ||
		receivableSource != "CHARGEBACK" || receivableStatus != "OPEN" || original != 100 || outstanding != 100 {
		t.Fatalf("receivable=%s/%s/%d/%d err=%v", receivableSource, receivableStatus, original, outstanding, err)
	}
	var posted, receivableCredits int64
	var walletVersion int64
	var financialStatus, freezeReason string
	if err := pool.QueryRow(ctx, `SELECT posted_microcredits,receivable_microcredits,financial_status,COALESCE(financial_freeze_reason,''),version
FROM wallets WHERE id=$1`, fixture.WalletID).
		Scan(&posted, &receivableCredits, &financialStatus, &freezeReason, &walletVersion); err != nil ||
		posted != 0 || receivableCredits != 100 || financialStatus != "RECEIVABLE_OPEN" ||
		freezeReason != "CHARGEBACK" || walletVersion != 2 {
		t.Fatalf("wallet=%d/%d/%s/%s/%d err=%v", posted, receivableCredits, financialStatus, freezeReason, walletVersion, err)
	}
	var entryType, reference string
	var postings, ledgerSum int64
	if err := pool.QueryRow(ctx, `SELECT entry_type,business_reference FROM journal_entries WHERE id=$1`, applied.JournalEntryID).
		Scan(&entryType, &reference); err != nil || entryType != "CHARGEBACK" ||
		reference != "chargeback-repair:"+fixture.ChargebackCaseID.String() {
		t.Fatalf("journal=%s/%s err=%v", entryType, reference, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*),COALESCE(sum(delta_microcredits),0) FROM ledger_postings WHERE journal_entry_id=$1`,
		applied.JournalEntryID).Scan(&postings, &ledgerSum); err != nil || postings != 2 || ledgerSum != 0 {
		t.Fatalf("postings=%d sum=%d err=%v", postings, ledgerSum, err)
	}
	var receivableLeg, clearingLeg int64
	if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(delta_microcredits) FILTER (WHERE ledger_account_id=$2),0),
COALESCE(sum(delta_microcredits) FILTER (WHERE ledger_account_id=$3),0) FROM ledger_postings WHERE journal_entry_id=$1`,
		applied.JournalEntryID, receivableAccount, chargebackClearingAccount).Scan(&receivableLeg, &clearingLeg); err != nil ||
		receivableLeg != -100 || clearingLeg != 100 {
		t.Fatalf("receivable leg=%d clearing leg=%d err=%v", receivableLeg, clearingLeg, err)
	}
	// The components and lots of the package never move: this command only books
	// the shortfall the existing confirmation path can no longer reclaim.
	var componentCount, revocable, revoked, lotRemaining int64
	if err := pool.QueryRow(ctx, `SELECT count(*),COALESCE(sum(available_microcredits+active_grant_microcredits+suspended_grant_microcredits),0),
COALESCE(sum(revoked_microcredits),0) FROM entitlement_components WHERE package_id=$1`, fixture.PackageID).
		Scan(&componentCount, &revocable, &revoked); err != nil || componentCount != 2 || revocable != 0 || revoked != 0 {
		t.Fatalf("components=%d revocable=%d revoked=%d err=%v", componentCount, revocable, revoked, err)
	}
	if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(remaining_microcredits),0) FROM credit_lots WHERE wallet_id=$1`, fixture.WalletID).
		Scan(&lotRemaining); err != nil || lotRemaining != 0 {
		t.Fatalf("lot remaining=%d err=%v", lotRemaining, err)
	}
	var actionStatus string
	var anomalyVersion int64
	if err := pool.QueryRow(ctx, `SELECT financial_action_status,version FROM payment_anomaly_cases WHERE id=$1`, fixture.AnomalyCaseID).
		Scan(&actionStatus, &anomalyVersion); err != nil || actionStatus != "APPLIED" || anomalyVersion != 2 {
		t.Fatalf("anomaly=%s/%d err=%v", actionStatus, anomalyVersion, err)
	}
	// The same approval replays to the same case with no further effect.
	replayed, err := execute(requestID, eventID, command, frozen)
	if err != nil || replayed != applied {
		t.Fatalf("idempotent replay %+v err=%v want %+v", replayed, err, applied)
	}
	var cases, journals, receivables int64
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM chargeback_financial_repair_cases WHERE chargeback_case_id=$1),
	(SELECT count(*) FROM journal_entries WHERE business_reference='chargeback-repair:'||$1),
	(SELECT count(*) FROM receivables WHERE source_type='CHARGEBACK' AND source_reference='chargeback-repair:'||$1)`,
		fixture.ChargebackCaseID).Scan(&cases, &journals, &receivables); err != nil ||
		cases != 1 || journals != 1 || receivables != 1 {
		t.Fatalf("idempotency cases=%d journals=%d receivables=%d err=%v", cases, journals, receivables, err)
	}

	// Negatives keep the transaction closed with zero writes.
	revocableCase := seedChargebackShortfall(t, ctx, repository, false)
	if _, err := pool.Exec(ctx, `UPDATE entitlement_components SET available_microcredits=10,used_microcredits=90
WHERE package_id=$1 AND component_type='BASE'`, revocableCase.PackageID); err != nil {
		t.Fatal(err)
	}
	if _, err := chargebackFinancialRepairPlanFrom(ctx, pool, ChargebackFinancialRepairCommandInput{
		ChargebackCaseID: revocableCase.ChargebackCaseID, PaymentEvidenceID: revocableCase.EvidenceID}, 1, 1, 1); !errors.Is(err, ErrApprovalSnapshotMismatch) {
		t.Fatalf("revocable component err=%v", err)
	}
	notConfirmed := seedChargebackShortfallWithEvidence(t, ctx, repository, false, "CHARGEBACK_REJECTED")
	if _, err := chargebackFinancialRepairPlanFrom(ctx, pool, ChargebackFinancialRepairCommandInput{
		ChargebackCaseID: notConfirmed.ChargebackCaseID, PaymentEvidenceID: notConfirmed.EvidenceID}, 1, 1, 1); !errors.Is(err, ErrApprovalSnapshotMismatch) {
		t.Fatalf("unconfirmed evidence err=%v", err)
	}
	foreignEvidence := seedChargebackShortfall(t, ctx, repository, false)
	otherCase := seedChargebackShortfall(t, ctx, repository, false)
	if _, err := chargebackFinancialRepairPlanFrom(ctx, pool, ChargebackFinancialRepairCommandInput{
		ChargebackCaseID: foreignEvidence.ChargebackCaseID, PaymentEvidenceID: otherCase.EvidenceID}, 1, 1, 1); !errors.Is(err, ErrApprovalSnapshotMismatch) {
		t.Fatalf("foreign evidence err=%v", err)
	}
	dismissed := seedChargebackShortfall(t, ctx, repository, false)
	if _, err := pool.Exec(ctx, `UPDATE chargeback_cases SET status='DISMISSED',version=version+1 WHERE id=$1`, dismissed.ChargebackCaseID); err != nil {
		t.Fatal(err)
	}
	if _, err := chargebackFinancialRepairPlanFrom(ctx, pool, ChargebackFinancialRepairCommandInput{
		ChargebackCaseID: dismissed.ChargebackCaseID, PaymentEvidenceID: dismissed.EvidenceID}, 1, 1, 1); !errors.Is(err, ErrApprovalSnapshotMismatch) {
		t.Fatalf("dismissed case err=%v", err)
	}
	var strayCases int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM chargeback_financial_repair_cases`).Scan(&strayCases); err != nil || strayCases != 1 {
		t.Fatalf("negatives wrote cases=%d err=%v", strayCases, err)
	}
}
