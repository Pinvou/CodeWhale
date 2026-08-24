package token

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestNewAPIUsageEvidenceClientReadsConsumptionByRequestID(t *testing.T) {
	newAPIRequestID := "newapi-request-123"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/log/" || r.URL.Query().Get("request_id") != newAPIRequestID || r.URL.Query().Get("type") != "2" {
			t.Fatalf("unexpected log lookup: %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		if r.Header.Get("Authorization") != "Bearer admin-token" || r.Header.Get("New-Api-User") != "7" {
			t.Fatal("missing New API admin authentication")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"items": []map[string]any{{
			"request_id": newAPIRequestID, "prompt_tokens": 12, "completion_tokens": 3, "other": `{"cache_tokens":4}`,
		}}}})
	}))
	defer server.Close()

	policy, err := NewProviderURLPolicy("development", []string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewNewAPIUsageEvidenceClient(server.URL, "admin-token", 7, policy)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := client.Lookup(context.Background(), newAPIRequestID)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Usage == nil || *evidence.Usage != (Usage{InputTokens: 8, OutputTokens: 3, CachedInputTokens: 4}) || evidence.Rejected {
		t.Fatalf("unexpected evidence: %#v", evidence)
	}
}

func TestUsageReconcilerCommitsSettlementPendingWithHistoricalPrice(t *testing.T) {
	usageID, holdID, priceID := uuid.New(), uuid.New(), uuid.New()
	store := &reconciliationStoreFake{}
	billing := &reconciliationBillingFake{price: PriceVersion{ID: priceID, Model: "test-model", InputPerMillion: 1_000_000, OutputPerMillion: 2_000_000}}
	reconciler := NewUsageReconciler(store, billing, nil, time.Second)
	item := ReconciliationItem{UsageRequest: UsageRequest{ID: usageID, HoldID: &holdID, PriceVersionID: &priceID, Model: "test-model", Status: "SETTLEMENT_PENDING", MaximumMicrocredits: 100}, UsageSnapshot: []byte(`{"usage":{"input_tokens":3,"output_tokens":2,"cached_input_tokens":0},"actual_microcredits":7}`)}

	if err := reconciler.reconcile(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if billing.committedActual != 7 || billing.committedUsageID != usageID || store.committedID != usageID || store.committedActual != 7 {
		t.Fatalf("settlement was not repaired: billing=%#v store=%#v", billing, store)
	}
}

func TestUsageReconcilerReleasesOnlyWithExplicitRejectionEvidence(t *testing.T) {
	usageID, holdID, priceID := uuid.New(), uuid.New(), uuid.New()
	store := &reconciliationStoreFake{}
	billing := &reconciliationBillingFake{}
	reconciler := NewUsageReconciler(store, billing, evidenceClientFake{evidence: UsageEvidence{Rejected: true}}, time.Second)
	providerID, adapterVersion, upstreamRequestID := "NEW_API", "new-api-v1", "newapi-request-456"
	item := ReconciliationItem{UsageRequest: UsageRequest{ID: usageID, HoldID: &holdID, PriceVersionID: &priceID, Model: "test-model", Status: "REVIEW", MaximumMicrocredits: 100}, ProviderID: &providerID, AdapterVersion: &adapterVersion, UpstreamRequestID: &upstreamRequestID}

	if err := reconciler.reconcile(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if billing.releasedID != usageID || store.releasedID != usageID || store.releasedReason != "reconciled_upstream_rejected" {
		t.Fatalf("hold was not released from explicit evidence: billing=%#v store=%#v", billing, store)
	}
}

func TestNewAPILogDoesNotTreatGenericFailureAsConclusiveRejection(t *testing.T) {
	if (newAPILog{Content: "upstream connection reset"}).conclusiveRejection() {
		t.Fatal("ambiguous upstream failure must remain in review")
	}
	if !(newAPILog{Content: "invalid_api_key"}).conclusiveRejection() {
		t.Fatal("pre-generation authentication rejection should be conclusive")
	}
}

func TestReconciliationErrorCodeDoesNotPersistProviderMessage(t *testing.T) {
	const marker = "secret-provider-response-marker"
	if code := reconciliationErrorCode(errors.New(marker)); code != "USAGE_EVIDENCE_UNAVAILABLE" || code == marker {
		t.Fatalf("unsafe reconciliation error code %q", code)
	}
}

type reconciliationStoreFake struct {
	committedID     uuid.UUID
	committedActual int64
	releasedID      uuid.UUID
	releasedReason  string
}

func (s *reconciliationStoreFake) RecordConvergenceEvidence(_ context.Context, _ uuid.UUID, _, _, _, _ string, _ map[string]any) error {
	return nil
}

func (*reconciliationStoreFake) ClaimReconciliation(context.Context, int, time.Duration) ([]ReconciliationItem, error) {
	return nil, nil
}
func (*reconciliationStoreFake) RescheduleReconciliation(context.Context, uuid.UUID, string, time.Time) error {
	return nil
}
func (*reconciliationStoreFake) StopReconciliation(context.Context, uuid.UUID, string) error {
	return nil
}
func (s *reconciliationStoreFake) MarkCommitted(_ context.Context, id uuid.UUID, actual int64, _ map[string]any) error {
	s.committedID, s.committedActual = id, actual
	return nil
}
func (s *reconciliationStoreFake) MarkReleased(_ context.Context, id uuid.UUID, reason string) error {
	s.releasedID, s.releasedReason = id, reason
	return nil
}

type reconciliationBillingFake struct {
	price            PriceVersion
	committedUsageID uuid.UUID
	committedActual  int64
	releasedID       uuid.UUID
}

func (b *reconciliationBillingFake) PriceByID(context.Context, uuid.UUID) (PriceVersion, error) {
	return b.price, nil
}
func (b *reconciliationBillingFake) Commit(_ context.Context, usageID, _ uuid.UUID, actual int64, _ Usage) error {
	b.committedUsageID, b.committedActual = usageID, actual
	return nil
}
func (b *reconciliationBillingFake) Release(_ context.Context, usageID, _ uuid.UUID, _ string) error {
	b.releasedID = usageID
	return nil
}

type evidenceClientFake struct{ evidence UsageEvidence }

func (e evidenceClientFake) ProviderID() string     { return "NEW_API" }
func (e evidenceClientFake) AdapterVersion() string { return "new-api-v1" }

func (e evidenceClientFake) Lookup(context.Context, string) (UsageEvidence, error) {
	return e.evidence, nil
}
