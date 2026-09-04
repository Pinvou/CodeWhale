package token

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/requestdeadline"
	"github.com/google/uuid"
)

type stubImpactRepository struct {
	evidence ImpactLocalEvidence
	err      error
	wait     bool
}

type generatedImpactKeyRows struct {
	ctx     context.Context
	count   int
	index   int
	reverse bool
	errorAt int
	err     error
}

func (rows *generatedImpactKeyRows) Next() bool {
	if rows.ctx.Err() != nil || rows.index >= rows.count || rows.err != nil {
		return false
	}
	if rows.errorAt > 0 && rows.index == rows.errorAt {
		rows.err = context.DeadlineExceeded
		return false
	}
	return true
}

func (rows *generatedImpactKeyRows) Scan(destinations ...any) error {
	value := rows.index
	if rows.reverse {
		value = rows.count - rows.index - 1
	}
	*(destinations[0].(*string)) = strconv.Itoa(value)
	rows.index++
	return nil
}

func (rows *generatedImpactKeyRows) Err() error { return rows.err }
func (rows *generatedImpactKeyRows) Close()     {}

func (stub stubImpactRepository) result(ctx context.Context) (ImpactLocalEvidence, error) {
	if stub.wait {
		<-ctx.Done()
		return ImpactLocalEvidence{}, ctx.Err()
	}
	result := stub.evidence
	if result.ObservedAt.IsZero() {
		result.ObservedAt = time.Now().UTC()
	}
	return result, stub.err
}

func (stub stubImpactRepository) ModelLifecycleImpact(ctx context.Context, _ uuid.UUID, _ *uuid.UUID) (ImpactLocalEvidence, error) {
	return stub.result(ctx)
}
func (stub stubImpactRepository) CatalogRollbackImpact(ctx context.Context, _ int64) (ImpactLocalEvidence, error) {
	return stub.result(ctx)
}
func (stub stubImpactRepository) TokenAccountImpact(ctx context.Context, _ uuid.UUID) (ImpactLocalEvidence, error) {
	return stub.result(ctx)
}
func (stub stubImpactRepository) TokenRouteImpact(ctx context.Context, _ uuid.UUID, _, _, _ string) (ImpactLocalEvidence, error) {
	return stub.result(ctx)
}
func (stub stubImpactRepository) TokenCredentialImpact(ctx context.Context, _ uuid.UUID, _ *uuid.UUID) (ImpactLocalEvidence, error) {
	return stub.result(ctx)
}

type stubImpactPricing struct {
	result ImpactPriceCoverageResult
	err    error
}

func (stub stubImpactPricing) ImpactPriceCoverage(context.Context, []ImpactPriceQuery) (ImpactPriceCoverageResult, error) {
	return stub.result, stub.err
}

func impactTestHandler(t *testing.T, allowed bool, repository ImpactPrecheckRepository, pricing ImpactPriceCoverageClient, enabled bool) http.Handler {
	t.Helper()
	authorizationServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Authorization-Version", "17")
		_ = json.NewEncoder(w).Encode(map[string]any{"allowed": allowed, "user_id": uuid.New(), "granted_permissions": []string{"platform.models.read", "platform.tokens.read"}})
	}))
	t.Cleanup(authorizationServer.Close)
	authorizer, err := authorization.NewClient(authorizationServer.URL, "impact-test-service")
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(nil, nil, nil, nil, ProviderURLPolicy{}, nil, nil, 0).
		WithAuthorizer(authorizer).
		WithImpactPrechecks(enabled, repository, pricing)
	verify := func(context.Context, string) (identity.Principal, error) {
		return identity.Principal{Issuer: "https://issuer.test", Subject: uuid.NewString(), SessionID: "impact-session", AuthenticatedAt: time.Now().UTC()}, nil
	}
	return requestdeadline.TokenAdminMiddleware(service.Handler("", verify), time.Now)
}

