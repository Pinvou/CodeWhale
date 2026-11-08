package token

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/resilience"
	"github.com/google/uuid"
)

// Pinned canonical-v1 vector for the historical null body below. Keep this
// independent of the runtime helpers so a fingerprint change cannot bless its
// own replacement of an already accepted request.
const chatBudgetHistoricalFingerprint = "78ceb6d31a62ceffaf8e7baeb49bad838edf52c8e755829b963cfece204a07a4"

// Storage and funds are controlled; the installed HTTP handler, authorization,
// canonicalization, metering, hold producer and upstream HTTP client are real.
type chatBudgetRepository struct {
	streamRepositoryFake
	CatalogRepository
	lookupErr                     error
	model                         CatalogModel
	route                         Candidate
	baseURL                       string
	existing                      *UsageRequest
	usage                         UsageRequest
	envelope                      DispatchEnvelope
	lookups, catalogs, begins     int
	credentialReads, preparations int
	loaded                        int
}

func (r *chatBudgetRepository) FindUsageByIdempotency(context.Context, uuid.UUID, string) (UsageRequest, error) {
	r.lookups++
	if r.lookupErr != nil {
		return UsageRequest{}, r.lookupErr
	}
	if r.existing != nil {
		return *r.existing, nil
	}
	return UsageRequest{}, ErrNotFound
}
func (r *chatBudgetRepository) ResolveCatalogModel(context.Context, string, string, time.Time) (CatalogModel, error) {
	r.catalogs++
	return r.model, nil
}
func (r *chatBudgetRepository) ListCandidates(context.Context, string, string) ([]Candidate, error) {
	return []Candidate{r.route}, nil
}
func (r *chatBudgetRepository) BeginUsage(_ context.Context, usage UsageRequest, snapshot map[string]any) (UsageRequest, bool, error) {
	r.begins++
	usage.RequestSnapshot, _ = json.Marshal(snapshot)
	usage.Status, usage.ExecutionStatus = "CREATED", "RECEIVED"
	r.usage = usage
	return usage, true, nil
}
func (r *chatBudgetRepository) CredentialSecret(context.Context, uuid.UUID, uuid.UUID) (Credential, []byte, error) {
	r.credentialReads++
	return Credential{Provider: "budget-fixture", BaseURL: r.baseURL}, []byte("synthetic-budget-secret"), nil
}
func (r *chatBudgetRepository) PrepareDispatch(_ context.Context, _ uuid.UUID, envelope DispatchEnvelope) error {
	r.preparations++
	envelope.Credential = append([]byte(nil), envelope.Credential...)
	r.envelope = envelope
	return nil
}
func (r *chatBudgetRepository) MarkHoldReserving(context.Context, uuid.UUID) error {
	r.usage.ExecutionStatus = "HOLD_RESERVING"
	return nil
}
func (r *chatBudgetRepository) CompleteHoldAndDispatch(_ context.Context, _, hold, price uuid.UUID) error {
	r.usage.HoldID, r.usage.PriceVersionID = &hold, &price
	r.usage.ExecutionStatus = "DISPATCH_INTENT_PERSISTED"
	return nil
}
func (r *chatBudgetRepository) AcquireDispatch(context.Context, uuid.UUID, string, time.Duration) (DispatchEnvelope, bool, error) {
	r.envelope.leaseOwner = "budget-fixture-lease"
	r.usage.ExecutionStatus = "UPSTREAM_DISPATCHING"
	envelope := r.envelope
	envelope.Credential = append([]byte(nil), envelope.Credential...)
	return envelope, true, nil
}
func (r *chatBudgetRepository) LoadDispatchPreparation(context.Context, uuid.UUID) (DispatchEnvelope, error) {
	r.loaded++
	return r.envelope, nil
}
func (r *chatBudgetRepository) GetUsage(context.Context, uuid.UUID) (UsageRequest, error) {
	return r.usage, nil
}
func (r *chatBudgetRepository) MarkCommitted(_ context.Context, _ uuid.UUID, actual int64, snapshot map[string]any) error {
	r.committed, r.commitSnapshot = true, snapshot
	r.usage.ActualMicrocredits = &actual
	r.usage.Status, r.usage.ExecutionStatus = "COMMITTED", "SUCCEEDED"
	return nil
}

