package token

import (
	"net/http"
	"strconv"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
)

func (s *Service) catalogRepository(w http.ResponseWriter, r *http.Request) (CatalogRepository, bool) {
	repository, ok := s.repository.(CatalogRepository)
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "MODEL_CATALOG_UNAVAILABLE", "the model catalog is unavailable", httpserver.RequestID(r.Context()))
	}
	return repository, ok
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
