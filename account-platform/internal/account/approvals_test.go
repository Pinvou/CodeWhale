package account

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
)

func approvalTestDraft() ApprovalDraft {
	return ApprovalDraft{
		CommandType: "WALLET_ADJUSTMENT", IdempotencyKey: "approval-test-key",
		NormalizedCommand:  json.RawMessage(`{"adjustment_id":"0198f110-f538-7f30-a8b1-f3984fd459a0","wallet_id":"0198f110-f538-7f30-a8b1-f3984fd459a1","amount_microcredits":100,"requested_by":"0198f110-f538-7f30-a8b1-f3984fd459a2"}`),
		TargetResourceType: "MANUAL_ADJUSTMENT", TargetResourceID: "0198f110-f538-7f30-a8b1-f3984fd459a0",
		Reason: "independent financial repair", RedactedEvidence: json.RawMessage(`[]`), ExpectedImpact: json.RawMessage(`{"delta":100}`),
		CriticalResourceVersions: json.RawMessage(`{"adjustment_version":1}`), FinancialSnapshot: json.RawMessage(`{"available":500}`),
	}
}

func TestServicePrincipalCannotRequestOrApprove(t *testing.T) {
	service := identity.Principal{Issuer: "https://issuer.example", Subject: "billing-command-executor"}
	if humanApprovalPrincipal(service) {
		t.Fatal("service identity without an interactive session must be rejected")
	}
	human := service
	human.SessionID = "interactive-session"
	human.AuthenticatedAt = time.Now()
	if !humanApprovalPrincipal(human) {
		t.Fatal("interactive authenticated user must be recognized")
	}
}

func TestApprovalDraftRejectsSecretsInEveryFrozenSnapshot(t *testing.T) {
	draft := approvalTestDraft()
	draft.FinancialSnapshot = json.RawMessage(`{"api_key":"must-not-be-stored"}`)
	if _, _, _, err := normalizeApprovalDraft(draft); err == nil {
		t.Fatal("financial snapshot secret must be rejected")
	}
	draft = approvalTestDraft()
	draft.RedactedEvidence = json.RawMessage(`[{"access_token":"must-not-be-stored"}]`)
	if _, _, _, err := normalizeApprovalDraft(draft); err == nil {
		t.Fatal("unredacted evidence secret must be rejected")
	}
}

func TestApprovalFingerprintCanonicalizesCriticalFinancialSnapshots(t *testing.T) {
	first := approvalTestDraft()
	second := approvalTestDraft()
	second.CriticalResourceVersions = json.RawMessage(`{ "adjustment_version" : 1 }`)
	second.FinancialSnapshot = json.RawMessage(`{ "available" : 500 }`)
	_, _, firstFingerprint, firstErr := normalizeApprovalDraft(first)
	_, _, secondFingerprint, secondErr := normalizeApprovalDraft(second)
	if firstErr != nil || secondErr != nil || firstFingerprint != secondFingerprint {
		t.Fatalf("semantic JSON equality must produce one fingerprint: %v %v", firstErr, secondErr)
	}
}

// The eight financial repair commands are all executable now (each one still
// gates on its live Billing activation), so the old "unimplemented command" case
// no longer exists. What must keep failing closed is any command that is not a
// registered approval-required policy at all.
func TestUnregisteredCommandCannotEnterApprovalLifecycle(t *testing.T) {
	draft := approvalTestDraft()
	draft.CommandType = "FUTURE_FINANCIAL_REPAIR"
	if _, _, _, err := normalizeApprovalDraft(draft); err == nil {
		t.Fatal("an unregistered command must fail before draft creation")
	}
}
