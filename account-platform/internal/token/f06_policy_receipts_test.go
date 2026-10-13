package token

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/google/uuid"
)

// Installed handlers use the genuine identity middleware and HTTP authorizer.
// Domain seams below deliberately stop before wallet/price/provider writes.
type f06PolicyRepository struct {
	Repository
	CatalogRepository
	err   error
	model uuid.UUID
	saves int
}

func (r *f06PolicyRepository) FindUsageByIdempotency(context.Context, uuid.UUID, string) (UsageRequest, error) {
	return UsageRequest{}, ErrNotFound
}
func (r *f06PolicyRepository) ResolveCatalogModel(context.Context, string, string, time.Time) (CatalogModel, error) {
	return CatalogModel{ModelID: r.model, ProtocolCode: "CHAT", ProtocolVersion: 1}, nil
}
func (r *f06PolicyRepository) GetOrganizationModelPolicy(_ context.Context, org uuid.UUID) (OrganizationModelPolicy, error) {
	return OrganizationModelPolicy{OrganizationID: org, Version: 1, Mode: "ALLOW_ALL", ModelIDs: []uuid.UUID{}}, r.err
}
func (r *f06PolicyRepository) SaveOrganizationModelPolicy(_ context.Context, org, actor uuid.UUID, authorizationVersion, expected int64, mode string, models []uuid.UUID, key, reason string) (OrganizationModelPolicy, error) {
	r.saves++
	if actor == uuid.Nil || authorizationVersion != 17 || expected != 7 || mode != "ALLOW_ALL" || len(models) != 0 || key != "f06-policy-original-key" || reason != "controlled update" {
		panic("policy intent changed")
	}
	return OrganizationModelPolicy{OrganizationID: org, Version: 8, Mode: mode, ModelIDs: models}, r.err
}

type f06PolicyValidator struct {
	err   error
	calls int
}

func (v *f06PolicyValidator) ValidateContextModelPolicy(context.Context, AuthorizationContext, uuid.UUID) error {
	v.calls++
	return v.err
}
func (*f06PolicyValidator) Ensure(context.Context, AuthorizationContext, uuid.UUID, CatalogModel) (RouteProvisioning, error) {
	panic("rejected policy reached provisioning")
}

type f06UnsupportedPolicy struct{}

func (f06UnsupportedPolicy) Ensure(context.Context, AuthorizationContext, uuid.UUID, CatalogModel) (RouteProvisioning, error) {
	panic("unsupported policy reached provisioning")
}

func TestF06InstalledModelPolicyReceipts(t *testing.T) {
	for _, tc := range []struct {
		name, method, suffix, code string
		err                        error
		want                       int
		deny, unsupported          bool
	}{
		{"invoke-denied", "POST", "/chat/completions", "MODEL_POLICY_DENIED", fmt.Errorf("wrapped: %w", ErrModelPolicyDenied), 403, false, false},
		{"invoke-unavailable", "POST", "/chat/completions", "MODEL_UNAVAILABLE", ErrModelUnavailable, 422, false, false},
		{"invoke-policy-seam-missing", "POST", "/chat/completions", "MODEL_POLICY_UNAVAILABLE", nil, 503, false, true},
		{"policy-version", "PUT", "", "MODEL_POLICY_VERSION_CONFLICT", fmt.Errorf("wrapped: %w", ErrModelPolicyVersionConflict), 409, false, false},
		{"policy-model-unavailable", "PUT", "", "MODEL_UNAVAILABLE", ErrModelUnavailable, 422, false, false},
		{"policy-storage", "PUT", "", "INTERNAL_ERROR", errors.New("private storage detail"), 500, false, false},
		{"policy-current-update", "PUT", "", "", nil, 200, false, false},
		{"policy-current-read", "GET", "", "", nil, 200, false, false},
		{"policy-permission-denied", "PUT", "", "TOKEN_FORBIDDEN", nil, 403, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			user, org := uuid.New(), uuid.New()
			version := int64(3)
			decision := &authorizationDecisionRecorder{allowed: !tc.deny, userID: user}
			authority := httptest.NewServer(decision)
			defer authority.Close()
			auth, err := authorization.NewClient(authority.URL, "controlled-service")
			if err != nil {
				t.Fatal(err)
			}
			repo := &f06PolicyRepository{err: tc.err, model: uuid.New()}
			validator := &f06PolicyValidator{err: tc.err}
			s := &Service{repository: repo, managementEnabled: true, personalExecutionEnabled: true, authorizer: auth, contexts: fixedBillingContextResolver{AuthorizationContext{UserID: user, ContextType: "ORGANIZATION", OrganizationID: &org, OrganizationMembershipVersion: &version}}, defaultRoutes: validator, defaultMaxOutputTokens: 10, maximumOutputTokens: 100, maximumInputTokens: 1000}
			if tc.unsupported {
				s.defaultRoutes = f06UnsupportedPolicy{}
			}
			path := "/v1/organizations/" + org.String() + "/model-policy"
			body := `{"expected_version":7,"mode":"ALLOW_ALL","model_ids":[],"reason":"controlled update"}`
			if tc.method == "POST" {
				path = "/v1/chat/completions"
				body = `{"model":"controlled-model","messages":[{"role":"user","content":"hello"}]}`
			}
			req := httptest.NewRequest(tc.method, path, strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer controlled-user")
			req.Header.Set("Idempotency-Key", "f06-policy-original-key")
			out := httptest.NewRecorder()
			s.Handler(successfulBillingVerify).ServeHTTP(out, req)
			if out.Code != tc.want || tc.code != "" && !strings.Contains(out.Body.String(), `"code":"`+tc.code+`"`) {
				t.Fatalf("HTTP=%d body=%s", out.Code, out.Body)
			}
			if strings.Contains(out.Body.String(), "private storage detail") {
				t.Fatal("storage detail leaked")
			}
			if tc.method == "PUT" && repo.saves != map[bool]int{true: 0, false: 1}[tc.deny] {
				t.Fatal("permission or installed policy dispatch changed")
			}
			if tc.method == "POST" && !tc.unsupported && validator.calls != 1 {
				t.Fatal("current organization policy not checked")
			}
			decision.mu.Lock()
			defer decision.mu.Unlock()
			permission := "org.models.manage"
			if tc.method == "GET" {
				permission = "org.models.read"
			}
			if tc.method == "POST" {
				permission = "org.model.invoke"
			}
			if len(decision.permissions) != 1 || decision.permissions[0] != permission || decision.organizationID == nil || *decision.organizationID != org {
				t.Fatal("wrong current organization authority")
			}
		})
	}
}

