package account

import (
	"net/http"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
)

func (s *Service) internalClientCompatibility(w http.ResponseWriter, _ *http.Request) {
	serviceVersion, contractVersion := s.serviceVersion, s.clientContractVersion
	if serviceVersion == "" {
		serviceVersion = "development"
	}
	if contractVersion == "" {
		contractVersion = "account-client-v1"
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{
		"service":          "account",
		"service_version":  serviceVersion,
		"contract_version": contractVersion,
	})
}
