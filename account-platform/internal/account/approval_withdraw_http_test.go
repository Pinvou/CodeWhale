package account

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

// Spies exercise the production decoder/context/response boundary only. Real
// authority, immutable event and CAS behavior are separate PostgreSQL cases.
type withdrawalHTTPSpy struct {
	Repository
	approvalRepository
	writes, reads               int
	protocol                    bool
	expected, original, current int64
}

func (s *withdrawalHTTPSpy) WithdrawApprovalRequest(ctx context.Context, _ identity.Principal, id uuid.UUID, expected int64) (ApprovalRequest, error) {
	s.writes++
	s.protocol = approvalWithdrawProtocol(ctx)
	s.expected = expected
	s.current = approvalV2ControlVersion(ctx)
	return ApprovalRequest{ID: id, Status: "WITHDRAWN", Version: expected + 1}, nil
}
func (s *withdrawalHTTPSpy) ApprovalRequestV2(_ context.Context, _ identity.Principal, id uuid.UUID) (any, error) {
	return map[string]any{"approval_request_id": id}, nil
}
func (s *withdrawalHTTPSpy) ApprovalWithdrawalResult(ctx context.Context, _ identity.Principal, id uuid.UUID, expected, original int64) (ApprovalWithdrawalReceipt, error) {
	s.reads++
	s.expected = expected
	s.original = original
	s.current = approvalV2ControlVersion(ctx)
	return ApprovalWithdrawalReceipt{ApprovalRequestID: id, CommandProtocol: 1, Operation: "WITHDRAW", ExpectedVersion: expected, ResultVersion: expected + 1, OriginActionControlVersion: original, ActionControlVersion: s.current, ReceiptEventID: uuid.New(), Status: "WITHDRAWN", RecordedAt: time.Now().UTC()}, nil
}
func withdrawalHTTPCall(s *Service, handler http.HandlerFunc, method, target, body, control string) *httptest.ResponseRecorder {
	id := "11111111-1111-4111-8111-111111111111"
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.SetPathValue("approval_request_id", id)
	r.Header.Set("Authorization", "Bearer controlled-interactive")
	if control != "" {
		r.Header.Set("X-Pinvou-Approval-Action-Control-Version", control)
	}
	w := httptest.NewRecorder()
	identity.Middleware(func(context.Context, string) (identity.Principal, error) {
		return identity.Principal{Issuer: "controlled", Subject: "controlled-user", SessionID: "controlled-session", AuthenticatedAt: time.Now()}, nil
	}, handler).ServeHTTP(w, r)
	return w
}
func TestApprovalWithdrawalProtocolHTTP(t *testing.T) {
	for _, wire := range []string{"v1", "v2"} {
		t.Run(wire, func(t *testing.T) {
			spy := &withdrawalHTTPSpy{}
			s := NewService(spy, nil)
			handler := s.adminWithdrawApprovalRequest
			if wire == "v2" {
				handler = s.adminWithdrawApprovalRequestV2
			}
			w := withdrawalHTTPCall(s, handler, "POST", "/approval", `{"expected_version":4,"command_protocol":1}`, "7")
			if w.Code != 200 || spy.writes != 1 || !spy.protocol || spy.expected != 4 || spy.current != 7 {
				t.Fatal("new protocol lost trusted original CAS/control", w.Code, spy, w.Body.String())
			}
			for _, body := range []string{`{"expected_version":4,"command_protocol":0}`, `{"expected_version":4,"command_protocol":"1"}`, `{"expected_version":4,"command_protocol":2}`, `{"expected_version":4,"command_protocol":1,"unknown":true}`} {
				before := spy.writes
				w = withdrawalHTTPCall(s, handler, "POST", "/approval", body, "7")
				if w.Code != 400 || spy.writes != before {
					t.Fatal("invalid protocol reached repository", w.Code, body)
				}
			}
			before := spy.writes
			w = withdrawalHTTPCall(s, handler, "POST", "/approval", `{"expected_version":4,"command_protocol":1}`, "")
			if w.Code != 400 || spy.writes != before {
				t.Fatal("new protocol accepted missing control", w.Code)
			}
		})
	}
	t.Run("confirmation-keeps-original-CAS-and-control-separate-from-current-read", func(t *testing.T) {
		spy := &withdrawalHTTPSpy{}
		s := NewService(spy, nil)
		w := withdrawalHTTPCall(s, s.adminApprovalWithdrawalResult, "GET", "/withdraw-result?command_protocol=1&expected_version=4&origin_action_control_version=1", "", "7")
		if w.Code != 200 || spy.reads != 1 || spy.writes != 0 || spy.expected != 4 || spy.original != 1 || spy.current != 7 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal(w.Code, spy, w.Body.String())
		}
	})
	t.Run("closed-confirmation-query-never-falls-back-to-write", func(t *testing.T) {
		for _, q := range []string{"", "command_protocol=1&expected_version=4", "command_protocol=1&expected_version=04&origin_action_control_version=1", "command_protocol=1&expected_version=4&origin_action_control_version=1&extra=1", "command_protocol=1&expected_version=4&expected_version=4&origin_action_control_version=1"} {
			spy := &withdrawalHTTPSpy{}
			s := NewService(spy, nil)
			w := withdrawalHTTPCall(s, s.adminApprovalWithdrawalResult, "GET", "/withdraw-result?"+q, "", "7")
			if w.Code != 400 || spy.writes != 0 || spy.reads != 0 {
				t.Fatal(w.Code, q)
			}
		}
	})
}