type chatBudgetBilling struct {
	streamBillingFake
	BillingClient
	price    PriceVersion
	prices   int
	reserves int
	maximum  int64
	usageID  uuid.UUID
	holdID   uuid.UUID
	actual   int64
}

func (b *chatBudgetBilling) Price(context.Context, string) (PriceVersion, error) {
	b.prices++
	return b.price, nil
}
func (b *chatBudgetBilling) ReservePriced(_ context.Context, _, _ uuid.UUID, maximum int64, _ uuid.UUID, _ HoldPricingContext) (BillingHold, error) {
	b.reserves++
	b.maximum = maximum
	return BillingHold{ID: uuid.New()}, nil
}
func (b *chatBudgetBilling) Commit(_ context.Context, usage, hold uuid.UUID, actual int64, _ Usage) error {
	b.commitCalls++
	b.usageID, b.holdID, b.actual = usage, hold, actual
	return nil
}

type chatBudgetWallet struct{ id uuid.UUID }

func (w chatBudgetWallet) Ensure(context.Context, string, uuid.UUID) (uuid.UUID, error) {
	return w.id, nil
}

type chatBudgetHTTPFixture struct {
	repo       *chatBudgetRepository
	billing    *chatBudgetBilling
	dispatches atomic.Int32
	mu         sync.Mutex
	payload    []byte
	credential string
	handler    http.Handler
}

