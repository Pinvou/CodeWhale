package billing

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/google/uuid"
)

// Real Billing writes and DB audit; Account is a controlled proof provider.
func TestLedgerRepairAuthorityDuplicateForcedRefundPostgres(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	payment, repairedOrder, repairedTxn, _ := retainedFinanceOrder(t, r, ctx, 1000000, true)
	_, ordinaryOrder, ordinaryTxn, _ := retainedFinanceOrder(t, r, ctx, 1000000, true)
	ref := "controlled-duplicate-" + uuid.NewString()
	first, err := r.RecordAndFreezeForcedRefund(ctx, repairedTxn, ref, 50, json.RawMessage(`{"provider":"controlled","item":"first"}`))
	if err != nil {
		t.Fatal("first actual provider evidence", err)
	}
	second, err := r.RecordAndFreezeForcedRefund(ctx, ordinaryTxn, ref, 50, json.RawMessage(`{"provider":"controlled","item":"second"}`))
	if err != nil {
		t.Fatal("second actual provider evidence with identical external ID", err)
	}
	for _, forced := range []ForcedRefundCase{first, second} {
		if forced.ManualReviewCaseID == nil {
			t.Fatal("actual forced refund did not create review case")
		}
		actor, key := uuid.New(), uuid.NewString()
		retainedClaimAnomaly(t, r, ctx, *forced.ManualReviewCaseID, actor)
		if _, complete, prepareErr := r.PreparePaymentAnomalyResolution(ctx, *forced.ManualReviewCaseID, actor, key, "CONFIRM_FORCED_REFUND", "controlled review"); prepareErr != nil || complete {
			t.Fatal("controlled confirmation intent", prepareErr, complete)
		}
		if _, err = r.ApplyReviewedForcedRefund(ctx, forced.ID); err != nil {
			t.Fatal("controlled forced refund confirmation", err)
		}
		if _, err = retainedResolveClaimedAnomaly(t, r, ctx, *forced.ManualReviewCaseID, actor, key, "CONFIRM_FORCED_REFUND", "controlled review", "APPLIED"); err != nil {
			t.Fatal("controlled confirmation receipt", err)
		}
	}
	if first.PaymentEvidenceID == second.PaymentEvidenceID || first.ID == second.ID {
		t.Fatal("distinct evidence required")
	}
	var digestCount int
	if err = r.pool.QueryRow(ctx, `SELECT count(DISTINCT payload_digest) FROM payment_evidence WHERE id=ANY($1)`, []uuid.UUID{first.PaymentEvidenceID, second.PaymentEvidenceID}).Scan(&digestCount); err != nil || digestCount != 2 {
		t.Fatal("distinct actual evidence payloads", digestCount, err)
	}
	var journal, posting uuid.UUID
	var amount int64
	if err = r.pool.QueryRow(ctx, `SELECT j.id,p.id,p.delta_microcredits FROM journal_entries j JOIN ledger_postings p ON p.journal_entry_id=j.id WHERE j.business_reference=$1 AND p.ledger_account_id=$2`, "forced-refund:"+first.ID.String(), refundClearingAccount).Scan(&journal, &posting, &amount); err != nil {
		t.Fatal(err)
	}
	statement := fmt.Sprintf(`BEGIN; ALTER TABLE public.ledger_postings DISABLE TRIGGER ledger_postings_immutable; UPDATE public.ledger_postings SET ledger_account_id='%s' WHERE id='%s'; SET CONSTRAINTS ALL IMMEDIATE; ALTER TABLE public.ledger_postings ENABLE TRIGGER ledger_postings_immutable; COMMIT`, adjustmentAccount.String(), posting.String())
	if _, err = retainedFinanceOwnerExec(t, r, ctx, statement); err != nil {
		t.Fatal(err)
	}
	snapshot := ledgerRepairZeroChangeSnapshot(t, ctx, r.pool)
	operator := ledgerActivationFixtureOperator(t, ctx, r.pool)
	activation := setLedgerActivationFixtureState(t, ctx, operator, "ACTIVE")
	input := LedgerRepairCommandInput{TargetJournalEntryID: journal, TargetPostingID: posting, WrongLedgerAccountID: adjustmentAccount, CorrectLedgerAccountID: refundClearingAccount, AmountMicrocredits: absInt64(amount)}
	plan, err := r.LedgerRepairPlanForCommand(ctx, input, 1, 1, activation)
	if err != nil {
		t.Fatal("actual repair plan", err)
	}
	requestID, eventID, key := uuid.New(), uuid.New(), "controlled-duplicate-"+uuid.NewString()
	critical, financial := ledgerRepairProofProjection(plan)
	normalized, _ := json.Marshal(map[string]any{"target_journal_entry_id": journal, "target_posting_id": posting, "wrong_ledger_account_id": adjustmentAccount, "correct_ledger_account_id": refundClearingAccount, "amount_microcredits": absInt64(amount), "reason_code": "ATTRIBUTION_TO_WRONG_PLATFORM_ACCOUNT"})
	frozen := approvalencoding.LegacyProofInput{CommandType: ledgerRepairCommand, PolicyVersion: 1, SnapshotSchemaVersion: 1, TargetResourceType: "LEDGER_POSTING", TargetResourceID: posting.String(), NormalizedCommand: normalized, CriticalResourceVersions: critical, FinancialSnapshot: financial}
	artifacts, err := approvalencoding.ConvertLegacyProofInput(frozen)
	if err != nil {
		t.Fatal(err)
	}
	revision := billingProofRevision(ledgerRepairCommand, activation, frozen, plan.EntryType+":"+plan.WrongLedgerAccountType+"->"+plan.CorrectLedgerAccountType)
	if _, err = r.pool.Exec(ctx, `INSERT INTO billing_approval_proof_claims(approval_request_id,authorization_event_id,command_type,command_digest,snapshot_digest,domain_revision,approval_epoch) VALUES($1,$2,'LEDGER_REPAIR',$3,$4,$5,1)`, requestID, eventID, artifacts.CommandDigest, artifacts.SnapshotDigest, revision); err != nil {
		t.Fatal(err)
	}
	execution := LedgerRepairExecutionInput{ApprovalRequestID: requestID, AuthorizationEventID: eventID, ApprovalEpoch: 1, ExecutionKey: key, PolicyVersion: 1, SchemaVersion: 1, ActivationVersion: activation, Command: input, Frozen: LedgerRepairFrozenFacts{TargetJournalEntryID: journal, TargetPostingID: posting, WrongLedgerAccountID: adjustmentAccount, CorrectLedgerAccountID: refundClearingAccount, EntryType: plan.EntryType, AmountMicrocredits: plan.AmountMicrocredits, PostingSetDigest: plan.PostingSetDigest, FactsDigest: plan.FactsDigest}}
	result, err := r.ApplyLedgerRepair(ctx, execution)
	if err != nil {
		t.Fatal("actual ledger correction", err)
	}
	if replay, replayErr := r.ApplyLedgerRepair(ctx, execution); replayErr != nil || replay != result {
		t.Fatal("repair replay", replayErr)
	}
	ledgerRepairAssertZeroChange(t, ctx, r.pool, snapshot)
	var wire struct {
		NormalizedCommand        json.RawMessage `json:"normalized_command"`
		CriticalResourceVersions json.RawMessage `json:"critical_resource_versions"`
		FinancialSnapshot        json.RawMessage `json:"financial_snapshot"`
	}
	if err = json.Unmarshal(artifacts.ProofInputJSON, &wire); err != nil {
		t.Fatal(err)
	}
	id := ledgerRepairProofIdentity{ApprovalRequestID: requestID, AuthorizationEventID: eventID, ApprovalEpoch: 1}
	proof := ledgerRepairAuthorizationProof{ledgerRepairProofIdentity: id, Status: "VERIFIED", CommandType: ledgerRepairCommand, PolicyVersion: 1, SnapshotSchemaVersion: 1, ExecutionKeyDigest: ledgerRepairExecutionKeyDigest(key), CommandDigest: artifacts.CommandDigest, SnapshotDigest: artifacts.SnapshotDigest, DomainRevision: revision, NormalizedCommand: wire.NormalizedCommand, CriticalResourceVersions: wire.CriticalResourceVersions, FinancialSnapshot: wire.FinancialSnapshot}
	date := reconciliationFixtureDateForPaidAt(r.now())
	start, end := reconciliationBusinessDateRange(date)
	facts, err := payment.listFourWayReconciliationFacts(ctx, r.pool, repairedOrder.Channel, start, end)
	if err != nil {
		t.Fatal(err)
	}
	selected := make([]ReconciliationInternalFact, 0, 2)
	for _, fact := range facts {
		if fact.EntryType == "REFUND" && fact.ProviderReferenceID == ref && (fact.PaymentOrderID == repairedOrder.ID || fact.PaymentOrderID == ordinaryOrder.ID) {
			selected = append(selected, fact)
		}
	}
	if len(selected) != 2 {
		var total, confirmed, inRange, matchingChannel, matchingProvider int
		caps := payment.Capabilities()
		if diagErr := r.pool.QueryRow(ctx, `SELECT count(*),
		 count(*) FILTER (WHERE f.resolution_code='CONFIRM_FORCED_REFUND'),
		 count(*) FILTER (WHERE f.resolved_at >= $2 AND f.resolved_at < $3),
		 count(*) FILTER (WHERE p.channel=$4),
		 count(*) FILTER (WHERE p.provider_id=$5 AND p.provider_version=$6)
		 FROM forced_refund_cases f JOIN payment_orders p ON p.id=f.payment_order_id WHERE f.id=ANY($1)`,
			[]uuid.UUID{first.ID, second.ID}, start, end, repairedOrder.Channel, caps.ProviderID, caps.Version).Scan(&total, &confirmed, &inRange, &matchingChannel, &matchingProvider); diagErr != nil {
			t.Fatal("read controlled predicate diagnostics", diagErr)
		}
		t.Fatalf("same reference query selected %d; controlled rows=%d confirmed=%d in-range=%d channel=%d provider-version=%d", len(selected), total, confirmed, inRange, matchingChannel, matchingProvider)
	}
	if err = auditFourWayReconciliationFacts(ctx, r.pool, selected); err != nil {
		t.Fatal("actual financial audit", err)
	}
	if selected[0].PaymentOrderID != repairedOrder.ID {
		selected[0], selected[1] = selected[1], selected[0]
	}
	if selected[0].PaymentOrderID != repairedOrder.ID || selected[1].PaymentOrderID != ordinaryOrder.ID {
		t.Fatal("order identity")
	}
	untouched := selected[1]
	provider := []ProviderBillEntry{{EntryType: "REFUND", ProviderReferenceID: ref, MerchantOrderNo: repairedOrder.MerchantOrderNo, Channel: repairedOrder.Channel, AmountFen: 50, Currency: repairedOrder.Currency, Status: "SUCCESS"}}
	for _, reverse := range []bool{false, true} {
		for _, available := range []bool{false, true} {
			t.Run(fmt.Sprintf("reverse=%v/available=%v", reverse, available), func(t *testing.T) {
				current := append([]ReconciliationInternalFact(nil), selected...)
				proofs := ledgerRepairProofSet{}
				if available {
					proofs[id] = proof
				}
				applyLedgerRepairAuthority(current, proofs)
				if !reflect.DeepEqual(current[1], untouched) {
					t.Fatal("ordinary order fact changed")
				}
				if available && current[0].ledgerRepairAuthorityStatus != "VERIFIED" {
					t.Fatal("actual repair proof rejected", current[0].ledgerRepairAuthorityStatus)
				}
				if !available && !current[0].ledgerRepairProofUnavailable {
					t.Fatal("actual repair proof outage not detected")
				}
				if reverse {
					current[0], current[1] = current[1], current[0]
				}
				seen := map[string]map[string]int{}
				for _, d := range CompareFourWayReconciliation(provider, current) {
					if seen[d.MerchantOrderNo] == nil {
						seen[d.MerchantOrderNo] = map[string]int{}
					}
					seen[d.MerchantOrderNo][d.CaseType]++
				}
				wantUnavailable := 0
				if !available {
					wantUnavailable = 1
				}
				if seen[repairedOrder.MerchantOrderNo][ledgerRepairProofUnavailableCase] != wantUnavailable || seen[ordinaryOrder.MerchantOrderNo][ledgerRepairProofUnavailableCase] != 0 {
					t.Fatal("wrong proof outage classification", seen)
				}
				duplicate := ordinaryOrder.MerchantOrderNo
				if reverse {
					duplicate = repairedOrder.MerchantOrderNo
				}
				if seen[duplicate]["DUPLICATE_TRANSACTION"] != 1 {
					t.Fatal("duplicate not reported", seen)
				}
			})
		}
	}
	ledgerRepairAssertZeroChange(t, ctx, r.pool, snapshot)
}
