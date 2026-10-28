package billing

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/google/uuid"
)

func decodeLedgerRepairAuthorityProof(proof ledgerRepairAuthorizationProof, target string) (approvalencoding.ProofArtifacts, approvalencoding.LegacyProofInput, error) {
	raw, err := json.Marshal(map[string]any{
		"snapshot_schema_version":    1,
		"target":                     map[string]any{"resource_type": "LEDGER_POSTING", "resource_id": target},
		"normalized_command":         proof.NormalizedCommand,
		"critical_resource_versions": proof.CriticalResourceVersions,
		"financial_snapshot":         proof.FinancialSnapshot,
	})
	if err != nil {
		return approvalencoding.ProofArtifacts{}, approvalencoding.LegacyProofInput{}, err
	}
	artifacts, err := approvalencoding.DecodeProofInput(ledgerRepairCommand, 1, raw)
	if err != nil {
		return approvalencoding.ProofArtifacts{}, approvalencoding.LegacyProofInput{}, err
	}
	frozen, err := approvalencoding.LegacyProofInputFromArtifacts(artifacts)
	if err != nil {
		return approvalencoding.ProofArtifacts{}, approvalencoding.LegacyProofInput{}, err
	}
	if artifacts.CommandDigest != proof.CommandDigest || artifacts.SnapshotDigest != proof.SnapshotDigest {
		return approvalencoding.ProofArtifacts{}, approvalencoding.LegacyProofInput{}, errors.New("ledger repair proof digest mismatch")
	}
	return artifacts, frozen, nil
}

