package account

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

func (s *Service) adminApprovalWithdrawalResult(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	repository, id, ok := s.approvalRequestTarget(w, r)
	if !ok {
		return
	}
	// This parser only establishes the current control for a read. The frozen
	// original control is a separate query fact, never a replacement write CAS.
	ctx, ok := approvalV2WriteContext(w, r)
	if !ok {
		return
	}
	query := r.URL.Query()
	valid := len(query) == 3 && len(query["command_protocol"]) == 1 && query.Get("command_protocol") == "1"
	parse := func(name string) int64 {
		values := query[name]
		if len(values) != 1 {
			valid = false
			return 0
		}
		value, err := strconv.ParseInt(values[0], 10, 64)
		if err != nil || value < 1 || values[0] != strconv.FormatInt(value, 10) {
			valid = false
		}
		return value
	}
	expected, originalControl := parse("expected_version"), parse("origin_action_control_version")
	if !valid {
		api.WriteError(w, http.StatusBadRequest, "INVALID_APPROVAL_REQUEST", "invalid withdrawal confirmation", httpserver.RequestID(r.Context()))
		return
	}
	reader, ok := repository.(interface {
		ApprovalWithdrawalResult(context.Context, identity.Principal, uuid.UUID, int64, int64) (ApprovalWithdrawalReceipt, error)
	})
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "APPROVAL_UNAVAILABLE", "withdrawal confirmation is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	receipt, err := reader.ApprovalWithdrawalResult(ctx, mustPrincipal(r.Context()), id, expected, originalControl)
	if errors.Is(err, ErrConflict) {
		api.WriteError(w, http.StatusServiceUnavailable, "COMMAND_RESULT_UNKNOWN", "the original withdrawal is not confirmed", httpserver.RequestID(r.Context()))
		return
	}
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, receipt)
}
