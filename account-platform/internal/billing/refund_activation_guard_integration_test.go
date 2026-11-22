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

func TestF13RefundActivationFinancialTransactionPostgres(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("PINVOU_TEST_BILLING_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	repository, _ := newAdminPaginationIntegrationRepository(t, ctx, databaseURL)
	pool := repository.pool
	setActivation := refundFixtureActivation(t, ctx, pool)
	activationVersion := setActivation("INACTIVE")

	full := seedRefundException(t, ctx, repository, false)
	command := RefundExceptionRepairCommandInput{RefundID: full.RefundID, PaymentEvidenceID: full.EvidenceID, AnomalyCaseID: full.AnomalyCaseID}
	plan, err := refundExceptionRepairPlanFrom(ctx, pool, command, 1, 1, activationVersion)
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
			ExecutionKey: "refund-repair:" + requestID.String(), PolicyVersion: 1, SchemaVersion: 1, ActivationVersion: activationVersion,
			Command: command, Frozen: frozen,
		})
	}
	frozen := RefundExceptionRepairFrozenFacts{RefundUpdatedAt: plan.RefundUpdatedAt, AnomalyVersion: plan.AnomalyVersion,
		AmountFen: plan.AmountFen, CreditMicrocredits: plan.CreditMicrocredits, ProviderRefundID: plan.ProviderRefundID,
		RevokedMicrocredits: plan.RevokedMicrocredits, ShortfallMicrocredits: plan.ShortfallMicrocredits,
		WalletID: plan.WalletID, WalletVersion: plan.WalletVersion, PackageID: plan.PackageID, FactsDigest: plan.FactsDigest}
	requestID, eventID := uuid.New(), uuid.New()

	t.Run("inactive-zero-write", func(t *testing.T) {
		if _, err := execute(requestID, eventID, command, frozen); !errors.Is(err, ErrApprovalSnapshotMismatch) {
			t.Fatal("inactive fresh execution accepted", err)
		}
		assertRefundActivationNoEffect(t, ctx, repository, full)
	})
	activationVersion = setActivation("ACTIVE")
	t.Run("operator-version-zero-write", func(t *testing.T) {
		setActivation("INACTIVE")
		if _, err := execute(requestID, eventID, command, frozen); !errors.Is(err, ErrApprovalSnapshotMismatch) {
			t.Fatal("old frozen activation accepted", err)
		}
		assertRefundActivationNoEffect(t, ctx, repository, full)
	})
	activationVersion = setActivation("ACTIVE")
	t.Run("operator-waits-for-financial-commit-and-old-receipt", func(t *testing.T) {
		// The owner digest includes activation_version. A new operator activation
		// requires a new legitimate frozen projection, not the earlier inactive one.
		currentPlan, err := refundExceptionRepairPlanFrom(ctx, pool, command, 1, 1, activationVersion)
		if err != nil {
			t.Fatal("fresh active projection failed", err)
		}
		frozen.FactsDigest = currentPlan.FactsDigest
		input := RefundExceptionRepairExecutionInput{ApprovalRequestID: requestID, AuthorizationEventID: eventID, ApprovalEpoch: 2, ExecutionKey: "race:refund-repair", PolicyVersion: 1, SchemaVersion: 1, ActivationVersion: activationVersion, Command: command, Frozen: frozen}
		result, err := refundExceptionWithDeactivationRace(t, ctx, repository, input)
		if err != nil || result.Status != "SUCCEEDED" || result.RevokedMicrocredits != 100 || result.ShortfallMicrocredits != 0 {
			t.Fatal("qualified Refund race failed", err)
		}
		var posted, cases, postings, total int64
		if err := pool.QueryRow(ctx, `SELECT (SELECT posted_microcredits FROM wallets WHERE id=$1),(SELECT count(*) FROM refund_exception_repair_cases WHERE approval_request_id=$2),(SELECT count(*) FROM ledger_postings WHERE journal_entry_id=$3),(SELECT COALESCE(sum(delta_microcredits),0) FROM ledger_postings WHERE journal_entry_id=$3)`, full.WalletID, requestID, result.JournalEntryID).Scan(&posted, &cases, &postings, &total); err != nil || posted != 0 || cases != 1 || postings != 2 || total != 0 {
			t.Fatal("Refund actual financial facts differ", err)
		}
	})
}
func assertRefundActivationNoEffect(t *testing.T, ctx context.Context, repo *PostgresRepository, f refundExceptionFixture) {
	t.Helper()
	var cases, journals, posted int64
	if err := repo.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM refund_exception_repair_cases),(SELECT count(*) FROM journal_entries WHERE business_reference=$1),(SELECT posted_microcredits FROM wallets WHERE id=$2)`, "refund-repair:"+f.RefundID.String(), f.WalletID).Scan(&cases, &journals, &posted); err != nil || cases != 0 || journals != 0 || posted != 100 {
		t.Fatal("rejected Refund wrote funds", err)
	}
}