func TestImpactPrechecksArePermissionedFeatureGatedAndRedacted(t *testing.T) {
	modelID, accountID, credentialID := uuid.New(), uuid.New(), uuid.New()
	catalogVersion, routeGeneration, credentialVersion := int64(7), int64(3), 2
	evidence := ImpactLocalEvidence{
		ResourceType: "MODEL", ResourceID: modelID.String(), CurrentStatus: "PUBLISHED",
		CurrentVersion:  ImpactResourceVersion{CatalogVersion: &catalogVersion, RouteGeneration: &routeGeneration, CredentialVersion: &credentialVersion, EvidenceETag: `"impact-evidence"`},
		Dependencies:    ImpactDependencySummary{Routes: 2, ReadyProvisionings: 1, InflightRequests: 1, ActiveSessions: 1, AlternativeCredentials: 1},
		ModelIDs:        []uuid.UUID{modelID},
		PriceDimensions: []ImpactPriceQuery{{ModelID: modelID, ProtocolCode: "CHAT", CreditUnit: "MICROCREDIT"}},
	}
	formulaID, priceID, effectiveFrom := uuid.New(), uuid.New(), time.Now().UTC().Add(-time.Hour)
	priceVersion := int64(1)
	pricing := stubImpactPricing{result: ImpactPriceCoverageResult{Source: "BILLING", AsOf: time.Now().UTC(), FinancialFactRevision: 7, PriceSetFingerprint: strings.Repeat("a", 64), Items: []ImpactPriceCoverage{{ModelID: modelID, ProtocolCode: "CHAT", FormulaVersionID: &formulaID, CreditUnit: "MICROCREDIT", Covered: true, PriceVersionID: &priceID, PriceVersion: &priceVersion, EffectiveFrom: &effectiveFrom}}}}
	tests := []struct {
		path   string
		action string
	}{
		{"/admin/v1/models/" + modelID.String() + "/impact/lifecycle?target_status=DEPRECATED", "MODEL_LIFECYCLE_DEPRECATED"},
		{"/admin/v1/model-catalog/impact/rollback?source_catalog_version=3", "MODEL_CATALOG_ROLLBACK"},
		{"/admin/v1/token-accounts/" + accountID.String() + "/impact/status?target_status=DISABLED", "TOKEN_ACCOUNT_STATUS_DISABLED"},
		{"/admin/v1/token-accounts/" + accountID.String() + "/impact/route?action=DELETE&model=model-one", "TOKEN_ROUTE_DELETE"},
		{"/admin/v1/token-accounts/" + accountID.String() + "/impact/credential?action=REVOKE&credential_id=" + credentialID.String(), "TOKEN_CREDENTIAL_REVOKE"},
	}
	handler := impactTestHandler(t, true, stubImpactRepository{evidence: evidence}, pricing, true)
	for _, item := range tests {
		request := httptest.NewRequest(http.MethodGet, item.path, nil)
		request.Header.Set("Authorization", "Bearer impact-test")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"action":"`+item.action+`"`) {
			t.Fatalf("%s status=%d body=%s", item.path, response.Code, response.Body.String())
		}
		lower := strings.ToLower(response.Body.String())
		for _, forbidden := range []string{"base_url", "provider", "secret", "ciphertext", `"fingerprint":`, "raw_error"} {
			if strings.Contains(lower, forbidden) {
				t.Fatalf("%s leaked %q: %s", item.path, forbidden, response.Body.String())
			}
		}
	}

	for _, test := range []struct {
		name    string
		handler http.Handler
		want    int
	}{
		{"permission denied", impactTestHandler(t, false, stubImpactRepository{evidence: evidence}, pricing, true), http.StatusForbidden},
		{"feature gate disabled", impactTestHandler(t, true, stubImpactRepository{evidence: evidence}, pricing, false), http.StatusServiceUnavailable},
	} {
		request := httptest.NewRequest(http.MethodGet, "/admin/v1/models/"+modelID.String()+"/impact/lifecycle?target_status=REMOVED", nil)
		request.Header.Set("Authorization", "Bearer impact-test")
		response := httptest.NewRecorder()
		test.handler.ServeHTTP(response, request)
		if response.Code != test.want || strings.Contains(response.Body.String(), modelID.String()) {
			t.Fatalf("%s status=%d body=%s", test.name, response.Code, response.Body.String())
		}
	}
}

func TestImpactPrecheckUsesOneAbsoluteDeadlineAndFailsClosed(t *testing.T) {
	modelID := uuid.New()
	handler := impactTestHandler(t, true, stubImpactRepository{wait: true}, stubImpactPricing{}, true)
	request := httptest.NewRequest(http.MethodGet, "/admin/v1/models/"+modelID.String()+"/impact/lifecycle?target_status=REMOVED", nil)
	request.Header.Set("Authorization", "Bearer impact-test")
	request.Header.Set(requestdeadline.DeadlineClassHeader, string(requestdeadline.Precheck))
	request.Header.Set(requestdeadline.AbsoluteDeadlineHeader, time.Now().Add(15*time.Millisecond).UTC().Format(time.RFC3339Nano))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusGatewayTimeout || !strings.Contains(response.Body.String(), "PRECHECK_TIMEOUT") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestCredentialAlternativeBlockerOnlyAppliesToServingCredential(t *testing.T) {
	accountID, credentialID := uuid.New(), uuid.New()
	requestPath := "/admin/v1/token-accounts/" + accountID.String() + "/impact/credential?action=REVOKE&credential_id=" + credentialID.String()
	for _, item := range []struct {
		name       string
		dependency ImpactDependencySummary
		blocked    bool
	}{
		{name: "unused retired credential", dependency: ImpactDependencySummary{}, blocked: false},
		{name: "serving credential without candidate", dependency: ImpactDependencySummary{ReadyProvisionings: 1}, blocked: true},
		{name: "serving credential with switchable candidate", dependency: ImpactDependencySummary{ReadyProvisionings: 1, AlternativeCredentials: 1}, blocked: false},
	} {
		t.Run(item.name, func(t *testing.T) {
			evidence := ImpactLocalEvidence{ResourceType: "PROVIDER_CREDENTIAL", ResourceID: credentialID.String(), CurrentStatus: "RETIRED", CurrentVersion: ImpactResourceVersion{EvidenceETag: `"impact"`, EvidenceSetSHA256: strings.Repeat("a", 64)}, Dependencies: item.dependency}
			handler := impactTestHandler(t, true, stubImpactRepository{evidence: evidence}, nil, true)
			request := httptest.NewRequest(http.MethodGet, requestPath, nil)
			request.Header.Set("Authorization", "Bearer impact-test")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "ALTERNATIVE_CREDENTIAL_MISSING") != item.blocked {
				t.Fatalf("status=%d blocked=%v body=%s", response.Code, item.blocked, response.Body.String())
			}
		})
	}
}

func TestRouteCoverageTupleInvariantIsPublishedOverHTTP(t *testing.T) {
	accountID := uuid.New()
	evidence := ImpactLocalEvidence{
		ResourceType: "TOKEN_ROUTE", ResourceID: accountID.String() + ":model-one", CurrentStatus: "PRESENT",
		CurrentVersion: ImpactResourceVersion{EvidenceETag: `"impact"`, EvidenceSetSHA256: strings.Repeat("a", 64)},
		Dependencies:   ImpactDependencySummary{ReadyProvisionings: 3, AffectedCoverageTuples: 2, RemainingReadyCoverage: 1, CoverageGaps: 1},
	}
	handler := impactTestHandler(t, true, stubImpactRepository{evidence: evidence}, nil, true)
	request := httptest.NewRequest(http.MethodGet, "/admin/v1/token-accounts/"+accountID.String()+"/impact/route?action=DELETE&model=model-one", nil)
	request.Header.Set("Authorization", "Bearer impact-test")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var result ImpactPrecheckResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	dependencies := result.Evidence.Dependencies
	if dependencies.AffectedCoverageTuples != 2 || dependencies.RemainingReadyCoverage != 1 || dependencies.CoverageGaps != dependencies.AffectedCoverageTuples-dependencies.RemainingReadyCoverage {
		t.Fatalf("coverage invariant=%+v", dependencies)
	}
}

func TestImpactPriceCoverageFailsClosedOnIncompleteOrUnavailableBilling(t *testing.T) {
	modelID := uuid.New()
	evidence := ImpactLocalEvidence{ResourceType: "MODEL", ResourceID: modelID.String(), CurrentStatus: "PUBLISHED", ModelIDs: []uuid.UUID{modelID}, PriceDimensions: []ImpactPriceQuery{{ModelID: modelID, ProtocolCode: "CHAT", CreditUnit: "MICROCREDIT"}}, CurrentVersion: ImpactResourceVersion{EvidenceETag: `"impact"`}}
	for _, pricing := range []ImpactPriceCoverageClient{
		stubImpactPricing{result: ImpactPriceCoverageResult{AsOf: time.Now().UTC()}},
		stubImpactPricing{err: errors.New("private billing error")},
	} {
		handler := impactTestHandler(t, true, stubImpactRepository{evidence: evidence}, pricing, true)
		request := httptest.NewRequest(http.MethodGet, "/admin/v1/models/"+modelID.String()+"/impact/lifecycle?target_status=DEPRECATED", nil)
		request.Header.Set("Authorization", "Bearer impact-test")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusGatewayTimeout || !strings.Contains(response.Body.String(), "PRECHECK_TIMEOUT") || strings.Contains(response.Body.String(), "private billing error") {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
	}
}

func TestImpactPriceCoverageRejectsMalformedBillingEvidence(t *testing.T) {
	modelID, otherID, formulaID, priceID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	effectiveFrom := time.Now().UTC().Add(-time.Hour)
	evidence := ImpactLocalEvidence{ResourceType: "MODEL", ResourceID: modelID.String(), CurrentStatus: "PUBLISHED", PriceDimensions: []ImpactPriceQuery{{ModelID: modelID, ProtocolCode: "CHAT", CreditUnit: "MICROCREDIT"}}, CurrentVersion: ImpactResourceVersion{EvidenceETag: `"impact"`}}
	priceVersion := int64(1)
	validItem := ImpactPriceCoverage{ModelID: modelID, ProtocolCode: "CHAT", FormulaVersionID: &formulaID, CreditUnit: "MICROCREDIT", Covered: true, PriceVersionID: &priceID, PriceVersion: &priceVersion, EffectiveFrom: &effectiveFrom}
	cases := map[string]ImpactPriceCoverageResult{
		"wrong source":          {Source: "TOKEN", AsOf: time.Now().UTC(), Items: []ImpactPriceCoverage{validItem}},
		"zero observed time":    {Source: "BILLING", Items: []ImpactPriceCoverage{validItem}},
		"extra item":            {Source: "BILLING", AsOf: time.Now().UTC(), Items: []ImpactPriceCoverage{validItem, {ModelID: otherID, ProtocolCode: "CHAT", CreditUnit: "MICROCREDIT"}}},
		"missing formula":       {Source: "BILLING", AsOf: time.Now().UTC(), Items: []ImpactPriceCoverage{{ModelID: modelID, ProtocolCode: "CHAT", CreditUnit: "MICROCREDIT", Covered: true, PriceVersionID: &priceID, EffectiveFrom: &effectiveFrom}}},
		"uncovered has price":   {Source: "BILLING", AsOf: time.Now().UTC(), Items: []ImpactPriceCoverage{{ModelID: modelID, ProtocolCode: "CHAT", CreditUnit: "MICROCREDIT", Covered: false, PriceVersionID: &priceID}}},
		"noncanonical protocol": {Source: "BILLING", AsOf: time.Now().UTC(), Items: []ImpactPriceCoverage{{ModelID: modelID, ProtocolCode: "chat", FormulaVersionID: &formulaID, CreditUnit: "MICROCREDIT", Covered: true, PriceVersionID: &priceID, EffectiveFrom: &effectiveFrom}}},
	}
	for name, result := range cases {
		t.Run(name, func(t *testing.T) {
			handler := impactTestHandler(t, true, stubImpactRepository{evidence: evidence}, stubImpactPricing{result: result}, true)
			request := httptest.NewRequest(http.MethodGet, "/admin/v1/models/"+modelID.String()+"/impact/lifecycle?target_status=DEPRECATED", nil)
			request.Header.Set("Authorization", "Bearer impact-test")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusGatewayTimeout || !strings.Contains(response.Body.String(), "PRECHECK_TIMEOUT") {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

func TestPrecheckFingerprintBindsActionReplacementAndPriceVersion(t *testing.T) {
	modelID, replacementA, replacementB, formulaID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	effectiveFrom, observedAt := time.Now().UTC().Add(-time.Hour), time.Now().UTC().Add(-time.Minute)
	evidence := ImpactLocalEvidence{ResourceType: "MODEL", ResourceID: modelID.String(), CurrentStatus: "PUBLISHED", ObservedAt: observedAt, PriceDimensions: []ImpactPriceQuery{{ModelID: modelID, ProtocolCode: "CHAT", CreditUnit: "MICROCREDIT"}}, CurrentVersion: ImpactResourceVersion{EvidenceETag: `"impact-local"`, EvidenceSetSHA256: strings.Repeat("a", 64)}}
	read := func(target string, replacement uuid.UUID, priceID uuid.UUID) ImpactPrecheckResult {
		t.Helper()
		priceVersion := int64(1)
		pricing := stubImpactPricing{result: ImpactPriceCoverageResult{Source: "BILLING", AsOf: observedAt.Add(time.Second), FinancialFactRevision: priceVersion, PriceSetFingerprint: strings.Repeat(string(rune('a'+priceVersion-1)), 64), Items: []ImpactPriceCoverage{{ModelID: modelID, ProtocolCode: "CHAT", FormulaVersionID: &formulaID, CreditUnit: "MICROCREDIT", Covered: true, PriceVersionID: &priceID, PriceVersion: &priceVersion, EffectiveFrom: &effectiveFrom}}}}
		handler := impactTestHandler(t, true, stubImpactRepository{evidence: evidence}, pricing, true)
		request := httptest.NewRequest(http.MethodGet, "/admin/v1/models/"+modelID.String()+"/impact/lifecycle?target_status="+target+"&replacement_model_id="+replacement.String(), nil)
		request.Header.Set("Authorization", "Bearer impact-test")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
		var result ImpactPrecheckResult
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	priceA, priceB := uuid.New(), uuid.New()
	base := read("DEPRECATED", replacementA, priceA)
	changedAction := read("REMOVED", replacementA, priceA)
	changedReplacement := read("DEPRECATED", replacementB, priceA)
	changedPrice := read("DEPRECATED", replacementA, priceB)
	for label, result := range map[string]ImpactPrecheckResult{"action": changedAction, "replacement": changedReplacement, "price": changedPrice} {
		if result.PrecheckFingerprint == base.PrecheckFingerprint {
			t.Fatalf("%s was not bound to precheck fingerprint", label)
		}
	}
	if base.TokenObservedAt.IsZero() || base.BillingObservedAt == nil || base.SnapshotSemantics != "INDEPENDENT_TOKEN_AND_BILLING_SNAPSHOTS" {
		t.Fatalf("snapshot metadata=%+v", base)
	}
}

func TestImpactKeyFingerprintStreamsHighCardinalityWithinBudget(t *testing.T) {
	const keyCount = 1_000_000
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	started := time.Now()
	digest := sha256.New()
	if err := appendImpactKeyRowsIterator(context.Background(), digest, &generatedImpactKeyRows{ctx: context.Background(), count: keyCount}); err != nil {
		t.Fatal(err)
	}
	ascending := digest.Sum(nil)
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("streaming 1M dependency keys exceeded 5s: %s", elapsed)
	}
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	if retained := int64(after.HeapAlloc) - int64(before.HeapAlloc); retained > 16<<20 {
		t.Fatalf("streaming dependency hash retained unexpected aggregate state: bytes=%d", retained)
	}
	reverse := sha256.New()
	if err := appendImpactKeyRowsIterator(context.Background(), reverse, &generatedImpactKeyRows{ctx: context.Background(), count: keyCount, reverse: true}); err != nil {
		t.Fatal(err)
	}
	if string(ascending) == string(reverse.Sum(nil)) {
		t.Fatal("dependency fingerprint did not bind fixed row order")
	}
	deadline := sha256.New()
	if err := appendImpactKeyRowsIterator(context.Background(), deadline, &generatedImpactKeyRows{ctx: context.Background(), count: keyCount, errorAt: 10}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("iterator deadline err=%v", err)
	}
}

func pointerUUID(value uuid.UUID) *uuid.UUID { return &value }
