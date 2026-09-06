package token

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
)

// This independent acceptance matrix freezes the entire permission conjunction
// and recent-auth requirement, rather than deriving expectations from the
// production specs. In particular owner-candidates.read must not disappear
// behind a comparison of only the first permission.
func TestNineteenTokenCommandEnforcementMatrix(t *testing.T) {
	type contract struct {
		permissions []string
		recentAuth  bool
		approval    bool
	}
	expected := map[string]contract{
		"TOKEN_PROTOCOL_DRAFT_CREATE":      {[]string{"platform.pricing.manage"}, false, false},
		"TOKEN_PROTOCOL_PUBLISH":           {[]string{"platform.pricing.manage"}, true, false},
		"TOKEN_FORMULA_DRAFT_CREATE":       {[]string{"platform.pricing.manage"}, false, false},
		"TOKEN_FORMULA_PUBLISH":            {[]string{"platform.pricing.manage"}, true, false},
		"TOKEN_MODEL_DRAFT_SAVE":           {[]string{"platform.models.manage"}, false, false},
		"TOKEN_MODEL_PUBLISH":              {[]string{"platform.models.manage"}, true, false},
		"TOKEN_MODEL_LIFECYCLE_CHANGE":     {[]string{"platform.models.lifecycle.request"}, true, true},
		"TOKEN_MODEL_CATALOG_SYNC":         {[]string{"platform.models.sync"}, false, false},
		"TOKEN_MODEL_CATALOG_ROLLBACK":     {[]string{"platform.models.catalog.rollback.request"}, true, true},
		"TOKEN_ACCOUNT_CREATE":             {[]string{"platform.tokens.manage", "platform.tokens.owner-candidates.read"}, true, false},
		"TOKEN_ACCOUNT_STATUS_CHANGE":      {[]string{"platform.tokens.manage"}, true, false},
		"TOKEN_ROUTE_UPSERT":               {[]string{"platform.tokens.manage"}, true, false},
		"TOKEN_ROUTE_DELETE":               {[]string{"platform.tokens.manage"}, true, false},
		"TOKEN_CREDENTIAL_ENROLL":          {[]string{"platform.tokens.manage"}, true, false},
		"TOKEN_CREDENTIAL_ROTATE":          {[]string{"platform.tokens.manage"}, true, false},
		"TOKEN_CREDENTIAL_REVOKE":          {[]string{"platform.tokens.credentials.revoke.request"}, true, true},
		"TOKEN_CREDENTIAL_VALIDATE":        {[]string{"platform.tokens.credentials.validate"}, true, false},
		"TOKEN_ROUTE_PROVISIONING_REPAIR":  {[]string{"platform.tokens.manage"}, true, false},
		"TOKEN_USAGE_RECONCILIATION_RETRY": {[]string{"platform.usage.reconcile.retry"}, false, false},
	}
	if len(tokenCommandSpecs) != len(expected) {
		t.Fatalf("governed command coverage changed: got %d want %d", len(tokenCommandSpecs), len(expected))
	}
	approved, auditOnly := 0, 0
	for command, want := range expected {
		t.Run(command, func(t *testing.T) {
			got, exists := tokenCommandSpecs[command]
			if !exists || !slices.Equal(got.Permissions, want.permissions) || got.RecentAuth != want.recentAuth || got.Approval != want.approval {
				t.Fatalf("enforcement drift: got %+v want %+v", got, want)
			}
			policy, err := approvalpolicy.Lookup(command)
			if err != nil || policy.RequesterPermission != want.permissions[0] || approvalpolicy.Executable(command) != want.approval {
				t.Fatalf("policy/allowlist drift: %+v err=%v", policy, err)
			}
			if want.approval {
				approved++
				if policy.Mode != approvalpolicy.ApprovalRequired || policy.ExecutorIdentity != "token-command-executor" || policy.ApproverPermission == "" || !policy.SelfApprovalForbidden || policy.ApprovalCount != 1 {
					t.Fatalf("approval boundary drift: %+v", policy)
				}
			} else {
				auditOnly++
				if policy.Mode != approvalpolicy.AuditOnly || policy.ExecutorIdentity != "requesting-domain-service" || len(got.RequiredFacts) == 0 || !slices.Equal(got.RequiredFacts, policy.CriticalResourceFields) {
					t.Fatalf("audit-only CAS boundary drift: spec=%+v policy=%+v", got, policy)
				}
			}
		})
	}
	if approved != 3 || auditOnly != 16 {
		t.Fatalf("approval partition changed: approved=%d auditOnly=%d", approved, auditOnly)
	}
	// Token installation must not remove previously executable Billing work.
	for _, command := range []string{"WALLET_ADJUSTMENT", "PAYMENT_PROVIDER_RESUME", "PAYMENT_PRODUCT_RESUME"} {
		if !approvalpolicy.Executable(command) {
			t.Fatalf("existing executable command removed: %s", command)
		}
	}
}

