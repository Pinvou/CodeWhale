package account

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The production client test compares its actual wire bodies with this same
// closed contract; these bytes enter the genuine Account HTTP decoders below.
//
//go:embed testdata/approval-decision-wire.json
var approvalDecisionWireContract []byte

type decisionHTTPSpy struct {
	Repository
	approvalRepository
	writes, reads             int
	decision, reason          string
	expected, origin, current int64
	command                   approvalDecisionCommand
	protocol                  bool
}

func (s *decisionHTTPSpy) DecideApprovalRequest(ctx context.Context, _ identity.Principal, id uuid.UUID, expected int64, decision, reason string, _ json.RawMessage) (ApprovalRequest, error) {
	s.writes++
	s.expected = expected
	s.decision = decision
	s.reason = reason
	s.current = approvalV2ControlVersion(ctx)
	s.command, s.protocol = approvalDecisionProtocol(ctx)
	return ApprovalRequest{ID: id}, nil
}
func (s *decisionHTTPSpy) ApprovalRequest(_ context.Context, _ identity.Principal, id uuid.UUID) (ApprovalRequest, error) {
	return ApprovalRequest{ID: id}, nil
}
func (s *decisionHTTPSpy) ApprovalRequestV2(_ context.Context, _ identity.Principal, id uuid.UUID) (any, error) {
	return map[string]any{"approval_request_id": id}, nil
}
func (s *decisionHTTPSpy) ApprovalActionControl(context.Context) (AdminApprovalControl, error) {
	return AdminApprovalControl{Version: 1, Mode: AdminApprovalModeProofRequired}, nil
}
func (s *decisionHTTPSpy) ApprovalDecisionResult(ctx context.Context, _ identity.Principal, id uuid.UUID, expected, origin int64, decision, digest string) (ApprovalDecisionReceipt, error) {
	s.reads++
	s.expected = expected
	s.origin = origin
	s.current = approvalV2ControlVersion(ctx)
	return ApprovalDecisionReceipt{id, 1, "DECIDE", decision, expected, expected + 1, origin, s.current, digest, uuid.New(), uuid.New(), time.Now().UTC()}, nil
}
func decisionHTTPCall(handler http.HandlerFunc, method, target, body, control string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.SetPathValue("approval_request_id", "11111111-1111-4111-8111-111111111111")
	r.Header.Set("Authorization", "Bearer controlled")
	if control != "" {
		r.Header.Set("X-Pinvou-Approval-Action-Control-Version", control)
	}
	w := httptest.NewRecorder()
	identity.Middleware(func(context.Context, string) (identity.Principal, error) {
		return identity.Principal{Issuer: "controlled", Subject: "controlled", SessionID: "controlled", AuthenticatedAt: time.Now()}, nil
	}, handler).ServeHTTP(w, r)
	return w
}
func TestApprovalDecisionProtocolHTTP(t *testing.T) {
	var rows []struct {
		Wire   int             `json:"wire"`
		Action string          `json:"ui_action"`
		Body   json.RawMessage `json:"body"`
		Digest string          `json:"reason_digest"`
	}
	if json.Unmarshal(approvalDecisionWireContract, &rows) != nil || len(rows) != 4 {
		t.Fatal("missing closed shared wire contract")
	}
	for _, row := range rows {
		t.Run(fmt.Sprintf("v%d-%s-canonical-normal-command", row.Wire, row.Action), func(t *testing.T) {
			spy := &decisionHTTPSpy{}
			s := NewService(spy, nil)
			handler := s.adminDecideApprovalRequest
			if row.Wire == 2 {
				handler = s.adminDecideApprovalRequestV2
			}
			w := decisionHTTPCall(handler, "POST", "/approval", string(row.Body), "1")
			var body struct{ Decision, Reason string }
			json.Unmarshal(row.Body, &body)
			digest := sha256.Sum256([]byte(body.Reason))
			if w.Code != 200 || spy.writes != 1 || spy.expected != 4 || spy.current != 1 || !spy.protocol || spy.decision != body.Decision || spy.command.Decision != body.Decision || spy.command.RawReason != body.Reason || spy.command.ReasonDigest != hex.EncodeToString(digest[:]) || spy.command.ReasonDigest != row.Digest {
				t.Fatal("normal client wire lost canonical decision/original facts", w.Code, spy, w.Body.String())
			}
			legacy := map[string]any{}
			json.Unmarshal(row.Body, &legacy)
			legacy["decision"] = row.Action
			raw, _ := json.Marshal(legacy)
			before := spy.writes
			w = decisionHTTPCall(handler, "POST", "/approval", string(raw), "1")
			if w.Code != 400 || spy.writes != before {
				t.Fatal("UI action alias reached new decision repository", w.Code, spy.writes)
			}
		})
	}
	t.Run("protocol-control-and-fields-closed-before-dispatch", func(t *testing.T) {
		for _, wire := range []int{1, 2} {
			for _, field := range []string{`"command_protocol":0`, `"command_protocol":null`, `"command_protocol":"1"`, `"command_protocol":2`, `"command_protocol":1,"unknown":true`} {
				spy := &decisionHTTPSpy{}
				s := NewService(spy, nil)
				h := s.adminDecideApprovalRequest
				body := `{"expected_version":4,"decision":"REJECTED","reason":"Controlled reason",` + field
				if wire == 2 {
					h = s.adminDecideApprovalRequestV2
					body += `,"action_control_version":1`
				}
				body += `}`
				w := decisionHTTPCall(h, "POST", "/approval", body, "1")
				if w.Code != 400 || spy.writes != 0 {
					t.Fatal("invalid new protocol dispatched", wire, w.Code, field)
				}
			}
		}
	})
	t.Run("confirmation-keeps-original-facts-separate-from-current-control", func(t *testing.T) {
		spy := &decisionHTTPSpy{}
		s := NewService(spy, nil)
		digest := strings.Repeat("a", 64)
		q := "/decision-result?command_protocol=1&expected_version=4&origin_action_control_version=1&decision=APPROVED&reason_digest=" + digest
		w := decisionHTTPCall(s.adminApprovalDecisionResult, "GET", q, "", "7")
		if w.Code != 200 || spy.reads != 1 || spy.writes != 0 || spy.current != 7 || spy.origin != 1 || spy.expected != 4 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal(w.Code, spy, w.Body.String())
		}
		for _, suffix := range []string{"&extra=1", "&decision=REJECTED", "&reason_digest=bad"} {
			before := spy.reads
			w = decisionHTTPCall(s.adminApprovalDecisionResult, "GET", q+suffix, "", "7")
			if w.Code != 400 || spy.reads != before || spy.writes != 0 {
				t.Fatal("nonclosed confirmation reached repo", w.Code)
			}
		}
	})
}
