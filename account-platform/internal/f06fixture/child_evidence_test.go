package f06fixture

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPublicFundsChildRequiresExecutedCases(t *testing.T) {
	event := func(action, pkg, test string) string {
		value, err := json.Marshal(map[string]string{"Action": action, "Package": pkg, "Test": test})
		if err != nil {
			t.Fatal(err)
		}
		return string(value) + "\n"
	}
	const tokenPackage = "github.com/Pinvou/pinvou3/account-platform/internal/token"
	const billingPackage = "github.com/Pinvou/pinvou3/account-platform/internal/billing"
	const tokenTest = "TestPersonalExecutionPublicPostgresFundsAndReplay"
	const billingTest = "TestMemberHoldIdentityMutationBoundaries"
	token := event("pass", tokenPackage, tokenTest+"/JSON") + event("pass", tokenPackage, tokenTest)
	billing := event("pass", billingPackage, billingTest+"/commit-after-membership-version-advances") + event("pass", billingPackage, billingTest)
	for _, scenario := range []struct {
		name, stream string
		wantPass     bool
	}{
		{"empty", "", false},
		{"package-only-exit-zero", event("pass", tokenPackage, "") + event("pass", billingPackage, ""), false},
		{"nonexistent-test-name", event("pass", tokenPackage, "TestOrganizationExecutionPublicPostgresFundsAndReplay"), false},
		{"one-package-only", token, false},
		{"parents-without-cases", event("pass", tokenPackage, tokenTest) + event("pass", billingPackage, billingTest), false},
		{"cases-without-parent", event("pass", tokenPackage, tokenTest+"/JSON") + billing, false},
		{"conditional-skip", token + event("skip", billingPackage, billingTest), false},
		{"test-failure", token + event("fail", billingPackage, billingTest), false},
		{"malformed-evidence", token + billing + "not JSON", false},
		{"both-public-suites-completed", token + billing + event("pass", tokenPackage, "") + event("pass", billingPackage, ""), true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			count, err := validateFundsChildJSON(strings.NewReader(scenario.stream))
			if scenario.wantPass {
				if err != nil || count != 4 {
					t.Fatalf("completed public cases rejected: count=%d err=%v", count, err)
				}
			} else if err == nil {
				t.Fatal("unexecuted or invalid public evidence accepted")
			}
		})
	}
}
