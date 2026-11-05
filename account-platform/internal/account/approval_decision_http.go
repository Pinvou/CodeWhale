package account

import (
	"context"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"net/http"
	"strconv"
)

func (s *Service) adminApprovalDecisionResult(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	repository, id, ok := s.approvalRequestTarget(w, r)
	if !ok {
		return
	}
	ctx, ok := approvalV2WriteContext(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	valid := len(q) == 5 && len(q["command_protocol"]) == 1 && q.Get("command_protocol") == "1"
	parse := func(key string) int64 {
		v := q[key]
		if len(v) != 1 {
			valid = false
			return 0
		}
		n, e := strconv.ParseInt(v[0], 10, 64)
		if e != nil || n < 1 || strconv.FormatInt(n, 10) != v[0] {
			valid = false
		}
		return n
	}
	expected, origin := parse("expected_version"), parse("origin_action_control_version")
	decision, digest := q.Get("decision"), q.Get("reason_digest")
	if len(q["decision"]) != 1 || len(q["reason_digest"]) != 1 || !validApprovalDecisionReceiptFacts(expected, origin, decision, digest) {
		valid = false
	}
	if !valid {
		api.WriteError(w, 400, "INVALID_APPROVAL_REQUEST", "invalid decision confirmation", httpserver.RequestID(r.Context()))
		return
	}
	reader, ok := repository.(interface {
		ApprovalDecisionResult(context.Context, identity.Principal, uuid.UUID, int64, int64, string, string) (ApprovalDecisionReceipt, error)
	})
	if !ok {
		api.WriteError(w, 503, "APPROVAL_UNAVAILABLE", "decision confirmation is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	receipt, err := reader.ApprovalDecisionResult(ctx, mustPrincipal(r.Context()), id, expected, origin, decision, digest)
	if errors.Is(err, ErrApprovalDecisionUnconfirmed) {
		api.WriteError(w, 503, "COMMAND_RESULT_UNKNOWN", "the original approval decision is not confirmed", httpserver.RequestID(r.Context()))
		return
	}
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, 200, receipt)
}
