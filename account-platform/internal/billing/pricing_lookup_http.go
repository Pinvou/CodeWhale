package billing

import (
	"context"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/google/uuid"
	"net/http"
	"strings"
	"time"
)

type AdminPriceVersion struct {
	ID                    uuid.UUID  `json:"price_version_id"`
	Model                 string     `json:"model"`
	InputPerMillion       int64      `json:"input_per_million"`
	OutputPerMillion      int64      `json:"output_per_million"`
	CachedInputPerMillion int64      `json:"cached_input_per_million"`
	LifecycleStatus       string     `json:"lifecycle_status"`
	Status                string     `json:"status"`
	EffectiveFrom         time.Time  `json:"effective_from"`
	EffectiveTo           *time.Time `json:"effective_to,omitempty"`
	PublishedAt           *time.Time `json:"-"`
	CreatedAt             time.Time  `json:"created_at"`
}

func (s *Service) adminPriceVersions(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.repository.(interface {
		AdminPriceVersions(context.Context, int) ([]AdminPriceVersion, error)
	})
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "PRICING_UNAVAILABLE", "pricing is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	items, err := repo.AdminPriceVersions(r.Context(), billingAdminLimit(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (r *PostgresRepository) AdminPriceVersions(ctx context.Context, limit int) ([]AdminPriceVersion, error) {
	rows, err := r.pool.Query(ctx, `SELECT id,model,input_per_million,output_per_million,cached_input_per_million,lifecycle_status,effective_from,effective_to,published_at,created_at FROM pricing_versions WHERE published_at IS NOT NULL ORDER BY effective_from DESC,id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]AdminPriceVersion, 0, limit)
	for rows.Next() {
		var item AdminPriceVersion
		if err := rows.Scan(&item.ID, &item.Model, &item.InputPerMillion, &item.OutputPerMillion, &item.CachedInputPerMillion, &item.LifecycleStatus, &item.EffectiveFrom, &item.EffectiveTo, &item.PublishedAt, &item.CreatedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return projectPublishedAdminPrices(items, r.now()), nil
}

func projectPublishedAdminPrices(items []AdminPriceVersion, now time.Time) []AdminPriceVersion {
	result := make([]AdminPriceVersion, 0, len(items))
	for _, item := range items {
		if item.PublishedAt == nil || item.LifecycleStatus == "DRAFT" {
			continue
		}
		item.Status = derivePriceLifecycleStatus(item.LifecycleStatus, item.EffectiveFrom, item.EffectiveTo, now)
		result = append(result, item)
	}
	return result
}

func (s *Service) currentPrice(w http.ResponseWriter, r *http.Request) {
	model := strings.TrimSpace(r.URL.Query().Get("model"))
	if model == "" {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "model is required", httpserver.RequestID(r.Context()))
		return
	}
	price, err := s.repository.CurrentPrice(r.Context(), model)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, price)
}

func (s *Service) priceByID(w http.ResponseWriter, r *http.Request) {
	priceID, err := uuid.Parse(r.PathValue("price_version_id"))
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid price version identifier", httpserver.RequestID(r.Context()))
		return
	}
	price, err := s.repository.PriceByID(r.Context(), priceID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, price)
}
