package token

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

func TestPostgresModelCatalogContextFailureIsNotPermissionDenial(t *testing.T) {
	s, _, bill := proxyFixture(t, "http://127.0.0.1:1")
	resolved, _ := s.contexts.Resolve(context.Background(), "")
	validContext, err := json.Marshal(map[string]any{"user_id": resolved.UserID, "session_id": "fixture", "context_type": "PERSONAL"})
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []struct {
		name       string
		status     int
		body       string
		disconnect bool
		want       int
		code       string
	}{
		{name: "unauthorized-context", status: 401, want: 403, code: "TOKEN_FORBIDDEN"},
		{name: "forbidden-context", status: 403, want: 403, code: "TOKEN_FORBIDDEN"},
		{name: "account-server-error", status: 500, want: 503, code: "MODEL_CATALOG_UNAVAILABLE"},
		{name: "account-unavailable", status: 503, want: 503, code: "MODEL_CATALOG_UNAVAILABLE"},
		{name: "malformed-context", status: 200, body: `{`, want: 503, code: "MODEL_CATALOG_UNAVAILABLE"},
		{name: "null-context", status: 200, body: `null`, want: 503, code: "MODEL_CATALOG_UNAVAILABLE"},
		{name: "missing-context-identity", status: 200, body: `{}`, want: 503, code: "MODEL_CATALOG_UNAVAILABLE"},
		{name: "missing-context-session", status: 200, body: fmt.Sprintf(`{"user_id":%q,"context_type":"PERSONAL"}`, resolved.UserID.String()), want: 503, code: "MODEL_CATALOG_UNAVAILABLE"},
		{name: "unknown-active-context", status: 200, body: fmt.Sprintf(`{"user_id":%q,"session_id":"fixture","context_type":"UNKNOWN"}`, resolved.UserID.String()), want: 503, code: "MODEL_CATALOG_UNAVAILABLE"},
		{name: "missing-active-organization", status: 200, body: fmt.Sprintf(`{"user_id":%q,"session_id":"fixture","context_type":"ORGANIZATION"}`, resolved.UserID.String()), want: 503, code: "MODEL_CATALOG_UNAVAILABLE"},
		{name: "second-context-value", status: 200, body: string(validContext) + ` false`, want: 503, code: "MODEL_CATALOG_UNAVAILABLE"},
		{name: "trailing-context-garbage", status: 200, body: string(validContext) + ` garbage`, want: 503, code: "MODEL_CATALOG_UNAVAILABLE"},
		{name: "oversized-context-whitespace", status: 200, body: string(validContext) + strings.Repeat(" ", 1<<20), want: 503, code: "MODEL_CATALOG_UNAVAILABLE"},
		{name: "disconnected-account", disconnect: true, want: 503, code: "MODEL_CATALOG_UNAVAILABLE"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			account := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/me/authorization-context" || r.Header.Get("Authorization") != "Bearer fixture-token" {
					t.Error("context request lost its production path or bearer")
				}
				w.WriteHeader(scenario.status)
				w.Write([]byte(scenario.body))
			}))
			defer account.Close()
			resolver, err := NewHTTPContextResolver(account.URL)
			if err != nil {
				t.Fatal(err)
			}
			if scenario.disconnect {
				account.Close()
			}
			s.contexts = resolver
			handler := s.Handler(func(context.Context, string) (identity.Principal, error) {
				return identity.Principal{Subject: uuid.NewString()}, nil
			})
			r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			r.Header.Set("Authorization", "Bearer fixture-token")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			var body map[string]json.RawMessage
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			var code string
			if err := json.Unmarshal(body["code"], &code); err != nil {
				t.Fatal(err)
			}
			if w.Code != scenario.want || code != scenario.code || body["data"] != nil || bill.holdID != uuid.Nil {
				t.Fatalf("context failure became success, billing or incorrect classification: HTTP %d code %q", w.Code, code)
			}
		})
	}
}

