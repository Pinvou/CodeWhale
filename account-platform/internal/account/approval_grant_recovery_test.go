package account

import (
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGrantRecoveryRequiresCompletedEvidence(t *testing.T) {
	digest := strings.Repeat("a", 64)
	proposal := GrantRecoveryProposal{Authority: "BILLING", OriginalEventID: uuid.New(), RequestID: uuid.New(), CommandFingerprint: digest, ApprovedRequestVersion: 3, ApprovalEpoch: 1, Kind: "ACTION_ENDED_NO_EFFECT", Outcome: "NO_EFFECT", Reference: "FIN-RESULT-1", SHA256: digest, CompletionFenceReference: "FIN-FENCE-1", AllObservationsResolved: true}
	raw, _ := json.Marshal(proposal)
	if _, err := grantRecoveryPayload("PROPOSE", raw); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*GrantRecoveryProposal){
		func(p *GrantRecoveryProposal) { p.Kind = "ABSENT" },
		func(p *GrantRecoveryProposal) { p.Outcome = "UNKNOWN" },
		func(p *GrantRecoveryProposal) { p.CompletionFenceReference = "" },
		func(p *GrantRecoveryProposal) { p.AllObservationsResolved = false },
		func(p *GrantRecoveryProposal) { p.SHA256 = strings.Repeat("A", 64) },
		func(p *GrantRecoveryProposal) { p.Reference = "https://private.example/evidence" },
	} {
		p := proposal
		change(&p)
		raw, _ := json.Marshal(p)
		if _, err := grantRecoveryPayload("PROPOSE", raw); !errors.Is(err, ErrInvalidGrantRecovery) {
			t.Fatal("incomplete evidence accepted", err)
		}
	}
	observed, _ := json.Marshal(GrantRecoveryObservation{State: "ABSENT", Reference: "QUERY-1", SHA256: digest})
	if _, err := grantRecoveryPayload("OBSERVE", observed); err != nil {
		t.Fatal(err)
	}
	if _, err := grantRecoveryPayload("PROPOSE", observed); !errors.Is(err, ErrInvalidGrantRecovery) {
		t.Fatal("query absence became terminal evidence")
	}
	for _, raw := range []string{`null`, `{}`, `{"state":"ABSENT","state":"OBSERVED"}`, `{"state":"ABSENT","reference":"QUERY-1","sha256":1}`, `{"state":"ABSENT","reference":"QUERY-1","sha256":"` + digest + `","secret":"private"}`, string(make([]byte, 4097))} {
		if _, err := grantRecoveryPayload("OBSERVE", json.RawMessage(raw)); !errors.Is(err, ErrInvalidGrantRecovery) {
			t.Fatal("invalid evidence accepted")
		}
	}
}

func TestGrantRecoveryQueriesAreBounded(t *testing.T) {
	for _, suffix := range []string{"?limit=0", "?limit=101", "?limit=2&limit=3", "?after=bad", "?after_version=1", "?extra=1", "?"} {
		req := httptest.NewRequest("GET", "http://test/admin/v1/approval-grant-recovery-cases"+suffix, nil)
		if _, _, _, err := grantRecoveryQuery(req, false); err == nil {
			t.Fatal("invalid query accepted", suffix)
		}
	}
	req := httptest.NewRequest("GET", "http://test/admin/v1/approval-grant-recovery-cases?limit=100", nil)
	limit, after, version, err := grantRecoveryQuery(req, false)
	if err != nil || limit != 100 || after != nil || version != 0 {
		t.Fatal("valid boundary rejected", err)
	}
}
