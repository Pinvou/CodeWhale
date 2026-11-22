package token

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

// Embed only the required base interface: these optional capabilities must be
// discovered by the real handler, not supplied by a replacement handler.
type protocolAdminMissingRepository struct{ Repository }

type protocolAdminHTTPRepository struct {
	Repository
	protocol ProtocolDefinition
	formula  ProtocolFormulaVersion
	err      error
	calls    int
	gotID    uuid.UUID
}

func (r *protocolAdminHTTPRepository) ListProtocolsAndFormulas(context.Context) ([]ProtocolDefinition, []ProtocolFormulaVersion, error) {
	r.calls++
	return []ProtocolDefinition{r.protocol}, []ProtocolFormulaVersion{r.formula}, r.err
}

func (r *protocolAdminHTTPRepository) FormulaDraft(_ context.Context, id uuid.UUID) (ProtocolFormulaVersion, error) {
	r.calls++
	r.gotID = id
	return r.formula, r.err
}

func TestProtocolAdminRegisteredHTTPErrorResponses(t *testing.T) {
	actor := uuid.New()
	id := uuid.New()
	unit := uuid.New()
	for _, test := range []struct {
		name, path, code           string
		status, calls              int
		missing, denied, anonymous bool
		failure                    error
	}{
		{name: "protocols missing capability", path: "/protocols", status: 503, code: "ADMIN_UNAVAILABLE", missing: true},
		{name: "preview missing capability", path: "/formulas/" + id.String() + "/preview", status: 503, code: "ADMIN_UNAVAILABLE", missing: true},
		{name: "preview missing capability precedes invalid UUID", path: "/formulas/not-a-uuid/preview", status: 503, code: "ADMIN_UNAVAILABLE", missing: true},
		{name: "preview invalid UUID", path: "/formulas/not-a-uuid/preview", status: 400, code: "INVALID_REQUEST"},
		{name: "protocols success", path: "/protocols", status: 200, calls: 1},
		{name: "preview success", path: "/formulas/" + id.String() + "/preview", status: 200, calls: 1},
		{name: "protocols backend failure", path: "/protocols", status: 500, code: "INTERNAL_ERROR", calls: 1, failure: errors.New("controlled-backend-detail")},
		{name: "preview backend failure", path: "/formulas/" + id.String() + "/preview", status: 500, code: "INTERNAL_ERROR", calls: 1, failure: errors.New("controlled-backend-detail")},
		{name: "preview not found", path: "/formulas/" + id.String() + "/preview", status: 404, code: "TOKEN_NOT_FOUND", calls: 1, failure: ErrNotFound},
		{name: "protocols denied", path: "/protocols", status: 403, denied: true},
		{name: "invalid preview denied", path: "/formulas/not-a-uuid/preview", status: 403, denied: true},
		{name: "protocols anonymous", path: "/protocols", status: 401, anonymous: true},
		{name: "invalid preview anonymous", path: "/formulas/not-a-uuid/preview", status: 401, anonymous: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var authorityCalls atomic.Int32
			authority := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				authorityCalls.Add(1)
				var check struct{ Permission string }
				if err := json.NewDecoder(r.Body).Decode(&check); err != nil || r.URL.Path != "/internal/v1/authorization/check" || check.Permission != "platform.pricing.read" {
					t.Error("unexpected authorization request", err, r.URL.Path, check.Permission)
				}
				w.Header().Set("X-Authorization-Version", "1")
				_ = json.NewEncoder(w).Encode(map[string]any{"allowed": !test.denied, "user_id": actor})
			}))
			defer authority.Close()
			authorizer, err := authorization.NewClient(authority.URL, "controlled-service")
			if err != nil {
				t.Fatal(err)
			}
			repository := &protocolAdminHTTPRepository{
				protocol: ProtocolDefinition{ID: uuid.New(), ProtocolCode: "CHAT", Version: 1, Status: "PUBLISHED", RequestSchema: json.RawMessage(`{}`)},
				formula:  ProtocolFormulaVersion{ID: id, ProtocolCode: "CHAT", Version: 1, Status: "DRAFT", RequiredUnitVersions: map[string]uuid.UUID{"TOKENS": unit}, Expression: map[string]string{"TOKENS": "max_output_tokens"}, EvaluatorVersion: "SAFE_PARAMETER_MAP_V1"},
				err:      test.failure,
			}
			var base Repository = repository
			if test.missing {
				base = protocolAdminMissingRepository{}
			}
			s := NewService(nil, nil, nil).WithManagement(base, nil, ProviderURLPolicy{}, "", nil).WithAuthorizer(authorizer)
			handler := httpserver.CorrelationHandler(s.ManagementHandler("controlled-service", func(context.Context, string) (identity.Principal, error) {
				return identity.Principal{Issuer: "fixture", Subject: actor.String()}, nil
			}))
			request := httptest.NewRequest(http.MethodGet, "/admin/v1"+test.path, nil)
			if !test.anonymous {
				request.Header.Set("Authorization", "Bearer controlled")
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.status || repository.calls != test.calls {
				t.Fatalf("status=%d calls=%d; want %d/%d; body=%s", response.Code, repository.calls, test.status, test.calls, response.Body.String())
			}
			wantAuthority := 1
			if test.anonymous {
				wantAuthority = 0
			}
			if authorityCalls.Load() != int32(wantAuthority) {
				t.Fatalf("authorization calls=%d want=%d", authorityCalls.Load(), wantAuthority)
			}
			if response.Header().Get("Content-Type") != "application/json" || response.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("missing JSON/cache contract")
			}
			if test.code != "" {
				var body api.Error
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Code != test.code || body.Message == "" || body.Message == "controlled-backend-detail" || body.RequestID == "" {
					t.Fatalf("error contract: %s (%v)", response.Body.String(), err)
				}
			}
			if test.status != 200 {
				return
			}
			if test.path == "/protocols" {
				var body struct {
					Protocols []ProtocolDefinition
					Formulas  []ProtocolFormulaVersion
				}
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || !reflect.DeepEqual(body.Protocols, []ProtocolDefinition{repository.protocol}) || !reflect.DeepEqual(body.Formulas, []ProtocolFormulaVersion{repository.formula}) {
					t.Fatalf("list response changed: %s (%v)", response.Body.String(), err)
				}
			} else {
				var body struct {
					Formula        ProtocolFormulaVersion
					Sample         map[string]int64 `json:"sample_missing_optional_parameters"`
					Deterministic  bool
					ExternalAccess bool `json:"external_access"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || !reflect.DeepEqual(body.Formula, repository.formula) || repository.gotID != id || !body.Deterministic || body.ExternalAccess || !reflect.DeepEqual(body.Sample, map[string]int64{"TOKENS": 0}) {
					t.Fatalf("preview response changed: %s (%v)", response.Body.String(), err)
				}
			}
		})
	}
}
