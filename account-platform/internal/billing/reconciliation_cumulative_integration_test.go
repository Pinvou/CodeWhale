package billing

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	"math"
	"testing"
	"time"
)

func auditCumulativeForcedCase(t *testing.T, r *PostgresRepository, ctx context.Context, order PaymentOrder, id uuid.UUID) ReconciliationInternalFact {
	t.Helper()
	var source []byte
	if err := r.pool.QueryRow(ctx, `SELECT to_jsonb(f) FROM forced_refund_cases f WHERE id=$1`, id).Scan(&source); err != nil {
		t.Fatal(err)
	}
	facts := []ReconciliationInternalFact{{EntryType: "REFUND", PaymentOrderID: order.ID, InternalSnapshot: json.RawMessage(`{}`), RefundSnapshot: source}}
	if err := auditFourWayReconciliationFacts(ctx, r.pool, facts); err != nil {
		t.Fatal(err)
	}
	return facts[0]
}

func TestF03CumulativeForcedRefundReceiptAudit(t *testing.T) {
	for _, scenario := range []string{"rounded", "zero", "max-int", "overcash", "overbase", "missing-zero-receipt", "wrong-zero-receipt", "wrong-source-wallet", "unknown-peer"} {
		t.Run(scenario, func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			fixed := r.now()
			r.now = func() time.Time { return fixed }
			credit := int64(10)
			cash := []int64{33, 33, 34}
			if scenario == "zero" || scenario == "missing-zero-receipt" || scenario == "wrong-zero-receipt" {
				credit = 1
				cash = []int64{1, 1}
			}
			if scenario == "max-int" {
				credit = math.MaxInt64
				cash = []int64{50, 50}
			}
			_, order, transaction, _ := retainedFinanceOrder(t, r, ctx, credit, true)
			var ids []uuid.UUID
			for i, amount := range cash {
				item, err := r.RecordAndFreezeForcedRefund(ctx, transaction, fmt.Sprintf("audit-cumulative-%d", i), amount, nil)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = r.ApplyReviewedForcedRefund(ctx, item.ID); err != nil {
					t.Fatal(err)
				}
				ids = append(ids, item.ID)
			}
			for _, id := range ids {
				if fact := auditCumulativeForcedCase(t, r, ctx, order, id); !fact.LedgerValid {
					t.Fatal("legitimate cumulative receipt rejected", fact.IntegrityCaseType)
				}
			}
			before := auditCumulativeForcedCase(t, r, ctx, order, ids[0]).IntegrityDigest
			var statement string
			switch scenario {
			case "rounded", "zero", "max-int":
				return
			case "overcash":
				statement = fmt.Sprintf(`ALTER TABLE forced_refund_cases DISABLE TRIGGER USER; UPDATE forced_refund_cases SET amount_fen=35 WHERE id='%s'; ALTER TABLE forced_refund_cases ENABLE TRIGGER USER`, ids[2])
			case "overbase":
				statement = fmt.Sprintf(`ALTER TABLE forced_refund_cases DISABLE TRIGGER USER; ALTER TABLE ledger_postings DISABLE TRIGGER ledger_postings_immutable; UPDATE forced_refund_cases SET revoked_base_microcredits=4 WHERE id='%s'; UPDATE ledger_postings SET delta_microcredits=CASE WHEN delta_microcredits<0 THEN -4 ELSE 4 END WHERE journal_entry_id IN(SELECT id FROM journal_entries WHERE business_reference='forced-refund:%s'); SET CONSTRAINTS ALL IMMEDIATE; ALTER TABLE ledger_postings ENABLE TRIGGER ledger_postings_immutable; ALTER TABLE forced_refund_cases ENABLE TRIGGER USER`, ids[1], ids[1])
			case "missing-zero-receipt":
				statement = fmt.Sprintf(`ALTER TABLE journal_entries DISABLE TRIGGER USER; DELETE FROM journal_entries WHERE business_reference='forced-refund:%s'; ALTER TABLE journal_entries ENABLE TRIGGER USER`, ids[1])
			case "wrong-zero-receipt":
				statement = fmt.Sprintf(`ALTER TABLE journal_entries DISABLE TRIGGER USER; UPDATE journal_entries SET entry_type='RECHARGE' WHERE business_reference='forced-refund:%s'; ALTER TABLE journal_entries ENABLE TRIGGER USER`, ids[1])
			case "wrong-source-wallet":
				wallet, _, err := r.EnsureWallet(ctx, "PERSONAL", uuid.New())
				if err != nil {
					t.Fatal(err)
				}
				statement = fmt.Sprintf(`ALTER TABLE forced_refund_cases DISABLE TRIGGER USER; UPDATE forced_refund_cases SET wallet_id='%s' WHERE id='%s'; ALTER TABLE forced_refund_cases ENABLE TRIGGER USER`, wallet.ID, ids[1])
			case "unknown-peer":
				statement = fmt.Sprintf(`UPDATE forced_refund_cases SET status='RESOLVED',resolution_code=NULL,version=version+1,resolved_at=now() WHERE id='%s'`, ids[1])
			}
			if _, err := retainedFinanceOwnerExec(t, r, ctx, "BEGIN;"+statement+";COMMIT;"); err != nil {
				t.Fatal("owned fault injection", err)
			}
			// Even an unchanged receipt must see the invalid same-source group.
			fact := auditCumulativeForcedCase(t, r, ctx, order, ids[0])
			if fact.LedgerValid || fact.IntegrityCaseType != "REFUND_LEDGER_INVALID" {
				t.Fatal("invalid cumulative group accepted", fact.IntegrityCaseType)
			}
			if fact.IntegrityDigest == before {
				t.Fatal("changed cumulative financial evidence reused incident identity")
			}
		})
	}
}

