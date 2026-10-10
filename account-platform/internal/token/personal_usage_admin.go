package token

import (
	"net/http"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
)

func (s *Service) adminUsageRequests(w http.ResponseWriter, r *http.Request) {
	if s.paginatedUsage(w, r) {
		return
	}
	repository, ok := s.requireAdminRepository(w, r)
	if !ok {
		return
	}
	items, err := repository.AdminUsageRequests(r.Context(), tokenAdminLimit(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	summaries := make([]AdminUsageSummary, 0, len(items))
	for _, item := range items {
		summaries = append(summaries, redactAdminUsageSummary(item))
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, map[string]any{"items": summaries})
}
func (s *Service) adminUsageRequest(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAdminRepository(w, r)
	if !ok {
		return
	}
	usageID, ok := adminUUID(w, r, "usage_request_id")
	if !ok {
		return
	}
	result, err := repository.AdminUsageRequest(r.Context(), usageID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, redactAdminUsageDetail(result))
}
