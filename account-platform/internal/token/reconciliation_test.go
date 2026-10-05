package token

import (
	"context"
	"encoding/json"
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
	if evidence.Usage == nil || *evidence.Usage != (Usage{InputTokens: 12, OutputTokens: 3, CachedInputTokens: 4}) {
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

func TestUsageReconcilerRetainsFundsWithoutConsumptionEvidence(t *testing.T) {
	usageID, holdID, priceID := uuid.New(), uuid.New(), uuid.New()
	store := &reconciliationStoreFake{}
	billing := &reconciliationBillingFake{}
	reconciler := NewUsageReconciler(store, billing, evidenceClientFake{}, time.Second)
	requestID := "fixture-incomplete-request"
	item := ReconciliationItem{UsageRequest: UsageRequest{ID: usageID, HoldID: &holdID, PriceVersionID: &priceID, Status: "REVIEW"}, NewAPIRequestID: &requestID}
	if err := reconciler.reconcile(context.Background(), item); err == nil {
		t.Fatal("missing consumption evidence was accepted")
	}
	if billing.releasedID != uuid.Nil || billing.committedUsageID != uuid.Nil || store.releasedID != uuid.Nil || store.committedID != uuid.Nil {
		t.Fatal("inconclusive evidence changed protected funds or a terminal state")
	}
}

type reconciliationStoreFake struct {
	items           []ReconciliationItem
	rescheduled     int
	stopped         int
	committedID     uuid.UUID
	committedActual int64
	releasedID      uuid.UUID
	releasedReason  string
}

func (s *reconciliationStoreFake) ClaimReconciliation(context.Context, int, time.Duration) ([]ReconciliationItem, error) {
	return s.items, nil
}
func (s *reconciliationStoreFake) RescheduleReconciliation(context.Context, ReconciliationItem, string, time.Time) error {
	s.rescheduled++
	return nil
}
func (s *reconciliationStoreFake) StopReconciliation(context.Context, ReconciliationItem, string) error {
	s.stopped++
	return nil
}

type boundedEvidenceClient struct {
	t     *testing.T
	calls int
	until time.Time
}

func (e *boundedEvidenceClient) Lookup(ctx context.Context, _ string) (UsageEvidence, error) {
	e.calls++
	deadline, ok := ctx.Deadline()
	if !ok || !deadline.Equal(e.until) {
		e.t.Fatal("external evidence work was not bounded by the claim lease")
	}
	return UsageEvidence{}, nil
}

func TestUsageReconcilerSkipsExpiredBatchItemsAndBoundsActiveWork(t *testing.T) {
	now := time.Now().UTC()
	hold, price := uuid.New(), uuid.New()
	requestID := "fixture-request"
	item := ReconciliationItem{UsageRequest: UsageRequest{ID: uuid.New(), HoldID: &hold, PriceVersionID: &price, Status: "REVIEW", StartedAt: now}, NewAPIRequestID: &requestID, LeaseUntil: now.Add(time.Minute)}
	expired := item
	expired.ID, expired.LeaseUntil = uuid.New(), now
	store := &reconciliationStoreFake{items: []ReconciliationItem{expired, item}}
	evidence := &boundedEvidenceClient{t: t, until: item.LeaseUntil}
	reconciler := NewUsageReconciler(store, &reconciliationBillingFake{}, evidence, time.Second)
	reconciler.now = func() time.Time { return now }
	reconciler.runOnce(context.Background())
	if evidence.calls != 1 || store.rescheduled != 1 || store.stopped != 0 {
		t.Fatal("expired claim executed external work or changed reconciliation")
	}
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
func (b *reconciliationBillingFake) Commit(_ context.Context, usageID, _, _ uuid.UUID, actual int64, _ Usage) error {
	b.committedUsageID, b.committedActual = usageID, actual
	return nil
}
func (b *reconciliationBillingFake) Release(_ context.Context, usageID, _, _ uuid.UUID, _ string) error {
	b.releasedID = usageID
	return nil
}

type evidenceClientFake struct{ evidence UsageEvidence }

func (e evidenceClientFake) Lookup(context.Context, string) (UsageEvidence, error) {
	return e.evidence, nil
}

func TestUsageReconcilerInvalidOrUnknownReleaseIntentCannotRelease(t *testing.T) {
	hold, other := uuid.New(), uuid.New()
	for _, scenario := range []struct{ name, reason, snapshot string }{
		{"unknown-class", "upstream_error_unknown_usage", "{\"hold_id\":\"" + hold.String() + "\",\"release_reason\":\"upstream_rejected\"}"},
		{"missing-hold", "release_pending", "{\"release_reason\":\"upstream_rejected\"}"},
		{"mismatched-hold", "release_pending", "{\"hold_id\":\"" + other.String() + "\",\"release_reason\":\"upstream_rejected\"}"},
		{"missing-reason", "release_pending", "{\"hold_id\":\"" + hold.String() + "\"}"},
		{"unconfirmed-local", "local_release_record_pending", "{\"hold_id\":\"" + hold.String() + "\",\"release_reason\":\"upstream_rejected\"}"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			store := &reconciliationStoreFake{}
			billing := &reconciliationBillingFake{}
			reconciler := NewUsageReconciler(store, billing, nil, time.Second)
			item := ReconciliationItem{UsageRequest: UsageRequest{ID: uuid.New(), Status: "REVIEW", HoldID: &hold, ErrorClass: &scenario.reason}, UsageSnapshot: []byte(scenario.snapshot)}
			if err := reconciler.reconcile(context.Background(), item); err == nil {
				t.Fatal("invalid or unknown release intent was accepted")
			}
			if billing.releasedID != uuid.Nil || store.releasedID != uuid.Nil {
				t.Fatal("invalid or unknown intent released funds")
			}
		})
	}
}