func catalogResponse(t *testing.T, s *Service) (*httptest.ResponseRecorder, []string) {
	t.Helper()
	w := httptest.NewRecorder()
	s.listModels(w, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if w.Code != http.StatusOK {
		return w, nil
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || len(body) != 1 || body["data"] == nil {
		t.Fatal("catalog returned unexpected metadata")
	}
	var entries []map[string]string
	if err := json.Unmarshal(body["data"], &entries); err != nil || entries == nil {
		t.Fatal("catalog data must be a non-null array")
	}
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		if len(entry) != 1 || entry["id"] == "" {
			t.Fatal("catalog exposed account or credential metadata")
		}
		ids = append(ids, entry["id"])
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("account-specific catalog is cacheable")
	}
	return w, ids
}

func catalogProvisioner(t *testing.T, s *Service, repo *PostgresRepository, models []string) *PostgresDefaultRouteProvisioner {
	t.Helper()
	p, err := NewDefaultRouteProvisioner(repo, s.providerURLs, DefaultRouteConfig{Provider: "fixture", BaseURL: "http://127.0.0.1:1", Secret: []byte("catalog-fixture-secret"), Models: models, Priority: 10})
	if err != nil {
		t.Fatal(err)
	}
	s.WithDefaultRoutes(p)
	return p
}

func TestPostgresModelCatalogFirstUseIsReadOnlyAndBoundToOwner(t *testing.T) {
	s, repo, bill := proxyFixture(t, "http://127.0.0.1:1")
	p := catalogProvisioner(t, s, repo, []string{"z-model", "a-model", "a-model"})
	w, ids := catalogResponse(t, s)
	if w.Code != 200 || !slices.Equal(ids, []string{"a-model", "model-test", "z-model"}) {
		t.Fatal("configured first-use models missing or not sorted/unique")
	}
	var accounts, credentials, routes, audits, usage int
	err := repo.pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM token_accounts),(SELECT count(*) FROM provider_credentials),(SELECT count(*) FROM routing_policies),(SELECT count(*) FROM token_audit_logs),(SELECT count(*) FROM usage_requests)`).Scan(&accounts, &credentials, &routes, &audits, &usage)
	if err != nil || accounts != 1 || credentials != 1 || routes != 1 || audits != 0 || usage != 0 || bill.holdID != uuid.Nil {
		t.Fatal("read-only discovery wrote data or billed")
	}
	resolved, _ := s.contexts.Resolve(context.Background(), "")
	if err := p.Ensure(context.Background(), resolved, uuid.New(), "a-model"); err != nil {
		t.Fatal(err)
	}
	w, ids = catalogResponse(t, s)
	if w.Code != 200 || !slices.Equal(ids, []string{"a-model", "model-test", "z-model"}) {
		t.Fatal("first execution provisioning changed discovery contract")
	}
	// A different authenticated owner gets only its own first-use projection.
	s.contexts = fixtureContexts{AuthorizationContext{UserID: uuid.New(), ContextType: "PERSONAL"}}
	w, ids = catalogResponse(t, s)
	if w.Code != 200 || !slices.Equal(ids, []string{"a-model", "z-model"}) {
		t.Fatal("catalog leaked another user's personal route")
	}
}

func TestPostgresModelCatalogPreservesAdministratorDecisions(t *testing.T) {
	for _, decision := range []string{"disabled-account", "invalid-credential", "unhealthy-route", "deleted-route"} {
		t.Run(decision, func(t *testing.T) {
			s, repo, _ := proxyFixture(t, "http://127.0.0.1:1")
			p := catalogProvisioner(t, s, repo, []string{"default-model"})
			ctx := context.Background()
			resolved, _ := s.contexts.Resolve(ctx, "")
			if err := p.Ensure(ctx, resolved, uuid.New(), "default-model"); err != nil {
				t.Fatal(err)
			}
			items, err := repo.ListCandidates(ctx, "default-model")
			if err != nil || len(items) != 1 {
				t.Fatal("fixture route missing")
			}
			c := items[0]
			switch decision {
			case "disabled-account":
				_, err = repo.SetAccountStatus(ctx, c.TokenAccountID, "DISABLED", resolved.UserID, "fixture")
			case "invalid-credential":
				_, err = repo.AdminRecordCredentialValidation(ctx, c.TokenAccountID, c.CredentialID, resolved.UserID, false)
			case "unhealthy-route":
				_, err = repo.UpsertRoute(ctx, c.TokenAccountID, "default-model", 10, "NONE", "UNHEALTHY", resolved.UserID)
			case "deleted-route":
				err = repo.AdminDeleteRoute(ctx, c.TokenAccountID, "default-model", resolved.UserID)
			}
			if err != nil {
				t.Fatal(err)
			}
			w, ids := catalogResponse(t, s)
			if w.Code != 200 || !slices.Equal(ids, []string{"model-test"}) {
				t.Fatal("catalog resurrected an administrator-disabled default")
			}
		})
	}
}

func TestPostgresModelCatalogUsesEachRouteHealthAndAllowedModels(t *testing.T) {
	s, repo, _ := proxyFixture(t, "http://127.0.0.1:1")
	ctx := context.Background()
	candidates, err := repo.ListCandidates(ctx, "model-test")
	if err != nil || len(candidates) != 1 {
		t.Fatal("fixture missing")
	}
	c := candidates[0]
	if _, err := repo.pool.Exec(ctx, `UPDATE provider_credentials SET allowed_models=ARRAY['model-test','unhealthy','no-route']`); err != nil {
		t.Fatal(err)
	}
	for _, model := range []string{"unhealthy"} {
		if _, err := repo.UpsertRoute(ctx, c.TokenAccountID, model, 10, "NONE", "UNHEALTHY", c.OwnerID); err != nil {
			t.Fatal(err)
		}
	}
	// Persisted legacy routes may refer to a model removed from the credential.
	if _, err := repo.pool.Exec(ctx, `INSERT INTO routing_policies(id,token_account_id,model,priority,default_scope,health_status,updated_at) VALUES($1,$2,'not-allowed',10,'NONE','HEALTHY',now())`, uuid.New(), c.TokenAccountID); err != nil {
		t.Fatal(err)
	}
	w, ids := catalogResponse(t, s)
	if w.Code != 200 || !slices.Equal(ids, []string{"model-test"}) {
		t.Fatal("other credential models inherited healthy route")
	}
	if _, err := repo.UpsertRoute(ctx, c.TokenAccountID, "unhealthy", 10, "NONE", "HEALTHY", c.OwnerID); err != nil {
		t.Fatal(err)
	}
	w, ids = catalogResponse(t, s)
	if w.Code != 200 || !slices.Equal(ids, []string{"model-test", "unhealthy"}) {
		t.Fatal("healthy allowed route absent")
	}
}

func TestPostgresModelCatalogOrganizationSharingUsesCurrentAuthority(t *testing.T) {
	s, repo, _ := proxyFixture(t, "http://127.0.0.1:1")
	ctx := context.Background()
	org, unit, otherOrg, otherUnit, user := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	var active atomic.Bool
	var authorityUnavailable atomic.Bool
	var unitChecks atomic.Int32
	var unitSuffix atomic.Value
	unitSuffix.Store("")
	var unitBodyOverride atomic.Value
	unitBodyOverride.Store("")
	active.Store(true)
	accountServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/me/authorization-context" {
			json.NewEncoder(w).Encode(map[string]any{"user_id": user, "session_id": "fixture", "context_type": "ORGANIZATION", "organization_id": org, "unit_ids": []uuid.UUID{unit}, "ancestor_unit_ids": []uuid.UUID{unit}})
			return
		}
		unitChecks.Add(1)
		if authorityUnavailable.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if body := unitBodyOverride.Load().(string); body != "" {
			w.Write([]byte(body))
			return
		}
		status := "DISABLED"
		if active.Load() {
			status = "ACTIVE"
		}
		json.NewEncoder(w).Encode(map[string]any{"items": []any{map[string]any{"unit_id": unit, "organization_id": org, "status": status}, map[string]any{"unit_id": otherUnit, "organization_id": org, "status": "ACTIVE"}}})
		w.Write([]byte(unitSuffix.Load().(string)))
	}))
	defer accountServer.Close()
	var err error
	s.contexts, err = NewHTTPContextResolver(accountServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		model, owner, scope string
		organization, unit  uuid.UUID
		member              bool
	}{
		{"org-all", "ORG", "ORG_ALL", org, uuid.Nil, false},
		{"foreign-org", "ORG", "ORG_ALL", otherOrg, uuid.Nil, false},
		{"member-allowed", "ORG", "EXPLICIT_MEMBERS", org, uuid.Nil, true},
		{"member-denied", "ORG", "EXPLICIT_MEMBERS", org, uuid.Nil, false},
		{"unit-only", "ORG_UNIT", "OWNER_UNIT_ONLY", org, unit, false},
		{"unit-subtree", "ORG_UNIT", "OWNER_SUBTREE", org, unit, false},
		{"foreign-unit", "ORG_UNIT", "OWNER_UNIT_ONLY", org, otherUnit, false},
	} {
		ownerID := row.organization
		var unitID *uuid.UUID
		if row.owner == "ORG_UNIT" {
			ownerID = row.unit
			unitID = &row.unit
		}
		account, err := repo.CreateAccount(ctx, CreateAccountRequest{OwnerType: row.owner, OwnerID: ownerID, OwnerOrganization: &row.organization, OwnerUnit: unitID, WalletID: uuid.New(), Name: row.model, ShareScope: row.scope, DefaultPriority: 10, CreatedBy: user})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := repo.RotateCredential(ctx, account.ID, user, "fixture", "http://127.0.0.1:1", []byte("fixture-secret"), []string{row.model}); err != nil {
			t.Fatal(err)
		}
		if row.member {
			if _, err := repo.pool.Exec(ctx, `INSERT INTO token_explicit_members(token_account_id,user_id,created_at) VALUES($1,$2,now())`, account.ID, user); err != nil {
				t.Fatal(err)
			}
		}
	}
	w, ids := catalogResponse(t, s)
	if w.Code != 200 || !slices.Equal(ids, []string{"member-allowed", "org-all", "unit-only", "unit-subtree"}) {
		t.Fatalf("sharing projection mismatch: status=%d models=%v", w.Code, ids)
	}
	active.Store(false)
	w, ids = catalogResponse(t, s)
	if w.Code != 200 || !slices.Equal(ids, []string{"member-allowed", "org-all"}) {
		t.Fatal("inactive unit routes remained discoverable")
	}
	active.Store(true)
	authorityUnavailable.Store(true)
	w, _ = catalogResponse(t, s)
	if w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), `"data"`) {
		t.Fatal("unavailable unit authority became a partial successful catalog")
	}
	if unitChecks.Load() != 3 {
		t.Fatal("unit authority was repeatedly queried per route or queried for a nonshared route")
	}
	authorityUnavailable.Store(false)
	for _, malformed := range []struct{ name, suffix string }{
		{"second-unit-value", " false"},
		{"trailing-unit-garbage", " garbage"},
		{"oversized-unit-whitespace", strings.Repeat(" ", 2<<20)},
	} {
		t.Run(malformed.name, func(t *testing.T) {
			unitSuffix.Store(malformed.suffix)
			w, _ := catalogResponse(t, s)
			if w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), `"data"`) {
				t.Fatal("incomplete or malformed unit authority became a successful catalog")
			}
		})
	}
	for _, shape := range []struct {
		name, body string
		want       int
	}{
		{"null-unit-list", `null`, http.StatusServiceUnavailable},
		{"missing-unit-list", `{}`, http.StatusServiceUnavailable},
		{"null-unit-items", `{"items":null}`, http.StatusServiceUnavailable},
		{"valid-empty-unit-list", `{"items":[]}`, http.StatusOK},
	} {
		t.Run(shape.name, func(t *testing.T) {
			unitBodyOverride.Store(shape.body)
			w, ids := catalogResponse(t, s)
			if w.Code != shape.want || (shape.want == http.StatusOK && !slices.Equal(ids, []string{"member-allowed", "org-all"})) {
				t.Fatal("malformed units became successful or a valid empty inventory stopped being authoritative")
			}
		})
	}
}

func TestPostgresModelCatalogCandidateOverflowIsNotTruncated(t *testing.T) {
	s, repo, _ := proxyFixture(t, "http://127.0.0.1:1")
	ctx := context.Background()
	if _, err := repo.pool.Exec(ctx, `UPDATE provider_credentials SET allowed_models=ARRAY(SELECT 'catalog-'||i FROM generate_series(1,16385) i)`); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.pool.Exec(ctx, `INSERT INTO routing_policies(id,token_account_id,model,priority,default_scope,health_status,updated_at) SELECT gen_random_uuid(),ta.id,'catalog-'||i,10,'NONE','HEALTHY',now() FROM token_accounts ta CROSS JOIN generate_series(1,16385) i`); err != nil {
		t.Fatal(err)
	}
	resolved, _ := s.contexts.Resolve(ctx, "")
	items, err := repo.ListModelCandidates(ctx, resolved)
	if err != nil || len(items) != maximumCatalogCandidates+1 {
		t.Fatal("query did not return overflow sentinel")
	}
	w, _ := catalogResponse(t, s)
	if w.Code != 503 || strings.Contains(w.Body.String(), `"data"`) {
		t.Fatal("candidate cap silently truncated authorized catalog")
	}
}

func TestPostgresModelCatalogBoundsFailWithoutPartialSuccess(t *testing.T) {
	for _, kind := range []string{"models", "bytes"} {
		t.Run(kind, func(t *testing.T) {
			s, repo, _ := proxyFixture(t, "http://127.0.0.1:1")
			models := []string{strings.Repeat("m", maximumCatalogBytes)}
			if kind == "models" {
				models = make([]string, maximumCatalogModels+1)
				for i := range models {
					models[i] = fmt.Sprintf("model-%05d", i)
				}
			}
			catalogProvisioner(t, s, repo, models)
			w, _ := catalogResponse(t, s)
			if w.Code != 503 || !strings.Contains(w.Body.String(), "MODEL_CATALOG_UNAVAILABLE") || strings.Contains(w.Body.String(), `"data"`) {
				t.Fatal("oversized catalog returned partial success")
			}
		})
	}
}

func TestPostgresModelCatalogHTTPRequiresAuthenticationAndFailsClosed(t *testing.T) {
	s, repo, _ := proxyFixture(t, "http://127.0.0.1:1")
	handler := s.Handler(func(context.Context, string) (identity.Principal, error) {
		return identity.Principal{Subject: uuid.NewString()}, nil
	})
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/v1/models", nil))
	if w.Code != 401 {
		t.Fatal("unauthenticated model catalog exposed")
	}
	r := httptest.NewRequest("GET", "/v1/models", nil)
	r.Header.Set("Authorization", "Bearer fixture-token")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "model-test") {
		t.Fatal("production authenticated route not wired")
	}
	if _, err := repo.pool.Exec(context.Background(), `DROP TABLE routing_policies`); err != nil {
		t.Fatal(err)
	}
	w, _ = catalogResponse(t, s)
	if w.Code != 503 || strings.Contains(w.Body.String(), "routing_policies") {
		t.Fatal("database failure leaked internals or became empty success")
	}
}
