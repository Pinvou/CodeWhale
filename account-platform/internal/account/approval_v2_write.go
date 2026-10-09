package account

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var approvalV2DraftFields = map[string]bool{
	"command_type": true, "idempotency_key": true, "normalized_command": true,
	"target_resource_type": true, "target_resource_id": true, "reason": true,
	"redacted_evidence": true, "expected_impact": true,
	"critical_resource_versions": true, "financial_snapshot": true,
	"supersedes_request_id": true,
}

func decodeApprovalV2WriteBody(body io.Reader, allowed map[string]bool, target any) error {
	raw, err := io.ReadAll(io.LimitReader(body, (4<<20)+1))
	if err != nil || len(raw) > 4<<20 {
		return errInvalidApprovalDecision
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return errInvalidApprovalDecision
	}
	seen := map[string]bool{}
	for decoder.More() {
		key, err := decoder.Token()
		name, ok := key.(string)
		if err != nil || !ok || !allowed[name] || seen[name] {
			return errInvalidApprovalDecision
		}
		seen[name] = true
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return errInvalidApprovalDecision
		}
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') || decoder.Decode(new(any)) != io.EOF {
		return errInvalidApprovalDecision
	}
	if json.Unmarshal(raw, target) != nil {
		return errInvalidApprovalDecision
	}
	return nil
}

func invalidApprovalV2Write(w http.ResponseWriter, r *http.Request) {
	api.WriteError(w, http.StatusBadRequest, "INVALID_APPROVAL_REQUEST", "invalid v2 approval request", httpserver.RequestID(r.Context()))
}

// requireApprovalDraftWriteControl locks the sole control row shared and then
// re-verifies, inside the caller's write transaction, both the frozen
// SAFE_ROLLBACK write closure and (when the caller supplied one) the v2
// action-control generation. Existing v1 callers pass no expected generation,
// but they are no longer exempt from the mode matrix: design section 8.7
// closes create/update/submit from the SAFE CAS onward, and section 5.4 fixes
// the answer as 503 APPROVAL_SAFE_ROLLBACK with zero side effects.
func requireApprovalDraftWriteControl(ctx context.Context, tx pgx.Tx) error {
	control, err := LockAdminApprovalControlShared(ctx, tx)
	if err != nil {
		return err
	}
	if expected := approvalV2ControlVersion(ctx); expected > 0 && control.Version != expected {
		return ErrApprovalActionControlChanged
	}
	return approvalDraftWriteGateError(control)
}

// requireApprovalDraftCloseControl keeps the safe-close entry (withdraw) on the
// same shared control lock. The frozen matrix allows WITHDRAW/REJECT in every
// mode, so it only re-verifies the v2 generation and never applies the
// SAFE_ROLLBACK write closure.
func requireApprovalDraftCloseControl(ctx context.Context, tx pgx.Tx) error {
	control, err := LockAdminApprovalControlShared(ctx, tx)
	if err != nil {
		return err
	}
	if expected := approvalV2ControlVersion(ctx); expected > 0 && control.Version != expected {
		return ErrApprovalActionControlChanged
	}
	return nil
}

// V2 writes retain the v1 repository/state machine, but never expose the raw
// ApprovalRequest response or accept a stale action-control generation.
func approvalV2WriteContext(w http.ResponseWriter, r *http.Request) (context.Context, bool) {
	values := r.Header.Values("X-Pinvou-Approval-Action-Control-Version")
	if len(values) != 1 {
		api.WriteError(w, http.StatusBadRequest, "INVALID_APPROVAL_ACTION_CONTROL_VERSION", "approval action control version is invalid", httpserver.RequestID(r.Context()))
		return nil, false
	}
	version, err := strconv.ParseInt(values[0], 10, 64)
	if err != nil || version < 1 || values[0] != strconv.FormatInt(version, 10) {
		api.WriteError(w, http.StatusBadRequest, "INVALID_APPROVAL_ACTION_CONTROL_VERSION", "approval action control version is invalid", httpserver.RequestID(r.Context()))
		return nil, false
	}
	return context.WithValue(r.Context(), approvalV2ControlVersionKey{}, version), true
}

func (s *Service) writeApprovalV2MutationResult(w http.ResponseWriter, r *http.Request, id uuid.UUID, status int) {
	view, ok := s.repository.(interface {
		ApprovalRequestV2(context.Context, identity.Principal, uuid.UUID) (any, error)
	})
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "APPROVAL_UNAVAILABLE", "approval workflow is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	detail, err := view.ApprovalRequestV2(r.Context(), mustPrincipal(r.Context()), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, status, detail)
}

func (s *Service) adminCreateApprovalDraftV2(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	repository, ok := s.requireApprovalRepository(w, r)
	if !ok {
		return
	}
	ctx, ok := approvalV2WriteContext(w, r)
	if !ok {
		return
	}
	var body approvalDraftBody
	if decodeApprovalV2WriteBody(r.Body, approvalV2DraftFields, &body) != nil {
		invalidApprovalV2Write(w, r)
		return
	}
	item, err := repository.CreateApprovalDraft(ctx, mustPrincipal(r.Context()), body.model())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeApprovalV2MutationResult(w, r, item.ID, http.StatusCreated)
}

func (s *Service) adminUpdateApprovalDraftV2(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	repository, id, ok := s.approvalRequestTarget(w, r)
	if !ok {
		return
	}
	ctx, ok := approvalV2WriteContext(w, r)
	if !ok {
		return
	}
	var body approvalDraftBody
	updateFields := make(map[string]bool, len(approvalV2DraftFields)+1)
	for key := range approvalV2DraftFields {
		updateFields[key] = true
	}
	updateFields["expected_version"] = true
	if decodeApprovalV2WriteBody(r.Body, updateFields, &body) != nil || body.ExpectedVersion <= 0 {
		invalidApprovalV2Write(w, r)
		return
	}
	if _, err := repository.UpdateApprovalDraft(ctx, mustPrincipal(r.Context()), id, body.ExpectedVersion, body.model()); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeApprovalV2MutationResult(w, r, id, http.StatusOK)
}

func (s *Service) adminSubmitApprovalRequestV2(w http.ResponseWriter, r *http.Request) {
	s.adminTransitionApprovalV2(w, r, "submit")
}

func (s *Service) adminWithdrawApprovalRequestV2(w http.ResponseWriter, r *http.Request) {
	s.adminTransitionApprovalV2(w, r, "withdraw")
}

func (s *Service) adminTransitionApprovalV2(w http.ResponseWriter, r *http.Request, action string) {
	w.Header().Set("Cache-Control", "no-store")
	repository, id, ok := s.approvalRequestTarget(w, r)
	if !ok {
		return
	}
	ctx, ok := approvalV2WriteContext(w, r)
	if !ok {
		return
	}
	var body struct {
		ExpectedVersion int64 `json:"expected_version"`
	}
	if decodeApprovalV2WriteBody(r.Body, map[string]bool{"expected_version": true}, &body) != nil || body.ExpectedVersion <= 0 {
		invalidApprovalV2Write(w, r)
		return
	}
	var err error
	if action == "submit" {
		_, err = repository.SubmitApprovalRequest(ctx, mustPrincipal(r.Context()), id, body.ExpectedVersion)
	} else {
		_, err = repository.WithdrawApprovalRequest(ctx, mustPrincipal(r.Context()), id, body.ExpectedVersion)
	}
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeApprovalV2MutationResult(w, r, id, http.StatusOK)
}
