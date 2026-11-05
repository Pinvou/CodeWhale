package account

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Embedded nil methods deliberately fail the test if malformed input reaches
// a repository operation. This verifies the actual handler error boundary.
type malformedApprovalRepository struct {
	Repository
	approvalRepository
}

func TestApprovalInvalidWriteReturnsErrorBeforeMutation(t *testing.T) {
	s := NewService(malformedApprovalRepository{}, nil)
	for name, handler := range map[string]http.HandlerFunc{
		"create": s.adminCreateApprovalDraft, "update": s.adminUpdateApprovalDraft,
		"submit": s.adminSubmitApprovalRequest, "withdraw": s.adminWithdrawApprovalRequest,
		"decision": s.adminDecideApprovalRequest,
	} {
		for i, body := range []string{`{`, `{} {}`, `{"unknown":true}`} {
			t.Run(fmt.Sprintf("%s/malformed-%d", name, i), func(t *testing.T) {
				r := httptest.NewRequest(http.MethodPost, "/admin/v1/approval-requests/11111111-1111-4111-8111-111111111111", strings.NewReader(body))
				r.SetPathValue("approval_request_id", "11111111-1111-4111-8111-111111111111")
				w := httptest.NewRecorder()
				handler(w, r)
				if w.Code != http.StatusBadRequest {
					t.Fatalf("invalid input returned %d", w.Code)
				}
			})
		}
	}
}

func TestDelegationMalformedRequestReturnsError(t *testing.T) {
	for _, body := range []string{`{`, `{} {}`, `{"unknown":true}`} {
		t.Run(body, func(t *testing.T) {
			s := NewService(nil, nil)
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/internal/v1/authorization/human-delegations", strings.NewReader(body))
			s.createHumanDelegation(w, r)
			if w.Code != 400 {
				t.Fatalf("invalid request returned %d", w.Code)
			}
		})
	}
}

func TestApprovalProofConstructorRejectsAmbiguousAuthority(t *testing.T) {
	for _, raw := range []string{"https://authority.invalid?", "https://:443", "https://user:pass@authority.invalid", "https://authority.invalid#fragment", "https://authority.invalid/path", "file:///private", "https://authority.invalid?name=value"} {
		t.Run(raw, func(t *testing.T) {
			if _, err := NewHTTPApprovalProofSource(raw, "test-credential"); err == nil {
				t.Fatal("ambiguous authority accepted")
			}
		})
	}
}
