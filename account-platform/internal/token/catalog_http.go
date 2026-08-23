package token

import (
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
	repository, ok := s.catalogRepository(w, r)
	if !ok {
		return
	}
	version, items, err := repository.ListCatalog(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("ETag", catalogETag(version))
	api.WriteJSON(w, http.StatusOK, map[string]any{"catalog_version": version, "items": items})
}

func (s *Service) adminCatalogModels(w http.ResponseWriter, r *http.Request) {
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
	version, err := repository.RollbackCatalog(r.Context(), body.SourceCatalogVersion, body.ExpectedCatalogVersion, body.IdempotencyKey, body.Reason, tokenAdminActor(r), s.upstream.Protocols())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"catalog_version": version, "rollback_source_version": body.SourceCatalogVersion})
}

func (s *Service) adminSyncCatalog(w http.ResponseWriter, r *http.Request) {
	if s.catalogSync == nil {
		api.WriteError(w, http.StatusServiceUnavailable, "MODEL_CATALOG_SYNC_UNAVAILABLE", "model catalog synchronization is not configured", httpserver.RequestID(r.Context()))
		return
	}
	version, changed, err := s.catalogSync.Sync(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"catalog_version": version, "changed": changed})
}
