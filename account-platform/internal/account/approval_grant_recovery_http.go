package account

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

type grantRecoveryRepository interface {
	GrantRecoveryCases(context.Context, identity.Principal, *uuid.UUID, int) (GrantRecoveryPage, error)
	GrantRecoveryCase(context.Context, identity.Principal, uuid.UUID, int64, int) (GrantRecoveryDetail, error)
	ApplyGrantRecovery(context.Context, identity.Principal, uuid.UUID, string, GrantRecoveryMutation) (GrantRecoveryCase, error)
}

func (s *Service) registerGrantRecoveryRoutes(admin *httpserver.RouteGroup) {
	admin.GET("/approval-grant-recovery-cases", httpserver.AdaptFunc(s.adminGrantRecoveryCases))
	admin.GET("/approval-grant-recovery-cases/:case_id", httpserver.AdaptFunc(s.adminGrantRecoveryCase))
	for _, route := range []struct{ path, action, policy string }{
		{"observations", "OBSERVE", "APPROVAL_GRANT_RECOVERY_OBSERVE"},
		{"proposals", "PROPOSE", "APPROVAL_GRANT_RECOVERY_PROPOSE"},
		{"reviews", "REVIEW", "APPROVAL_GRANT_RECOVERY_REVIEW"},
	} {
		action := route.action
		admin.POST("/approval-grant-recovery-cases/:case_id/"+route.path, approvalpolicy.Require(route.policy, approvalpolicy.AuditOnly), httpserver.AdaptFunc(func(w http.ResponseWriter, r *http.Request) { s.adminGrantRecoveryMutation(w, r, action) }))
	}
}

func (s *Service) grantRecoveryRepository(w http.ResponseWriter, r *http.Request) (grantRecoveryRepository, bool) {
	w.Header().Set("Cache-Control", "no-store")
	repo, ok := s.repository.(grantRecoveryRepository)
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "APPROVAL_GRANT_RECOVERY_UNAVAILABLE", "manual approval recovery is unavailable", httpserver.RequestID(r.Context()))
	}
	return repo, ok
}

func (s *Service) writeGrantRecoveryError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, ErrInvalidGrantRecovery) {
		api.WriteError(w, http.StatusBadRequest, "INVALID_GRANT_RECOVERY", "invalid manual recovery request", httpserver.RequestID(r.Context()))
		return
	}
	if errors.Is(err, ErrGrantRecoveryUnavailable) {
		api.WriteError(w, http.StatusServiceUnavailable, "APPROVAL_GRANT_RECOVERY_UNAVAILABLE", "manual approval recovery is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	s.writeError(w, r, err)
}

func grantRecoveryQuery(r *http.Request, detail bool) (int, *uuid.UUID, int64, error) {
	if r.URL.ForceQuery {
		return 0, nil, 0, ErrInvalidGrantRecovery
	}
	q := r.URL.Query()
	for k, values := range q {
		if len(values) != 1 || (k != "limit" && !(k == "after" && !detail) && !(k == "after_version" && detail)) {
			return 0, nil, 0, ErrInvalidGrantRecovery
		}
	}
	limit := 25
	if q.Has("limit") {
		n, err := strconv.Atoi(q.Get("limit"))
		if err != nil || n < 1 || n > 100 {
			return 0, nil, 0, ErrInvalidGrantRecovery
		}
		limit = n
	}
	var after *uuid.UUID
	if q.Has("after") {
		id, err := uuid.Parse(q.Get("after"))
		if err != nil || id == uuid.Nil {
			return 0, nil, 0, ErrInvalidGrantRecovery
		}
		after = &id
	}
	version := int64(0)
	if q.Has("after_version") {
		n, err := strconv.ParseInt(q.Get("after_version"), 10, 64)
		if err != nil || n < 0 {
			return 0, nil, 0, ErrInvalidGrantRecovery
		}
		version = n
	}
	return limit, after, version, nil
}

func (s *Service) adminGrantRecoveryCases(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.grantRecoveryRepository(w, r)
	if !ok {
		return
	}
	limit, after, _, err := grantRecoveryQuery(r, false)
	if err != nil {
		s.writeGrantRecoveryError(w, r, err)
		return
	}
	result, err := repo.GrantRecoveryCases(r.Context(), mustPrincipal(r.Context()), after, limit)
	if err != nil {
		s.writeGrantRecoveryError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, result)
}

func (s *Service) adminGrantRecoveryCase(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.grantRecoveryRepository(w, r)
	if !ok {
		return
	}
	id, ok := pathUUID(w, r, "case_id")
	if !ok {
		return
	}
	limit, _, after, err := grantRecoveryQuery(r, true)
	if err != nil {
		s.writeGrantRecoveryError(w, r, err)
		return
	}
	result, err := repo.GrantRecoveryCase(r.Context(), mustPrincipal(r.Context()), id, after, limit)
	if err != nil {
		s.writeGrantRecoveryError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, result)
}

func (s *Service) adminGrantRecoveryMutation(w http.ResponseWriter, r *http.Request, action string) {
	repo, ok := s.grantRecoveryRepository(w, r)
	if !ok {
		return
	}
	id, ok := pathUUID(w, r, "case_id")
	if !ok {
		return
	}
	if r.URL.RawQuery != "" || r.URL.ForceQuery || r.Header.Get("Content-Type") != "application/json" {
		s.writeGrantRecoveryError(w, r, ErrInvalidGrantRecovery)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, (8<<10)+1))
	if err != nil || len(raw) > 8<<10 || !uniqueRecoveryJSONKeys(raw) {
		s.writeGrantRecoveryError(w, r, ErrInvalidGrantRecovery)
		return
	}
	var body GrantRecoveryMutation
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&body) != nil || decoder.Decode(new(any)) != io.EOF {
		s.writeGrantRecoveryError(w, r, ErrInvalidGrantRecovery)
		return
	}
	result, err := repo.ApplyGrantRecovery(r.Context(), mustPrincipal(r.Context()), id, action, body)
	if err != nil {
		s.writeGrantRecoveryError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, result)
}
