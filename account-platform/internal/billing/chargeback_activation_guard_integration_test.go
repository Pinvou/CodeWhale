package billing

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"os"
	"strings"
	"testing"
	"time"
)

func TestF14ChargebackActivationFinancialTransactionPostgres(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("PINVOU_TEST_BILLING_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	repository, _ := newAdminPaginationIntegrationRepository(t, ctx, databaseURL)
	pool := repository.pool
	setActivation := chargebackFixtureActivation(t, ctx, pool)
	activationVersion := setActivation("INACTIVE")

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

	baseline := chargebackActivationFinancialSnapshot(t, ctx, repository)
	t.Run("inactive-zero-write", func(t *testing.T) {
		if _, err := execute(requestID, eventID, command, frozen); !errors.Is(err, ErrApprovalSnapshotMismatch) {
			t.Fatal("inactive fresh execution accepted", err)
		}
		if chargebackActivationFinancialSnapshot(t, ctx, repository) != baseline {
			t.Fatal("inactive changed financial or order facts")
		}
	})
	activationVersion = setActivation("ACTIVE")
	t.Run("operator-version-zero-write", func(t *testing.T) {
		setActivation("INACTIVE")
		if _, err := execute(requestID, eventID, command, frozen); !errors.Is(err, ErrApprovalSnapshotMismatch) {
			t.Fatal("stale activation execution accepted", err)
		}
		if chargebackActivationFinancialSnapshot(t, ctx, repository) != baseline {
			t.Fatal("stale version changed financial or order facts")
		}
	})
	activationVersion = setActivation("ACTIVE")
	t.Run("operator-waits-for-financial-commit-and-old-receipt", func(t *testing.T) {
		currentPlan, err := chargebackFinancialRepairPlanFrom(ctx, pool, command, 1, 1, activationVersion)
		if err != nil {
			t.Fatal("current active owner projection failed", err)
		}
		frozen = ChargebackFinancialRepairFrozenFacts{CaseVersion: currentPlan.CaseVersion,
			BaseShortfallMicrocredits: currentPlan.BaseShortfallMicrocredits, ComponentDigest: currentPlan.ComponentDigest,
			WalletID: currentPlan.WalletID, WalletVersion: currentPlan.WalletVersion, PackageID: currentPlan.PackageID, FactsDigest: currentPlan.FactsDigest}
		input := ChargebackFinancialRepairExecutionInput{ApprovalRequestID: requestID, AuthorizationEventID: eventID, ApprovalEpoch: 3, ExecutionKey: "race:chargeback-repair", PolicyVersion: 1, SchemaVersion: 1, ActivationVersion: activationVersion, Command: command, Frozen: frozen}
		result, err := chargebackFinancialWithDeactivationRace(t, ctx, repository, input)
		if err != nil || result.Status != "SUCCEEDED" || result.BaseShortfallMicrocredits != 100 {
			t.Fatal("qualified chargeback race failed", err)
		}
		var posted, receivable, cases, postings, total int64
		if err := pool.QueryRow(ctx, `SELECT (SELECT posted_microcredits FROM wallets WHERE id=$1),(SELECT receivable_microcredits FROM wallets WHERE id=$1),(SELECT count(*) FROM chargeback_financial_repair_cases WHERE approval_request_id=$2),(SELECT count(*) FROM ledger_postings WHERE journal_entry_id=$3),(SELECT COALESCE(sum(delta_microcredits),0) FROM ledger_postings WHERE journal_entry_id=$3)`, fixture.WalletID, requestID, result.JournalEntryID).Scan(&posted, &receivable, &cases, &postings, &total); err != nil || posted != 0 || receivable != 100 || cases != 1 || postings != 2 || total != 0 {
			t.Fatal("actual chargeback compensation differs", err)
		}
		var original, available, used int64
		if err := pool.QueryRow(ctx, `SELECT sum(original_microcredits),sum(available_microcredits),sum(used_microcredits) FROM entitlement_components WHERE package_id=$1`, fixture.PackageID).Scan(&original, &available, &used); err != nil || original != 150 || available != 0 || used != 150 {
			t.Fatal("BASE or BONUS entitlement changed", err)
		}
		var summaryVersion int64
		var attention, open, lost string
		if err := pool.QueryRow(ctx, `SELECT summary_version,attention_summary,chargeback_summary->>'open_count',chargeback_summary->>'lost_amount_fen' FROM payment_orders WHERE id=$1`, fixture.OrderID).Scan(&summaryVersion, &attention, &open, &lost); err != nil || summaryVersion != 2 || attention != "RECEIVABLE_OPEN" || open != "1" || lost != "100" {
			t.Fatal("retained order summary repair differs", err)
		}
		after := chargebackActivationFinancialSnapshot(t, ctx, repository)
		replay, err := repository.ApplyChargebackFinancialRepair(ctx, input)
		if err != nil || replay != result || chargebackActivationFinancialSnapshot(t, ctx, repository) != after {
			t.Fatal("inactive immutable receipt replay changed financial or order facts", err)
		}
	})
}

func chargebackActivationFinancialSnapshot(t *testing.T, ctx context.Context, repo *PostgresRepository) string {
	t.Helper()
	var result string
	if err := repo.pool.QueryRow(ctx, `SELECT jsonb_build_object(
 'wallets',(SELECT COALESCE(jsonb_agg(to_jsonb(t) ORDER BY id),'[]'::jsonb) FROM wallets t),
 'lots',(SELECT COALESCE(jsonb_agg(to_jsonb(t) ORDER BY id),'[]'::jsonb) FROM credit_lots t),
 'packages',(SELECT COALESCE(jsonb_agg(to_jsonb(t) ORDER BY id),'[]'::jsonb) FROM entitlement_packages t),
 'components',(SELECT COALESCE(jsonb_agg(to_jsonb(t) ORDER BY id),'[]'::jsonb) FROM entitlement_components t),
 'orders',(SELECT COALESCE(jsonb_agg(to_jsonb(t) ORDER BY id),'[]'::jsonb) FROM payment_orders t),
 'transactions',(SELECT COALESCE(jsonb_agg(to_jsonb(t) ORDER BY id),'[]'::jsonb) FROM payment_transactions t),
 'chargebacks',(SELECT COALESCE(jsonb_agg(to_jsonb(t) ORDER BY id),'[]'::jsonb) FROM chargeback_cases t),
 'receivables',(SELECT COALESCE(jsonb_agg(to_jsonb(t) ORDER BY id),'[]'::jsonb) FROM receivables t),
 'anomalies',(SELECT COALESCE(jsonb_agg(to_jsonb(t) ORDER BY id),'[]'::jsonb) FROM payment_anomaly_cases t),
 'cases',(SELECT COALESCE(jsonb_agg(to_jsonb(t) ORDER BY id),'[]'::jsonb) FROM chargeback_financial_repair_cases t),
 'journals',(SELECT COALESCE(jsonb_agg(to_jsonb(t) ORDER BY id),'[]'::jsonb) FROM journal_entries t),
 'postings',(SELECT COALESCE(jsonb_agg(to_jsonb(t) ORDER BY id),'[]'::jsonb) FROM ledger_postings t))::text`).Scan(&result); err != nil {
		t.Fatal("financial snapshot failed")
	}
	return result
}