func newChatBudgetHTTPFixture(t *testing.T) *chatBudgetHTTPFixture {
	t.Helper()
	f := &chatBudgetHTTPFixture{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.dispatches.Add(1)
		payload, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.payload = payload
		f.credential = r.Header.Get("Authorization")
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"controlled-result","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":0,"total_tokens":1}}`))
	}))
	t.Cleanup(upstream.Close)
	user, wallet, account, credential := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	authority := httptest.NewServer(&authorizationDecisionRecorder{allowed: true, userID: user})
	t.Cleanup(authority.Close)
	authorizer, err := authorization.NewClient(authority.URL, "controlled-service")
	if err != nil {
		t.Fatal(err)
	}
	policy, err := NewProviderURLPolicy("test", []string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	model := CatalogModel{ModelID: uuid.New(), ModelCapabilityVersionID: uuid.New(), ProtocolDefinitionID: uuid.New(), ProtocolCode: "CHAT", ProtocolVersion: 1, UpstreamModelRef: "budget-upstream", CatalogVersion: 3, ContextLength: 10000, MaximumOutputTokens: 5000}
	now, expires := time.Now().UTC(), time.Now().UTC().Add(time.Hour)
	f.repo = &chatBudgetRepository{model: model, baseURL: upstream.URL, route: Candidate{TokenAccountID: account, CredentialID: credential, WalletID: wallet, OwnerType: "USER", OwnerID: user, Status: "ACTIVE", HealthStatus: "HEALTHY", HealthExpiresAt: &expires, DatabaseNow: now, AllowedModels: []string{model.UpstreamModelRef}, ProtocolCode: "CHAT", ProviderID: "budget-fixture", RouteGeneration: 7}}
	f.billing = &chatBudgetBilling{price: PriceVersion{ID: uuid.New(), FormulaVersionID: uuid.New(), ProtocolCode: "CHAT", CreditUnit: "MICROCREDIT", FormulaSnapshot: json.RawMessage(`{"evaluator_version":"SAFE_PARAMETER_MAP_V1","expression":{"INPUT_TOKEN":"estimated_input_tokens","CACHED_INPUT_TOKEN":"cached_input_tokens","OUTPUT_TOKEN":"max_output_tokens"}}`)}}
	for _, unit := range []string{"INPUT_TOKEN", "CACHED_INPUT_TOKEN", "OUTPUT_TOKEN"} {
		f.billing.price.Units = append(f.billing.price.Units, ResolvedUnitPrice{UnitCode: unit, MinimumPrecision: 1, PriceDenominator: 1, RoundingRule: "CEILING", ResolvedPrice: 1})
	}
	service := &Service{repository: f.repo, contexts: fixedBillingContextResolver{AuthorizationContext{UserID: user, SessionID: "budget-session", ContextType: "PERSONAL"}}, wallets: chatBudgetWallet{wallet}, billing: f.billing, cancellationBilling: f.billing, executionBilling: f.billing, executionUpstream: NewHTTPUpstreamClient(), defaultRoutes: f05DeadlineReadyRoute{}, authorizer: authorizer, providerURLs: policy, managementEnabled: true, personalExecutionEnabled: true, defaultMaxOutputTokens: 4096, maximumOutputTokens: 5000, maximumInputTokens: 5000, breaker: resilience.NewBreaker()}
	f.handler = service.Handler(successfulBillingVerify)
	return f
}

func (f *chatBudgetHTTPFixture) dispatchedPayload() ([]byte, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.payload...), f.credential
}

func (f *chatBudgetHTTPFixture) request(body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer controlled-user")
	r.Header.Set("Idempotency-Key", "budget-original-intent-key")
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	return w
}

func TestChatBudgetNewNullRejectedBeforeAcceptanceHTTP(t *testing.T) {
	for _, fields := range []string{`"max_completion_tokens":null`, `"max_tokens":null`, `"max_tokens":null,"max_completion_tokens":null`, `"max_completion_tokens":null,"max_tokens":64`, `"max_completion_tokens":64,"max_tokens":null`, `"stream":true,"max_completion_tokens":null`} {
		t.Run(fields, func(t *testing.T) {
			f := newChatBudgetHTTPFixture(t)
			w := f.request(`{"model":"model","messages":[{"role":"user","content":"hello"}],` + fields + `}`)
			if w.Code != 400 || !strings.Contains(w.Body.String(), `"code":"INVALID_REQUEST"`) {
				t.Fatalf("new null request not rejected before acceptance: status=%d begins=%d reserve=%d dispatch=%d", w.Code, f.repo.begins, f.billing.reserves, f.dispatches.Load())
			}
			if f.repo.lookups != 1 || f.repo.catalogs != 0 || f.repo.begins != 0 || f.repo.usage.ID != uuid.Nil || f.billing.reserves != 0 || f.dispatches.Load() != 0 || f.repo.credentialReads != 0 {
				t.Fatal("invalid new request created usage, funds, or provider work")
			}
		})
	}
}

func TestChatBudgetValidFieldsKeepExecutionBudgetAndFingerprintHTTP(t *testing.T) {
	for _, tc := range []struct {
		name, fields string
		maximum      int64
		fingerprint  string
	}{
		{"missing", "", 4096, "c6be851bcbe43176de0b119bf419b206b394ab36859efe07c31c3f84227014e2"},
		{"completion-zero", `,"max_completion_tokens":0`, 0, "b868b11b90afa618d4edb9054835758c0b4074d54db292d384314318b8338055"},
		{"legacy-zero", `,"max_tokens":0`, 0, "d7c9b5723c7502b5cb63425745d8074f2cb50aa847a729838c03c55e99e93d9c"},
		{"both", `,"max_completion_tokens":64,"max_tokens":32`, 64, "a3a0e9fdc17bbfb72405ca2bbf32946afb982cc9a980ed6c0fe80738dfbf20f1"},
		{"both-completion-zero", `,"max_completion_tokens":0,"max_tokens":32`, 32, "fe2519ec085e2e0b9621b5be2f0bda21463dfe360b4f1ec950ddeefdfe4329ea"},
		{"both-zero", `,"max_completion_tokens":0,"max_tokens":0`, 0, "ee4c39ef57e2075193e431c6095c5921403318f7fa4eba3d3eed1388a5e3b333"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newChatBudgetHTTPFixture(t)
			body := `{"model":"model","messages":[{"role":"user","content":"hello"}]` + tc.fields + `}`
			w := f.request(body)
			if w.Code != 200 || f.repo.begins != 1 || f.billing.reserves != 1 || f.dispatches.Load() != 1 || !f.repo.committed {
				t.Fatalf("valid request changed: status=%d body=%s begins=%d reserve=%d dispatch=%d", w.Code, w.Body, f.repo.begins, f.billing.reserves, f.dispatches.Load())
			}
			if f.repo.usage.RequestFingerprint != tc.fingerprint || f.repo.usage.CanonicalVersion != 1 || f.repo.envelope.MaximumOutput != tc.maximum || f.billing.maximum != 2+tc.maximum {
				t.Fatal("execution budget, original fingerprint or pricing changed")
			}
			var forwarded map[string]json.RawMessage
			payload, _ := f.dispatchedPayload()
			if json.Unmarshal(payload, &forwarded) != nil || string(forwarded["model"]) != `"budget-upstream"` {
				t.Fatal("actual upstream payload missing")
			}
			if tc.name == "missing" && string(forwarded["max_completion_tokens"]) != "4096" || tc.name == "completion-zero" && string(forwarded["max_completion_tokens"]) != "0" || tc.name == "legacy-zero" && string(forwarded["max_tokens"]) != "0" {
				t.Fatal("default or explicit zero changed on real upstream HTTP")
			}
		})
	}
}

func TestChatBudgetAcceptedNullResumesFrozenHoldHTTP(t *testing.T) {
	f := newChatBudgetHTTPFixture(t)
	body := `{"model":"old-model","messages":[{"role":"user","content":"frozen"}],"max_completion_tokens":null}`
	id, hold, price := uuid.New(), uuid.New(), uuid.New()
	snapshot := json.RawMessage(`{"provider_id":"budget-fixture","adapter_version":"` + NewHTTPUpstreamClient().AdapterVersion() + `","maximum_output_tokens":0,"frozen":"historical"}`)
	usage := UsageRequest{ID: id, UserID: f.repo.route.OwnerID, SessionID: "old-session", TokenAccountID: uuid.New(), CredentialID: uuid.New(), WalletID: uuid.New(), HoldID: &hold, PriceVersionID: &price, Status: "RESERVED", ExecutionStatus: "HOLD_RESERVED", CanonicalVersion: 1, RequestFingerprint: chatBudgetHistoricalFingerprint, MaximumMicrocredits: 17, RequestSnapshot: snapshot}
	f.repo.usage = usage
	f.repo.existing = &f.repo.usage
	frozenPrice := f.billing.price
	frozenPrice.ID = price
	frozenPrice.Units = append([]ResolvedUnitPrice(nil), frozenPrice.Units...)
	frozenPrice.Units[0].ResolvedPrice = 3
	frozenBody := []byte(strings.Replace(body, "old-model", "frozen-upstream", 1))
	f.repo.envelope = DispatchEnvelope{BaseURL: f.repo.baseURL, Provider: "budget-fixture", AdapterVersion: NewHTTPUpstreamClient().AdapterVersion(), Credential: []byte("historical-frozen-secret"), RequestBody: frozenBody, Price: frozenPrice, MaximumCost: 17, MaximumOutput: 0, ContextType: "PERSONAL"}
	w := f.request(body)
	if w.Code != 200 || f.repo.begins != 0 || f.repo.catalogs != 0 || f.repo.credentialReads != 0 || f.billing.prices != 0 || f.billing.reserves != 0 || f.dispatches.Load() != 1 || f.repo.loaded != 1 {
		t.Fatalf("accepted null did not resume original hold: status=%d body=%s", w.Code, w.Body)
	}
	if f.repo.usage.ID != id || f.repo.usage.CredentialID != usage.CredentialID || f.repo.usage.HoldID == nil || *f.repo.usage.HoldID != hold || f.repo.usage.PriceVersionID == nil || *f.repo.usage.PriceVersionID != price || f.repo.usage.MaximumMicrocredits != 17 || string(f.repo.usage.RequestSnapshot) != string(snapshot) || f.billing.usageID != id || f.billing.holdID != hold || f.billing.actual != 3 {
		t.Fatal("historical usage, credential, price or funds were replaced or repriced")
	}
	payload, credential := f.dispatchedPayload()
	if string(payload) != string(frozenBody) || credential != "Bearer historical-frozen-secret" || f.repo.envelope.MaximumOutput != 0 {
		t.Fatal("historical null body/default or frozen credential changed")
	}
}

func TestChatBudgetUnknownHistoryDoesNotClaimNewRejectionHTTP(t *testing.T) {
	f := newChatBudgetHTTPFixture(t)
	f.repo.lookupErr = errors.New("controlled history unavailable")
	w := f.request(`{"model":"model","messages":[{"role":"user","content":"hello"}],"max_completion_tokens":null}`)
	if w.Code != 500 || strings.Contains(w.Body.String(), "INVALID_REQUEST") || f.repo.lookups != 1 || f.repo.begins != 0 || f.billing.reserves != 0 || f.dispatches.Load() != 0 {
		t.Fatalf("unknown history was treated as a proven new rejection: status=%d body=%s", w.Code, w.Body)
	}
}

func TestChatBudgetAcceptedNullKeepsOriginalReplayHTTP(t *testing.T) {
	for _, status := range []string{"COMMITTED", "REVIEW", "RELEASED"} {
		t.Run(status, func(t *testing.T) {
			f := newChatBudgetHTTPFixture(t)
			body := `{"model":"old-model","messages":[{"role":"user","content":"frozen"}],"max_completion_tokens":null}`
			hold, price, id := uuid.New(), uuid.New(), uuid.New()
			actual := int64(37)
			usage := UsageRequest{ID: id, UserID: f.repo.route.OwnerID, SessionID: "old-session", TokenAccountID: uuid.New(), CredentialID: uuid.New(), WalletID: uuid.New(), HoldID: &hold, PriceVersionID: &price, Status: status, ExecutionStatus: status, CanonicalVersion: 1, RequestFingerprint: chatBudgetHistoricalFingerprint, MaximumMicrocredits: 17, ActualMicrocredits: &actual, RequestSnapshot: json.RawMessage(`{"maximum_output_tokens":0,"frozen":"historical"}`)}
			if status == "COMMITTED" {
				usage.ExecutionStatus = "SUCCEEDED"
			}
			if status == "RELEASED" {
				usage.ExecutionStatus = "FAILED"
			}
			f.repo.existing = &usage
			f.repo.replaySnapshot = ReplaySnapshot{Usage: usage}
			want := 202
			if status == "COMMITTED" {
				want = 200
				f.repo.replaySnapshot.Result = &ReplayResult{Kind: "JSON", HTTPStatus: 200, ContentType: "application/json", Payload: []byte(`{"frozen_result":true}`)}
			} else if status == "RELEASED" {
				want, f.repo.replaySnapshot.ResultExpired = 410, true
			}
			before, _ := json.Marshal(usage)
			for range 2 {
				w := f.request(body)
				if w.Code != want || status == "COMMITTED" && w.Body.String() != `{"frozen_result":true}` || status == "REVIEW" && !strings.Contains(w.Body.String(), id.String()) {
					t.Fatalf("historical null replay blocked/reinterpreted: status=%d body=%s", w.Code, w.Body)
				}
			}
			changed := f.request(strings.Replace(body, "null", "0", 1))
			if changed.Code != 409 || !strings.Contains(changed.Body.String(), "IDEMPOTENCY_KEY_REUSED") {
				t.Fatal("changed canonical facts reused the accepted key")
			}
			after, _ := json.Marshal(f.repo.existing)
			if !reflect.DeepEqual(before, after) || f.repo.begins != 0 || f.repo.catalogs != 0 || f.repo.credentialReads != 0 || f.billing.prices != 0 || f.billing.reserves != 0 || f.dispatches.Load() != 0 || f.repo.loadReplayCalls != 2 {
				t.Fatal("accepted result changed IDs, funds or recovery boundary")
			}
		})
	}
}
