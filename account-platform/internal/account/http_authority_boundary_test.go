package account

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestAccountMalformedMutationBodiesRejectBeforeEffects(t *testing.T) {
	service := NewService(&PostgresRepository{}, nil)
	for _, action := range []struct {
		name    string
		handler func(http.ResponseWriter, *http.Request)
	}{
		{"update", service.adminUpdateApprovalDraft},
		{"submit", service.adminSubmitApprovalRequest},
		{"withdraw", service.adminWithdrawApprovalRequest},
		{"decision", service.adminDecideApprovalRequest},
		{"organization_role", service.grantOrganizationRoleBinding},
		{"authorization", service.checkPermission},
	} {
		for index, body := range []string{"{", `{"unknown":true}`, `{} {}`, `{} {"late":true}`, strings.Repeat(" ", 1<<20) + "{}", `{"expected_version":0}`} {
			t.Run(fmt.Sprintf("%s/%d", action.name, index), func(t *testing.T) {
				request := httptest.NewRequest("POST", "/controlled", strings.NewReader(body))
				request.SetPathValue("approval_request_id", uuid.NewString())
				request.SetPathValue("organization_id", uuid.NewString())
				response := httptest.NewRecorder()
				action.handler(response, request)
				if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"INVALID_REQUEST"`) {
					t.Fatal("invalid mutation request was not rejected before repository effects")
				}
			})
		}
	}
}

func TestIdentityMutationsRequireAuthoritativeNoContent(t *testing.T) {
	for _, operation := range []struct {
		name   string
		invoke func(*HTTPIdentityProvisioner) error
	}{
		{"validate", func(p *HTTPIdentityProvisioner) error {
			return p.ValidateMobileUser(context.Background(), uuid.New(), "controlled.user", "+8613800138000", "controlled", "v1")
		}},
		{"disable", func(p *HTTPIdentityProvisioner) error {
			return p.DisableUserAndConfirm(context.Background(), "controlled-subject")
		}},
		{"delete", func(p *HTTPIdentityProvisioner) error {
			return p.DeleteUser(context.Background(), "controlled-subject")
		}},
		{"username", func(p *HTTPIdentityProvisioner) error {
			return p.UpdateUsername(context.Background(), "controlled-subject", uuid.New(), "controlled.user")
		}},
		{"mobile", func(p *HTTPIdentityProvisioner) error {
			return p.UpdateMobile(context.Background(), "controlled-subject", uuid.New(), "+8613800138000")
		}},
		{"reset", func(p *HTTPIdentityProvisioner) error {
			return p.ResetPassword(context.Background(), "controlled-subject", uuid.New(), "+8613800138000", "controlled", "v1")
		}},
		{"revoke", func(p *HTTPIdentityProvisioner) error {
			return p.RevokeSessions(context.Background(), "controlled-subject", uuid.New(), true)
		}},
	} {
		for _, status := range []int{200, 201, 202, 204, 302} {
			t.Run(fmt.Sprintf("%s/%d", operation.name, status), func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(status)
					if status != 204 {
						_, _ = w.Write([]byte("<html>unconfirmed</html>"))
					}
				}))
				defer server.Close()
				client, err := NewHTTPIdentityProvisioner(server.URL, "controlled-service-token")
				if err != nil {
					t.Fatal("identity fixture configuration failed")
				}
				if (operation.invoke(client) == nil) != (status == 204) {
					t.Fatal("identity mutation accepted an unconfirmed success status")
				}
			})
		}
	}
}
