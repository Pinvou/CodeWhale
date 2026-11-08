package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Called by the existing public FrozenExecution root; this adds no dormant root
// or new fixture. Provider values are controlled, while all persistence is real.
func testReconciliationRepairRecurrence(t *testing.T, ctx context.Context, r *PostgresRepository, activation int64) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	day := now.In(time.FixedZone("Asia/Shanghai", 8*60*60))
	row := ProviderBillEntry{EntryType: "PAYMENT", ProviderReferenceID: uuid.NewString(),
		MerchantOrderNo: "repair-recurrence-" + uuid.NewString(), ChannelTransactionID: uuid.NewString(),
		Channel: "ALIPAY", AmountFen: 1500, Currency: "CNY", Status: "SUCCESS", PaidAt: now}
	provider := &reconciliationFixtureProvider{rows: []ProviderBillEntry{row}}
	c, err := newBillingFixturePaymentCoordinator(r, provider, "test")
	if err != nil {
		t.Fatal(err)
	}
	actor := uuid.New()
	first, err := c.RunAdminReconciliation(ctx, actor, row.Channel, day)
	if err != nil {
		t.Fatal("first real API observation", err)
	}
	cases, err := r.AdminReconciliationCases(ctx, first.ID, 10)
	if err != nil || len(cases) != 1 || cases[0].CaseType != "PROVIDER_PAID_INTERNAL_MISSING" || cases[0].Status != "OPEN" {
		t.Fatalf("expected one original provider-only case, got %+v: %v", cases, err)
	}
	caseID := cases[0].ID
	refund := row
	refund.EntryType, refund.ProviderReferenceID = "REFUND", "disposed-"+uuid.NewString()
	// Persist the already-obtained provider disposal through the real BILL writer.
	// The subsequent recurrence is a complete API run, not a direct helper call.
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err := c.mergeProviderBillEvidenceWithinTransaction(ctx, tx, refund, nil, now); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var evidenceID uuid.UUID
	if err := r.pool.QueryRow(ctx, `SELECT id FROM payment_evidence WHERE provider_id='controlled' AND provider_version='v1' AND evidence_type='BILL' AND merchant_order_no=$1 AND provider_refund_id=$2`, row.MerchantOrderNo, billReference(refund)).Scan(&evidenceID); err != nil {
		t.Fatal(err)
	}
	command := ReconciliationFinancialRepairCommandInput{ReconciliationCaseID: caseID, PaymentEvidenceID: evidenceID, BusinessDate: first.BusinessDate}
	plan, err := reconciliationFinancialRepairPlanFrom(ctx, r.pool, command, 1, 1, activation)
	if err != nil {
		t.Fatal("real observed case plan", err)
	}
	input := reconciliationFrozenInput(plan, activation)
	financialBefore := reconciliationEffectSnapshot(t, ctx, r, false)
	result, err := r.ApplyReconciliationFinancialRepair(ctx, input)
	if err != nil || result.Status != "SUCCEEDED" {
		t.Fatal("repair actual observed case", result, err)
	}
	snapshot := func() string {
		var raw string
		if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_array(
 (SELECT to_jsonb(c) FROM reconciliation_cases c WHERE c.id=$1),
 (SELECT to_jsonb(m) FROM manual_review_cases m WHERE m.id=(SELECT manual_review_case_id FROM reconciliation_cases WHERE id=$1)),
 (SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM reconciliation_repair_cases x WHERE reconciliation_case_id=$1),
 (SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM manual_review_case_events x WHERE case_id=(SELECT manual_review_case_id FROM reconciliation_cases WHERE id=$1)))::text`, caseID).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		return raw
	}
	closed := snapshot()
	var status, manual string
	if err := r.pool.QueryRow(ctx, `SELECT c.status,m.status FROM reconciliation_cases c JOIN manual_review_cases m ON m.id=c.manual_review_case_id WHERE c.id=$1`, caseID).Scan(&status, &manual); err != nil || status != "RESOLVED" || manual != "RESOLVED" {
		t.Fatal("repair did not close both cases", status, manual, err)
	}
	second, err := c.RunAdminReconciliation(ctx, actor, row.Channel, day)
	if err != nil || second.ID == first.ID {
		t.Fatal("expected genuine new API observation", second, err)
	}
	if snapshot() != closed || reconciliationEffectSnapshot(t, ctx, r, false) != financialBefore {
		t.Fatal("same-digest new run reopened repaired case/manual review or changed immutable/financial facts")
	}
	var links, progress int
	if err := r.pool.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM reconciliation_case_runs WHERE run_id=$1 AND case_id=$2),
 (SELECT count(*) FROM reconciliation_observation_progress p JOIN reconciliation_observations o ON o.id=p.observation_id WHERE o.run_id=$1 AND p.case_id=$2)`, second.ID, caseID).Scan(&links, &progress); err != nil || links != 1 || progress != 1 {
		t.Fatal("suppressed reopening lost new run/progress association", links, progress, err)
	}
	replay, err := r.ApplyReconciliationFinancialRepair(ctx, input)
	if err != nil || replay != result || snapshot() != closed {
		t.Fatal("new observation broke same-key result-first replay", replay, err)
	}
	changed := row
	changed.AmountFen++
	provider.rows = []ProviderBillEntry{changed}
	third, err := c.RunAdminReconciliation(ctx, actor, row.Channel, day)
	if err != nil {
		t.Fatal(err)
	}
	newCases, err := r.AdminReconciliationCases(ctx, third.ID, 10)
	if err != nil || len(newCases) != 1 || newCases[0].ID == caseID || newCases[0].Status != "OPEN" || snapshot() != closed {
		t.Fatal("changed source fact failed to create a new open case while preserving old closure", newCases, err)
	}
}

