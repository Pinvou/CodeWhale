package token

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/google/uuid"
)

func (s *Service) catalogRepository(w http.ResponseWriter, r *http.Request) (CatalogRepository, bool) {
	repository, ok := s.repository.(CatalogRepository)
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "MODEL_CATALOG_UNAVAILABLE", "the model catalog is unavailable", httpserver.RequestID(r.Context()))
	}
	return repository, ok
}

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

func (s *Service) adminCatalogModels(w http.ResponseWriter, r *http.Request) {
	if s.paginatedModels(w, r) {
		return
	}
	repository, ok := s.catalogRepository(w, r)
	if !ok {
		return
	}
	version, items, err := repository.AdminCatalogModels(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("ETag", catalogETag(version))
	api.WriteJSON(w, http.StatusOK, map[string]any{"catalog_version": version, "items": items})
}

func catalogETag(version int64) string {
	return `"model-catalog-` + strconv.FormatInt(version, 10) + `"`
}

func catalogContextETag(catalogVersion, contextVersion, authorizationVersion int64) string {
	return `"model-catalog-` + strconv.FormatInt(catalogVersion, 10) + `-context-` + strconv.FormatInt(contextVersion, 10) + `-authorization-` + strconv.FormatInt(authorizationVersion, 10) + `"`
}

func (s *Service) adminSaveCatalogDraft(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.catalogRepository(w, r)
	if !ok {
		return
	}
	modelID, ok := adminUUID(w, r, "model_id")
	if !ok {
		return
	}
	var body CatalogDraftInput
	if err := api.DecodeJSON(w, r, &body); err != nil {
		return
	}
	draft, err := repository.SaveCatalogDraft(r.Context(), modelID, body, tokenAdminActor(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, draft)
}

func (s *Service) adminPublishCatalogDraft(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.catalogRepository(w, r)
	if !ok {
		return
	}
	modelID, ok := adminUUID(w, r, "model_id")
	if !ok {
		return
	}
	var body struct {
		ExpectedCatalogVersion int64  `json:"expected_catalog_version"`
		ExpectedDraftVersion   int64  `json:"expected_draft_version"`
		IdempotencyKey         string `json:"idempotency_key"`
		Reason                 string `json:"reason"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil {
		return
	}
	item, err := repository.PublishCatalogDraft(r.Context(), modelID, body.ExpectedCatalogVersion, body.ExpectedDraftVersion, body.IdempotencyKey, body.Reason, tokenAdminActor(r), s.upstream.Protocols())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, item)
}

func (s *Service) adminTransitionCatalogModel(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.catalogRepository(w, r)
	if !ok {
		return
	}
	modelID, ok := adminUUID(w, r, "model_id")
	if !ok {
		return
	}
	var body struct {
		ExpectedCatalogVersion int64      `json:"expected_catalog_version"`
		TargetStatus           string     `json:"target_status"`
		EffectiveAt            *time.Time `json:"effective_at"`
		ReplacementModelID     *uuid.UUID `json:"replacement_model_id"`
		NewSessionCutoffAt     *time.Time `json:"new_session_cutoff_at"`
		ContinuationCutoffAt   *time.Time `json:"continuation_cutoff_at"`
		ForceCancelInflight    bool       `json:"force_cancel_inflight"`
		IdempotencyKey         string     `json:"idempotency_key"`
		Reason                 string     `json:"reason"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil {
		return
	}
	effective := time.Time{}
	if body.EffectiveAt != nil {
		effective = *body.EffectiveAt
	}
	item, err := repository.TransitionCatalogModel(r.Context(), modelID, CatalogLifecycleInput{ExpectedCatalogVersion: body.ExpectedCatalogVersion, TargetStatus: body.TargetStatus, EffectiveAt: effective, ReplacementModelID: body.ReplacementModelID, NewSessionCutoffAt: body.NewSessionCutoffAt, ContinuationCutoffAt: body.ContinuationCutoffAt, ForceCancelInflight: body.ForceCancelInflight, IdempotencyKey: body.IdempotencyKey, Reason: body.Reason}, tokenAdminActor(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, item)
}

func (s *Service) adminRollbackCatalog(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.catalogRepository(w, r)
	if !ok {
		return
	}
	var body struct {
		SourceCatalogVersion   int64  `json:"source_catalog_version"`
		ExpectedCatalogVersion int64  `json:"expected_catalog_version"`
		IdempotencyKey         string `json:"idempotency_key"`
		Reason                 string `json:"reason"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil {
		return
	}
	models, err := repository.CatalogModelsAtVersion(r.Context(), body.SourceCatalogVersion)
	if err != nil || len(models) == 0 {
		s.writeError(w, r, ErrModelUnavailable)
		return
	}
	operations, ok := s.provisioningOperations(w, r)
	if !ok {
		return
	}
	for _, model := range models {
		if _, priceErr := s.billing.Price(r.Context(), model.ModelID.String()); priceErr != nil {
			s.writeError(w, r, ErrModelUnavailable)
			return
		}
		if readyErr := operations.ValidateModelReady(r.Context(), model.ModelID, model.ProtocolCode); readyErr != nil {
			s.writeError(w, r, readyErr)
			return
		}
	}
	version, err := repository.RollbackCatalog(r.Context(), body.SourceCatalogVersion, body.ExpectedCatalogVersion, body.IdempotencyKey, body.Reason, tokenAdminActor(r), s.upstream.Protocols())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"catalog_version": version, "rollback_source_version": body.SourceCatalogVersion})
}

func (s *Service) adminSyncCatalog(w http.ResponseWriter, r *http.Request) {
	// The historical direct endpoint cannot bind the frozen command precheck to
	// the publication transaction. Keep it fail-closed during PRE_ACTIVATION;
	// APT-138 activates only /commands with the governed sync adapter.
	api.WriteError(w, http.StatusServiceUnavailable, "TOKEN_COMMAND_GOVERNANCE_INACTIVE", "submit catalog synchronization through the governed command API after activation", httpserver.RequestID(r.Context()))
}
