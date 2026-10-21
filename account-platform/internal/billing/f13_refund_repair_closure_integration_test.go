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

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
)

// These regressions install the original public SQL with the existing isolated
// actors. Provider facts remain controlled; no real payment or release is claimed.
func f13RefundRepairInput(t *testing.T, ctx context.Context, r *PostgresRepository, f refundExceptionFixture, activation int64) (RefundExceptionRepairPlan, RefundExceptionRepairExecutionInput) {
	t.Helper()
	plan, err := refundExceptionRepairPlanFrom(ctx, r.pool, RefundExceptionRepairCommandInput{RefundID: f.RefundID, PaymentEvidenceID: f.EvidenceID, AnomalyCaseID: f.AnomalyCaseID}, 1, 1, activation)
	if err != nil {
		t.Fatal("refund repair preflight", err)
	}
	input := refundFrozenInput(plan)
	input.ActivationVersion = activation
	return plan, input
}

func f13PaymentOrderSnapshot(t *testing.T, ctx context.Context, r *PostgresRepository, orderID uuid.UUID) string {
	t.Helper()
	var snapshot string
	if err := r.pool.QueryRow(ctx, `SELECT to_jsonb(payment)::text FROM payment_orders payment WHERE id=$1`, orderID).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestF13RefundRepairProjectsOrderAndClosurePostgres(t *testing.T) {
	for _, test := range []struct {
		name                 string
		partial, otherReview bool
		attention            string
	}{
		{"full-reclaim", false, false, "NORMAL"},
		{"shortfall-preserves-receivable", true, false, "RECEIVABLE_OPEN"},
		{"unrelated-review-stays-open", false, true, "REVIEW_REQUIRED"},
	} {
		t.Run(test.name, func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			activation := refundFixtureActivation(t, ctx, r.pool)("ACTIVE")
			fixture := seedRefundException(t, ctx, r, test.partial)
			// Exercise a retained uncertain-dispatch lease, without changing money.
			leaseUntil := r.now().Add(time.Minute).Truncate(time.Microsecond)
			if tag, err := r.pool.Exec(ctx, `UPDATE refunds SET dispatch_state='RESULT_UNKNOWN',dispatch_lease_until=$2 WHERE id=$1 AND status='MANUAL_REVIEW'`, fixture.RefundID, leaseUntil); err != nil || tag.RowsAffected() != 1 {
				t.Fatal("uncertain refund dispatch fixture", err)
			}
			var intentVersion int64
			if err := r.pool.QueryRow(ctx, `SELECT intent_version FROM refunds WHERE id=$1 AND dispatch_state='RESULT_UNKNOWN' AND dispatch_lease_until=$2`, fixture.RefundID, leaseUntil).Scan(&intentVersion); err != nil {
				t.Fatal("original dispatch state not exercised", err)
			}
			plan, input := f13RefundRepairInput(t, ctx, r, fixture, activation)
			var ownerID uuid.UUID
			if err := r.pool.QueryRow(ctx, `SELECT owner_id FROM wallets WHERE id=$1`, plan.WalletID).Scan(&ownerID); err != nil {
				t.Fatal(err)
			}
			tx, err := r.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if test.otherReview {
				if _, err := createPaymentAnomalyWithinTransaction(ctx, tx, "DUPLICATE_PAYMENT", "P1", "f13-unrelated:"+plan.PaymentOrderID.String(), paymentAnomalyReference{PaymentOrderID: &plan.PaymentOrderID}, map[string]any{"controlled": true}, r.now()); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := projectPaymentOrderWithinTransaction(ctx, tx, plan.PaymentOrderID, r.now()); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			var version, reviewCount int64
			var attention string
			if err := r.pool.QueryRow(ctx, `SELECT summary_version,(refund_summary->>'review_count')::bigint,attention_summary FROM payment_orders WHERE id=$1`, plan.PaymentOrderID).Scan(&version, &reviewCount, &attention); err != nil {
				t.Fatal(err)
			}
			if reviewCount != 1 || attention != "REVIEW_REQUIRED" {
				t.Fatal("fixture did not project the original unresolved refund")
			}
			before, err := r.AccountClosureAssessment(ctx, ownerID)
			if err != nil || before.Ready || !f13HasClosureBlocker(before, "PAYMENT_PROCESSING") || !f13HasClosureBlocker(before, "REFUND_UNRESOLVED") {
				t.Fatal("original unresolved refund closure not exercised", err)
			}
			result, err := r.ApplyRefundExceptionRepair(ctx, input)
			if err != nil || result.Status != "SUCCEEDED" || result.RevokedMicrocredits != plan.RevokedMicrocredits || result.ShortfallMicrocredits != plan.ShortfallMicrocredits {
				t.Fatal("real repair", err)
			}
			var refundStatus, dispatchState string
			var leaseCleared bool
			var nextIntentVersion int64
			if err := r.pool.QueryRow(ctx, `SELECT status,dispatch_state,dispatch_lease_until IS NULL,intent_version FROM refunds WHERE id=$1`, fixture.RefundID).Scan(&refundStatus, &dispatchState, &leaseCleared, &nextIntentVersion); err != nil || refundStatus != "SUCCEEDED" || dispatchState != "COMPLETED" || !leaseCleared || nextIntentVersion != intentVersion+1 {
				t.Fatal("successful repair did not finish dispatch metadata exactly once", err)
			}
			var nextVersion, pendingCount, succeeded int64
			if err := r.pool.QueryRow(ctx, `SELECT summary_version,(refund_summary->>'review_count')::bigint,(refund_summary->>'pending_count')::bigint,(refund_summary->>'succeeded_amount_fen')::bigint,attention_summary FROM payment_orders WHERE id=$1`, plan.PaymentOrderID).Scan(&nextVersion, &reviewCount, &pendingCount, &succeeded, &attention); err != nil {
				t.Fatal(err)
			}
			if nextVersion != version+1 || reviewCount != 0 || pendingCount != 0 || succeeded != 100 || attention != test.attention {
				t.Fatalf("projection revision=%d expected=%d review=%d pending=%d succeeded=%d attention=%s", nextVersion, version+1, reviewCount, pendingCount, succeeded, attention)
			}
			after, err := r.AccountClosureAssessment(ctx, ownerID)
			if err != nil || f13HasClosureBlocker(after, "REFUND_UNRESOLVED") {
				t.Fatal("successful refund retained its unresolved blocker", err)
			}
			switch {
			case test.partial:
				if after.Ready || !f13HasClosureBlocker(after, "RECEIVABLE_OUTSTANDING") || !f13HasClosureBlocker(after, "PAYMENT_PROCESSING") {
					t.Fatal("shortfall improperly released closure")
				}
			case test.otherReview:
				if after.Ready || !f13HasClosureBlocker(after, "PAYMENT_PROCESSING") || !f13HasClosureBlocker(after, "PAYMENT_ANOMALY_OPEN") {
					t.Fatal("unrelated open review was cleared")
				}
			default:
				if !after.Ready || len(after.Blockers) != 0 {
					t.Fatal("complete local repair did not close the actual account blockers")
				}
			}
			snapshot := f13PaymentOrderSnapshot(t, ctx, r, plan.PaymentOrderID)
			// The full refunds rows in this snapshot also freeze dispatch/version/lease
			// metadata across the original-key receipt replay.
			financial := refundEffectSnapshot(t, ctx, r)
			same, err := r.ApplyRefundExceptionRepair(ctx, input)
			if err != nil || !reflect.DeepEqual(same, result) || f13PaymentOrderSnapshot(t, ctx, r, plan.PaymentOrderID) != snapshot || refundEffectSnapshot(t, ctx, r) != financial {
				t.Fatal("receipt replay changed the order revision or financial facts", err)
			}
		})
	}
}

func f13HasClosureBlocker(assessment AccountClosureAssessment, code string) bool {
	for _, blocker := range assessment.Blockers {
		if blocker.Code == code {
			return true
		}
	}
	return false
}

func TestF13RefundRepairAcquiresWalletBeforeRefundPostgres(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	activation := refundFixtureActivation(t, ctx, r.pool)("ACTIVE")
	fixture := seedRefundException(t, ctx, r, false)
	_, input := f13RefundRepairInput(t, ctx, r, fixture, activation)
	gate, err := r.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(context.Background())
	if _, err := getWallet(ctx, gate, fixture.WalletID, true); err != nil {
		t.Fatal(err)
	}
	var gatePID int
	if err := gate.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&gatePID); err != nil {
		t.Fatal(err)
	}
	var runtimeActor, runtimeSession string
	if err := gate.QueryRow(ctx, `SELECT current_user,session_user`).Scan(&runtimeActor, &runtimeSession); err != nil || runtimeActor != "billing_service" || runtimeSession != r.pool.Config().ConnConfig.User {
		t.Fatal("wallet lock fixture did not observe its exact runtime actor/session")
	}
	applyContext, cancel := context.WithCancel(ctx)
	type outcome struct {
		result RefundExceptionRepairResult
		err    error
	}
	applied := make(chan outcome, 1)
	finished := false
	go func() {
		result, err := r.ApplyRefundExceptionRepair(applyContext, input)
		applied <- outcome{result, err}
	}()
	defer func() {
		gate.Rollback(context.Background())
		cancel()
		if !finished {
			select {
			case <-applied:
			case <-time.After(5 * time.Second):
				t.Error("real Apply did not stop after fixture gate cleanup")
			}
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	blocked := false
	for time.Now().Before(deadline) {
		select {
		case got := <-applied:
			finished = true
			t.Fatal("Apply returned before the wallet gate", got.err)
		default:
		}
		// Statistics snapshots are cached inside the gate transaction. Refresh
		// only that observation before reading the real current blocking graph.
		if _, err := gate.Exec(ctx, `SELECT pg_stat_clear_snapshot()`); err != nil {
			t.Fatal("wallet wait statistics refresh", err)
		}
		if err := gate.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND usename=$2 AND $1=ANY(pg_blocking_pids(pid)))`, gatePID, runtimeSession).Scan(&blocked); err != nil {
			t.Fatal("real wallet wait observation", err)
		}
		if blocked {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !blocked {
		t.Fatal("real Apply did not wait on the wallet gate")
	}
	// The ordinary refund actor can now take its refund lock while holding the
	// wallet gate. The inverted implementation held this row while awaiting wallet.
	var refundStatus string
	if err := gate.QueryRow(ctx, `SELECT status FROM refunds WHERE id=$1 FOR UPDATE NOWAIT`, fixture.RefundID).Scan(&refundStatus); err != nil {
		t.Fatal("repair locked refund before its wallet", err)
	}
	if refundStatus != "MANUAL_REVIEW" {
		t.Fatal("blocked repair changed refund before the owner gate")
	}
	if err := gate.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-applied:
		finished = true
		if got.err != nil || got.result.Status != "SUCCEEDED" || got.result.RevokedMicrocredits != 100 || got.result.ShortfallMicrocredits != 0 {
			t.Fatal("real repair after correct gate release", got.err)
		}
	case <-ctx.Done():
		t.Fatal("real Apply did not finish within fixture budget")
	}
}

// This seed is the unchanged initial payment evidence fixture from the existing
// correction read-plan regression, with its full balanced recharge postings.
func f13PristineCorrectionPackage(t *testing.T, ctx context.Context, repository *PostgresRepository) (uuid.UUID, uuid.UUID, uuid.UUID) {
	t.Helper()
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

	return packageID, orderID, journalID
}

func f13CorrectionFactsSnapshot(t *testing.T, ctx context.Context, r *PostgresRepository) string {
	t.Helper()
	var snapshot string
	if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_array(
 (SELECT jsonb_agg(to_jsonb(w) ORDER BY id) FROM wallets w),
 (SELECT jsonb_agg(to_jsonb(o) ORDER BY id) FROM payment_orders o),
 (SELECT jsonb_agg(to_jsonb(p) ORDER BY id) FROM entitlement_packages p),
 (SELECT jsonb_agg(to_jsonb(c) ORDER BY id) FROM entitlement_components c),
 (SELECT jsonb_agg(to_jsonb(l) ORDER BY id) FROM credit_lots l),
 (SELECT jsonb_agg(to_jsonb(c) ORDER BY id) FROM refund_exception_repair_cases c),
 (SELECT jsonb_agg(to_jsonb(r) ORDER BY id) FROM refunds r),
 (SELECT jsonb_agg(to_jsonb(e) ORDER BY id) FROM payment_evidence e),
 (SELECT jsonb_agg(to_jsonb(c) ORDER BY id) FROM payment_anomaly_cases c),
 (SELECT jsonb_agg(to_jsonb(r) ORDER BY id) FROM receivables r),
 (SELECT jsonb_agg(to_jsonb(e) ORDER BY event_id) FROM outbox_events e),
 (SELECT jsonb_agg(to_jsonb(j) ORDER BY id) FROM journal_entries j),
 (SELECT jsonb_agg(to_jsonb(p) ORDER BY id) FROM ledger_postings p),
 (SELECT count(*) FROM outbox_events),(SELECT count(*) FROM receivables))::text`).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestF13CorrectionRejectsRefundRepairPackageHistoryPostgres(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	packageID, orderID, journalID := f13PristineCorrectionPackage(t, ctx, r)
	unrelatedPackage, unrelatedOrder, _ := f13PristineCorrectionPackage(t, ctx, r)
	original, err := r.planEntitlementComponentCorrection(ctx, packageID, orderID)
	if err != nil || original.Amounts.SourceType != "BASE" || original.Amounts.TargetType != "BONUS" || original.Amounts.Amount != 20 {
		t.Fatal("pristine correction positive control", err)
	}
	if f13LegacyRelatedHistory(t, ctx, r, original) {
		t.Fatal("pristine package has legacy financial history")
	}
	foreignRefund := seedRefundException(t, ctx, r, false)
	// All original PK/FK/check/immutable constraints stay enabled. The table has
	// independent refund_id and package_id FKs, so a package-history check cannot
	// infer its absence solely from refunds.entitlement_package_id. This controlled
	// immutable history fixture is not a production repair execution.
	if _, err := r.pool.Exec(ctx, `INSERT INTO refund_exception_repair_cases(id,approval_request_id,authorization_event_id,approval_epoch,execution_key_digest,refund_id,anomaly_case_id,wallet_id,package_id,revoked_microcredits,shortfall_microcredits,payment_evidence_id,facts_digest,journal_entry_id,state)
 VALUES($1,$2,$3,1,$4,$5,$6,$7,$8,100,0,$9,$10,$11,'EXECUTED')`, uuid.New(), uuid.New(), uuid.New(), strings.Repeat("cd", 32), foreignRefund.RefundID, foreignRefund.AnomalyCaseID, foreignRefund.WalletID, packageID, foreignRefund.EvidenceID, strings.Repeat("ef", 32), journalID); err != nil {
		t.Fatal("legal independent-FK refund history fixture", err)
	}
	var identity bool
	if err := r.pool.QueryRow(ctx, `SELECT repair.package_id=$1 AND refund.entitlement_package_id<>repair.package_id FROM refund_exception_repair_cases repair JOIN refunds refund ON refund.id=repair.refund_id WHERE repair.package_id=$1`, packageID).Scan(&identity); err != nil || !identity {
		t.Fatal("independent package history was not exercised", err)
	}
	if f13LegacyRelatedHistory(t, ctx, r, original) {
		t.Fatal("the direct case counterexample also triggered an old history predicate")
	}
	before := f13CorrectionFactsSnapshot(t, ctx, r)
	if _, err := r.planEntitlementComponentCorrection(ctx, packageID, orderID); !errors.Is(err, ErrConflict) {
		t.Fatal("refund repair package history qualified for zero-history reclassification", err)
	}
	valid, err := r.planEntitlementComponentCorrection(ctx, unrelatedPackage, unrelatedOrder)
	if err != nil || valid.Amounts.Amount != 20 {
		t.Fatal("unrelated package was incorrectly disqualified", err)
	}
	if f13CorrectionFactsSnapshot(t, ctx, r) != before {
		t.Fatal("read-only eligibility mutated financial history")
	}
}

func TestF13CorrectionWithoutRefundRepairFacilityPostgres(t *testing.T) {
	raw := strings.TrimSpace(os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL"))
	if raw == "" {
		t.Skip("PINVOU_TEST_BILLING_DATABASE_URL is required for actual PostgreSQL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	r, _ := entitlementRepositoryWithStage(t, ctx, raw, func(loaded []migrations.Migration) ([]migrations.Migration, error) {
		selected := make([]migrations.Migration, 0, len(loaded))
		removed := 0
		for _, migration := range loaded {
			expected := ""
			switch migration.Version {
			case 67:
				expected = "refund_exception_repair_cases"
			case 68:
				expected = "refund_exception_repair_proof_claim_allowlist"
			case 70:
				expected = "chargeback_financial_repair_cases"
			case 71:
				expected = "chargeback_financial_repair_proof_claim_allowlist"
			case 72:
				expected = "reconciliation_repair_cases"
			case 73:
				expected = "reconciliation_financial_repair_proof_claim_allowlist"
			case 74:
				expected = "usage_settlement_repair_cases"
			case 75:
				expected = "usage_loss_review_settled_state"
			case 76:
				expected = "usage_settlement_repair_proof_claim_allowlist"
			case 900018:
				expected = "refund_activation_execution_lock"
			case 900019:
				expected = "chargeback_activation_execution_lock"
			case 900020:
				expected = "reconciliation_activation_execution_lock"
			case 900021:
				expected = "usage_activation_execution_lock"
			}
			if expected != "" {
				if migration.Name != expected {
					return nil, errors.New("absent refund facility fixture migration identity changed")
				}
				removed++
				continue
			}
			selected = append(selected, migration)
		}
		if removed != 13 {
			return nil, errors.New("absent refund fixture requires its original three migrations and complete dependent financial phases")
		}
		return selected, nil
	})
	var installed bool
	if err := r.pool.QueryRow(ctx, `SELECT to_regclass('public.refund_exception_repair_cases') IS NOT NULL`).Scan(&installed); err != nil || installed {
		t.Fatal("old installed phase unexpectedly contains the new case facility", err)
	}
	packageID, orderID, _ := f13PristineCorrectionPackage(t, ctx, r)
	valid, err := r.planEntitlementComponentCorrection(ctx, packageID, orderID)
	if err != nil || valid.Amounts.Amount != 20 {
		t.Fatal("older valid correction installation lost its eligibility", err)
	}
}

// Keep the preceding predicate unchanged so this regression proves that only
// the current wave's direct package-history facility closes the omission.
func f13LegacyRelatedHistory(t *testing.T, ctx context.Context, r *PostgresRepository, plan entitlementCorrectionReadPlan) bool {
	t.Helper()
	var related bool
	if err := r.pool.QueryRow(ctx, `SELECT
 EXISTS(SELECT 1 FROM member_quota_grants WHERE package_id=$1) OR
 EXISTS(SELECT 1 FROM usage_hold_allocations WHERE package_id=$1) OR
 EXISTS(SELECT 1 FROM refunds WHERE entitlement_package_id=$1) OR
 EXISTS(SELECT 1 FROM forced_refund_cases WHERE entitlement_package_id=$1) OR
 EXISTS(SELECT 1 FROM chargeback_cases WHERE entitlement_package_id=$1) OR
 EXISTS(SELECT 1 FROM receivables WHERE entitlement_package_id=$1) OR
 EXISTS(SELECT 1 FROM entitlement_component_corrections WHERE package_id=$1) OR
 EXISTS(SELECT 1 FROM payment_order_repairs WHERE payment_order_id=$2) OR
 EXISTS(SELECT 1 FROM payment_anomaly_cases WHERE payment_order_id=$2 OR payment_transaction_id=$3)`, plan.PackageID, plan.PaymentOrderID, plan.PaymentTransactionID).Scan(&related); err != nil {
		t.Fatal(err)
	}
	return related
}
