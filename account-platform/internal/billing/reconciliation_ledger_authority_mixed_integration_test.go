package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/google/uuid"
)

type controlledLedgerRepairAuthority struct {
	proof       ledgerRepairAuthorizationProof
	unavailable bool
	calls       int
}

func (a *controlledLedgerRepairAuthority) ReadLedgerRepairProofs(_ context.Context, ids []ledgerRepairProofIdentity) ([]ledgerRepairAuthorizationProof, error) {
	a.calls++
	var out []ledgerRepairAuthorizationProof
	for _, id := range ids {
		if id != a.proof.ledgerRepairProofIdentity {
			out = append(out, ledgerRepairAuthorizationProof{ledgerRepairProofIdentity: id, Status: "UNAVAILABLE"})
			continue
		}
		p := a.proof
		if a.unavailable {
			p.Status = "UNAVAILABLE"
		}
		out = append(out, p)
	}
	return out, nil
}

// Uses real public Billing migrations/roles and a controlled Account-authority
// fixture. The full real Account-to-Billing delivery is tested separately.
func TestLedgerRepairAuthorityMixedBatchPostgres(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	first, order, transaction, _ := retainedFinanceOrder(t, r, ctx, 1000000, true)
	second, ordinary, _, _ := retainedFinanceOrder(t, r, ctx, 1000000, true)
	fact1 := first.provider.(*retainedFinancialProvider).result
	fact2 := second.provider.(*retainedFinancialProvider).result
	row := func(o PaymentOrder, provider ProviderOrder) ProviderBillEntry {
		return ProviderBillEntry{EntryType: "PAYMENT", ProviderReferenceID: *provider.ChannelTransactionID, ChannelTransactionID: *provider.ChannelTransactionID, MerchantOrderNo: o.MerchantOrderNo, Channel: o.Channel, AmountFen: o.AmountFen, Currency: o.Currency, Status: "SUCCESS", PaidAt: *provider.PaidAt}
	}
	rows := []ProviderBillEntry{row(order, fact1), row(ordinary, fact2)}
	// Keep a separate ordinary mismatch in the same batch to prove it can be
	// ignored even while the repaired order's authority is unavailable.
	rows[1].AmountFen++
	provider := &reconciliationFixtureProvider{retainedFinancialProvider: retainedFinancialProvider{result: fact1}, rows: rows}
	c, err := NewPaymentCoordinatorWithProvider(r, provider, "test")
	if err != nil {
		t.Fatal(err)
	}
	var journalID, postingID uuid.UUID
	var amount int64
	if err = r.pool.QueryRow(ctx, `SELECT j.id,p.id,p.delta_microcredits FROM journal_entries j JOIN ledger_postings p ON p.journal_entry_id=j.id WHERE j.business_reference=$1 AND p.ledger_account_id=$2`, "payment-transaction:"+transaction.String(), paymentClearingAccount).Scan(&journalID, &postingID, &amount); err != nil {
		t.Fatal(err)
	}
	statement := fmt.Sprintf(`BEGIN; ALTER TABLE public.ledger_postings DISABLE TRIGGER ledger_postings_immutable; UPDATE public.ledger_postings SET ledger_account_id='%s' WHERE id='%s'; SET CONSTRAINTS ALL IMMEDIATE; ALTER TABLE public.ledger_postings ENABLE TRIGGER ledger_postings_immutable; COMMIT`, adjustmentAccount.String(), postingID.String())
	if _, err = retainedFinanceOwnerExec(t, r, ctx, statement); err != nil {
		t.Fatal(err)
	}
	actor := uuid.New()
	date := reconciliationFixtureDateForPaidAt(*fact1.PaidAt)
	before, err := c.RunAdminReconciliation(ctx, actor, order.Channel, date)
	if err != nil || before.Status != "COMPLETED" {
		t.Fatal("original run", err)
	}
	cases, err := r.AdminReconciliationCases(ctx, before.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	var originalID, ordinaryID uuid.UUID
	for _, item := range cases {
		if item.MerchantOrderNo == order.MerchantOrderNo && item.CaseType == "PAYMENT_LEDGER_INVALID" {
			originalID = item.ID
		}
		if item.MerchantOrderNo == ordinary.MerchantOrderNo && item.CaseType == "AMOUNT_MISMATCH" {
			ordinaryID = item.ID
		}
	}
	if originalID == uuid.Nil || ordinaryID == uuid.Nil {
		t.Fatal("original and unrelated cases absent", len(cases))
	}
	operator := ledgerActivationFixtureOperator(t, ctx, r.pool)
	activation := setLedgerActivationFixtureState(t, ctx, operator, "ACTIVE")
	input := LedgerRepairCommandInput{TargetJournalEntryID: journalID, TargetPostingID: postingID, WrongLedgerAccountID: adjustmentAccount, CorrectLedgerAccountID: paymentClearingAccount, AmountMicrocredits: absInt64(amount)}
	plan, err := r.LedgerRepairPlanForCommand(ctx, input, 1, 1, activation)
	if err != nil {
		t.Fatal(err)
	}
	requestID, eventID := uuid.New(), uuid.New()
	key := "controlled-" + uuid.NewString()
	execution := LedgerRepairExecutionInput{ApprovalRequestID: requestID, AuthorizationEventID: eventID, ApprovalEpoch: 1, ExecutionKey: key, PolicyVersion: 1, SchemaVersion: 1, ActivationVersion: activation, Command: input, Frozen: LedgerRepairFrozenFacts{TargetJournalEntryID: journalID, TargetPostingID: postingID, WrongLedgerAccountID: adjustmentAccount, CorrectLedgerAccountID: paymentClearingAccount, EntryType: plan.EntryType, AmountMicrocredits: plan.AmountMicrocredits, PostingSetDigest: plan.PostingSetDigest, FactsDigest: plan.FactsDigest}}
	critical, financial := ledgerRepairProofProjection(plan)
	normalized, _ := json.Marshal(map[string]any{"target_journal_entry_id": journalID, "target_posting_id": postingID, "wrong_ledger_account_id": adjustmentAccount, "correct_ledger_account_id": paymentClearingAccount, "amount_microcredits": absInt64(amount), "reason_code": "ATTRIBUTION_TO_WRONG_PLATFORM_ACCOUNT"})
	frozen := approvalencoding.LegacyProofInput{CommandType: ledgerRepairCommand, PolicyVersion: 1, SnapshotSchemaVersion: 1, TargetResourceType: "LEDGER_POSTING", TargetResourceID: postingID.String(), NormalizedCommand: normalized, CriticalResourceVersions: critical, FinancialSnapshot: financial}
	artifacts, err := approvalencoding.ConvertLegacyProofInput(frozen)
	if err != nil {
		t.Fatal(err)
	}
	revision := billingProofRevision(ledgerRepairCommand, activation, frozen, plan.EntryType+":"+plan.WrongLedgerAccountType+"->"+plan.CorrectLedgerAccountType)
	if _, err = r.pool.Exec(ctx, `INSERT INTO billing_approval_proof_claims(approval_request_id,authorization_event_id,command_type,command_digest,snapshot_digest,domain_revision,approval_epoch) VALUES($1,$2,'LEDGER_REPAIR',$3,$4,$5,1)`, requestID, eventID, artifacts.CommandDigest, artifacts.SnapshotDigest, revision); err != nil {
		t.Fatal(err)
	}
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
	unknown, err := c.RunAdminReconciliation(ctx, actor, order.Channel, date)
	if err != nil || unknown.Status != "COMPLETED" {
		t.Fatal("unavailable proof must classify without blocking unrelated batch", err)
	}
	cases, err = r.AdminReconciliationCases(ctx, unknown.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	var unknownID uuid.UUID
	for _, item := range cases {
		if item.CaseType == ledgerRepairProofUnavailableCase && item.MerchantOrderNo == order.MerchantOrderNo {
			unknownID = item.ID
		}
	}
	if unknownID == uuid.Nil {
		t.Fatal("unavailable proof case missing")
	}
	claim := func(id uuid.UUID) {
		t.Helper()
		if _, err := r.UpdateReconciliationCase(ctx, id, ReconciliationCaseCommand{Action: "CLAIM", ActorID: actor, IdempotencyKey: uuid.NewString(), Reason: "controlled claim case"}); err != nil {
			t.Fatal(err)
		}
	}
	claim(originalID)
	claim(ordinaryID)
	claim(unknownID)
	ignore := func(id uuid.UUID) error {
		cmd := ReconciliationCaseCommand{Action: "IGNORE", ActorID: actor, IdempotencyKey: uuid.NewString(), Reason: "controlled ignore reviewed case", ReviewResult: "controlled review"}
		if e := c.prepareReconciliationCaseCommand(ctx, id, &cmd); e != nil {
			return e
		}
		_, e := r.UpdateReconciliationCase(ctx, id, cmd)
		return e
	}
	if err = ignore(originalID); !errors.Is(err, ErrConflict) {
		t.Fatal("unavailable original case ignored", err)
	}
	if err = ignore(unknownID); !errors.Is(err, ErrConflict) {
		t.Fatal("unverified case ignored", err)
	}
	if err = ignore(ordinaryID); err != nil {
		t.Fatal("unrelated ordinary case blocked", err)
	}
	resolve := func(id uuid.UUID) error {
		cmd := ReconciliationCaseCommand{Action: "RESOLVE", ActorID: actor, IdempotencyKey: uuid.NewString(), Reason: "controlled resolved repaired case", ReviewResult: "controlled review"}
		if e := c.prepareReconciliationResolution(ctx, id, &cmd); e != nil {
			return e
		}
		_, e := r.UpdateReconciliationCase(ctx, id, cmd)
		return e
	}
	if err = resolve(originalID); !errors.Is(err, ErrConflict) {
		t.Fatal("unavailable original case resolved", err)
	}
	authority.unavailable = false
	if err = resolve(originalID); err != nil {
		t.Fatal("verified original case did not resolve", err)
	}
	if err = resolve(unknownID); err != nil {
		t.Fatal("verified unavailable case did not resolve", err)
	}
}
