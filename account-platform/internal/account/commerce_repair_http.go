package account

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/google/uuid"
)

func (s *Service) registerCommerceRepairRoutes(admin *httpserver.RouteGroup) {
	admin.GET("/commerce-repairs", s.requirePermission(PermissionCommerceRepairsRead), httpserver.AdaptFunc(s.adminCommerceRepairs))
	admin.GET("/commerce-repairs/:case_id", s.requirePermission(PermissionCommerceRepairsRead), httpserver.AdaptFunc(s.adminCommerceRepairDetail))
	admin.POST("/commerce-repairs", s.requirePermission(PermissionCommerceRepairsRequest), approvalpolicy.Require("ACCOUNT_COMMERCE_REPAIR_QUARANTINE", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminQuarantineCommerceRepair))
	admin.POST("/commerce-repairs/:case_id/observations", s.requirePermission(PermissionCommerceRepairsRequest), approvalpolicy.Require("ACCOUNT_COMMERCE_REPAIR_OBSERVE", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminObserveCommerceRepair))
	admin.POST("/commerce-repairs/:case_id/actions/confirm", s.requirePermission(PermissionCommerceRepairsApprove), approvalpolicy.Require("ACCOUNT_COMMERCE_REPAIR_CONFIRM", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminConfirmCommerceRepair))
}

func (s *Service) commerceRepairRepository(w http.ResponseWriter, r *http.Request) (CommerceRepairRepository, bool) {
	w.Header().Set("Cache-Control", "no-store")
	repo, ok := s.repository.(CommerceRepairRepository)
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "COMMERCE_REPAIR_UNAVAILABLE", "commerce repair is unavailable", httpserver.RequestID(r.Context()))
	}
	return repo, ok
}

func commerceRepairID(r *http.Request) (uuid.UUID, error) {
	raw := r.PathValue("case_id")
	id, err := uuid.Parse(raw)
	if err != nil || id == uuid.Nil || id.String() != raw {
		return uuid.Nil, ErrInvalidCommerceRepair
	}
	return id, nil
}

func decodeCommerceRepair(r *http.Request, body any) error {
	if r.URL.RawQuery != "" || r.URL.ForceQuery || r.Header.Get("Content-Type") != "application/json" {
		return ErrInvalidCommerceRepair
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, (8<<10)+1))
	if err != nil || len(raw) > 8<<10 || !uniqueRecoveryJSONKeys(raw) {
		return ErrInvalidCommerceRepair
	}
	// encoding/json accepts case-folded field aliases and several UUID text
	// aliases. Keep the operator protocol closed before decoding its DTO.
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || object == nil {
		return ErrInvalidCommerceRepair
	}
	for key, value := range object {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return ErrInvalidCommerceRepair
		}
		switch key {
		case "job_id", "observation_id":
			var text string
			if json.Unmarshal(value, &text) != nil {
				return ErrInvalidCommerceRepair
			}
			id, e := uuid.Parse(text)
			if e != nil || id == uuid.Nil || id.String() != text {
				return ErrInvalidCommerceRepair
			}
		case "expected_version", "outcome", "evidence_reference", "evidence_sha256", "all_attempts_ended", "external_reference_redacted":
		default:
			return ErrInvalidCommerceRepair
		}
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(body) != nil || d.Decode(new(any)) != io.EOF {
		return ErrInvalidCommerceRepair
	}
	return nil
}

func commerceRepairQuery(r *http.Request, allowed ...string) (url.Values, error) {
	if r.URL.ForceQuery {
		return nil, ErrInvalidCommerceRepair
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, ErrInvalidCommerceRepair
	}
	for key, values := range q {
		found := false
		for _, a := range allowed {
			found = found || key == a
		}
		if !found || len(values) != 1 || values[0] == "" {
			return nil, ErrInvalidCommerceRepair
		}
	}
	return q, nil
}

func (s *Service) adminCommerceRepairs(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.commerceRepairRepository(w, r)
	if !ok {
		return
	}
	q, err := commerceRepairQuery(r, "after_id", "limit")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	after := uuid.Nil
	limit := 50
	if raw := q.Get("after_id"); raw != "" {
		after, err = uuid.Parse(raw)
		if err != nil || after == uuid.Nil || after.String() != raw {
			s.writeError(w, r, ErrInvalidCommerceRepair)
			return
		}
	}
	if raw := q.Get("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 100 || strconv.Itoa(limit) != raw {
			s.writeError(w, r, ErrInvalidCommerceRepair)
			return
		}
	}
	items, err := repo.CommerceRepairCases(r.Context(), mustPrincipal(r.Context()), after, limit)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Service) adminCommerceRepairDetail(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.commerceRepairRepository(w, r)
	if !ok {
		return
	}
	id, err := commerceRepairID(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	q, err := commerceRepairQuery(r, "after_observation_version", "after_callback_sequence")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var after int64
	if raw := q.Get("after_observation_version"); raw != "" {
		after, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || after < 0 || strconv.FormatInt(after, 10) != raw {
			s.writeError(w, r, ErrInvalidCommerceRepair)
			return
		}
	}
	var afterCallback int64
	if raw := q.Get("after_callback_sequence"); raw != "" {
		afterCallback, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || afterCallback < 0 || strconv.FormatInt(afterCallback, 10) != raw {
			s.writeError(w, r, ErrInvalidCommerceRepair)
			return
		}
	}
	out, err := repo.CommerceRepairDetail(r.Context(), mustPrincipal(r.Context()), id, after, afterCallback)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, out)
}

func (s *Service) adminQuarantineCommerceRepair(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.commerceRepairRepository(w, r)
	if !ok {
		return
	}
	var body struct {
		JobID uuid.UUID `json:"job_id"`
	}
	if err := decodeCommerceRepair(r, &body); err != nil || body.JobID == uuid.Nil {
		s.writeError(w, r, ErrInvalidCommerceRepair)
		return
	}
	out, err := repo.QuarantineCommerceRepair(r.Context(), mustPrincipal(r.Context()), body.JobID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, out)
}

func (s *Service) adminObserveCommerceRepair(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.commerceRepairRepository(w, r)
	if !ok {
		return
	}
	id, err := commerceRepairID(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var body CommerceRepairObservationInput
	if err = decodeCommerceRepair(r, &body); err != nil || !body.valid() {
		s.writeError(w, r, ErrInvalidCommerceRepair)
		return
	}
	out, err := repo.ObserveCommerceRepair(r.Context(), mustPrincipal(r.Context()), id, body)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, out)
}

func (s *Service) adminConfirmCommerceRepair(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.commerceRepairRepository(w, r)
	if !ok {
		return
	}
	id, err := commerceRepairID(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var body CommerceRepairConfirmation
	if err = decodeCommerceRepair(r, &body); err != nil || body.ObservationID == uuid.Nil || body.ExpectedVersion < 2 {
		s.writeError(w, r, ErrInvalidCommerceRepair)
		return
	}
	out, err := repo.ConfirmCommerceRepair(r.Context(), mustPrincipal(r.Context()), id, body)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, out)
}