func TestF06InitialMemberLifecycleFailureIsDefinitiveOnlyForExactReceipt(t *testing.T) {
	if definitiveBillingFailure(&BillingHTTPError{Status: 409, Code: "MEMBER_LIFECYCLE_CONFLICT"}) {
		t.Fatal("initial reservation rejection leaked into generic cancellation recovery")
	}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"exact-before-funds", &BillingHTTPError{Status: 409, Code: "MEMBER_LIFECYCLE_CONFLICT"}, true},
		{"wrapped-exact", fmt.Errorf("wrapped: %w", &BillingHTTPError{Status: 409, Code: "MEMBER_LIFECYCLE_CONFLICT"}), true},
		{"other-conflict", &BillingHTTPError{Status: 409, Code: "OTHER_CONFLICT"}, false},
		{"wrong-status", &BillingHTTPError{Status: 503, Code: "MEMBER_LIFECYCLE_CONFLICT"}, false},
		{"network", errors.New("uncertain response"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if definitiveInitialBillingFailure(tc.err) != tc.want {
				t.Fatal("certain rejection and unknown funds were conflated")
			}
		})
	}
	// Follow the production receipt/recovery path after the exact admission
	// predicate, keeping a lost local ACK recoverable under the original usage.
	r := &streamRepositoryFake{releaseErr: errors.New("lost local ack")}
	s := &Service{repository: r}
	usage, org := uuid.New(), uuid.New()
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	out := httptest.NewRecorder()
	failure := &BillingHTTPError{Status: 409, Code: "MEMBER_LIFECYCLE_CONFLICT"}
	if !definitiveInitialBillingFailure(failure) {
		t.Fatal("known rejected reservation became unknown")
	}
	s.writeInitialReservationFailure(out, req, usage, DispatchEnvelope{ContextType: "ORGANIZATION", OrganizationID: &org}, failure)
	if out.Code != 503 || r.savedResult.HTTPStatus != 409 || !strings.Contains(string(r.savedResult.Payload), "MEMBER_LIFECYCLE_CONFLICT") {
		t.Fatal("original terminal receipt lost")
	}
	r.releaseErr = nil
	r.replaySnapshot = ReplaySnapshot{Usage: UsageRequest{ID: usage, ExecutionStatus: "HOLD_RESERVING", RequestSnapshot: json.RawMessage(`{"provider_id":"controlled","adapter_version":"openai-compatible-v1"}`)}, Result: &r.savedResult}
	retry := httptest.NewRecorder()
	handled, err := s.recoverInitialReservationFailure(retry, req, usage)
	if err != nil || !handled || retry.Code != 409 || !r.released {
		t.Fatal("saved exact rejection did not recover", err)
	}
}

type f06UnconfirmedCancelRepository struct {
	Repository
	managementCancellationStore
	usage               UsageRequest
	requested, reviewed int
}

