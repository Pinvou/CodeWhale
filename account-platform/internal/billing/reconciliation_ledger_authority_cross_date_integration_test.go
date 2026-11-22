package billing

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/google/uuid"
)

// Exercises a real old payment and a later refund in an owned public Billing
// database. Account proof and provider are controlled fixture authorities.
func TestLedgerRepairAuthorityCrossDatePostgres(t *testing.T) {
	for _, action := range []string{"RESOLVE", "IGNORE"} {
		t.Run(action, func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			clock := r.now
			old := clock().Add(-72 * time.Hour)
			r.now = func() time.Time { return old }
			payment, order, transaction, pack := retainedFinanceOrder(t, r, ctx, 1000000, true)
			r.now = clock
			payment.AllowNormalRefundCreation(true)
			refund, _, err := payment.CreateRefund(ctx, order.ID, order.RequesterID, transaction, pack, uuid.NewString(), "controlled cross-date refund")
			if err != nil {
				t.Fatal(err)
			}
			ref := "controlled-cross-date-" + uuid.NewString()
			if _, _, err = payment.finishRefund(ctx, refund, ProviderRefund{MerchantRefundNo: refund.MerchantRefundNo, AmountFen: refund.AmountFen, Status: "SUCCESS", ChannelRefundID: ref}, false); err != nil {
				t.Fatal(err)
			}
			now := clock()
			row := ProviderBillEntry{EntryType: "REFUND", ProviderReferenceID: ref, MerchantOrderNo: order.MerchantOrderNo, Channel: order.Channel, AmountFen: refund.AmountFen + 1, Currency: order.Currency, Status: "SUCCESS", PaidAt: now}
			provider := &reconciliationFixtureProvider{retainedFinancialProvider: *payment.provider.(*retainedFinancialProvider), rows: []ProviderBillEntry{row}}
			c, err := newBillingFixturePaymentCoordinator(r, provider, "test")
			if err != nil {
				t.Fatal(err)
			}
			actor := uuid.New()
			date := reconciliationFixtureDateForPaidAt(now)
			before, err := c.RunAdminReconciliation(ctx, actor, order.Channel, date)
			if err != nil || before.Status != "COMPLETED" {
				t.Fatal("refund observation", err)
			}
			cases, err := r.AdminReconciliationCases(ctx, before.ID, 10)
			if err != nil {
				t.Fatal(err)
			}
			var caseID uuid.UUID
			for _, item := range cases {
				if item.MerchantOrderNo == order.MerchantOrderNo && item.CaseType == "REFUND_AMOUNT_MISMATCH" {
					caseID = item.ID
				}
			}
			if caseID == uuid.Nil {
				t.Fatal("refund mismatch missing")
			}
			if _, err = r.UpdateReconciliationCase(ctx, caseID, ReconciliationCaseCommand{Action: "CLAIM", ActorID: actor, IdempotencyKey: uuid.NewString(), Reason: "controlled cross date claim"}); err != nil {
				t.Fatal(err)
			}
			var journalID, postingID uuid.UUID
			var delta int64
			if err = r.pool.QueryRow(ctx, `SELECT j.id,p.id,p.delta_microcredits FROM journal_entries j JOIN ledger_postings p ON p.journal_entry_id=j.id WHERE j.business_reference=$1 AND p.ledger_account_id=$2`, "payment-transaction:"+transaction.String(), paymentClearingAccount).Scan(&journalID, &postingID, &delta); err != nil {
				t.Fatal(err)
			}
			statement := fmt.Sprintf(`BEGIN; ALTER TABLE public.ledger_postings DISABLE TRIGGER ledger_postings_immutable; UPDATE public.ledger_postings SET ledger_account_id='%s' WHERE id='%s'; SET CONSTRAINTS ALL IMMEDIATE; ALTER TABLE public.ledger_postings ENABLE TRIGGER ledger_postings_immutable; COMMIT`, adjustmentAccount.String(), postingID.String())
			if _, err = retainedFinanceOwnerExec(t, r, ctx, statement); err != nil {
				t.Fatal(err)
			}
			operator := ledgerActivationFixtureOperator(t, ctx, r.pool)
			activation := setLedgerActivationFixtureState(t, ctx, operator, "ACTIVE")
			input := LedgerRepairCommandInput{TargetJournalEntryID: journalID, TargetPostingID: postingID, WrongLedgerAccountID: adjustmentAccount, CorrectLedgerAccountID: paymentClearingAccount, AmountMicrocredits: absInt64(delta)}
			plan, err := r.LedgerRepairPlanForCommand(ctx, input, 1, 1, activation)
			if err != nil {
				t.Fatal(err)
			}
			requestID, eventID := uuid.New(), uuid.New()
			key := "controlled-cross-date-" + uuid.NewString()
			critical, financial := ledgerRepairProofProjection(plan)
			normalized, _ := json.Marshal(map[string]any{"target_journal_entry_id": journalID, "target_posting_id": postingID, "wrong_ledger_account_id": adjustmentAccount, "correct_ledger_account_id": paymentClearingAccount, "amount_microcredits": absInt64(delta), "reason_code": "ATTRIBUTION_TO_WRONG_PLATFORM_ACCOUNT"})
			frozen := approvalencoding.LegacyProofInput{CommandType: ledgerRepairCommand, PolicyVersion: 1, SnapshotSchemaVersion: 1, TargetResourceType: "LEDGER_POSTING", TargetResourceID: postingID.String(), NormalizedCommand: normalized, CriticalResourceVersions: critical, FinancialSnapshot: financial}
			artifacts, err := approvalencoding.ConvertLegacyProofInput(frozen)
			if err != nil {
				t.Fatal(err)
			}
			revision := billingProofRevision(ledgerRepairCommand, activation, frozen, plan.EntryType+":"+plan.WrongLedgerAccountType+"->"+plan.CorrectLedgerAccountType)
			if _, err = r.pool.Exec(ctx, `INSERT INTO billing_approval_proof_claims(approval_request_id,authorization_event_id,command_type,command_digest,snapshot_digest,domain_revision,approval_epoch) VALUES($1,$2,'LEDGER_REPAIR',$3,$4,$5,1)`, requestID, eventID, artifacts.CommandDigest, artifacts.SnapshotDigest, revision); err != nil {
				t.Fatal(err)
			}
			execution := LedgerRepairExecutionInput{ApprovalRequestID: requestID, AuthorizationEventID: eventID, ApprovalEpoch: 1, ExecutionKey: key, PolicyVersion: 1, SchemaVersion: 1, ActivationVersion: activation, Command: input, Frozen: LedgerRepairFrozenFacts{TargetJournalEntryID: journalID, TargetPostingID: postingID, WrongLedgerAccountID: adjustmentAccount, CorrectLedgerAccountID: paymentClearingAccount, EntryType: plan.EntryType, AmountMicrocredits: plan.AmountMicrocredits, PostingSetDigest: plan.PostingSetDigest, FactsDigest: plan.FactsDigest}}
			if _, err = r.ApplyLedgerRepair(ctx, execution); err != nil {
				t.Fatal(err)
			}
			var wire struct {
				NormalizedCommand        json.RawMessage `json:"normalized_command"`
				CriticalResourceVersions json.RawMessage `json:"critical_resource_versions"`
				FinancialSnapshot        json.RawMessage `json:"financial_snapshot"`
			}
			if err = json.Unmarshal(artifacts.ProofInputJSON, &wire); err != nil {
				t.Fatal(err)
			}
			id := ledgerRepairProofIdentity{ApprovalRequestID: requestID, AuthorizationEventID: eventID, ApprovalEpoch: 1}
			authority := &controlledLedgerRepairAuthority{unavailable: true, proof: ledgerRepairAuthorizationProof{ledgerRepairProofIdentity: id, Status: "VERIFIED", CommandType: ledgerRepairCommand, PolicyVersion: 1, SnapshotSchemaVersion: 1, ExecutionKeyDigest: ledgerRepairExecutionKeyDigest(key), CommandDigest: artifacts.CommandDigest, SnapshotDigest: artifacts.SnapshotDigest, DomainRevision: revision, NormalizedCommand: wire.NormalizedCommand, CriticalResourceVersions: wire.CriticalResourceVersions, FinancialSnapshot: wire.FinancialSnapshot}}
			c.WithLedgerRepairProofAuthority(authority)
			start, end := reconciliationBusinessDateRange(date)
			facts, err := c.listFourWayReconciliationFacts(ctx, r.pool, order.Channel, start, end)
			if err != nil {
				t.Fatal(err)
			}
			for _, fact := range facts {
				if fact.EntryType == "PAYMENT" && fact.PaymentOrderID == order.ID {
					t.Fatal("old payment unexpectedly included in current day")
				}
			}
			if action == "RESOLVE" {
				provider.rows[0].AmountFen = refund.AmountFen
			}
			attempt := func() error {
				cmd := ReconciliationCaseCommand{Action: action, ActorID: actor, IdempotencyKey: uuid.NewString(), Reason: "controlled cross date review", ReviewResult: "reviewed exact facts"}
				if e := c.prepareReconciliationCaseCommand(ctx, caseID, &cmd); e != nil {
					return e
				}
				_, e := r.UpdateReconciliationCase(ctx, caseID, cmd)
				return e
			}
			if err = attempt(); !errors.Is(err, ErrConflict) {
				t.Fatal("cross-date unavailable repair did not block", action, err)
			}
			authority.unavailable = false
			// An orphan reversal for another original posting must not disappear
			// from the cross-day order proof. Remove only this synthetic row in
			// the owned UUID test database after checking fail-closed behavior.
			var otherPosting uuid.UUID
			if err = r.pool.QueryRow(ctx, `SELECT id FROM ledger_postings WHERE journal_entry_id=$1 AND id<>$2 LIMIT 1`, journalID, postingID).Scan(&otherPosting); err != nil {
				t.Fatal(err)
			}
			orphanID := uuid.New()
			if _, err = r.pool.Exec(ctx, `INSERT INTO journal_entries(id,entry_type,business_reference,description,created_at) VALUES($1,'REVERSAL',$2,'controlled orphan',now())`, orphanID, "ledger-repair:"+otherPosting.String()); err != nil {
				t.Fatal(err)
			}
			if err = attempt(); !errors.Is(err, ErrConflict) {
				t.Fatal("cross-date orphan reversal accepted", action, err)
			}
			cleanup := fmt.Sprintf(`BEGIN; ALTER TABLE public.journal_entries DISABLE TRIGGER journal_entries_immutable; DELETE FROM public.journal_entries WHERE id='%s'; SET CONSTRAINTS ALL IMMEDIATE; ALTER TABLE public.journal_entries ENABLE TRIGGER journal_entries_immutable; COMMIT`, orphanID.String())
			if _, err = retainedFinanceOwnerExec(t, r, ctx, cleanup); err != nil {
				t.Fatal("owned orphan cleanup", err)
			}
			if err = attempt(); err != nil {
				t.Fatal("restored cross-date proof could not finish", action, err)
			}
		})
	}
}