// validateLedgerRepairAuthority binds the Account-authoritative frozen command
// to Billing's immutable claim and the original posting. It performs no I/O;
// callers must fetch the proofs before opening their financial transaction.
// A missing proof is inconclusive, not a fabricated ledger discrepancy.
func validateLedgerRepairAuthority(payload reconciliationAuditRecord, proofs map[ledgerRepairProofIdentity]ledgerRepairAuthorizationProof) (bool, error) {
	var evidence reconciliationAuditRecord
	if len(payload["ledger_repairs"]) == 0 || string(payload["ledger_repairs"]) == "null" {
		return true, nil
	}
	if json.Unmarshal(payload["ledger_repairs"], &evidence) != nil {
		return false, nil
	}
	cases := auditRecords(evidence["cases"])
	if len(cases) == 0 {
		return true, nil // the independent structural verifier rejects orphans
	}
	if _, ok := reconciliationLedgerRepairPostings(payload); !ok {
		return false, nil
	}
	journal := auditRecords(payload["journals"])[0]
	journalID, _ := uuid.Parse(auditString(journal, "id"))
	posts := auditRecords(payload["postings"])
	history := auditRecords(evidence["activation_history"])
	if len(history) == 0 {
		return false, ErrLedgerRepairAuthorityUnavailable
	}
	for _, c := range cases {
		requestID, err1 := uuid.Parse(auditString(c, "approval_request_id"))
		eventID, err2 := uuid.Parse(auditString(c, "authorization_event_id"))
		epoch := auditInteger(c, "approval_epoch")
		if err1 != nil || err2 != nil || requestID == uuid.Nil || eventID == uuid.Nil || epoch == nil || !epoch.IsInt64() || epoch.Sign() <= 0 {
			return false, nil
		}
		identity := ledgerRepairProofIdentity{ApprovalRequestID: requestID, AuthorizationEventID: eventID, ApprovalEpoch: epoch.Int64()}
		proof, ok := proofs[identity]
		if !ok || proof.Status == "UNAVAILABLE" {
			return false, ErrLedgerRepairAuthorityUnavailable
		}
		if proof.Status != "VERIFIED" || proof.ledgerRepairProofIdentity != identity || proof.CommandType != ledgerRepairCommand || proof.PolicyVersion != 1 || proof.SnapshotSchemaVersion != 1 || !reconciliationRepairDigest(proof.ExecutionKeyDigest) || !reconciliationRepairDigest(proof.CommandDigest) || !reconciliationRepairDigest(proof.SnapshotDigest) {
			return false, ErrLedgerRepairAuthorityUnavailable
		}
		var claim, wrong, correct reconciliationAuditRecord
		if json.Unmarshal(c["approval_claim"], &claim) != nil || claim == nil {
			return false, ErrLedgerRepairAuthorityUnavailable
		}
		if json.Unmarshal(c["wrong_account"], &wrong) != nil || json.Unmarshal(c["correct_account"], &correct) != nil {
			return false, nil
		}
		// Account returns the proof wire form, in which integer values are
		// decimal strings. Decode with the shared closed-contract implementation
		// before comparing against Billing's legacy numeric projection.
		_, frozen, err := decodeLedgerRepairAuthorityProof(proof, auditString(c, "target_posting_id"))
		if err != nil {
			return false, ErrLedgerRepairAuthorityUnavailable
		}
		if proof.ExecutionKeyDigest != auditString(c, "execution_key_digest") || auditString(claim, "approval_request_id") != requestID.String() || auditString(claim, "authorization_event_id") != eventID.String() || auditString(claim, "command_type") != ledgerRepairCommand || auditInteger(claim, "approval_epoch") == nil || auditInteger(claim, "approval_epoch").Cmp(epoch) != 0 || auditString(claim, "command_digest") != proof.CommandDigest || auditString(claim, "snapshot_digest") != proof.SnapshotDigest || auditString(claim, "domain_revision") != proof.DomainRevision {
			return false, nil
		}
		var command struct {
			TargetJournalEntryID   uuid.UUID `json:"target_journal_entry_id"`
			TargetPostingID        uuid.UUID `json:"target_posting_id"`
			WrongLedgerAccountID   uuid.UUID `json:"wrong_ledger_account_id"`
			CorrectLedgerAccountID uuid.UUID `json:"correct_ledger_account_id"`
			AmountMicrocredits     int64     `json:"amount_microcredits"`
			ReasonCode             string    `json:"reason_code"`
		}
		if json.Unmarshal(frozen.NormalizedCommand, &command) != nil || command.TargetJournalEntryID.String() != auditString(c, "target_journal_entry_id") || command.TargetPostingID.String() != auditString(c, "target_posting_id") || command.WrongLedgerAccountID.String() != auditString(c, "wrong_ledger_account_id") || command.CorrectLedgerAccountID.String() != auditString(c, "correct_ledger_account_id") || command.ReasonCode != "ATTRIBUTION_TO_WRONG_PLATFORM_ACCOUNT" || command.AmountMicrocredits < 1 || auditInteger(c, "amount_microcredits") == nil || !auditInteger(c, "amount_microcredits").IsInt64() || auditInteger(c, "amount_microcredits").Int64() != command.AmountMicrocredits {
			return false, nil
		}
		plan := LedgerRepairPlan{TargetJournalEntryID: command.TargetJournalEntryID, TargetPostingID: command.TargetPostingID, WrongLedgerAccountID: command.WrongLedgerAccountID, CorrectLedgerAccountID: command.CorrectLedgerAccountID, AmountMicrocredits: command.AmountMicrocredits, EntryType: auditString(c, "entry_type"), WrongLedgerAccountType: auditString(wrong, "account_type"), CorrectLedgerAccountType: auditString(correct, "account_type"), PostingSetDigest: auditString(c, "posting_set_digest")}
		critical, financial := ledgerRepairProofProjection(plan)
		if !sameRawJSON(critical, frozen.CriticalResourceVersions) || !sameRawJSON(financial, frozen.FinancialSnapshot) {
			return false, nil
		}
		input := approvalencoding.LegacyProofInput{CommandType: ledgerRepairCommand, PolicyVersion: 1, SnapshotSchemaVersion: 1, TargetResourceType: "LEDGER_POSTING", TargetResourceID: command.TargetPostingID.String(), NormalizedCommand: frozen.NormalizedCommand, CriticalResourceVersions: critical, FinancialSnapshot: financial}
		claimVersionText, _, ok := strings.Cut(proof.DomainRevision, ":")
		claimVersion, err := strconv.ParseInt(claimVersionText, 10, 64)
		if !ok || err != nil || claimVersion < 1 || strconv.FormatInt(claimVersion, 10) != claimVersionText || billingProofRevision(ledgerRepairCommand, claimVersion, input, plan.EntryType+":"+plan.WrongLedgerAccountType+"->"+plan.CorrectLedgerAccountType) != proof.DomainRevision {
			return false, nil
		}
		var original reconciliationAuditRecord
		for _, post := range posts {
			if auditString(post, "id") == command.TargetPostingID.String() {
				original = post
				break
			}
		}
		delta := auditInteger(original, "delta_microcredits")
		if original == nil || delta == nil || !delta.IsInt64() || journalID != command.TargetJournalEntryID {
			return false, nil
		}
		target := ledgerRepairTarget{JournalEntryID: journalID, EntryType: plan.EntryType, BusinessReference: auditString(journal, "business_reference")}
		posting := ledgerRepairPosting{ID: command.TargetPostingID, LedgerAccountID: command.WrongLedgerAccountID, AccountType: plan.WrongLedgerAccountType, Delta: delta.Int64()}
		correctAccount := ledgerRepairAccount{ID: command.CorrectLedgerAccountID, Code: auditString(correct, "code"), AccountType: plan.CorrectLedgerAccountType}
		repairInput := LedgerRepairCommandInput{TargetJournalEntryID: journalID, TargetPostingID: command.TargetPostingID, WrongLedgerAccountID: command.WrongLedgerAccountID, CorrectLedgerAccountID: command.CorrectLedgerAccountID, AmountMicrocredits: command.AmountMicrocredits}
		claimVersionSeen, executionDigestMatches := false, 0
		for _, item := range history {
			version := auditInteger(item, "version")
			policy := auditInteger(item, "policy_version")
			schema := auditInteger(item, "snapshot_schema_version")
			if version == nil || !version.IsInt64() || version.Sign() <= 0 || policy == nil || !policy.IsInt64() || policy.Int64() != 1 || schema == nil || !schema.IsInt64() || schema.Int64() != 1 || auditString(item, "state") != "ACTIVE" {
				return false, nil
			}
			if version.Int64() == claimVersion {
				claimVersionSeen = true
			}
			if ledgerRepairFactsDigest(target, posting, correctAccount, repairInput, 1, 1, version.Int64(), plan.PostingSetDigest) == auditString(c, "facts_digest") {
				executionDigestMatches++
			}
		}
		if !claimVersionSeen || executionDigestMatches != 1 {
			return false, nil
		}
	}
	return true, nil
}