func (r *f06UnconfirmedCancelRepository) RequestCancellation(_ context.Context, id, user uuid.UUID, key, fingerprint, adapter string) (CancellationResult, bool, error) {
	if id != r.usage.ID || user != r.usage.UserID || key != digestString([]byte("f06-cancel-original-key")) || fingerprint != cancellationFingerprint(id) || adapter != "openai-compatible-v1" {
		panic("cancel original identity/key changed")
	}
	r.requested++
	return cancellationSnapshot(r.usage), true, nil
}
func (r *f06UnconfirmedCancelRepository) GetUsage(context.Context, uuid.UUID) (UsageRequest, error) {
	return r.usage, nil
}
func (r *f06UnconfirmedCancelRepository) LoadDispatchPreparation(context.Context, uuid.UUID) (DispatchEnvelope, error) {
	return DispatchEnvelope{Price: PriceVersion{ID: uuid.New(), FormulaVersionID: uuid.New()}}, nil
}
func (r *f06UnconfirmedCancelRepository) MarkReview(_ context.Context, id uuid.UUID, reason string, evidence map[string]any) error {
	if id != r.usage.ID || reason != "cancel_hold_recovery_unconfirmed" || evidence["customer_hold_release_unconfirmed"] != true {
		panic("lost original uncertainty evidence")
	}
	r.reviewed++
	r.usage.Status = "REVIEW"
	r.usage.ExecutionStatus = "RECONCILIATION_PENDING"
	r.usage.CancelStatus = "UNCERTAINTY"
	return nil
}
func (*f06UnconfirmedCancelRepository) ConfirmPreDispatchCancellation(context.Context, uuid.UUID) (CancellationResult, error) {
	panic("unconfirmed earlier hold was treated as released")
}
func (*f06UnconfirmedCancelRepository) MarkHoldReserving(context.Context, uuid.UUID) error {
	panic("existing reservation was restarted")
}
func (*f06UnconfirmedCancelRepository) CompleteCancellationHold(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) error {
	panic("unconfirmed recovery created a hold receipt")
}

type f06UnconfirmedCancelBilling struct {
	BillingClient
	cancellationBillingClient
	SnapshotOrganizationBillingClient
	reserved int
}

func (b *f06UnconfirmedCancelBilling) ReserveMemberQuotaPriced(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, int64, int64, uuid.UUID, HoldPricingContext) (BillingHold, error) {
	b.reserved++
	return BillingHold{}, &BillingHTTPError{Status: 409, Code: "MEMBER_LIFECYCLE_CONFLICT"}
}
func (*f06UnconfirmedCancelBilling) Release(context.Context, uuid.UUID, uuid.UUID, string) error {
	panic("possible earlier hold was blindly released")
}

func TestF06InstalledCancellationMemberConflictRemainsUnconfirmed(t *testing.T) {
	user, org, usage := uuid.New(), uuid.New(), uuid.New()
	version := int64(3)
	snapshot, _ := json.Marshal(frozenUsageSnapshot{ContextType: "ORGANIZATION", OrganizationID: &org, OrganizationMembershipVersion: &version})
	repo := &f06UnconfirmedCancelRepository{usage: UsageRequest{ID: usage, UserID: user, OrganizationID: &org, WalletID: uuid.New(), Model: "controlled", MaximumMicrocredits: 10, CanonicalVersion: 1, ExecutionStatus: "HOLD_RESERVING", CancelStatus: "CANCEL_REQUESTED", RequestSnapshot: snapshot}}
	decision := &authorizationDecisionRecorder{allowed: true, userID: user}
	authority := httptest.NewServer(decision)
	defer authority.Close()
	auth, err := authorization.NewClient(authority.URL, "controlled-service")
	if err != nil {
		t.Fatal(err)
	}
	billing := &f06UnconfirmedCancelBilling{}
	s := &Service{repository: repo, cancellationStore: repo, cancellationBilling: billing, billing: billing, executionUpstream: NewHTTPUpstreamClient(), authorizer: auth, personalExecutionEnabled: true}
	req := httptest.NewRequest("POST", "/v1/usage-requests/"+usage.String()+"/cancel", nil)
	req.Header.Set("Authorization", "Bearer controlled-user")
	req.Header.Set("Idempotency-Key", "f06-cancel-original-key")
	out := httptest.NewRecorder()
	s.Handler(successfulBillingVerify).ServeHTTP(out, req)
	if out.Code != http.StatusAccepted || out.Header().Get("Location") != "/v1/usage-requests/"+usage.String() || out.Header().Get("Cache-Control") != "no-store" || !strings.Contains(out.Body.String(), "UNCERTAINTY") || repo.requested != 1 || repo.reviewed != 1 || billing.reserved != 1 || repo.usage.HoldID != nil {
		t.Fatalf("cancel recovery changed known pending identity/funds: HTTP=%d body=%s", out.Code, out.Body)
	}
}
