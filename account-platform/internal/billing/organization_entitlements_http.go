package billing

import (
	"context"
	"net/http"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/google/uuid"
)

func (s *Service) organizationEntitlementWallet(w http.ResponseWriter, r *http.Request) (uuid.UUID, uuid.UUID, bool) {
	organizationID, organizationErr := uuid.Parse(r.PathValue("organization_id"))
	walletID, walletErr := uuid.Parse(r.URL.Query().Get("wallet_id"))
	if organizationErr != nil || walletErr != nil || organizationID == uuid.Nil || walletID == uuid.Nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid organization wallet lookup", httpserver.RequestID(r.Context()))
		return uuid.Nil, uuid.Nil, false
	}
	wallet, err := s.repository.GetWallet(r.Context(), walletID)
	if err != nil {
		s.writeError(w, r, err)
		return uuid.Nil, uuid.Nil, false
	}
	if wallet.OwnerType != "ORGANIZATION" || wallet.OwnerID != organizationID {
		s.writeError(w, r, ErrConflict)
		return uuid.Nil, uuid.Nil, false
	}
	return organizationID, walletID, true
}

func (s *Service) listOrganizationEntitlementProducts(w http.ResponseWriter, r *http.Request) {
	_, walletID, ok := s.organizationEntitlementWallet(w, r)
	if !ok {
		return
	}
	wallet, err := s.repository.GetWallet(r.Context(), walletID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if wallet.Status != "ACTIVE" {
		s.writeError(w, r, ErrWalletUnavailable)
		return
	}
	s.listRechargeCatalog(w, r, false)
}

func (s *Service) listOrganizationEntitlementPackages(w http.ResponseWriter, r *http.Request) {
	organizationID, walletID, ok := s.organizationEntitlementWallet(w, r)
	if !ok {
		return
	}
	repository, ok := s.repository.(interface {
		ListOrganizationPackages(context.Context, uuid.UUID, uuid.UUID, int) ([]EntitlementPackage, PackageSummary, error)
	})
	if !ok {
		s.writeError(w, r, ErrConflict)
		return
	}
	items, summary, err := repository.ListOrganizationPackages(r.Context(), organizationID, walletID, 100)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "summary": summary})
}
