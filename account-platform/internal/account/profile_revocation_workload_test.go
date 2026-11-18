package account

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/identityadapter"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/profilerevocation"
	"github.com/google/uuid"
)

type profileRevocationAuthorityStore struct {
	ProfileChangeStore
	claims        int
	confirmations int
}

func (s *profileRevocationAuthorityStore) ClaimProfileRevocation(context.Context, profilerevocation.Command) (profilerevocation.Result, error) {
	s.claims++
	return profilerevocation.Result{State: "CONFIRMED"}, nil
}

func (s *profileRevocationAuthorityStore) ConfirmProfileRevocation(context.Context, profilerevocation.Command) error {
	s.confirmations++
	return nil
}

func TestProfileRevocationExactWorkloadRoutes(t *testing.T) {
	otherToken, identityToken := configureEntitlementProofIdentity(t)
	var rules []map[string]any
	if err := json.Unmarshal([]byte(os.Getenv("WORKLOAD_IDENTITY_RULES_JSON")), &rules); err != nil {
		t.Fatal(err)
	}
	rules[1]["service"] = "identity-service"
	claimPermission := "internal.account.profile-revocation.claim"
	confirmPermission := "internal.account.profile-revocation.confirm"
	for _, scenario := range []struct {
		name           string
		permissions    []string
		token          string
		claim, confirm bool
	}{
		{"both", []string{claimPermission, confirmPermission}, identityToken, true, true},
		{"claim-only", []string{claimPermission}, identityToken, true, false},
		{"confirm-only", []string{confirmPermission}, identityToken, false, true},
		{"other-scope", []string{"internal.auth.password-preparation.claim"}, identityToken, false, false},
		{"other-workload", []string{claimPermission, confirmPermission}, otherToken, false, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			rules[1]["permissions"] = scenario.permissions
			raw, err := json.Marshal(rules)
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("WORKLOAD_IDENTITY_RULES_JSON", string(raw))
			store := &profileRevocationAuthorityStore{}
			service := NewService(nil, nil).WithProfileChanges(&ProfileChangeService{store: store})
			server := httptest.NewServer(service.Handler(nil))
			defer server.Close()
			client, err := identityadapter.NewHTTPProfileRevocationAuthority(server.URL, scenario.token, "")
			if err != nil {
				t.Fatal(err)
			}
			command := profilerevocation.Command{OperationID: uuid.New(), Subject: "controlled-subject", DispatchToken: uuid.New()}
			_, claimErr := client.ClaimProfileRevocation(context.Background(), command)
			confirmErr := client.ConfirmProfileRevocation(context.Background(), command)
			if (claimErr == nil) != scenario.claim || (store.claims == 1) != scenario.claim || (confirmErr == nil) != scenario.confirm || (store.confirmations == 1) != scenario.confirm {
				t.Fatal("exact workload route permission mismatch", store.claims, store.confirmations)
			}
		})
	}
}
