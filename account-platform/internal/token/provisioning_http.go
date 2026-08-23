package token

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/google/uuid"
)

type provisioningOperations interface {
	List(context.Context, string, int) ([]RouteProvisioning, error)
	Detail(context.Context, uuid.UUID) (ProvisioningDetail, error)
	Repair(context.Context, uuid.UUID, uuid.UUID, string, string) (RouteProvisioning, error)
	ValidateModelReady(context.Context, uuid.UUID, string) error
	ContextEligible(context.Context, AuthorizationContext, uuid.UUID, string) (bool, string, error)
}

func (s *Service) provisioningOperations(w http.ResponseWriter, r *http.Request) (provisioningOperations, bool) {
	operations, ok := s.defaultRoutes.(provisioningOperations)
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "ROUTE_PROVISIONING_UNAVAILABLE", "managed route provisioning is unavailable", httpserver.RequestID(r.Context()))
	}
	return operations, ok
}

func (s *Service) adminProvisionings(w http.ResponseWriter, r *http.Request) {
	operations, ok := s.provisioningOperations(w, r)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	items, err := operations.List(r.Context(), r.URL.Query().Get("status"), limit)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Service) adminProvisioning(w http.ResponseWriter, r *http.Request) {
	operations, ok := s.provisioningOperations(w, r)
	if !ok {
		return
	}
	id, ok := adminUUID(w, r, "provisioning_id")
	if !ok {
		return
	}
	item, err := operations.Detail(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, item)
}

func (s *Service) adminRepairProvisioning(w http.ResponseWriter, r *http.Request) {
	operations, ok := s.provisioningOperations(w, r)
	if !ok {
		return
	}
	id, ok := adminUUID(w, r, "provisioning_id")
	if !ok {
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if api.DecodeJSON(w, r, &body) != nil || strings.TrimSpace(body.Reason) == "" {
		return
	}
	item, err := operations.Repair(r.Context(), id, tokenAdminActor(r), r.Header.Get("Idempotency-Key"), body.Reason)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusAccepted, item)
}