func TestF03CumulativeAuditIdentityIgnoresClockAndRepayment(t *testing.T) {
	var before, after reconciliationAuditRecord
	raw := []byte(`{"forced_mixed":false,"forced_history":[{"source":{"id":"case","version":2,"status":"REVIEW_REQUIRED","amount_fen":33,"revoked_base_microcredits":4,"revoked_bonus_microcredits":0},"journals":[{"id":"receipt","entry_type":"FORCED_REFUND","business_reference":"forced-refund:case","created_at":"old"}],"receivables":[{"id":"debt","original_microcredits":4,"outstanding_microcredits":4}]}]}`)
	if err := json.Unmarshal(raw, &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &after); err != nil {
		t.Fatal(err)
	}
	history := auditRecords(after["forced_history"])
	var source reconciliationAuditRecord
	_ = json.Unmarshal(history[0]["source"], &source)
	source["version"] = json.RawMessage(`3`)
	source["status"] = json.RawMessage(`"RESOLVED"`)
	source["resolution_code"] = json.RawMessage(`"CONFIRM_FORCED_REFUND"`)
	source["updated_at"] = json.RawMessage(`"new"`)
	history[0]["source"], _ = json.Marshal(source)
	journals := auditRecords(history[0]["journals"])
	journals[0]["created_at"] = json.RawMessage(`"new"`)
	history[0]["journals"], _ = json.Marshal(journals)
	debt := auditRecords(history[0]["receivables"])
	debt[0]["outstanding_microcredits"] = json.RawMessage(`0`)
	debt[0]["settled_microcredits"] = json.RawMessage(`4`)
	history[0]["receivables"], _ = json.Marshal(debt)
	after["forced_history"], _ = json.Marshal(history)
	if reconciliationIntegrityDigest(before) != reconciliationIntegrityDigest(after) {
		t.Fatal("clock, known review completion or repayment changed identity")
	}
}

