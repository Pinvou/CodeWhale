package billing

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
)

// The original correction payment seed and prior history predicate are inherited
// from the adjacent F13 regression. The complete tests require that propagation;
// this candidate alone does not claim a buildable or independently approved wave.
func TestF14CorrectionRejectsChargebackRepairPackageHistoryPostgres(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	packageID, orderID, _ := f13PristineCorrectionPackage(t, ctx, r)
	unrelatedPackage, unrelatedOrder, _ := f13PristineCorrectionPackage(t, ctx, r)
	original, err := r.planEntitlementComponentCorrection(ctx, packageID, orderID)
	if err != nil || original.Amounts.Amount != 20 || original.Amounts.SourceType != "BASE" || original.Amounts.TargetType != "BONUS" {
		t.Fatal("pristine correction positive control", err)
	}
	if f13LegacyRelatedHistory(t, ctx, r, original) {
		t.Fatal("pristine package already has prior financial history")
	}
	foreign := seedChargebackShortfall(t, ctx, r, false)
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	reference := "f14-history:" + foreign.ChargebackCaseID.String()
	now := r.now()
	receivable, err := createReceivableWithinTransaction(ctx, tx, foreign.WalletID, "CHARGEBACK", reference, foreign.BaseMicrocredits, &foreign.OrderID, &foreign.TransactionID, &foreign.PackageID, nil, now)
	if err != nil {
		t.Fatal("controlled foreign-source receivable", err)
	}
	entryID := uuid.New()
	if err := insertJournal(ctx, tx, entryID, "CHARGEBACK", reference, "controlled immutable package-history fixture", now); err != nil {
		t.Fatal(err)
	}
	if err := insertPosting(ctx, tx, entryID, receivableAccount, -foreign.BaseMicrocredits, nil, now); err != nil {
		t.Fatal(err)
	}
	if err := insertPosting(ctx, tx, entryID, chargebackClearingAccount, foreign.BaseMicrocredits, nil, now); err != nil {
		t.Fatal(err)
	}
	// All PK/FK/check/immutable constraints remain enabled. The direct case
	// package FK is independent of its chargeback source and receivable FKs.
	// This legal-schema legacy-history counterexample is not an execution claim.
	if _, err := tx.Exec(ctx, `INSERT INTO chargeback_financial_repair_cases(id,approval_request_id,authorization_event_id,approval_epoch,execution_key_digest,chargeback_case_id,anomaly_case_id,wallet_id,package_id,payment_evidence_id,base_shortfall_microcredits,component_digest,facts_digest,journal_entry_id,receivable_id,state,created_at)
 VALUES($1,$2,$3,1,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,'EXECUTED',$15)`, uuid.New(), uuid.New(), uuid.New(), strings.Repeat("ab", 32), foreign.ChargebackCaseID, foreign.AnomalyCaseID, foreign.WalletID, packageID, foreign.EvidenceID, foreign.BaseMicrocredits, strings.Repeat("cd", 32), strings.Repeat("ef", 32), entryID, receivable.ID, now); err != nil {
		t.Fatal("legal independent-FK immutable case", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var independent bool
	if err := r.pool.QueryRow(ctx, `SELECT repair.package_id=$1 AND source.entitlement_package_id<>repair.package_id AND receivable.entitlement_package_id<>repair.package_id FROM chargeback_financial_repair_cases repair JOIN chargeback_cases source ON source.id=repair.chargeback_case_id JOIN receivables receivable ON receivable.id=repair.receivable_id WHERE repair.package_id=$1`, packageID).Scan(&independent); err != nil || !independent {
		t.Fatal("direct package history counterexample was not exercised", err)
	}
	if f13LegacyRelatedHistory(t, ctx, r, original) {
		t.Fatal("new direct case history also triggered an old qualifier")
	}
	before := f14CorrectionHistorySnapshot(t, ctx, r)
	if _, err := r.planEntitlementComponentCorrection(ctx, packageID, orderID); !errors.Is(err, ErrConflict) {
		t.Fatal("chargeback repair history incorrectly qualified as zero history", err)
	}
	valid, err := r.planEntitlementComponentCorrection(ctx, unrelatedPackage, unrelatedOrder)
	if err != nil || valid.Amounts.Amount != 20 {
		t.Fatal("unrelated pristine package lost eligibility", err)
	}
	if f14CorrectionHistorySnapshot(t, ctx, r) != before {
		t.Fatal("read-only qualification changed financial history")
	}
}

func f14CorrectionHistorySnapshot(t *testing.T, ctx context.Context, r *PostgresRepository) string {
	t.Helper()
	var chargebackFacts string
	if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_array((SELECT jsonb_agg(to_jsonb(c) ORDER BY id) FROM chargeback_cases c),(SELECT jsonb_agg(to_jsonb(c) ORDER BY id) FROM chargeback_financial_repair_cases c))::text`).Scan(&chargebackFacts); err != nil {
		t.Fatal(err)
	}
	return f13CorrectionFactsSnapshot(t, ctx, r) + "\n" + chargebackFacts
}

func TestF14CorrectionWithoutChargebackRepairFacilityPostgres(t *testing.T) {
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
			case 70:
				expected = "chargeback_financial_repair_cases"
			case 71:
				expected = "chargeback_financial_repair_proof_claim_allowlist"
			case 900019:
				expected = "chargeback_activation_execution_lock"
			}
			if expected != "" {
				if migration.Name != expected {
					return nil, errors.New("absent chargeback facility fixture migration identity changed")
				}
				removed++
				continue
			}
			selected = append(selected, migration)
		}
		if removed != 3 {
			return nil, errors.New("absent chargeback facility fixture requires exactly its case/proof-allowlist/owner-lock migrations")
		}
		return selected, nil
	})
	var installed bool
	if err := r.pool.QueryRow(ctx, `SELECT to_regclass('public.chargeback_financial_repair_cases') IS NOT NULL`).Scan(&installed); err != nil || installed {
		t.Fatal("old installation unexpectedly contains the chargeback repair case facility", err)
	}
	packageID, orderID, _ := f13PristineCorrectionPackage(t, ctx, r)
	valid, err := r.planEntitlementComponentCorrection(ctx, packageID, orderID)
	if err != nil || valid.Amounts.Amount != 20 {
		t.Fatal("older valid correction installation lost eligibility", err)
	}
}
