package token

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
)

// Gateway already owns the provider allowlist and CA trust. Portal can inspect
// its read-only status without gaining access to the private model/log port.
func (s *Service) WithNewAPIStatus(baseURL string) (*Service, error) {
	if err := s.providerURLs.Validate(baseURL); err != nil {
		return nil, errors.New("invalid New API status configuration")
	}
	target, err := url.Parse(baseURL)
	if err != nil || target.RawQuery != "" || target.Fragment != "" {
		return nil, errors.New("invalid New API status configuration")
	}
	target.Path = strings.TrimRight(target.Path, "/") + "/api/status"
	s.newAPIStatusURL = target
	s.newAPIStatusClient = observability.NewHTTPClient(5 * time.Second)
	return s, nil
}

func (s *Service) adminNewAPIStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	status := http.StatusServiceUnavailable
	healthy := false
	if s.newAPIStatusURL != nil && s.newAPIStatusClient != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, s.newAPIStatusURL.String(), nil)
		if err == nil {
			request.Header.Set("Accept", "application/json")
			response, err := s.newAPIStatusClient.Do(request)
			if err == nil {
				defer response.Body.Close()
				var payload struct {
					Success *bool `json:"success"`
				}
				body, readErr := io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
				healthy = response.StatusCode == http.StatusOK && readErr == nil && len(body) <= 64<<10 && json.Unmarshal(body, &payload) == nil && payload.Success != nil && *payload.Success
			}
		}
	}
	if healthy {
		status = http.StatusOK
	}
	api.WriteJSON(w, status, map[string]bool{"success": healthy})
}
