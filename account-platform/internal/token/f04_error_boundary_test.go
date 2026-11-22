package token

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/google/uuid"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type failingCatalogAdapter struct{ failure error }

func (a failingCatalogAdapter) AdapterVersion() string { return "controlled-v1" }
func (a failingCatalogAdapter) DiscoverModels(context.Context, string, []byte) ([]DiscoveredModel, error) {
	return nil, a.failure
}
func TestF04CatalogSyncFailureIsObservableWithoutProviderDetails(t *testing.T) {
	const secret = "controlled-secret-must-not-appear"
	previous := slog.Default()
	defer slog.SetDefault(previous)
	for _, test := range []struct {
		name    string
		failure error
		code    string
	}{{"provider", errors.New(secret), "CATALOG_SYNC_FAILED"}, {"timeout", fmt.Errorf("%s: %w", secret, context.DeadlineExceeded), "CATALOG_SYNC_TIMEOUT"}, {"version", fmt.Errorf("%s: %w", secret, ErrCatalogVersionConflict), "CATALOG_SYNC_VERSION_CONFLICT"}, {"shutdown", context.Canceled, ""}} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			slog.SetDefault(observability.NewLogger(&output, observability.Config{Service: "token-test"}))
			s, err := NewCatalogSynchronizer(&catalogPreviewRepository{}, failingCatalogAdapter{test.failure}, "controlled", "https://invalid.example", []byte(secret), 4096, 1024, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if err = s.runOnce(context.Background()); !errors.Is(err, test.failure) {
				t.Fatal("error was swallowed", err)
			}
			if strings.Contains(output.String(), secret) || strings.Contains(output.String(), "invalid.example") {
				t.Fatal("provider or credential leaked", output.String())
			}
			if test.code == "" {
				if output.Len() != 0 {
					t.Fatal("normal shutdown reported failure", output.String())
				}
				return
			}
			var event map[string]any
			if err = json.Unmarshal(output.Bytes(), &event); err != nil {
				t.Fatal(err)
			}
			if event["event"] != "model_catalog_sync_failed" || event["error_code"] != test.code || event["result"] != "failure" || event["stage"] != "retry_pending" {
				t.Fatal("missing stable diagnostic", event)
			}
		})
	}
}

type missingModelDetailRepository struct{ Repository }

func (missingModelDetailRepository) adminModelDetail(context.Context, uuid.UUID) (AdminModelDetail, error) {
	return AdminModelDetail{}, ErrNotFound
}
func TestF04MissingModelThroughAuthenticatedManagementHTTPReturns404(t *testing.T) {
	actor := uuid.New()
	authority := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"allowed": true, "user_id": actor.String()})
	}))
	defer authority.Close()
	authorizer, err := authorization.NewClient(authority.URL, "controlled-service")
	if err != nil {
		t.Fatal(err)
	}
	s := NewService(nil, nil, nil).WithManagement(missingModelDetailRepository{}, nil, ProviderURLPolicy{}, "", nil).WithAuthorizer(authorizer)
	handler := s.ManagementHandler("controlled-service", func(context.Context, string) (identity.Principal, error) {
		return identity.Principal{Issuer: "fixture", Subject: actor.String()}, nil
	})
	request := httptest.NewRequest(http.MethodGet, "/admin/v1/models/"+uuid.NewString(), nil)
	request.Header.Set("Authorization", "Bearer controlled")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	var body struct {
		Code string `json:"code"`
	}
	if err = json.Unmarshal(response.Body.Bytes(), &body); err != nil || response.Code != 404 || body.Code != "TOKEN_NOT_FOUND" {
		t.Fatal("missing model was not explicit404", response.Code, response.Body.String(), err)
	}
}

// This tests the error mapper directly; it does not claim a registered usage route.
func TestF04AdminQueryErrorMappingMissingResourceReturns404(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	response := httptest.NewRecorder()
	writeAdminQueryError(response, request, fmt.Errorf("controlled wrapper: %w", ErrNotFound))
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || response.Code != 404 || body.Code != "TOKEN_NOT_FOUND" {
		t.Fatal(response.Code, response.Body.String(), err)
	}
}
