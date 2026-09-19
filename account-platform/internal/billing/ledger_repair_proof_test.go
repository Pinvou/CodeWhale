package billing

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/google/uuid"
)

// TestLedgerRepairProofProjectionIsClosedAndActivationGated covers the GSA-24
// proof-authority slice: the closed projection is recomputed by the owner
// domain, a rejected repair is a closed DENY reason, and no proof can be
// issued while the command's activation row is INACTIVE.
func TestLedgerRepairProofProjectionIsClosedAndActivationGated(t *testing.T) {
	journalID, postingID := uuid.New(), uuid.New()
	targetPostingID := postingID
	command := LedgerRepairCommandInput{
		TargetJournalEntryID: journalID, TargetPostingID: targetPostingID,
		WrongLedgerAccountID: adjustmentAccount, CorrectLedgerAccountID: usageRevenueAccount, AmountMicrocredits: 30,
	}
	frozen := approvalencoding.LegacyProofInput{
		CommandType: ledgerRepairCommand, PolicyVersion: 1, SnapshotSchemaVersion: 1,
		TargetResourceID: journalID.String(),
		NormalizedCommand: marshalRaw(map[string]any{
			"command_type": ledgerRepairCommand, "policy_version": 1, "schema_version": 1,
			"target_journal_entry_id": journalID.String(), "target_posting_id": postingID.String(),
			"wrong_ledger_account_id": adjustmentAccount.String(), "correct_ledger_account_id": usageRevenueAccount.String(),
			"amount_microcredits": 30, "reason_code": "ATTRIBUTION_TO_WRONG_PLATFORM_ACCOUNT",
		}),
	}
	plan := LedgerRepairPlan{
		TargetJournalEntryID: journalID, TargetPostingID: postingID, EntryType: "USAGE", AmountMicrocredits: 30,
		WrongLedgerAccountID: adjustmentAccount, WrongLedgerAccountType: "ADJUSTMENT",
		CorrectLedgerAccountID: usageRevenueAccount, CorrectLedgerAccountType: "USAGE_REVENUE",
		PostingSetDigest: "aa", FactsDigest: "bb",
	}
	repository := &billingProofFactsStub{ledgerRepair: plan}
	current, reason, ownerState, err := currentBillingProofInput(context.Background(), repository, frozen)
	if err != nil || reason != "" {
		t.Fatalf("ledger repair proof reason=%s err=%v", reason, err)
	}
	if ownerState != "USAGE:ADJUSTMENT->USAGE_REVENUE" {
		t.Fatalf("unexpected owner state %q", ownerState)
	}
	var critical, financial map[string]any
	if err := json.Unmarshal(current.CriticalResourceVersions, &critical); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(current.FinancialSnapshot, &financial); err != nil {
		t.Fatal(err)
	}
	if critical["target_posting_id"] != postingID.String() || critical["posting_set_digest"] != "aa" || critical["entry_type"] != "USAGE" {
		t.Fatalf("unexpected critical projection %+v", critical)
	}
	if financial["amount_microcredits"] != float64(30) || financial["wallet_id"] != nil ||
		financial["wrong_account_type"] != "ADJUSTMENT" || financial["correct_account_type"] != "USAGE_REVENUE" {
		t.Fatalf("unexpected financial projection %+v", financial)
	}
	if len(financial) != 6 {
		t.Fatalf("financial projection is not closed: %+v", financial)
	}

	rejected := &billingProofFactsStub{ledgerRepairErr: ErrApprovalSnapshotMismatch}
	if _, reason, _, err := currentBillingProofInput(context.Background(), rejected, frozen); err != nil || reason != "DOMAIN_INVARIANT_FAILED" {
		t.Fatalf("rejected repair reason=%s err=%v", reason, err)
	}
	mismatched := &billingProofFactsStub{ledgerRepair: plan}
	mismatched.ledgerRepair.AmountMicrocredits = 31
	if _, reason, _, err := currentBillingProofInput(context.Background(), mismatched, frozen); err != nil || reason != "SNAPSHOT_MISMATCH" {
		t.Fatalf("mismatched repair reason=%s err=%v", reason, err)
	}
	if _, err := (&billingProofFactsStub{}).ledgerRepairPlan(context.Background(), command); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	inactive := validBillingApprovalActivationSnapshot(now)
	reader := &billingApprovalProofStub{billingProofFactsStub: repository, activation: inactive}
	service := NewService(reader)
	if _, err := service.billingApprovalProof(context.Background(), approvalencoding.ProofRequest{
		ApprovalRequestID: uuid.New(),
		Artifacts:         approvalencoding.ProofArtifacts{CommandType: ledgerRepairCommand, PolicyVersion: 1, SnapshotSchemaVersion: 1},
	}); !errors.Is(err, errBillingApprovalProofUnavailable) {
		t.Fatalf("inactive ledger repair proof err=%v", err)
	}
}

func marshalRaw(value any) json.RawMessage {
	encoded, _ := json.Marshal(value)
	return encoded
}
// The consumed authorization freezes only the wire-visible owner projection, so
// an empty facts digest means "the execution transaction recomputes it" while a
// presented digest must still match the owner revision exactly.
func TestLedgerRepairFrozenFactsDigestIsOptionalOnlyWhenAbsent(t *testing.T) {
	plan := LedgerRepairPlan{
		TargetJournalEntryID: uuid.New(), TargetPostingID: uuid.New(),
		WrongLedgerAccountID: uuid.New(), CorrectLedgerAccountID: uuid.New(),
		EntryType: "USAGE", AmountMicrocredits: 30, PostingSetDigest: strings.Repeat("a", 64),
		FactsDigest: strings.Repeat("b", 64),
	}
	frozen := LedgerRepairFrozenFacts{
		TargetJournalEntryID: plan.TargetJournalEntryID, TargetPostingID: plan.TargetPostingID,
		WrongLedgerAccountID: plan.WrongLedgerAccountID, CorrectLedgerAccountID: plan.CorrectLedgerAccountID,
		EntryType: plan.EntryType, AmountMicrocredits: plan.AmountMicrocredits, PostingSetDigest: plan.PostingSetDigest,
	}
	if mismatch := ledgerRepairFrozenMismatch(plan, frozen); mismatch != "" {
		t.Fatalf("owner-recomputed digest rejected: %s", mismatch)
	}
	frozen.FactsDigest = strings.Repeat("c", 64)
	if mismatch := ledgerRepairFrozenMismatch(plan, frozen); mismatch != "facts_digest" {
		t.Fatalf("presented digest mismatch=%q", mismatch)
	}
	frozen.FactsDigest = plan.FactsDigest
	if mismatch := ledgerRepairFrozenMismatch(plan, frozen); mismatch != "" {
		t.Fatalf("matching digest rejected: %s", mismatch)
	}
	frozen.PostingSetDigest = strings.Repeat("d", 64)
	if mismatch := ledgerRepairFrozenMismatch(plan, frozen); mismatch != "posting_set_digest" {
		t.Fatalf("posting set digest mismatch=%q", mismatch)
	}
}
