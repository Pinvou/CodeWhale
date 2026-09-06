package token

import (
	"slices"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
)

func TestTokenAccountRecoveryUsesCurrentCompletePolicy(t *testing.T) {
	current, err := approvalpolicy.Lookup("TOKEN_ACCOUNT_CREATE")
	if err != nil || !validTokenAccountRecoveryPolicy(current) {
		t.Fatalf("current create recovery policy is unavailable: %v", err)
	}
	for _, mutate := range []func(*approvalpolicy.Policy){
		func(p *approvalpolicy.Policy) { p.PolicyVersion = 2 },
		func(p *approvalpolicy.Policy) { p.SnapshotSchemaVersion = 2 },
		func(p *approvalpolicy.Policy) { p.Mode = approvalpolicy.ApprovalRequired },
		func(p *approvalpolicy.Policy) { p.RequesterPermission = "platform.tokens.read" },
		func(p *approvalpolicy.Policy) {
			p.CriticalResourceFields = p.CriticalResourceFields[:len(p.CriticalResourceFields)-1]
		},
		func(p *approvalpolicy.Policy) {
			p.CriticalResourceFields[3], p.CriticalResourceFields[4] = p.CriticalResourceFields[4], p.CriticalResourceFields[3]
		},
	} {
		changed := current
		changed.CriticalResourceFields = slices.Clone(current.CriticalResourceFields)
		mutate(&changed)
		if validTokenAccountRecoveryPolicy(changed) {
			t.Fatal("changed or partial current policy must fail closed")
		}
	}
}
