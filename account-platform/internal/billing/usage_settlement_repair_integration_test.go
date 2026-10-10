package billing

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

type usageRepairFixture struct {
	HoldID         uuid.UUID
	UsageRequestID uuid.UUID
	LossReviewID   uuid.UUID
	WalletID       uuid.UUID
	PackageID      uuid.UUID
	ComponentID    uuid.UUID
	LotID          uuid.UUID
	Claimed        int64
	Backed         int64
	Posted         int64
}

// seedUnsettledUsageLoss writes one released-with-loss usage hold whose whole
// reservation was returned to the component, plus the open loss review the
// governed repair is allowed to settle. claimed is the provider-independent
// amount the review recorded as actually used.
func seedUnsettledUsageLoss(t *testing.T, ctx context.Context, repository *PostgresRepository, suffix string, claimed, backed, posted int64) usageRepairFixture {
	t.Helper()
	pool := repository.pool
	now := time.Now().UTC()
	fixture := usageRepairFixture{
		HoldID: uuid.New(), UsageRequestID: uuid.New(), LossReviewID: uuid.New(), WalletID: uuid.New(),
		PackageID: uuid.New(), ComponentID: uuid.New(), LotID: uuid.New(),
		Claimed: claimed, Backed: backed, Posted: posted,
	}
	ownerID := uuid.New()
	priceID, formulaID := uuid.New(), uuid.MustParse("00000000-0000-7000-8200-000000000001")
	model := "usage-repair-" + fixture.HoldID.String()
	priceJSON, _ := json.Marshal(PriceVersion{ID: priceID, Model: model, ProtocolCode: "CHAT", FormulaVersionID: formulaID, CreditUnit: "MICROCREDIT", InputPerMillion: 1000000})
	usageJSON, _ := json.Marshal(usageRepairUsage{InputTokens: claimed})
	product, err := json.Marshal(Product{ProductID: "USAGE_REPAIR", AmountFen: 100, CreditMicrocredits: posted,
		BasePoints: posted, Currency: "CNY", PlanName: "UsageRepair"})
	if err != nil {
		t.Fatal(err)
	}
	orderID := uuid.New()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	for index, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO wallets(id,owner_type,owner_id,status,posted_microcredits,version,created_at,updated_at)
VALUES($1,'PERSONAL',$2,'ACTIVE',$3,1,$4,$4)`, []any{fixture.WalletID, ownerID, posted, now}},
		{`INSERT INTO ledger_accounts(id,wallet_id,code,account_type,created_at) VALUES($1,$2,$3,'WALLET_LIABILITY',$4)`,
			[]any{uuid.New(), fixture.WalletID, "wallet:" + fixture.WalletID.String(), now}},
		{`INSERT INTO payment_orders(id,merchant_order_no,requester_id,payer_user_id,wallet_id,product_id,product_snapshot,channel,
amount_fen,currency,credit_microcredits,status,fulfillment_status,expires_at,provider_payable_until,late_review_boundary_at,final_archive_eligible_at,
created_at,updated_at,provider_sku,provider_id,provider_version)
VALUES($1,$2,$3,$3,$4,'USAGE_REPAIR',$5,'ALIPAY',100,'CNY',$6,'PAID','CREDITED',$7,$7,$8,$9,$10,$10,'sku-repair','test-provider','v1')`,
			[]any{orderID, "usage-repair-" + orderID.String(), ownerID, fixture.WalletID, product, posted,
				now.Add(time.Hour), now.Add(72 * time.Hour), now.Add(8 * 24 * time.Hour), now}},
		{`INSERT INTO credit_lots(id,wallet_id,source_type,source_reference,original_microcredits,remaining_microcredits,refundable,expires_at,created_at)
VALUES($1,$2,'RECHARGE',$3,$4,$5,TRUE,$6,$7)`,
			[]any{fixture.LotID, fixture.WalletID, "recharge:" + fixture.LotID.String(), posted, backed, now.Add(24 * time.Hour), now}},
		{`INSERT INTO entitlement_packages(id,user_id,wallet_id,product_id,plan_name,sku_code,amount_fen,points_total,points_remaining,
credited_at,valid_from,valid_until,status,created_at,updated_at,source_type,source_reference,credit_lot_id)
VALUES($1,$2,$3,'USAGE_REPAIR','UsageRepair','sku-repair',100,$4,$5,$6,$6,$7,'ACTIVE',$6,$6,'SYSTEM',$8,$9)`,
			[]any{fixture.PackageID, ownerID, fixture.WalletID, posted, backed, now, now.Add(24 * time.Hour),
				"usage-repair:" + fixture.PackageID.String(), fixture.LotID}},
		{`INSERT INTO entitlement_components(id,package_id,component_type,credit_lot_id,original_microcredits,available_microcredits,
credited_at,expires_at,status,created_at,updated_at)
VALUES($1,$2,'BASE',$3,$4,$5,$6,$7,'ACTIVE',$6,$6)`,
			[]any{fixture.ComponentID, fixture.PackageID, fixture.LotID, posted, backed, now, now.Add(24 * time.Hour)}},
		{`INSERT INTO usage_holds(id,usage_request_id,wallet_id,status,max_microcredits,actual_microcredits,usage_snapshot,
expires_at,terminal_at,created_at,updated_at)
VALUES($1,$2,$3,'RELEASED',$4,NULL,jsonb_build_object('release_reason','ENTITLEMENT_EXPIRED_UNCERTAIN'),$5,$5,$5,$5)`,
			[]any{fixture.HoldID, fixture.UsageRequestID, fixture.WalletID, backed, now}},
		{`INSERT INTO usage_hold_allocations(hold_id,package_id,component_id,reserved_microcredits,settled_microcredits,
released_microcredits,created_at,updated_at)
VALUES($1,$2,$3,$4,0,$4,$5,$5)`,
			[]any{fixture.HoldID, fixture.PackageID, fixture.ComponentID, backed, now}},
		{`INSERT INTO usage_loss_reviews(id,usage_request_id,hold_id,wallet_id,reason_code,claimed_actual_microcredits,
usage_snapshot,status,created_at,updated_at)
VALUES($1,$2,$3,$4,'ENTITLEMENT_EXPIRED_UNCERTAIN',$5,$7,'REVIEW_REQUIRED',$6,$6)`,
			[]any{fixture.LossReviewID, fixture.UsageRequestID, fixture.HoldID, fixture.WalletID, claimed, now, usageJSON}},
	} {
		if _, err := tx.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatalf("usage repair statement %d (%s): %v", index, suffix, err)
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO pricing_versions(id,model,input_per_million,output_per_million,cached_input_per_million,effective_from,created_at,scope_type,scope_id,protocol_code,formula_version_id,credit_unit,lifecycle_status,published_at) VALUES($1,$2,1000000,0,0,$3,$3,'MODEL',$2,'CHAT',$4,'MICROCREDIT','ACTIVE',$3)`, priceID, model, now, formulaID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE usage_holds SET price_version_id=$2,model_id=$3,protocol_code=$4,formula_version_id=$5,pricing_snapshot=$6 WHERE id=$1`, fixture.HoldID, priceID, model, "CHAT", formulaID, priceJSON); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// TestUsageSettlementRepairPostgresIntegration proves the GSA-36 domain contract
// on real PostgreSQL: an unsettled usage loss is补结算 against the capacity the
// release path gave back, the remainder becomes an overage receivable instead of
// available credit, the loss review advances to RESOLVED_SETTLED, the replay is
// idempotent and every negative stays at zero writes.
func TestUsageSettlementRepairPostgresIntegration(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("PINVOU_TEST_BILLING_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	repository, _ := newAdminPaginationIntegrationRepository(t, ctx, databaseURL)
	pool := repository.pool
	activationVersion := usageRepairFixtureActivation(t, ctx, pool)("ACTIVE")

	full := seedUnsettledUsageLoss(t, ctx, repository, "full", 100, 100, 100)
	command := UsageSettlementRepairCommandInput{UsageRequestID: full.UsageRequestID, HoldID: full.HoldID,
		EvidenceKind: usageSettlementRepairEvidenceKind, LossReviewID: full.LossReviewID}
	plan, err := usageSettlementRepairPlanFrom(ctx, pool, command, 1, 1, activationVersion)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.ExpectedMicrocredits != 100 || plan.SettledMicrocredits != 100 || plan.OverageMicrocredits != 0 ||
		plan.AllocationsDigest == "" || plan.FactsDigest == "" || len(plan.Allocations) != 1 {
		t.Fatalf("unexpected plan %+v", plan)
	}
	execute := func(requestID, eventID uuid.UUID, command UsageSettlementRepairCommandInput, frozen UsageSettlementRepairFrozenFacts) (UsageSettlementRepairResult, error) {
		return repository.ApplyUsageSettlementRepair(ctx, UsageSettlementRepairExecutionInput{
			ApprovalRequestID: requestID, AuthorizationEventID: eventID, ApprovalEpoch: 5,
			ExecutionKey: "usage-repair:" + requestID.String(), PolicyVersion: 1, SchemaVersion: 1, ActivationVersion: activationVersion,
			Command: command, Frozen: frozen,
		})
	}
	frozen := UsageSettlementRepairFrozenFacts{HoldUpdatedAt: plan.HoldUpdatedAt, LossReviewUpdatedAt: plan.LossReviewUpdatedAt,
		ExpectedMicrocredits: plan.ExpectedMicrocredits, SettledMicrocredits: plan.SettledMicrocredits,
		WalletID: plan.WalletID, WalletVersion: plan.WalletVersion, AllocationsDigest: plan.AllocationsDigest, FactsDigest: plan.FactsDigest}
	requestID, eventID := uuid.New(), uuid.New()
	applied, err := execute(requestID, eventID, command, frozen)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if applied.Status != "SUCCEEDED" || applied.ExpectedMicrocredits != 100 || applied.SettledMicrocredits != 100 ||
		applied.OverageMicrocredits != 0 || applied.ReceivableID != nil || applied.JournalEntryID == uuid.Nil {
		t.Fatalf("unexpected result %+v", applied)
	}
	var reviewStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM usage_loss_reviews WHERE id=$1`, full.LossReviewID).Scan(&reviewStatus); err != nil ||
		reviewStatus != "RESOLVED_SETTLED" {
		t.Fatalf("loss review=%s err=%v", reviewStatus, err)
	}
	var holdStatus string
	var holdActual int64
	var repairSnapshot []byte
	if err := pool.QueryRow(ctx, `SELECT status,actual_microcredits,usage_snapshot->'settlement_repair' FROM usage_holds WHERE id=$1`, full.HoldID).
		Scan(&holdStatus, &holdActual, &repairSnapshot); err != nil || holdStatus != "RELEASED" || holdActual != 100 || len(repairSnapshot) == 0 {
		t.Fatalf("hold=%s/%d/%s err=%v", holdStatus, holdActual, repairSnapshot, err)
	}
	var componentAvailable, componentUsed, lotRemaining, allocationSettled, allocationReleased int64
	if err := pool.QueryRow(ctx, `SELECT component.available_microcredits,component.used_microcredits,lot.remaining_microcredits
FROM entitlement_components component JOIN credit_lots lot ON lot.id=component.credit_lot_id WHERE component.id=$1`, full.ComponentID).
		Scan(&componentAvailable, &componentUsed, &lotRemaining); err != nil ||
		componentAvailable != 0 || componentUsed != 100 || lotRemaining != 0 {
		t.Fatalf("component=%d/%d lot=%d err=%v", componentAvailable, componentUsed, lotRemaining, err)
	}
	if err := pool.QueryRow(ctx, `SELECT settled_microcredits,released_microcredits FROM usage_hold_allocations
WHERE hold_id=$1 AND component_id=$2`, full.HoldID, full.ComponentID).Scan(&allocationSettled, &allocationReleased); err != nil ||
		allocationSettled != 100 || allocationReleased != 0 {
		t.Fatalf("allocation=%d/%d err=%v", allocationSettled, allocationReleased, err)
	}
	var walletPosted int64
	if err := pool.QueryRow(ctx, `SELECT posted_microcredits FROM wallets WHERE id=$1`, full.WalletID).Scan(&walletPosted); err != nil ||
		walletPosted != 0 {
		t.Fatalf("wallet posted=%d err=%v", walletPosted, err)
	}
	var entryType, reference string
	var postings, ledgerSum, revenueLeg int64
	if err := pool.QueryRow(ctx, `SELECT entry_type,business_reference FROM journal_entries WHERE id=$1`, applied.JournalEntryID).
		Scan(&entryType, &reference); err != nil || entryType != "USAGE" ||
		reference != "usage-settlement-repair:"+full.UsageRequestID.String() {
		t.Fatalf("journal=%s/%s err=%v", entryType, reference, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*),COALESCE(sum(delta_microcredits),0),
COALESCE(sum(delta_microcredits) FILTER (WHERE ledger_account_id=$2),0) FROM ledger_postings WHERE journal_entry_id=$1`,
		applied.JournalEntryID, usageRevenueAccount).Scan(&postings, &ledgerSum, &revenueLeg); err != nil ||
		postings != 2 || ledgerSum != 0 || revenueLeg != 100 {
		t.Fatalf("postings=%d sum=%d revenue=%d err=%v", postings, ledgerSum, revenueLeg, err)
	}
	// The same approval replays to the same case with no further effect.
	replayed, err := execute(requestID, eventID, command, frozen)
	if err != nil || !reflect.DeepEqual(replayed, applied) {
		t.Fatalf("idempotent replay %+v err=%v want %+v", replayed, err, applied)
	}
	var cases, journals int64
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM usage_settlement_repair_cases WHERE usage_request_id=$1),
		(SELECT count(*) FROM journal_entries WHERE business_reference='usage-settlement-repair:'||$1)`,
		full.UsageRequestID).Scan(&cases, &journals); err != nil || cases != 1 || journals != 1 {
		t.Fatalf("idempotency cases=%d journals=%d err=%v", cases, journals, err)
	}

	// Partial backing: the unbacked remainder becomes an overage receivable, never
	// available credit.
	partial := seedUnsettledUsageLoss(t, ctx, repository, "partial", 150, 100, 100)
	partialCommand := UsageSettlementRepairCommandInput{UsageRequestID: partial.UsageRequestID, HoldID: partial.HoldID,
		EvidenceKind: usageSettlementRepairEvidenceKind, LossReviewID: partial.LossReviewID}
	partialPlan, err := usageSettlementRepairPlanFrom(ctx, pool, partialCommand, 1, 1, activationVersion)
	if err != nil {
		t.Fatalf("partial plan: %v", err)
	}
	if partialPlan.SettledMicrocredits != 100 || partialPlan.OverageMicrocredits != 50 {
		t.Fatalf("partial split %+v", partialPlan)
	}
	partialFrozen := UsageSettlementRepairFrozenFacts{HoldUpdatedAt: partialPlan.HoldUpdatedAt,
		LossReviewUpdatedAt: partialPlan.LossReviewUpdatedAt, ExpectedMicrocredits: partialPlan.ExpectedMicrocredits,
		SettledMicrocredits: partialPlan.SettledMicrocredits, WalletID: partialPlan.WalletID,
		WalletVersion: partialPlan.WalletVersion, AllocationsDigest: partialPlan.AllocationsDigest, FactsDigest: partialPlan.FactsDigest}
	partialResult, err := execute(uuid.New(), uuid.New(), partialCommand, partialFrozen)
	if err != nil {
		t.Fatalf("partial apply: %v", err)
	}
	if partialResult.OverageMicrocredits != 50 || partialResult.ReceivableID == nil {
		t.Fatalf("partial result %+v", partialResult)
	}
	var receivableSource, receivableStatus string
	var receivableOutstanding, walletReceivables int64
	if err := pool.QueryRow(ctx, `SELECT source_type,status,outstanding_microcredits FROM receivables WHERE id=$1`, *partialResult.ReceivableID).
		Scan(&receivableSource, &receivableStatus, &receivableOutstanding); err != nil ||
		receivableSource != "USAGE_OVERAGE" || receivableStatus != "OPEN" || receivableOutstanding != 50 {
		t.Fatalf("receivable=%s/%s/%d err=%v", receivableSource, receivableStatus, receivableOutstanding, err)
	}
	if err := pool.QueryRow(ctx, `SELECT receivable_microcredits FROM wallets WHERE id=$1`, partial.WalletID).Scan(&walletReceivables); err != nil ||
		walletReceivables != 50 {
		t.Fatalf("wallet receivables=%d err=%v", walletReceivables, err)
	}

	// Negatives keep the transaction closed with zero writes.
	committed := seedUnsettledUsageLoss(t, ctx, repository, "committed", 100, 100, 100)
	if _, err := pool.Exec(ctx, `UPDATE usage_holds SET status='COMMITTED' WHERE id=$1`, committed.HoldID); err != nil {
		t.Fatal(err)
	}
	if _, err := usageSettlementRepairPlanFrom(ctx, pool, UsageSettlementRepairCommandInput{UsageRequestID: committed.UsageRequestID,
		HoldID: committed.HoldID, EvidenceKind: usageSettlementRepairEvidenceKind, LossReviewID: committed.LossReviewID}, 1, 1, 1); !errors.Is(err, ErrApprovalSnapshotMismatch) {
		t.Fatalf("committed hold err=%v", err)
	}
	resolved := seedUnsettledUsageLoss(t, ctx, repository, "resolved", 100, 100, 100)
	if _, err := pool.Exec(ctx, `UPDATE usage_loss_reviews SET status='RESOLVED_PLATFORM_LOSS' WHERE id=$1`, resolved.LossReviewID); err != nil {
		t.Fatal(err)
	}
	if _, err := usageSettlementRepairPlanFrom(ctx, pool, UsageSettlementRepairCommandInput{UsageRequestID: resolved.UsageRequestID,
		HoldID: resolved.HoldID, EvidenceKind: usageSettlementRepairEvidenceKind, LossReviewID: resolved.LossReviewID}, 1, 1, 1); !errors.Is(err, ErrApprovalSnapshotMismatch) {
		t.Fatalf("resolved review err=%v", err)
	}
	foreign := seedUnsettledUsageLoss(t, ctx, repository, "foreign", 100, 100, 100)
	other := seedUnsettledUsageLoss(t, ctx, repository, "other", 100, 100, 100)
	if _, err := usageSettlementRepairPlanFrom(ctx, pool, UsageSettlementRepairCommandInput{UsageRequestID: foreign.UsageRequestID,
		HoldID: foreign.HoldID, EvidenceKind: usageSettlementRepairEvidenceKind, LossReviewID: other.LossReviewID}, 1, 1, 1); !errors.Is(err, ErrApprovalSnapshotMismatch) {
		t.Fatalf("foreign loss review err=%v", err)
	}
	var strayCases int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM usage_settlement_repair_cases`).Scan(&strayCases); err != nil || strayCases != 2 {
		t.Fatalf("negatives wrote cases=%d err=%v", strayCases, err)
	}
}
