package billing

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

// Only persistence is controlled here. Registered routes, identity middleware,
// Account authorization client, live guard callbacks and error mapping are real.
type manualAdmissionHTTPRepository struct {
	*PostgresRepository
	value        ProductManualAdmission
	beforeResult func()
	failure      error
	calls        int
}

func (r *manualAdmissionHTTPRepository) result(ctx context.Context, guard func(context.Context) error) (ProductManualAdmission, error) {
	r.calls++
	if r.beforeResult != nil {
		r.beforeResult()
	}
	if r.failure != nil {
		return ProductManualAdmission{}, r.failure
	}
	if guard != nil {
		if err := guard(ctx); err != nil {
			return ProductManualAdmission{}, err
		}
	}
	return r.value, nil
}
func (r *manualAdmissionHTTPRepository) CreateManualProductAdmission(ctx context.Context, _ ProductManualAdmissionCreate, guard func(context.Context) error) (ProductManualAdmission, error) {
	return r.result(ctx, guard)
}
func (r *manualAdmissionHTTPRepository) DecideManualProductAdmission(ctx context.Context, _ ProductManualAdmissionDecide, guard func(context.Context) error) (ProductManualAdmission, error) {
	return r.result(ctx, guard)
}
func (r *manualAdmissionHTTPRepository) GetManualProductAdmission(ctx context.Context, _, _ uuid.UUID) (ProductManualAdmission, error) {
	return r.result(ctx, nil)
}

func TestManualProductAdmissionHTTPAuthorityErrors(t *testing.T) {
	_ = tokenWalletWorkload(t)
	for _, endpoint := range []string{"create", "get", "decide"} {
		for _, scenario := range []string{"allowed", "unauthenticated", "stale", "middleware-denied", "actor-denied", "actor-guard-denied", "late-denied", "not-found", "backend-error"} {
			t.Run(endpoint+"/"+scenario, func(t *testing.T) {
				actor, catalog, verification, material := uuid.New(), uuid.New(), uuid.New(), uuid.New()
				var allowed atomic.Bool
				allowed.Store(true)
				var checks atomic.Int32
				denyAt := map[string]int32{"middleware-denied": 1, "actor-denied": 2, "actor-guard-denied": 3}[scenario]
				authority := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					var input struct{ Permission string }
					if req.URL.Path != "/internal/v1/authorization/check" || json.NewDecoder(req.Body).Decode(&input) != nil || input.Permission != "platform.products.publish" {
						t.Error("actual Account authorization contract changed")
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					count := checks.Add(1)
					ok := allowed.Load() && (denyAt == 0 || count < denyAt)
					_ = json.NewEncoder(w).Encode(map[string]any{"allowed": ok, "user_id": actor})
				}))
				defer authority.Close()
				authorizer, err := authorization.NewClient(authority.URL, "controlled-service")
				if err != nil {
					t.Fatal(err)
				}
				repository := &manualAdmissionHTTPRepository{value: ProductManualAdmission{VerificationID: verification, CatalogItemID: catalog, MaterialID: material, Status: "PENDING", Snapshot: json.RawMessage(`{"reason":"controlled-private-snapshot"}`)}}
				if scenario == "late-denied" {
					repository.beforeResult = func() { allowed.Store(false) }
				}
				if scenario == "backend-error" {
					repository.failure = errors.New("controlled-private-diagnostic")
				}
				if scenario == "not-found" {
					repository.failure = ErrNotFound
				}
				handler := NewService(repository).WithRechargeProductCatalog(repository).WithAuthorizer(authorizer).WithProductAdmissionMaterials(repository, nil, nil).Handler("controlled-service", func(context.Context, string) (identity.Principal, error) {
					authenticated := time.Now()
					if scenario == "stale" {
						authenticated = authenticated.Add(-16 * time.Minute)
					}
					return identity.Principal{Issuer: "controlled", Subject: "external-subject", AuthenticatedAt: authenticated}, nil
				})
				path := "/admin/v1/recharge-products/" + catalog.String() + "/manual-admission-submissions"
				method, payload := http.MethodPost, `{"expected_version":1,"material_id":"`+material.String()+`","reason":"controlled review"}`
				if endpoint == "get" {
					method, payload, path = http.MethodGet, "", path+"/"+verification.String()
				} else if endpoint == "decide" {
					path += "/" + verification.String() + "/decisions"
					payload = `{"expected_version":1,"submission_sha":"` + strings.Repeat("a", 64) + `","decision":"APPROVE","reason":"controlled decision"}`
				}
				request := httptest.NewRequest(method, path, strings.NewReader(payload))
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("Idempotency-Key", uuid.NewString())
				if scenario != "unauthenticated" {
					request.Header.Set("Authorization", "Bearer controlled-human")
				}
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				want := http.StatusForbidden
				if scenario == "allowed" {
					want = http.StatusOK
					if endpoint == "create" {
						want = http.StatusCreated
					}
				} else if scenario == "unauthenticated" || scenario == "stale" {
					want = http.StatusUnauthorized
				} else if scenario == "backend-error" {
					want = http.StatusInternalServerError
				} else if scenario == "not-found" {
					want = http.StatusNotFound
				}
				if response.Code != want {
					t.Fatal("manual admission lost authority/error semantics", response.Code, want, response.Body.String(), checks.Load())
				}
				if want >= 400 && (strings.Contains(response.Body.String(), "controlled-private") || strings.Contains(response.Body.String(), verification.String())) {
					t.Fatal("rejected read disclosed controlled facts", response.Body.String())
				}
				wantCalls := 0
				if scenario == "allowed" || scenario == "late-denied" || scenario == "not-found" || scenario == "backend-error" {
					wantCalls = 1
				}
				if repository.calls != wantCalls {
					t.Fatal("authority boundary reached wrong persistence phase", repository.calls, wantCalls)
				}
				if want == http.StatusForbidden && !strings.Contains(response.Body.String(), "PERMISSION_DENIED") {
					t.Fatal("permission denial lost stable error code", response.Body.String())
				}
				if want == http.StatusNotFound && !strings.Contains(response.Body.String(), "NOT_FOUND") {
					t.Fatal("missing resource lost stable error code", response.Body.String())
				}
			})
		}
	}
}