func TestF03CumulativeAuditPendingEvidenceDoesNotChangeIncident(t *testing.T) {
	for _, kind := range []string{"PAYMENT", "FORCED"} {
		t.Run(kind, func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			_, order, transaction, _ := retainedFinanceOrder(t, r, ctx, 10, true)
			var first ForcedRefundCase
			var err error
			if kind == "PAYMENT" {
				statement := fmt.Sprintf(`BEGIN;ALTER TABLE ledger_postings DISABLE TRIGGER ledger_postings_immutable;UPDATE ledger_postings SET delta_microcredits=delta_microcredits/2 WHERE journal_entry_id IN(SELECT id FROM journal_entries WHERE business_reference='payment-transaction:%s');SET CONSTRAINTS ALL IMMEDIATE;ALTER TABLE ledger_postings ENABLE TRIGGER ledger_postings_immutable;COMMIT`, transaction)
				if _, err = retainedFinanceOwnerExec(t, r, ctx, statement); err != nil {
					t.Fatal(err)
				}
			} else {
				first, err = r.RecordAndFreezeForcedRefund(ctx, transaction, "first-applied", 50, nil)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = r.ApplyReviewedForcedRefund(ctx, first.ID); err != nil {
					t.Fatal(err)
				}
			}
			audit := func() ReconciliationInternalFact {
				if kind == "PAYMENT" {
					return reconciliationAuditPayment(t, r, ctx, order, transaction)
				}
				return auditCumulativeForcedCase(t, r, ctx, order, first.ID)
			}
			before := audit()
			pending, err := r.RecordAndFreezeForcedRefund(ctx, transaction, "pending-unapplied", 33, nil)
			if err != nil {
				t.Fatal(err)
			}
			after := audit()
			if before.LedgerValid != after.LedgerValid || before.IntegrityDigest != after.IntegrityDigest {
				t.Fatal("no-money pending evidence manufactured incident")
			}
			if kind == "FORCED" {
				// Corrupt only the extracted audit projection, not persisted money:
				// unreceipted debt must not be skipped as known pending evidence.
				var payload reconciliationAuditRecord
				if err := json.Unmarshal(after.LedgerSnapshot, &payload); err != nil {
					t.Fatal(err)
				}
				history := auditRecords(payload["forced_history"])
				for index := range history {
					var source reconciliationAuditRecord
					_ = json.Unmarshal(history[index]["source"], &source)
					if auditString(source, "id") == pending.ID.String() {
						history[index]["receivables"] = json.RawMessage(`[{"id":"unbound-debt","original_microcredits":1}]`)
					}
				}
				payload["forced_history"], _ = json.Marshal(history)
				projection := after
				auditReconciliationFact(&projection, reconciliationAuditRequest{Order: order.ID.String(), Transaction: transaction.String(), Source: first.ID.String(), References: []string{"forced-refund:" + first.ID.String()}, IsForced: true}, payload)
				if projection.LedgerValid {
					t.Fatal("unreceipted debt projection was skipped")
				}
				actor := uuid.New()
				if _, err = retainedResolveClaimedAnomaly(t, r, ctx, *pending.ManualReviewCaseID, actor, uuid.NewString(), "DISMISS", "controlled source dismissal", "NONE"); err != nil {
					t.Fatal(err)
				}
				after = audit()
				if !after.LedgerValid || before.IntegrityDigest != after.IntegrityDigest {
					t.Fatal("proven no-money dismissal manufactured incident")
				}
				unknown, err := r.RecordAndFreezeForcedRefund(ctx, transaction, "unknown-disposition", 1, nil)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = r.pool.Exec(ctx, `UPDATE forced_refund_cases SET status='RESOLVED',resolution_code=NULL,version=version+1,resolved_at=now() WHERE id=$1`, unknown.ID); err != nil {
					t.Fatal(err)
				}
				after = audit()
				if after.LedgerValid || before.IntegrityDigest == after.IntegrityDigest {
					t.Fatal("unknown disposition lost critical incident identity")
				}
			}
		})
	}
}