func testReconciliationLegacyBillEvidence(t *testing.T, ctx context.Context, r *PostgresRepository, activation int64) {
	t.Helper()
	for _, fallback := range []bool{false, true} {
		t.Run(fmt.Sprintf("valid-null-column-fallback-%t", fallback), func(t *testing.T) {
			f := seedProviderPaidInternalMissing(t, ctx, r, "legacy-null")
			row := ProviderBillEntry{EntryType: "REFUND", ProviderReferenceID: "legacy-refund-" + uuid.NewString(),
				MerchantOrderNo: f.MerchantOrderNo, ChannelTransactionID: f.ChannelTransactionID, Channel: "ALIPAY",
				AmountFen: f.AmountFen, Currency: f.Currency, Status: "SUCCESS", PaidAt: time.Now().UTC().Truncate(time.Second)}
			if fallback {
				row.ProviderReferenceID = ""
			}
			raw, err := json.Marshal(row)
			if err != nil {
				t.Fatal(err)
			}
			key := fmt.Sprintf("REFUND|%s|%s|%s", billReference(row), f.MerchantOrderNo, row.PaidAt.Format(time.RFC3339Nano))
			id := uuid.New()
			if _, err := r.pool.Exec(ctx, `INSERT INTO payment_evidence(id,evidence_type,provider_id,provider_version,evidence_key,merchant_order_no,provider_transaction_id,provider_refund_id,amount_fen,currency,provider_status,provider_event_at,received_at,payload_digest,payload_snapshot,merge_result)
VALUES($1,'BILL','test-provider','v1',$2,$3,$4,NULL,$5,$6,'SUCCESS',$7,$7,$8,$9,'REVIEW_REQUIRED')`, id, key, f.MerchantOrderNo, f.ChannelTransactionID, f.AmountFen, f.Currency, row.PaidAt, digestHex(raw), raw); err != nil {
				t.Fatal(err)
			}
			snapshot := func() string {
				var text string
				if err := r.pool.QueryRow(ctx, `SELECT to_jsonb(e)::text FROM payment_evidence e WHERE id=$1`, id).Scan(&text); err != nil {
					t.Fatal(err)
				}
				return text
			}
			original := snapshot()
			// Duplicate import must remain DO NOTHING; acceptance is read-only.
			provider := &paymentRepairStubProvider{}
			c := &PaymentCoordinator{repository: r, provider: provider, now: time.Now}
			tx, err := r.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if err := c.mergeProviderBillEvidenceWithinTransaction(ctx, tx, row, nil, row.PaidAt); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if snapshot() != original {
				t.Fatal("duplicate import rewrote legacy evidence")
			}
			command := ReconciliationFinancialRepairCommandInput{ReconciliationCaseID: f.CaseID, PaymentEvidenceID: id, BusinessDate: f.BusinessDate}
			plan, err := reconciliationFinancialRepairPlanFrom(ctx, r.pool, command, 1, 1, activation)
			if err != nil {
				t.Fatal("legacy typed BILL alternative rejected", err)
			}
			financial := reconciliationEffectSnapshot(t, ctx, r, false)
			input := reconciliationFrozenInput(plan, activation)
			result, err := r.ApplyReconciliationFinancialRepair(ctx, input)
			if err != nil || result.Status != "SUCCEEDED" || snapshot() != original || reconciliationEffectSnapshot(t, ctx, r, false) != financial {
				t.Fatal("legacy closure failed or rewrote evidence/financial facts", result, err)
			}
			replayed, err := r.ApplyReconciliationFinancialRepair(ctx, input)
			if err != nil || replayed != result || snapshot() != original {
				t.Fatal("legacy evidence result-first replay changed", replayed, err)
			}
		})
	}
	for _, change := range []string{"merchant", "transaction", "provider", "version", "amount", "currency", "status", "payload", "nonnull-reference", "nonnull-padded-reference"} {
		t.Run("reject-"+change, func(t *testing.T) {
			f := seedProviderPaidInternalMissing(t, ctx, r, "legacy-negative")
			id := uuid.New()
			if _, err := r.pool.Exec(ctx, `INSERT INTO payment_evidence(id,evidence_type,provider_id,provider_version,evidence_key,merchant_order_no,provider_transaction_id,provider_refund_id,amount_fen,currency,provider_status,provider_event_at,received_at,payload_digest,payload_snapshot,merge_result)
SELECT $1,evidence_type,CASE WHEN $2='provider' THEN 'other-provider' ELSE provider_id END,
CASE WHEN $2='version' THEN 'other-version' ELSE provider_version END,evidence_key||$1::uuid::text,
CASE WHEN $2='merchant' THEN merchant_order_no||'-other' ELSE merchant_order_no END,
CASE WHEN $2='transaction' THEN provider_transaction_id||'-other' ELSE provider_transaction_id END,
CASE WHEN $2='nonnull-reference' THEN provider_refund_id||'-other' WHEN $2='nonnull-padded-reference' THEN ' '||provider_refund_id ELSE NULL END,
CASE WHEN $2='amount' THEN amount_fen+1 ELSE amount_fen END,CASE WHEN $2='currency' THEN 'USD' ELSE currency END,
CASE WHEN $2='status' THEN 'FAILED' ELSE provider_status END,provider_event_at,received_at,payload_digest,
CASE WHEN $2='payload' THEN '{}'::jsonb ELSE payload_snapshot END,merge_result FROM payment_evidence WHERE id=$3`, id, change, f.EvidenceID); err != nil {
				t.Fatal(err)
			}
			before := reconciliationEffectSnapshot(t, ctx, r, true)
			_, err := reconciliationFinancialRepairPlanFrom(ctx, r.pool, ReconciliationFinancialRepairCommandInput{ReconciliationCaseID: f.CaseID, PaymentEvidenceID: id, BusinessDate: f.BusinessDate}, 1, 1, activation)
			if !errors.Is(err, ErrApprovalSnapshotMismatch) || reconciliationEffectSnapshot(t, ctx, r, true) != before {
				t.Fatal("invalid legacy evidence accepted or changed facts", change, err)
			}
		})
	}
}
