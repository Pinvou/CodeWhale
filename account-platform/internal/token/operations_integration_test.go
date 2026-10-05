package token

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

func TestPostgresDefaultProvisioningPreservesAdministratorDecisions(t *testing.T) {
	for _, decision := range []string{"disabled-account", "invalid-credential", "unhealthy-route", "deleted-route"} {
		t.Run(decision, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("disabled platform route reached the provider") }))
			defer upstream.Close()
			s, repo, bill := proxyFixture(t, upstream.URL)
			ctx := context.Background()
			resolved, _ := s.contexts.Resolve(ctx, "")
			provisioner, err := NewDefaultRouteProvisioner(repo, s.providerURLs, DefaultRouteConfig{Provider: "test-provider", BaseURL: upstream.URL, Secret: []byte("fixture-default-key"), Models: []string{"default-model"}, Priority: 10})
			if err != nil {
				t.Fatal(err)
			}
			wallet := uuid.New()
			if err := provisioner.Ensure(ctx, resolved, wallet, "default-model"); err != nil {
				t.Fatal(err)
			}
			candidates, err := repo.ListCandidates(ctx, "default-model")
			if err != nil || len(candidates) != 1 {
				t.Fatal("initial default provisioning did not produce an eligible route")
			}
			candidate := candidates[0]
			s.WithDefaultRoutes(provisioner)
			s.wallets = operationWallet{wallet}
			switch decision {
			case "disabled-account":
				_, err = repo.SetAccountStatus(ctx, candidate.TokenAccountID, "DISABLED", resolved.UserID, "controlled maintenance")
			case "invalid-credential":
				_, err = repo.AdminRecordCredentialValidation(ctx, candidate.TokenAccountID, candidate.CredentialID, resolved.UserID, false)
			case "unhealthy-route":
				_, err = repo.UpsertRoute(ctx, candidate.TokenAccountID, "default-model", 10, "NONE", "UNHEALTHY", resolved.UserID)
			case "deleted-route":
				err = repo.AdminDeleteRoute(ctx, candidate.TokenAccountID, "default-model", resolved.UserID)
			}
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			s.chatCompletions(response, modelRequest(`{"model":"default-model","messages":[],"max_tokens":1}`))
			if response.Code != http.StatusUnprocessableEntity || bill.holdID != uuid.Nil {
				t.Fatal("automatic provisioning overrode the administrator decision or reserved funds")
			}
			items, err := repo.ListCandidates(ctx, "default-model")
			if err != nil {
				t.Fatal(err)
			}
			if decision == "unhealthy-route" {
				if len(items) != 1 || items[0].HealthStatus != "UNHEALTHY" {
					t.Fatal("administrator route health was reset")
				}
			} else if len(items) != 0 {
				t.Fatal("administrator decision did not survive the actual proxy path")
			}
			var audits int
			if err := repo.pool.QueryRow(ctx, `SELECT count(*) FROM token_audit_logs`).Scan(&audits); err != nil || audits != 1 {
				t.Fatal("operator action was not atomically audited")
			}
		})
	}
}

type operationWallet struct{ id uuid.UUID }

func (w operationWallet) Ensure(context.Context, string, uuid.UUID) (uuid.UUID, error) {
	return w.id, nil
}

func TestPostgresAdminUsageListKeepsOperationsEvidenceAndExactMoney(t *testing.T) {
	repo, usage := settlementFixture(t)
	ctx := context.Background()
	if err := repo.RecordUpstreamRequest(ctx, usage.ID, "fixture-newapi-request"); err != nil {
		t.Fatal(err)
	}
	if err := repo.MarkReview(ctx, usage.ID, "fixture_uncertain", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.pool.Exec(ctx, `UPDATE usage_requests SET reconciliation_last_error='payment provider unavailable',maximum_microcredits=9007199254740993 WHERE id=$1`, usage.ID); err != nil {
		t.Fatal(err)
	}
	s := NewService(repo, nil, nil, nil, ProviderURLPolicy{}, nil, nil, 100).WithAuthorizer(tokenTestAuthorizer(t))
	handler := s.Handler(func(context.Context, string) (identity.Principal, error) {
		return identity.Principal{Issuer: "https://fixture-authority.invalid", Subject: "fixture-platform-operator", RealmRoles: []string{"platform-admin"}}, nil
	})
	for _, path := range []string{"/admin/v1/usage-requests", "/admin/v1/usage-requests/" + usage.ID.String()} {
		request := httptest.NewRequest("GET", path, nil)
		request.Header.Set("Authorization", "Bearer fixture-auth")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		var payload map[string]any
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &payload) != nil {
			t.Fatal("admin usage HTTP read failed")
		}
		if items, ok := payload["items"].([]any); ok {
			payload = items[0].(map[string]any)
		}
		if payload["maximum_microcredits"] != "9007199254740993" || payload["actual_microcredits"] != nil || payload["newapi_request_id"] != "fixture-newapi-request" || payload["reconciliation_last_error"] != "payment provider unavailable" {
			t.Fatal("admin usage evidence was omitted or money rounded")
		}
	}
}

func TestPostgresTokenAdminInvalidJSONReturns400WithoutAudit(t *testing.T) {
	s, repo, _ := proxyFixture(t, "http://127.0.0.1:1")
	s.WithAuthorizer(tokenTestAuthorizer(t))
	ctx := context.Background()
	candidates, err := repo.ListCandidates(ctx, "model-test")
	if err != nil {
		t.Fatal(err)
	}
	id := candidates[0].TokenAccountID.String()
	handler := s.Handler(func(context.Context, string) (identity.Principal, error) {
		return identity.Principal{Issuer: "https://fixture-authority.invalid", Subject: "fixture-platform-operator", RealmRoles: []string{"platform-admin"}}, nil
	})
	for _, route := range []struct{ method, path string }{{"PATCH", "/admin/v1/token-accounts/" + id + "/status"}, {"PUT", "/admin/v1/token-accounts/" + id + "/routes"}, {"DELETE", "/admin/v1/token-accounts/" + id + "/routes"}, {"POST", "/admin/v1/usage-requests/" + uuid.NewString() + "/retry-reconciliation"}} {
		for _, body := range []string{"{", `{"unexpected":true}`, `{"model":[]}`, `{} {}`, `{"reason":"` + strings.Repeat("x", 1<<20) + `"}`} {
			request := httptest.NewRequest(route.method, route.path, strings.NewReader(body))
			request.Header.Set("Authorization", "Bearer fixture-auth")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != 400 || !strings.Contains(response.Body.String(), `"code":"INVALID_REQUEST"`) {
				t.Fatal("invalid operator JSON did not fail closed")
			}
		}
	}
	var count int
	if err := repo.pool.QueryRow(ctx, `SELECT count(*) FROM token_audit_logs`).Scan(&count); err != nil || count != 0 {
		t.Fatal("invalid JSON wrote audit state")
	}
}
