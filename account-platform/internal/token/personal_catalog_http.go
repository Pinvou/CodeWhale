package token

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"

	"github.com/google/uuid"
)

func (s *Service) listCatalogModels(w http.ResponseWriter, r *http.Request) {
	resolved, ok := s.resolveModelUsagePermission(w, r, "personal.models.read", "org.models.read")
	if !ok {
		return
	}
	repository, ok := s.catalogRepository(w, r)
	if !ok {
		return
	}
	version, items, err := repository.ListCatalog(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	// Route provisioning used to start only after a chat request. Clients correctly
	// refuse to select a model until its route is READY, so a new scope could never
	// issue that first request. Catalog discovery is the earliest authenticated
	// scope-establishment point shared by existing clients; reconcile the idempotent
	// desired route here when the same context also has model-invoke permission.
	// The request remains bounded and never performs an upstream inference call.
	queued := false
	if canPrepareCatalogRoutes(resolved) && s.defaultRoutes != nil {
		if walletID, walletErr := s.activeWallet(r.Context(), resolved); walletErr == nil {
			queued = s.ensureCatalogRoutes(r.Context(), resolved, walletID, items)
		}
	}
	publicItems := make([]map[string]any, 0, len(items))
	operations, supportsEligibility := s.defaultRoutes.(provisioningOperations)
	if queued && supportsEligibility {
		waitForCatalogRoutes(r.Context(), operations, resolved, items, 2*time.Second)
	}
	for _, item := range items {
		payload, _ := json.Marshal(item)
		var public map[string]any
		_ = json.Unmarshal(payload, &public)
		eligible, reason := false, "ROUTE_PROVISIONING_UNAVAILABLE"
		if supportsEligibility {
			var eligibilityErr error
			eligible, reason, eligibilityErr = operations.ContextEligible(r.Context(), resolved, item.ModelID, item.ProtocolCode)
			if eligibilityErr != nil {
				reason = "ROUTE_PROVISIONING_UNAVAILABLE"
			}
		}
		public["context_eligible"] = eligible
		public["eligibility_reason"] = reason
		public["context_version"] = resolved.ContextVersion
		public["authorization_version"] = resolved.AuthorizationVersion
		publicItems = append(publicItems, public)
	}
	w.Header().Set("Cache-Control", "private, no-cache")
	w.Header().Set("ETag", catalogContextETag(version, resolved.ContextVersion, resolved.AuthorizationVersion))
	api.WriteJSON(w, http.StatusOK, map[string]any{"catalog_version": version, "context_version": resolved.ContextVersion, "authorization_version": resolved.AuthorizationVersion, "items": publicItems})
}

func canPrepareCatalogRoutes(resolved AuthorizationContext) bool {
	if resolved.ContextType == "PERSONAL" {
		return hasPermission(resolved, "personal.model.invoke")
	}
	if resolved.ContextType == "ORGANIZATION" {
		return hasPermission(resolved, "org.model.invoke")
	}
	return false
}

func (s *Service) ensureCatalogRoutes(ctx context.Context, resolved AuthorizationContext, walletID uuid.UUID, items []CatalogModel) bool {
	queued := false
	for _, item := range items {
		if !item.SourceAvailable || item.LifecycleStatus == "REMOVED" {
			continue
		}
		provisioning, err := s.defaultRoutes.Ensure(ctx, resolved, walletID, item)
		if err == nil && provisioning.Status != "READY" {
			queued = true
		}
	}
	return queued
}

func waitForCatalogRoutes(ctx context.Context, operations provisioningOperations, resolved AuthorizationContext, items []CatalogModel, maximum time.Duration) {
	deadline := time.NewTimer(maximum)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer deadline.Stop()
	defer ticker.Stop()
	for {
		allReady := true
		for _, item := range items {
			if !item.SourceAvailable || item.LifecycleStatus == "REMOVED" {
				continue
			}
			eligible, reason, err := operations.ContextEligible(ctx, resolved, item.ModelID, item.ProtocolCode)
			if err != nil {
				allReady = false
				break
			}
			if !eligible && (reason == "ROUTE_PROVISIONING_REQUIRED" || reason == "ROUTE_PROVISIONING_PENDING" || reason == "ROUTE_PROVISIONING_FAILED") {
				allReady = false
				break
			}
		}
		if allReady {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-deadline.C:
			return
		case <-ticker.C:
		}
	}
}

func catalogContextETag(catalogVersion, contextVersion, authorizationVersion int64) string {
	return `"model-catalog-` + strconv.FormatInt(catalogVersion, 10) + `-context-` + strconv.FormatInt(contextVersion, 10) + `-authorization-` + strconv.FormatInt(authorizationVersion, 10) + `"`
}