func TestTokenCommandHandlerManifestMatchesExecutableDispatch(t *testing.T) {
	expected := map[string][2]string{
		"TOKEN_PROTOCOL_DRAFT_CREATE": {"PROTOCOL_FORMULA", "COMMAND"}, "TOKEN_PROTOCOL_PUBLISH": {"PROTOCOL_FORMULA", "COMMAND"},
		"TOKEN_FORMULA_DRAFT_CREATE": {"PROTOCOL_FORMULA", "COMMAND"}, "TOKEN_FORMULA_PUBLISH": {"PROTOCOL_FORMULA", "COMMAND"},
		"TOKEN_MODEL_DRAFT_SAVE": {"MODEL_ACCOUNT", "COMMAND"}, "TOKEN_MODEL_PUBLISH": {"MODEL_ACCOUNT", "COMMAND"},
		"TOKEN_MODEL_LIFECYCLE_CHANGE": {"LIFECYCLE", "APPROVED"}, "TOKEN_MODEL_CATALOG_SYNC": {"CATALOG_SYNC", "COMMAND"},
		"TOKEN_MODEL_CATALOG_ROLLBACK": {"CATALOG_ROLLBACK", "APPROVED"}, "TOKEN_ACCOUNT_CREATE": {"MODEL_ACCOUNT", "COMMAND"},
		"TOKEN_ACCOUNT_STATUS_CHANGE": {"MODEL_ACCOUNT", "COMMAND"}, "TOKEN_ROUTE_UPSERT": {"ROUTE", "COMMAND"},
		"TOKEN_ROUTE_DELETE": {"ROUTE", "COMMAND"}, "TOKEN_CREDENTIAL_ENROLL": {"CREDENTIAL_ENROLL", "ISOLATED"},
		"TOKEN_CREDENTIAL_ROTATE": {"CREDENTIAL_ROTATE", "ISOLATED"}, "TOKEN_CREDENTIAL_REVOKE": {"CREDENTIAL_REVOKE", "APPROVED"},
		"TOKEN_CREDENTIAL_VALIDATE": {"CREDENTIAL_VALIDATE", "COMMAND"}, "TOKEN_ROUTE_PROVISIONING_REPAIR": {"RUNTIME", "COMMAND"},
		"TOKEN_USAGE_RECONCILIATION_RETRY": {"RUNTIME", "COMMAND"},
	}
	manifest, err := TokenCommandHandlerManifest()
	if err != nil || len(manifest) != len(expected) {
		t.Fatalf("handler manifest is incomplete: %d err=%v", len(manifest), err)
	}
	previous := ""
	for _, entry := range manifest {
		want, exists := expected[entry.CommandType]
		if !exists || entry.CommandType <= previous || entry.Family != want[0] || entry.Transport != want[1] || entry.Version != 1 {
			t.Fatalf("wrong adapter ownership or unstable ordering: %+v", entry)
		}
		previous = entry.CommandType
	}
	digest, err := TokenCommandHandlerDigest()
	if err != nil || len(digest) != 64 {
		t.Fatalf("manifest digest=%q err=%v", digest, err)
	}
	const command = "TOKEN_ROUTE_UPSERT"
	original := tokenGovernanceHandlers[command]
	t.Cleanup(func() { tokenGovernanceHandlers[command] = original })
	delete(tokenGovernanceHandlers, command)
	if _, err := TokenCommandHandlerDigest(); err == nil {
		t.Fatal("a missing executable adapter must prevent activation")
	}
	response := httptest.NewRecorder()
	(&Service{}).registeredTokenHandler(command, "COMMAND")(response, httptest.NewRequest(http.MethodPost, "/commands", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("missing adapter still dispatches: %d", response.Code)
	}
	broken := original
	broken.handle = nil
	tokenGovernanceHandlers[command] = broken
	if _, err := TokenCommandHandlerDigest(); err == nil {
		t.Fatal("a nil executable adapter must prevent activation")
	}
	tokenGovernanceHandlers[command] = original
	if restored, err := TokenCommandHandlerDigest(); err != nil || restored != digest {
		t.Fatalf("restoring the executable registry did not restore its digest: %q err=%v", restored, err)
	}
	// The returned manifest is a copy, not a mutation handle for enforcement.
	manifest[0].Permissions[0] = "unregistered.permission"
	if restored, err := TokenCommandHandlerDigest(); err != nil || restored != digest {
		t.Fatal("returned manifest aliases live permission enforcement")
	}
}
